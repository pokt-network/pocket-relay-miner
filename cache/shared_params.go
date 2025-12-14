package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

var _ SharedParamCache = (*RedisSharedParamCache)(nil)

// RedisSharedParamCache implements SharedParamCache using Redis as L2 cache.
type RedisSharedParamCache struct {
	logger       logging.Logger
	redisClient  redis.UniversalClient
	sharedClient client.SharedQueryClient
	blockClient  client.BlockClient
	config       CacheConfig

	// L1 local cache
	localCache sync.Map // map[string]*sharedtypes.Params

	// Cache keys helper
	keys CacheKeys

	// Lifecycle
	mu       sync.RWMutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewRedisSharedParamCache creates a new SharedParamCache backed by Redis.
func NewRedisSharedParamCache(
	logger logging.Logger,
	redisClient redis.UniversalClient,
	sharedClient client.SharedQueryClient,
	blockClient client.BlockClient,
	config CacheConfig,
) *RedisSharedParamCache {
	if config.CachePrefix == "" {
		config.CachePrefix = "ha:cache"
	}
	if config.TTLBlocks == 0 {
		config.TTLBlocks = 1
	}
	if config.BlockTimeSeconds == 0 {
		config.BlockTimeSeconds = 30
	}
	if config.LockTimeout == 0 {
		config.LockTimeout = 5 * time.Second
	}

	return &RedisSharedParamCache{
		logger:       logging.ForComponent(logger, logging.ComponentSharedParamCache),
		redisClient:  redisClient,
		sharedClient: sharedClient,
		blockClient:  blockClient,
		config:       config,
		keys:         CacheKeys{Prefix: config.CachePrefix},
	}
}

// Start begins the cache's background processes.
func (c *RedisSharedParamCache) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("cache is closed")
	}

	ctx, c.cancelFn = context.WithCancel(ctx)
	c.mu.Unlock()

	// Subscribe to cache invalidation events
	c.wg.Add(1)
	go c.subscribeToInvalidations(ctx)

	c.logger.Info().Msg("shared param cache started")
	return nil
}

// subscribeToInvalidations listens for cache invalidation events from other instances.
func (c *RedisSharedParamCache) subscribeToInvalidations(ctx context.Context) {
	defer c.wg.Done()

	channel := c.config.PubSubPrefix + ":invalidate:params"
	pubsub := c.redisClient.Subscribe(ctx, channel)
	defer func() { _ = pubsub.Close() }()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-pubsub.Channel():
			// Parse the height from the message
			var height int64
			if _, err := fmt.Sscanf(msg.Payload, "%d", &height); err != nil {
				c.logger.Warn().Err(err).Str("payload", msg.Payload).Msg("invalid invalidation message")
				continue
			}

			// Clear local cache for this height
			key := c.keys.SharedParams(height)
			c.localCache.Delete(key)
			cacheInvalidations.WithLabelValues("shared_params", "pubsub").Inc()
		}
	}
}

// GetSharedParams returns the shared module parameters for the given block height.
func (c *RedisSharedParamCache) GetSharedParams(ctx context.Context, height int64) (*sharedtypes.Params, error) {
	start := time.Now()

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, fmt.Errorf("cache is closed")
	}
	c.mu.RUnlock()

	key := c.keys.SharedParams(height)

	// L1: Check local cache
	if cached, ok := c.localCache.Load(key); ok {
		cacheHits.WithLabelValues("shared_params", CacheLevelL1).Inc()
		cacheGetLatency.WithLabelValues("shared_params", CacheLevelL1).Observe(time.Since(start).Seconds())
		return cached.(*sharedtypes.Params), nil
	}
	cacheMisses.WithLabelValues("shared_params", "l1").Inc()

	// L2: Check Redis cache
	data, err := c.redisClient.Get(ctx, key).Bytes()
	if err == nil {
		params := &sharedtypes.Params{}
		if unmarshalErr := json.Unmarshal(data, params); unmarshalErr != nil {
			c.logger.Warn().Err(unmarshalErr).Msg("failed to unmarshal cached params")
		} else {
			cacheHits.WithLabelValues("shared_params", CacheLevelL2).Inc()
			cacheGetLatency.WithLabelValues("shared_params", CacheLevelL2).Observe(time.Since(start).Seconds())
			// Store in L1
			c.localCache.Store(key, params)
			return params, nil
		}
	}
	if err != nil && err != redis.Nil {
		c.logger.Warn().Err(err).Msg("error fetching from Redis cache")
	}
	cacheMisses.WithLabelValues("shared_params", "l2").Inc()

	// L3: Query chain with distributed lock
	params, err := c.queryAndCacheParams(ctx, height, key)
	if err != nil {
		cacheGetLatency.WithLabelValues("shared_params", "l3_error").Observe(time.Since(start).Seconds())
		return nil, err
	}
	cacheGetLatency.WithLabelValues("shared_params", CacheLevelL3).Observe(time.Since(start).Seconds())
	return params, nil
}

