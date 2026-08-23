package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/observability"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const (
	flagMinerConfig  = "config"
	flagConsumerName = "consumer-name"
	flagHotReload    = "hot-reload"
	flagSessionTTL   = "session-ttl"
)

// MinerCmd returns the command for starting the HA Miner component.
func MinerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "miner",
		Short: "Start the HA Miner (SMST builder and claim/proof submitter)",
		Long: `Start the High-Availability Miner component.

The HA Miner consumes mined relays from Redis Streams and builds SMST trees.
It supports multiple suppliers and dynamically adds/removes them based on key changes.

Configuration:
  --config: Path to miner config YAML file (required)


Features:
- Multi-supplier support (one consumer per supplier)
- Consumes mined relays from Redis Streams
- Builds SMST (Sparse Merkle Sum Tree) for each session
- WAL-based crash recovery
- Hot-reload of keys (add/remove suppliers without restart)
- Publishes supplier registry for relayer discovery
- Prometheus metrics at /metrics

Example:
  pocketd relayminer ha miner --config /path/to/miner-config.yaml

`,
		RunE: runHAMiner,
	}

	cmd.Flags().String(flagMinerConfig, "", "Path to miner config YAML file (required)")

	// Redis flags (can override config)
	cmd.Flags().String(flagRedisURL, "", "Redis connection URL (overrides config)")
	cmd.Flags().String(flagConsumerName, "", "Consumer name (defaults to hostname)")

	// Configuration flags (can override config)
	cmd.Flags().Bool(flagHotReload, true, "Enable hot-reload of keys")
	cmd.Flags().Duration(flagSessionTTL, 0, "Session data TTL (default: same as cache_ttl to prevent orphaned sessions)")

	cmd.AddCommand(minerValidateCmd())

	return cmd
}

// minerValidateCmd runs the exact config checks the miner runs at startup —
// load + validateMinerConfig — without starting anything, and exits non-zero on
// the first error. Run it before deploying a config change: a config the miner
// rejects will not boot.
func minerValidateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "validate",
		Short: "Validate a miner config without starting the miner",
		Long: `Validate a miner config against the same checks the miner runs at startup.

Exits 0 if the config would boot, non-zero with the first error otherwise.
Run this before rolling out a config change.

Example:
  pocket-relay-miner miner validate --config /path/to/miner.yaml`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := loadMinerConfig(cmd)
			if err != nil {
				return fmt.Errorf("config is INVALID: %w", err)
			}
			if err := validateMinerConfig(config); err != nil {
				return fmt.Errorf("config is INVALID: %w", err)
			}
			configPath, _ := cmd.Flags().GetString(flagMinerConfig)
			fmt.Printf("config OK: %s would start\n", configPath)
			return nil
		},
	}
	c.Flags().String(flagMinerConfig, "", "Path to miner config YAML file (required)")
	_ = c.MarkFlagRequired(flagMinerConfig)
	return c
}

