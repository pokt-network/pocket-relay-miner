package miner

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// Config is the configuration for the HA Miner service.
type Config struct {
	// Redis configuration for consuming mined relays.
	Redis RedisConfig `yaml:"redis"`

	// PocketNode is the configuration for connecting to the Pocket blockchain.
	PocketNode PocketNodeConfig `yaml:"pocket_node"`

	// Keys configuration for loading supplier signing keys.
	Keys KeysConfig `yaml:"keys"`

	// Suppliers is a list of supplier configurations this miner manages.
	// Each supplier has its own session trees, claims, and proofs.
	Suppliers []SupplierConfig `yaml:"suppliers"`

	// Metrics configuration.
	Metrics MetricsConfig `yaml:"metrics"`

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
}

// SessionLifecycleConfigYAML contains configuration for session lifecycle management.
type SessionLifecycleConfigYAML struct {
	// WindowStartBufferBlocks is blocks after window open to wait before earliest submission.
	// This spreads out supplier submissions more evenly across the window.
	// Default: 10
	WindowStartBufferBlocks int64 `yaml:"window_start_buffer_blocks,omitempty"`

	// ClaimSubmissionBuffer is blocks before claim window close to start claiming.
	// This provides buffer time for transaction confirmation.
	// Default: 2
	ClaimSubmissionBuffer int64 `yaml:"claim_submission_buffer,omitempty"`

	// ProofSubmissionBuffer is blocks before proof window close to start proving.
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

	// HeartbeatRateSeconds is how often to attempt acquire/renew leadership (in seconds).
	// Should be less than LeaderTTLSeconds to ensure renewal before expiration.
	// Default: 10 seconds
	HeartbeatRateSeconds int `yaml:"heartbeat_rate_seconds,omitempty"`
}

