package redis

import (
	"context"
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/relayer"
	transportredis "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

var (
	// Package-level variables set by parent cmd package
	RedisURL    string
	RedisConfig string
)

// CreateRedisClient creates a wrapped Redis client with KeyBuilder support.
// Loads namespace config from miner/relayer config file if provided, otherwise uses defaults.
func CreateRedisClient(ctx context.Context) (*DebugRedisClient, error) {
	logger := logging.NewLoggerFromConfig(logging.Config{
		Level:  "info",
		Format: "text",
		Async:  false,
	})

	var url string
	var namespace config.RedisNamespaceConfig

	// Load config file if provided (inherits namespace and Redis settings)
	if RedisConfig != "" {
		// Try loading as miner config first
		minerCfg, minerErr := miner.LoadConfig(RedisConfig)
		if minerErr == nil {
			url = minerCfg.Redis.URL
			namespace = minerCfg.Redis.Namespace
			logger.Info().
				Str("config_file", RedisConfig).
				Str("type", "miner").
				Msg("loaded namespace config from miner config")
		} else {
			// Try as relayer config
			relayerCfg, relayerErr := relayer.LoadConfig(RedisConfig)
			if relayerErr == nil {
				url = relayerCfg.Redis.URL
				namespace = relayerCfg.Redis.Namespace
				logger.Info().
					Str("config_file", RedisConfig).
					Str("type", "relayer").
					Msg("loaded namespace config from relayer config")
			} else {
				return nil, fmt.Errorf("failed to load config as miner or relayer: miner_err=%v, relayer_err=%v", minerErr, relayerErr)
			}
		}
	} else {
		// No config file - use default namespace
		namespace = config.DefaultRedisNamespaceConfig()
		logger.Info().Msg("using default namespace config (ha:*)")
	}

	// Allow --redis flag to override URL from config
	if RedisURL != "" {
		url = RedisURL
	}

	// Default to localhost:6379 if no URL provided
	if url == "" {
		url = "redis://localhost:6379"
	}

	client, err := transportredis.NewClient(ctx, transportredis.ClientConfig{
		URL:       url,
		Namespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis at %s: %w", url, err)
	}

	logger.Info().
		Str("redis_url", url).
		Str("base_prefix", namespace.BasePrefix).
		Msg("connected to Redis with namespace config")

	return &DebugRedisClient{
		Client: client,
		Logger: logger,
	}, nil
}

// DebugRedisClient wraps the transport Redis client with additional helpers for debugging.
// The embedded *transportredis.Client provides both *redisutil.Client interface
// and KeyBuilder access via KB() method.
type DebugRedisClient struct {
	*transportredis.Client
	Logger logging.Logger
}
