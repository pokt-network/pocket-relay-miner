package cache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cosmos/gogoproto/proto"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

const (
	// Redis key for session params cache
	sessionParamsRedisKey = "ha:cache:session_params"

	// Lock key for distributed locking during L3 query
	sessionParamsLockKey = "ha:cache:lock:session_params"

	// Cache type for pub/sub and metrics
	sessionParamsCacheType = "session_params"

	// Default TTL for session params cache (10 minutes)
	sessionParamsCacheTTL = 10 * time.Minute
)

// sessionParamsCache implements SingletonEntityCache[*sessiontypes.Params]
// for caching session module parameters.
//
// Cache levels:
// - L1: In-memory cache using atomic.Pointer for lock-free reads
// - L2: Redis cache with proto marshaling
// - L3: Chain query via SessionQueryClient
//
// The cache subscribes to pub/sub invalidation events to stay synchronized
// across all instances.
type sessionParamsCache struct {
	logger      logging.Logger
	redisClient redis.UniversalClient
	queryClient SessionQueryClient

	// L1: In-memory cache (atomic for lock-free reads)
	localCache atomic.Pointer[sessiontypes.Params]

	// Pub/sub
	pubsub *redis.PubSub

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// SessionQueryClient defines the interface for querying session module parameters.
type SessionQueryClient interface {
	// GetParams queries the session module parameters from the chain.
	GetParams(ctx context.Context) (*sessiontypes.Params, error)
}

// NewSessionParamsCache creates a new session params cache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
func NewSessionParamsCache(
	logger logging.Logger,
	redisClient redis.UniversalClient,
	queryClient SessionQueryClient,
) SingletonEntityCache[*sessiontypes.Params] {
	return &sessionParamsCache{
		logger:      logging.ForComponent(logger, logging.ComponentSharedParamCache),
		redisClient: redisClient,
		queryClient: queryClient,
	}
}

// Start initializes the cache and subscribes to pub/sub invalidation events.
func (c *sessionParamsCache) Start(ctx context.Context) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	// Subscribe to invalidation events
	if err := SubscribeToInvalidations(
		c.ctx,
		c.redisClient,
		c.logger,
		sessionParamsCacheType,
		c.handleInvalidation,
	); err != nil {
		return fmt.Errorf("failed to subscribe to invalidations: %w", err)
	}

	c.logger.Info().Msg("session params cache started")

	return nil
}

// Close gracefully shuts down the cache.
func (c *sessionParamsCache) Close() error {
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.wg.Wait()

	if c.pubsub != nil {
		_ = c.pubsub.Close()
	}

	c.logger.Info().Msg("session params cache stopped")

	return nil
}

// Get retrieves the session params using L1 → L2 → L3 fallback pattern.
func (c *sessionParamsCache) Get(ctx context.Context) (*sessiontypes.Params, error) {
	// L1: Check local cache (atomic pointer)
	if params := c.localCache.Load(); params != nil {
		cacheHits.WithLabelValues(sessionParamsCacheType, CacheLevelL1).Inc()
		return params, nil
	}

	// L2: Check Redis cache
	data, err := c.redisClient.Get(ctx, sessionParamsRedisKey).Bytes()
	if err == nil {
		var params sessiontypes.Params
		if err := proto.Unmarshal(data, &params); err == nil {
			// Store in L1 for next time
			c.localCache.Store(&params)
			cacheHits.WithLabelValues(sessionParamsCacheType, CacheLevelL2).Inc()

			c.logger.Debug().Msg("session params cache hit (L2)")

			return &params, nil
		} else {
			c.logger.Warn().
				Err(err).
				Msg("failed to unmarshal session params from Redis")
		}
	}

	// L3: Query chain with distributed lock
	params, err := c.queryChainWithLock(ctx)
	if err != nil {
		cacheMisses.WithLabelValues(sessionParamsCacheType, "l3_error").Inc()
		return nil, fmt.Errorf("failed to query session params: %w", err)
	}

	// Store in L2 and L1
	if err := c.Set(ctx, params, sessionParamsCacheTTL); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to cache session params after L3 query")
	}

	cacheMisses.WithLabelValues(sessionParamsCacheType, "l3").Inc()
	c.logger.Debug().Msg("session params cache miss (L3)")

	return params, nil
}

