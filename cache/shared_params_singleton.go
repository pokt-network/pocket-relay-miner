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
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

const (
	// Redis key for shared params cache
	sharedParamsRedisKey = "ha:cache:shared_params"

	// Lock key for distributed locking during L3 query
	sharedParamsLockKey = "ha:cache:lock:shared_params"

	// Cache type for pub/sub and metrics
	sharedParamsCacheType = "shared_params"

	// Default TTL for shared params cache (10 minutes)
	sharedParamsCacheTTL = 10 * time.Minute
)

// sharedParamsCache implements SingletonEntityCache[*sharedtypes.Params]
// for caching shared module parameters.
//
// Cache levels:
// - L1: In-memory cache using atomic.Pointer for lock-free reads
// - L2: Redis cache with proto marshaling
// - L3: Chain query via SharedQueryClient
//
// The cache subscribes to pub/sub invalidation events to stay synchronized
// across all instances.
type sharedParamsCache struct {
	logger      logging.Logger
	redisClient redis.UniversalClient
	queryClient client.SharedQueryClient

	// L1: In-memory cache (atomic for lock-free reads)
	localCache atomic.Pointer[sharedtypes.Params]

	// Pub/sub
	pubsub *redis.PubSub

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewSharedParamsCache creates a new shared params cache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
func NewSharedParamsCache(
	logger logging.Logger,
	redisClient redis.UniversalClient,
	queryClient client.SharedQueryClient,
) SingletonEntityCache[*sharedtypes.Params] {
	return &sharedParamsCache{
		logger:      logging.ForComponent(logger, logging.ComponentSharedParamCache),
		redisClient: redisClient,
		queryClient: queryClient,
	}
}

// Start initializes the cache and subscribes to pub/sub invalidation events.
func (c *sharedParamsCache) Start(ctx context.Context) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	// Subscribe to invalidation events
	if err := SubscribeToInvalidations(
		c.ctx,
		c.redisClient,
		c.logger,
		sharedParamsCacheType,
		c.handleInvalidation,
	); err != nil {
		return fmt.Errorf("failed to subscribe to invalidations: %w", err)
	}

	c.logger.Info().Msg("shared params cache started")

	return nil
}

// Close gracefully shuts down the cache.
func (c *sharedParamsCache) Close() error {
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.wg.Wait()

	if c.pubsub != nil {
		_ = c.pubsub.Close()
	}

	c.logger.Info().Msg("shared params cache stopped")

	return nil
}

// Get retrieves the shared params using L1 → L2 → L3 fallback pattern.
func (c *sharedParamsCache) Get(ctx context.Context) (*sharedtypes.Params, error) {
	// L1: Check local cache (atomic pointer)
	if params := c.localCache.Load(); params != nil {
		cacheHits.WithLabelValues(sharedParamsCacheType, CacheLevelL1).Inc()
		return params, nil
	}

	// L2: Check Redis cache
	data, err := c.redisClient.Get(ctx, sharedParamsRedisKey).Bytes()
	if err == nil {
		var params sharedtypes.Params
		if err := proto.Unmarshal(data, &params); err == nil {
			// Store in L1 for next time
			c.localCache.Store(&params)
			cacheHits.WithLabelValues(sharedParamsCacheType, CacheLevelL2).Inc()

			c.logger.Debug().Msg("shared params cache hit (L2)")

			return &params, nil
		} else {
			c.logger.Warn().
				Err(err).
				Msg("failed to unmarshal shared params from Redis")
		}
	}

	// L3: Query chain with distributed lock
	params, err := c.queryChainWithLock(ctx)
	if err != nil {
		cacheMisses.WithLabelValues(sharedParamsCacheType, "l3_error").Inc()
		return nil, fmt.Errorf("failed to query shared params: %w", err)
	}

	// Store in L2 and L1
	if err := c.Set(ctx, params, sharedParamsCacheTTL); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to cache shared params after L3 query")
	}

	cacheMisses.WithLabelValues(sharedParamsCacheType, "l3").Inc()
	c.logger.Debug().Msg("shared params cache miss (L3)")

	return params, nil
}

