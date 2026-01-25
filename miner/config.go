package miner

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/alitto/pond/v2"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// Config is the configuration for the HA Miner service.
type Config struct {
	// Redis configuration for consuming mined relays.
	Redis RedisConfig `yaml:"redis"`

	// PocketNode is the configuration for connecting to the Pocket blockchain.
	PocketNode config.PocketNodeConfig `yaml:"pocket_node"`

	// Keys configuration for loading supplier signing keys.
	Keys config.KeysConfig `yaml:"keys"`

	// Transaction configuration for claim/proof submission.
	Transaction TransactionConfig `yaml:"transaction,omitempty"`

	// Metrics configuration.
	Metrics config.MetricsConfig `yaml:"metrics"`

	// PProf configuration.
	PProf config.PprofConfig `yaml:"pprof"`

	// Logging configuration.
	Logging logging.Config `yaml:"logging"`

	// DeduplicationTTLBlocks is how many blocks to keep relay hashes for deduplication.
	// Default: 10 (session length + grace period + buffer)
	DeduplicationTTLBlocks int64 `yaml:"deduplication_ttl_blocks"`

	// BatchSize is the number of relays to process in a single batch.
	// Default: 100
	BatchSize int64 `yaml:"batch_size"`

	// AckBatchSize is the number of messages to acknowledge in a batch.
	// Default: 50
	AckBatchSize int64 `yaml:"ack_batch_size"`

	// HotReloadEnabled enables hot-reload of keys.
	// Default: true
	HotReloadEnabled bool `yaml:"hot_reload_enabled"`

	// SessionTTL is the TTL for session state data in Redis.
	// Default: CacheTTL (2h) - aligned with SMST tree TTL to prevent orphaned sessions.
	// Setting SessionTTL != CacheTTL can cause "SMST missing but relay count > 0" warnings.
	SessionTTL time.Duration `yaml:"session_ttl"`

	// CacheTTL is the TTL for Redis cached data (params, app stakes, service data, SMST trees).
	// This is a backup safety net - manual cleanup is primary, TTL prevents leaks if cleanup fails.
	// Default: 2h (covers ~15 session lifecycles at 30s blocks)
	CacheTTL time.Duration `yaml:"cache_ttl"`

	// KnownApplications is a list of application addresses to pre-discover at startup.
	// These apps will be fetched from the network and added to the cache during initialization.
	KnownApplications []string `yaml:"known_applications,omitempty"`

	// LeaderElection configures the global leader election for HA deployments.
	LeaderElection LeaderElectionConfig `yaml:"leader_election,omitempty"`

	// SessionLifecycle configures session lifecycle management.
	SessionLifecycle SessionLifecycleConfigYAML `yaml:"session_lifecycle,omitempty"`

	// BalanceMonitor configures balance and stake monitoring with alerts.
	BalanceMonitor BalanceMonitorConfigYAML `yaml:"balance_monitor,omitempty"`

	// BlockTimeSeconds is the expected block time in seconds.
	// This is used for timing calculations in caches, deduplication, and submission windows.
	// Default: 30
	BlockTimeSeconds int64 `yaml:"block_time_seconds,omitempty"`

	// BlockHealthMonitor configures block time health monitoring.
	BlockHealthMonitor BlockHealthConfig `yaml:"block_health_monitor,omitempty"`

	// DefaultServiceFactor is the global serviceFactor applied to all services.
	// If set, effectiveLimit = appStake × DefaultServiceFactor
	// If not set (0), use baseLimit formula: (appStake / numSuppliers) / proof_window_close_offset_blocks
	// Default: 0 (use baseLimit formula)
	DefaultServiceFactor float64 `yaml:"default_service_factor,omitempty"`

	// ServiceFactors is a map of per-service serviceFactor overrides.
	// Key: serviceID, Value: serviceFactor
	// Example: {"eth-mainnet": 0.007, "polygon": 0.003}
	// If a service has an override, it takes precedence over DefaultServiceFactor.
	ServiceFactors map[string]float64 `yaml:"service_factors,omitempty"`

	// WorkerPools configures worker pool sizing for parallel processing.
	// Auto-sizing formula: max(cpu × cpu_multiplier, suppliers × workers_per_supplier) + overhead
	WorkerPools WorkerPoolConfigYAML `yaml:"worker_pools,omitempty"`

	// Note: Supplier claiming is always enabled with hardcoded timing values.
	// See miner/supplier_claimer.go for the constants (ClaimTTL, RenewRate, etc.)
	// These values are NOT user-configurable to ensure production reliability.
}

