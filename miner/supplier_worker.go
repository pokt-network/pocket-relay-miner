package miner

import (
	"context"
	"crypto/tls"
	"fmt"
	stdhttp "net/http"
	"runtime"
	"sync"

	"github.com/alitto/pond/v2"
	"github.com/cometbft/cometbft/rpc/client/http"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/relayer"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/pocket-relay-miner/tx"

	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// SupplierWorkerConfig contains configuration for supplier processing.
// This runs on ALL miners (not just leader) when distributed claiming is enabled.
type SupplierWorkerConfig struct {
	// Core dependencies
	Logger      logging.Logger
	RedisClient *redistransport.Client
	KeyManager  keys.KeyManager
	Config      *Config

	// Blockchain connection config
	QueryNodeRPCUrl  string
	QueryNodeGRPCUrl string
	GRPCInsecure     bool
	ChainID          string
}

// SupplierWorker manages supplier processing for all miners.
// Unlike LeaderController, this runs on ALL miners when distributed claiming is enabled.
// Each miner claims a subset of suppliers and processes relays for those suppliers.
type SupplierWorker struct {
	logger logging.Logger
	config SupplierWorkerConfig

	// Resources created at startup
	queryClients            *query.Clients
	redisBlockSubscriber    *cache.RedisBlockSubscriber
	redisBlockClientAdapter *cache.RedisBlockClientAdapter
	supplierCache           *cache.SupplierCache
	txClient                *tx.TxClient
	proofChecker            *ProofRequirementChecker
	supplierManager         *SupplierManager
	supplierRegistry        *SupplierRegistry
	serviceFactorClient     *relayer.ServiceFactorClient
	masterPool              pond.Pool

	// Caches - these read from Redis L2 (populated by leader) and fall back to network
	sharedParamsCache  cache.SingletonEntityCache[*sharedtypes.Params]
	sessionParamsCache cache.SingletonEntityCache[*sessiontypes.Params]
	proofParamsCache   cache.SingletonEntityCache[*prooftypes.Params]

	// Cache orchestrator reference for discovered apps/services
	// Only used when worker is running inside leader
	cacheOrchestrator *cache.CacheOrchestrator

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	mu       sync.Mutex
	active   bool
}

// NewSupplierWorker creates a new supplier worker.
func NewSupplierWorker(config SupplierWorkerConfig) *SupplierWorker {
	return &SupplierWorker{
		logger: logging.ForComponent(config.Logger, "supplier_worker"),
		config: config,
	}
}

// SetCacheOrchestrator sets the cache orchestrator for tracking discovered apps/services.
// This is only called when the worker runs inside a leader.
func (w *SupplierWorker) SetCacheOrchestrator(orch *cache.CacheOrchestrator) {
	w.cacheOrchestrator = orch
}

// Start initializes and starts the supplier worker.
func (w *SupplierWorker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.active {
		return fmt.Errorf("supplier worker already active")
	}

	w.ctx, w.cancelFn = context.WithCancel(ctx)
	w.logger.Info().Msg("starting supplier worker")

	// Create master worker pool
	// Formula: max(cpu × cpu_multiplier, suppliers × workers_per_supplier) + overhead
	// This scales with both CPU count and supplier count for optimal parallelism
	numSuppliers := len(w.config.KeyManager.ListSuppliers())
	masterPoolSize := w.config.Config.GetMasterPoolSize(numSuppliers)
	w.masterPool = pond.NewPool(
		masterPoolSize,
		pond.WithQueueSize(pond.Unbounded),
		pond.WithNonBlocking(true),
	)
	w.logger.Info().
		Int("max_workers", masterPoolSize).
		Int("num_suppliers", numSuppliers).
		Int("num_cpu", runtime.NumCPU()).
		Msg("created master worker pool (auto-sized based on supplier count and CPU)")

	// Start worker pool metrics ticker for Prometheus monitoring
	StartWorkerPoolMetricsTicker(w.ctx, w.logger, w.masterPool, "supplier_worker", masterPoolSize)

	// Create query clients
	var err error
	w.queryClients, err = query.NewQueryClients(
		w.logger,
		query.ClientConfig{
			GRPCEndpoint: w.config.QueryNodeGRPCUrl,
			QueryTimeout: w.config.Config.GetQueryTimeout(),
			UseTLS:       !w.config.GRPCInsecure,
		},
	)
	if err != nil {
		w.cleanup()
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	w.logger.Info().
		Str("grpc_endpoint", w.config.QueryNodeGRPCUrl).
		Msg("query clients initialized")

	// Create RPC client for querying specific block heights (needed for proof generation)
	// This is CRITICAL - proof generation needs to query the exact block at a specific height
	// to get the canonical BlockID.Hash that matches what the validator stores
	var rpcClient *http.HTTP
	if w.config.QueryNodeRPCUrl != "" {
		if w.config.GRPCInsecure {
			rpcClient, err = http.New(w.config.QueryNodeRPCUrl, "/websocket")
		} else {
			tlsConfig := &tls.Config{
				MinVersion: tls.VersionTLS12,
			}
			httpClient := &stdhttp.Client{
				Transport: &stdhttp.Transport{
					TLSClientConfig: tlsConfig,
				},
			}
			rpcClient, err = http.NewWithClient(w.config.QueryNodeRPCUrl, "/websocket", httpClient)
		}
		if err != nil {
			w.cleanup()
			return fmt.Errorf("failed to create CometBFT RPC client: %w", err)
		}
		w.logger.Info().
			Str("rpc_endpoint", w.config.QueryNodeRPCUrl).
			Msg("created CometBFT RPC client for block queries")
	} else {
		w.logger.Warn().Msg("QueryNodeRPCUrl not configured - proof generation may fail")
	}

	// Create Redis block subscriber (subscribes to block events via Redis pub/sub)
	// This allows non-leader miners to receive block events published by the leader
	w.redisBlockSubscriber = cache.NewRedisBlockSubscriber(
		w.logger,
		w.config.RedisClient,
		nil, // No direct blockchain client - events come from leader via Redis
	)
	if err = w.redisBlockSubscriber.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start redis block subscriber: %w", err)
	}
	w.logger.Info().Msg("redis block subscriber started (receiving events from leader)")

	// Create Redis block client adapter to implement client.BlockClient interface
	// Pass the RPC client so it can query specific block heights for proof generation
	w.redisBlockClientAdapter = cache.NewRedisBlockClientAdapter(
		w.logger,
		w.redisBlockSubscriber,
		rpcClient, // For GetBlockAtHeight() - critical for proof generation
	)
	if err = w.redisBlockClientAdapter.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start redis block client adapter: %w", err)
	}
	w.logger.Info().Msg("redis block client adapter started")

	// Create caches (read from Redis L2 populated by leader, fall back to network query)
	blockTimeSeconds := w.config.Config.GetBlockTimeSeconds()

	w.sharedParamsCache = cache.NewSharedParamsCache(
		w.logger,
		w.config.RedisClient,
		w.queryClients.Shared(),
		blockTimeSeconds,
	)
	if err = w.sharedParamsCache.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start shared params cache: %w", err)
	}

	w.sessionParamsCache = cache.NewSessionParamsCache(
		w.logger,
		w.config.RedisClient,
		cache.NewSessionQueryClientAdapter(w.queryClients.Session()),
		w.queryClients.Shared(),
		blockTimeSeconds,
	)
	if err = w.sessionParamsCache.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start session params cache: %w", err)
	}

	w.proofParamsCache = cache.NewProofParamsCache(
		w.logger,
		w.config.RedisClient,
		cache.NewProofQueryClientAdapter(w.queryClients.Proof()),
		w.queryClients.Shared(),
		blockTimeSeconds,
	)
	if err = w.proofParamsCache.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start proof params cache: %w", err)
	}

	// Create supplier cache (for publishing supplier state)
	w.supplierCache = cache.NewSupplierCache(
		w.logger,
		w.config.RedisClient,
		cache.SupplierCacheConfig{
			KeyPrefix: w.config.RedisClient.KB().SupplierKeyPrefix(),
			FailOpen:  false,
		},
	)
	if err = w.supplierCache.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start supplier cache: %w", err)
	}

	// Get chain ID - required for transaction signing
	// ChainID comes from config (defaults to "pocket" if not explicitly set)
	chainID := w.config.ChainID
	w.logger.Info().Str("chain_id", chainID).Msg("using chain ID for transaction signing")

	// Create proof checker
	w.proofChecker = NewProofRequirementChecker(
		w.logger,
		w.queryClients.Proof(),
		w.queryClients.Shared(),
		w.queryClients.ServiceDifficulty(),
	)

	// Create tx client
	// Parse gas price from config
	gasPrice, err := cosmostypes.ParseDecCoin(w.config.Config.GetTxGasPrice())
	if err != nil {
		w.cleanup()
		return fmt.Errorf("failed to parse gas price: %w", err)
	}

	w.txClient, err = tx.NewTxClient(
		w.logger,
		w.config.KeyManager,
		tx.TxClientConfig{
			GRPCConn:      w.queryClients.GRPCConnection(),
			ChainID:       chainID,
			GasLimit:      w.config.Config.GetTxGasLimit(),
			GasPrice:      gasPrice,
			GasAdjustment: w.config.Config.GetTxGasAdjustment(),
			TimeoutBlocks: tx.DefaultTimeoutHeight,
		},
	)
	if err != nil {
		w.cleanup()
		return fmt.Errorf("failed to create tx client: %w", err)
	}
	w.logger.Info().Msg("transaction client initialized")

	// Create supplier registry
	w.supplierRegistry = NewSupplierRegistry(
		w.logger,
		w.config.RedisClient,
		SupplierRegistryConfig{
			KeyPrefix:    w.config.RedisClient.KB().SuppliersRegistryPrefix(),
			IndexKey:     w.config.RedisClient.KB().SuppliersRegistryIndexKey(),
			EventChannel: w.config.RedisClient.KB().SupplierUpdateChannel(),
		},
	)

	// Create service factor client (reads from Redis, published by leader)
	w.serviceFactorClient = relayer.NewServiceFactorClient(
		w.logger,
		w.config.RedisClient,
	)
	if err = w.serviceFactorClient.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start service factor client: %w", err)
	}

	// Create cached shared client wrapper
	cachedSharedClient := cache.NewCachedSharedQueryClient(w.sharedParamsCache, w.queryClients.Shared())

	// Create supplier manager with distributed claiming enabled
	w.supplierManager = NewSupplierManager(
		w.logger,
		w.config.KeyManager,
		w.supplierRegistry,
		SupplierManagerConfig{
			RedisClient:           w.config.RedisClient,
			ConsumerName:          w.config.Config.Redis.ConsumerName,
			SessionTTL:            w.config.Config.GetSessionTTL(), // Uses CacheTTL if not explicitly set
			CacheTTL:              w.config.Config.GetCacheTTL(),
			BatchSize:             w.config.Config.BatchSize,
			AckBatchSize:          w.config.Config.AckBatchSize,
			ClaimIdleTimeout:      w.config.Config.GetClaimIdleTimeout(),
			SupplierCache:         w.supplierCache,
			MinerID:               w.config.Config.Redis.ConsumerName,
			SupplierQueryClient:   w.queryClients.Supplier(),
			WorkerPool:            w.masterPool,
			TxClient:              w.txClient,
			BlockClient:           w.redisBlockClientAdapter, // Use Redis pub/sub for block events
			SharedClient:          cachedSharedClient,
			SessionClient:         w.queryClients.Session(),
			ProofChecker:          w.proofChecker,
			ServiceFactorProvider: newServiceFactorClientAdapter(ctx, w.serviceFactorClient),
			AppClient:             cache.NewApplicationQueryClientAdapter(w.queryClients.Application()),
			SessionLifecycleConfig: SessionLifecycleConfig{
				CheckInterval:            0, // Event-driven via Redis pub/sub
				MaxConcurrentTransitions: w.config.Config.GetSessionLifecycleMaxConcurrentTransitions(),
			},
			ClaimerConfig:         w.config.Config.GetSupplierClaimingConfig(),
			DisableClaimBatching:  w.config.Config.Transaction.DisableClaimBatching,
			DisableProofBatching:  w.config.Config.Transaction.DisableProofBatching,
			SubmissionTrackingTTL: w.config.Config.GetSubmissionTrackingTTL(),
			QueryWorkers:          w.config.Config.GetQueryWorkers(),
		},
	)

	// Set relay handler
	w.supplierManager.SetRelayHandler(w.handleRelay)

	// Start supplier manager
	if err = w.supplierManager.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start supplier manager: %w", err)
	}

	w.active = true
	w.logger.Info().Msg("supplier worker started - processing relays with distributed claiming")
	return nil
}

