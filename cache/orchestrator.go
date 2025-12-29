package cache

import (
	"context"
	"fmt"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"runtime"
	"sync"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/puzpuzpuz/xsync/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// CacheOrchestrator coordinates all caches and manages parallel refresh on block updates.
//
// The orchestrator:
// - Monitors block events from BlockSubscriber (WebSocket-based, no polling)
// - Only refreshes caches when this instance is the GLOBAL leader
// - Refreshes all caches in parallel using a worker pool
// - Tracks discovered apps, services, and registered suppliers
// - Warms up caches from Redis on startup
type CacheOrchestrator struct {
	logger logging.Logger
	config CacheOrchestratorConfig

	// Global leader election (Redis-based)
	leaderElector *leader.GlobalLeaderElector

	// Block subscriber for refresh triggers (WebSocket-based)
	blockSubscriber BlockHeightSubscriber

	// Redis client for warmup operations
	redisClient *redisutil.Client

	// All entity caches
	sharedParamsCache   SingletonEntityCache[*sharedtypes.Params]
	sessionParamsCache  SingletonEntityCache[*sessiontypes.Params]
	proofParamsCache    SingletonEntityCache[*prooftypes.Params]
	supplierParamsCache SupplierParamCache // Supplier module params (min_stake, etc.)
	applicationCache    KeyedEntityCache[string, *apptypes.Application]
	serviceCache        KeyedEntityCache[string, *sharedtypes.Service]
	supplierCache       *SupplierCache // Uses SupplierState, not proto types
	sessionCache        SessionCache   // Existing implementation

	// Tracked entities - using xsync for lock-free performance
	// Apps and Services: dynamically discovered from relay traffic
	// Suppliers: registered from KeyManager when keyring/keyfile is loaded/updated
	knownApps      *xsync.Map[string, struct{}]
	knownServices  *xsync.Map[string, struct{}]
	knownSuppliers *xsync.Map[string, struct{}]

	// Pond subpool for parallel cache refresh (I/O-bound network queries)
	refreshSubpool pond.Pool

	// Block subscription management (leader-only)
	blockChan        <-chan BlockEvent
	blockCancelFn    context.CancelFunc
	blockWg          sync.WaitGroup
	lastRefreshBlock int64 // Last block height that triggered a full cache refresh

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// CacheOrchestratorConfig contains configuration for the CacheOrchestrator.
type CacheOrchestratorConfig struct {
	// KnownApplications is a list of application addresses to pre-discover at startup.
	// These apps will be fetched from the network and added to the cache during warmup.
	KnownApplications []string

	// RefreshIntervalBlocks controls how often caches are refreshed.
	// A value of 1 means refresh every block (default).
	// A value of 2 means refresh every 2nd block (50% reduction in gRPC calls).
	// This is useful for reducing load on slow blockchain nodes.
	// Params rarely change, so values of 2-4 are safe for most use cases.
	RefreshIntervalBlocks int64
}

// NewCacheOrchestrator creates a new cache orchestrator.
// The workerPool parameter is required for creating refresh subpool.
func NewCacheOrchestrator(
	logger logging.Logger,
	config CacheOrchestratorConfig,
	leaderElector *leader.GlobalLeaderElector,
	blockSubscriber BlockHeightSubscriber,
	redisClient *redisutil.Client,
	sharedParamsCache SingletonEntityCache[*sharedtypes.Params],
	sessionParamsCache SingletonEntityCache[*sessiontypes.Params],
	proofParamsCache SingletonEntityCache[*prooftypes.Params],
	supplierParamsCache SupplierParamCache,
	applicationCache KeyedEntityCache[string, *apptypes.Application],
	serviceCache KeyedEntityCache[string, *sharedtypes.Service],
	supplierCache *SupplierCache,
	sessionCache SessionCache,
	workerPool pond.Pool,
) *CacheOrchestrator {
	// Calculate refresh workers: 15% of master pool for I/O-bound cache refresh
	// Cache refresh is network I/O (chain queries, Redis ops)
	numCPU := runtime.NumCPU()
	masterPoolSize := numCPU * 8
	refreshWorkers := int(float64(masterPoolSize) * 0.15)
	if refreshWorkers < 4 {
		refreshWorkers = 4 // Minimum 4 workers
	}

	// Create refresh subpool from master pool (percentage-based)
	refreshSubpool := workerPool.NewSubpool(refreshWorkers)

	percentage := int(float64(refreshWorkers) / float64(masterPoolSize) * 100)

	logger.Info().
		Int("refresh_workers", refreshWorkers).
		Int("master_pool_size", masterPoolSize).
		Int("percentage", percentage).
		Int("num_cpu", numCPU).
		Msg("created cache refresh subpool with percentage-based allocation")

	return &CacheOrchestrator{
		logger:              logging.ForComponent(logger, logging.ComponentCacheOrchestrator),
		config:              config,
		leaderElector:       leaderElector,
		blockSubscriber:     blockSubscriber,
		redisClient:         redisClient,
		sharedParamsCache:   sharedParamsCache,
		sessionParamsCache:  sessionParamsCache,
		proofParamsCache:    proofParamsCache,
		supplierParamsCache: supplierParamsCache,
		applicationCache:    applicationCache,
		serviceCache:        serviceCache,
		supplierCache:       supplierCache,
		sessionCache:        sessionCache,
		knownApps:           xsync.NewMap[string, struct{}](),
		knownServices:       xsync.NewMap[string, struct{}](),
		knownSuppliers:      xsync.NewMap[string, struct{}](),
		refreshSubpool:      refreshSubpool,
	}
}

// Start begins the cache orchestrator and performs initial warmup.
// NOTE: Assumes all caches are already started by the caller (cmd_miner.go).
func (o *CacheOrchestrator) Start(ctx context.Context) error {
	o.ctx, o.cancelFn = context.WithCancel(ctx)

	// 1. Warmup L1 caches from Redis (L2)
	o.logger.Info().Msg("warming up caches from Redis...")
	if err := o.warmupCaches(o.ctx); err != nil {
		o.logger.Warn().Err(err).Msg("cache warmup encountered errors")
	}

	// 2. Register leader transition callbacks
	// When we become leader, start block subscription and refresh worker
	// When we lose leadership, stop block subscription and refresh worker
	o.leaderElector.OnElected(o.onBecameLeader)
	o.leaderElector.OnLost(o.onLostLeadership)

	// 3. If already leader, start block subscription immediately
	if o.leaderElector.IsLeader() {
		o.onBecameLeader(o.ctx)
	}

	o.logger.Info().Msg("cache orchestrator started")

	return nil
}

// Close gracefully shuts down the orchestrator and all caches.
func (o *CacheOrchestrator) Close() error {
	// Stop block subscription if still running (in case we're still leader)
	if o.blockCancelFn != nil {
		o.blockCancelFn()
	}
	o.blockWg.Wait()

	// Stop main orchestrator context
	if o.cancelFn != nil {
		o.cancelFn()
	}
	o.wg.Wait()

	// Stop refresh subpool gracefully (drains queued tasks)
	if o.refreshSubpool != nil {
		o.refreshSubpool.StopAndWait()
	}

	// Close all caches (with nil checks for partial initialization)
	if o.sharedParamsCache != nil {
		_ = o.sharedParamsCache.Close()
	}
	if o.sessionParamsCache != nil {
		_ = o.sessionParamsCache.Close()
	}
	if o.proofParamsCache != nil {
		_ = o.proofParamsCache.Close()
	}
	if o.applicationCache != nil {
		_ = o.applicationCache.Close()
	}
	if o.serviceCache != nil {
		_ = o.serviceCache.Close()
	}
	if o.supplierCache != nil {
		_ = o.supplierCache.Close()
	}
	if o.sessionCache != nil {
		_ = o.sessionCache.Close()
	}

	o.logger.Info().Msg("cache orchestrator stopped")

	return nil
}

// onBecameLeader is called when this instance becomes the global leader.
// Starts block subscription and refresh worker (leader-only operations).
func (o *CacheOrchestrator) onBecameLeader(ctx context.Context) {
	o.logger.Info().Msg("became leader, starting block subscription and cache refresh")

	// Create cancellable context for leader-only operations
	blockCtx, blockCancel := context.WithCancel(o.ctx)
	o.blockCancelFn = blockCancel

	// Start the BlockSubscriberAdapter (leader-only to prevent channel overflow)
	// On non-leader pods, the adapter doesn't start, so no events are produced
	if starter, ok := o.blockSubscriber.(interface{ Start(context.Context) error }); ok {
		if err := starter.Start(blockCtx); err != nil {
			o.logger.Error().Err(err).Msg("failed to start block subscriber adapter")
			return
		}
		o.logger.Info().Msg("block subscriber adapter started (leader)")
	}

	// Subscribe to block events (push pattern, not polling)
	o.blockChan = o.blockSubscriber.Subscribe(blockCtx)

	// Start refresh worker
	o.blockWg.Add(1)
	go logging.RecoverGoRoutine(o.logger, "cache_refresh_worker", func(ctx context.Context) {
		o.refreshWorker(ctx)
	})(blockCtx)

	// Update leader status metric
	cacheOrchestratorLeaderStatus.Set(1)
}

// onLostLeadership is called when this instance loses leadership.
// Stops block subscription and refresh worker.
func (o *CacheOrchestrator) onLostLeadership(ctx context.Context) {
	o.logger.Warn().Msg("lost leadership, stopping block subscription and cache refresh")

	// Cancel leader-only operations
	if o.blockCancelFn != nil {
		o.blockCancelFn()
	}

	// Wait for refresh worker to stop
	o.blockWg.Wait()

	// Close the BlockSubscriberAdapter (leader-only)
	// This prevents channel accumulation on non-leader pods
	if closer, ok := o.blockSubscriber.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			o.logger.Warn().Err(err).Msg("failed to close block subscriber adapter")
		} else {
			o.logger.Info().Msg("block subscriber adapter closed (lost leadership)")
		}
	}

	// Update leader status metric
	cacheOrchestratorLeaderStatus.Set(0)
}

