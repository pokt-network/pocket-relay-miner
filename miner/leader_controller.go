package miner

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/alitto/pond/v2"

	"github.com/pokt-network/pocket-relay-miner/cache"
	haclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// LeaderControllerConfig contains configuration for leader-only resources.
type LeaderControllerConfig struct {
	// Core dependencies (lightweight, created before election)
	Logger      logging.Logger
	RedisClient *redistransport.Client // Wrapped client with KeyBuilder
	KeyManager  keys.KeyManager
	Config      *Config

	// Leader election
	GlobalLeader *leader.GlobalLeaderElector

	// Blockchain connection config
	QueryNodeRPCUrl  string
	QueryNodeGRPCUrl string
	GRPCInsecure     bool
	ChainID          string
}

// LeaderController manages all leader-only resources.
// It creates expensive resources (query clients, caches, monitors) only when elected leader
// and cleans them up when losing leadership. This prevents resource waste on standby instances.
// Note: Supplier processing is handled by SupplierWorker (distributed across all replicas), not LeaderController.
type LeaderController struct {
	logger logging.Logger
	config LeaderControllerConfig

	// Heavy resources (only created when leader)
	queryClients          *query.Clients
	blockSubscriber       *haclient.BlockSubscriber
	redisBlockSubscriber  *cache.RedisBlockSubscriber
	blockPublisher        *cache.BlockPublisher
	sharedParamsCache     cache.SingletonEntityCache[*sharedtypes.Params]
	sessionParamsCache    cache.SingletonEntityCache[*sessiontypes.Params]
	proofParamsCache      cache.SingletonEntityCache[*prooftypes.Params]
	supplierParamsCache   *cache.RedisSupplierParamCache
	applicationCache      cache.KeyedEntityCache[string, *apptypes.Application]
	serviceCache          cache.KeyedEntityCache[string, *sharedtypes.Service]
	supplierCache         *cache.SupplierCache
	cacheOrchestrator     *cache.CacheOrchestrator
	proofChecker          *ProofRequirementChecker
	balanceMonitor        *BalanceMonitor
	blockHealthMonitor    *BlockHealthMonitor
	supplierRegistry      *SupplierRegistry
	serviceFactorRegistry *ServiceFactorRegistry
	settlementMonitor     *SettlementMonitor
	masterPool            pond.Pool

	// Lifecycle
	mu     sync.Mutex
	active bool
}

// NewLeaderController creates a new leader controller.
func NewLeaderController(config LeaderControllerConfig) *LeaderController {
	return &LeaderController{
		logger: logging.ForComponent(config.Logger, logging.ComponentLeaderController),
		config: config,
	}
}