// Set stores the shared params in both L1 and L2 caches.
func (c *sharedParamsCache) Set(ctx context.Context, params *sharedtypes.Params, ttl time.Duration) error {
	// L1: Store in local cache
	c.localCache.Store(params)

	// L2: Store in Redis (proto marshaling)
	data, err := proto.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to marshal shared params: %w", err)
	}

	if err := c.redisClient.Set(ctx, sharedParamsRedisKey, data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set Redis cache: %w", err)
	}

	c.logger.Debug().
		Dur("ttl", ttl).
		Msg("shared params cached")

	return nil
}

// Refresh updates the cache from the chain (called by leader only).
func (c *sharedParamsCache) Refresh(ctx context.Context) error {
	// Query latest params from chain
	params, err := c.queryClient.GetParams(ctx)
	if err != nil {
		return fmt.Errorf("failed to refresh shared params: %w", err)
	}

	// Update cache
	if err := c.Set(ctx, params, sharedParamsCacheTTL); err != nil {
		return err
	}

	// Publish invalidation event to other instances
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, sharedParamsCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event")
	}

	return nil
}

// InvalidateAll clears the entire cache (both L1 and L2).
func (c *sharedParamsCache) InvalidateAll(ctx context.Context) error {
	// Clear L1 (local cache)
	c.localCache.Store(nil)

	// Clear L2 (Redis)
	if err := c.redisClient.Del(ctx, sharedParamsRedisKey).Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to delete from Redis")
	}

	// Publish invalidation event (empty payload means invalidate all)
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, sharedParamsCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(sharedParamsCacheType, SourceManual).Inc()

	c.logger.Info().Msg("shared params cache invalidated")

	return nil
}

// WarmupFromRedis populates L1 cache from Redis on startup.
func (c *sharedParamsCache) WarmupFromRedis(ctx context.Context) error {
	c.logger.Info().Msg("warming up shared params cache from Redis")

	// Try to load from Redis into L1
	data, err := c.redisClient.Get(ctx, sharedParamsRedisKey).Bytes()
	if err != nil {
		if err == redis.Nil {
			// Not in cache yet, that's OK
			c.logger.Debug().Msg("shared params not in Redis, will be loaded on first query")
			return nil
		}
		c.logger.Warn().Err(err).Msg("failed to get shared params from Redis during warmup")
		return err
	}

	// Unmarshal and store in L1
	var params sharedtypes.Params
	if err := proto.Unmarshal(data, &params); err != nil {
		c.logger.Warn().Err(err).Msg("failed to unmarshal shared params during warmup")
		return err
	}

	c.localCache.Store(&params)
	c.logger.Info().Msg("shared params cache warmup complete")

	return nil
}

// queryChainWithLock queries the chain with distributed locking to prevent
// duplicate queries from multiple instances.
func (c *sharedParamsCache) queryChainWithLock(ctx context.Context) (*sharedtypes.Params, error) {
	// Try to acquire distributed lock
	locked, err := c.redisClient.SetNX(ctx, sharedParamsLockKey, "1", 5*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer c.redisClient.Del(ctx, sharedParamsLockKey)

	if !locked {
		// Another instance is querying, wait and retry L2
		c.logger.Debug().Msg("another instance is querying shared params, waiting")
		time.Sleep(100 * time.Millisecond)

		// Retry L2 after waiting
		data, err := c.redisClient.Get(ctx, sharedParamsRedisKey).Bytes()
		if err == nil {
			var params sharedtypes.Params
			if err := proto.Unmarshal(data, &params); err == nil {
				c.localCache.Store(&params)
				cacheHits.WithLabelValues(sharedParamsCacheType, CacheLevelL2Retry).Inc()
				return &params, nil
			}
		}

		// If still not in Redis, query chain anyway
	}

	// Query chain
	chainQueries.WithLabelValues("shared_params").Inc()

	params, err := c.queryClient.GetParams(ctx)
	if err != nil {
		chainQueryErrors.WithLabelValues("shared_params").Inc()
		return nil, err
	}

	return params, nil
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *sharedParamsCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received shared params invalidation event")

	// Clear L1 (local cache)
	c.localCache.Store(nil)

	cacheInvalidations.WithLabelValues(sharedParamsCacheType, SourcePubSub).Inc()

	return nil
}
