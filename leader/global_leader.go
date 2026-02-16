package leader

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Lua script for atomic acquire (only if key doesn't exist or is expired)
const acquireLuaScript = `
if redis.call("EXISTS", KEYS[1]) == 0 then
    redis.call("SET", KEYS[1], ARGV[1], "EX", ARGV[2])
    return 1
else
    return 0
end
`

// Lua script for atomic renew (only if we own the lock)
const renewLuaScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    redis.call("EXPIRE", KEYS[1], ARGV[2])
    return 1
else
    return 0
end
`

// Lua script for atomic release (only if we own the lock)
const releaseLuaScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    redis.call("DEL", KEYS[1])
    return 1
else
    return 0
end
`

// LeadershipCallback is called when leadership status changes.
type LeadershipCallback func(ctx context.Context)

// GlobalLeaderElectorConfig holds configuration for the leader elector.
type GlobalLeaderElectorConfig struct {
	// LeaderTTL is how long the leader lock lasts before expiring
	LeaderTTL time.Duration

	// HeartbeatRate is how often we try to acquire/renew leadership
	HeartbeatRate time.Duration
}

// GlobalLeaderElector is the SINGLE lighthouse component for leadership.
// All components that need leader awareness check this instance.
// Uses Lua scripts for atomic acquire/renew/release operations.
//
// This component is used by:
// - CacheOrchestrator (cache refresh)
// - LifecycleCallback (claim/proof submission)
// - Any future leader-only operations
type GlobalLeaderElector struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	instanceID  string // Unique ID for this miner instance (hostname + UUID)
	config      GlobalLeaderElectorConfig
	metrics     *leaderMetrics
	leaderKey   string // Redis key for global leader lock (built via KeyBuilder)

	isLeader                   atomic.Bool
	consecutiveAcquireFailures int

	// Lua scripts for atomic operations
	acquireScript *redis.Script
	renewScript   *redis.Script
	releaseScript *redis.Script

	// Callbacks for leadership changes
	onElectedCallbacks []LeadershipCallback
	onLostCallbacks    []LeadershipCallback
	callbackMu         sync.RWMutex

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewGlobalLeaderElectorWithConfig creates a new global leader elector with custom configuration.
//
// Parameters:
//   - logger: Logger instance for this component
//   - redisClient: Redis client for distributed locking
//   - instanceID: Unique identifier for this instance (e.g., hostname + UUID)
//   - config: Configuration for leader election timing
//
// The instance ID should be unique across all miner instances to prevent conflicts.
func NewGlobalLeaderElectorWithConfig(
	logger logging.Logger,
	redisClient *redisutil.Client,
	instanceID string,
	config GlobalLeaderElectorConfig,
) *GlobalLeaderElector {
	return &GlobalLeaderElector{
		logger:        logging.ForComponent(logger, logging.ComponentLeaderElector),
		redisClient:   redisClient,
		instanceID:    instanceID,
		config:        config,
		metrics:       initMetrics(), // Lazy-load metrics only when elector is created
		leaderKey:     redisClient.KB().GlobalLeaderKey(),
		acquireScript: redis.NewScript(acquireLuaScript),
		renewScript:   redis.NewScript(renewLuaScript),
		releaseScript: redis.NewScript(releaseLuaScript),
	}
}

// Start begins the leader election loop.
//
// The leader election process:
// 1. Every globalHeartbeatRate (10s), attempt to acquire or renew leadership
// 2. If not leader, try to acquire the lock atomically
// 3. If leader, try to renew the lock atomically
// 4. If renew fails, we've lost leadership (another instance took over)
//
// Leadership is maintained through a Redis key with TTL. The instance that
// successfully sets the key becomes the leader. The leader must renew the
// lock before it expires to maintain leadership.
func (e *GlobalLeaderElector) Start(ctx context.Context) error {
	e.ctx, e.cancelFn = context.WithCancel(ctx)

	e.wg.Add(1)
	go e.leaderLoop(e.ctx)

	e.logger.Info().
		Str(logging.FieldInstance, e.instanceID).
		Msg("global leader elector started")

	return nil
}

// leaderLoop is the main loop that manages leader election.
func (e *GlobalLeaderElector) leaderLoop(ctx context.Context) {
	defer e.wg.Done()

	// Attempt leadership immediately on startup (don't wait for first ticker)
	e.attemptLeadership(ctx)

	ticker := time.NewTicker(e.config.HeartbeatRate)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Atomic release on shutdown (only if we own the lock)
			if e.isLeader.Load() {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

				result, err := e.releaseScript.Run(releaseCtx, e.redisClient,
					[]string{e.leaderKey},
					e.instanceID,
				).Int()

				cancel() // Call directly to avoid a defer-in-loop antipattern

				if err == nil && result == 1 {
					e.logger.Info().Msg("released leadership on shutdown")
				}
			}
			return

		case <-ticker.C:
			e.attemptLeadership(ctx)
		}
	}
}

