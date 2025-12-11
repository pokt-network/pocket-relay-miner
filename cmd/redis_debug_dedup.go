package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func redisDebugDedupCmd() *cobra.Command {
	var sessionID string

	cmd := &cobra.Command{
		Use:   "dedup",
		Short: "Inspect deduplication sets",
		Long: `Inspect relay deduplication sets in Redis.

Dedup data is stored at:
  - Key: ha:miner:dedup:session:{sessionID} (Set)
  - Members: Hex-encoded relay hashes

This prevents processing the same relay multiple times.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			return inspectDedup(ctx, client, sessionID)
		},
	}

	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID (required)")
	_ = cmd.MarkFlagRequired("session")

	return cmd
}

func inspectDedup(ctx context.Context, client *redisClient, sessionID string) error {
	key := fmt.Sprintf("ha:miner:dedup:session:%s", sessionID)

	// Check if set exists
	exists, err := client.Exists(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to check dedup set existence: %w", err)
	}

	if exists == 0 {
		fmt.Printf("No deduplication data found for session: %s\n", sessionID)
		return nil
	}

	// Get set size
	count, err := client.SCard(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to get dedup set size: %w", err)
	}

	// Get TTL
	ttl, err := client.TTL(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to get dedup TTL: %w", err)
	}

	fmt.Printf("Deduplication Set for Session: %s\n", sessionID)
	fmt.Printf("Total Relay Hashes: %d\n", count)
	fmt.Printf("TTL: %v\n", ttl)
	fmt.Printf("\n")

	if count == 0 {
		return nil
	}

	// Show sample hashes
	limit := int64(10)
	if count < limit {
		limit = count
	}

	hashes, err := client.SRandMemberN(ctx, key, limit).Result()
	if err != nil {
		return fmt.Errorf("failed to get sample hashes: %w", err)
	}

	fmt.Printf("Sample Relay Hashes (showing %d of %d):\n", len(hashes), count)
	for i, hash := range hashes {
		fmt.Printf("  %d. %s\n", i+1, hash)
	}

	return nil
}
