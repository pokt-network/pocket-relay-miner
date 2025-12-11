package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func redisDebugSMSTCmd() *cobra.Command {
	var (
		sessionID string
		limit     int64
	)

	cmd := &cobra.Command{
		Use:   "smst",
		Short: "Inspect SMST tree data",
		Long: `Inspect Sparse Merkle Sum Tree (SMST) nodes stored in Redis.

SMST data is stored at:
  - Key: ha:smst:{sessionID}:nodes (Hash)
  - Fields: Hex-encoded SMST node keys
  - Values: Raw SMST node data

This shows the number of nodes and sample keys.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			return inspectSMST(ctx, client, sessionID, limit)
		},
	}

	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID (required)")
	cmd.Flags().Int64Var(&limit, "limit", 10, "Number of sample nodes to display")
	_ = cmd.MarkFlagRequired("session")

	return cmd
}

func inspectSMST(ctx context.Context, client *redisClient, sessionID string, limit int64) error {
	key := fmt.Sprintf("ha:smst:%s:nodes", sessionID)

	// Check if key exists
	exists, err := client.Exists(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to check SMST existence: %w", err)
	}

	if exists == 0 {
		fmt.Printf("No SMST data found for session: %s\n", sessionID)
		return nil
	}

	// Get total node count
	count, err := client.HLen(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to get SMST node count: %w", err)
	}

	fmt.Printf("SMST Tree for Session: %s\n", sessionID)
	fmt.Printf("Total Nodes: %d\n\n", count)

	if count == 0 {
		return nil
	}

	// Get sample nodes
	cursor := uint64(0)
	var sampleKeys []string
	var sampleValues []string

	for len(sampleKeys) < int(limit) {
		keys, newCursor, err := client.HScan(ctx, key, cursor, "*", limit).Result()
		if err != nil {
			return fmt.Errorf("failed to scan SMST nodes: %w", err)
		}

		// HScan returns alternating key-value pairs
		for i := 0; i < len(keys); i += 2 {
			if len(sampleKeys) >= int(limit) {
				break
			}
			sampleKeys = append(sampleKeys, keys[i])
			if i+1 < len(keys) {
				sampleValues = append(sampleValues, keys[i+1])
			}
		}

		cursor = newCursor
		if cursor == 0 {
			break
		}
	}

	// Display sample nodes
	fmt.Printf("Sample Nodes (showing %d):\n\n", len(sampleKeys))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "NODE KEY (HEX)\tVALUE SIZE\n")

	for i, keyHex := range sampleKeys {
		var valueSize int
		if i < len(sampleValues) {
			// Decode hex to get actual size
			decoded, err := hex.DecodeString(sampleValues[i])
			if err == nil {
				valueSize = len(decoded)
			} else {
				valueSize = len(sampleValues[i])
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%d bytes\n", keyHex, valueSize)
	}

	_ = w.Flush()

	// Offer to delete
	fmt.Printf("\nTo delete this SMST tree, use: redis-debug flush --pattern 'ha:smst:%s:*'\n", sessionID)

	return nil
}