// SessionLifecycleConfigYAML contains configuration for session lifecycle management.
type SessionLifecycleConfigYAML struct {
	// MaxConcurrentTransitions is the max number of sessions transitioning at once.
	// Default: 10
	MaxConcurrentTransitions int `yaml:"max_concurrent_transitions,omitempty"`
}

// LeaderElectionConfig contains configuration for distributed leader election.
type LeaderElectionConfig struct {
	// LeaderTTLSeconds is how long the leader lock lasts before expiring (in seconds).
	// The leader must renew the lock before this expires to maintain leadership.
	// Default: 30 seconds
	LeaderTTLSeconds int `yaml:"leader_ttl_seconds,omitempty"`

	// HeartbeatRateSeconds is how frequent to attempt to acquire/renew leadership (in seconds).
	// Should be less than LeaderTTLSeconds to ensure renewal before expiration.
	// Default: 10 seconds
	HeartbeatRateSeconds int `yaml:"heartbeat_rate_seconds,omitempty"`
}

// BalanceMonitorConfigYAML contains configuration for balance/stake monitoring.
type BalanceMonitorConfigYAML struct {
	// Enabled enables balance/stake monitoring.
	// Default: true
	Enabled bool `yaml:"enabled,omitempty"`

	// CheckIntervalSeconds is how frequent to check balances and stakes (in seconds).
	// Default: 300 (5 minutes)
	CheckIntervalSeconds int64 `yaml:"check_interval_seconds,omitempty"`

	// BalanceThresholdUpokt is the minimum balance in uPOKT before triggering warnings.
	// Operators should set this based on their operational needs.
	// Example: 1000 (1000 uPOKT)
	BalanceThresholdUpokt int64 `yaml:"balance_threshold_upokt,omitempty"`

	// StakeWarningProofThreshold is the number of missed proofs remaining before triggering a warning.
	// Warning triggers when: (stake - min_stake) / proof_missing_penalty < threshold
	// This is calculated dynamically based on protocol parameters.
	// Default: 10 (warn when less than 10 missed proofs away from auto-unstake)
	StakeWarningProofThreshold int64 `yaml:"stake_warning_proof_threshold,omitempty"`

	// StakeCriticalProofThreshold is the number of missed proofs remaining before triggering a critical alert.
	// Critical triggers when: (stake - min_stake) / proof_missing_penalty < threshold
	// Default: 3 (critical when less than 3 missed proofs away from auto-unstake)
	StakeCriticalProofThreshold int64 `yaml:"stake_critical_proof_threshold,omitempty"`
}

// BlockHealthConfig contains configuration for block time health monitoring.
type BlockHealthConfig struct {
	// Enabled enables block time health monitoring.
	// Default: false
	Enabled bool `yaml:"enabled,omitempty"`

	// SlownessThreshold is the multiplier for determining slow blocks.
	// If actualTime > configuredTime × threshold, a warning is logged.
	// Default: 1.5 (50% slower than expected)
	SlownessThreshold float64 `yaml:"slowness_threshold,omitempty"`
}

// WorkerPoolConfigYAML contains configuration for worker pool sizing.
// Worker pools control parallelism for claim/proof submission and background work.
// Auto-sizing formula: max(cpu × cpu_multiplier, suppliers × workers_per_supplier) + overhead
type WorkerPoolConfigYAML struct {
	// MasterPoolSize is the total master pool size.
	// Set to 0 for auto-calculation based on CPU and supplier count.
	// Default: 0 (auto-calculate)
	MasterPoolSize int `yaml:"master_pool_size,omitempty"`

	// CPUMultiplier is the multiplier for CPU-based sizing baseline.
	// Used in formula: cpu_count × cpu_multiplier
	// Default: 4
	CPUMultiplier int `yaml:"cpu_multiplier,omitempty"`

	// WorkersPerSupplier is the number of workers allocated per supplier.
	// With batching disabled, each session needs its own worker for claim submission.
	// Used in formula: num_suppliers × workers_per_supplier
	// Default: 6 (handles ~5-6 sessions per supplier unbatched)
	WorkersPerSupplier int `yaml:"workers_per_supplier,omitempty"`

	// QueryWorkers is the fixed number of workers for blockchain queries.
	// Used for startup queries, cache refresh, supplier registry.
	// Default: 20
	QueryWorkers int `yaml:"query_workers,omitempty"`

	// SettlementWorkers is the fixed number of workers for settlement event processing.
	// block_results can be 1GB+ on mainnet, needs dedicated workers.
	// Default: 2
	SettlementWorkers int `yaml:"settlement_workers,omitempty"`
}

