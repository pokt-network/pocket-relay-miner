package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Redis config now uses namespace configuration for stream prefix and consumer group
	// These are derived from the namespace config at runtime
	require.Equal(t, "redis://localhost:6379", cfg.Redis.URL)
	// Note: BlockTimeout removed - the consumer uses a bounded block (see
	// blockInterval in transport/redis/consumer.go), NOT BLOCK 0: an unbounded
	// block sets no read deadline, so a cancelled context cannot end the read
	// and Close() hangs.
	require.Equal(t, int64(60000), cfg.Redis.ClaimIdleTimeoutMs)
	require.Equal(t, int64(1000), cfg.BatchSize) // Increased from 100 for better throughput
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
		// Required since the transaction deadline became derived: there is no
		// default block time to fall back on, so a config without it is invalid.
		BlockTimeSeconds: 60,
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
	// The rule and its wording now live in keys.ValidateKeySources, shared with
	// the relayer so the two binaries cannot drift on what a key config means.
	require.Contains(t, err.Error(), "no key source configured")
	require.Contains(t, err.Error(), "keys_file")
	require.Contains(t, err.Error(), "keyring")
}

// TestLoadConfig_RetiredKeysAreNamedWithWhatTheyChanged replaces the two
// tombstone tests this file used to carry (keys.keys_dir and the top-level
// hot_reload_enabled).
//
// The tombstone STRUCT FIELDS are gone: a field per retired key is config that
// configures nothing, and it could never cover the case that actually bit us --
// a key that was never a field at all. What replaced them is a strict second
// decode of the same bytes, so this test drives the real path (file ->
// LoadConfig -> Warnings) instead of a struct literal, which is the stronger
// assertion: the struct literal could never have caught a typo.
//
// The retired-key SENTENCE is the part worth pinning. A bare "field not found"
// tells the operator a key is unknown; it does not tell them their keys were
// silently not loaded, which is the loss that earned the tombstone.
func TestLoadConfig_RetiredKeysAreNamedWithWhatTheyChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "miner.yaml")

	// A config that boots, carrying both retired keys. Neither may fail the
	// load: warn-and-start is the deliberate default, because a rolling deploy
	// lands a new binary beside an older ConfigMap as a matter of course.
	require.NoError(t, os.WriteFile(path, []byte(
		"redis:\n"+
			"  url: redis://localhost:6379\n"+
			"  consumer_name: miner-1\n"+
			"pocket_node:\n"+
			"  query_node_rpc_url: http://localhost:26657\n"+
			"  query_node_grpc_url: localhost:9090\n"+
			"keys:\n"+
			"  keys_file: /path/to/keys.yaml\n"+
			"  keys_dir: /etc/pocket/keys\n"+
			"block_time_seconds: 60\n"+
			"hot_reload_enabled: true\n"), 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err, "a retired key must NOT fail the load: the serving binary warns and starts")

	warnings := strings.Join(cfg.Warnings(), "\n")

	require.Contains(t, warnings, "keys_dir")
	require.Contains(t, warnings, "keys_file",
		"the advice must name the safe migration, not just the removed mechanism")

	require.Contains(t, warnings, "hot_reload_enabled")
	require.Contains(t, warnings, "keys.hot_reload_enabled",
		"the operator has to be told the NEW home, or the warning costs them a search")

	require.Contains(t, warnings, "REMOVED",
		"a retired key must read as removed, not merely unknown")
}

