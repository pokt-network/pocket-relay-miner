package redis

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func SMSTCmd() *cobra.Command {
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
			client, err := CreateRedisClient(ctx)
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

func inspectSMST(ctx context.Context, client *DebugRedisClient, sessionID string, limit int64) error {
	// Production writes per-supplier SMST keys ({base}:smst:{supplier}:{session}:nodes),
	// so a session can have several trees (one per supplier that served it).
	// Scan every supplier's tree for this session rather than the legacy
	// single-arg key ({base}:smst:{session}:nodes) that production no longer
	// writes — that shape silently reported "No SMST data found" for every
	// real key.
	pattern := fmt.Sprintf("%s*:%s:nodes", client.KB().SMSTNodesPrefix(), sessionID)
	keys, err := clusterAwareScanAllKeys(ctx, client, pattern)
	if err != nil {
		return fmt.Errorf("failed to scan SMST keys: %w", err)
	}

	if len(keys) == 0 {
		fmt.Printf("No SMST data found for session: %s\n", sessionID)
		return nil
	}

	for _, key := range keys {
		if err := displaySMSTTree(ctx, client, key, limit); err != nil {
			return err
		}
	}

	return nil
}

// displaySMSTTree prints node count and a sample of nodes for one SMST tree key
// (one supplier's tree for a session).
func displaySMSTTree(ctx context.Context, client *DebugRedisClient, key string, limit int64) error {
	// Get total node count
	count, err := client.HLen(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to get SMST node count for %s: %w", key, err)
	}

	fmt.Printf("SMST Tree: %s\n", key)
	fmt.Printf("Total Nodes: %d\n\n", count)

	if count == 0 {
		return nil
	}

	// Get sample nodes
	cursor := uint64(0)
	var sampleKeys []string
	var sampleValues []string

	for len(sampleKeys) < int(limit) {
		nodes, newCursor, err := client.HScan(ctx, key, cursor, "*", limit).Result()
		if err != nil {
			return fmt.Errorf("failed to scan SMST nodes for %s: %w", key, err)
		}

		// HScan returns alternating key-value pairs
		for i := 0; i < len(nodes); i += 2 {
			if len(sampleKeys) >= int(limit) {
				break
			}
			sampleKeys = append(sampleKeys, nodes[i])
			if i+1 < len(nodes) {
				sampleValues = append(sampleValues, nodes[i+1])
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
	fmt.Printf("\nTo delete this SMST tree, use: redis-debug flush --pattern '%s'\n\n", key)

	return nil
}