// BalanceMonitorConfigYAML contains configuration for balance/stake monitoring.
type BalanceMonitorConfigYAML struct {
	// Enabled enables balance/stake monitoring.
	// Default: true
	Enabled bool `yaml:"enabled,omitempty"`

	// CheckIntervalSeconds is how often to check balances and stakes (in seconds).
	// Default: 300 (5 minutes)
	CheckIntervalSeconds int64 `yaml:"check_interval_seconds,omitempty"`

	// BalanceThresholdUpokt is the minimum balance in uPOKT before triggering warnings.
	// Operators should set this based on their operational needs.
	// Example: 1000 (1000 uPOKT)
	BalanceThresholdUpokt int64 `yaml:"balance_threshold_upokt,omitempty"`

	// StakeWarningProofThreshold is the number of missed proofs remaining before triggering a warning.
	// Warning triggers when: (stake - min_stake) / proof_missing_penalty < threshold
	// This is calculated dynamically based on protocol parameters.
	// Default: 1000 (warn when less than 1000 missed proofs away from auto-unstake)
	StakeWarningProofThreshold int64 `yaml:"stake_warning_proof_threshold,omitempty"`

	// StakeCriticalProofThreshold is the number of missed proofs remaining before triggering a critical alert.
	// Critical triggers when: (stake - min_stake) / proof_missing_penalty < threshold
	// Default: 100 (critical when less than 100 missed proofs away from auto-unstake)
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

// KeysConfig contains key provider configuration.
type KeysConfig struct {
	// KeysFile is the path to a supplier.yaml file with hex-encoded keys.
	KeysFile string `yaml:"keys_file,omitempty"`

	// KeysDir is a directory containing individual key files.
	KeysDir string `yaml:"keys_dir,omitempty"`

	// Keyring configuration for Cosmos SDK keyring.
	Keyring *KeyringConfig `yaml:"keyring,omitempty"`
}

// KeyringConfig contains Cosmos SDK keyring configuration.
type KeyringConfig struct {
	// Backend is the keyring backend type: "file", "os", "test", "memory"
	Backend string `yaml:"backend"`

	// Dir is the directory containing the keyring (for "file" backend).
	Dir string `yaml:"dir,omitempty"`

	// AppName is the application name for the keyring.
	// Default: "pocket"
	AppName string `yaml:"app_name,omitempty"`

	// KeyNames is an optional list of specific key names to load.
	KeyNames []string `yaml:"key_names,omitempty"`
}

// RedisConfig contains Redis connection configuration.
type RedisConfig struct {
	// URL is the Redis connection URL.
	URL string `yaml:"url"`

	// StreamPrefix is the prefix for Redis stream names.
	StreamPrefix string `yaml:"stream_prefix"`

	// ConsumerGroup is the consumer group name for this miner cluster.
	// All miner instances for the same supplier should use the same group.
	ConsumerGroup string `yaml:"consumer_group"`

	// ConsumerName is the unique name of this miner instance.
	// Typically derived from hostname/pod name.
	ConsumerName string `yaml:"consumer_name"`

	// BlockTimeout is how long to wait for new messages (milliseconds).
	// Default: 5000 (5 seconds)
	BlockTimeoutMs int64 `yaml:"block_timeout_ms"`

	// ClaimIdleTimeoutMs is how long a message can be pending before being claimed.
	// Default: 60000 (1 minute)
	ClaimIdleTimeoutMs int64 `yaml:"claim_idle_timeout_ms"`

	// PoolSize is the maximum number of socket connections.
	// Default: 20 × runtime.GOMAXPROCS (2x go-redis default for production)
	// Set to 0 to use go-redis default (10 × GOMAXPROCS)
	PoolSize int `yaml:"pool_size,omitempty"`

	// MinIdleConns is the minimum number of idle connections to maintain.
	// Keeping idle connections warm eliminates connection dial latency (~1-5ms).
	// Default: PoolSize / 4
	// Set to 0 to disable (connections created on demand)
	MinIdleConns int `yaml:"min_idle_conns,omitempty"`

	// PoolTimeout is the amount of time to wait for a connection from the pool.
	// Default: 4 seconds
	// Set to 0 to wait indefinitely
	PoolTimeoutSeconds int `yaml:"pool_timeout_seconds,omitempty"`

	// ConnMaxIdleTime is the maximum amount of time a connection can be idle.
	// Idle connections older than this are closed.
	// Default: 5 minutes
	// Set to 0 to disable (connections never closed due to idle time)
	ConnMaxIdleTimeSeconds int `yaml:"conn_max_idle_time_seconds,omitempty"`
}

// PocketNodeConfig contains Pocket blockchain connection configuration.
type PocketNodeConfig struct {
	// QueryNodeRPCUrl is the URL for RPC queries and transaction submission.
	QueryNodeRPCUrl string `yaml:"query_node_rpc_url"`

	// QueryNodeGRPCUrl is the URL for gRPC queries.
	QueryNodeGRPCUrl string `yaml:"query_node_grpc_url"`

	// GRPCInsecure disables TLS for gRPC connections.
	// Default: false (use TLS for secure connections)
	GRPCInsecure bool `yaml:"grpc_insecure,omitempty"`
}

// SupplierConfig contains configuration for a single supplier.
type SupplierConfig struct {
	// OperatorAddress is the supplier's operator address (bech32).
	OperatorAddress string `yaml:"operator_address"`

	// SigningKeyName is the name of the key in the keyring used for signing.
	SigningKeyName string `yaml:"signing_key_name"`

	// Services is a list of service IDs this supplier serves.
	// Used for filtering relays from the stream.
	Services []string `yaml:"services,omitempty"`
}

// MetricsConfig contains Prometheus metrics configuration.
type MetricsConfig struct {
	// Enabled enables metrics collection.
	Enabled bool `yaml:"enabled"`

	// Addr is the address to expose metrics on.
	// Default: ":9092"
	Addr string `yaml:"addr"`

	// PprofEnabled enables pprof profiling server.
	// Default: false (disabled for production safety)
	PprofEnabled bool `yaml:"pprof_enabled,omitempty"`

	// PprofAddr is the address for pprof server.
	// Default: "localhost:6060" (localhost only for security)
	PprofAddr string `yaml:"pprof_addr,omitempty"`
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if c.Redis.URL == "" {
		return fmt.Errorf("redis.url is required")
	}

	if _, err := url.Parse(c.Redis.URL); err != nil {
		return fmt.Errorf("invalid redis.url: %w", err)
	}

	if c.Redis.StreamPrefix == "" {
		return fmt.Errorf("redis.stream_prefix is required")
	}

	if c.Redis.ConsumerGroup == "" {
		return fmt.Errorf("redis.consumer_group is required")
	}

	if c.Redis.ConsumerName == "" {
		return fmt.Errorf("redis.consumer_name is required")
	}

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

	// Either keys config or explicit suppliers must be configured
	if !c.HasKeySource() && len(c.Suppliers) == 0 {
		return fmt.Errorf("either keys config or at least one supplier must be configured")
	}

	// Validate explicit suppliers if configured
	for i, supplier := range c.Suppliers {
		if err := c.validateSupplierConfig(i, supplier); err != nil {
			return err
		}
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

// validateSupplierConfig validates a single supplier configuration.
func (c *Config) validateSupplierConfig(index int, supplier SupplierConfig) error {
	if supplier.OperatorAddress == "" {
		return fmt.Errorf("suppliers[%d].operator_address is required", index)
	}

	if supplier.SigningKeyName == "" {
		return fmt.Errorf("suppliers[%d].signing_key_name is required", index)
	}

	return nil
}

// GetRedisBlockTimeout returns the Redis block timeout as a duration.
func (c *Config) GetRedisBlockTimeout() time.Duration {
	if c.Redis.BlockTimeoutMs > 0 {
		return time.Duration(c.Redis.BlockTimeoutMs) * time.Millisecond
	}
	return 5 * time.Second // Default
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
	return 1000 // Default: warn when < 1000 missed proofs remaining
}

// GetBalanceMonitorStakeCriticalProofThreshold returns the critical threshold in missed proofs.
func (c *Config) GetBalanceMonitorStakeCriticalProofThreshold() int64 {
	if c.BalanceMonitor.StakeCriticalProofThreshold > 0 {
		return c.BalanceMonitor.StakeCriticalProofThreshold
	}
	return 100 // Default: critical when < 100 missed proofs remaining
}

// GetBlockTimeSeconds returns the configured block time in seconds.
func (c *Config) GetBlockTimeSeconds() int64 {
	if c.BlockTimeSeconds > 0 {
		return c.BlockTimeSeconds
	}
	return 30 // Default: 30s (not 6s - that was old testnet value)
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

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Redis: RedisConfig{
			URL:                "redis://localhost:6379",
			StreamPrefix:       "ha:relays",
			ConsumerGroup:      "ha-miners",
			BlockTimeoutMs:     5000,
			ClaimIdleTimeoutMs: 60000,
		},
		Metrics: MetricsConfig{
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
		BalanceMonitor: BalanceMonitorConfigYAML{
			Enabled:                     true,    // Enable by default
			BalanceThresholdUpokt:       1000000, // 1 POKT = 1,000,000 upokt
			StakeWarningProofThreshold:  1000,    // Warn when < 1000 missed proofs remaining
			StakeCriticalProofThreshold: 100,     // Critical when < 100 missed proofs remaining
		},
	}
}

// LoadConfig loads a miner configuration from a YAML file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Start with defaults
	config := DefaultConfig()

	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Generate consumer name from hostname if not set
	if config.Redis.ConsumerName == "" {
		hostname, _ := os.Hostname()
		config.Redis.ConsumerName = fmt.Sprintf("miner-%s-%d", hostname, os.Getpid())
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return config, nil
}

// HasKeySource returns true if at least one key source is configured.
func (c *Config) HasKeySource() bool {
	return c.Keys.KeysFile != "" ||
		c.Keys.KeysDir != "" ||
		(c.Keys.Keyring != nil && c.Keys.Keyring.Backend != "")
}
