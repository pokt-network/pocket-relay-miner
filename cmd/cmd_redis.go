package cmd

import (
	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/cmd/redis"
)

// RedisCmd returns the redis command for debugging Redis data.
func RedisCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redis",
		Short: "Debug and inspect Redis data structures",
		Long: `Debug tooling for inspecting and managing Redis data used by the RelayMiner.

This command provides operators with tools to:
- Inspect session state and SMST trees
- Monitor Redis Streams (WAL)
- Check cache entries and invalidate stale data
- Verify leader election status
- Inspect deduplication sets
- Monitor pub/sub channels
- Flush data with safety confirmations

Configuration:
  --config: Use miner or relayer config file (inherits namespace settings)
  --redis:  Override Redis URL (optional if using --config)

Examples:
  # Use miner config (inherits namespace, connection settings)
  pocket-relay-miner redis sessions --config config/miner.yaml --supplier pokt1abc...

  # Use relayer config
  pocket-relay-miner redis cache --config config/relayer.yaml --type application

  # Direct connection (uses default namespace: ha:*)
  pocket-relay-miner redis leader --redis redis://localhost:6379`,
	}

	// Add global flags and bind to redis package variables
	cmd.PersistentFlags().StringVar(&redis.RedisURL, "redis", "", "Redis connection URL (optional if using --config)")
	cmd.PersistentFlags().StringVar(&redis.RedisConfig, "config", "", "Path to miner/relayer config file (inherits namespace settings)")

	// Add subcommands
	cmd.AddCommand(redis.SessionsCmd())
	cmd.AddCommand(redis.SMSTCmd())
	cmd.AddCommand(redis.StreamsCmd())
	cmd.AddCommand(redis.DedupCmd())
	cmd.AddCommand(redis.LeaderCmd())
	cmd.AddCommand(redis.CacheCmd())
	cmd.AddCommand(redis.SupplierCmd())
	cmd.AddCommand(redis.MeterCmd())
	cmd.AddCommand(redis.PubSubCmd())
	cmd.AddCommand(redis.KeysCmd())
	cmd.AddCommand(redis.FlushCmd())

	return cmd
}
