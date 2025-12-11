package cmd

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/logging"
	transportredis "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

var (
	// Global flags for redis-debug
	redisURL string
)

// RedisDebugCmd returns the redis-debug command for debugging Redis data.
func RedisDebugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redis-debug",
		Short: "Debug and inspect Redis data structures",
		Long: `Debug tooling for inspecting and managing Redis data used by the RelayMiner.

This command provides operators with tools to:
- Inspect session state and SMST trees
- Monitor Redis Streams (WAL)
- Check cache entries and invalidate stale data
- Verify leader election status
- Inspect deduplication sets
- Monitor pub/sub channels
- Flush data with safety confirmations`,
	}

	// Add global flags
	cmd.PersistentFlags().StringVar(&redisURL, "redis", "redis://localhost:6379", "Redis connection URL")

	// Add subcommands
	cmd.AddCommand(redisDebugSessionsCmd())
	cmd.AddCommand(redisDebugSMSTCmd())
	cmd.AddCommand(redisDebugStreamsCmd())
	cmd.AddCommand(redisDebugDedupCmd())
	cmd.AddCommand(redisDebugLeaderCmd())
	cmd.AddCommand(redisDebugCacheCmd())
	cmd.AddCommand(redisDebugSupplierCmd())
	cmd.AddCommand(redisDebugMeterCmd())
	cmd.AddCommand(redisDebugPubSubCmd())
	cmd.AddCommand(redisDebugKeysCmd())
	cmd.AddCommand(redisDebugFlushCmd())

	return cmd
}

// createRedisClient creates a Redis client from the global redisURL flag.
func createRedisClient(ctx context.Context) (*redisClient, error) {
	logger := logging.NewLoggerFromConfig(logging.Config{
		Level:  "info",
		Format: "text",
		Async:  false,
	})

	client, err := transportredis.NewClient(ctx, transportredis.ClientConfig{
		URL:        redisURL,
		MaxRetries: 3,
		PoolSize:   5,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis at %s: %w", redisURL, err)
	}

	return &redisClient{
		UniversalClient: client,
		logger:          logger,
	}, nil
}

// redisClient wraps redis.UniversalClient with additional helpers.
type redisClient struct {
	redis.UniversalClient
	logger logging.Logger
}
