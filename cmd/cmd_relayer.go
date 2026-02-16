package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alitto/pond/v2"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/relayer"
	"github.com/pokt-network/pocket-relay-miner/rings"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const (
	flagRelayerConfig = "config"
	flagRedisURL      = "redis-url"

	// Pocket Network Bech32 address prefix
	// Reference: poktroll/app/app.go:49
	accountAddressPrefix = "pokt"
)

// initSDKConfig initializes the Cosmos SDK configuration with Pocket Network's Bech32 prefix.
// This is required for address validation to work correctly with "pokt" addresses.
func initSDKConfig() {
	config := sdk.GetConfig()

	// Check if already initialized (config may be sealed)
	if config.GetBech32AccountAddrPrefix() == accountAddressPrefix {
		return
	}

	// Set Bech32 prefixes for Pocket Network
	accountPubKeyPrefix := accountAddressPrefix + "pub"
	validatorAddressPrefix := accountAddressPrefix + "valoper"
	validatorPubKeyPrefix := accountAddressPrefix + "valoperpub"
	consNodeAddressPrefix := accountAddressPrefix + "valcons"
	consNodePubKeyPrefix := accountAddressPrefix + "valconspub"

	config.SetBech32PrefixForAccount(accountAddressPrefix, accountPubKeyPrefix)
	config.SetBech32PrefixForValidator(validatorAddressPrefix, validatorPubKeyPrefix)
	config.SetBech32PrefixForConsensusNode(consNodeAddressPrefix, consNodePubKeyPrefix)
	config.Seal()
}

// startRelayerCmd returns the command for starting the HA Relayer component.
func RelayerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relayer",
		Short: "Start the HA Relayer (HTTP/WebSocket proxy)",
		Long: `Start the High-Availability Relayer component.

The HA Relayer handles incoming relay requests and forwards them to backend services.
It is stateless and can be scaled horizontally behind a load balancer.

Features:
- HTTP and WebSocket relay proxying
- Request validation and signing
- Health checking for backends
- Prometheus metrics at /metrics

Example:
  pocketd relayminer ha relayer --config /path/to/ha-relayer.yaml --redis-url redis://localhost:6379
`,
		RunE: runHARelayer,
	}

	cmd.Flags().String(flagRelayerConfig, "", "Path to HA relayer config file (required)")
	cmd.Flags().String(flagRedisURL, "redis://localhost:6379", "Redis connection URL")

	_ = cmd.MarkFlagRequired(flagRelayerConfig)

	return cmd
}

// serviceDifficultyQueryAdapter adapts the query.ServiceDifficultyClient to the
// relayer.ServiceDifficultyQueryClient interface by extracting TargetHash.
// It uses the height-aware difficulty query to get difficulty at the session start height,
// ensuring consistency with on-chain proof validation.
type serviceDifficultyQueryAdapter struct {
	queryClient query.ServiceDifficultyClient
}

func (a *serviceDifficultyQueryAdapter) GetServiceRelayDifficulty(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error) {
	difficulty, err := a.queryClient.GetServiceRelayDifficultyAtHeight(ctx, serviceID, sessionStartHeight)
	if err != nil {
		return nil, err
	}
	return difficulty.TargetHash, nil
}

