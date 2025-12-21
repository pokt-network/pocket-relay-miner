package miner

import (
	"fmt"
	"net/url"
	"os"
	"time"

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

	// SessionTTL is the TTL for session data.
	// Default: 24h
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

	// Note: Supplier claiming is always enabled with hardcoded timing values.
	// See miner/supplier_claimer.go for the constants (ClaimTTL, RenewRate, etc.)
	// These values are NOT user-configurable to ensure production reliability.
}

// SessionLifecycleConfigYAML contains configuration for session lifecycle management.
type SessionLifecycleConfigYAML struct {
	// WindowStartBufferBlocks is blocks after a window open to wait before the earliest submission.
	// This spreads out supplier submissions more evenly across the window.
	// Default: 10
	WindowStartBufferBlocks int64 `yaml:"window_start_buffer_blocks,omitempty"`

	// ClaimSubmissionBuffer is blocks before claim window close to start claiming.
	// This provides buffer time for transaction confirmation.
	// Default: 2
	ClaimSubmissionBuffer int64 `yaml:"claim_submission_buffer,omitempty"`

	// ProofSubmissionBuffer is blocks before a proof window close to start proving.
	// Default: 2
	ProofSubmissionBuffer int64 `yaml:"proof_submission_buffer,omitempty"`

	// MaxConcurrentTransitions is the max number of sessions transitioning at once.
	// Default: 10
	MaxConcurrentTransitions int `yaml:"max_concurrent_transitions,omitempty"`

	// StreamDiscoveryIntervalSeconds is how often to scan for new session streams (in seconds).
	// This controls how quickly new session streams are discovered for consumption.
	// Default: 10 seconds
	StreamDiscoveryIntervalSeconds int64 `yaml:"stream_discovery_interval_seconds,omitempty"`
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

// SupplierConfig contains configuration for a single supplier.
// DEPRECATED: This type is no longer used in production code.
// Suppliers are auto-discovered from keys configuration.
// Kept only for legacy test compatibility.
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

// GetSessionLifecycleWindowStartBuffer returns the window start buffer in blocks.
// TODO: This is not yet wired up in production code.
// The SubmissionTimingCalculator (miner/submission_timing.go) uses WindowStartBufferBlocks,
// but it's only instantiated in tests. Wire this config value through when the timing
// calculator is integrated into the session lifecycle manager.
func (c *Config) GetSessionLifecycleWindowStartBuffer() int64 {
	if c.SessionLifecycle.WindowStartBufferBlocks > 0 {
		return c.SessionLifecycle.WindowStartBufferBlocks
	}
	return 10 // Default
}

// GetSessionLifecycleClaimBuffer returns the claim submission buffer in blocks.
func (c *Config) GetSessionLifecycleClaimBuffer() int64 {
	if c.SessionLifecycle.ClaimSubmissionBuffer > 0 {
		return c.SessionLifecycle.ClaimSubmissionBuffer
	}
	return 2 // Default
}

// GetSessionLifecycleProofBuffer returns the proof submission buffer in blocks.
func (c *Config) GetSessionLifecycleProofBuffer() int64 {
	if c.SessionLifecycle.ProofSubmissionBuffer > 0 {
		return c.SessionLifecycle.ProofSubmissionBuffer
	}
	return 2 // Default
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

// GetStreamDiscoveryInterval returns the stream discovery interval as a duration.
func (c *Config) GetStreamDiscoveryInterval() time.Duration {
	if c.SessionLifecycle.StreamDiscoveryIntervalSeconds > 0 {
		return time.Duration(c.SessionLifecycle.StreamDiscoveryIntervalSeconds) * time.Second
	}
	return 10 * time.Second // Default: 10 seconds
}

// GetCacheTTL returns the cache TTL for Redis cached data.
func (c *Config) GetCacheTTL() time.Duration {
	if c.CacheTTL > 0 {
		return c.CacheTTL
	}
	return 2 * time.Hour // Default: 2h (covers ~15 session lifecycles at 30s blocks)
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
		DeduplicationTTLBlocks: 10,
		BatchSize:              1000, // Increased from 100 for better throughput (10x more efficient)
		AckBatchSize:           50,
		HotReloadEnabled:       true,
		SessionTTL:             24 * time.Hour,
		CacheTTL:               2 * time.Hour, // Covers ~15 session lifecycles at 30s blocks
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
