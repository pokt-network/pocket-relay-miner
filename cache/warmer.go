package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alitto/pond/v2"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
)

const (
	// Default warmup concurrency
	defaultWarmupConcurrency = 10

	// Default warmup timeout per application
	defaultWarmupTimeout = 5 * time.Second
)

// CacheWarmerConfig contains configuration for the cache warmer.
type CacheWarmerConfig struct {
	// KnownApplications is a list of application addresses to pre-warm on startup.
	// These are configured by the operator.
	KnownApplications []string

	// WarmupConcurrency is the number of parallel warmup operations.
	// Higher values = faster warmup but more load on the chain.
	WarmupConcurrency int

	// WarmupTimeout is the timeout for warming each application.
	WarmupTimeout time.Duration
}

// CacheWarmer handles pre-warming caches for faster request processing.
type CacheWarmer struct {
	logger logging.Logger
	config CacheWarmerConfig

	// Query clients for warming
	appClient     client.ApplicationQueryClient
	accountClient client.AccountQueryClient
	sharedClient  client.SharedQueryClient

	// Worker pool for parallel warmup operations
	workerPool pond.Pool

	// Metrics
	warmedApps   int64
	failedApps   int64
	warmupTimeMs int64
}

// NewCacheWarmer creates a new cache warmer.
func NewCacheWarmer(
	logger logging.Logger,
	config CacheWarmerConfig,
	appClient client.ApplicationQueryClient,
	accountClient client.AccountQueryClient,
	sharedClient client.SharedQueryClient,
) *CacheWarmer {
	if config.WarmupConcurrency == 0 {
		config.WarmupConcurrency = defaultWarmupConcurrency
	}
	if config.WarmupTimeout == 0 {
		config.WarmupTimeout = defaultWarmupTimeout
	}

	// Create worker pool for parallel warmup operations
	// This is a bounded pool to prevent overwhelming the network/Redis
	workerPool := pond.NewPool(config.WarmupConcurrency)

	logger.Info().
		Int("warmup_workers", config.WarmupConcurrency).
		Dur("warmup_timeout", config.WarmupTimeout).
		Msg("created cache warmer worker pool")

	return &CacheWarmer{
		logger:        logger.With().Str("component", "cache_warmer").Logger(),
		config:        config,
		appClient:     appClient,
		accountClient: accountClient,
		sharedClient:  sharedClient,
		workerPool:    workerPool,
	}
}

// WarmupResult contains the results of a warmup operation.
type WarmupResult struct {
	TotalApps   int
	WarmedApps  int
	FailedApps  int
	DurationMs  int64
	FailedAddrs []string
}

// Warmup pre-warms caches for known applications.
// This should be called at startup for fastest first-request performance.
func (w *CacheWarmer) Warmup(ctx context.Context) (*WarmupResult, error) {
	startTime := time.Now()

	// Collect all apps to warm: config + redis persisted
	appsToWarm := w.collectAppsToWarm(ctx)
	if len(appsToWarm) == 0 {
		w.logger.Info().Msg("no applications to warm up")
		return &WarmupResult{}, nil
	}

	w.logger.Info().
		Int("total_apps", len(appsToWarm)).
		Int("concurrency", w.config.WarmupConcurrency).
		Msg("starting cache warmup")

	// Warm in parallel
	result := w.warmAppsParallel(ctx, appsToWarm)
	result.DurationMs = time.Since(startTime).Milliseconds()

	// Update metrics
	atomic.StoreInt64(&w.warmedApps, int64(result.WarmedApps))
	atomic.StoreInt64(&w.failedApps, int64(result.FailedApps))
	atomic.StoreInt64(&w.warmupTimeMs, result.DurationMs)

	w.logger.Info().
		Int("warmed", result.WarmedApps).
		Int("failed", result.FailedApps).
		Int64("duration_ms", result.DurationMs).
		Msg("cache warmup complete")

	return result, nil
}

// collectAppsToWarm collects all application addresses to warm.
func (w *CacheWarmer) collectAppsToWarm(_ context.Context) []string {
	appSet := make(map[string]struct{})

	// Add configured known applications
	for _, addr := range w.config.KnownApplications {
		if addr != "" {
			appSet[addr] = struct{}{}
		}
	}

	// Convert to slice
	apps := make([]string, 0, len(appSet))
	for addr := range appSet {
		apps = append(apps, addr)
	}
	return apps
}

