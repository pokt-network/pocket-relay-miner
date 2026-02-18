package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	configpkg "github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/observability"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const (
	flagMinerConfig    = "config"
	flagKeysFile       = "keys-file"
	flagKeysDir        = "keys-dir"
	flagKeyringBackend = "keyring-backend"
	flagKeyringDir     = "keyring-dir"
	flagConsumerName   = "consumer-name"
	flagHotReload      = "hot-reload"
	flagSessionTTL     = "session-ttl"
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
  --config: Path to miner config YAML file (recommended)

Legacy Key Sources (if not using config file):
  --keys-file: Path to supplier.yaml containing hex-encoded private keys
  --keys-dir: Directory containing individual key files (YAML/JSON)
  --keyring-backend/--keyring-dir: Cosmos keyring integration

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
  pocketd relayminer ha miner --keys-file /path/to/supplier.yaml --redis-url redis://localhost:6379
`,
		RunE: runHAMiner,
	}

	// Config file (recommended approach)
	cmd.Flags().String(flagMinerConfig, "", "Path to miner config YAML file")

	// Legacy key source flags (for backwards compatibility)
	cmd.Flags().String(flagKeysFile, "", "Path to supplier.yaml with hex-encoded private keys")
	cmd.Flags().String(flagKeysDir, "", "Directory containing individual key files (YAML/JSON)")
	cmd.Flags().String(flagKeyringBackend, "", "Cosmos keyring backend: file, os, test")
	cmd.Flags().String(flagKeyringDir, "", "Cosmos keyring directory")

	// Redis flags (can override config)
	cmd.Flags().String(flagRedisURL, "", "Redis connection URL (overrides config)")
	cmd.Flags().String(flagConsumerName, "", "Consumer name (defaults to hostname)")

	// Configuration flags (can override config)
	cmd.Flags().Bool(flagHotReload, true, "Enable hot-reload of keys")
	cmd.Flags().Duration(flagSessionTTL, 0, "Session data TTL (default: same as cache_ttl to prevent orphaned sessions)")

	return cmd
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
		if err = redisClient.Close(); err != nil {
			logger.Error().Err(err).Msg("failed to close Redis client")
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

	// Create key providers from config
	providers, err := createKeyProviders(logger, config)
	if err != nil {
		return err
	}

	if len(providers) == 0 {
		return fmt.Errorf("no key providers configured")
	}

	// Create a key manager
	keyManager := keys.NewMultiProviderKeyManager(
		logger,
		providers,
		keys.KeyManagerConfig{
			HotReloadEnabled: config.HotReloadEnabled,
		},
	)
	defer func() { _ = keyManager.Close() }()

	// Start key manager
	if err = keyManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start key manager: %w", err)
	}

	// Check if any keys were loaded - FAIL FAST if no keys
	// This prevents the miner from silently running with no keys and doing nothing.
	// Users with invalid key files will see clear error messages instead of exit 0.
	suppliers := keyManager.ListSuppliers()
	if len(suppliers) == 0 {
		return fmt.Errorf("no supplier keys loaded at startup - cannot proceed. " +
			"Check your key file configuration and ensure at least one valid key is provided. " +
			"Key file errors are logged above with details about what's wrong")
	}
	logger.Info().
		Int("count", len(suppliers)).
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
		Int("suppliers", len(suppliers)).
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
	})

	// Register leader election callbacks
	// On elected: Start all leader-only resources
	globalLeader.OnElected(func(ctx context.Context) {
		logger.Info().Msg("starting leader controller (became leader)")
		if err = leaderController.Start(ctx); err != nil {
			logger.Fatal().Err(err).Msg("failed to start leader controller - exiting process")
		}
	})

	// On lost: Clean up all resources
	globalLeader.OnLost(func(ctx context.Context) {
		logger.Info().Msg("stopping leader controller (lost leadership)")
		if err = leaderController.Close(); err != nil {
			logger.Fatal().Err(err).Msg("failed to close leader controller")
		}
	})

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
		if err = leaderController.Close(); err != nil {
			logger.Error().Err(err).Msg("failed to close leader controller")
		}
	}()

	logger.Info().
		Str("consumer_name", config.Redis.ConsumerName).
		Bool("hot_reload", config.HotReloadEnabled).
		Msg("HA Miner started")

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Wait for a shutdown signal
	<-sigCh
	logger.Info().Msg("shutdown signal received, stopping HA Miner...")

	// Deferring handles graceful shutdown
	logger.Info().Msg("HA Miner stopped")
	return nil
}

// loadMinerConfig loads the miner configuration from a file or flags.
func loadMinerConfig(cmd *cobra.Command) (*miner.Config, error) {
	configPath, _ := cmd.Flags().GetString(flagMinerConfig)

	var config *miner.Config
	var err error

	if configPath != "" {
		// Load from a config file
		config, err = miner.LoadConfig(configPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load config: %w", err)
		}
	} else {
		// Build config from flags (legacy mode)
		config = miner.DefaultConfig()

		// Key sources
		keysFile, _ := cmd.Flags().GetString(flagKeysFile)
		keysDir, _ := cmd.Flags().GetString(flagKeysDir)
		keyringBackend, _ := cmd.Flags().GetString(flagKeyringBackend)
		keyringDir, _ := cmd.Flags().GetString(flagKeyringDir)

		config.Keys.KeysFile = keysFile
		config.Keys.KeysDir = keysDir
		if keyringBackend != "" {
			if keyringDir == "" {
				keyringDir = os.ExpandEnv("$HOME/.pocket")
			}
			config.Keys.Keyring = &configpkg.KeyringConfig{
				Backend: keyringBackend,
				Dir:     keyringDir,
			}
		}

		// Validate key sources
		if !config.HasKeySource() {
			return nil, fmt.Errorf("at least one key source must be specified: --config, --keys-file, --keys-dir, or --keyring-backend")
		}
	}

	// Apply flag overrides (flags take precedence over config file)
	applyFlagOverrides(cmd, config)

	// Generate consumer name from hostname if not set
	if config.Redis.ConsumerName == "" {
		hostname, _ := os.Hostname()
		config.Redis.ConsumerName = fmt.Sprintf("miner-%s-%d", hostname, os.Getpid())
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
		config.HotReloadEnabled, _ = cmd.Flags().GetBool(flagHotReload)
	}
	if cmd.Flags().Changed(flagSessionTTL) {
		config.SessionTTL, _ = cmd.Flags().GetDuration(flagSessionTTL)
	}
}

// createKeyProviders creates key providers based on the config.
func createKeyProviders(logger logging.Logger, config *miner.Config) ([]keys.KeyProvider, error) {
	var providers []keys.KeyProvider

	if config.Keys.KeysFile != "" {
		provider, err := keys.NewSupplierKeysFileProvider(logger, config.Keys.KeysFile)
		if err != nil {
			return nil, fmt.Errorf("failed to create supplier keys file provider: %w", err)
		}
		providers = append(providers, provider)
		logger.Info().Str("file", config.Keys.KeysFile).Msg("added supplier keys file provider")
	}

	if config.Keys.KeysDir != "" {
		provider, err := keys.NewFileKeyProvider(logger, config.Keys.KeysDir)
		if err != nil {
			return nil, fmt.Errorf("failed to create file key provider: %w", err)
		}
		providers = append(providers, provider)
		logger.Info().Str("dir", config.Keys.KeysDir).Msg("added file key provider")
	}

	if config.Keys.Keyring != nil && config.Keys.Keyring.Backend != "" {
		keyringDir := config.Keys.Keyring.Dir
		if keyringDir == "" {
			keyringDir = os.ExpandEnv("$HOME/.pocket")
		}
		provider, err := keys.NewKeyringProvider(logger, keys.KeyringProviderConfig{
			Backend:  config.Keys.Keyring.Backend,
			Dir:      keyringDir,
			AppName:  config.Keys.Keyring.AppName,
			KeyNames: config.Keys.Keyring.KeyNames,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create keyring provider: %w", err)
		}
		providers = append(providers, provider)
		logger.Info().
			Str("backend", config.Keys.Keyring.Backend).
			Str("dir", keyringDir).
			Msg("added keyring provider")
	}

	return providers, nil
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

	// Validate key sources
	if !config.HasKeySource() {
		return fmt.Errorf("at least one key source must be configured (keys_file, keys_dir, or keyring)")
	}

	// Note: SessionTTL = 0 means use CacheTTL (default 2h), so it doesn't need validation

	return nil
}
