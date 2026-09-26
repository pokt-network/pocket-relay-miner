package miner

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/alitto/pond/v2"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// Config is the configuration for the HA Miner service.
type Config struct {
	// Redis configuration for consuming mined relays.
	Redis RedisConfig `yaml:"redis"`

	// PocketNode is the configuration for connecting to the Pocket blockchain.
	PocketNode config.PocketNodeConfig `yaml:"pocket_node"`

	// Keys configuration for loading supplier signing keys.
	Keys config.KeysConfig `yaml:"keys"`

	// unknownKeys are the keys the file carries that this struct does not
	// declare, found by the strict second pass in LoadConfig and surfaced by
	// Warnings().
	//
	// Unexported on purpose: it is a property of the FILE this config was loaded
	// from, not a setting, and nothing may set it from YAML. A Config built in
	// code rather than loaded from disk correctly reports none.
	unknownKeys []string

	// Transaction configuration for claim/proof submission.
	Transaction TransactionConfig `yaml:"transaction,omitempty"`

	// Metrics configuration.
	Metrics config.MetricsConfig `yaml:"metrics"`

	// PProf configuration.
	PProf config.PprofConfig `yaml:"pprof"`

	// Logging configuration.
	Logging logging.Config `yaml:"logging"`

	// BatchSize is the number of relays to process in a single batch.
	// Default: 100
	BatchSize int64 `yaml:"batch_size"`

	// SessionTTL is the TTL for session state data in Redis.
	// Default: CacheTTL (2h) - aligned with SMST tree TTL to prevent orphaned sessions.
	// Setting SessionTTL != CacheTTL can cause "SMST missing but relay count > 0" warnings.
	SessionTTL time.Duration `yaml:"session_ttl"`

	// CacheTTL is the TTL for Redis cached data (params, app stakes, service data, SMST trees).
	// This is a backup safety net - manual cleanup is primary, TTL prevents leaks if cleanup fails.
	// Default: 2h -- covers ~6 session lifecycles at a rough 60s/block mainnet estimate (20 blocks/session; real block time drifts with network conditions and differs per network -- this is illustrative margin, not a precise budget)
	CacheTTL time.Duration `yaml:"cache_ttl"`

	// SubmissionTrackingTTL is the TTL for claim/proof submission tracking records.
	// These records are used for debugging failed submissions and auditing.
	// Default: 24h (covers multiple session windows for debugging)
	SubmissionTrackingTTL time.Duration `yaml:"submission_tracking_ttl"`

	// SupplierReconcileIntervalSeconds is how often (in seconds) the miner
	// re-checks every keyring supplier's on-chain staking status and
	// service list. Closes the gap between "operator stakes/unstakes/
	// changes services" and "miner notices" without requiring a restart
	// or a keyring file edit.
	// Default: 60. Set to a negative value to disable (tests only).
	SupplierReconcileIntervalSeconds int64 `yaml:"supplier_reconcile_interval_seconds,omitempty"`

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

	// ServiceFactorRepublishInterval is how often the leader rewrites the
	// service factor manifest.
	//
	// The manifest carries no TTL, so a miner that wrote it while it still
	// believed itself leader would leave its own config standing forever. The
	// current leader rewriting on a period bounds that to one interval without
	// giving the key an expiry -- an expiry is what made the relayer unable to
	// tell "no factor configured" from "the key aged out".
	// Default: 5m
	ServiceFactorRepublishInterval time.Duration `yaml:"service_factor_republish_interval,omitempty"`

	// WorkerPools configures worker pool sizing for parallel processing.
	// Auto-sizing formula: max(cpu × cpu_multiplier, suppliers × workers_per_supplier) + overhead
	WorkerPools WorkerPoolConfigYAML `yaml:"worker_pools,omitempty"`

	// SupplierClaiming configures distributed supplier claiming for HA multi-miner setups.
	SupplierClaiming SupplierClaimingConfigYAML `yaml:"supplier_claiming,omitempty"`
}