func runHARelayer(cmd *cobra.Command, _ []string) error {
	// Initialize Cosmos SDK config with "pokt" Bech32 prefix
	// This must be done before any address validation occurs
	initSDKConfig()

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// Load config first (needed for logger configuration)
	configPath, _ := cmd.Flags().GetString(flagRelayerConfig)
	config, err := relayer.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Set up logger from config
	logger := logging.NewLoggerFromConfig(config.Logging)

	// Start observability server (metrics and pprof)
	if config.Metrics.Enabled || config.Pprof.Enabled {
		// Default pprof addr to localhost:6060 for security if not specified
		pprofAddr := config.Pprof.Addr
		if pprofAddr == "" {
			pprofAddr = "localhost:6060"
		}

		// Combine RelayerRegistry and SharedRegistry so cache metrics are exposed
		combinedRegistry := prometheus.Gatherers{
			observability.RelayerRegistry,
			observability.SharedRegistry,
		}

		obsServer := observability.NewServer(logger, observability.ServerConfig{
			MetricsEnabled: config.Metrics.Enabled,
			MetricsAddr:    config.Metrics.Addr,
			PprofEnabled:   config.Pprof.Enabled,
			PprofAddr:      pprofAddr,
			Registry:       combinedRegistry,
		})
		if err := obsServer.Start(ctx); err != nil {
			return fmt.Errorf("failed to start observability server: %w", err)
		}
		defer func() { _ = obsServer.Stop() }()
		logger.Info().Str("addr", config.Metrics.Addr).Msg("observability server started")

		// Start runtime metrics collector
		runtimeMetrics := observability.NewRuntimeMetricsCollector(
			logger,
			observability.DefaultRuntimeMetricsCollectorConfig(),
			observability.RelayerFactory,
		)
		if err := runtimeMetrics.Start(ctx); err != nil {
			return fmt.Errorf("failed to start runtime metrics collector: %w", err)
		}
		defer runtimeMetrics.Stop()
		logger.Info().Msg("runtime metrics collector started")
	}

	// Use Redis URL from config, allow flag override
	redisURL := config.Redis.URL
	if cmd.Flags().Changed(flagRedisURL) {
		redisURL, _ = cmd.Flags().GetString(flagRedisURL)
	}

	// Create wrapped Redis client with KeyBuilder for namespace-aware key construction
	redisClient, err := redistransport.NewClient(ctx, redistransport.ClientConfig{
		URL:                    redisURL,
		PoolSize:               config.Redis.PoolSize,
		MinIdleConns:           config.Redis.MinIdleConns,
		PoolTimeoutSeconds:     config.Redis.PoolTimeoutSeconds,
		ConnMaxIdleTimeSeconds: config.Redis.ConnMaxIdleTimeSeconds,
		Namespace:              config.Redis.Namespace,
	})
	if err != nil {
		return fmt.Errorf("failed to create Redis client: %w", err)
	}
	defer func() { _ = redisClient.Close() }()
	logger.Info().Str("redis_url", redisURL).Msg("connected to Redis")

	// Create supplier cache for checking supplier staking state
	supplierCache := cache.NewSupplierCache(
		logger,
		redisClient,
		cache.SupplierCacheConfig{
			KeyPrefix: "ha:supplier",
			FailOpen:  true, // Prioritize serving traffic over strict validation
		},
	)
	// Start supplier cache for pub/sub subscription
	if err := supplierCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start supplier cache: %w", err)
	}
	defer func() { _ = supplierCache.Close() }()
	logger.Info().Msg("supplier cache initialized and started")

	// Warmup supplier cache from Redis (load existing suppliers into L1 cache)
	// This prevents relying solely on pub/sub, which can miss messages if relayer starts after miner
	if err := supplierCache.WarmupFromRedis(ctx, nil); err != nil {
		return fmt.Errorf("failed to warmup supplier cache: %w", err)
	}

	// Create query clients for fetching on-chain data (service compute units, etc.)
	// Determine TLS setting - use config.PocketNode.GRPCInsecure to match miner behavior
	grpcURL := config.PocketNode.QueryNodeGRPCUrl
	// Strip scheme for gRPC endpoint
	grpcEndpoint := grpcURL
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "grpcs://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "grpc://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "https://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "http://")

	queryClients, err := query.NewQueryClients(
		logger,
		query.ClientConfig{
			GRPCEndpoint: grpcEndpoint,
			QueryTimeout: 30 * time.Second,
			UseTLS:       !config.PocketNode.GRPCInsecure,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	defer func() { _ = queryClients.Close() }()
	logger.Info().Str("grpc_endpoint", grpcEndpoint).Bool("tls", !config.PocketNode.GRPCInsecure).Msg("query clients initialized")

	// Create new entity caches (relayer only subscribes to pub/sub, doesn't refresh)
	// These caches provide L1/L2/L3 pattern with pub/sub invalidation

	// Relayer uses default 30s block time (production default for mainnet/testnet)
	blockTimeSeconds := int64(30)

	// Create session params cache with dynamic session-duration TTL
	sessionParamsCache := cache.NewSessionParamsCache(
		logger,
		redisClient,
		cache.NewSessionQueryClientAdapter(queryClients.Session()),
		queryClients.Shared(), // For TTL calculation
		blockTimeSeconds,
	)
	if err := sessionParamsCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start session params cache: %w", err)
	}
	defer func() { _ = sessionParamsCache.Close() }()

	// Create shared params cache with dynamic 2-session TTL
	sharedParamsCache := cache.NewSharedParamsCache(
		logger,
		redisClient,
		queryClients.Shared(),
		blockTimeSeconds,
	)
	if err := sharedParamsCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start shared params cache: %w", err)
	}
	defer func() { _ = sharedParamsCache.Close() }()

	// Create proof params cache with dynamic session-duration TTL
	proofParamsCache := cache.NewProofParamsCache(
		logger,
		redisClient,
		cache.NewProofQueryClientAdapter(queryClients.Proof()),
		queryClients.Shared(), // For TTL calculation
		blockTimeSeconds,
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

	// Create account cache for public key lookups (used by ring client)
	// IMPORTANT: Public keys are immutable, so this cache has NO EXPIRY
	accountCache := cache.NewAccountCache(
		logger,
		redisClient,
		queryClients.Account(),
	)
	if err := accountCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start account cache: %w", err)
	}
	defer func() { _ = accountCache.Close() }()

	logger.Info().Msg("entity caches started (pub/sub subscribers)")

	// Cache warmup: Pre-warm L1 caches from L2 (Redis) for known applications
	// This significantly speeds up cold starts by avoiding L3 (chain) queries during first relays
	if config.CacheWarmup.Enabled {
		logger.Info().
			Int("known_apps", len(config.CacheWarmup.KnownApplications)).
			Bool("persist_discovered", config.CacheWarmup.PersistDiscoveredApps).
			Int("concurrency", config.CacheWarmup.WarmupConcurrency).
			Msg("starting cache warmup for faster cold starts")

		// Create ring client first (needed by cache warmer)
		ringClientForWarmup := rings.NewRingClient(
			logger,
			cache.NewCachedApplicationQueryClient(applicationCache),
			cache.NewCachedAccountQueryClient(accountCache),
			cache.NewCachedSharedQueryClient(sharedParamsCache, queryClients.Shared()),
		)

		// Create cache warmer
		cacheWarmer := cache.NewCacheWarmer(
			logger,
			cache.CacheWarmerConfig{
				KnownApplications:     config.CacheWarmup.KnownApplications,
				PersistDiscoveredApps: config.CacheWarmup.PersistDiscoveredApps,
				WarmupConcurrency:     config.CacheWarmup.WarmupConcurrency,
				WarmupTimeout:         time.Duration(config.CacheWarmup.WarmupTimeoutSeconds) * time.Second,
			},
			redisClient,
			queryClients.Application(),
			queryClients.Account(),
			queryClients.Shared(),
			ringClientForWarmup,
		)
		// Ensure worker pool cleanup
		defer cacheWarmer.Stop()

		// Execute warmup (this populates L1 cache from L2/L3)
		warmupCtx, warmupCancel := context.WithTimeout(ctx, 30*time.Second)
		warmupResult, err := cacheWarmer.Warmup(warmupCtx)
		warmupCancel()

		if err != nil {
			logger.Warn().Err(err).Msg("cache warmup failed (continuing anyway)")
		} else {
			logger.Info().
				Int("total_apps", warmupResult.TotalApps).
				Int("warmed", warmupResult.WarmedApps).
				Int("failed", warmupResult.FailedApps).
				Int64("duration_ms", warmupResult.DurationMs).
				Msg("cache warmup completed - relayer ready with pre-warmed caches")
		}
	} else {
		logger.Info().Msg("cache warmup disabled - caches will populate on-demand during relays")
	}

	// Verify RPC endpoint connectivity via HTTP /status (health check)
	rpcURL := config.PocketNode.QueryNodeRPCUrl
	if err := verifyRPCConnectivity(ctx, logger, rpcURL); err != nil {
		logger.Warn().Err(err).Str("rpc_url", rpcURL).Msg("RPC health check failed (continuing anyway)")
	} else {
		logger.Info().Str("rpc_url", rpcURL).Msg("RPC endpoint connectivity verified")
	}

	// Verify gRPC endpoint connectivity (health check)
	if err = verifyGRPCConnectivity(ctx, logger, grpcURL, queryClients); err != nil {
		logger.Warn().Err(err).Str("grpc_url", grpcURL).Msg("gRPC health check failed (continuing anyway)")
	} else {
		logger.Info().Str("grpc_url", grpcURL).Msg("gRPC endpoint connectivity verified")
	}

	// Create Redis block subscriber to receive block events from miner
	// Relayers use Redis pub/sub for block synchronization (no WebSocket connections).
	redisBlockSubscriber := cache.NewRedisBlockSubscriber(
		logger,
		redisClient,
		nil, // No direct blockchain client - events come from miner via Redis
	)
	if err := redisBlockSubscriber.Start(ctx); err != nil {
		return fmt.Errorf("failed to start redis block subscriber: %w", err)
	}
	defer func() { _ = redisBlockSubscriber.Close() }()
	logger.Info().Msg("redis block subscriber started (receiving events from miner)")

	// Create RedisBlockClientAdapter to implement client.BlockClient interface
	// This adapter receives events from Redis pub/sub and provides the BlockClient
	// interface required by relayer components (proxy, relay meter, etc.)
	// Relayers don't need to query specific blocks, so pass nil for RPC client
	blockSubscriber := cache.NewRedisBlockClientAdapter(
		logger,
		redisBlockSubscriber,
		nil, // Relayers only need block events, not specific block queries
	)
	if err := blockSubscriber.Start(ctx); err != nil {
		return fmt.Errorf("failed to start block client adapter: %w", err)
	}
	defer func() { blockSubscriber.Close() }()
	logger.Info().Msg("block client adapter started (Redis-backed)")

	// NOTE: Cache warmup is now handled by the miner's CacheOrchestrator.
	// The relayer's ApplicationCache.WarmupFromRedis() will populate L1 from L2 on startup.
	// Discovery of apps/services happens on the miner side when processing relays from Redis streams.

	// Create publisher for mined relays
	// CacheTTL default: 2h if not configured
	cacheTTL := config.RelayMeter.CacheTTL
	if cacheTTL == 0 {
		cacheTTL = 2 * time.Hour
	}
	publisher := redistransport.NewStreamsPublisher(
		logger,
		redisClient.UniversalClient,     // Embedded go-redis client
		redisClient.KB().StreamPrefix(), // Namespace-aware stream prefix (e.g., "ha:relays")
		cacheTTL,                        // TTL for relay streams (backup safety net)
	)

	// Create health checker
	healthChecker := relayer.NewHealthChecker(logger)

	// Create master worker pool for controlled concurrency
	// Uses unbounded queue with non-blocking submission to prevent goroutine explosion
	numCPU := runtime.NumCPU()
	masterPoolSize := numCPU * 8
	masterPool := pond.NewPool(
		masterPoolSize,
		pond.WithQueueSize(pond.Unbounded),
		pond.WithNonBlocking(true),
	)
	defer masterPool.StopAndWait()
	logger.Info().
		Int("max_workers", masterPoolSize).
		Int("num_cpu", numCPU).
		Msg("created master worker pool (unbounded, non-blocking, 8x CPU)")

	// Create proxy server
	proxy, err := relayer.NewProxyServer(
		logger,
		config,
		healthChecker,
		publisher,
		masterPool, // Pass master worker pool
	)
	if err != nil {
		return fmt.Errorf("failed to create proxy server: %w", err)
	}

	// Event-driven block height updates (replaces 1s polling)
	// Receives block events from Redis pub/sub for ~1-2ms latency (vs 1s polling)
	go func() {
		logger.Info().Msg("using event-driven block updates from Redis")
		for {
			select {
			case <-ctx.Done():
				return
			case block, ok := <-blockSubscriber.BlockEvents():
				if !ok {
					// Channel closed
					logger.Warn().Msg("block events channel closed")
					return
				}
				proxy.SetBlockHeight(block.Height())
			}
		}
	}()
	logger.Info().Msg("proxy subscribed to block height updates from Redis")

	// Load keys and create response signer
	// Support multiple key sources: keys_file, keys_dir, keyring
	var keyProviders []keys.KeyProvider

	// Try keys_file first (preferred for HA setup - same as miner)
	if config.Keys.KeysFile != "" {
		provider, keyErr := keys.NewSupplierKeysFileProvider(logger, config.Keys.KeysFile)
		if keyErr != nil {
			return fmt.Errorf("failed to create supplier keys file provider: %w", keyErr)
		}
		keyProviders = append(keyProviders, provider)
		logger.Info().Str("file", config.Keys.KeysFile).Msg("added supplier keys file provider")
	}

	// Try keys_dir as additional source (directory of individual key files)
	if config.Keys.KeysDir != "" {
		provider, keyErr := keys.NewFileKeyProvider(logger, config.Keys.KeysDir)
		if keyErr != nil {
			return fmt.Errorf("failed to create keys directory provider: %w", keyErr)
		}
		keyProviders = append(keyProviders, provider)
		logger.Info().Str("dir", config.Keys.KeysDir).Msg("added file key provider")
	}

	// Try keyring as additional source (can combine all)
	if config.Keys.Keyring != nil && config.Keys.Keyring.Backend != "" {
		provider, keyErr := keys.NewKeyringProvider(logger, keys.KeyringProviderConfig{
			Backend:  config.Keys.Keyring.Backend,
			Dir:      config.Keys.Keyring.Dir,
			AppName:  config.Keys.Keyring.AppName,
			KeyNames: config.Keys.Keyring.KeyNames,
		})
		if keyErr != nil {
			return fmt.Errorf("failed to create keyring provider: %w", keyErr)
		}
		keyProviders = append(keyProviders, provider)
		logger.Info().Str("backend", config.Keys.Keyring.Backend).Msg("added keyring provider")
	}

	if len(keyProviders) == 0 {
		logger.Warn().Msg("no key providers configured - response signing will be disabled (relays will fail)")
	} else {
		// Load keys from all providers (both return map[string]cryptotypes.PrivKey)
		loadedKeys := make(map[string]cryptotypes.PrivKey)
		for _, provider := range keyProviders {
			providerKeys, loadErr := provider.LoadKeys(ctx)
			if loadErr != nil {
				logger.Warn().Err(loadErr).Str("provider", provider.Name()).Msg("failed to load keys from provider")
				continue
			}
			for addr, key := range providerKeys {
				loadedKeys[addr] = key
			}
			logger.Info().Str("provider", provider.Name()).Int("keys", len(providerKeys)).Msg("loaded keys from provider")
		}

		// Close providers
		for _, provider := range keyProviders {
			_ = provider.Close()
		}

		if len(loadedKeys) == 0 {
			logger.Warn().Msg("no keys found - response signing will be disabled")
		} else {
			responseSigner, signerErr := relayer.NewResponseSigner(logger, loadedKeys)
			if signerErr != nil {
				return fmt.Errorf("failed to create response signer: %w", signerErr)
			}
			proxy.SetResponseSigner(responseSigner)
			logger.Info().
				Int("num_keys", len(responseSigner.GetOperatorAddresses())).
				Msg("response signer initialized")

			// Create RingClient for relay request signature verification
			// This is critical for security - it validates that relay requests
			// are properly signed by the application or a delegated gateway
			//
			// IMPORTANT: Use Redis-backed caches for all queries to minimize latency:
			// - Application cache: L1→L2→L3 for app delegation info
			// - Account cache: L1→L2→L3 for public key lookups (NO EXPIRY - keys are immutable)
			// - Shared params cache: L1→L2→L3 for session parameters
			//
			// At 1000 RPS, this prevents:
			// - 1000+ application queries/sec to blockchain
			// - 2000-4000 public key queries/sec to blockchain (N keys per ring)
			// - 1000+ shared params queries/sec to blockchain
			ringClient := rings.NewRingClient(
				logger,
				cache.NewCachedApplicationQueryClient(applicationCache),
				cache.NewCachedAccountQueryClient(accountCache),
				cache.NewCachedSharedQueryClient(sharedParamsCache, queryClients.Shared()),
			)

			// Create caches for full session validation
			cacheConfig := cache.CacheConfig{
				CachePrefix:      "ha:cache",
				PubSubPrefix:     "ha:events",
				TTLBlocks:        1,
				BlockTimeSeconds: 30, // CRITICAL: Must match actual block time (30s for this network, not 6s!)
			}

			// Create SharedParamCache for shared parameter caching
			sharedParamCache := cache.NewRedisSharedParamCache(
				logger,
				redisClient,
				queryClients.Shared(),
				blockSubscriber,
				cacheConfig,
			)
			if err := sharedParamCache.Start(ctx); err != nil {
				return fmt.Errorf("failed to start shared param cache: %w", err)
			}
			defer func() { _ = sharedParamCache.Close() }()
			logger.Info().Msg("shared param cache started")

			// Create SessionCache for session validation caching
			sessionCache := cache.NewRedisSessionCache(
				logger,
				redisClient,
				queryClients.Session(),
				queryClients.Shared(),
				blockSubscriber,
				cacheConfig,
			)
			if err := sessionCache.Start(ctx); err != nil {
				return fmt.Errorf("failed to start session cache: %w", err)
			}
			defer func() { _ = sessionCache.Close() }()
			logger.Info().Msg("session cache started")

			// Create full RelayValidator with all validations:
			// - Ring signature verification
			// - Session validity (not expired, within grace period)
			// - Supplier membership in session
			// - Application staking status (via session query)
			validatorConfig := &relayer.ValidatorConfig{
				AllowedSupplierAddresses: responseSigner.GetOperatorAddresses(),
			}
			fullValidator := relayer.NewRelayValidator(
				logger,
				validatorConfig,
				ringClient,
				sessionCache,
				sharedParamCache,
			)
			proxy.SetValidator(fullValidator)
			logger.Info().
				Int("allowed_suppliers", len(validatorConfig.AllowedSupplierAddresses)).
				Msg("full relay validator initialized with session validation")

			// Create RelayProcessor for proper relay mining with session metadata
			signerAdapter := relayer.NewResponseSignerAdapter(responseSigner)
			relayProcessor := relayer.NewRelayProcessor(
				logger,
				publisher,
				signerAdapter,
				ringClient, // Enable relay request signature verification
			)
			// Wire up the difficulty provider using on-chain service difficulty data
			difficultyProviderAdapter := &serviceDifficultyQueryAdapter{queryClient: queryClients.ServiceDifficulty()}
			difficultyProvider := relayer.NewQueryDifficultyProvider(logger, difficultyProviderAdapter)
			relayProcessor.SetDifficultyProvider(difficultyProvider)

			// Wire up the service compute units provider using on-chain service data
			computeUnitsProvider := relayer.NewCachedServiceComputeUnitsProvider(logger, queryClients.Service())
			// Preload compute units for configured services
			serviceIDs := make([]string, 0, len(config.Services))
			for serviceID := range config.Services {
				serviceIDs = append(serviceIDs, serviceID)
			}
			computeUnitsProvider.PreloadServiceComputeUnits(ctx, serviceIDs)
			relayProcessor.SetServiceComputeUnitsProvider(computeUnitsProvider)

			// NOTE: App discovery callbacks are no longer needed on the relayer.
			// Discovery now happens on the miner side via CacheOrchestrator.RecordDiscoveredApp()
			// when processing relays from Redis streams. Relayers only consume from shared caches.

			proxy.SetRelayProcessor(relayProcessor)
			logger.Info().Msg("relay processor initialized")

			// Create and wire relay meter for rate limiting based on app stakes
			if config.RelayMeter.Enabled {
				// Convert fail behavior string to type
				failBehavior := relayer.FailOpen // Default
				if config.RelayMeter.FailBehavior == "closed" {
					failBehavior = relayer.FailClosed
				}

				relayMeterConfig := relayer.RelayMeterConfig{
					RedisKeyPrefix: config.RelayMeter.RedisKeyPrefix,
					FailBehavior:   failBehavior,
					CacheTTL:       config.RelayMeter.CacheTTL,
				}

				// Create service factor client for reading service factors from Redis
				// Service factors are published by the miner
				serviceFactorClient := relayer.NewServiceFactorClient(
					logger,
					redisClient,
				)
				if err := serviceFactorClient.Start(ctx); err != nil {
					return fmt.Errorf("failed to start service factor client: %w", err)
				}
				defer func() { _ = serviceFactorClient.Close() }()

				relayMeter := relayer.NewRelayMeter(
					logger,
					redisClient,
					queryClients.Application(),
					queryClients.Shared(),
					queryClients.Session(),
					blockSubscriber,
					sharedParamCache,    // L1->L2->L3 cache for shared params (no Redis blocking!)
					serviceCache,        // L1->L2->L3 cache for service data (no Redis blocking!)
					serviceFactorClient, // Reads service factors from Redis (published by miner)
					relayMeterConfig,
				)

				if err := relayMeter.Start(ctx); err != nil {
					return fmt.Errorf("failed to start relay meter: %w", err)
				}
				defer func() { _ = relayMeter.Close() }()

				proxy.SetRelayMeter(relayMeter)
				logger.Info().
					Str("fail_behavior", string(failBehavior)).
					Msg("relay meter initialized and wired")
			} else {
				logger.Info().Msg("relay meter disabled in config")
			}

			// Initialize gRPC handler for gRPC and gRPC-Web requests
			proxy.InitGRPCHandler()
			// Initialize unified relay pipeline (validation + metering + signing + publishing)
			proxy.InitializeRelayPipeline()
		}
	}

	// Set supplier cache for checking supplier state before accepting relays
	proxy.SetSupplierCache(supplierCache)

	// Register backends for health checking (per RPC type)
	for serviceID, svc := range config.Services {
		for rpcType, backend := range svc.Backends {
			if backend.HealthCheck != nil && backend.HealthCheck.Enabled {
				backendID := fmt.Sprintf("%s:%s", serviceID, rpcType)
				healthChecker.RegisterBackend(backendID, backend.URL, backend.HealthCheck)
			}
		}
	}

	// Start health/readiness server
	if config.HealthCheck.Enabled {
		healthServer := startHealthServer(ctx, logger, config.HealthCheck.Addr, supplierCache)
		defer func() { _ = healthServer.Close() }()
		logger.Info().Str("addr", config.HealthCheck.Addr).Msg("health/readiness server started")
	}

	// Start components
	if err := healthChecker.Start(ctx); err != nil {
		return fmt.Errorf("failed to start health checker: %w", err)
	}

	if err := proxy.Start(ctx); err != nil {
		return fmt.Errorf("failed to start proxy: %w", err)
	}

	logger.Info().
		Str("listen_addr", config.ListenAddr).
		Int("num_services", len(config.Services)).
		Msg("HA Relayer started")

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info().Msg("shutdown signal received, stopping HA Relayer...")

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	_ = proxy.Close()
	_ = healthChecker.Close()

	_ = shutdownCtx // Used for graceful shutdown timing

	logger.Info().Msg("HA Relayer stopped")
	return nil
}

