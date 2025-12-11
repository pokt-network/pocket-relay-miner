package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/cache"
	haclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/transport"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

const (
	flagMinerConfig    = "config"
	flagKeysFile       = "keys-file"
	flagKeysDir        = "keys-dir"
	flagKeyringBackend = "keyring-backend"
	flagKeyringDir     = "keyring-dir"
	flagConsumerGroup  = "consumer-group"
	flagConsumerName   = "consumer-name"
	flagStreamPrefix   = "stream-prefix"
	flagHotReload      = "hot-reload"
	flagSessionTTL     = "session-ttl"
	flagWALMaxLen      = "wal-max-len"
)

// startMinerCmd returns the command for starting the HA Miner component.
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
	cmd.Flags().String(flagConsumerGroup, "", "Redis consumer group name (overrides config)")
	cmd.Flags().String(flagConsumerName, "", "Consumer name (defaults to hostname)")
	cmd.Flags().String(flagStreamPrefix, "", "Redis stream name prefix (overrides config)")

	// Configuration flags (can override config)
	cmd.Flags().Bool(flagHotReload, true, "Enable hot-reload of keys")
	cmd.Flags().Duration(flagSessionTTL, 24*time.Hour, "Session data TTL")
	cmd.Flags().Int64(flagWALMaxLen, 100000, "Maximum WAL entries per session")

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

	// Start observability server (metrics and pprof)
	if config.Metrics.Enabled {
		obsServer := observability.NewServer(logger, observability.ServerConfig{
			MetricsEnabled: config.Metrics.Enabled,
			MetricsAddr:    config.Metrics.Addr,
			PprofEnabled:   false, // pprof not yet configurable for miner
			Registry:       observability.MinerRegistry,
		})
		if err := obsServer.Start(ctx); err != nil {
			return fmt.Errorf("failed to start observability server: %w", err)
		}
		defer func() { _ = obsServer.Stop() }()
		logger.Info().Str("addr", config.Metrics.Addr).Msg("observability server started")
	}

	// Parse Redis URL
	redisOpts, err := redis.ParseURL(config.Redis.URL)
	if err != nil {
		return fmt.Errorf("failed to parse Redis URL: %w", err)
	}
	redisClient := redis.NewClient(redisOpts)
	defer func() { _ = redisClient.Close() }()

	// Test Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}
	logger.Info().
		Str("redis_url", config.Redis.URL).
		Str("consumer_name", config.Redis.ConsumerName).
		Msg("connected to Redis")

	// Create key providers from config
	providers, err := createKeyProviders(logger, config)
	if err != nil {
		return err
	}

	if len(providers) == 0 {
		return fmt.Errorf("no key providers configured")
	}

	// Create key manager
	keyManager := keys.NewMultiProviderKeyManager(
		logger,
		providers,
		keys.KeyManagerConfig{
			HotReloadEnabled: config.HotReloadEnabled,
		},
	)

	// Start key manager
	if err := keyManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start key manager: %w", err)
	}
	defer func() { _ = keyManager.Close() }()

	// Check if any keys were loaded
	suppliers := keyManager.ListSuppliers()
	if len(suppliers) == 0 {
		logger.Warn().Msg("no supplier keys found - miner will wait for keys to be added")
	} else {
		logger.Info().
			Int("count", len(suppliers)).
			Msg("loaded supplier keys")
	}

	// Create supplier registry
	registry := miner.NewSupplierRegistry(
		logger,
		redisClient,
		miner.SupplierRegistryConfig{
			KeyPrefix:    "ha:suppliers",
			IndexKey:     "ha:suppliers:index",
			EventChannel: "ha:events:supplier_update",
		},
	)

	// Create supplier cache for publishing supplier state to relayers
	supplierCache := cache.NewSupplierCache(
		logger,
		redisClient,
		cache.SupplierCacheConfig{
			KeyPrefix: "ha:supplier",
			FailOpen:  false, // Miner should fail-closed for writes
		},
	)
	logger.Info().Msg("supplier cache initialized for state publishing")

	// Create query clients to query supplier information from the blockchain
	queryClients, err := query.NewQueryClients(
		logger,
		query.QueryClientConfig{
			GRPCEndpoint: config.PocketNode.QueryNodeGRPCUrl,
			QueryTimeout: 30 * time.Second,
			UseTLS:       !config.PocketNode.GRPCInsecure,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	defer func() { _ = queryClients.Close() }()
	logger.Info().Str("grpc_endpoint", config.PocketNode.QueryNodeGRPCUrl).Msg("query clients initialized")

	// Create proof requirement checker for probabilistic proof selection
	// This determines whether a proof is required for a claimed session based on:
	// 1. Threshold check: High-value claims always require proof
	// 2. Probabilistic check: Random selection based on claim hash + block hash
	proofChecker := miner.NewProofRequirementChecker(
		logger,
		queryClients.Proof(),
		queryClients.Shared(),
		queryClients.Service(),
	)
	logger.Info().Msg("proof requirement checker initialized")

	// Create block subscriber for monitoring new blocks via WebSocket subscription
	// Uses tm.event='NewBlockHeader' for immediate block notifications with automatic reconnection
	blockSubscriber, err := haclient.NewBlockSubscriber(
		logger,
		haclient.BlockSubscriberConfig{
			RPCEndpoint: config.PocketNode.QueryNodeRPCUrl,
			UseTLS:      !config.PocketNode.GRPCInsecure,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create block subscriber: %w", err)
	}
	if err := blockSubscriber.Start(ctx); err != nil {
		return fmt.Errorf("failed to start block subscriber: %w", err)
	}
	defer func() { blockSubscriber.Close() }()
	logger.Info().Msg("block subscriber started (WebSocket)")

	// Create Redis block subscriber for distributing block events across instances
	redisBlockSubscriber := cache.NewRedisBlockSubscriber(
		logger,
		redisClient,
		blockSubscriber, // Uses WebSocket client as source
		cache.DefaultCacheConfig(),
	)
	if err := redisBlockSubscriber.Start(ctx); err != nil {
		return fmt.Errorf("failed to start redis block subscriber: %w", err)
	}
	defer func() { _ = redisBlockSubscriber.Close() }()
	logger.Info().Msg("redis block subscriber started")

	// Generate unique instance ID for global leader election
	hostname, _ := os.Hostname()
	instanceID := fmt.Sprintf("%s-%d", hostname, os.Getpid())

	// Create global leader elector (single source of truth for leadership)
	globalLeader := leader.NewGlobalLeaderElector(
		logger,
		redisClient,
		instanceID,
	)
	if err := globalLeader.Start(ctx); err != nil {
		return fmt.Errorf("failed to start global leader elector: %w", err)
	}
	defer func() { globalLeader.Close() }()
	logger.Info().
		Str("instance_id", instanceID).
		Msg("global leader elector started")

	// Create shared params cache
	sharedParamsCache := cache.NewSharedParamsCache(
		logger,
		redisClient,
		queryClients.Shared(),
	)
	if err := sharedParamsCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start shared params cache: %w", err)
	}
	defer func() { _ = sharedParamsCache.Close() }()

	// Create session params cache
	sessionParamsCache := cache.NewSessionParamsCache(
		logger,
		redisClient,
		cache.NewSessionQueryClientAdapter(queryClients.Session()),
	)
	if err := sessionParamsCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start session params cache: %w", err)
	}
	defer func() { _ = sessionParamsCache.Close() }()

	// Create proof params cache
	proofParamsCache := cache.NewProofParamsCache(
		logger,
		redisClient,
		cache.NewProofQueryClientAdapter(queryClients.Proof()),
	)
	if err := proofParamsCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start proof params cache: %w", err)
	}
	defer func() { _ = proofParamsCache.Close() }()

	// Create application cache
	applicationCache := cache.NewApplicationCache(
		logger,
		redisClient,
		cache.NewApplicationQueryClientAdapter(queryClients.Application()),
	)
	if err := applicationCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start application cache: %w", err)
	}
	defer func() { _ = applicationCache.Close() }()

	// Create service cache
	serviceCache := cache.NewServiceCache(
		logger,
		redisClient,
		cache.NewServiceQueryClientAdapter(queryClients.Service()),
	)
	if err := serviceCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start service cache: %w", err)
	}
	defer func() { _ = serviceCache.Close() }()

	// Start supplier cache for pub/sub subscription
	if err := supplierCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start supplier cache: %w", err)
	}
	defer func() { _ = supplierCache.Close() }()

	// Create session cache (placeholder for now - will be implemented separately)
	var sessionCache cache.SessionCache

	// Create cache orchestrator to coordinate all caches
	cacheOrchestrator := cache.NewCacheOrchestrator(
		logger,
		cache.CacheOrchestratorConfig{
			RefreshWorkers:    config.GetCacheRefreshWorkers(),
			KnownApplications: config.KnownApplications,
		},
		globalLeader,
		redisBlockSubscriber,
		redisClient,
		sharedParamsCache,
		sessionParamsCache,
		proofParamsCache,
		applicationCache,
		serviceCache,
		supplierCache,
		sessionCache,
	)

	if err := cacheOrchestrator.Start(ctx); err != nil {
		return fmt.Errorf("failed to start cache orchestrator: %w", err)
	}
	defer func() { _ = cacheOrchestrator.Close() }()
	logger.Info().Msg("cache orchestrator started (parallel refresh with 6 workers)")

	// Fetch chain ID from the node
	chainID, err := blockSubscriber.GetChainID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get chain ID from node: %w", err)
	}
	logger.Info().Str("chain_id", chainID).Msg("fetched chain ID from node")

	// Create transaction client for submitting claims and proofs
	// Reuse the gRPC connection from QueryClients to avoid creating a duplicate connection
	txClient, err := tx.NewTxClient(
		logger,
		keyManager,
		tx.TxClientConfig{
			GRPCConn:      queryClients.GRPCConnection(), // Share gRPC connection with QueryClients
			ChainID:       chainID,
			GasLimit:      tx.DefaultGasLimit,
			TimeoutBlocks: tx.DefaultTimeoutHeight,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create transaction client: %w", err)
	}
	defer func() { _ = txClient.Close() }()
	logger.Info().Str("grpc_endpoint", config.PocketNode.QueryNodeGRPCUrl).Msg("transaction client initialized")

	// Create supplier manager
	supplierManager := miner.NewSupplierManager(
		logger,
		keyManager,
		registry,
		miner.SupplierManagerConfig{
			RedisClient:         redisClient,
			StreamPrefix:        config.Redis.StreamPrefix,
			ConsumerGroup:       config.Redis.ConsumerGroup,
			ConsumerName:        config.Redis.ConsumerName,
			SessionTTL:          config.SessionTTL,
			WALMaxLen:           config.WALMaxLen,
			SupplierCache:       supplierCache,
			MinerID:             config.Redis.ConsumerName,
			SupplierQueryClient: queryClients.Supplier(),
			// New clients for claim/proof lifecycle management
			TxClient:      txClient,
			BlockClient:   blockSubscriber,
			SharedClient:  queryClients.Shared(),
			SessionClient: queryClients.Session(),

			// Proof requirement checker for probabilistic proof selection
			ProofChecker: proofChecker,
		},
	)

	// Set relay handler
	supplierManager.SetRelayHandler(func(ctx context.Context, supplierAddr string, msg *transport.StreamMessage) error {
		state, ok := supplierManager.GetSupplierState(supplierAddr)
		if !ok {
			return fmt.Errorf("supplier state not found: %s", supplierAddr)
		}

		// Track discovered apps and services for cache orchestrator
		if msg.Message.ApplicationAddress != "" {
			cacheOrchestrator.RecordDiscoveredApp(msg.Message.ApplicationAddress)
		}
		if msg.Message.ServiceId != "" {
			cacheOrchestrator.RecordDiscoveredService(msg.Message.ServiceId)
		}

		// Use the full metadata method to create session if it doesn't exist
		// This updates the WAL and session snapshot in Redis
		if err := state.SnapshotManager.OnRelayMinedWithMetadata(
			ctx,
			msg.Message.SessionId,
			msg.Message.RelayHash,
			msg.Message.RelayBytes,
			msg.Message.ComputeUnitsPerRelay,
			msg.Message.SupplierOperatorAddress,
			msg.Message.ServiceId,
			msg.Message.ApplicationAddress,
			msg.Message.SessionStartHeight,
			msg.Message.SessionEndHeight,
		); err != nil {
			return fmt.Errorf("failed to update snapshot manager: %w", err)
		}

		// Also update the in-memory SMST for claim/proof generation
		// This is critical - without this, FlushTree will fail with "session not found"
		return state.SMSTManager.UpdateTree(
			ctx,
			msg.Message.SessionId,
			msg.Message.RelayHash,
			msg.Message.RelayBytes,
			msg.Message.ComputeUnitsPerRelay,
		)
	})

	// Start supplier manager
	if err := supplierManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start supplier manager: %w", err)
	}
	defer func() { _ = supplierManager.Close() }()

	logger.Info().
		Int("suppliers", len(supplierManager.ListSuppliers())).
		Str("consumer_group", config.Redis.ConsumerGroup).
		Str("consumer_name", config.Redis.ConsumerName).
		Bool("hot_reload", config.HotReloadEnabled).
		Msg("HA Miner started")

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Wait for shutdown signal
	<-sigCh
	logger.Info().Msg("shutdown signal received, stopping HA Miner...")

	// Graceful shutdown is handled by defers
	logger.Info().Msg("HA Miner stopped")
	return nil
}