// TestLoadConfig_TheInclusionReconcilerSwitchIsRetired is the tooth on S11: the
// reconciler is CORE, so `disable_inclusion_reconciler` must reach the operator
// as a REMOVED key and not be quietly accepted. It drives the real path rather
// than the retiredKeys map, which is what makes it fail if anyone reintroduces
// the field: a struct that declares the key again stops UnknownKeys from
// reporting it, and every assertion below goes red at once.
//
// It must still not fail the load. A deployment that set this ran fire-once,
// and refusing to boot would turn its rolling deploy into an outage at exactly
// the moment the new binary is the one that would have saved its claims.
func TestLoadConfig_TheInclusionReconcilerSwitchIsRetired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "miner.yaml")

	require.NoError(t, os.WriteFile(path, []byte(
		"redis:\n"+
			"  url: redis://localhost:6379\n"+
			"  consumer_name: miner-1\n"+
			"pocket_node:\n"+
			"  query_node_rpc_url: http://localhost:26657\n"+
			"  query_node_grpc_url: localhost:9090\n"+
			"keys:\n"+
			"  keys_file: /path/to/keys.yaml\n"+
			"block_time_seconds: 60\n"+
			"transaction:\n"+
			"  disable_inclusion_reconciler: true\n"), 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err, "a retired key must NOT fail the load: warn-and-start is deliberate")

	warnings := strings.Join(cfg.Warnings(), "\n")

	require.Contains(t, warnings, "disable_inclusion_reconciler")
	require.Contains(t, warnings, "REMOVED",
		"the switch is gone, not merely unrecognised")
	require.Contains(t, warnings, "fire-once",
		"the sentence has to name what that deployment WAS doing -- an operator who "+
			"set this to true was running without verification or rebroadcast, and "+
			"telling them only that a key vanished hides the loss it was causing")
	require.Contains(t, warnings, "gas",
		"removing a switch IMPOSES a cost the operator did not choose -- the resends "+
			"pay gas and the verification queries their node once per supplier per "+
			"block. A tombstone that only lists what they gain is an advert")
	require.Contains(t, warnings, "max_rebroadcasts",
		"an operator who wanted the reconciler quiet needs the surviving knob that "+
			"gets closest to it, or the warning leaves them with no move")
}

// TestLoadConfig_AnUnknownKeyIsReportedButDoesNotFailTheLoad covers the case no
// tombstone could ever have covered: a key that was never a field. This is the
// shape that cost real money -- config.miner.example.yaml shipped a `suppliers:`
// block promising supplier filtering while no such field existed, so an operator
// who uncommented it believed they were filtering and the miner claimed for
// every key it held.
func TestLoadConfig_AnUnknownKeyIsReportedButDoesNotFailTheLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "miner.yaml")

	require.NoError(t, os.WriteFile(path, []byte(
		"redis:\n"+
			"  url: redis://localhost:6379\n"+
			"  consumer_name: miner-1\n"+
			"pocket_node:\n"+
			"  query_node_rpc_url: http://localhost:26657\n"+
			"  query_node_grpc_url: localhost:9090\n"+
			"keys:\n"+
			"  keys_file: /path/to/keys.yaml\n"+
			"block_time_seconds: 60\n"+
			"suppliers:\n"+
			"  - operator_address: pokt1abc\n"), 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	warnings := cfg.Warnings()
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "suppliers")
	require.Contains(t, warnings[0], "line ",
		"the operator must be pointed at the line, or a long config is a hunt")
	require.NotContains(t, warnings[0], "REMOVED",
		"a key that was never a field is unknown, not retired: calling it removed would be a lie")
}

// TestLoadConfig_AGoodConfigWarnsAboutNothing is the other half, and it is the
// one that keeps the warning worth reading. A false positive here trains the
// operator to ignore the output, which is worse than the silence it replaced.
func TestLoadConfig_AGoodConfigWarnsAboutNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "miner.yaml")

	require.NoError(t, os.WriteFile(path, []byte(
		"redis:\n"+
			"  url: redis://localhost:6379\n"+
			"  consumer_name: miner-1\n"+
			"pocket_node:\n"+
			"  query_node_rpc_url: http://localhost:26657\n"+
			"  query_node_grpc_url: localhost:9090\n"+
			"keys:\n"+
			"  keys_file: /path/to/keys.yaml\n"+
			"block_time_seconds: 60\n"), 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.Empty(t, cfg.Warnings())
}

// TestDefaultConfig_KeyHotReloadOn pins the default the operator gets when they
// say nothing. It is the counterpart of the tombstone above: moving the setting
// without moving its default is the same bug in the other direction, and it is
// the one that actually shipped.
func TestDefaultConfig_KeyHotReloadOn(t *testing.T) {
	require.True(t, DefaultConfig().Keys.HotReloadEnabled,
		"a miner that is told nothing must reload keys; the relayer already does")
}

// Note: Supplier validation tests removed - suppliers are auto-discovered from keys
// See TestConfig_Validate_NoKeySource for key validation

// Note: TestConfig_GetRedisBlockTimeout removed - the block duration is no longer
// configurable; it is the bounded blockInterval in transport/redis/consumer.go.

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

func TestGetMasterPoolSize(t *testing.T) {
	cfg := &Config{}

	// Test auto-calculation with small supplier count (CPU-bound)
	// With default values: cpu_multiplier=4, workers_per_supplier=6, query=20
	// Overhead = query_workers (20)
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

	// Test explicit override
	cfg.WorkerPools.MasterPoolSize = 500
	require.Equal(t, 500, cfg.GetMasterPoolSize(78))
	require.Equal(t, 500, cfg.GetMasterPoolSize(5)) // override ignores supplier count
}

func TestWorkerPoolConfigYAMLParsing(t *testing.T) {
	yamlData := `
worker_pools:
  master_pool_size: 150
  cpu_multiplier: 6
  workers_per_supplier: 3
  query_workers: 25
`
	var cfg Config
	err := yaml.Unmarshal([]byte(yamlData), &cfg)
	require.NoError(t, err)

	require.Equal(t, 150, cfg.WorkerPools.MasterPoolSize)
	require.Equal(t, 6, cfg.WorkerPools.CPUMultiplier)
	require.Equal(t, 3, cfg.WorkerPools.WorkersPerSupplier)
	require.Equal(t, 25, cfg.WorkerPools.QueryWorkers)

	// Test that getters use the parsed values
	require.Equal(t, 150, cfg.GetMasterPoolSize(100)) // explicit override
	require.Equal(t, 6, cfg.GetCPUMultiplier())
	require.Equal(t, 3, cfg.GetWorkersPerSupplier())
	require.Equal(t, 25, cfg.GetQueryWorkers())
}

// TestConfig_Validate_RejectsBadLoggingLevel pins that logging validation is
// wired into the miner's Validate: a typo'd level used to silently run the
// process at Info.
func TestConfig_Validate_RejectsBadLoggingLevel(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{
			RedisConfig: config.RedisConfig{
				URL: "redis://localhost:6379",
			},
			ConsumerName: "miner-1",
		},
		Logging: logging.Config{Level: "warning"},
	}

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "logging.level")
}