// Start creates all leader-only resources and starts them.
// This is called when the instance becomes leader.
func (c *LeaderController) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.active {
		return fmt.Errorf("leader controller already active")
	}

	c.logger.Info().Msg("starting leader controller - creating all resources")

	// Create a master worker pool for controlled concurrency
	// Formula: max(cpu × cpu_multiplier, suppliers × workers_per_supplier) + overhead
	// This scales with both CPU count and supplier count for optimal parallelism
	numSuppliers := len(c.config.KeyManager.ListSuppliers())
	masterPoolSize := c.config.Config.GetMasterPoolSize(numSuppliers)
	c.masterPool = pond.NewPool(
		masterPoolSize,
		pond.WithQueueSize(pond.Unbounded),
		pond.WithNonBlocking(true),
	)
	c.logger.Info().
		Int("max_workers", masterPoolSize).
		Int("num_suppliers", numSuppliers).
		Int("num_cpu", runtime.NumCPU()).
		Msg("created master worker pool (auto-sized based on supplier count and CPU)")

	// Start worker pool metrics ticker for Prometheus monitoring
	StartWorkerPoolMetricsTicker(ctx, c.logger, c.masterPool, "leader_controller", masterPoolSize)

	// Create query clients
	var err error
	c.queryClients, err = query.NewQueryClients(
		c.logger,
		query.ClientConfig{
			GRPCEndpoint: c.config.QueryNodeGRPCUrl,
			QueryTimeout: c.config.Config.GetQueryTimeout(),
			UseTLS:       !c.config.GRPCInsecure,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	c.logger.Info().
		Str("grpc_endpoint", c.config.QueryNodeGRPCUrl).
		Dur("query_timeout", c.config.Config.GetQueryTimeout()).
		Msg("query clients initialized")

	// Create a block subscriber
	c.blockSubscriber, err = haclient.NewBlockSubscriber(
		c.logger,
		haclient.BlockSubscriberConfig{
			RPCEndpoint: c.config.QueryNodeRPCUrl,
			UseTLS:      !c.config.GRPCInsecure,
		},
	)
	if err != nil {
		c.cleanup()
		return fmt.Errorf("failed to create block subscriber: %w", err)
	}
	if err = c.blockSubscriber.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start block subscriber: %w", err)
	}
	c.logger.Info().Msg("block subscriber started (WebSocket)")

	// Create a Redis block subscriber for publishing to relayers
	// Uses KeyBuilder for namespace-aware channel names
	c.redisBlockSubscriber = cache.NewRedisBlockSubscriber(
		c.logger,
		c.config.RedisClient,
		c.blockSubscriber,
	)
	if err = c.redisBlockSubscriber.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start redis block subscriber: %w", err)
	}
	c.logger.Info().Msg("redis block subscriber started for publishing to Redis")

	// Get block time
	blockTimeSeconds := c.config.Config.GetBlockTimeSeconds()

	// Create caches
	c.sessionParamsCache = cache.NewSessionParamsCache(
		c.logger,
		c.config.RedisClient,
		cache.NewSessionQueryClientAdapter(c.queryClients.Session()),
		c.queryClients.Shared(),
		blockTimeSeconds,
	)
	if err := c.sessionParamsCache.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start session params cache: %w", err)
	}

	c.sharedParamsCache = cache.NewSharedParamsCache(
		c.logger,
		c.config.RedisClient,
		c.queryClients.Shared(),
		blockTimeSeconds,
	)
	if err = c.sharedParamsCache.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start shared params cache: %w", err)
	}

	c.proofParamsCache = cache.NewProofParamsCache(
		c.logger,
		c.config.RedisClient,
		cache.NewProofQueryClientAdapter(c.queryClients.Proof()),
		c.queryClients.Shared(),
		blockTimeSeconds,
	)
	if err := c.proofParamsCache.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start proof params cache: %w", err)
	}

	c.supplierParamsCache = cache.NewRedisSupplierParamCache(
		c.logger,
		c.config.RedisClient,
		c.queryClients.Supplier(),
		cache.CacheConfig{
			CachePrefix:      c.config.RedisClient.KB().CachePrefix(),
			TTLBlocks:        100,
			BlockTimeSeconds: blockTimeSeconds,
			LockTimeout:      5,
			PubSubPrefix:     c.config.RedisClient.KB().EventsCachePrefix(),
		},
	)
	if err := c.supplierParamsCache.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start supplier params cache: %w", err)
	}

	c.applicationCache = cache.NewApplicationCache(
		c.logger,
		c.config.RedisClient,
		cache.NewApplicationQueryClientAdapter(c.queryClients.Application()),
	)
	if err := c.applicationCache.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start application cache: %w", err)
	}

	c.serviceCache = cache.NewServiceCache(
		c.logger,
		c.config.RedisClient,
		cache.NewServiceQueryClientAdapter(c.queryClients.Service()),
	)
	if err := c.serviceCache.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start service cache: %w", err)
	}

	// Create supplier cache
	c.supplierCache = cache.NewSupplierCache(
		c.logger,
		c.config.RedisClient,
		cache.SupplierCacheConfig{
			KeyPrefix: c.config.RedisClient.KB().SupplierKeyPrefix(),
			FailOpen:  false,
		},
	)
	if err := c.supplierCache.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start supplier cache: %w", err)
	}
	c.logger.Info().Msg("supplier cache initialized for state publishing")

	// Create block subscriber adapter for orchestrator
	blockSubscriberAdapter := cache.NewBlockSubscriberAdapter(
		c.logger,
		c.blockSubscriber,
	)

	// Create cache orchestrator
	c.cacheOrchestrator = cache.NewCacheOrchestrator(
		c.logger,
		cache.CacheOrchestratorConfig{
			KnownApplications:     c.config.Config.KnownApplications,
			RefreshIntervalBlocks: 4, // Refresh every 4 blocks to reduce gRPC load (params/apps/services rarely change)
		},
		c.config.GlobalLeader,
		blockSubscriberAdapter,
		c.config.RedisClient,
		c.sharedParamsCache,
		c.sessionParamsCache,
		c.proofParamsCache,
		c.supplierParamsCache,
		c.applicationCache,
		c.serviceCache,
		c.supplierCache,
		nil, // session cache placeholder
		c.masterPool,
	)
	if err := c.cacheOrchestrator.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start cache orchestrator: %w", err)
	}
	c.logger.Info().Msg("cache orchestrator started with pond workers")

	// Create a block publisher
	c.blockPublisher = cache.NewBlockPublisher(
		c.logger,
		c.blockSubscriber,
		c.redisBlockSubscriber,
	)
	if err = c.blockPublisher.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start block publisher: %w", err)
	}
	c.logger.Info().Msg("block publisher started")

	// Fetch chain ID of the network using the provided RPC.
	chainID := c.config.ChainID
	if chainID == "" {
		chainID, err = c.blockSubscriber.GetChainID(ctx)
		if err != nil {
			c.cleanup()
			return fmt.Errorf("failed to get chain ID from node: %w", err)
		}
		c.logger.Info().Str("chain_id", chainID).Msg("fetched chain ID from node")
	}

	// Create a proof checker
	c.proofChecker = NewProofRequirementChecker(
		c.logger,
		c.queryClients.Proof(),
		c.queryClients.Shared(),
		c.queryClients.Service(),
	)
	c.logger.Info().Msg("proof requirement checker initialized")

	// Create supplier registry
	c.supplierRegistry = NewSupplierRegistry(
		c.logger,
		c.config.RedisClient,
		SupplierRegistryConfig{
			KeyPrefix:    c.config.RedisClient.KB().SuppliersRegistryPrefix(),
			IndexKey:     c.config.RedisClient.KB().SuppliersRegistryIndexKey(),
			EventChannel: c.config.RedisClient.KB().SupplierUpdateChannel(),
		},
	)

	// Create and publish service factor registry
	// This publishes serviceFactor config to Redis for relayers to consume
	c.serviceFactorRegistry = NewServiceFactorRegistry(
		c.logger,
		c.config.RedisClient,
		c.config.RedisClient.KB(),
		ServiceFactorRegistryConfig{
			DefaultServiceFactor: c.config.Config.DefaultServiceFactor,
			ServiceFactors:       c.config.Config.ServiceFactors,
			CacheTTL:             c.config.Config.GetCacheTTL(),
		},
	)
	if err = c.serviceFactorRegistry.PublishServiceFactors(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to publish service factors: %w", err)
	}
	c.logger.Info().
		Float64("default_service_factor", c.config.Config.DefaultServiceFactor).
		Int("per_service_count", len(c.config.Config.ServiceFactors)).
		Msg("service factor registry published")

	// Create cached shared query client (used by SupplierWorker)
	_ = cache.NewCachedSharedQueryClient(c.sharedParamsCache, c.queryClients.Shared())

	// NOTE: Supplier processing is handled by SupplierWorker (distributed across all replicas).
	// LeaderController only manages shared caches, block publishing, and monitoring.

	// Start block health monitor if enabled
	if c.config.Config.BlockHealthMonitor.Enabled {
		c.blockHealthMonitor = NewBlockHealthMonitor(
			c.logger,
			c.blockSubscriber,
			c.config.GlobalLeader,
			BlockHealthMonitorConfig{
				BlockTimeSeconds:  blockTimeSeconds,
				SlownessThreshold: c.config.Config.GetBlockHealthSlownessThreshold(),
			},
		)
		if err := c.blockHealthMonitor.Start(ctx); err != nil {
			c.cleanup()
			return fmt.Errorf("failed to start block health monitor: %w", err)
		}
		c.logger.Info().Msg("block health monitor started (leader-only)")
	}

	// Start balance monitor if enabled
	if c.config.Config.GetBalanceMonitorEnabled() || c.config.Config.GetBalanceMonitorThreshold() > 0 {
		c.balanceMonitor = NewBalanceMonitor(
			c.logger,
			BalanceMonitorConfig{
				CheckInterval:               c.config.Config.GetBalanceMonitorCheckInterval(),
				BalanceThresholdUpokt:       c.config.Config.GetBalanceMonitorThreshold(),
				StakeWarningProofThreshold:  c.config.Config.GetBalanceMonitorStakeWarningProofThreshold(),
				StakeCriticalProofThreshold: c.config.Config.GetBalanceMonitorStakeCriticalProofThreshold(),
			},
			c.queryClients.Bank(),
			c.queryClients.Supplier(),
			c.supplierParamsCache,
			c.proofParamsCache,
			c.supplierRegistry,
			c.config.GlobalLeader,
		)
		if err := c.balanceMonitor.Start(ctx); err != nil {
			c.cleanup()
			return fmt.Errorf("failed to start balance monitor: %w", err)
		}
		c.logger.Info().Msg("balance monitor started")
	}

	// Start settlement monitor to track on-chain claim settlements
	// Get supplier addresses to monitor
	suppliersMap, err := c.supplierRegistry.GetAllSuppliers(ctx)
	if err != nil {
		c.cleanup()
		return fmt.Errorf("failed to get suppliers for settlement monitor: %w", err)
	}
	supplierAddresses := make([]string, 0, len(suppliersMap))
	for addr := range suppliersMap {
		supplierAddresses = append(supplierAddresses, addr)
	}

	c.settlementMonitor = NewSettlementMonitor(
		c.logger,
		c.blockSubscriber,
		c.blockSubscriber.GetRPCClient(),
		supplierAddresses,
		c.masterPool,
		nil, // SessionStore not available in LeaderController (settlement metadata won't be updated)
		c.config.Config.GetSettlementWorkers(),
	)
	if err := c.settlementMonitor.Start(ctx); err != nil {
		c.cleanup()
		return fmt.Errorf("failed to start settlement monitor: %w", err)
	}
	c.logger.Info().
		Int("suppliers", len(supplierAddresses)).
		Msg("settlement monitor started (tracking on-chain claim settlements)")

	c.active = true
	c.logger.Info().Msg("leader controller started - all resources active")
	return nil
}