// handleRelay processes a relay message.
func (w *SupplierWorker) handleRelay(ctx context.Context, supplierAddr string, msg *transport.StreamMessage) error {
	// CRITICAL: Log that we're processing a relay
	w.logger.Debug().
		Str("supplier", supplierAddr).
		Str("session_id", msg.Message.SessionId).
		Str("service_id", msg.Message.ServiceId).
		Uint64("compute_units", msg.Message.ComputeUnitsPerRelay).
		Msg("handleRelay: processing relay message")

	state, ok := w.supplierManager.GetSupplierState(supplierAddr)
	if !ok {
		// Supplier state not found - this can happen during shutdown or if supplier was removed.
		// This is a permanent condition (retrying won't help), so ACK and discard the relay.
		w.logger.Warn().
			Str("supplier", supplierAddr).
			Str("session_id", msg.Message.SessionId).
			Msg("supplier state not found - discarding relay (supplier may have been removed)")
		RecordRelayRejected(supplierAddr, "supplier_not_found", msg.Message.ServiceId)
		return nil // ACK and discard
	}

	// Track discovered apps and services (if cache orchestrator is available)
	if w.cacheOrchestrator != nil {
		if msg.Message.ApplicationAddress != "" {
			w.cacheOrchestrator.RecordDiscoveredApp(msg.Message.ApplicationAddress)
		}
		if msg.Message.ServiceId != "" {
			w.cacheOrchestrator.RecordDiscoveredService(msg.Message.ServiceId)
		}
	}

	// Check session state BEFORE updating SMST
	if state.SessionStore != nil {
		snapshot, storeErr := state.SessionStore.Get(ctx, msg.Message.SessionId)
		if storeErr == nil && snapshot != nil {
			if snapshot.State.IsTerminal() {
				w.logger.Info().
					Str("session_id", msg.Message.SessionId).
					Str("supplier", supplierAddr).
					Str("session_state", string(snapshot.State)).
					Msg("LATE_RELAY: dropping relay - session already in terminal state")
				RecordRelayRejected(supplierAddr, "session_sealed", msg.Message.ServiceId)
				return nil
			}
		}
	}

	// Update SMST with relay bytes
	if err := state.SMSTManager.UpdateTree(
		ctx,
		msg.Message.SessionId,
		msg.Message.RelayHash,
		msg.Message.RelayBytes,
		msg.Message.ComputeUnitsPerRelay,
	); err != nil {
		// Check for permanent SMST errors (late relays, sealed/claimed sessions)
		if IsPermanentSMSTError(err) {
			w.logger.Info().
				Err(err).
				Str("session_id", msg.Message.SessionId).
				Str("supplier", supplierAddr).
				Msg("dropping relay - permanent SMST error (session sealed/claimed)")
			RecordRelayRejected(supplierAddr, "session_sealed", msg.Message.ServiceId)
			RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, msg.Message.SessionId, "session_sealed")
			return nil // ACK and discard - no point retrying
		}
		// Check for transient errors (Redis connection issues) - allow retry
		if IsRetryableError(err) {
			RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, msg.Message.SessionId, "transient_error")
			return fmt.Errorf("transient SMST error (will retry): %w", err)
		}
		// Unknown/unexpected errors - log and discard (don't retry forever)
		w.logger.Warn().
			Err(err).
			Str("session_id", msg.Message.SessionId).
			Str("supplier", supplierAddr).
			Msg("unexpected SMST error - discarding relay")
		RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, msg.Message.SessionId, "unexpected_error")
		return nil // ACK and discard - unknown errors shouldn't block processing
	}

	// MEMORY OPTIMIZATION: Clear RelayBytes and RelayHash after SMST update
	// The SMST has copied the data to Redis - these fields are no longer needed.
	// This allows GC to reclaim the memory early instead of holding until message is ACK'd.
	msg.Message.RelayBytes = nil
	msg.Message.RelayHash = nil

	// Track relay successfully added to SMST
	RecordRelayAddedToSMST(supplierAddr, msg.Message.ServiceId, msg.Message.SessionId)

	// Track relay in session coordinator
	if err := state.SessionCoordinator.OnRelayProcessed(
		ctx,
		msg.Message.SessionId,
		msg.Message.ComputeUnitsPerRelay,
		msg.Message.SupplierOperatorAddress,
		msg.Message.ServiceId,
		msg.Message.ApplicationAddress,
		msg.Message.SessionStartHeight,
		msg.Message.SessionEndHeight,
	); err != nil {
		return fmt.Errorf("failed to update session coordinator: %w", err)
	}

	return nil
}