// refreshWorker monitors BlockSubscriber events and triggers parallel refresh.
// This uses the subscription pattern (push-based via channel, not polling).
// IMPORTANT: This should ONLY run when this instance is the leader.
func (o *CacheOrchestrator) refreshWorker(ctx context.Context) {
	defer o.blockWg.Done()

	// Default to refreshing every block if not configured
	refreshInterval := o.config.RefreshIntervalBlocks
	if refreshInterval <= 0 {
		refreshInterval = 1
	}

	o.logger.Info().
		Int64("refresh_interval_blocks", refreshInterval).
		Msg("refresh worker started (leader mode)")

	for {
		select {
		case <-ctx.Done():
			o.logger.Info().Msg("refresh worker stopped")
			return

		case block, ok := <-o.blockChan:
			if !ok {
				// Channel closed, stop worker
				o.logger.Warn().Msg("block channel closed, stopping refresh worker")
				return
			}

			// Skip blocks based on refresh interval
			// Always refresh the first block and then every N blocks after that
			if o.lastRefreshBlock > 0 && (block.Height-o.lastRefreshBlock) < refreshInterval {
				o.logger.Debug().
					Int64("height", block.Height).
					Int64("last_refresh", o.lastRefreshBlock).
					Int64("interval", refreshInterval).
					Msg("skipping cache refresh (interval not reached)")
				continue
			}

			o.logger.Debug().
				Int64("height", block.Height).
				Msg("new block event received, refreshing caches")

			start := time.Now()
			if err := o.refreshAllCaches(ctx); err != nil {
				o.logger.Error().
					Err(err).
					Int64("height", block.Height).
					Msg("failed to refresh caches")
				cacheOrchestratorRefreshes.WithLabelValues(ResultFailure).Inc()
			} else {
				o.lastRefreshBlock = block.Height
				cacheOrchestratorRefreshes.WithLabelValues(ResultSuccess).Inc()
				cacheOrchestratorRefreshDuration.Observe(time.Since(start).Seconds())

				o.logger.Info().
					Int64("height", block.Height).
					Dur("duration", time.Since(start)).
					Msg("refreshed all caches (leader)")
			}
		}
	}
}

