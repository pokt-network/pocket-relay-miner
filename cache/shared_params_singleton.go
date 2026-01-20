package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/cosmos/gogoproto/proto"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

const (
	// Cache type for pub/sub and metrics
	sharedParamsCacheType = "shared_params"

	// Default block time if not configured (30 seconds for mainnet/testnet)
	defaultBlockTimeSeconds = 30

	// Number of sessions to keep params cached (safety buffer)
	sessionTTLMultiplier = 2
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
//
// TTL is calculated dynamically as: 2 × session_duration × block_time
// This ensures params stay cached across multiple sessions while being
// refreshed frequently enough to pick up governance changes.
//
// TODO(mid-session-invalidation): Add governance event monitoring for param changes.
// Currently params are cached for the full session duration (2 sessions for shared params).
// If governance changes params mid-session, relayers continue using cached values until
// session boundary. This is acceptable because:
// 1. Governance changes are rare and typically planned for session boundaries
// 2. Impact is limited to single session duration (~10 blocks mainnet = ~5 minutes)
// 3. Emergency invalidation is available via cache pub/sub (PublishInvalidation)
//
// To implement proper mid-session invalidation:
// - Monitor governance proposal execution events
// - Detect param module updates
// - Publish invalidation to "ha:events:cache:shared_params:invalidate"
// - All instances clear L1 and query fresh from chain or L2
type sharedParamsCache struct {
	logger           logging.Logger
	redisClient      *redisutil.Client
	queryClient      client.SharedQueryClient
	blockTimeSeconds int64

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
//
// blockTimeSeconds is the expected block time (e.g., 30s for mainnet, 10s for localnet).
// TTL is calculated as: 2 × num_blocks_per_session (from shared params) × blockTimeSeconds
func NewSharedParamsCache(
	logger logging.Logger,
	redisClient *redisutil.Client,
	queryClient client.SharedQueryClient,
	blockTimeSeconds int64,
) SingletonEntityCache[*sharedtypes.Params] {
	if blockTimeSeconds <= 0 {
		blockTimeSeconds = defaultBlockTimeSeconds
	}

	return &sharedParamsCache{
		logger:           logging.ForComponent(logger, logging.ComponentSharedParamCache),
		redisClient:      redisClient,
		queryClient:      queryClient,
		blockTimeSeconds: blockTimeSeconds,
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

// Get retrieves shared params using L1 → L2 → L3 fallback pattern.
// If force=true, bypasses L1/L2 cache, queries L3 (chain), stores in L2+L1, and publishes invalidation.
// This is used by the leader's Refresh() to ensure fresh data on every block.
func (c *sharedParamsCache) Get(ctx context.Context, force ...bool) (*sharedtypes.Params, error) {
	forceRefresh := len(force) > 0 && force[0]

	if !forceRefresh {
		// L1: Check local cache (atomic pointer)
		if params := c.localCache.Load(); params != nil {
			cacheHits.WithLabelValues(sharedParamsCacheType, CacheLevelL1).Inc()
			c.logger.Debug().Msg("shared params cache hit (L1)")
			return params, nil
		}

		// L2: Check Redis cache
		data, err := c.redisClient.Get(ctx, c.redisClient.KB().ParamsSharedCacheKey()).Bytes()
		if err == nil {
			params := &sharedtypes.Params{}
			if err = proto.Unmarshal(data, params); err == nil {
				// Store in L1 for next time
				c.localCache.Store(params)
				cacheHits.WithLabelValues(sharedParamsCacheType, CacheLevelL2).Inc()

				c.logger.Debug().Msg("shared params cache hit (L2) → stored in L1")

				return params, nil
			}

			c.logger.Warn().
				Err(err).
				Msg("failed to unmarshal shared params from Redis")
		}
	}

	// L3: Query chain (force=true bypasses distributed lock since leader refreshes serially)
	var params *sharedtypes.Params
	var err error

	if forceRefresh {
		// Leader force refresh: Direct query without lock
		chainQueries.WithLabelValues("shared_params").Inc()
		chainStart := time.Now()
		params, err = c.queryClient.GetParams(ctx)
		chainQueryLatency.WithLabelValues("shared_params").Observe(time.Since(chainStart).Seconds())

		if err != nil {
			chainQueryErrors.WithLabelValues("shared_params").Inc()
			cacheMisses.WithLabelValues(sharedParamsCacheType, "l3_error").Inc()
			return nil, fmt.Errorf("failed to query shared params: %w", err)
		}
	} else {
		// Normal lazy load: Use distributed lock to prevent duplicate queries
		params, err = c.queryChainWithLock(ctx)
		if err != nil {
			cacheMisses.WithLabelValues(sharedParamsCacheType, "l3_error").Inc()
			return nil, fmt.Errorf("failed to query shared params: %w", err)
		}
	}

	// Store in L2 and L1 with dynamically calculated TTL
	// Use the params we just fetched instead of querying again
	ttl := c.calculateTTLFromParams(params)

	if err = c.Set(ctx, params, ttl); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to cache shared params after L3 query")
	} else {
		if forceRefresh {
			c.logger.Debug().
				Dur("ttl", ttl).
				Msg("shared params force refreshed from chain → stored in L1 and L2")
		} else {
			c.logger.Debug().
				Dur("ttl", ttl).
				Msg("shared params cache miss (L3) → stored in L1 and L2")
		}
	}

	// Publish the invalidation event if force refresh (leader only)
	if forceRefresh {
		payload := "{}"
		if err := PublishInvalidation(ctx, c.redisClient, c.logger, sharedParamsCacheType, payload); err != nil {
			c.logger.Warn().
				Err(err).
				Msg("failed to publish invalidation event after force refresh")
		}
	}

	cacheMisses.WithLabelValues(sharedParamsCacheType, "l3").Inc()

	return params, nil
}

// Set stores the shared params in both L1 and L2 caches.
func (c *sharedParamsCache) Set(ctx context.Context, params *sharedtypes.Params, ttl time.Duration) error {
	// L1: Store in a local cache
	c.localCache.Store(params)

	// L2: Store in Redis (proto marshaling)
	data, err := proto.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to marshal shared params: %w", err)
	}

	if err := c.redisClient.Set(ctx, c.redisClient.KB().ParamsSharedCacheKey(), data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set Redis cache: %w", err)
	}

	c.logger.Debug().
		Dur("ttl", ttl).
		Msg("shared params cached")

	return nil
}

// Refresh updates the cache from the chain (called by leader only).
func (c *sharedParamsCache) Refresh(ctx context.Context) error {
	// Force refresh: bypass L1/L2, query L3, store in L2+L1, publish invalidation
	_, err := c.Get(ctx, true)
	return err
}

// InvalidateAll clears the entire cache (both L1 and L2).
func (c *sharedParamsCache) InvalidateAll(ctx context.Context) error {
	// Clear L1 (local cache)
	c.localCache.Store(nil)

	// Clear L2 (Redis)
	if err := c.redisClient.Del(ctx, c.redisClient.KB().ParamsSharedCacheKey()).Err(); err != nil {
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
	data, err := c.redisClient.Get(ctx, c.redisClient.KB().ParamsSharedCacheKey()).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// Not in cache yet, that's OK
			c.logger.Debug().Msg("shared params not in Redis, will be loaded on first query")
			return nil
		}
		c.logger.Warn().Err(err).Msg("failed to get shared params from Redis during warmup")
		return err
	}

	// Unmarshal and store in L1
	params := &sharedtypes.Params{} // CRITICAL FIX: Allocate on heap, not stack
	if err := proto.Unmarshal(data, params); err != nil {
		c.logger.Warn().Err(err).Msg("failed to unmarshal shared params during warmup")
		return err
	}

	c.localCache.Store(params)
	c.logger.Info().Msg("shared params cache warmup complete")

	return nil
}

// queryChainWithLock queries the chain with distributed locking to prevent
// duplicate queries from multiple instances.
func (c *sharedParamsCache) queryChainWithLock(ctx context.Context) (*sharedtypes.Params, error) {
	lockKey := c.redisClient.KB().ParamsSharedLockKey()
	// Try to acquire distributed lock
	locked, err := c.redisClient.SetNX(ctx, lockKey, "1", 5*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer c.redisClient.Del(ctx, lockKey)

	if !locked {
		// Another instance is querying, wait and retry L2
		c.logger.Debug().Msg("another instance is querying shared params, waiting")
		time.Sleep(5 * time.Millisecond)

		// Retry L2 after waiting
		data, err := c.redisClient.Get(ctx, c.redisClient.KB().ParamsSharedCacheKey()).Bytes()
		if err == nil {
			params := &sharedtypes.Params{} // CRITICAL FIX: Allocate on heap, not stack
			if err := proto.Unmarshal(data, params); err == nil {
				c.localCache.Store(params)
				cacheHits.WithLabelValues(sharedParamsCacheType, CacheLevelL2Retry).Inc()
				return params, nil
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

// calculateTTLFromParams calculates the TTL from already-fetched params.
// This avoids an extra gRPC call during refresh.
// Formula: TTL = 2 × num_blocks_per_session × block_time_seconds
func (c *sharedParamsCache) calculateTTLFromParams(params *sharedtypes.Params) time.Duration {
	if params == nil {
		return 10 * time.Minute // Default fallback
	}

	numBlocksPerSession := params.NumBlocksPerSession
	ttlSeconds := int64(sessionTTLMultiplier*numBlocksPerSession) * c.blockTimeSeconds

	c.logger.Debug().
		Uint64("blocks_per_session", numBlocksPerSession).
		Int64("block_time_seconds", c.blockTimeSeconds).
		Int64("ttl_seconds", ttlSeconds).
		Msg("calculated shared params TTL")

	return time.Duration(ttlSeconds) * time.Second
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *sharedParamsCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received shared params invalidation event")

	// Clear L1 (local cache)
	c.localCache.Store(nil)

	cacheInvalidations.WithLabelValues(sharedParamsCacheType, SourcePubSub).Inc()

	// Eagerly reload from L2 (Redis) to avoid cold cache on next relay
	// This eliminates the latency penalty on the first relay after invalidation
	data, err := c.redisClient.Get(ctx, c.redisClient.KB().ParamsSharedCacheKey()).Bytes()
	if err == nil {
		params := &sharedtypes.Params{}
		if err := proto.Unmarshal(data, params); err == nil {
			// Warm L1 cache immediately
			c.localCache.Store(params)
			c.logger.Debug().Msg("eagerly reloaded shared params from L2 into L1")
			return nil
		} else {
			c.logger.Warn().
				Err(err).
				Msg("failed to unmarshal shared params during eager reload")
		}
	} else if err != redis.Nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to eagerly reload shared params from L2")
	}

	return nil
}