// A config that cannot say how fast its chain produces blocks must not start.
//
// This is the only protection left after the four tx_timeout knobs were retired.
// Before them there was a default of 30 seconds to fall back on, which at least
// produced a number; now an absent block time means WindowTimeout is called with
// zero, falls to the chain ceiling under the "unknown" regime, and every claim
// and proof carries a deadline nobody chose. Jorge's decision was explicit --
// "no moving to defaults, it doesn't start, so they fix it" -- and without a
// test a later refactor can delete the guard with nothing turning red.
//
// The fixtures are valid in EVERY other respect on purpose. The guard sits at
// the END of Validate, because putting it first masked the real first problem of
// a config with several errors, so a fixture with a second defect would pass
// this test for the wrong reason: it would be failing on the other one.
func TestConfig_Validate_BlockTimeSecondsIsRequired(t *testing.T) {
	otherwiseValid := func(blockTime int64) *Config {
		return &Config{
			Redis: RedisConfig{
				RedisConfig:  config.RedisConfig{URL: "redis://localhost:6379"},
				ConsumerName: "miner-1",
			},
			PocketNode: config.PocketNodeConfig{
				QueryNodeRPCUrl:  "http://localhost:26657",
				QueryNodeGRPCUrl: "localhost:9090",
			},
			Keys:             config.KeysConfig{KeysFile: "/path/to/keys.yaml"},
			BlockTimeSeconds: blockTime,
		}
	}

	for _, tc := range []struct {
		name      string
		blockTime int64
	}{
		{name: "absent", blockTime: 0},
		{name: "negative", blockTime: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := otherwiseValid(tc.blockTime).Validate()
			require.Error(t, err,
				"a config without a usable block time must refuse to start, not fall back to a default")
			require.Contains(t, err.Error(), "block_time_seconds",
				"the error must NAME the field: an operator reading it has to know what to fix")
		})
	}

	// The control. Without it, a Validate() that rejected everything would
	// satisfy both cases above.
	require.NoError(t, otherwiseValid(60).Validate(),
		"the same config with a positive block time must be valid")
}

// The gas price fallback is the mainnet minimum gas price, the same value
// DefaultConfig carries. It used to return 0.00001upokt, 10 times that, when
// transaction.gas_price was set to an empty string.
func TestConfig_GetTxGasPrice_FallbackIsMainnetMinimum(t *testing.T) {
	const mainnetMinGasPrice = "0.000001upokt"
	require.Equal(t, mainnetMinGasPrice, (&Config{}).GetTxGasPrice(), "empty gas_price falls back to the mainnet minimum")
	require.Equal(t, mainnetMinGasPrice, DefaultConfig().GetTxGasPrice(), "the default config carries the same value")
}