// refreshAllCaches refreshes all caches in parallel using pond subpool.
func (o *CacheOrchestrator) refreshAllCaches(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Define refresh tasks
	tasks := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"shared_params", o.refreshSharedParams},
		{"session_params", o.refreshSessionParams},
		{"proof_params", o.refreshProofParams},
		{"applications", o.refreshApplications},
		{"services", o.refreshServices},
		{"suppliers", o.refreshSuppliers},
		// Note: Sessions are not refreshed globally, they're cached on-demand
	}

	// Use pond Group for coordinated task submission and error collection
	group := o.refreshSubpool.NewGroup()

	// Submit all refresh tasks to the subpool
	for _, task := range tasks {
		// Capture for closure
		capturedTask := task
		group.SubmitErr(func() error {
			start := time.Now()

			if err := capturedTask.fn(ctx); err != nil {
				o.logger.Warn().
					Err(err).
					Str("cache", capturedTask.name).
					Msg("cache refresh failed")
				return fmt.Errorf("%s: %w", capturedTask.name, err)
			}

			o.logger.Debug().
				Str("cache", capturedTask.name).
				Dur("duration", time.Since(start)).
				Msg("cache refreshed")
			return nil
		})
	}

	// Wait for all tasks to complete and collect errors
	if err := group.Wait(); err != nil {
		return fmt.Errorf("cache refresh failed: %w", err)
	}

	return nil
}