// RedisConfig embeds shared RedisConfig and adds miner-specific fields.
type RedisConfig struct {
	config.RedisConfig `yaml:",inline"`

	// ConsumerName is the unique name of this miner instance.
	// Typically derived from the hostname / pod name.
	// If not set, auto-generated from the hostname.
	ConsumerName string `yaml:"consumer_name,omitempty"`

	// Note: Stream consumption uses BLOCK 0 (TRUE PUSH) for live consumption.
	// This is not configurable - messages are delivered instantly when available.

	// ClaimIdleTimeoutMs is how long a message can be pending before being claimed.
	// Default: 60000 (1 minute)
	ClaimIdleTimeoutMs int64 `yaml:"claim_idle_timeout_ms,omitempty"`
}

// TransactionConfig contains configuration for claim/proof transaction submission.
type TransactionConfig struct {
	// GasLimit is the gas limit for transactions.
	// Set to 0 for automatic gas estimation (simulation).
	// Set to a positive value for a fixed gas limit.
	// When set to 0, gas is estimated via simulation and multiplied by GasAdjustment.
	// Default: 0 (automatic estimation)
	GasLimit uint64 `yaml:"gas_limit,omitempty"`

	// GasPrice is the gas price per unit (e.g., "0.00001upokt").
	// Default: "0.00001upokt"
	GasPrice string `yaml:"gas_price,omitempty"`

	// GasAdjustment is the multiplier applied to simulated gas to add safety margin.
	// Only used when GasLimit=0 (automatic gas estimation).
	// Example: 1.7 means add 70% safety margin above simulated gas.
	// Default: 1.7
	GasAdjustment float64 `yaml:"gas_adjustment,omitempty"`

	// DisableClaimBatching disables batching of claim submissions.
	// When true, each session's claim is submitted in a separate transaction.
	// When false (default), claims with the same session end height are batched.
	// WORKAROUND: Set to true if experiencing claim failures due to one invalid claim in a batch.
	// Default: false (batching enabled for gas efficiency)
	DisableClaimBatching bool `yaml:"disable_claim_batching,omitempty"`

	// DisableProofBatching disables batching of proof submissions.
	// When true, each session's proof is submitted in a separate transaction.
	// When false (default), proofs with the same session end height are batched.
	// WORKAROUND: Set to true if experiencing proof failures due to difficulty validation
	// or other issues where one invalid proof causes the entire batch to fail.
	// Default: false (batching enabled for gas efficiency)
	DisableProofBatching bool `yaml:"disable_proof_batching,omitempty"`
}

