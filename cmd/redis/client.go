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

	// yaml.v3 ignores every key it was not asked about and errors on none, so
	// ANY well-formed YAML unmarshals into that struct "successfully" and yields
	// an empty URL -- which CreateRedisClient then defaults to localhost. Measured
	// 2026-08-28: `image:\n  tag: v1` resolved to url="" base="ha" err=<nil>.
	// An operator who passes a helm values.yaml, or yesterday's renamed file,
	// gets "No keys found" against their own laptop -- the exact "my data is
	// gone" confusion this function's header says it exists to prevent -- and
	// `flush` and `cache --invalidate` act on the wrong server. A config file
	// that carries no redis.url is not a config file for this tool.
	if file.Redis.URL == "" {
		return "", config.RedisNamespaceConfig{}, fmt.Errorf(
			"%s has no redis.url: it parsed as YAML but carries no redis block, so it is "+
				"not a miner or relayer config. Point --config at the rendered config, or "+
				"pass --redis explicitly", path)
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
		case RedisURL == "":
			// --base-prefix settles the namespace, but nothing settles the URL,
			// and the default is localhost. Warning and continuing meant a typo
			// in the config path produced a clean-looking connection to a local
			// Redis, an empty keyspace, and an operator concluding the upgrade
			// destroyed their data -- one Warn above a "connected to Redis" line
			// that prints localhost as if it had been chosen. The flag exists for
			// a config the FLEET rejects; a config this tool cannot even read
			// says nothing about which server to act on.
			return "", namespace, fmt.Errorf(
				"%w -- and no --redis was given, so there is no server to act on. "+
					"Pass --redis explicitly alongside --base-prefix", err)
		default:
			// --base-prefix settles the namespace and --redis settles the server,
			// so the config was going to add nothing. This is the situation the
			// two flags exist for: a config the fleet rejects must not stop the
			// tool an operator reaches for BECAUSE the fleet rejects it.
			logger.Warn().
				Err(err).
				Str("config_file", RedisConfig).
				Msg("could not read the config; continuing with --base-prefix and the URL from --redis")
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