func runHAMiner(cmd *cobra.Command, _ []string) (err error) {
	// Panic recovery for production resilience
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("miner panic: %v", r)
		}
	}()

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// Load config first (needed for logger configuration)
	config, err := loadMinerConfig(cmd)
	if err != nil {
		return err
	}

	// Set up logger from config
	logger := logging.NewLoggerFromConfig(config.Logging)

	// Validate configuration before starting components
	if err := validateMinerConfig(config); err != nil {
		logger.Error().Err(err).Msg("configuration validation failed")
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Start an observability server (metrics and/or pprof)
	var obsServer *observability.Server
	if config.Metrics.Enabled || config.PProf.Enabled {
		// Combine MinerRegistry and SharedRegistry so cache metrics are exposed
		combinedRegistry := prometheus.Gatherers{
			observability.MinerRegistry,
			observability.SharedRegistry,
		}

		obsServer = observability.NewServer(logger, observability.ServerConfig{
			MetricsEnabled: config.Metrics.Enabled,
			MetricsAddr:    config.Metrics.Addr,
			PprofEnabled:   config.PProf.Enabled,
			PprofAddr:      config.PProf.Addr,
			Registry:       combinedRegistry,
		})
		if err := obsServer.Start(ctx); err != nil {
			return fmt.Errorf("failed to start observability server: %w", err)
		}
		defer func() { _ = obsServer.Stop() }()
		logger.Info().Str("addr", config.Metrics.Addr).Msg("observability server started")

		// Start runtime metrics collector (not started automatically when using custom registry)
		runtimeMetrics := observability.NewRuntimeMetricsCollector(
			logger,
			observability.DefaultRuntimeMetricsCollectorConfig(),
			observability.MinerFactory,
		)
		if err := runtimeMetrics.Start(ctx); err != nil {
			return fmt.Errorf("failed to start runtime metrics collector: %w", err)
		}
		defer runtimeMetrics.Stop()
		logger.Info().Msg("runtime metrics collector started")
	}

	// Create a wrapped Redis client with KeyBuilder for namespace-aware key construction
	redisClient, err := redistransport.NewClient(ctx, redistransport.ClientConfig{
		URL:                    config.Redis.URL,
		PoolSize:               config.Redis.PoolSize,
		MinIdleConns:           config.Redis.MinIdleConns,
		PoolTimeoutSeconds:     config.Redis.PoolTimeoutSeconds,
		ConnMaxIdleTimeSeconds: config.Redis.ConnMaxIdleTimeSeconds,
		Namespace:              config.Redis.Namespace,
	})
	if err != nil {
		return fmt.Errorf("failed to create Redis client: %w", err)
	}
	defer func() {
		// closeErr, NOT err: runHAMiner has a NAMED result, so assigning to
		// err here overwrites whatever the function returned — a nil Close
		// would mask the leader-controller failure below and exit 0.
		if closeErr := redisClient.Close(); closeErr != nil {
			logger.Error().Err(closeErr).Msg("failed to close Redis client")
		}
	}()
	logger.Info().
		Str("redis_url", config.Redis.URL).
		Str("consumer_name", config.Redis.ConsumerName).
		Msg("connected to Redis")

	// Set readiness check to verify Redis connectivity via PING
	if obsServer != nil {
		obsServer.SetReadinessCheck(func(ctx context.Context) error {
			return redisClient.Ping(ctx).Err()
		})
	}

	// Start Redis health monitor (runs on ALL replicas for OOM visibility)
	redisHealthMonitor := leader.NewRedisHealthMonitor(logger, redisClient)
	if err = redisHealthMonitor.Start(ctx); err != nil {
		return fmt.Errorf("failed to start Redis health monitor: %w", err)
	}
	defer func() { _ = redisHealthMonitor.Close() }()

	// One shared sequence for both binaries: build the providers the config
	// names, put a key manager over them, load once, arm the watch and the
	// reload timer, and refuse to continue with no keys. See keys.OpenManager
	// for why that lives there and not here.
	keyManager, err := keys.OpenManager(
		ctx, logger,
		config.Keys.KeysFile,
		keyringSettings(config.Keys.Keyring),
		config.Keys.HotReloadEnabled,
	)
	if err != nil {
		return err
	}
	defer func() { _ = keyManager.Close() }()

	logger.Info().
		Int("count", len(keyManager.ListSuppliers())).
		Msg("loaded supplier keys")

	// Generate unique instance ID for global leader election
	hostname, _ := os.Hostname()
	instanceID := fmt.Sprintf("%s-%d", hostname, os.Getpid())

	// Create global leader elector FIRST to determine replica status before other components start
	leaderConfig := leader.GlobalLeaderElectorConfig{
		LeaderTTL:     config.GetLeaderTTL(),
		HeartbeatRate: config.GetLeaderHeartbeatRate(),
	}

	// Warn if the heartbeat rate is too close to TTL (risk of lock expiration)
	if leaderConfig.HeartbeatRate > leaderConfig.LeaderTTL/2 {
		logger.Warn().
			Dur("heartbeat_rate", leaderConfig.HeartbeatRate).
			Dur("leader_ttl", leaderConfig.LeaderTTL).
			Msg("WARNING: heartbeat_rate is more than half of leader_ttl - risk of lock expiration before renewal! Recommended: heartbeat_rate <= leader_ttl/3")
	}

	globalLeader := leader.NewGlobalLeaderElectorWithConfig(
		logger,
		redisClient,
		instanceID,
		leaderConfig,
	)
	if err = globalLeader.Start(ctx); err != nil {
		return fmt.Errorf("failed to start global leader elector: %w", err)
	}
	defer func() { globalLeader.Close() }()

	// Use dynamic logger that evaluates replica status at log time
	// The replica field will automatically reflect leader election changes
	logger = logging.ForMinerDynamic(logger, instanceID, globalLeader)
	logger.Info().Msg("miner context initialized")

	// One-time migration of SMST keys written under the legacy shared-session
	// schema (pre-per-supplier fix). For any session whose shared root key
	// matches exactly one supplier's stored `claimed_root_hash`, rename the
	// three legacy keys under the new per-supplier schema so that supplier's
	// lazy-load can complete proof generation. Other suppliers in the same
	// session cannot be rescued — their trees were overwritten by the last
	// flusher — and their claims will expire once. The migration is a no-op
	// on clusters that have never run the legacy code, and idempotent on
	// re-runs (all keys already in the new schema are skipped).
	if _, migrateErr := miner.MigrateLegacySMSTKeys(ctx, logger, redisClient); migrateErr != nil {
		logger.Warn().Err(migrateErr).Msg("legacy SMST migration encountered errors (continuing startup)")
	}

	// Start SupplierWorker for ALL miners
	// This runs BEFORE leader callbacks - every miner claims and processes its share of suppliers.
	// If there's only 1 miner, it claims all suppliers.
	// If there are multiple miners, they automatically distribute suppliers via Redis leases.
	supplierWorker := miner.NewSupplierWorker(miner.SupplierWorkerConfig{
		Logger:           logger,
		RedisClient:      redisClient,
		KeyManager:       keyManager,
		Config:           config,
		QueryNodeRPCUrl:  config.PocketNode.QueryNodeRPCUrl,
		QueryNodeGRPCUrl: config.PocketNode.QueryNodeGRPCUrl,
		GRPCInsecure:     config.PocketNode.GRPCInsecure,
		ChainID:          config.GetChainID(), // Get from config (defaults to "pocket" if not set)
	})

	if err = supplierWorker.Start(ctx); err != nil {
		return fmt.Errorf("failed to start supplier worker: %w", err)
	}
	defer func() {
		if closeErr := supplierWorker.Close(); closeErr != nil {
			logger.Error().Err(closeErr).Msg("failed to close supplier worker")
		}
	}()

	logger.Info().
		Int("suppliers", len(keyManager.ListSuppliers())).
		Msg("SupplierWorker started - claiming suppliers")

	// Create leader controller for leader-only resources (cache refresh + block publishing)
	// SupplierWorker handles all supplier processing (distributed across all replicas).
	// LeaderController only manages:
	// - Shared cache refresh (params, applications, services)
	// - Block event publishing to Redis for distributed consumption
	// - Balance and health monitoring
	leaderController := miner.NewLeaderController(miner.LeaderControllerConfig{
		Logger:           logger,
		RedisClient:      redisClient,
		KeyManager:       keyManager,
		Config:           config,
		GlobalLeader:     globalLeader,
		QueryNodeRPCUrl:  config.PocketNode.QueryNodeRPCUrl,
		QueryNodeGRPCUrl: config.PocketNode.QueryNodeGRPCUrl,
		GRPCInsecure:     config.PocketNode.GRPCInsecure,
		ChainID:          config.GetChainID(), // Get from config (defaults to "pocket" if not set)

		// Share the worker's supplier cache instead of letting the leader build
		// a second one in this same process. Safe to read here: the worker's
		// Start() above already constructed it, and the worker outlives every
		// leadership change, so the controller borrows and never closes it.
		SharedSupplierCache: supplierWorker.GetSupplierCache(),
	})

	// Register leader election callbacks. They run in goroutines, so failures
	// are propagated to the main goroutine via this channel instead of
	// logger.Fatal (os.Exit would skip every deferred cleanup).
	leaderErrCh := make(chan error, 1)
	registerLeaderCallbacks(logger, globalLeader, leaderController, leaderErrCh)

	// If already a leader at startup, start immediately
	if globalLeader.IsLeader() {
		logger.Info().Msg("starting leader controller (already leader at startup)")
		if err = leaderController.Start(ctx); err != nil {
			return fmt.Errorf("failed to start leader controller: %w", err)
		}
	} else {
		logger.Info().Msg("leader controller in standby mode (not leader)")
	}
	defer func() {
		// closeErr, NOT err: see the Redis defer above — assigning to the
		// named result here discarded the error this function returns.
		if closeErr := leaderController.Close(); closeErr != nil {
			logger.Error().Err(closeErr).Msg("failed to close leader controller")
		}
	}()

	logger.Info().
		Str("consumer_name", config.Redis.ConsumerName).
		Bool("hot_reload", config.Keys.HotReloadEnabled).
		Msg("HA Miner started")

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Wait for a shutdown signal or a leader-controller failure
	var runErr error
	select {
	case <-sigCh:
		logger.Info().Msg("shutdown signal received, stopping HA Miner...")
	case runErr = <-leaderErrCh:
		logger.Error().Err(runErr).Msg("leader controller failed, stopping HA Miner so a standby can take over...")
	}

	// Deferring handles graceful shutdown
	logger.Info().Msg("HA Miner stopped")
	return runErr
}