type SupplierConfig struct {
	// OperatorAddress is the supplier's operator address (bech32).
	OperatorAddress string `yaml:"operator_address"`

	// SigningKeyName is the name of the key in the keyring used for signing.
	SigningKeyName string `yaml:"signing_key_name"`

	// Services is a list of service IDs this supplier serves.
	// Used for filtering relays from the stream.
	Services []string `yaml:"services,omitempty"`
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if c.Redis.URL == "" {
		return fmt.Errorf("redis.url is required")
	}

	if _, err := url.Parse(c.Redis.URL); err != nil {
		return fmt.Errorf("invalid redis.url: %w", err)
	}

	// ConsumerName is optional - auto-generated if not set
	// ConsumerGroup is derived from namespace config

	// Validate Redis pool settings (all are optional, 0 = use defaults)
	if c.Redis.PoolSize < 0 {
		return fmt.Errorf("redis.pool_size must be >= 0 (0 = use default)")
	}
	if c.Redis.MinIdleConns < 0 {
		return fmt.Errorf("redis.min_idle_conns must be >= 0 (0 = use default)")
	}
	if c.Redis.PoolTimeoutSeconds < 0 {
		return fmt.Errorf("redis.pool_timeout_seconds must be >= 0 (0 = use default)")
	}
	if c.Redis.ConnMaxIdleTimeSeconds < 0 {
		return fmt.Errorf("redis.conn_max_idle_time_seconds must be >= 0 (0 = use default)")
	}

	if c.PocketNode.QueryNodeRPCUrl == "" {
		return fmt.Errorf("pocket_node.query_node_rpc_url is required")
	}

	if c.PocketNode.QueryNodeGRPCUrl == "" {
		return fmt.Errorf("pocket_node.query_node_grpc_url is required")
	}

	// Keys config is required (suppliers are auto-discovered from keys)
	if !c.HasKeySource() {
		return fmt.Errorf("keys config is required (at least one of: keys_file, keys_dir, or keyring)")
	}

	// Validate keyring config if provided
	if c.Keys.Keyring != nil && c.Keys.Keyring.Backend != "" {
		validBackends := map[string]bool{"file": true, "os": true, "test": true, "memory": true}
		if !validBackends[c.Keys.Keyring.Backend] {
			return fmt.Errorf("invalid keys.keyring.backend: %s", c.Keys.Keyring.Backend)
		}
	}

	// Validate leader election: heartbeat must be less than TTL
	if c.LeaderElection.HeartbeatRateSeconds > 0 && c.LeaderElection.LeaderTTLSeconds > 0 {
		if c.LeaderElection.HeartbeatRateSeconds >= c.LeaderElection.LeaderTTLSeconds {
			return fmt.Errorf("leader_election.heartbeat_rate_seconds (%d) must be less than leader_ttl_seconds (%d) to prevent lock expiration before renewal",
				c.LeaderElection.HeartbeatRateSeconds, c.LeaderElection.LeaderTTLSeconds)
		}
	}

	// Note: Storage validation removed - all session trees now use Redis

	return nil
}

// GetClaimIdleTimeout returns the claim idle timeout as a duration.
func (c *Config) GetClaimIdleTimeout() time.Duration {
	if c.Redis.ClaimIdleTimeoutMs > 0 {
		return time.Duration(c.Redis.ClaimIdleTimeoutMs) * time.Millisecond
	}
	return time.Minute // Default
}

// GetBatchSize returns the batch size with defaults.
func (c *Config) GetBatchSize() int64 {
	if c.BatchSize > 0 {
		return c.BatchSize
	}
	return 1000 // Default (increased from 100 for better throughput)
}

// GetAckBatchSize returns the ack batch size with defaults.
func (c *Config) GetAckBatchSize() int64 {
	if c.AckBatchSize > 0 {
		return c.AckBatchSize
	}
	return 50 // Default
}

// GetTxGasLimit returns the transaction gas limit with defaults.
// Returns 0 for automatic gas estimation (simulation).
func (c *Config) GetTxGasLimit() uint64 {
	// Note: GasLimit defaults to 0 if not set, which means auto/simulation mode
	return c.Transaction.GasLimit
}

// GetTxGasPrice returns the transaction gas price with defaults.
func (c *Config) GetTxGasPrice() string {
	if c.Transaction.GasPrice != "" {
		return c.Transaction.GasPrice
	}
	return "0.00001upokt" // Default: 0.00001 upokt (10x higher than previous default)
}

// GetTxGasAdjustment returns the gas adjustment multiplier with defaults.
// Only used when GasLimit=0 (automatic gas estimation).
func (c *Config) GetTxGasAdjustment() float64 {
	if c.Transaction.GasAdjustment > 0 {
		return c.Transaction.GasAdjustment
	}
	return 1.7 // Default: 1.7 (adds 70% safety margin to simulated gas)
}

// GetDeduplicationTTL returns the deduplication TTL in blocks.
func (c *Config) GetDeduplicationTTL() int64 {
	if c.DeduplicationTTLBlocks > 0 {
		return c.DeduplicationTTLBlocks
	}
	return 10 // Default (session length + grace + buffer)
}

