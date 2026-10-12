package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/storage/kv"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
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

// maxPendingLocalReuse bounds how long an L1 answer that is not final is reused
// when no new block arrives: a stalled block feed must not make it permanent.
const maxPendingLocalReuse = 30 * time.Second

// sharedParamLocalEntry is an L1-cached shared params value plus its height (for
// window-bounded pruning) and fetch time (for the TTL floor). An entry that is not
// final is served only until this process sees a block past seenAt, the height
// when it was asked for.
type sharedParamLocalEntry struct {
	params   *sharedtypes.Params
	height   int64
	cachedAt time.Time
	final    bool
	seenAt   int64
}

// storeLocal caches params in L1 and prunes entries whose height is far below the
// newest, keeping the map bounded to ~sharedParamsLocalKeepHeights entries.
func (c *RedisSharedParamCache) storeLocal(key string, height int64, params *sharedtypes.Params, final bool, seenAt int64) {
	c.localCache.Store(key, sharedParamLocalEntry{
		params: params, height: height, cachedAt: time.Now(), final: final, seenAt: seenAt,
	})

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
	logger logging.Logger
	store  kv.Store
	// sharedClient says whether each at-height answer is final. Only a final
	// answer may reach L2, which every replica shares: an answer from a node that
	// has not committed the height yet can still change.
	sharedClient query.SharedFinalityClient
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
	store kv.Store,
	sharedClient query.SharedFinalityClient,
	blockClient client.BlockClient,
	config CacheConfig,
) *RedisSharedParamCache {
	if config.TTLBlocks == 0 {
		config.TTLBlocks = 1
	}
	if config.BlockTimeSeconds == 0 {
		config.BlockTimeSeconds = DefaultBlockTimeSeconds
	}
	// Below the floor is treated as unset, not honoured. A bare `LockTimeout: 5`
	// on a time.Duration field is FIVE NANOSECONDS -- go-redis truncates it to
	// PX 1 and the lock expires in about a millisecond, so it dedups nothing
	// while every reader still pays the contended path. It was wired exactly
	// that way in miner/leader_controller.go and the `== 0` check waved it
	// through, because an absurd value is not a zero one. Measured 2026-08-28.
	if config.LockTimeout < minLockTimeout {
		config.LockTimeout = 5 * time.Second
	}

	return &RedisSharedParamCache{
		localCache:   xsync.NewMap[string, sharedParamLocalEntry](),
		logger:       logging.ForComponent(logger, logging.ComponentSharedParamCache),
		store:        store,
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

	key := c.store.KB().ParamsSharedAtHeightKey(height)

	// L1: a final entry within the TTL floor, or one that is not final while no
	// newer block has been seen since it was stored.
	if e, ok := c.localCache.Load(key); ok {
		if e.final && time.Since(e.cachedAt) < sharedParamsLocalTTL || !e.final && c.reusableUntilNextBlock(e) {
			cacheHits.WithLabelValues("shared_params", CacheLevelL1).Inc()
			cacheGetLatency.WithLabelValues("shared_params", CacheLevelL1).Observe(time.Since(start).Seconds())
			return e.params, nil
		}
	}
	cacheMisses.WithLabelValues("shared_params", "l1").Inc()

	// L2: Check Redis cache
	data, err := c.store.Get(ctx, key)
	if err == nil {
		params := &sharedtypes.Params{}
		if unmarshalErr := json.Unmarshal(data, params); unmarshalErr != nil {
			c.logger.Warn().Err(unmarshalErr).Msg("failed to unmarshal cached params")
		} else {
			cacheHits.WithLabelValues("shared_params", CacheLevelL2).Inc()
			cacheGetLatency.WithLabelValues("shared_params", CacheLevelL2).Observe(time.Since(start).Seconds())
			// Store in L1; L2 only ever holds final answers.
			c.storeLocal(key, height, params, true, c.currentHeight())
			return params, nil
		}
	}
	if err != nil && !errors.Is(err, kv.ErrNotFound) {
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
	lockKey := c.store.KB().ParamsSharedAtHeightLockKey(height)

	// Try to acquire lock
	lockToken := newLockToken()
	locked, err := c.store.SetNX(ctx, lockKey, []byte(lockToken), c.config.LockTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}

	if locked {
		// We got the lock - query chain
		lockAcquisitions.WithLabelValues("shared_params", "acquired").Inc()
		defer releaseCacheLock(ctx, c.store, lockKey, lockToken)

		chainQueries.WithLabelValues("shared_params").Inc()
		chainStart := time.Now()

		seenAt := c.currentHeight()
		params, final, queryErr := c.sharedClient.GetParamsAtHeightWithFinality(ctx, height)
		chainQueryLatency.WithLabelValues("shared_params").Observe(time.Since(chainStart).Seconds())

		if queryErr != nil {
			chainQueryErrors.WithLabelValues("shared_params").Inc()
			return nil, fmt.Errorf("failed to query chain: %w", queryErr)
		}

		// Cache in Redis only a final answer: every replica reads it.
		if final {
			data, marshalErr := json.Marshal(params)
			if marshalErr == nil {
				ttl := c.config.BlocksToTTL(c.config.TTLBlocks)
				if cacheErr := c.store.Set(ctx, key, data, ttl); cacheErr != nil {
					c.logger.Warn().Err(cacheErr).Msg("failed to cache params in Redis")
				}
			}
		}

		// Cache in L1
		c.storeLocal(key, height, params, final, seenAt)

		return params, nil
	}

	// Another instance is populating - wait and retry from Redis
	lockAcquisitions.WithLabelValues("shared_params", "contended").Inc()
	time.Sleep(5 * time.Millisecond)

	retryData, retryErr := c.store.Get(ctx, key)
	if retryErr == nil {
		params := &sharedtypes.Params{}
		if unmarshalErr := json.Unmarshal(retryData, params); unmarshalErr == nil {
			cacheHits.WithLabelValues("shared_params", CacheLevelL2Retry).Inc()
			c.storeLocal(key, height, params, true, c.currentHeight())
			return params, nil
		}
	}

	// Still not available - query chain directly at the requested height. Not
	// written to L2 (the lock holder owns that key), but kept in this process's
	// L1: an answer that is not final never reaches L2, so without it every
	// lookup here would take this path again until the height is final.
	seenAt := c.currentHeight()
	params, final, fallbackErr := c.sharedClient.GetParamsAtHeightWithFinality(ctx, height)
	if fallbackErr != nil {
		chainQueryErrors.WithLabelValues("shared_params").Inc()
		return nil, fmt.Errorf("failed to query chain: %w", fallbackErr)
	}
	chainQueries.WithLabelValues("shared_params").Inc()
	c.storeLocal(key, height, params, final, seenAt)

	return params, nil
}

// currentHeight is this process's block height, 0 when it has no block client.
func (c *RedisSharedParamCache) currentHeight() int64 {
	if c.blockClient == nil {
		return 0
	}
	block := c.blockClient.LastBlock(context.Background())
	if block == nil {
		return 0
	}
	return block.Height()
}

// reusableUntilNextBlock reports whether an entry that is not final may still be
// served: no block past the one it was asked for at has been seen, for at most
// maxPendingLocalReuse.
func (c *RedisSharedParamCache) reusableUntilNextBlock(e sharedParamLocalEntry) bool {
	h := c.currentHeight()
	return h > 0 && h <= e.seenAt && time.Since(e.cachedAt) < maxPendingLocalReuse
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
	key := c.store.KB().ParamsSharedAtHeightKey(height)
	data, err := c.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, kv.ErrNotFound) {
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

	c.storeLocal(key, height, params, true, height)
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