// SessionLifecycleConfigYAML contains configuration for session lifecycle management.
type SessionLifecycleConfigYAML struct {
	// MaxConcurrentTransitions is the max number of sessions transitioning at once.
	// Default: 10
	MaxConcurrentTransitions int `yaml:"max_concurrent_transitions,omitempty"`
}

// SupplierClaimingConfigYAML contains configuration for distributed supplier claiming.
// In HA setups, miners distribute suppliers among themselves using Redis-based leases.
// These values control the lease timing and must be tuned for high supplier counts.
type SupplierClaimingConfigYAML struct {
	// ClaimTTLSeconds is how long a supplier claim lease is valid before expiring (in seconds).
	// If a miner crashes, other miners can reclaim its suppliers after this duration.
	// Higher values give more headroom for the renewal loop under load but increase
	// failover time when a miner dies.
	//
	// IMPORTANT: With 500+ suppliers, the sequential renewal loop can take several
	// seconds per cycle. If the renewal can't complete before TTL expires, claims
	// get orphaned and cause duplicate lifecycle issues. For high supplier counts,
	// increase this value.
	//
	// Guidelines:
	//   - <100 suppliers: 90s (default) is fine
	//   - 100-500 suppliers: 90s is fine
	//   - 500-1000 suppliers: 120s recommended
	//   - 1000+ suppliers: 180s recommended
	//
	// Default: 90s
	ClaimTTLSeconds int `yaml:"claim_ttl_seconds,omitempty"`

	// RenewRateSeconds is how often to renew all supplier claim leases (in seconds).
	// Must be significantly less than ClaimTTLSeconds to allow multiple renewal
	// attempts before expiry.
	// Default: 10s
	RenewRateSeconds int `yaml:"renew_rate_seconds,omitempty"`

	// RebalanceIntervalSeconds is how often to check for fair supplier distribution
	// across miner instances and scan for orphaned suppliers (in seconds).
	// Default: 30s
	RebalanceIntervalSeconds int `yaml:"rebalance_interval_seconds,omitempty"`
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
	// Enabled runs the balance and stake monitor; false turns it off entirely,
	// both the balance warnings and the stake alerts.
	// Default: true
	Enabled bool `yaml:"enabled,omitempty"`

	// CheckIntervalSeconds is how frequent to check balances and stakes (in seconds).
	// Default: 300 (5 minutes)
	CheckIntervalSeconds int64 `yaml:"check_interval_seconds,omitempty"`

	// BalanceThresholdUpokt is the minimum balance in uPOKT before triggering warnings.
	// Operators should set this based on their operational needs.
	// Default: 1000000 (1 POKT)
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
}

// RedisConfig embeds shared RedisConfig and adds miner-specific fields.
type RedisConfig struct {
	config.RedisConfig `yaml:",inline"`

	// ConsumerName is a readable PREFIX for this instance's Redis
	// stream-consumer name, not the name itself: UniqueConsumerName always
	// appends the host and pid, because Redis identifies a consumer by name
	// alone and two replicas sharing one would share a pending-entries list.
	// Defaults to "miner".
	ConsumerName string `yaml:"consumer_name,omitempty"`

	// Note: stream consumption blocks for one block interval per XREADGROUP,
	// not BLOCK 0 -- a bounded block is what lets a shutdown interrupt the read.
	// This is not configurable - messages are delivered instantly when available.

	// ClaimIdleTimeoutMs is how long a message can be pending before being claimed.
	// Default: 60000 (1 minute)
	ClaimIdleTimeoutMs int64 `yaml:"claim_idle_timeout_ms,omitempty"`

	// RelayBatchFlushIntervalMs is how often each supplier marks, counts and
	// acknowledges, in one script per session, the relays it already put in
	// the SMST. Until then those stream entries stay pending, so it must be at
	// most a quarter of claim_idle_timeout_ms: another miner's reclaim takes an
	// entry idle past that timeout without asking whether its owner is alive.
	// Default: 15000 (15 seconds), exactly the ceiling for the default 60 s.
	RelayBatchFlushIntervalMs int64 `yaml:"relay_batch_flush_interval_ms,omitempty"`
}

