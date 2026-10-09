package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

var _ SupplierParamCache = (*RedisSupplierParamCache)(nil)

// RedisSupplierParamCache implements SupplierParamCache using Redis as L2 cache.
type RedisSupplierParamCache struct {
	logger         logging.Logger
	store          kv.Store
	supplierClient client.SupplierQueryClient
	config         CacheConfig

	// L1 local cache (singleton params, not keyed by height)
	localCache    *suppliertypes.Params
	localCacheMu  sync.RWMutex
	localCacheSet bool

	// Cache keys helper

	// Lifecycle
	mu       sync.RWMutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewRedisSupplierParamCache creates a new SupplierParamCache backed by Redis.
func NewRedisSupplierParamCache(
	logger logging.Logger,
	store kv.Store,
	supplierClient client.SupplierQueryClient,
	config CacheConfig,
) *RedisSupplierParamCache {
	if config.TTLBlocks == 0 {
		config.TTLBlocks = 100 // Supplier params rarely change
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

	return &RedisSupplierParamCache{
		logger:         logging.ForComponent(logger, logging.ComponentSupplierParamCache),
		store:          store,
		supplierClient: supplierClient,
		config:         config,
	}
}

// Start begins the cache's background processes.
func (c *RedisSupplierParamCache) Start(ctx context.Context) error {
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

	c.logger.Info().Msg("supplier param cache started")
	return nil
}

// subscribeToInvalidations listens for cache invalidation events from other
// instances, subscribing again whenever the subscription fails or is lost.
func (c *RedisSupplierParamCache) subscribeToInvalidations(ctx context.Context) {
	defer c.wg.Done()
	redisutil.NewReconnectionLoop(c.logger, "pubsub_supplier_params", c.store.Ping, c.runInvalidations).Run(ctx)
}

func (c *RedisSupplierParamCache) runInvalidations(ctx context.Context) error {
	channel := c.store.KB().SupplierParamsInvalidateChannel()
	sub, err := c.store.Subscribe(ctx, channel)
	if err != nil {
		return fmt.Errorf("failed to subscribe to supplier params invalidations: %w", err)
	}
	defer func() { _ = sub.Close() }()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-sub.Messages():
			if !ok {
				return fmt.Errorf("supplier params invalidation subscription closed")
			}
			// Clear local cache
			c.localCacheMu.Lock()
			c.localCache = nil
			c.localCacheSet = false
			c.localCacheMu.Unlock()

			cacheInvalidations.WithLabelValues("supplier_params", "pubsub").Inc()
			c.logger.Debug().Msg("supplier params cache invalidated via pub/sub")
		}
	}
}

// GetSupplierParams returns the supplier module parameters.
// Supplier params are global (not per-height), so we cache as singleton.
func (c *RedisSupplierParamCache) GetSupplierParams(ctx context.Context) (*suppliertypes.Params, error) {
	start := time.Now()

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, fmt.Errorf("cache is closed")
	}
	c.mu.RUnlock()

	// L1: Check local cache
	c.localCacheMu.RLock()
	if c.localCacheSet && c.localCache != nil {
		cached := c.localCache
		c.localCacheMu.RUnlock()
		cacheHits.WithLabelValues("supplier_params", CacheLevelL1).Inc()
		cacheGetLatency.WithLabelValues("supplier_params", CacheLevelL1).Observe(time.Since(start).Seconds())
		return cached, nil
	}
	c.localCacheMu.RUnlock()
	cacheMisses.WithLabelValues("supplier_params", "l1").Inc()

	// L2: Check Redis cache
	key := c.store.KB().ParamsSupplierKey()
	data, err := c.store.Get(ctx, key)
	if err == nil {
		params := &suppliertypes.Params{}
		if unmarshalErr := json.Unmarshal(data, params); unmarshalErr != nil {
			c.logger.Warn().Err(unmarshalErr).Msg("failed to unmarshal cached supplier params")
		} else {
			cacheHits.WithLabelValues("supplier_params", CacheLevelL2).Inc()
			cacheGetLatency.WithLabelValues("supplier_params", CacheLevelL2).Observe(time.Since(start).Seconds())
			// Store in L1
			c.localCacheMu.Lock()
			c.localCache = params
			c.localCacheSet = true
			c.localCacheMu.Unlock()
			return params, nil
		}
	}
	if err != nil && !errors.Is(err, kv.ErrNotFound) {
		c.logger.Warn().Err(err).Msg("error fetching supplier params from Redis cache")
	}
	cacheMisses.WithLabelValues("supplier_params", "l2").Inc()

	// L3: Query chain with distributed lock
	params, err := c.queryAndCacheParams(ctx, key)
	if err != nil {
		cacheGetLatency.WithLabelValues("supplier_params", "l3_error").Observe(time.Since(start).Seconds())
		return nil, err
	}
	cacheGetLatency.WithLabelValues("supplier_params", CacheLevelL3).Observe(time.Since(start).Seconds())
	return params, nil
}