// warmAppsParallel warms applications in parallel for speed using pond workers.
func (w *CacheWarmer) warmAppsParallel(ctx context.Context, apps []string) *WarmupResult {
	result := &WarmupResult{
		TotalApps: len(apps),
	}

	// Results collection (protected by mutex)
	var mu sync.Mutex

	// Create pond Group for coordinated task execution
	group := w.workerPool.NewGroup()

	// Submit warmup tasks to the worker pool
	for _, appAddr := range apps {
		// Capture for closure
		capturedAddr := appAddr
		group.Submit(func() {
			// Create timeout context for this warmup operation
			warmCtx, cancel := context.WithTimeout(ctx, w.config.WarmupTimeout)
			defer cancel()

			// Warm the application
			err := w.warmApp(warmCtx, capturedAddr)

			// Update results (thread-safe)
			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				result.FailedApps++
				result.FailedAddrs = append(result.FailedAddrs, capturedAddr)
				w.logger.Debug().
					Err(err).
					Str("app_address", capturedAddr).
					Msg("failed to warm app")
			} else {
				result.WarmedApps++
				w.logger.Debug().
					Str("app_address", capturedAddr).
					Msg("warmed app successfully")
			}
		})
	}

	// Wait for all warmup tasks to complete.
	//
	// "We track errors manually in the result struct" was true for errors and
	// false for panics: pond recovers them by default (pool.go:534) and returns
	// them here, and a panicking task updates neither WarmedApps nor FailedApps.
	// The old `_ =` therefore lost it twice -- no log, no metric, and a summary
	// where WarmedApps+FailedApps silently falls short of TotalApps.
	//
	// The rule below is deliberately a rule and not a list. Tasks go in as func(),
	// so no task error is possible, but the channel still carries ErrPoolStopped
	// for a Submit made after the pool stopped (result.go:77-83), ErrGroupStopped
	// for a stopped group (group.go:12), and the context error if the context is
	// cancelled -- and Stop() stops this pool (:263). Only a recovered panic is
	// counted and raised; anything else here is shutdown.
	//
	// Unlike the reconciler and the stream trimmer, this work is NOT retried:
	// warmup is one-shot at startup. What bounds the loss instead is that the
	// caches populate on demand during relays, so the cost is first-use latency for
	// the apps whose task died, not missing data -- which is why the panic counter
	// is enough and no loss-specific counter is added.
	if err := group.Wait(); err != nil {
		if errors.Is(err, pond.ErrPanic) {
			logging.PanicRecoveriesTotal.WithLabelValues("cache_warmup_app").Inc()
			w.logger.Error().Err(err).
				Int("total_apps", result.TotalApps).
				Int("warmed", result.WarmedApps).
				Int("failed", result.FailedApps).
				Msg("cache warmup: a warm task panicked; the counts on this line do not add up to total")
		} else {
			w.logger.Debug().Err(err).Msg("cache warmup: abandoned (pool shutting down)")
		}
	}

	return result
}

// warmApp warms caches for a single application.
func (w *CacheWarmer) warmApp(ctx context.Context, appAddr string) error {
	// 1. Warm application cache
	app, err := w.appClient.GetApplication(ctx, appAddr)
	if err != nil {
		return err
	}

	// 2. Warm account cache for app address
	_, err = w.accountClient.GetPubKeyFromAddress(ctx, appAddr)
	if err != nil {
		return err
	}

	// 3. Warm account cache for delegated gateways
	for _, gatewayAddr := range app.DelegateeGatewayAddresses {
		_, err = w.accountClient.GetPubKeyFromAddress(ctx, gatewayAddr)
		if err != nil {
			w.logger.Debug().
				Err(err).
				Str("gateway_address", gatewayAddr).
				Msg("failed to warm gateway account (may be okay if not on chain)")
			// Don't fail - gateway might not have a registered account yet
		}
	}

	// 4. Warm shared params (only once, but safe to call multiple times)
	_, _ = w.sharedClient.GetParams(ctx)

	return nil
}

// Stop stops the cache warmer and cleans up resources.
// This should be called when the cache warmer is no longer needed.
func (w *CacheWarmer) Stop() {
	if w.workerPool != nil {
		w.workerPool.StopAndWait()
		w.logger.Debug().Msg("cache warmer worker pool stopped")
	}
}
