package miner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

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

func TestGetCPUMultiplier(t *testing.T) {
	cfg := &Config{}
	require.Equal(t, 4, cfg.GetCPUMultiplier()) // default

	cfg.WorkerPools.CPUMultiplier = 8
	require.Equal(t, 8, cfg.GetCPUMultiplier())
}

func TestGetWorkersPerSupplier(t *testing.T) {
	cfg := &Config{}
	require.Equal(t, 6, cfg.GetWorkersPerSupplier()) // default: handles unbatched claims

	cfg.WorkerPools.WorkersPerSupplier = 4
	require.Equal(t, 4, cfg.GetWorkersPerSupplier())
}

func TestGetQueryWorkers(t *testing.T) {
	cfg := &Config{}
	require.Equal(t, 20, cfg.GetQueryWorkers()) // default

	cfg.WorkerPools.QueryWorkers = 30
	require.Equal(t, 30, cfg.GetQueryWorkers())
}

func TestGetSettlementWorkers(t *testing.T) {
	cfg := &Config{}
	require.Equal(t, 2, cfg.GetSettlementWorkers()) // default

	cfg.WorkerPools.SettlementWorkers = 4
	require.Equal(t, 4, cfg.GetSettlementWorkers())
}

func TestGetMasterPoolSize(t *testing.T) {
	cfg := &Config{}

	// Test auto-calculation with small supplier count (CPU-bound)
	// With default values: cpu_multiplier=4, workers_per_supplier=6, query=20
	// Settlement monitor disabled by default, so overhead = query_workers only (20)
	// On a machine with N CPUs: max(N×4, suppliers×6) + 20
	// For 5 suppliers: max(N×4, 30) + 20
	// This test uses 5 suppliers which should be CPU-bound on most machines
	size := cfg.GetMasterPoolSize(5)
	require.Greater(t, size, 20) // At minimum, overhead is 20 (no settlement workers)

	// Test auto-calculation with high supplier count (supplier-bound)
	// For 78 suppliers: max(N×4, 468) + 20 = 468 + 20 = 488 (on most machines)
	size = cfg.GetMasterPoolSize(78)
	// 78 × 6 = 468, plus overhead 20 = 488
	// This should be supplier-bound unless running on 117+ core machine
	require.GreaterOrEqual(t, size, 488)

	// Test with settlement monitor enabled (adds settlement_workers to overhead)
	cfg.SettlementMonitor.Enabled = true
	size = cfg.GetMasterPoolSize(78)
	// 78 × 6 = 468, plus overhead 22 (20 query + 2 settlement) = 490
	require.GreaterOrEqual(t, size, 490)
	cfg.SettlementMonitor.Enabled = false // reset

	// Test explicit override
	cfg.WorkerPools.MasterPoolSize = 500
	require.Equal(t, 500, cfg.GetMasterPoolSize(78))
	require.Equal(t, 500, cfg.GetMasterPoolSize(5)) // override ignores supplier count
}

// TestSMSTLiveRootCheckpointIntervalYAMLParsing verifies the full wire
// from config.miner.yaml (smst_live_root_checkpoint_interval key) down
// to the SMST manager's internal checkpoint cadence. A regression here
// would leave operators unable to tune the relay-loss-on-failover
// trade-off in production even though the knob exists.
func TestSMSTLiveRootCheckpointIntervalYAMLParsing(t *testing.T) {
	t.Run("parses configured value", func(t *testing.T) {
		yamlData := `smst_live_root_checkpoint_interval: 25`
		var cfg Config
		err := yaml.Unmarshal([]byte(yamlData), &cfg)
		require.NoError(t, err)
		require.Equal(t, 25, cfg.SMSTLiveRootCheckpointInterval)
	})

	t.Run("absent yields zero (manager falls back to default)", func(t *testing.T) {
		var cfg Config
		err := yaml.Unmarshal([]byte(""), &cfg)
		require.NoError(t, err)
		require.Equal(t, 0, cfg.SMSTLiveRootCheckpointInterval,
			"absent key must yield zero so the manager picks DefaultLiveRootCheckpointInterval")
	})
}

// TestSMSTLiveRootCheckpointInterval_PropagatesToManager is the integration
// side of the wiring check: build a RedisSMSTManagerConfig from a specific
// operator-provided interval and verify the manager actually applies it
// (as opposed to always using the default). This would have caught the
// gap where the YAML key existed but nothing downstream read it.
func TestSMSTLiveRootCheckpointInterval_PropagatesToManager(t *testing.T) {
	// Without a miniredis harness here we exercise only the config helper,
	// which is the single source of truth the manager consults. The
	// end-to-end test in smst_live_root_test.go's TestLiveRoot_CustomIntervalRespected
	// covers the behavioural side (writes respect the interval).
	cases := []struct {
		name     string
		input    int
		expected int
	}{
		{"explicit 1 = zero-loss mode", 1, 1},
		{"explicit 50", 50, 50},
		{"zero falls back to default", 0, DefaultLiveRootCheckpointInterval},
		{"negative falls back to default (defensive)", -5, DefaultLiveRootCheckpointInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &RedisSMSTManager{
				config: RedisSMSTManagerConfig{LiveRootCheckpointInterval: tc.input},
			}
			require.Equal(t, tc.expected, m.liveRootInterval())
		})
	}
}

func TestWorkerPoolConfigYAMLParsing(t *testing.T) {
	yamlData := `
worker_pools:
  master_pool_size: 150
  cpu_multiplier: 6
  workers_per_supplier: 3
  query_workers: 25
  settlement_workers: 4
`
	var cfg Config
	err := yaml.Unmarshal([]byte(yamlData), &cfg)
	require.NoError(t, err)

	require.Equal(t, 150, cfg.WorkerPools.MasterPoolSize)
	require.Equal(t, 6, cfg.WorkerPools.CPUMultiplier)
	require.Equal(t, 3, cfg.WorkerPools.WorkersPerSupplier)
	require.Equal(t, 25, cfg.WorkerPools.QueryWorkers)
	require.Equal(t, 4, cfg.WorkerPools.SettlementWorkers)

	// Test that getters use the parsed values
	require.Equal(t, 150, cfg.GetMasterPoolSize(100)) // explicit override
	require.Equal(t, 6, cfg.GetCPUMultiplier())
	require.Equal(t, 3, cfg.GetWorkersPerSupplier())
	require.Equal(t, 25, cfg.GetQueryWorkers())
	require.Equal(t, 4, cfg.GetSettlementWorkers())
}