// Close shuts down the supplier worker.
func (w *SupplierWorker) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.active {
		return nil
	}

	w.logger.Info().Msg("stopping supplier worker")

	if w.cancelFn != nil {
		w.cancelFn()
	}

	w.cleanup()
	w.active = false
	w.logger.Info().Msg("supplier worker stopped")
	return nil
}

// cleanup closes all resources.
func (w *SupplierWorker) cleanup() {
	if w.supplierManager != nil {
		if err := w.supplierManager.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close supplier manager")
		}
		w.supplierManager = nil
	}

	if w.serviceFactorClient != nil {
		if err := w.serviceFactorClient.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close service factor client")
		}
		w.serviceFactorClient = nil
	}

	if w.txClient != nil {
		if err := w.txClient.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close tx client")
		}
		w.txClient = nil
	}

	if w.supplierCache != nil {
		if err := w.supplierCache.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close supplier cache")
		}
		w.supplierCache = nil
	}

	if w.proofParamsCache != nil {
		if err := w.proofParamsCache.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close proof params cache")
		}
		w.proofParamsCache = nil
	}

	if w.sessionParamsCache != nil {
		if err := w.sessionParamsCache.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close session params cache")
		}
		w.sessionParamsCache = nil
	}

	if w.sharedParamsCache != nil {
		if err := w.sharedParamsCache.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close shared params cache")
		}
		w.sharedParamsCache = nil
	}

	if w.redisBlockClientAdapter != nil {
		w.redisBlockClientAdapter.Close() // Close() doesn't return error
		w.redisBlockClientAdapter = nil
	}

	if w.redisBlockSubscriber != nil {
		err := w.redisBlockSubscriber.Close()
		if err != nil {
			w.logger.Error().Err(err).Msg("failed to close redis block subscriber")
		} else {
			w.redisBlockSubscriber = nil
		}
	}

	if w.queryClients != nil {
		if err := w.queryClients.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close query clients")
		}
		w.queryClients = nil
	}

	if w.masterPool != nil {
		w.masterPool.Stop()
		w.masterPool = nil
	}
}

// GetSupplierManager returns the supplier manager for external access.
func (w *SupplierWorker) GetSupplierManager() *SupplierManager {
	return w.supplierManager
}

// serviceFactorClientAdapter wraps the relayer's ServiceFactorClient to implement
// the miner's ServiceFactorProvider interface (which doesn't take a context).
type serviceFactorClientAdapter struct {
	client *relayer.ServiceFactorClient
	ctx    context.Context
}

func newServiceFactorClientAdapter(ctx context.Context, client *relayer.ServiceFactorClient) *serviceFactorClientAdapter {
	return &serviceFactorClientAdapter{
		client: client,
		ctx:    ctx,
	}
}

// GetServiceFactor implements miner.ServiceFactorProvider.
func (a *serviceFactorClientAdapter) GetServiceFactor(serviceID string) (float64, bool) {
	return a.client.GetServiceFactor(a.ctx, serviceID)
}