// refreshSharedParams refreshes the shared params cache.
func (o *CacheOrchestrator) refreshSharedParams(ctx context.Context) error {
	return o.sharedParamsCache.Refresh(ctx)
}

// refreshSessionParams refreshes the session params cache.
func (o *CacheOrchestrator) refreshSessionParams(ctx context.Context) error {
	return o.sessionParamsCache.Refresh(ctx)
}

// refreshProofParams refreshes the proof params cache.
func (o *CacheOrchestrator) refreshProofParams(ctx context.Context) error {
	return o.proofParamsCache.Refresh(ctx)
}

// refreshApplications refreshes all known applications.
func (o *CacheOrchestrator) refreshApplications(ctx context.Context) error {
	// Collect known apps using xsync.MapOf.Range()
	var knownApps []string
	o.knownApps.Range(func(addr string, _ struct{}) bool {
		knownApps = append(knownApps, addr)
		return true // continue iteration
	})

	// Type assert to get RefreshEntity method
	type RefreshableCache interface {
		RefreshEntity(ctx context.Context, key string) error
	}

	refresher, ok := o.applicationCache.(RefreshableCache)
	if !ok {
		// Fallback to Get() if RefreshEntity not available
		o.logger.Warn().Msg("application cache does not support RefreshEntity, using Get()")
		for _, appAddr := range knownApps {
			if _, err := o.applicationCache.Get(ctx, appAddr); err != nil {
				o.logger.Warn().
					Err(err).
					Str(logging.FieldAppAddress, appAddr).
					Msg("failed to refresh application")
			}
		}
		return nil
	}

	// Force refresh from L3 (chain) for each known app IN PARALLEL using pond workers
	// This ensures fresh data in L2 (Redis) and publishes invalidation events
	group := o.refreshSubpool.NewGroup()
	for _, appAddr := range knownApps {
		// Capture for closure
		addr := appAddr
		group.SubmitErr(func() error {
			if err := refresher.RefreshEntity(ctx, addr); err != nil {
				// Check if it's a NotFound error (app was unstaked/deleted)
				if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
					// Expected: App was unstaked or deleted from chain
					// Log at Debug level to reduce noise
					o.logger.Debug().
						Str(logging.FieldAppAddress, addr).
						Msg("application not found on chain (likely unstaked)")
					// TODO: Consider removing from knownApps after N consecutive NotFound
					return nil // Don't fail the group for NotFound
				} else {
					// Unexpected error (network, timeout, etc.)
					o.logger.Warn().
						Err(err).
						Str(logging.FieldAppAddress, addr).
						Msg("failed to refresh application")
					return err
				}
			}
			return nil
		})
	}

	// Wait for all application refreshes to complete
	if err := group.Wait(); err != nil {
		o.logger.Warn().Err(err).Msg("some application refreshes failed")
		// Don't return error - continue with other caches
	}

	// Update Redis set with known apps (for warmup on restart)
	if err := o.updateKnownAppsSet(ctx, knownApps); err != nil {
		o.logger.Warn().Err(err).Msg("failed to update known apps set")
	}

	return nil
}