// DefaultRelayBatchFlushInterval is the relay batch flush interval when
// redis.relay_batch_flush_interval_ms is unset.
const DefaultRelayBatchFlushInterval = 15 * time.Second

// validateRelayBatchFlushInterval refuses a flush interval longer than a
// quarter of the reclaim's idle timeout. A batched entry stays pending until
// its flush, and its idle time also includes the wait in the delivery channel;
// the quarter keeps the whole of it under the timeout.
func validateRelayBatchFlushInterval(interval, claimIdleTimeout time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("redis.relay_batch_flush_interval_ms must be positive (got %s)", interval)
	}
	if interval > claimIdleTimeout/4 {
		return fmt.Errorf(
			"redis.relay_batch_flush_interval_ms (%s) must be at most a quarter of redis.claim_idle_timeout_ms (%s): "+
				"relays wait unacknowledged until the flush, and an entry idle past the timeout is "+
				"reclaimed by another miner while this one is still processing it",
			interval, claimIdleTimeout)
	}
	return nil
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

	// TxMaxConcurrent caps how many claim/proof broadcasts may be in flight on
	// the transaction connection at once. Default 32.
	//
	// What it bounds is the FULL NODE, not a stream ceiling: with gas
	// estimation on, each transaction costs a Simulate — which executes the
	// messages — plus a broadcast. Nobody has measured how much concurrent
	// simulation a node absorbs, so the default is conservative on purpose.
	//
	// Raise it only with evidence, and the evidence is ha_tx_permit_wait_seconds:
	// if nothing ever waits, the cap costs nothing and raising it buys nothing.
	TxMaxConcurrent int `yaml:"tx_max_concurrent,omitempty"`

	// TxRPCTimeoutSeconds bounds ONE broadcast attempt's network work.
	// Default 30.
	//
	// It is deliberately NOT the window timeout: that one says how long the
	// transaction is worth something on-chain (at least two minutes), while a
	// healthy node answers a broadcast in milliseconds. A permit held for two
	// minutes by a transaction that is already dead is a permit the inclusion
	// reconciler's resend cannot get.
	//
	// Tune it from the p99 of ha_tx_broadcast_latency_seconds on your own node.
	TxRPCTimeoutSeconds int64 `yaml:"tx_rpc_timeout_seconds,omitempty"`

	// TxConnProbeIntervalSeconds is how often the miner probes its dedicated
	// transaction connection while it is idle. Default: 60.
	//
	// The connection only carries traffic inside claim and proof windows, and
	// a flow dropped by a middlebox in between leaves both ends believing it
	// is healthy -- there are no keepalive pings without streams. The probe is
	// what turns that into a log line before the window instead of a lost
	// claim inside it.
	//
	// Lower it if probe failures show a small idle_seconds on your network:
	// that value is how long the connection had been silent when it broke.
	TxConnProbeIntervalSeconds int64 `yaml:"tx_conn_probe_interval_seconds,omitempty"`

	// DisablePreProofClaimVerification disables the pre-proof GetClaim guard.
	// The guard queries the chain for each session's claim before proof
	// submission; sessions whose claim is not on-chain are dropped from the
	// proof batch and marked claim_missing. This prevents the
	// "no claim found for session ID" FailedPrecondition retry storm when a
	// claim tx was accepted into the mempool but never included in a block.
	// Leave disabled only to reproduce pre-WS-A behavior. Default: false
	// (guard enabled — recommended for production).
	DisablePreProofClaimVerification bool `yaml:"disable_pre_proof_claim_verification,omitempty"`

	// InclusionReconcilerMaxConcurrent bounds the per-block group-reconcile
	// worker pool (one task per owned supplier per block). Default: 64.
	InclusionReconcilerMaxConcurrent int `yaml:"inclusion_reconciler_max_concurrent,omitempty"`

	// MaxRebroadcasts caps how many times a still-missing claim/proof is
	// re-submitted within its window. Pointer so an explicit 0 (observe-only:
	// verify + record outcomes but never resend) is distinguishable from unset.
	//
	// UNSET NOW MEANS NO CAP, and that is a change for every deployment that
	// never configured this: it used to inherit a cap of 2 with the resends
	// spaced two blocks apart, and now a still-missing claim is re-sent on every
	// block its window allows. Setting an explicit number restores a cap; 0
	// still means observe-only and is unaffected.
	//
	// The cost of the new default is one transaction fee per resend (1 upokt),
	// against a claim that pays nothing at all if it never lands.
	MaxRebroadcasts *int `yaml:"max_rebroadcasts,omitempty"`

	// RebroadcastSafetyBlocks stops rebroadcasting once the chain is within this
	// many blocks of window-close (claim or proof), so a re-submit cannot land
	// after the window. Pointer so an explicit 0 is honored. Default: 1.
	RebroadcastSafetyBlocks *int64 `yaml:"rebroadcast_safety_blocks,omitempty"`

	// InclusionReconcilerPerGroupTimeoutMs bounds one supplier-group reconcile
	// per block (the AllProofs/AllClaims query plus any rebroadcasts). Default:
	// 10000 (10s).
	InclusionReconcilerPerGroupTimeoutMs int64 `yaml:"inclusion_reconciler_per_group_timeout_ms,omitempty"`
}

