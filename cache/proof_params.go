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
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

const (
	// Redis key for proof params cache
	proofParamsRedisKey = "ha:cache:proof_params"

	// Lock key for distributed locking during L3 query
	proofParamsLockKey = "ha:cache:lock:proof_params"

	// Cache type for pub/sub and metrics
	proofParamsCacheType = "proof_params"

	// Default TTL for proof params cache (10 minutes)
	proofParamsCacheTTL = 10 * time.Minute
)

// proofParamsCache implements SingletonEntityCache[*prooftypes.Params]
// for caching proof module parameters.
//
// Cache levels:
// - L1: In-memory cache using atomic.Pointer for lock-free reads
// - L2: Redis cache with proto marshaling
// - L3: Chain query via ProofQueryClient
//
// The cache subscribes to pub/sub invalidation events to stay synchronized
// across all instances.
type proofParamsCache struct {
	logger      logging.Logger
	redisClient redis.UniversalClient
	queryClient ProofQueryClient

	// L1: In-memory cache (atomic for lock-free reads)
	localCache atomic.Pointer[prooftypes.Params]

	// Pub/sub
	pubsub *redis.PubSub

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// ProofQueryClient defines the interface for querying proof module parameters.
type ProofQueryClient interface {
	// GetParams queries the proof module parameters from the chain.
	GetParams(ctx context.Context) (*prooftypes.Params, error)
}

// NewProofParamsCache creates a new proof params cache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
func NewProofParamsCache(
	logger logging.Logger,
	redisClient redis.UniversalClient,
	queryClient ProofQueryClient,
) SingletonEntityCache[*prooftypes.Params] {
	return &proofParamsCache{
		logger:      logging.ForComponent(logger, logging.ComponentSharedParamCache),
		redisClient: redisClient,
		queryClient: queryClient,
	}
}

// Start initializes the cache and subscribes to pub/sub invalidation events.
func (c *proofParamsCache) Start(ctx context.Context) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	// Subscribe to invalidation events
	if err := SubscribeToInvalidations(
		c.ctx,
		c.redisClient,
		c.logger,
		proofParamsCacheType,
		c.handleInvalidation,
	); err != nil {
		return fmt.Errorf("failed to subscribe to invalidations: %w", err)
	}

	c.logger.Info().Msg("proof params cache started")

	return nil
}

// Close gracefully shuts down the cache.
func (c *proofParamsCache) Close() error {
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.wg.Wait()

	if c.pubsub != nil {
		_ = c.pubsub.Close()
	}

	c.logger.Info().Msg("proof params cache stopped")

	return nil
}

// Get retrieves the proof params using L1 → L2 → L3 fallback pattern.
func (c *proofParamsCache) Get(ctx context.Context) (*prooftypes.Params, error) {
	// L1: Check local cache (atomic pointer)
	if params := c.localCache.Load(); params != nil {
		cacheHits.WithLabelValues(proofParamsCacheType, CacheLevelL1).Inc()
		return params, nil
	}

	// L2: Check Redis cache
	data, err := c.redisClient.Get(ctx, proofParamsRedisKey).Bytes()
	if err == nil {
		var params prooftypes.Params
		if err := proto.Unmarshal(data, &params); err == nil {
			// Store in L1 for next time
			c.localCache.Store(&params)
			cacheHits.WithLabelValues(proofParamsCacheType, CacheLevelL2).Inc()

			c.logger.Debug().Msg("proof params cache hit (L2)")

			return &params, nil
		} else {
			c.logger.Warn().
				Err(err).
				Msg("failed to unmarshal proof params from Redis")
		}
	}

	// L3: Query chain with distributed lock
	params, err := c.queryChainWithLock(ctx)
	if err != nil {
		cacheMisses.WithLabelValues(proofParamsCacheType, "l3_error").Inc()
		return nil, fmt.Errorf("failed to query proof params: %w", err)
	}

	// Store in L2 and L1
	if err := c.Set(ctx, params, proofParamsCacheTTL); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to cache proof params after L3 query")
	}

	cacheMisses.WithLabelValues(proofParamsCacheType, "l3").Inc()
	c.logger.Debug().Msg("proof params cache miss (L3)")

	return params, nil
}

