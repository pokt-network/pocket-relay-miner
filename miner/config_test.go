package miner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Redis config now uses namespace configuration for stream prefix and consumer group
	// These are derived from the namespace config at runtime
	require.Equal(t, "redis://localhost:6379", cfg.Redis.URL)
	// Note: BlockTimeout removed - BLOCK 0 (TRUE PUSH) is now hardcoded in consumer
	require.Equal(t, int64(60000), cfg.Redis.ClaimIdleTimeoutMs)
	require.Equal(t, int64(10), cfg.DeduplicationTTLBlocks)
	require.Equal(t, int64(1000), cfg.BatchSize) // Increased from 100 for better throughput
	require.Equal(t, int64(50), cfg.AckBatchSize)
}

func TestConfig_Validate_Valid(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{
			RedisConfig: config.RedisConfig{
				URL: "redis://localhost:6379",
			},
			ConsumerName: "miner-1",
		},
		PocketNode: config.PocketNodeConfig{
			QueryNodeRPCUrl:  "http://localhost:26657",
			QueryNodeGRPCUrl: "localhost:9090",
		},
		Keys: config.KeysConfig{
			KeysFile: "/path/to/keys.yaml",
		},
	}

	err := cfg.Validate()
	require.NoError(t, err)
}

func TestConfig_Validate_MissingRedisURL(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{
			ConsumerName: "miner-1",
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis.url is required")
}

func TestConfig_Validate_InvalidRedisURL(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{
			RedisConfig: config.RedisConfig{
				URL: "://invalid",
			},
			ConsumerName: "miner-1",
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid redis.url")
}

func TestConfig_Validate_MissingPocketNodeRPC(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{
			RedisConfig: config.RedisConfig{
				URL: "redis://localhost:6379",
			},
			ConsumerName: "miner-1",
		},
		PocketNode: config.PocketNodeConfig{
			QueryNodeGRPCUrl: "localhost:9090",
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "pocket_node.query_node_rpc_url is required")
}

func TestConfig_Validate_MissingPocketNodeGRPC(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{
			RedisConfig: config.RedisConfig{
				URL: "redis://localhost:6379",
			},
			ConsumerName: "miner-1",
		},
		PocketNode: config.PocketNodeConfig{
			QueryNodeRPCUrl: "http://localhost:26657",
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "pocket_node.query_node_grpc_url is required")
}

func TestConfig_Validate_NoKeySource(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{
			RedisConfig: config.RedisConfig{
				URL: "redis://localhost:6379",
			},
			ConsumerName: "miner-1",
		},
		PocketNode: config.PocketNodeConfig{
			QueryNodeRPCUrl:  "http://localhost:26657",
			QueryNodeGRPCUrl: "localhost:9090",
		},
		Keys: config.KeysConfig{},
	}

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "keys config is required")
}

// Note: Supplier validation tests removed - suppliers are auto-discovered from keys
// See TestConfig_Validate_NoKeySource for key validation

// Note: TestConfig_GetRedisBlockTimeout removed - BLOCK 0 is now hardcoded

func TestConfig_GetClaimIdleTimeout(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{
			ClaimIdleTimeoutMs: 120000,
		},
	}

	require.Equal(t, 2*time.Minute, cfg.GetClaimIdleTimeout())

	// Test default
	cfg.Redis.ClaimIdleTimeoutMs = 0
	require.Equal(t, time.Minute, cfg.GetClaimIdleTimeout())
}

func TestConfig_GetBatchSize(t *testing.T) {
	cfg := &Config{BatchSize: 200}
	require.Equal(t, int64(200), cfg.GetBatchSize())

	// Test default
	cfg.BatchSize = 0
	require.Equal(t, int64(1000), cfg.GetBatchSize()) // Default increased from 100
}

func TestConfig_GetAckBatchSize(t *testing.T) {
	cfg := &Config{AckBatchSize: 25}
	require.Equal(t, int64(25), cfg.GetAckBatchSize())

	// Test default
	cfg.AckBatchSize = 0
	require.Equal(t, int64(50), cfg.GetAckBatchSize())
}

func TestConfig_GetDeduplicationTTL(t *testing.T) {
	cfg := &Config{DeduplicationTTLBlocks: 20}
	require.Equal(t, int64(20), cfg.GetDeduplicationTTL())

	// Test default
	cfg.DeduplicationTTLBlocks = 0
	require.Equal(t, int64(10), cfg.GetDeduplicationTTL())
}

// Note: TestSupplierConfig_WithServices and TestConfig_Validate_MultipleSuppliers removed
// Suppliers are now auto-discovered from keys configuration - no explicit supplier config needed