// loadMinerConfig loads the miner configuration from file or flags.
func loadMinerConfig(cmd *cobra.Command) (*miner.Config, error) {
	configPath, _ := cmd.Flags().GetString(flagMinerConfig)

	var config *miner.Config
	var err error

	if configPath != "" {
		// Load from config file
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
			config.Keys.Keyring = &miner.KeyringConfig{
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
		redisURL, _ := cmd.Flags().GetString(flagRedisURL)
		config.Redis.URL = redisURL
	}
	if cmd.Flags().Changed(flagConsumerGroup) {
		consumerGroup, _ := cmd.Flags().GetString(flagConsumerGroup)
		config.Redis.ConsumerGroup = consumerGroup
	}
	if cmd.Flags().Changed(flagConsumerName) {
		consumerName, _ := cmd.Flags().GetString(flagConsumerName)
		config.Redis.ConsumerName = consumerName
	}
	if cmd.Flags().Changed(flagStreamPrefix) {
		streamPrefix, _ := cmd.Flags().GetString(flagStreamPrefix)
		config.Redis.StreamPrefix = streamPrefix
	}
	if cmd.Flags().Changed(flagHotReload) {
		hotReload, _ := cmd.Flags().GetBool(flagHotReload)
		config.HotReloadEnabled = hotReload
	}
	if cmd.Flags().Changed(flagSessionTTL) {
		sessionTTL, _ := cmd.Flags().GetDuration(flagSessionTTL)
		config.SessionTTL = sessionTTL
	}
	if cmd.Flags().Changed(flagWALMaxLen) {
		walMaxLen, _ := cmd.Flags().GetInt64(flagWALMaxLen)
		config.WALMaxLen = walMaxLen
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
	if config.Redis.StreamPrefix == "" {
		return fmt.Errorf("redis.stream_prefix is required")
	}
	if config.Redis.ConsumerGroup == "" {
		return fmt.Errorf("redis.consumer_group is required")
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

	// Validate timeouts and limits
	if config.SessionTTL <= 0 {
		return fmt.Errorf("session_ttl must be positive")
	}
	if config.WALMaxLen <= 0 {
		return fmt.Errorf("wal_max_len must be positive")
	}

	return nil
}