// Set stores the proof params in both L1 and L2 caches.
func (c *proofParamsCache) Set(ctx context.Context, params *prooftypes.Params, ttl time.Duration) error {
	// L1: Store in local cache
	c.localCache.Store(params)

	// L2: Store in Redis (proto marshaling)
	data, err := proto.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to marshal proof params: %w", err)
	}

	if err := c.redisClient.Set(ctx, proofParamsRedisKey, data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set Redis cache: %w", err)
	}

	c.logger.Debug().
		Dur("ttl", ttl).
		Msg("proof params cached")

	return nil
}

// Refresh updates the cache from the chain (called by leader only).
func (c *proofParamsCache) Refresh(ctx context.Context) error {
	start := time.Now()

	params, err := c.queryClient.GetParams(ctx)
	if err != nil {
		cacheRefreshErrors.WithLabelValues(proofParamsCacheType).Inc()
		return fmt.Errorf("failed to query proof params: %w", err)
	}

	// Store in L2 and L1
	if err := c.Set(ctx, params, proofParamsCacheTTL); err != nil {
		cacheRefreshErrors.WithLabelValues(proofParamsCacheType).Inc()
		return fmt.Errorf("failed to cache proof params: %w", err)
	}

	// Publish invalidation event to notify other instances
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, proofParamsCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event after refresh")
	}

	cacheRefreshDuration.WithLabelValues(proofParamsCacheType).Observe(time.Since(start).Seconds())

	c.logger.Debug().
		Dur("duration", time.Since(start)).
		Msg("proof params cache refreshed")

	return nil
}

// InvalidateAll clears the entire cache (both L1 and L2).
func (c *proofParamsCache) InvalidateAll(ctx context.Context) error {
	// Remove from L1 (local cache)
	c.localCache.Store(nil)

	// Remove from L2 (Redis)
	if err := c.redisClient.Del(ctx, proofParamsRedisKey).Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to delete proof params from Redis")
	}

	// Publish invalidation event to other instances
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, proofParamsCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(proofParamsCacheType, SourceManual).Inc()

	c.logger.Info().Msg("proof params cache invalidated")

	return nil
}

// WarmupFromRedis populates L1 cache from Redis on startup.
func (c *proofParamsCache) WarmupFromRedis(ctx context.Context) error {
	c.logger.Info().Msg("warming up proof params cache from Redis")

	// Load from Redis (L2) into local cache (L1)
	data, err := c.redisClient.Get(ctx, proofParamsRedisKey).Bytes()
	if err != nil {
		// Key doesn't exist in Redis, skip warmup
		c.logger.Debug().Msg("no proof params in Redis to warm up")
		return nil
	}

	var params prooftypes.Params
	if err := proto.Unmarshal(data, &params); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to unmarshal proof params during warmup")
		return err
	}

	c.localCache.Store(&params)

	c.logger.Info().Msg("proof params cache warmup complete")

	return nil
}

// queryChainWithLock queries the chain with distributed locking to prevent
// duplicate queries from multiple instances.
func (c *proofParamsCache) queryChainWithLock(ctx context.Context) (*prooftypes.Params, error) {
	// Try to acquire distributed lock
	locked, err := c.redisClient.SetNX(ctx, proofParamsLockKey, "1", 5*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer c.redisClient.Del(ctx, proofParamsLockKey)

	if !locked {
		// Another instance is querying, wait and retry L2
		c.logger.Debug().Msg("another instance is querying proof params, waiting")
		time.Sleep(100 * time.Millisecond)

		// Retry L2 after waiting
		data, err := c.redisClient.Get(ctx, proofParamsRedisKey).Bytes()
		if err == nil {
			var params prooftypes.Params
			if err := proto.Unmarshal(data, &params); err == nil {
				cacheHits.WithLabelValues(proofParamsCacheType, CacheLevelL2Retry).Inc()
				return &params, nil
			}
		}

		// If still not in Redis, query chain anyway
	}

	// Query chain
	chainQueries.WithLabelValues("proof_params").Inc()

	params, err := c.queryClient.GetParams(ctx)
	if err != nil {
		chainQueryErrors.WithLabelValues("proof_params").Inc()
		return nil, err
	}

	return params, nil
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *proofParamsCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received proof params invalidation event")

	// Remove from L1 (local cache)
	c.localCache.Store(nil)

	cacheInvalidations.WithLabelValues(proofParamsCacheType, SourcePubSub).Inc()

	return nil
}