// GetLeaderTTL returns the leader TTL as a duration.
func (c *Config) GetLeaderTTL() time.Duration {
	if c.LeaderElection.LeaderTTLSeconds > 0 {
		return time.Duration(c.LeaderElection.LeaderTTLSeconds) * time.Second
	}
	return 30 * time.Second // Default
}

// GetLeaderHeartbeatRate returns the leader heartbeat rate as a duration.
func (c *Config) GetLeaderHeartbeatRate() time.Duration {
	if c.LeaderElection.HeartbeatRateSeconds > 0 {
		return time.Duration(c.LeaderElection.HeartbeatRateSeconds) * time.Second
	}
	return 10 * time.Second // Default
}

// GetSessionLifecycleMaxConcurrentTransitions returns the max concurrent transitions.
func (c *Config) GetSessionLifecycleMaxConcurrentTransitions() int {
	if c.SessionLifecycle.MaxConcurrentTransitions > 0 {
		return c.SessionLifecycle.MaxConcurrentTransitions
	}
	return 10 // Default
}

// GetBalanceMonitorEnabled returns whether balance monitoring is enabled.
func (c *Config) GetBalanceMonitorEnabled() bool {
	// Default to true if not explicitly set
	return c.BalanceMonitor.Enabled
}

// GetBalanceMonitorCheckInterval returns the balance check interval as a duration.
func (c *Config) GetBalanceMonitorCheckInterval() time.Duration {
	if c.BalanceMonitor.CheckIntervalSeconds > 0 {
		return time.Duration(c.BalanceMonitor.CheckIntervalSeconds) * time.Second
	}
	return 5 * time.Minute // Default: 5 minutes
}

// GetBalanceMonitorThreshold returns the balance threshold in uPOKT.
func (c *Config) GetBalanceMonitorThreshold() int64 {
	return c.BalanceMonitor.BalanceThresholdUpokt
}

// GetBalanceMonitorStakeWarningProofThreshold returns the warning threshold in missed proofs.
func (c *Config) GetBalanceMonitorStakeWarningProofThreshold() int64 {
	if c.BalanceMonitor.StakeWarningProofThreshold > 0 {
		return c.BalanceMonitor.StakeWarningProofThreshold
	}
	return 10 // Default: warn when < 10 missed proofs remaining
}

// GetBalanceMonitorStakeCriticalProofThreshold returns the critical threshold in missed proofs.
func (c *Config) GetBalanceMonitorStakeCriticalProofThreshold() int64 {
	if c.BalanceMonitor.StakeCriticalProofThreshold > 0 {
		return c.BalanceMonitor.StakeCriticalProofThreshold
	}
	return 3 // Default: critical when < 3 missed proofs remaining
}

// GetBlockTimeSeconds returns the configured block time in seconds.
func (c *Config) GetBlockTimeSeconds() int64 {
	if c.BlockTimeSeconds > 0 {
		return c.BlockTimeSeconds
	}
	return 30 // Default: 30s
}

// GetBlockHealthSlownessThreshold returns the slowness threshold for block health monitoring.
func (c *Config) GetBlockHealthSlownessThreshold() float64 {
	if c.BlockHealthMonitor.SlownessThreshold > 0 {
		return c.BlockHealthMonitor.SlownessThreshold
	}
	return 1.5 // Default: 50% slower than expected
}

// GetCacheTTL returns the cache TTL for Redis cached data.
func (c *Config) GetCacheTTL() time.Duration {
	if c.CacheTTL > 0 {
		return c.CacheTTL
	}
	return 2 * time.Hour // Default: 2h (covers ~15 session lifecycles at 30s blocks)
}

// GetSessionTTL returns the session TTL for session state data.
// Defaults to CacheTTL if not explicitly set, ensuring SMST trees and sessions
// expire at the same time (prevents orphaned sessions causing false positive warnings).
func (c *Config) GetSessionTTL() time.Duration {
	if c.SessionTTL > 0 {
		return c.SessionTTL
	}
	return c.GetCacheTTL() // Default: align with CacheTTL
}

