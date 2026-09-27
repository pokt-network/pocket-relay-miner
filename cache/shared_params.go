package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

var _ SharedParamCache = (*RedisSharedParamCache)(nil)

// sharedParamsLocalTTL is the max-age safety floor for an L1 (in-process) shared
// params entry. Params at a given height are immutable, but per the cache-TTL
// mandate no entry may outlive the process; an entry past this floor is treated
// as a miss and re-read. A var so tests can shrink it.
var sharedParamsLocalTTL = 30 * time.Minute

// sharedParamsLocalKeepHeights bounds the height window retained in L1. Callers
// read the latest (and occasionally a recent session-start) height, so entries
// far below the newest height are dead weight; storing prunes anything older.
// This caps L1 at ~this many entries instead of growing ~1/block forever.
const sharedParamsLocalKeepHeights = 64

// sharedParamLocalEntry is an L1-cached shared params value plus its height (for
// window-bounded pruning) and fetch time (for the TTL floor).
type sharedParamLocalEntry struct {
	params   *sharedtypes.Params
	height   int64
	cachedAt time.Time
}

// storeLocal caches params in L1 and prunes entries whose height is far below the
// newest, keeping the map bounded to ~sharedParamsLocalKeepHeights entries.
func (c *RedisSharedParamCache) storeLocal(key string, height int64, params *sharedtypes.Params) {
	c.localCache.Store(key, sharedParamLocalEntry{params: params, height: height, cachedAt: time.Now()})

	cutoff := height - sharedParamsLocalKeepHeights
	if cutoff <= 0 {
		return
	}
	c.localCache.Range(func(k string, e sharedParamLocalEntry) bool {
		if e.height < cutoff {
			c.localCache.Delete(k)
		}
		return true
	})
}

// RedisSharedParamCache implements SharedParamCache using Redis as L2 cache.
type RedisSharedParamCache struct {
	logger       logging.Logger
	redisClient  *redisutil.Client
	sharedClient client.SharedQueryClient
	blockClient  client.BlockClient
	config       CacheConfig

	// L1 local cache, keyed by height. Values are sharedParamLocalEntry so each
	// is TTL-floored and the map is pruned to a bounded height window (it used to
	// grow ~1 entry/block forever — only the latest few heights are ever read).
	localCache *xsync.Map[string, sharedParamLocalEntry]

	// Cache keys helper

	// Lifecycle
	mu     sync.RWMutex
	closed bool
}

// NewRedisSharedParamCache creates a new SharedParamCache backed by Redis.
func NewRedisSharedParamCache(
	logger logging.Logger,
	redisClient *redisutil.Client,
	sharedClient client.SharedQueryClient,
	blockClient client.BlockClient,
	config CacheConfig,
) *RedisSharedParamCache {
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
		localCache:   xsync.NewMap[string, sharedParamLocalEntry](),
		logger:       logging.ForComponent(logger, logging.ComponentSharedParamCache),
		redisClient:  redisClient,
		sharedClient: sharedClient,
		blockClient:  blockClient,
		config:       config,
	}
}

// Start marks the cache as running. It spawns no background processes:
// per-height entries are immutable on chain and age out via the L1 TTL
// floor. A height-targeted invalidation subscriber used to run here, but its
// only publisher was removed — keeping the subscriber cost every replica a
// dedicated pub/sub connection on a channel nothing could publish to, while
// implying a working invalidation mechanism that did not exist.
func (c *RedisSharedParamCache) Start(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("cache is closed")
	}

	c.logger.Info().Msg("shared param cache started")
	return nil
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

	key := c.redisClient.KB().ParamsSharedAtHeightKey(height)

	// L1: Check local cache (fresh within the TTL floor only).
	if e, ok := c.localCache.Load(key); ok {
		if time.Since(e.cachedAt) < sharedParamsLocalTTL {
			cacheHits.WithLabelValues("shared_params", CacheLevelL1).Inc()
			cacheGetLatency.WithLabelValues("shared_params", CacheLevelL1).Observe(time.Since(start).Seconds())
			return e.params, nil
		}
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
			c.storeLocal(key, height, params)
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
//
// The chain read MUST be GetParamsAtHeight(height), not GetParams(): the cache is
// keyed by height and the entry is shared over Redis with every other replica.
// Reading live params and storing them under a past height's key both returns the
// wrong answer to the caller AND poisons that key fleet-wide until its TTL lapses.
// (For the latest height the two are equivalent, which is why this was invisible
// while GetLatestSharedParams was the only caller.)
func (c *RedisSharedParamCache) queryAndCacheParams(ctx context.Context, height int64, key string) (*sharedtypes.Params, error) {
	lockKey := c.redisClient.KB().ParamsSharedAtHeightLockKey(height)

	// Try to acquire lock
	locked, err := c.redisClient.SetNX(ctx, lockKey, "1", c.config.LockTimeout).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}

	if locked {
		// We got the lock - query chain
		lockAcquisitions.WithLabelValues("shared_params", "acquired").Inc()
		defer releaseCacheLock(ctx, c.redisClient, lockKey)

		chainQueries.WithLabelValues("shared_params").Inc()
		chainStart := time.Now()

		params, queryErr := c.sharedClient.GetParamsAtHeight(ctx, height)
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
		c.storeLocal(key, height, params)

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
			c.storeLocal(key, height, params)
			return params, nil
		}
	}

	// Still not available - query chain directly at the requested height.
	// Deliberately not cached: the lock holder owns populating this key.
	params, fallbackErr := c.sharedClient.GetParamsAtHeight(ctx, height)
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

// WarmupFromRedis populates L1 cache from Redis for the latest block height.
// Since shared params are indexed by height, we warm up the most recent params
// which are most likely to be queried on startup.
func (c *RedisSharedParamCache) WarmupFromRedis(ctx context.Context) error {
	c.logger.Info().Msg("warming up shared params cache from Redis")

	// Get latest block height
	latestBlock := c.blockClient.LastBlock(ctx)
	height := latestBlock.Height()

	// Try to load from Redis into L1
	key := c.redisClient.KB().ParamsSharedAtHeightKey(height)
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

	c.storeLocal(key, height, params)
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

	c.logger.Info().Msg("shared param cache closed")
	return nil
}
