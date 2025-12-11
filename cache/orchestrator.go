package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"

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
	redisClient redis.UniversalClient

	// All entity caches
	sharedParamsCache  SingletonEntityCache[*sharedtypes.Params]
	sessionParamsCache SingletonEntityCache[*sessiontypes.Params]
	proofParamsCache   SingletonEntityCache[*prooftypes.Params]
	applicationCache   KeyedEntityCache[string, *apptypes.Application]
	serviceCache       KeyedEntityCache[string, *sharedtypes.Service]
	supplierCache      *SupplierCache // Uses SupplierState, not proto types
	sessionCache       SessionCache   // Existing implementation

	// Tracked entities - using xsync for lock-free performance
	// Apps and Services: dynamically discovered from relay traffic
	// Suppliers: registered from KeyManager when keyring/keyfile is loaded/updated
	knownApps      *xsync.Map[string, struct{}]
	knownServices  *xsync.Map[string, struct{}]
	knownSuppliers *xsync.Map[string, struct{}]

	// Block subscription management (leader-only)
	blockChan     <-chan BlockEvent
	blockCancelFn context.CancelFunc
	blockWg       sync.WaitGroup

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// CacheOrchestratorConfig contains configuration for the CacheOrchestrator.
type CacheOrchestratorConfig struct {
	// RefreshWorkers is the number of concurrent workers for parallel refresh.
	// Default: 6
	RefreshWorkers int

	// KnownApplications is a list of application addresses to pre-discover at startup.
	// These apps will be fetched from the network and added to the cache during warmup.
	KnownApplications []string
}