// InclusionReconcilerConfig translates the YAML-facing fields on
// TransactionConfig into the miner-layer InclusionReconcilerConfig, starting
// from DefaultInclusionReconcilerConfig and overriding only the fields the
// operator set. Callers pass the result to NewInclusionReconciler.
func (c TransactionConfig) InclusionReconcilerConfig() InclusionReconcilerConfig {
	cfg := DefaultInclusionReconcilerConfig()
	if c.InclusionReconcilerMaxConcurrent > 0 {
		cfg.MaxConcurrent = c.InclusionReconcilerMaxConcurrent
	}
	// Pointers: nil keeps the default; an explicit value (including 0) is honored.
	if c.MaxRebroadcasts != nil {
		cfg.MaxRebroadcasts = c.MaxRebroadcasts
	}
	if c.RebroadcastSafetyBlocks != nil {
		cfg.RebroadcastSafetyBlocks = *c.RebroadcastSafetyBlocks
	}
	if c.InclusionReconcilerPerGroupTimeoutMs > 0 {
		cfg.PerGroupTimeout = time.Duration(c.InclusionReconcilerPerGroupTimeoutMs) * time.Millisecond
	}
	// The pool is capped by the transaction client's permit count, from the
	// same config field rather than a second constant: a worker above that
	// number can only start in order to park.
	cfg.TxMaxConcurrent = c.txMaxConcurrent()
	return cfg
}