// refreshServices refreshes all known services.
func (o *CacheOrchestrator) refreshServices(ctx context.Context) error {
	// Collect known services using xsync.MapOf.Range()
	var knownServices []string
	o.knownServices.Range(func(serviceID string, _ struct{}) bool {
		knownServices = append(knownServices, serviceID)
		return true // continue iteration
	})

	// Type assert to get RefreshEntity method
	type RefreshableCache interface {
		RefreshEntity(ctx context.Context, key string) error
	}

	refresher, ok := o.serviceCache.(RefreshableCache)
	if !ok {
		// Fallback to Get() if RefreshEntity not available
		o.logger.Warn().Msg("service cache does not support RefreshEntity, using Get()")
		for _, serviceID := range knownServices {
			if _, err := o.serviceCache.Get(ctx, serviceID); err != nil {
				o.logger.Warn().
					Err(err).
					Str(logging.FieldServiceID, serviceID).
					Msg("failed to refresh service")
			}
		}
		return nil
	}

	// Force refresh from L3 (chain) for each known service IN PARALLEL using pond workers
	// This ensures fresh data in L2 (Redis) and publishes invalidation events
	group := o.refreshSubpool.NewGroup()
	for _, serviceID := range knownServices {
		// Capture for closure
		svcID := serviceID
		group.SubmitErr(func() error {
			if err := refresher.RefreshEntity(ctx, svcID); err != nil {
				// Check if it's a NotFound error (service was removed from chain)
				if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
					// Expected: Service was removed from chain
					// Log at Debug level to reduce noise
					o.logger.Debug().
						Str(logging.FieldServiceID, svcID).
						Msg("service not found on chain (likely removed)")
					// TODO: Consider removing from knownServices after N consecutive NotFound
					return nil // Don't fail the group for NotFound
				} else {
					// Unexpected error (network, timeout, etc.)
					o.logger.Warn().
						Err(err).
						Str(logging.FieldServiceID, svcID).
						Msg("failed to refresh service")
					return err
				}
			}
			return nil
		})
	}

	// Wait for all service refreshes to complete
	if err := group.Wait(); err != nil {
		o.logger.Warn().Err(err).Msg("some service refreshes failed")
		// Don't return error - continue with other caches
	}

	// Update Redis set with known services (for warmup on restart)
	if err := o.updateKnownServicesSet(ctx, knownServices); err != nil {
		o.logger.Warn().Err(err).Msg("failed to update known services set")
	}

	return nil
}

// refreshSuppliers refreshes all known suppliers and discovers their services.
func (o *CacheOrchestrator) refreshSuppliers(ctx context.Context) error {
	// Note: Suppliers are NOT discovered from relay traffic
	// They are registered via RegisterSupplier() when KeyManager loads them
	// from keyring/keyfile

	// Collect known suppliers
	var knownSuppliers []string
	o.knownSuppliers.Range(func(addr string, _ struct{}) bool {
		knownSuppliers = append(knownSuppliers, addr)
		return true
	})

	// For each supplier, get their state and discover their services IN PARALLEL
	// This is critical for operators with 2000+ suppliers
	group := o.refreshSubpool.NewGroup()
	for _, supplierAddr := range knownSuppliers {
		// Capture for closure
		addr := supplierAddr
		group.SubmitErr(func() error {
			state, err := o.supplierCache.GetSupplierState(ctx, addr)
			if err != nil {
				// Check if it's a NotFound error (supplier was unstaked/deleted)
				if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
					// Expected: Supplier was unstaked or deleted from chain
					// Log at Debug level to reduce noise
					o.logger.Debug().
						Str("supplier_address", addr).
						Msg("supplier not found on chain (likely unstaked)")
					// TODO: Consider removing from knownSuppliers after N consecutive NotFound
					return nil // Don't fail the group for NotFound
				}
				// Unexpected error (network, timeout, etc.)
				o.logger.Warn().
					Err(err).
					Str("supplier_address", addr).
					Msg("failed to get supplier state during refresh")
				return err
			}

			if state == nil {
				return nil
			}

			// Register services from this supplier
			for _, serviceID := range state.Services {
				if serviceID != "" {
					o.knownServices.Store(serviceID, struct{}{})
				}
			}
			return nil
		})
	}

	// Wait for all supplier refreshes to complete
	if err := group.Wait(); err != nil {
		o.logger.Warn().Err(err).Msg("some supplier refreshes failed")
		// Don't return error - continue with Redis update
	}

	// Update Redis set with known suppliers (for warmup on restart)
	if err := o.updateKnownSuppliersSet(ctx, knownSuppliers); err != nil {
		o.logger.Warn().Err(err).Msg("failed to update known suppliers set")
	}

	return nil
}