// queryAndCacheParams queries the chain and caches the result.
// Uses distributed locking to prevent thundering herd.
func (c *RedisSharedParamCache) queryAndCacheParams(ctx context.Context, height int64, key string) (*sharedtypes.Params, error) {
	lockKey := c.keys.SharedParamsLock(height)

	// Try to acquire lock
	locked, err := c.redisClient.SetNX(ctx, lockKey, "1", c.config.LockTimeout).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}

	if locked {
		// We got the lock - query chain
		lockAcquisitions.WithLabelValues("shared_params", "acquired").Inc()
		defer c.redisClient.Del(ctx, lockKey)

		chainQueries.WithLabelValues("shared_params").Inc()
		chainStart := time.Now()

		params, queryErr := c.sharedClient.GetParams(ctx)
		chainQueryLatency.WithLabelValues("shared_params").Observe(time.Since(chainStart).Seconds())

		if queryErr != nil {
			chainQueryErrors.WithLabelValues("shared_params").Inc()
			return nil, fmt.Errorf("failed to query chain: %w", queryErr)
		}

		// Cache in Redis
		data, marshalErr := json.Marshal(params)
		if marshalErr == nil {
			ttl := c.config.BlocksToTTL(c.config.TTLBlocks)
			if cacheErr := c.redisClient.Set(ctx, key, data, ttl).Err(); cacheErr != nil {
				c.logger.Warn().Err(cacheErr).Msg("failed to cache params in Redis")
			}
		}

		// Cache in L1
		c.localCache.Store(key, params)

		return params, nil
	}

	// Another instance is populating - wait and retry from Redis
	lockAcquisitions.WithLabelValues("shared_params", "contended").Inc()
	time.Sleep(5 * time.Millisecond)

	retryData, retryErr := c.redisClient.Get(ctx, key).Bytes()
	if retryErr == nil {
		params := &sharedtypes.Params{}
		if unmarshalErr := json.Unmarshal(retryData, params); unmarshalErr == nil {
			cacheHits.WithLabelValues("shared_params", CacheLevelL2Retry).Inc()
			c.localCache.Store(key, params)
			return params, nil
		}
	}

	// Still not available - query chain directly
	params, fallbackErr := c.sharedClient.GetParams(ctx)
	if fallbackErr != nil {
		chainQueryErrors.WithLabelValues("shared_params").Inc()
		return nil, fmt.Errorf("failed to query chain: %w", fallbackErr)
	}
	chainQueries.WithLabelValues("shared_params").Inc()

	return params, nil
}

// GetLatestSharedParams returns the shared module parameters for the latest block.
func (c *RedisSharedParamCache) GetLatestSharedParams(ctx context.Context) (*sharedtypes.Params, error) {
	latestBlock := c.blockClient.LastBlock(ctx)
	return c.GetSharedParams(ctx, latestBlock.Height())
}

// InvalidateSharedParams invalidates the cached shared params for a specific height.
func (c *RedisSharedParamCache) InvalidateSharedParams(ctx context.Context, height int64) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("cache is closed")
	}
	c.mu.RUnlock()

	key := c.keys.SharedParams(height)

	// Clear L1
	c.localCache.Delete(key)

	// Clear L2
	if err := c.redisClient.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete from Redis: %w", err)
	}

	// Notify other instances
	channel := c.config.PubSubPrefix + ":invalidate:params"
	if err := c.redisClient.Publish(ctx, channel, fmt.Sprintf("%d", height)).Err(); err != nil {
		c.logger.Warn().Err(err).Msg("failed to publish invalidation")
	}

	cacheInvalidations.WithLabelValues("shared_params", "manual").Inc()
	return nil
}

// WarmupFromRedis populates L1 cache from Redis for the latest block height.
// Since shared params are indexed by height, we warm up the most recent params
// which are most likely to be queried on startup.
func (c *RedisSharedParamCache) WarmupFromRedis(ctx context.Context) error {
	c.logger.Info().Msg("warming up shared params cache from Redis")

	// Get latest block height
	latestBlock := c.blockClient.LastBlock(ctx)
	height := latestBlock.Height()

	// Try to load from Redis into L1
	key := c.keys.SharedParams(height)
	data, err := c.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			// Not in cache yet, that's OK
			c.logger.Debug().Int64("height", height).Msg("shared params not in Redis, will be loaded on first query")
			return nil
		}
		c.logger.Warn().Err(err).Msg("failed to get shared params from Redis during warmup")
		return err
	}

	// Unmarshal and store in L1
	params := &sharedtypes.Params{}
	if err := json.Unmarshal(data, params); err != nil {
		c.logger.Warn().Err(err).Msg("failed to unmarshal shared params during warmup")
		return err
	}

	c.localCache.Store(key, params)
	c.logger.Info().Int64("height", height).Msg("shared params cache warmup complete")

	return nil
}

// Close gracefully shuts down the cache.
func (c *RedisSharedParamCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true

	if c.cancelFn != nil {
		c.cancelFn()
	}

	c.wg.Wait()

	c.logger.Info().Msg("shared param cache closed")
	return nil
}
