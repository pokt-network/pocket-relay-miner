package redis

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func FlushCmd() *cobra.Command {
	var (
		pattern  string
		flushAll bool
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Flush Redis data (dangerous)",
		Long: `Flush (delete) Redis data with safety confirmations.

WARNING: This is a destructive operation!

Options:
  --pattern  Delete keys matching pattern (e.g., "ha:smst:*")
  --all      Delete ALL HA-related data (ha:*)
  --force    Skip confirmation prompts (use with caution)

Examples:
  # Delete all SMST trees
  redis-debug flush --pattern "ha:smst:*"

  # Delete all session data for a supplier
  redis-debug flush --pattern "ha:miner:sessions:pokt1abc*"

  # Delete everything (DANGEROUS)
  redis-debug flush --all

Safety:
  - Without --force, you will be prompted to confirm
  - Shows count of keys that will be deleted
  - Requires typing "DELETE" to confirm`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := CreateRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if flushAll {
				pattern = "ha:*"
			}

			if pattern == "" {
				return fmt.Errorf("specify --pattern or --all")
			}

			return flushKeys(ctx, client, pattern, force)
		},
	}

	cmd.Flags().StringVar(&pattern, "pattern", "", "Key pattern to delete")
	cmd.Flags().BoolVar(&flushAll, "all", false, "Delete ALL HA-related data (ha:*)")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompts")

	return cmd
}

func flushKeys(ctx context.Context, client *DebugRedisClient, pattern string, force bool) error {
	// First, scan to see what will be deleted (cluster-aware: a plain SCAN
	// on Redis Cluster hits a single node and silently misses the rest).
	fmt.Printf("Scanning for keys matching: %s\n", pattern)

	keys, err := clusterAwareScanAllKeys(ctx, client, pattern)
	if err != nil {
		return fmt.Errorf("failed to scan keys: %w", err)
	}

	if len(keys) == 0 {
		fmt.Printf("No keys found matching pattern: %s\n", pattern)
		return nil
	}

	// Show what will be deleted
	fmt.Printf("\nFound %d keys matching pattern '%s'\n\n", len(keys), pattern)

	// Show sample (first 20)
	sampleSize := 20
	if len(keys) < sampleSize {
		sampleSize = len(keys)
	}

	fmt.Printf("Sample keys (showing first %d):\n", sampleSize)
	for i := 0; i < sampleSize; i++ {
		fmt.Printf("  - %s\n", keys[i])
	}

	if len(keys) > sampleSize {
		fmt.Printf("  ... and %d more\n", len(keys)-sampleSize)
	}

	fmt.Printf("\n")

	// Confirmation
	if !force {
		fmt.Printf("⚠️  WARNING: This will DELETE %d keys!\n", len(keys))
		fmt.Printf("⚠️  This operation is IRREVERSIBLE!\n\n")

		// Risk assessment
		if pattern == "ha:*" || pattern == "*" {
			fmt.Printf("🚨 CRITICAL: You are about to delete ALL data!\n")
			fmt.Printf("🚨 This will cause:\n")
			fmt.Printf("   - Loss of all session state\n")
			fmt.Printf("   - Loss of all SMST trees\n")
			fmt.Printf("   - Loss of all cached data\n")
			fmt.Printf("   - Disruption to all running instances\n\n")
		} else if strings.HasPrefix(pattern, "ha:smst:") {
			fmt.Printf("⚠️  This will delete SMST tree data\n")
			fmt.Printf("⚠️  May cause proof submission failures\n\n")
		} else if strings.HasPrefix(pattern, "ha:miner:sessions:") {
			fmt.Printf("⚠️  This will delete session metadata\n")
			fmt.Printf("⚠️  May cause claim/proof tracking issues\n\n")
		}

		fmt.Printf("To confirm, type 'DELETE' (all caps): ")
		reader := bufio.NewReader(os.Stdin)
		confirmation, _ := reader.ReadString('\n')
		confirmation = strings.TrimSpace(confirmation)

		if confirmation != "DELETE" {
			fmt.Printf("\nAborted. No keys were deleted.\n")
			return nil
		}
	}

	// Perform deletion: single-key DELs inside pipelines, so Redis Cluster
	// deployments don't fail with CROSSSLOT on multi-key commands.
	fmt.Printf("\nDeleting %d keys...\n", len(keys))

	deleted, err := pipelinedDelete(ctx, client, keys, 100, func(deleted, total int) {
		fmt.Printf("  Deleted %d / %d keys\n", deleted, total)
	})
	if err != nil {
		return fmt.Errorf("failed to delete keys (deleted %d): %w", deleted, err)
	}

	fmt.Printf("\n✅ Successfully deleted %d keys\n", deleted)

	return nil
}
