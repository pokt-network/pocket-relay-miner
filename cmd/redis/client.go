package redis

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
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

	url, namespace, err := resolveTarget(logger)
	if err != nil {
		return nil, err
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

// namespaceFromConfig reads the TWO values this tool needs out of a miner or
// relayer config file: the Redis URL and the namespace.
//
// Deliberately NOT miner.LoadConfig / relayer.LoadConfig. Those validate the
// whole config, so a file the fleet rejects -- which is precisely when an
// operator reaches for this tool -- also stopped the tool: a config missing an
// unrelated field like pocket_node.query_node_rpc_url made it fall back to
// localhost and report "No keys found", which reads as "my data is gone".
// Both configs carry the same `redis:` block, so one shape reads either.
func namespaceFromConfig(path string, logger logging.Logger) (string, config.RedisNamespaceConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", config.RedisNamespaceConfig{}, fmt.Errorf("read %s: %w", path, err)
	}

	var file struct {
		Redis struct {
			URL       string                      `yaml:"url"`
			Namespace config.RedisNamespaceConfig `yaml:"namespace"`
		} `yaml:"redis"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return "", config.RedisNamespaceConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}

	logger.Info().
		Str("config_file", path).
		Str("base_prefix", file.Redis.Namespace.WithDefaults().BasePrefix).
		Msg("read the redis url and namespace from the config file")
	return file.Redis.URL, file.Redis.Namespace.WithDefaults(), nil
}

// resolveTarget decides WHICH Redis and WHICH keyspace this invocation acts on.
//
// Extracted so the rule can be tested: it is the whole of the CLI's
// config-versus-flags behaviour, and it used to be three exclusive branches
// inside client construction, where the only way to exercise it was to run the
// binary against a live server.
func resolveTarget(logger logging.Logger) (string, config.RedisNamespaceConfig, error) {
	var url string
	// ONE rule, no exclusive branches: start from the defaults, let a config
	// file replace them, then let each flag override its own value. The previous
	// shape was three branches over two settings, and it had already produced a
	// defect -- `--base-prefix midemo` with no --config logged "using the base
	// prefix given on the command line" and then fell into the else that reset
	// the namespace to the default, so the flag did nothing.
	namespace := config.DefaultRedisNamespaceConfig()

	if RedisConfig != "" {
		cfgURL, cfgNS, err := namespaceFromConfig(RedisConfig, logger)
		switch {
		case err == nil:
			url, namespace = cfgURL, cfgNS
		case RedisBasePrefix == "":
			return "", namespace, err
		default:
			// --base-prefix settles the namespace on its own; the config was only
			// going to supply the URL. A config this tool cannot load is the very
			// situation the flag exists for, so it must not be fatal here.
			logger.Warn().
				Err(err).
				Str("config_file", RedisConfig).
				Msg("could not read the config; continuing with --base-prefix, and the URL from --redis or the default")
		}
	}

	if RedisBasePrefix != "" {
		namespace = config.RedisNamespaceConfig{BasePrefix: RedisBasePrefix}
		logger.Info().
			Str("base_prefix", RedisBasePrefix).
			Msg("using the base prefix given on the command line")
	}

	// Validated wherever it came from. This is the one binary that deletes by
	// pattern: a glob in the base makes AllKeysPattern() "*:*", which flush --all
	// and cache --invalidate hand straight to a delete.
	if err := namespace.Validate(); err != nil {
		return "", namespace, err
	}

	return url, namespace, nil
}