// GetQueryTimeout returns the blockchain query timeout as a duration.
func (c *Config) GetQueryTimeout() time.Duration {
	if c.PocketNode.QueryTimeoutSeconds > 0 {
		return time.Duration(c.PocketNode.QueryTimeoutSeconds) * time.Second
	}
	return 5 * time.Second // Default: 5s
}

// GetServiceFactor returns the serviceFactor for a specific service.
// Returns (factor, hasServiceFactor):
// - If a service has an override in ServiceFactors, returns (override, true)
// - If DefaultServiceFactor is set (>0), returns (default, true)
// - Otherwise returns (0, false) meaning use baseLimit formula
func (c *Config) GetServiceFactor(serviceID string) (float64, bool) {
	// Check per-service override first
	if factor, exists := c.ServiceFactors[serviceID]; exists && factor > 0 {
		return factor, true
	}

	// Fall back to default
	if c.DefaultServiceFactor > 0 {
		return c.DefaultServiceFactor, true
	}

	return 0, false
}

// GetSupplierClaimingConfig returns the SupplierClaimerConfig for supplier claiming.
// All timing values are hardcoded constants to ensure production reliability.
// See supplier_claimer.go for the constant definitions and documentation.
func (c *Config) GetSupplierClaimingConfig() SupplierClaimerConfig {
	// Always return hardcoded constants - these are NOT user-configurable
	// to prevent operators from accidentally breaking the claiming system.
	return SupplierClaimerConfig{
		ClaimTTL:              ClaimTTL,
		RenewRate:             RenewRate,
		InstanceTTL:           InstanceTTL,
		InstanceHeartbeatRate: InstanceHeartbeatRate,
		RebalanceInterval:     RebalanceInterval,
	}
}

// GetMasterPoolSize returns the master pool size, auto-calculating if not explicitly set.
// Formula: max(cpu × cpu_multiplier, suppliers × workers_per_supplier) + overhead
// Example (4 CPU, 78 suppliers): max(4×4, 78×6) + 22 = max(16, 468) + 22 = 490
func (c *Config) GetMasterPoolSize(numSuppliers int) int {
	if c.WorkerPools.MasterPoolSize > 0 {
		return c.WorkerPools.MasterPoolSize
	}
	// Auto-calculate based on CPU and supplier count
	// Use getEffectiveCPUCount() which respects GOMAXPROCS for container environments
	cpuBased := getEffectiveCPUCount() * c.GetCPUMultiplier()
	supplierBased := numSuppliers * c.GetWorkersPerSupplier()
	overhead := c.GetQueryWorkers() + c.GetSettlementWorkers()

	baseSize := cpuBased
	if supplierBased > cpuBased {
		baseSize = supplierBased
	}
	return baseSize + overhead
}

// getEffectiveCPUCount returns the effective CPU count for the process.
// Uses runtime.GOMAXPROCS(0) which returns the current value set by automaxprocs
// (cgroup-aware) or falls back to runtime.NumCPU() if not limited.
func getEffectiveCPUCount() int {
	// runtime.GOMAXPROCS(0) returns current value without changing it.
	// automaxprocs (imported in main.go) sets this based on cgroup limits at init().
	return runtime.GOMAXPROCS(0)
}

// CreateBoundedSubpool creates a subpool with size capped to the parent pool's max.
// If requested size exceeds parent max, it logs a warning and uses the parent max.
// This prevents panics from misconfiguration while alerting operators.
func CreateBoundedSubpool(logger logging.Logger, pool pond.Pool, requestedSize int, name string) pond.Pool {
	parentMax := pool.MaxConcurrency()
	actualSize := requestedSize

	if requestedSize > parentMax {
		logger.Warn().
			Str("subpool", name).
			Int("requested_size", requestedSize).
			Int("parent_max", parentMax).
			Int("actual_size", parentMax).
			Msg("subpool size exceeds parent pool max, capping to parent max")
		actualSize = parentMax
	}

	return pool.NewSubpool(actualSize)
}

// GetCPUMultiplier returns the CPU multiplier for pool sizing.
// Default: 4
func (c *Config) GetCPUMultiplier() int {
	if c.WorkerPools.CPUMultiplier > 0 {
		return c.WorkerPools.CPUMultiplier
	}
	return 4 // Default
}

