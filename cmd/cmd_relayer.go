package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/cache"
	haclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/relayer"
	"github.com/pokt-network/pocket-relay-miner/rings"
	"github.com/pokt-network/pocket-relay-miner/transport"
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

	// Start observability server (metrics)
	if config.Metrics.Enabled {
		obsServer := observability.NewServer(logger, observability.ServerConfig{
			MetricsEnabled: config.Metrics.Enabled,
			MetricsAddr:    config.Metrics.Addr,
			PprofEnabled:   false,
			Registry:       observability.RelayerRegistry,
		})
		if err := obsServer.Start(ctx); err != nil {
			return fmt.Errorf("failed to start observability server: %w", err)
		}
		defer func() { _ = obsServer.Stop() }()
		logger.Info().Str("addr", config.Metrics.Addr).Msg("observability server started")
	}

	// Use Redis URL from config, allow flag override
	redisURL := config.Redis.URL
	if cmd.Flags().Changed(flagRedisURL) {
		redisURL, _ = cmd.Flags().GetString(flagRedisURL)
	}
	redisOpts, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("failed to parse Redis URL: %w", err)
	}
	redisClient := redis.NewClient(redisOpts)
	defer func() { _ = redisClient.Close() }()

	// Test Redis connection
	if pingErr := redisClient.Ping(ctx).Err(); pingErr != nil {
		return fmt.Errorf("failed to connect to Redis: %w", pingErr)
	}
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

	// Create query clients for fetching on-chain data (service compute units, etc.)
	// Determine TLS from URL - if port 443 or grpcs:// prefix, use TLS
	grpcURL := config.PocketNode.QueryNodeGRPCUrl
	useTLS := strings.HasPrefix(grpcURL, "grpcs://") ||
		strings.HasPrefix(grpcURL, "https://") ||
		strings.HasSuffix(grpcURL, ":443")
	// Strip scheme for gRPC endpoint
	grpcEndpoint := grpcURL
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "grpcs://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "grpc://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "https://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "http://")

	queryClients, err := query.NewQueryClients(
		logger,
		query.QueryClientConfig{
			GRPCEndpoint: grpcEndpoint,
			QueryTimeout: 30 * time.Second,
			UseTLS:       useTLS,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	defer func() { _ = queryClients.Close() }()
	logger.Info().Str("grpc_endpoint", grpcEndpoint).Bool("tls", useTLS).Msg("query clients initialized")

	// Create new entity caches (relayer only subscribes to pub/sub, doesn't refresh)
	// These caches provide L1/L2/L3 pattern with pub/sub invalidation

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

	logger.Info().Msg("entity caches started (pub/sub subscribers)")

	// Create block subscriber for monitoring new blocks via WebSocket subscription
	// Used for relay validation and relay meter
	rpcURL := config.PocketNode.QueryNodeRPCUrl
	rpcUseTLS := strings.HasPrefix(rpcURL, "https://") || strings.HasSuffix(rpcURL, ":443")
	blockSubscriber, err := haclient.NewBlockSubscriber(
		logger,
		haclient.BlockSubscriberConfig{
			RPCEndpoint: rpcURL,
			UseTLS:      rpcUseTLS,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create block subscriber: %w", err)
	}
	if err := blockSubscriber.Start(ctx); err != nil {
		return fmt.Errorf("failed to start block subscriber: %w", err)
	}
	defer func() { blockSubscriber.Close() }()
	logger.Info().Str("rpc_endpoint", rpcURL).Msg("block subscriber started (WebSocket)")

	// NOTE: Cache warmup is now handled by the miner's CacheOrchestrator.
	// The relayer's ApplicationCache.WarmupFromRedis() will populate L1 from L2 on startup.
	// Discovery of apps/services happens on the miner side when processing relays from Redis streams.

	// Create publisher for mined relays
	publisher := redistransport.NewStreamsPublisher(
		logger,
		redisClient,
		transport.PublisherConfig{
			StreamPrefix: config.Redis.StreamPrefix,
			MaxLen:       config.Redis.MaxStreamLen,
			ApproxMaxLen: true,
		},
	)

	// Create health checker
	healthChecker := relayer.NewHealthChecker(logger)

	// Create proxy server
	proxy, err := relayer.NewProxyServer(
		logger,
		config,
		healthChecker,
		publisher,
	)
	if err != nil {
		return fmt.Errorf("failed to create proxy server: %w", err)
	}

	// Poll block height updates and push to proxy
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer func() { ticker.Stop() }()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				block := blockSubscriber.LastBlock(ctx)
				proxy.SetBlockHeight(block.Height())
			}
		}
	}()
	logger.Info().Msg("proxy subscribed to block height updates")

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

	// Try keyring as additional source (can combine both)
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
				Str("operator_addresses", strings.Join(responseSigner.GetOperatorAddresses(), ", ")).
				Msg("response signer initialized")

			// Create RingClient for relay request signature verification
			// This is critical for security - it validates that relay requests
			// are properly signed by the application or a delegated gateway
			ringClient := rings.NewRingClient(
				logger,
				queryClients.Application(),
				queryClients.Account(),
				queryClients.Shared(),
			)

			// Create caches for full session validation
			cacheConfig := cache.CacheConfig{
				CachePrefix:            "ha:cache",
				PubSubPrefix:           "ha:events",
				TTLBlocks:              1,
				BlockTimeSeconds:       30, // CRITICAL: Must match actual block time (30s for this network, not 6s!)
				ExtraGracePeriodBlocks: config.GracePeriodExtraBlocks,
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
				GracePeriodExtraBlocks:   config.GracePeriodExtraBlocks,
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
				Int64("grace_period_extra_blocks", validatorConfig.GracePeriodExtraBlocks).
				Msg("full relay validator initialized with session validation")

			// Create RelayProcessor for proper relay mining with session metadata
			signerAdapter := relayer.NewResponseSignerAdapter(responseSigner)
			relayProcessor := relayer.NewRelayProcessor(
				logger,
				publisher,
				signerAdapter,
				ringClient, // Enable relay request signature verification
			)
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
					OverServicingEnabled:   config.RelayMeter.OverServicingEnabled,
					RedisKeyPrefix:         config.RelayMeter.RedisKeyPrefix,
					FailBehavior:           failBehavior,
					SessionCleanupInterval: config.RelayMeter.SessionCleanupInterval,
					ParamsCacheTTL:         config.RelayMeter.ParamsCacheTTL,
					AppStakeCacheTTL:       config.RelayMeter.AppStakeCacheTTL,
				}

				relayMeter := relayer.NewRelayMeter(
					logger,
					redisClient,
					queryClients.Application(),
					queryClients.Shared(),
					queryClients.Session(),
					blockSubscriber,
					relayMeterConfig,
				)

				if err := relayMeter.Start(ctx); err != nil {
					return fmt.Errorf("failed to start relay meter: %w", err)
				}
				defer func() { _ = relayMeter.Close() }()

				proxy.SetRelayMeter(relayMeter)
				logger.Info().
					Bool("over_servicing", config.RelayMeter.OverServicingEnabled).
					Str("fail_behavior", string(failBehavior)).
					Msg("relay meter initialized and wired")
			} else {
				logger.Info().Msg("relay meter disabled in config")
			}

			// Initialize gRPC handler for gRPC and gRPC-Web requests
			proxy.InitGRPCHandler()
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