// attemptLeadership tries to acquire or renew leadership atomically.
func (e *GlobalLeaderElector) attemptLeadership(ctx context.Context) {
	wasLeader := e.isLeader.Load()

	if wasLeader {
		// We think we're a leader - try to renew
		result, err := e.renewScript.Run(ctx, e.redisClient,
			[]string{e.leaderKey},
			e.instanceID,
			int(e.config.LeaderTTL.Seconds()),
		).Int()

		if err != nil {
			e.logger.Warn().
				Err(err).
				Str(logging.FieldInstance, e.instanceID).
				Msg("failed to renew leadership (Redis error)")
			e.isLeader.Store(false)
			e.metrics.status.WithLabelValues(e.instanceID).Set(0)
			e.metrics.losses.WithLabelValues(e.instanceID).Inc()
			e.invokeOnLostCallbacks(ctx)
		} else if result == 0 {
			// Lost leadership - another instance took over
			e.logger.Warn().
				Str(logging.FieldInstance, e.instanceID).
				Msg("LOST global leadership")
			e.isLeader.Store(false)
			e.metrics.status.WithLabelValues(e.instanceID).Set(0)
			e.metrics.losses.WithLabelValues(e.instanceID).Inc()
			e.invokeOnLostCallbacks(ctx)
		} else {
			// Successfully renewed - log periodically for visibility
			e.logger.Debug().
				Str(logging.FieldInstance, e.instanceID).
				Msg("leadership renewed successfully")
		}
	} else {
		// Not leader - try to acquire
		result, err := e.acquireScript.Run(ctx, e.redisClient,
			[]string{e.leaderKey},
			e.instanceID,
			int(e.config.LeaderTTL.Seconds()),
		).Int()

		if err != nil {
			e.consecutiveAcquireFailures++

			// Determine if this is an OOM error for targeted alerting
			if redisutil.IsOOMError(err) {
				e.logger.Error().
					Err(err).
					Str(logging.FieldInstance, e.instanceID).
					Int("consecutive_failures", e.consecutiveAcquireFailures).
					Msg("REDIS OOM - cannot acquire leadership, standby miner blocked until memory is freed")
				e.metrics.acquisitionFailures.WithLabelValues(e.instanceID, "redis_oom").Inc()
			} else {
				e.logger.Warn().
					Err(err).
					Str(logging.FieldInstance, e.instanceID).
					Int("consecutive_failures", e.consecutiveAcquireFailures).
					Msg("failed to acquire leadership (Redis error)")
				e.metrics.acquisitionFailures.WithLabelValues(e.instanceID, "redis_error").Inc()
			}
		} else if result == 1 {
			// Became leader
			if e.consecutiveAcquireFailures > 0 {
				e.logger.Info().
					Str(logging.FieldInstance, e.instanceID).
					Int("recovered_after_failures", e.consecutiveAcquireFailures).
					Msg("ELECTED as GLOBAL leader (recovered from previous failures)")
			} else {
				e.logger.Info().
					Str(logging.FieldInstance, e.instanceID).
					Msg("ELECTED as GLOBAL leader")
			}
			e.consecutiveAcquireFailures = 0
			e.isLeader.Store(true)
			e.metrics.status.WithLabelValues(e.instanceID).Set(1)
			e.metrics.elections.WithLabelValues(e.instanceID).Inc()
			e.invokeOnElectedCallbacks(ctx)
		} else {
			// Another instance is leader — Redis is healthy, reset failure counter
			e.consecutiveAcquireFailures = 0
			e.metrics.status.WithLabelValues(e.instanceID).Set(0)
			e.logger.Debug().
				Str(logging.FieldInstance, e.instanceID).
				Msg("still standing as standby replica (not leader)")
		}
	}
}

