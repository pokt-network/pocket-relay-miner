package cache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/cosmos/gogoproto/proto"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

const (
	// Cache type for pub/sub and metrics
	proofParamsCacheType = "proof_params"
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
//
// TODO(mid-session-invalidation): Same as shared_params_singleton.go.
// Proof params are cached for 1 session duration and should be invalidated
// if governance changes params mid-session. See shared_params_singleton.go for details.
type proofParamsCache struct {
	logger           logging.Logger
	redisClient      *redisutil.Client
	queryClient      ProofQueryClient
	sharedClient     ProofSharedQueryClient
	blockTimeSeconds int64

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

// ProofSharedQueryClient defines the interface for querying shared module parameters.
type ProofSharedQueryClient interface {
	// GetParams queries the shared module parameters from the chain.
	GetParams(ctx context.Context) (*sharedtypes.Params, error)
}

// NewProofParamsCache creates a new proof params cache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
//
// blockTimeSeconds is the expected block time (e.g., 30s for mainnet, 10s for localnet).
// TTL is calculated as: num_blocks_per_session (from shared params) × blockTimeSeconds
func NewProofParamsCache(
	logger logging.Logger,
	redisClient *redisutil.Client,
	queryClient ProofQueryClient,
	sharedClient ProofSharedQueryClient,
	blockTimeSeconds int64,
) SingletonEntityCache[*prooftypes.Params] {
	if blockTimeSeconds <= 0 {
		blockTimeSeconds = defaultBlockTimeSeconds
	}

	return &proofParamsCache{
		logger:           logging.ForComponent(logger, logging.ComponentSharedParamCache),
		redisClient:      redisClient,
		queryClient:      queryClient,
		sharedClient:     sharedClient,
		blockTimeSeconds: blockTimeSeconds,
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

// Get retrieves proof params using L1 → L2 → L3 fallback pattern.
// If force=true, bypasses L1/L2 cache, queries L3 (chain), stores in L2+L1, and publishes invalidation.
// This is used by the leader's Refresh() to ensure fresh data on every block.
func (c *proofParamsCache) Get(ctx context.Context, force ...bool) (*prooftypes.Params, error) {
	start := time.Now()
	forceRefresh := len(force) > 0 && force[0]

	if !forceRefresh {
		// L1: Check local cache (atomic pointer)
		if params := c.localCache.Load(); params != nil {
			cacheHits.WithLabelValues(proofParamsCacheType, CacheLevelL1).Inc()
			cacheGetLatency.WithLabelValues(proofParamsCacheType, CacheLevelL1).Observe(time.Since(start).Seconds())
			c.logger.Debug().Msg("proof params cache hit (L1)")
			return params, nil
		}

		// L2: Check Redis cache
		data, err := c.redisClient.Get(ctx, c.redisClient.KB().ParamsProofKey()).Bytes()
		if err == nil {
			params := &prooftypes.Params{} // CRITICAL FIX: Allocate on heap, not stack
			if err := proto.Unmarshal(data, params); err == nil {
				// Store in L1 for next time
				c.localCache.Store(params)
				cacheHits.WithLabelValues(proofParamsCacheType, CacheLevelL2).Inc()
				cacheGetLatency.WithLabelValues(proofParamsCacheType, CacheLevelL2).Observe(time.Since(start).Seconds())

				c.logger.Debug().Msg("proof params cache hit (L2) → stored in L1")

				return params, nil
			} else {
				c.logger.Warn().
					Err(err).
					Msg("failed to unmarshal proof params from Redis")
			}
		}
	}

	// L3: Query chain (force=true bypasses distributed lock since leader refreshes serially)
	var params *prooftypes.Params
	var err error

	if forceRefresh {
		// Leader force refresh: Direct query without lock
		chainQueries.WithLabelValues("proof_params").Inc()
		chainStart := time.Now()
		params, err = c.queryClient.GetParams(ctx)
		chainQueryLatency.WithLabelValues("proof_params").Observe(time.Since(chainStart).Seconds())

		if err != nil {
			chainQueryErrors.WithLabelValues("proof_params").Inc()
			cacheMisses.WithLabelValues(proofParamsCacheType, "l3_error").Inc()
			cacheGetLatency.WithLabelValues(proofParamsCacheType, "l3_error").Observe(time.Since(start).Seconds())
			return nil, fmt.Errorf("failed to query proof params: %w", err)
		}
	} else {
		// Normal lazy load: Use distributed lock to prevent duplicate queries
		params, err = c.queryChainWithLock(ctx)
		if err != nil {
			cacheMisses.WithLabelValues(proofParamsCacheType, "l3_error").Inc()
			cacheGetLatency.WithLabelValues(proofParamsCacheType, "l3_error").Observe(time.Since(start).Seconds())
			return nil, fmt.Errorf("failed to query proof params: %w", err)
		}
	}

	// Store in L2 and L1 with dynamically calculated TTL
	// Use already-cached shared params to avoid extra gRPC call
	ttl := c.calculateTTLFromSharedParams()
	if err := c.Set(ctx, params, ttl); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to cache proof params after L3 query")
	} else {
		if forceRefresh {
			c.logger.Debug().
				Dur("ttl", ttl).
				Msg("proof params force refreshed from chain → stored in L1 and L2")
		} else {
			c.logger.Debug().
				Dur("ttl", ttl).
				Msg("proof params cache miss (L3) → stored in L1 and L2")
		}
	}

	// Publish invalidation event if force refresh (leader only)
	if forceRefresh {
		payload := "{}"
		if err := PublishInvalidation(ctx, c.redisClient, c.logger, proofParamsCacheType, payload); err != nil {
			c.logger.Warn().
				Err(err).
				Msg("failed to publish invalidation event after force refresh")
		}
	}

	cacheMisses.WithLabelValues(proofParamsCacheType, CacheLevelL3).Inc()
	cacheGetLatency.WithLabelValues(proofParamsCacheType, CacheLevelL3).Observe(time.Since(start).Seconds())

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

	if err := c.redisClient.Set(ctx, c.redisClient.KB().ParamsProofKey(), data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set Redis cache: %w", err)
	}

	c.logger.Debug().
		Dur("ttl", ttl).
		Msg("proof params cached")

	return nil
}

// Refresh updates the cache from the chain (called by leader only).
func (c *proofParamsCache) Refresh(ctx context.Context) error {
	// Force refresh: bypass L1/L2, query L3, store in L2+L1, publish invalidation
	_, err := c.Get(ctx, true)
	return err
}

// InvalidateAll clears the entire cache (both L1 and L2).
func (c *proofParamsCache) InvalidateAll(ctx context.Context) error {
	// Remove from L1 (local cache)
	c.localCache.Store(nil)

	// Remove from L2 (Redis)
	if err := c.redisClient.Del(ctx, c.redisClient.KB().ParamsProofKey()).Err(); err != nil {
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
	data, err := c.redisClient.Get(ctx, c.redisClient.KB().ParamsProofKey()).Bytes()
	if err != nil {
		// Key doesn't exist in Redis, skip warmup
		c.logger.Debug().Msg("no proof params in Redis to warm up")
		return nil
	}

	params := &prooftypes.Params{} // CRITICAL FIX: Allocate on heap, not stack
	if err := proto.Unmarshal(data, params); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to unmarshal proof params during warmup")
		return err
	}

	c.localCache.Store(params)

	c.logger.Info().Msg("proof params cache warmup complete")

	return nil
}

// queryChainWithLock queries the chain with distributed locking to prevent
// duplicate queries from multiple instances.
func (c *proofParamsCache) queryChainWithLock(ctx context.Context) (*prooftypes.Params, error) {
	lockKey := c.redisClient.KB().ParamsProofLockKey()
	// Try to acquire distributed lock
	locked, err := c.redisClient.SetNX(ctx, lockKey, "1", 5*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer c.redisClient.Del(ctx, lockKey)

	if !locked {
		// Another instance is querying, wait and retry L2
		c.logger.Debug().Msg("another instance is querying proof params, waiting")
		time.Sleep(5 * time.Millisecond)

		// Retry L2 after waiting
		data, err := c.redisClient.Get(ctx, c.redisClient.KB().ParamsProofKey()).Bytes()
		if err == nil {
			params := &prooftypes.Params{} // CRITICAL FIX: Allocate on heap, not stack
			if err := proto.Unmarshal(data, params); err == nil {
				cacheHits.WithLabelValues(proofParamsCacheType, CacheLevelL2Retry).Inc()
				return params, nil
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

// calculateTTLFromSharedParams returns a fixed TTL for proof params.
// This is just a Redis key expiration safety net - actual refresh frequency
// is controlled by CacheOrchestratorConfig.RefreshIntervalBlocks.
// Uses 10-minute TTL which is long enough for any reasonable refresh interval.
func (c *proofParamsCache) calculateTTLFromSharedParams() time.Duration {
	// 10-minute TTL: just a safety net for Redis key expiration
	// Actual refresh is controlled by RefreshIntervalBlocks (operator configurable)
	// If leader dies, new leader refreshes immediately on election
	return 10 * time.Minute
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *proofParamsCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received proof params invalidation event")

	// Clear L1 (local cache)
	c.localCache.Store(nil)

	cacheInvalidations.WithLabelValues(proofParamsCacheType, SourcePubSub).Inc()

	// Eagerly reload from L2 (Redis) to avoid cold cache on next relay
	// This eliminates the latency penalty on the first relay after invalidation
	data, err := c.redisClient.Get(ctx, c.redisClient.KB().ParamsProofKey()).Bytes()
	if err == nil {
		params := &prooftypes.Params{}
		if err := proto.Unmarshal(data, params); err == nil {
			// Warm L1 cache immediately
			c.localCache.Store(params)
			c.logger.Debug().Msg("eagerly reloaded proof params from L2 into L1")
			return nil
		} else {
			c.logger.Warn().
				Err(err).
				Msg("failed to unmarshal proof params during eager reload")
		}
	} else if err != redis.Nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to eagerly reload proof params from L2")
	}

	return nil
}