// loadMinerConfig loads the miner configuration from a file or flags.
func loadMinerConfig(cmd *cobra.Command) (*miner.Config, error) {
	configPath, _ := cmd.Flags().GetString(flagMinerConfig)

	var config *miner.Config
	var err error

	if configPath == "" {
		// The flags-only legacy mode duplicated the whole config assembly and
		// had zero tracked invocations; a config file is the one way to start.
		return nil, fmt.Errorf("--config is required")
	}
	config, err = miner.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Apply flag overrides (flags take precedence over config file)
	applyFlagOverrides(cmd, config)

	// LoadConfig already ran the name through miner.UniqueConsumerName, so it
	// is re-derived ONLY when the flag overrode it. Doing it unconditionally
	// appends the host and pid twice ("relay-a-host-1234-host-1234"), which is
	// still unique but makes the schema's "-<hostname>-<pid> is appended" a
	// lie and the name in XINFO CONSUMERS unreadable.
	if cmd.Flags().Changed(flagConsumerName) {
		config.Redis.ConsumerName = miner.UniqueConsumerName(config.Redis.ConsumerName)
	}

	return config, nil
}

// applyFlagOverrides applies command-line flag overrides to the config.
func applyFlagOverrides(cmd *cobra.Command, config *miner.Config) {
	if cmd.Flags().Changed(flagRedisURL) {
		config.Redis.URL, _ = cmd.Flags().GetString(flagRedisURL)
	}
	if cmd.Flags().Changed(flagConsumerName) {
		config.Redis.ConsumerName, _ = cmd.Flags().GetString(flagConsumerName)
	}
	if cmd.Flags().Changed(flagHotReload) {
		// The flag drives the same field the config file does; there is one
		// hot-reload switch per process, not one per surface.
		config.Keys.HotReloadEnabled, _ = cmd.Flags().GetBool(flagHotReload)
	}
	if cmd.Flags().Changed(flagSessionTTL) {
		config.SessionTTL, _ = cmd.Flags().GetDuration(flagSessionTTL)
	}
}

// validateMinerConfig performs upfront validation of configuration
// to fail fast before starting components.
func validateMinerConfig(config *miner.Config) error {
	// Validate Redis configuration
	if config.Redis.URL == "" {
		return fmt.Errorf("redis.url is required")
	}
	if config.Redis.ConsumerName == "" {
		return fmt.Errorf("redis.consumer_name is required")
	}

	// Validate PocketNode configuration
	if config.PocketNode.QueryNodeRPCUrl == "" {
		return fmt.Errorf("pocket_node.query_node_rpc_url is required")
	}
	if config.PocketNode.QueryNodeGRPCUrl == "" {
		return fmt.Errorf("pocket_node.query_node_grpc_url is required")
	}

	// Key sources are NOT validated here. miner.LoadConfig calls
	// Config.Validate, which enforces exactly one source -- this function does
	// not, so a caller that builds a miner.Config in memory and calls only this
	// gets no key-source check at all. Every caller goes through LoadConfig
	// today; anyone adding one that does not must call Config.Validate itself.

	// Note: SessionTTL = 0 means use CacheTTL (default 2h), so it doesn't need validation

	return nil
}