// Close shuts down all leader-only resources.
// This is called when the instance loses leadership.
func (c *LeaderController) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.active {
		return nil
	}

	c.logger.Info().Msg("stopping leader controller - cleaning up all resources")
	c.cleanup()
	c.active = false
	c.logger.Info().Msg("leader controller stopped")
	return nil
}

// cleanup closes all resources (called during Start errors or Close).
// Must be called with c.mu held.
func (c *LeaderController) cleanup() {
	// Close in reverse order of creation
	if c.settlementMonitor != nil {
		c.settlementMonitor.Close()
		c.settlementMonitor = nil
	}

	if c.balanceMonitor != nil {
		if err := c.balanceMonitor.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close balance monitor")
		}
		c.balanceMonitor = nil
	}

	if c.blockHealthMonitor != nil {
		if err := c.blockHealthMonitor.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close block health monitor")
		}
		c.blockHealthMonitor = nil
	}

	// ServiceFactorRegistry doesn't have a Close method - it just holds config
	// Keys will expire based on Redis TTL or stay until overwritten
	c.serviceFactorRegistry = nil

	if c.blockPublisher != nil {
		if err := c.blockPublisher.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close block publisher")
		}
		c.blockPublisher = nil
	}

	if c.cacheOrchestrator != nil {
		if err := c.cacheOrchestrator.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close cache orchestrator")
		}
		c.cacheOrchestrator = nil
	}

	if c.supplierCache != nil {
		if err := c.supplierCache.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close supplier cache")
		}
		c.supplierCache = nil
	}

	if c.serviceCache != nil {
		if err := c.serviceCache.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close service cache")
		}
		c.serviceCache = nil
	}

	if c.applicationCache != nil {
		if err := c.applicationCache.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close application cache")
		}
		c.applicationCache = nil
	}

	if c.supplierParamsCache != nil {
		if err := c.supplierParamsCache.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close supplier params cache")
		}
		c.supplierParamsCache = nil
	}

	if c.proofParamsCache != nil {
		if err := c.proofParamsCache.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close proof params cache")
		}
		c.proofParamsCache = nil
	}

	if c.sharedParamsCache != nil {
		if err := c.sharedParamsCache.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close shared params cache")
		}
		c.sharedParamsCache = nil
	}

	if c.sessionParamsCache != nil {
		if err := c.sessionParamsCache.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close session params cache")
		}
		c.sessionParamsCache = nil
	}

	if c.redisBlockSubscriber != nil {
		if err := c.redisBlockSubscriber.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close redis block subscriber")
		}
		c.redisBlockSubscriber = nil
	}

	if c.blockSubscriber != nil {
		c.blockSubscriber.Close()
		c.blockSubscriber = nil
	}

	if c.queryClients != nil {
		if err := c.queryClients.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close query clients")
		}
		c.queryClients = nil
	}

	if c.masterPool != nil {
		c.masterPool.StopAndWait()
		c.masterPool = nil
	}

	c.logger.Info().Msg("all resources cleaned up")
}