// warmupCaches populates L1 caches from Redis on startup.
func (o *CacheOrchestrator) warmupCaches(ctx context.Context) error {
	// Get known entities from Redis (stored by leader during refresh)
	knownApps := o.getKnownAppsFromRedis(ctx)
	knownServices := o.getKnownServicesFromRedis(ctx)
	knownSuppliers := o.getKnownSuppliersFromRedis(ctx)

	// Merge configured known applications with those from Redis
	// This allows operators to pre-configure apps for faster first-request performance
	if len(o.config.KnownApplications) > 0 {
		appSet := make(map[string]struct{}, len(knownApps)+len(o.config.KnownApplications))
		for _, addr := range knownApps {
			appSet[addr] = struct{}{}
		}
		for _, addr := range o.config.KnownApplications {
			if addr != "" {
				appSet[addr] = struct{}{}
			}
		}
		// Convert back to slice
		knownApps = make([]string, 0, len(appSet))
		for addr := range appSet {
			knownApps = append(knownApps, addr)
		}

		// Persist configured apps to Redis for future warmups
		if len(o.config.KnownApplications) > 0 {
			go func() {
				for _, addr := range o.config.KnownApplications {
					if addr != "" {
						_ = o.redisClient.SAdd(ctx, o.redisClient.KB().CacheKnownKey("applications"), addr).Err()
					}
				}
			}()
		}
	}

	// Populate known entity maps
	for _, addr := range knownApps {
		o.knownApps.Store(addr, struct{}{})
	}
	for _, serviceID := range knownServices {
		o.knownServices.Store(serviceID, struct{}{})
	}
	for _, supplierAddr := range knownSuppliers {
		o.knownSuppliers.Store(supplierAddr, struct{}{})
	}

	o.logger.Info().
		Int("apps", len(knownApps)).
		Int("services", len(knownServices)).
		Int("suppliers", len(knownSuppliers)).
		Msg("discovered known entities from Redis")

	// Warmup keyed caches in parallel using pond workers
	o.logger.Info().
		Int("apps", len(knownApps)).
		Int("services", len(knownServices)).
		Int("suppliers", len(knownSuppliers)).
		Msg("warming up caches from Redis using pond workers")

	// Create pond group for warmup tasks
	group := o.refreshSubpool.NewGroup()

	// Warmup applications in parallel
	if app, ok := o.applicationCache.(*applicationCache); ok {
		for _, appAddr := range knownApps {
			addr := appAddr
			group.SubmitErr(func() error {
				return app.warmupSingleApp(ctx, addr)
			})
		}
	}

	// Warmup services in parallel
	if svc, ok := o.serviceCache.(*serviceCache); ok {
		for _, serviceID := range knownServices {
			svcID := serviceID
			group.SubmitErr(func() error {
				return svc.warmupSingleService(ctx, svcID)
			})
		}
	}

	// Warmup suppliers - use existing method (already handles concurrency internally)
	group.SubmitErr(func() error {
		return o.supplierCache.WarmupFromRedis(ctx, knownSuppliers)
	})

	// Wait for all warmup tasks to complete
	if err := group.Wait(); err != nil {
		o.logger.Warn().Err(err).Msg("some cache warmup tasks failed")
		// Continue even if warmup fails - caches will be populated on demand
	}

	// Warmup singleton caches (params)
	if shared, ok := o.sharedParamsCache.(*sharedParamsCache); ok {
		_ = shared.WarmupFromRedis(ctx)
	}
	if session, ok := o.sessionParamsCache.(*sessionParamsCache); ok {
		_ = session.WarmupFromRedis(ctx)
	}
	if proof, ok := o.proofParamsCache.(*proofParamsCache); ok {
		_ = proof.WarmupFromRedis(ctx)
	}
	if supplier, ok := o.supplierParamsCache.(*RedisSupplierParamCache); ok {
		_ = supplier.WarmupFromRedis(ctx)
	}

	o.logger.Info().Msg("cache warmup complete")

	return nil
}

// getKnownAppsFromRedis retrieves the list of known app addresses from Redis.
func (o *CacheOrchestrator) getKnownAppsFromRedis(ctx context.Context) []string {
	members, err := o.redisClient.SMembers(ctx, o.redisClient.KB().CacheKnownKey("applications")).Result()
	if err != nil {
		o.logger.Warn().Err(err).Msg("failed to get known apps from Redis")
		return nil
	}
	return members
}

