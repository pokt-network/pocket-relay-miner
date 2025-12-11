package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func redisDebugKeysCmd() *cobra.Command {
	var (
		pattern string
		limit   int64
		stats   bool
	)

	cmd := &cobra.Command{
		Use:   "keys",
		Short: "List keys by pattern",
		Long: `List Redis keys matching a pattern.

Common patterns:
  - ha:smst:* - All SMST trees
  - ha:miner:sessions:* - All session data
  - ha:cache:* - All cache entries
  - ha:relays:* - All relay streams
  - ha:* - All HA-related keys

Use --stats to show type and TTL information.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			return listKeys(ctx, client, pattern, limit, stats)
		},
	}

	cmd.Flags().StringVar(&pattern, "pattern", "ha:*", "Key pattern (supports * and ?)")
	cmd.Flags().Int64Var(&limit, "limit", 100, "Maximum number of keys to return")
	cmd.Flags().BoolVar(&stats, "stats", false, "Show type and TTL for each key")

	return cmd
}

func listKeys(ctx context.Context, client *redisClient, pattern string, limit int64, showStats bool) error {
	var cursor uint64
	var keys []string
	var scanned int64

	fmt.Printf("Scanning for keys matching: %s\n", pattern)
	if limit > 0 {
		fmt.Printf("Limit: %d keys\n", limit)
	}
	fmt.Printf("\n")

	for {
		// Scan in batches
		batchSize := int64(100)
		if limit > 0 && scanned+batchSize > limit {
			batchSize = limit - scanned
		}

		scanKeys, newCursor, err := client.Scan(ctx, cursor, pattern, batchSize).Result()
		if err != nil {
			return fmt.Errorf("failed to scan keys: %w", err)
		}

		keys = append(keys, scanKeys...)
		scanned += int64(len(scanKeys))

		cursor = newCursor

		// Stop if we hit limit or completed scan
		if (limit > 0 && scanned >= limit) || cursor == 0 {
			break
		}
	}

	if len(keys) == 0 {
		fmt.Printf("No keys found matching pattern: %s\n", pattern)
		return nil
	}

	if !showStats {
		// Simple list
		for i, key := range keys {
			fmt.Printf("%d. %s\n", i+1, key)
		}
		fmt.Printf("\nTotal: %d keys\n", len(keys))
		return nil
	}

	// Detailed list with stats
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "#\tKEY\tTYPE\tTTL\n")

	for i, key := range keys {
		keyType, _ := client.Type(ctx, key).Result()
		ttl, _ := client.TTL(ctx, key).Result()

		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%v\n", i+1, key, keyType, ttl)
	}

	_ = w.Flush()
	fmt.Printf("\nTotal: %d keys\n", len(keys))

	return nil
}