// GetWorkersPerSupplier returns the number of workers per supplier.
// Default: 6 (handles unbatched claims with up to 6 sessions per supplier)
// With batching disabled, each session needs its own worker for claim submission.
// Formula: suppliers × workers_per_supplier should cover max concurrent claims.
func (c *Config) GetWorkersPerSupplier() int {
	if c.WorkerPools.WorkersPerSupplier > 0 {
		return c.WorkerPools.WorkersPerSupplier
	}
	return 6 // Default: handles ~5-6 sessions per supplier unbatched
}

// GetQueryWorkers returns the fixed number of query workers.
// Default: 20
func (c *Config) GetQueryWorkers() int {
	if c.WorkerPools.QueryWorkers > 0 {
		return c.WorkerPools.QueryWorkers
	}
	return 20 // Default
}

// GetSettlementWorkers returns the fixed number of settlement workers.
// Default: 2
func (c *Config) GetSettlementWorkers() int {
	if c.WorkerPools.SettlementWorkers > 0 {
		return c.WorkerPools.SettlementWorkers
	}
	return 2 // Default
}

// GetChainID returns the chain ID for transaction signing.
// Default: "pocket" (mainnet) for backward compatibility
func (c *Config) GetChainID() string {
	if c.PocketNode.ChainID != "" {
		return c.PocketNode.ChainID
	}
	return "pocket" // Default: mainnet
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Redis: RedisConfig{
			RedisConfig: config.RedisConfig{
				URL: "redis://localhost:6379",
				// Namespace uses defaults (ha:cache, ha:events, ha-miners, etc.)
			},
			// Note: BlockTimeout removed - BLOCK 0 (TRUE PUSH) is now hardcoded in consumer
			ClaimIdleTimeoutMs: 60000,
		},
		Metrics: config.MetricsConfig{
			Enabled: true,
			Addr:    ":9092",
		},
		Logging: logging.Config{
			Level:           "info",
			Format:          "json",
			Async:           true,
			AsyncBufferSize: 100000,
		},
		Transaction: TransactionConfig{
			GasLimit:             0,               // 0 = automatic gas estimation via simulation
			GasPrice:             "0.000001upokt", // Default gas price
			GasAdjustment:        1.7,             // Default 70% safety margin
			DisableClaimBatching: true,            // Default: true (WORKAROUND for difficulty validation failures)
			DisableProofBatching: true,            // Default: true (WORKAROUND for difficulty validation failures)
		},
		DeduplicationTTLBlocks: 10,
		BatchSize:              1000, // Increased from 100 for better throughput (10x more efficient)
		AckBatchSize:           50,
		HotReloadEnabled:       true,
		// SessionTTL: 0 means use CacheTTL (default 2h) - ensures SMST trees and sessions expire together
		// This prevents orphaned sessions causing "SMST missing but relay count > 0" warnings
		CacheTTL: 2 * time.Hour, // Covers ~15 session lifecycles at 30s blocks
		BalanceMonitor: BalanceMonitorConfigYAML{
			Enabled:                     true,    // Enable by default
			BalanceThresholdUpokt:       1000000, // 1 POKT = 1,000,000 upokt
			StakeWarningProofThreshold:  10,      // Warn when < 10 missed proofs remaining
			StakeCriticalProofThreshold: 3,       // Critical when < 3 missed proofs remaining
		},
	}
}

// LoadConfig loads a miner configuration from a YAML file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cf file: %w", err)
	}

	// Start with defaults
	cf := DefaultConfig()

	if err = yaml.Unmarshal(data, cf); err != nil {
		return nil, fmt.Errorf("failed to parse cf file: %w", err)
	}

	// Generate consumer name from hostname if not set
	if cf.Redis.ConsumerName == "" {
		hostname, _ := os.Hostname()
		cf.Redis.ConsumerName = fmt.Sprintf("miner-%s-%d", hostname, os.Getpid())
	}

	if err = cf.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cf: %w", err)
	}

	return cf, nil
}

// HasKeySource returns true if at least one key source is configured.
func (c *Config) HasKeySource() bool {
	return c.Keys.KeysFile != "" ||
		c.Keys.KeysDir != "" ||
		(c.Keys.Keyring != nil && c.Keys.Keyring.Backend != "")
}