// NewCacheOrchestrator creates a new cache orchestrator.
func NewCacheOrchestrator(
	logger logging.Logger,
	config CacheOrchestratorConfig,
	leaderElector *leader.GlobalLeaderElector,
	blockSubscriber BlockHeightSubscriber,
	redisClient redis.UniversalClient,
	sharedParamsCache SingletonEntityCache[*sharedtypes.Params],
	sessionParamsCache SingletonEntityCache[*sessiontypes.Params],
	proofParamsCache SingletonEntityCache[*prooftypes.Params],
	applicationCache KeyedEntityCache[string, *apptypes.Application],
	serviceCache KeyedEntityCache[string, *sharedtypes.Service],
	supplierCache *SupplierCache,
	sessionCache SessionCache,
) *CacheOrchestrator {
	// Default config values
	if config.RefreshWorkers == 0 {
		config.RefreshWorkers = 6
	}

	return &CacheOrchestrator{
		logger:             logging.ForComponent(logger, logging.ComponentCacheOrchestrator),
		config:             config,
		leaderElector:      leaderElector,
		blockSubscriber:    blockSubscriber,
		redisClient:        redisClient,
		sharedParamsCache:  sharedParamsCache,
		sessionParamsCache: sessionParamsCache,
		proofParamsCache:   proofParamsCache,
		applicationCache:   applicationCache,
		serviceCache:       serviceCache,
		supplierCache:      supplierCache,
		sessionCache:       sessionCache,
		knownApps:          xsync.NewMap[string, struct{}](),
		knownServices:      xsync.NewMap[string, struct{}](),
		knownSuppliers:     xsync.NewMap[string, struct{}](),
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

	o.logger.Info().
		Int("refresh_workers", o.config.RefreshWorkers).
		Msg("cache orchestrator started")

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

	// Update leader status metric
	cacheOrchestratorLeaderStatus.Set(0)
}

// refreshWorker monitors BlockSubscriber events and triggers parallel refresh.
// This uses the subscription pattern (push-based via channel, not polling).
// IMPORTANT: This should ONLY run when this instance is the leader.
func (o *CacheOrchestrator) refreshWorker(ctx context.Context) {
	defer o.blockWg.Done()

	o.logger.Info().Msg("refresh worker started (leader mode)")

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

// refreshAllCaches refreshes all caches in parallel using worker pool.
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

	// Create worker pool
	taskCh := make(chan int, len(tasks))
	errCh := make(chan error, len(tasks))
	var wg sync.WaitGroup

	// Spawn workers
	for i := 0; i < o.config.RefreshWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for taskIdx := range taskCh {
				task := tasks[taskIdx]
				start := time.Now()

				if err := task.fn(ctx); err != nil {
					o.logger.Warn().
						Err(err).
						Str("cache", task.name).
						Msg("cache refresh failed")
					errCh <- fmt.Errorf("%s: %w", task.name, err)
				} else {
					o.logger.Debug().
						Str("cache", task.name).
						Dur("duration", time.Since(start)).
						Msg("cache refreshed")
				}
			}
		}()
	}

	// Enqueue tasks
	for i := range tasks {
		taskCh <- i
	}
	close(taskCh)

	// Wait for completion
	wg.Wait()
	close(errCh)

	// Collect errors
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("cache refresh had %d errors: %v", len(errs), errs)
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

	// Refresh each known app by calling Get (which will trigger L3 if needed)
	for _, appAddr := range knownApps {
		if _, err := o.applicationCache.Get(ctx, appAddr); err != nil {
			o.logger.Warn().
				Err(err).
				Str(logging.FieldAppAddress, appAddr).
				Msg("failed to refresh application")
		}
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

	// Refresh each known service by calling Get (which will trigger L3 if needed)
	for _, serviceID := range knownServices {
		if _, err := o.serviceCache.Get(ctx, serviceID); err != nil {
			o.logger.Warn().
				Err(err).
				Str(logging.FieldServiceID, serviceID).
				Msg("failed to refresh service")
		}
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

	// For each supplier, get their state and discover their services
	for _, supplierAddr := range knownSuppliers {
		state, err := o.supplierCache.GetSupplierState(ctx, supplierAddr)
		if err != nil {
			o.logger.Warn().
				Err(err).
				Str("supplier_address", supplierAddr).
				Msg("failed to get supplier state during refresh")
			continue
		}

		if state == nil {
			continue
		}

		// Register services from this supplier
		for _, serviceID := range state.Services {
			if serviceID != "" {
				o.knownServices.Store(serviceID, struct{}{})
			}
		}
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
						_ = o.redisClient.SAdd(ctx, "ha:cache:known:applications", addr).Err()
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

	// Warmup keyed caches in parallel
	var wg sync.WaitGroup
	wg.Add(3)

	go logging.RecoverGoRoutine(o.logger, "cache_warmup_apps", func(ctx context.Context) {
		defer wg.Done()
		if app, ok := o.applicationCache.(*applicationCache); ok {
			_ = app.WarmupFromRedis(ctx, knownApps)
		}
	})(ctx)

	go logging.RecoverGoRoutine(o.logger, "cache_warmup_services", func(ctx context.Context) {
		defer wg.Done()
		if svc, ok := o.serviceCache.(*serviceCache); ok {
			_ = svc.WarmupFromRedis(ctx, knownServices)
		}
	})(ctx)

	go logging.RecoverGoRoutine(o.logger, "cache_warmup_suppliers", func(ctx context.Context) {
		defer wg.Done()
		_ = o.supplierCache.WarmupFromRedis(ctx, knownSuppliers)
	})(ctx)

	wg.Wait()

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

	o.logger.Info().Msg("cache warmup complete")

	return nil
}

// getKnownAppsFromRedis retrieves the list of known app addresses from Redis.
func (o *CacheOrchestrator) getKnownAppsFromRedis(ctx context.Context) []string {
	members, err := o.redisClient.SMembers(ctx, "ha:cache:known:applications").Result()
	if err != nil {
		o.logger.Warn().Err(err).Msg("failed to get known apps from Redis")
		return nil
	}
	return members
}

// getKnownServicesFromRedis retrieves the list of known service IDs from Redis.
func (o *CacheOrchestrator) getKnownServicesFromRedis(ctx context.Context) []string {
	members, err := o.redisClient.SMembers(ctx, "ha:cache:known:services").Result()
	if err != nil {
		o.logger.Warn().Err(err).Msg("failed to get known services from Redis")
		return nil
	}
	return members
}

// getKnownSuppliersFromRedis retrieves the list of known supplier addresses from Redis.
func (o *CacheOrchestrator) getKnownSuppliersFromRedis(ctx context.Context) []string {
	members, err := o.redisClient.SMembers(ctx, "ha:cache:known:suppliers").Result()
	if err != nil {
		o.logger.Warn().Err(err).Msg("failed to get known suppliers from Redis")
		return nil
	}
	return members
}

// updateKnownAppsSet updates the Redis set with known apps (for warmup on restart).
func (o *CacheOrchestrator) updateKnownAppsSet(ctx context.Context, knownApps []string) error {
	o.redisClient.Del(ctx, "ha:cache:known:applications")
	if len(knownApps) > 0 {
		return o.redisClient.SAdd(ctx, "ha:cache:known:applications", knownApps).Err()
	}
	return nil
}

// updateKnownServicesSet updates the Redis set with known services (for warmup on restart).
func (o *CacheOrchestrator) updateKnownServicesSet(ctx context.Context, knownServices []string) error {
	o.redisClient.Del(ctx, "ha:cache:known:services")
	if len(knownServices) > 0 {
		return o.redisClient.SAdd(ctx, "ha:cache:known:services", knownServices).Err()
	}
	return nil
}

// updateKnownSuppliersSet updates the Redis set with known suppliers (for warmup on restart).
func (o *CacheOrchestrator) updateKnownSuppliersSet(ctx context.Context, knownSuppliers []string) error {
	o.redisClient.Del(ctx, "ha:cache:known:suppliers")
	if len(knownSuppliers) > 0 {
		return o.redisClient.SAdd(ctx, "ha:cache:known:suppliers", knownSuppliers).Err()
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