// getKnownServicesFromRedis retrieves the list of known service IDs from Redis.
func (o *CacheOrchestrator) getKnownServicesFromRedis(ctx context.Context) []string {
	members, err := o.redisClient.SMembers(ctx, o.redisClient.KB().CacheKnownKey("services")).Result()
	if err != nil {
		o.logger.Warn().Err(err).Msg("failed to get known services from Redis")
		return nil
	}
	return members
}

// getKnownSuppliersFromRedis retrieves the list of known supplier addresses from Redis.
func (o *CacheOrchestrator) getKnownSuppliersFromRedis(ctx context.Context) []string {
	members, err := o.redisClient.SMembers(ctx, o.redisClient.KB().CacheKnownKey("suppliers")).Result()
	if err != nil {
		o.logger.Warn().Err(err).Msg("failed to get known suppliers from Redis")
		return nil
	}
	return members
}

// updateKnownAppsSet updates the Redis set with known apps (for warmup on restart).
func (o *CacheOrchestrator) updateKnownAppsSet(ctx context.Context, knownApps []string) error {
	key := o.redisClient.KB().CacheKnownKey("applications")
	o.redisClient.Del(ctx, key)
	if len(knownApps) > 0 {
		return o.redisClient.SAdd(ctx, key, knownApps).Err()
	}
	return nil
}

// updateKnownServicesSet updates the Redis set with known services (for warmup on restart).
func (o *CacheOrchestrator) updateKnownServicesSet(ctx context.Context, knownServices []string) error {
	key := o.redisClient.KB().CacheKnownKey("services")
	o.redisClient.Del(ctx, key)
	if len(knownServices) > 0 {
		return o.redisClient.SAdd(ctx, key, knownServices).Err()
	}
	return nil
}

// updateKnownSuppliersSet updates the Redis set with known suppliers (for warmup on restart).
func (o *CacheOrchestrator) updateKnownSuppliersSet(ctx context.Context, knownSuppliers []string) error {
	key := o.redisClient.KB().CacheKnownKey("suppliers")
	o.redisClient.Del(ctx, key)
	if len(knownSuppliers) > 0 {
		return o.redisClient.SAdd(ctx, key, knownSuppliers).Err()
	}
	return nil
}

// RecordDiscoveredApp records an app discovered from relay traffic.
// This is called by the RelayProcessor when it sees a new app address.
func (o *CacheOrchestrator) RecordDiscoveredApp(appAddress string) {
	o.knownApps.Store(appAddress, struct{}{})
}

// RecordDiscoveredService records a service discovered from relay traffic.
// This is called by the RelayProcessor when it sees a new service ID.
func (o *CacheOrchestrator) RecordDiscoveredService(serviceID string) {
	o.knownServices.Store(serviceID, struct{}{})
}

// RegisterSupplier registers a supplier from KeyManager when keyring/keyfile is loaded.
// This is called by the KeyManager when supplier keys are loaded from keyring/keyfile.
func (o *CacheOrchestrator) RegisterSupplier(supplierAddress string) {
	o.knownSuppliers.Store(supplierAddress, struct{}{})

	o.logger.Debug().
		Str(logging.FieldSupplier, supplierAddress).
		Msg("registered supplier from KeyManager")

	// Update Redis set immediately
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Collect all known suppliers
	var knownSuppliers []string
	o.knownSuppliers.Range(func(addr string, _ struct{}) bool {
		knownSuppliers = append(knownSuppliers, addr)
		return true
	})

	if err := o.updateKnownSuppliersSet(ctx, knownSuppliers); err != nil {
		o.logger.Warn().Err(err).Msg("failed to update known suppliers set")
	}
}

// UnregisterSupplier removes a supplier from the known suppliers set.
// This is called by the KeyManager when supplier keys are removed.
func (o *CacheOrchestrator) UnregisterSupplier(supplierAddress string) {
	o.knownSuppliers.Delete(supplierAddress)

	o.logger.Debug().
		Str(logging.FieldSupplier, supplierAddress).
		Msg("unregistered supplier from KeyManager")

	// Update Redis set immediately
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Collect all known suppliers
	var knownSuppliers []string
	o.knownSuppliers.Range(func(addr string, _ struct{}) bool {
		knownSuppliers = append(knownSuppliers, addr)
		return true
	})

	if err := o.updateKnownSuppliersSet(ctx, knownSuppliers); err != nil {
		o.logger.Warn().Err(err).Msg("failed to update known suppliers set")
	}
}