// startHealthServer starts a simple HTTP server for health and readiness checks.
func startHealthServer(ctx context.Context, logger logging.Logger, addr string, supplierCache *cache.SupplierCache) *http.Server {
	mux := http.NewServeMux()

	// /health - liveness probe (always returns OK if server is running)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// /ready - readiness probe (checks if supplier cache has data)
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		// Check if supplier cache has loaded any suppliers
		// This indicates the relayer has connected to Redis and miner has published supplier data
		if supplierCache == nil {
			http.Error(w, "supplier cache not initialized", http.StatusServiceUnavailable)
			return
		}

		// For now, always return ready after cache is initialized
		// TODO: Could check if cache has any suppliers loaded
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("READY"))
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error().Err(err).Msg("health server error")
		}
	}()

	// Shutdown on context cancellation
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	return server
}

// verifyRPCConnectivity checks HTTP RPC endpoint health via /status endpoint.
// This is a non-blocking health check - failure is logged but doesn't prevent startup.
func verifyRPCConnectivity(ctx context.Context, logger logging.Logger, rpcURL string) error {
	// Create HTTP client with short timeout for health check
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// Convert tcp:// scheme to http:// for HTTP client
	// CometBFT RPC URLs often use tcp:// prefix but serve HTTP
	httpURL := rpcURL
	if strings.HasPrefix(rpcURL, "tcp://") {
		httpURL = "http://" + strings.TrimPrefix(rpcURL, "tcp://")
	}

	// Query /status endpoint
	statusURL := strings.TrimSuffix(httpURL, "/") + "/status"
	req, err := http.NewRequestWithContext(ctx, "GET", statusURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	logger.Debug().
		Str("rpc_url", httpURL).
		Int("status_code", resp.StatusCode).
		Msg("RPC /status endpoint responded successfully")

	return nil
}

// verifyGRPCConnectivity checks gRPC endpoint health by querying shared params.
// This is a non-blocking health check - failure is logged but doesn't prevent startup.
func verifyGRPCConnectivity(ctx context.Context, logger logging.Logger, grpcURL string, queryClients *query.Clients) error {
	// Create context with short timeout for health check
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Try to query shared params as a connectivity test
	_, err := queryClients.Shared().GetParams(checkCtx)
	if err != nil {
		return fmt.Errorf("gRPC query failed: %w", err)
	}

	logger.Debug().
		Str("grpc_url", grpcURL).
		Msg("gRPC endpoint responded successfully to GetParams query")

	return nil
}