// txMaxConcurrent is the configured broadcast concurrency, or the tx package's
// default when unset.
func (c TransactionConfig) txMaxConcurrent() int {
	if c.TxMaxConcurrent > 0 {
		return c.TxMaxConcurrent
	}
	return int(tx.DefaultTxMaxConcurrent)
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if c.Redis.URL == "" {
		return fmt.Errorf("redis.url is required")
	}

	if err := c.Logging.Validate(); err != nil {
		return err
	}

	// The namespace is validated here rather than where keys are built, because
	// the failure it catches is a config that would relocate the whole keyspace:
	// it has to stop startup, not surface as a cache miss.
	if err := c.Redis.Namespace.Validate(); err != nil {
		return err
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
	if c.Redis.RelayBatchFlushIntervalMs < 0 {
		return fmt.Errorf("redis.relay_batch_flush_interval_ms must be >= 0 (0 = use default)")
	}
	if err := validateRelayBatchFlushInterval(c.GetRelayBatchFlushInterval(), c.GetClaimIdleTimeout()); err != nil {
		return err
	}

	if c.PocketNode.QueryNodeRPCUrl == "" {
		return fmt.Errorf("pocket_node.query_node_rpc_url is required")
	}

	if c.PocketNode.QueryNodeGRPCUrl == "" {
		return fmt.Errorf("pocket_node.query_node_grpc_url is required")
	}

	// Exactly one key source (suppliers are auto-discovered from the keys).
	keyringBackend := ""
	if c.Keys.Keyring != nil {
		keyringBackend = c.Keys.Keyring.Backend
	}
	if err := keys.ValidateKeySources(c.Keys.KeysFile, keyringBackend); err != nil {
		return err
	}

	// Validate keyring config if provided
	if c.Keys.Keyring != nil && c.Keys.Keyring.Backend != "" {
		if err := keys.ValidateKeyringBackend(c.Keys.Keyring.Backend); err != nil {
			return err
		}
		if err := keys.ValidatePassphraseSource(c.Keys.Keyring.Backend, keys.PassphraseSource{
			File: c.Keys.Keyring.PassphraseFile,
			Env:  c.Keys.Keyring.PassphraseEnv,
		}); err != nil {
			return err
		}
	}

	// Validate leader election: heartbeat must be less than TTL
	if c.LeaderElection.HeartbeatRateSeconds > 0 && c.LeaderElection.LeaderTTLSeconds > 0 {
		if c.LeaderElection.HeartbeatRateSeconds >= c.LeaderElection.LeaderTTLSeconds {
			return fmt.Errorf("leader_election.heartbeat_rate_seconds (%d) must be less than leader_ttl_seconds (%d) to prevent lock expiration before renewal",
				c.LeaderElection.HeartbeatRateSeconds, c.LeaderElection.LeaderTTLSeconds)
		}
	}

	// Validate supplier claiming: renew rate must be less than TTL
	if c.SupplierClaiming.RenewRateSeconds > 0 && c.SupplierClaiming.ClaimTTLSeconds > 0 {
		if c.SupplierClaiming.RenewRateSeconds >= c.SupplierClaiming.ClaimTTLSeconds {
			return fmt.Errorf("supplier_claiming.renew_rate_seconds (%d) must be less than claim_ttl_seconds (%d)",
				c.SupplierClaiming.RenewRateSeconds, c.SupplierClaiming.ClaimTTLSeconds)
		}
	}

	// Note: Storage validation removed - all session trees now use Redis

	// block_time_seconds is REQUIRED, and refusing to start is the point rather
	// than an inconvenience.
	//
	// It became load-bearing when the transaction deadline stopped being
	// configurable: the deadline is now the window in blocks times this number,
	// so a wrong value is a wrong deadline on every claim and every proof. There
	// used to be a default of 30 to fall back on, and falling back is exactly
	// what must not happen here -- 30 is right for no network we run on. An
	// operator on mainnet who set nothing would have had every deadline computed
	// at half the real block time, and nothing would have looked wrong: the
	// number is plausible, the transactions still broadcast, and the loss only
	// shows up as claims that stopped landing late in the window.
	//
	// A config that cannot say how fast its chain produces blocks is a config
	// that cannot be reasoned about, so it stops the process while somebody is
	// watching, instead of quietly picking a number.
	if c.BlockTimeSeconds <= 0 {
		return fmt.Errorf(
			"block_time_seconds is required and must be positive (got %d): it is the "+
				"basis of every claim and proof transaction deadline, and there is no "+
				"safe default -- set it to the measured block time of the network this "+
				"miner runs against",
			c.BlockTimeSeconds,
		)
	}

	return nil
}

// GetSupplierReconcileInterval returns the configured interval, falling back
// to DefaultSupplierReconcileInterval when unset (zero). Negative values are
// returned as-is so tests can disable the loop via
// SupplierReconcileIntervalSeconds: -1.
func (c *Config) GetSupplierReconcileInterval() time.Duration {
	if c.SupplierReconcileIntervalSeconds == 0 {
		return DefaultSupplierReconcileInterval
	}
	return time.Duration(c.SupplierReconcileIntervalSeconds) * time.Second
}

// GetClaimIdleTimeout returns the claim idle timeout as a duration.
func (c *Config) GetClaimIdleTimeout() time.Duration {
	if c.Redis.ClaimIdleTimeoutMs > 0 {
		return time.Duration(c.Redis.ClaimIdleTimeoutMs) * time.Millisecond
	}
	return time.Minute // Default
}

// GetRelayBatchFlushInterval returns the relay batch flush interval as a duration.
func (c *Config) GetRelayBatchFlushInterval() time.Duration {
	if c.Redis.RelayBatchFlushIntervalMs > 0 {
		return time.Duration(c.Redis.RelayBatchFlushIntervalMs) * time.Millisecond
	}
	return DefaultRelayBatchFlushInterval
}

// GetBatchSize returns the batch size with defaults.
func (c *Config) GetBatchSize() int64 {
	if c.BatchSize > 0 {
		return c.BatchSize
	}
	return 1000 // Default (increased from 100 for better throughput)
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

// GetTxMaxConcurrent returns the broadcast concurrency cap.
func (c *Config) GetTxMaxConcurrent() int {
	return c.Transaction.txMaxConcurrent()
}

// GetTxRPCTimeout returns the per-attempt broadcast timeout, or zero to let the
// tx client apply its default.
func (c *Config) GetTxRPCTimeout() time.Duration {
	if c.Transaction.TxRPCTimeoutSeconds > 0 {
		return time.Duration(c.Transaction.TxRPCTimeoutSeconds) * time.Second
	}
	return 0
}

// GetTxConnProbeInterval returns the idle-probe interval for the dedicated
// transaction connection, or zero to let the tx client apply its default.
func (c *Config) GetTxConnProbeInterval() time.Duration {
	if c.Transaction.TxConnProbeIntervalSeconds > 0 {
		return time.Duration(c.Transaction.TxConnProbeIntervalSeconds) * time.Second
	}
	return 0
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
	// DefaultConfig sets it true; a config loaded without the key keeps that.
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
	return cache.DefaultBlockTimeSeconds
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
	return 2 * time.Hour // Default: 2h -- covers ~6 session lifecycles at a rough 60s/block mainnet estimate (20 blocks/session; real block time drifts with network conditions and differs per network -- this is illustrative margin, not a precise budget)
}

// GetServiceFactorRepublishInterval returns how often the leader rewrites the
// service factor manifest.
func (c *Config) GetServiceFactorRepublishInterval() time.Duration {
	if c.ServiceFactorRepublishInterval > 0 {
		return c.ServiceFactorRepublishInterval
	}
	return 5 * time.Minute
}

// GetSubmissionTrackingTTL returns the TTL for submission tracking records.
func (c *Config) GetSubmissionTrackingTTL() time.Duration {
	if c.SubmissionTrackingTTL > 0 {
		return c.SubmissionTrackingTTL
	}
	return 24 * time.Hour // Default: 24h for debugging
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

// GetSupplierClaimingConfig returns the SupplierClaimerConfig for supplier claiming.
// Uses YAML config values if set, otherwise falls back to constants defined in supplier_claimer.go.
func (c *Config) GetSupplierClaimingConfig() SupplierClaimerConfig {
	claimTTL := ClaimTTL
	if c.SupplierClaiming.ClaimTTLSeconds > 0 {
		claimTTL = time.Duration(c.SupplierClaiming.ClaimTTLSeconds) * time.Second
	}

	renewRate := RenewRate
	if c.SupplierClaiming.RenewRateSeconds > 0 {
		renewRate = time.Duration(c.SupplierClaiming.RenewRateSeconds) * time.Second
	}

	rebalanceInterval := RebalanceInterval
	if c.SupplierClaiming.RebalanceIntervalSeconds > 0 {
		rebalanceInterval = time.Duration(c.SupplierClaiming.RebalanceIntervalSeconds) * time.Second
	}

	// Instance TTL and heartbeat rate always match claim TTL and renew rate
	// to keep the timing relationships consistent.
	return SupplierClaimerConfig{
		ClaimTTL:              claimTTL,
		RenewRate:             renewRate,
		InstanceTTL:           claimTTL,
		InstanceHeartbeatRate: renewRate,
		RebalanceInterval:     rebalanceInterval,
	}
}

// suppliersPerCPUWarnThreshold is a ROUGH advisory floor, not an SLA. At
// window-open every owned supplier builds its claim/proof concurrently and SMST
// proving (ProveClosest) is CPU-bound; well above this ratio an under-provisioned
// instance can submit too late and forfeit. The real fix is horizontal scaling
// (the SupplierClaimer distributes suppliers across replicas automatically).
const suppliersPerCPUWarnThreshold = 50

// LogStartupCapacityAdvisory emits operator-facing warnings when this instance
// looks under-provisioned for the number of supplier keys it drives, or is
// configured in a way known to cause CLAIM_MISSING/PROOF_MISSING at scale. It is
// advisory only (never fatal) and meant to surface in the logs of operators who
// deploy fast without reading the docs.
func (c *Config) LogStartupCapacityAdvisory(logger logging.Logger, numSuppliers int) {
	cpu := getEffectiveCPUCount()
	if cpu > 0 && numSuppliers > cpu*suppliersPerCPUWarnThreshold {
		logger.Warn().
			Int("num_suppliers", numSuppliers).
			Int("effective_cpu", cpu).
			Int("suppliers_per_cpu", numSuppliers/cpu).
			Int("rough_recommended_min_cpu", (numSuppliers+suppliersPerCPUWarnThreshold-1)/suppliersPerCPUWarnThreshold).
			Msg("LIKELY UNDER-PROVISIONED: many supplier keys per CPU on this instance. At proof-window-open all suppliers " +
				"build+submit proofs concurrently (CPU-bound SMST proving); too little CPU can submit proofs too late and " +
				"forfeit (PROOF_MISSING). Recommended: scale horizontally (run more miner replicas — suppliers are " +
				"distributed automatically), and/or give the instance more CPU, and/or run fewer keys per instance.")
	}

	// Operator explicitly capped the master pool below what the auto formula
	// would pick for this supplier count.
	if c.WorkerPools.MasterPoolSize > 0 {
		autoCalc := maxInt(getEffectiveCPUCount()*c.GetCPUMultiplier(), numSuppliers*c.GetWorkersPerSupplier()) + c.GetQueryWorkers()
		if c.WorkerPools.MasterPoolSize < autoCalc {
			logger.Warn().
				Int("master_pool_size", c.WorkerPools.MasterPoolSize).
				Int("auto_recommended", autoCalc).
				Int("num_suppliers", numSuppliers).
				Msg("DISCOURAGED CONFIG: master_pool_size is set BELOW the auto-sized recommendation for this supplier " +
					"count — claim/proof building/submission may serialize and miss windows. Remove the override to auto-size, " +
					"or raise it to at least the recommended value.")
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// GetMasterPoolSize returns the master pool size, auto-calculating if not explicitly set.
// Formula: max(cpu × cpu_multiplier, suppliers × workers_per_supplier) + overhead
// Overhead = query_workers
// Example (4 CPU, 78 suppliers): max(4×4, 78×6) + 20 = max(16, 468) + 20 = 488
func (c *Config) GetMasterPoolSize(numSuppliers int) int {
	if c.WorkerPools.MasterPoolSize > 0 {
		return c.WorkerPools.MasterPoolSize
	}
	// Auto-calculate based on CPU and supplier count
	// Use getEffectiveCPUCount() which respects GOMAXPROCS for container environments
	cpuBased := getEffectiveCPUCount() * c.GetCPUMultiplier()
	supplierBased := numSuppliers * c.GetWorkersPerSupplier()
	overhead := c.GetQueryWorkers()

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
			// Note: BlockTimeout removed - the consumer blocks for one block
			// interval per read, so a shutdown is never more than that away.
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
			GasLimit:      0,               // 0 = automatic gas estimation via simulation
			GasPrice:      "0.000001upokt", // Default gas price
			GasAdjustment: 1.7,             // Default 70% safety margin
		},
		BatchSize: 1000, // Increased from 100 for better throughput (10x more efficient)
		// Hot reload on by default, in BOTH binaries: an operator who never
		// thinks about it gets a fleet that picks up a key change on its own,
		// and one who turns it off is told so at startup by the key manager's
		// own warning.
		Keys: config.KeysConfig{
			HotReloadEnabled: true,
		},
		// SessionTTL: 0 means use CacheTTL (default 2h) - ensures SMST trees and sessions expire together
		// This prevents orphaned sessions causing "SMST missing but relay count > 0" warnings
		CacheTTL:              2 * time.Hour,  // Covers ~6 session lifecycles at a rough 60s/block mainnet estimate (20 blocks/session; real block time drifts with network conditions and differs per network -- this is illustrative margin, not a precise budget)
		SubmissionTrackingTTL: 24 * time.Hour, // 24h for debugging (was 7 days)
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
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Second pass over the same bytes, diagnostic only: the decode above is
	// lenient and drops every key this struct does not declare, so the file and
	// the process can disagree with no signal at all. What to DO with the finding
	// belongs to the caller -- `validate` fails on it because validating is its
	// whole job, and the serving binary warns and starts unless --strict-config
	// was passed, because refusing to boot over a stale key turns a rolling
	// deploy into an outage. See config.UnknownKeys.
	cf.unknownKeys = config.UnknownKeys(data, &Config{})

	cf.Redis.ConsumerName = UniqueConsumerName(cf.Redis.ConsumerName)

	if err = cf.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cf, nil
}

// Warnings returns one line per key the file carries that this struct does not
// declare -- typos, settings this project retired, and keys that were never
// fields at all.
//
// The miner had no channel for this at all, which is why the retired top-level
// hot_reload_enabled had to be a HARD boot failure: with only "fail" and "say
// nothing" available, failing was the right call. With a channel, a stale key is
// a warning at startup and a hard failure under `miner validate` or
// --strict-config, which is the same rule the relayer follows.
//
// The sentence that says what each removal CHANGED for the operator lives in
// config.retiredKeys and is attached to the generic finding, so deleting the
// tombstone struct fields lost the fields and not the knowledge.
func (c *Config) Warnings() []string {
	return c.unknownKeys
}

// UniqueConsumerName returns the name this process registers with the Redis
// stream group, given whatever the operator configured (possibly nothing).
//
// The process discriminator is appended ALWAYS, not only when the field is
// empty. Redis identifies a consumer by name and by nothing else, so two
// processes sharing one name share a pending-entries list: a crashed replica's
// stranded deliveries then read as the survivor's own in-flight work and the
// reclaim path passes over them forever. A fixed name in a shared ConfigMap is
// an ordinary thing to write — the schema even calls the field "unique" —, so
// uniqueness cannot be left to the operator to remember.
//
// What is configured survives as a readable prefix, which is what an operator
// setting it actually wants.
func UniqueConsumerName(configured string) string {
	prefix := configured
	if prefix == "" {
		prefix = "miner"
	}
	// The discriminator is ProcessIdentity(), shared with the leader-lock value:
	// one identity with two uses, so they cannot drift apart. It replaced a
	// local helper whose comment claimed "the pid still discriminates within a
	// host" -- true as written and false in the deployment this repo has, where
	// every replica is PID 1 in its own namespace.
	return fmt.Sprintf("%s-%s", prefix, ProcessIdentity())
}
