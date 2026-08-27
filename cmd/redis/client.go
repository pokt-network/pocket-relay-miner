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

	// RedisBasePrefix points the tool at a keyspace directly, bypassing config
	// loading entirely.
	RedisBasePrefix string
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

	// An explicit base prefix wins over a config file, and needs no config at
	// all. It exists because the two are otherwise coupled in the worst place:
	// the namespace guard rejects a config whose keys this version would
	// relocate, and this CLI resolves its namespace through the same LoadConfig,
	// so the operator told to "inspect and migrate your keys" would find this
	// tool refusing to start for the same reason their fleet did.
	if RedisBasePrefix != "" {
		namespace = config.RedisNamespaceConfig{BasePrefix: RedisBasePrefix}
		// VALIDATED, unlike before. The flag exists to get past the retired-field
		// guard, not past the character rule -- and this is the one binary that
		// deletes by pattern: --base-prefix '*' produced AllKeysPattern() "*:*",
		// which flush --all and cache --invalidate hand straight to a delete.
		// The retired-field branch of Validate cannot fire here: this namespace
		// has nothing but a base prefix.
		if err := namespace.Validate(); err != nil {
			return nil, err
		}
		logger.Info().
			Str("base_prefix", RedisBasePrefix).
			Msg("using the base prefix given on the command line")
	}
	if RedisBasePrefix == "" && RedisConfig != "" {
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

	// With --base-prefix AND --config, the base prefix overrides the namespace
	// but the config still supplies the URL. Before this, the flag returned
	// before any config was read, so `--config x --base-prefix ha` connected to
	// localhost and reported "No keys found" -- to an operator inspecting a
	// keyspace they had just been told to migrate.
	if RedisBasePrefix != "" && RedisConfig != "" && url == "" {
		if minerCfg, err := miner.LoadConfig(RedisConfig); err == nil {
			url = minerCfg.Redis.URL
		} else if relayerCfg, err := relayer.LoadConfig(RedisConfig); err == nil {
			url = relayerCfg.Redis.URL
		}
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