// Set stores the session params in both L1 and L2 caches.
func (c *sessionParamsCache) Set(ctx context.Context, params *sessiontypes.Params, ttl time.Duration) error {
	// L1: Store in local cache
	c.localCache.Store(params)

	// L2: Store in Redis (proto marshaling)
	data, err := proto.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to marshal session params: %w", err)
	}

	if err := c.redisClient.Set(ctx, sessionParamsRedisKey, data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set Redis cache: %w", err)
	}

	c.logger.Debug().
		Dur("ttl", ttl).
		Msg("session params cached")

	return nil
}

// Refresh updates the cache from the chain (called by leader only).
func (c *sessionParamsCache) Refresh(ctx context.Context) error {
	start := time.Now()

	params, err := c.queryClient.GetParams(ctx)
	if err != nil {
		cacheRefreshErrors.WithLabelValues(sessionParamsCacheType).Inc()
		return fmt.Errorf("failed to query session params: %w", err)
	}

	// Store in L2 and L1
	if err := c.Set(ctx, params, sessionParamsCacheTTL); err != nil {
		cacheRefreshErrors.WithLabelValues(sessionParamsCacheType).Inc()
		return fmt.Errorf("failed to cache session params: %w", err)
	}

	// Publish invalidation event to notify other instances
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, sessionParamsCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event after refresh")
	}

	cacheRefreshDuration.WithLabelValues(sessionParamsCacheType).Observe(time.Since(start).Seconds())

	c.logger.Debug().
		Dur("duration", time.Since(start)).
		Msg("session params cache refreshed")

	return nil
}

// InvalidateAll clears the entire cache (both L1 and L2).
func (c *sessionParamsCache) InvalidateAll(ctx context.Context) error {
	// Remove from L1 (local cache)
	c.localCache.Store(nil)

	// Remove from L2 (Redis)
	if err := c.redisClient.Del(ctx, sessionParamsRedisKey).Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to delete session params from Redis")
	}

	// Publish invalidation event to other instances
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, sessionParamsCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(sessionParamsCacheType, SourceManual).Inc()

	c.logger.Info().Msg("session params cache invalidated")

	return nil
}

// WarmupFromRedis populates L1 cache from Redis on startup.
func (c *sessionParamsCache) WarmupFromRedis(ctx context.Context) error {
	c.logger.Info().Msg("warming up session params cache from Redis")

	// Load from Redis (L2) into local cache (L1)
	data, err := c.redisClient.Get(ctx, sessionParamsRedisKey).Bytes()
	if err != nil {
		// Key doesn't exist in Redis, skip warmup
		c.logger.Debug().Msg("no session params in Redis to warm up")
		return nil
	}

	var params sessiontypes.Params
	if err := proto.Unmarshal(data, &params); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to unmarshal session params during warmup")
		return err
	}

	c.localCache.Store(&params)

	c.logger.Info().Msg("session params cache warmup complete")

	return nil
}

// queryChainWithLock queries the chain with distributed locking to prevent
// duplicate queries from multiple instances.
func (c *sessionParamsCache) queryChainWithLock(ctx context.Context) (*sessiontypes.Params, error) {
	// Try to acquire distributed lock
	locked, err := c.redisClient.SetNX(ctx, sessionParamsLockKey, "1", 5*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer c.redisClient.Del(ctx, sessionParamsLockKey)

	if !locked {
		// Another instance is querying, wait and retry L2
		c.logger.Debug().Msg("another instance is querying session params, waiting")
		time.Sleep(100 * time.Millisecond)

		// Retry L2 after waiting
		data, err := c.redisClient.Get(ctx, sessionParamsRedisKey).Bytes()
		if err == nil {
			var params sessiontypes.Params
			if err := proto.Unmarshal(data, &params); err == nil {
				cacheHits.WithLabelValues(sessionParamsCacheType, CacheLevelL2Retry).Inc()
				return &params, nil
			}
		}

		// If still not in Redis, query chain anyway
	}

	// Query chain
	chainQueries.WithLabelValues("session_params").Inc()

	params, err := c.queryClient.GetParams(ctx)
	if err != nil {
		chainQueryErrors.WithLabelValues("session_params").Inc()
		return nil, err
	}

	return params, nil
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *sessionParamsCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received session params invalidation event")

	// Remove from L1 (local cache)
	c.localCache.Store(nil)

	cacheInvalidations.WithLabelValues(sessionParamsCacheType, SourcePubSub).Inc()

	return nil
}