// queryAndCacheParams queries the chain and caches the result.
// Uses distributed locking to prevent thundering herd.
func (c *RedisSupplierParamCache) queryAndCacheParams(ctx context.Context, key string) (*suppliertypes.Params, error) {
	lockKey := c.store.KB().ParamsSupplierLockKey()

	// Try to acquire lock
	lockToken := newLockToken()
	locked, err := c.store.SetNX(ctx, lockKey, []byte(lockToken), c.config.LockTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}

	if locked {
		// We got the lock - query chain
		lockAcquisitions.WithLabelValues("supplier_params", "acquired").Inc()
		defer releaseCacheLock(ctx, c.store, lockKey, lockToken)

		chainQueries.WithLabelValues("supplier_params").Inc()
		chainStart := time.Now()

		params, queryErr := c.supplierClient.GetParams(ctx)
		chainQueryLatency.WithLabelValues("supplier_params").Observe(time.Since(chainStart).Seconds())

		if queryErr != nil {
			chainQueryErrors.WithLabelValues("supplier_params").Inc()
			return nil, fmt.Errorf("failed to query chain: %w", queryErr)
		}

		// Cache in Redis
		data, marshalErr := json.Marshal(params)
		if marshalErr == nil {
			ttl := c.config.BlocksToTTL(c.config.TTLBlocks)
			if cacheErr := c.store.Set(ctx, key, data, ttl); cacheErr != nil {
				c.logger.Warn().Err(cacheErr).Msg("failed to cache supplier params in Redis")
			}
		}

		// Cache in L1
		c.localCacheMu.Lock()
		c.localCache = params
		c.localCacheSet = true
		c.localCacheMu.Unlock()

		return params, nil
	}

	// Another instance is populating - wait and retry from Redis
	lockAcquisitions.WithLabelValues("supplier_params", "contended").Inc()
	time.Sleep(5 * time.Millisecond)

	retryData, retryErr := c.store.Get(ctx, key)
	if retryErr == nil {
		params := &suppliertypes.Params{}
		if unmarshalErr := json.Unmarshal(retryData, params); unmarshalErr == nil {
			cacheHits.WithLabelValues("supplier_params", CacheLevelL2Retry).Inc()
			c.localCacheMu.Lock()
			c.localCache = params
			c.localCacheSet = true
			c.localCacheMu.Unlock()
			return params, nil
		}
	}

	// Still not available - query chain directly
	params, fallbackErr := c.supplierClient.GetParams(ctx)
	if fallbackErr != nil {
		chainQueryErrors.WithLabelValues("supplier_params").Inc()
		return nil, fmt.Errorf("failed to query chain: %w", fallbackErr)
	}
	chainQueries.WithLabelValues("supplier_params").Inc()

	return params, nil
}

// Refresh updates the cache from the chain (called by leader only).
// Forces a fresh query and publishes invalidation to other instances.
func (c *RedisSupplierParamCache) Refresh(ctx context.Context) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("cache is closed")
	}
	c.mu.RUnlock()

	// Query chain directly (bypass cache)
	chainQueries.WithLabelValues("supplier_params").Inc()
	chainStart := time.Now()

	params, err := c.supplierClient.GetParams(ctx)
	chainQueryLatency.WithLabelValues("supplier_params").Observe(time.Since(chainStart).Seconds())

	if err != nil {
		chainQueryErrors.WithLabelValues("supplier_params").Inc()
		return fmt.Errorf("failed to query chain: %w", err)
	}

	// Update L1 cache
	c.localCacheMu.Lock()
	c.localCache = params
	c.localCacheSet = true
	c.localCacheMu.Unlock()

	// Update L2 cache (Redis)
	key := c.store.KB().ParamsSupplierKey()
	data, marshalErr := json.Marshal(params)
	if marshalErr == nil {
		ttl := c.config.BlocksToTTL(c.config.TTLBlocks)
		if cacheErr := c.store.Set(ctx, key, data, ttl); cacheErr != nil {
			c.logger.Warn().Err(cacheErr).Msg("failed to cache supplier params in Redis")
		}
	}

	// Publish invalidation to other instances so they clear L1 and reload from L2
	channel := c.store.KB().SupplierParamsInvalidateChannel()
	if err := c.store.Publish(ctx, channel, []byte("refresh")); err != nil {
		c.logger.Warn().Err(err).Msg("failed to publish supplier params refresh notification")
	}

	c.logger.Debug().Msg("supplier params refreshed from chain")
	return nil
}

// WarmupFromRedis populates L1 cache from Redis.
func (c *RedisSupplierParamCache) WarmupFromRedis(ctx context.Context) error {
	c.logger.Info().Msg("warming up supplier params cache from Redis")

	key := c.store.KB().ParamsSupplierKey()
	data, err := c.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, kv.ErrNotFound) {
			// Not in cache yet, that's OK
			c.logger.Debug().Msg("supplier params not in Redis, will be loaded on first query")
			return nil
		}
		c.logger.Warn().Err(err).Msg("failed to get supplier params from Redis during warmup")
		return err
	}

	// Unmarshal and store in L1
	params := &suppliertypes.Params{}
	if err := json.Unmarshal(data, params); err != nil {
		c.logger.Warn().Err(err).Msg("failed to unmarshal supplier params during warmup")
		return err
	}

	c.localCacheMu.Lock()
	c.localCache = params
	c.localCacheSet = true
	c.localCacheMu.Unlock()

	c.logger.Info().Msg("supplier params cache warmup complete")
	return nil
}

// Close gracefully shuts down the cache.
func (c *RedisSupplierParamCache) Close() error {
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

	c.logger.Info().Msg("supplier param cache closed")
	return nil
}