// invokeOnElectedCallbacks calls all registered "elected" callbacks asynchronously.
func (e *GlobalLeaderElector) invokeOnElectedCallbacks(ctx context.Context) {
	e.callbackMu.RLock()
	callbacks := make([]LeadershipCallback, len(e.onElectedCallbacks))
	copy(callbacks, e.onElectedCallbacks)
	e.callbackMu.RUnlock()

	for _, callback := range callbacks {
		e.wg.Add(1)
		go logging.RecoverGoRoutine(e.logger, "leader_callback_elected", func(ctx context.Context) {
			defer e.wg.Done()
			callback(ctx)
		})(ctx)
	}
}

// invokeOnLostCallbacks calls all registered "lost" callbacks asynchronously.
func (e *GlobalLeaderElector) invokeOnLostCallbacks(ctx context.Context) {
	e.callbackMu.RLock()
	callbacks := make([]LeadershipCallback, len(e.onLostCallbacks))
	copy(callbacks, e.onLostCallbacks)
	e.callbackMu.RUnlock()

	for _, callback := range callbacks {
		e.wg.Add(1)
		go logging.RecoverGoRoutine(e.logger, "leader_callback_lost", func(ctx context.Context) {
			defer e.wg.Done()
			callback(ctx)
		})(ctx)
	}
}

// OnElected registers a callback to be called when this instance becomes leader.
// The callback is invoked asynchronously in a separate goroutine.
func (e *GlobalLeaderElector) OnElected(callback LeadershipCallback) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.onElectedCallbacks = append(e.onElectedCallbacks, callback)
}

// OnLost registers a callback to be called when this instance loses leadership.
// The callback is invoked asynchronously in a separate goroutine.
func (e *GlobalLeaderElector) OnLost(callback LeadershipCallback) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.onLostCallbacks = append(e.onLostCallbacks, callback)
}

// IsLeader is the lighthouse method - all components check this to determine
// if the current instance should perform leader-only operations.
//
// This method is thread-safe and can be called from any goroutine.
//
// Example usage:
//
//	if globalLeader.IsLeader() {
//	    // Perform leader-only operation (e.g., refresh caches, submit claims)
//	}
func (e *GlobalLeaderElector) IsLeader() bool {
	return e.isLeader.Load()
}

// Close stops the leader election loop and releases leadership if held.
func (e *GlobalLeaderElector) Close() {
	if e.cancelFn != nil {
		e.cancelFn()
	}
	e.wg.Wait()

	// Release leader lock on shutdown for faster failover
	if e.isLeader.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		released, err := e.releaseScript.Run(
			ctx,
			e.redisClient,
			[]string{e.leaderKey},
			e.instanceID,
		).Int()

		if err != nil {
			e.logger.Warn().Err(err).Msg("failed to release leader lock on shutdown")
		} else if released == 1 {
			e.logger.Info().Msg("released leader lock on shutdown for faster failover")
		}
	}

	e.logger.Info().Msg("global leader elector stopped")
}
