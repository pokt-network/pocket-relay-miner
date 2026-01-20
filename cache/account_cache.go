package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	// Cache type for pub/sub and metrics
	accountCacheType = "account"

	// NO EXPIRY for account cache - public keys are immutable on blockchain
	// Use 0 for no expiry, or a very long TTL (e.g., 365 days)
	accountCacheTTL = 0
)

// AccountQueryClient defines the interface for querying account public keys.
type AccountQueryClient interface {
	// GetPubKeyFromAddress queries the public key for a given address.
	GetPubKeyFromAddress(ctx context.Context, address string) (cryptotypes.PubKey, error)
}

// accountCache implements KeyedEntityCache[string, cryptotypes.PubKey]
// for caching account public keys.
//
// Cache levels:
// - L1: In-memory cache using xsync.Map for lock-free reads
// - L2: Redis cache with proto marshaling
// - L3: Chain query via AccountQueryClient
//
// The cache subscribes to pub/sub invalidation events to stay synchronized
// across all instances.
//
// IMPORTANT: Public keys NEVER change on-chain, so this cache has NO EXPIRY.
// Even if an app/gateway unstakes, their public key remains valid and static.
type accountCache struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	queryClient AccountQueryClient

	// L1: In-memory cache (xsync for lock-free performance)
	localCache *xsync.Map[string, cryptotypes.PubKey]

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
}

// NewAccountCache creates a new account public key cache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
func NewAccountCache(
	logger logging.Logger,
	redisClient *redisutil.Client,
	queryClient AccountQueryClient,
) KeyedEntityCache[string, cryptotypes.PubKey] {
	return &accountCache{
		logger:      logging.ForComponent(logger, logging.ComponentQueryAccount),
		redisClient: redisClient,
		queryClient: queryClient,
		localCache:  xsync.NewMap[string, cryptotypes.PubKey](),
	}
}

// Start initializes the cache and subscribes to pub/sub invalidation events.
func (c *accountCache) Start(ctx context.Context) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	// Subscribe to invalidation events
	if err := SubscribeToInvalidations(
		c.ctx,
		c.redisClient,
		c.logger,
		accountCacheType,
		c.handleInvalidation,
	); err != nil {
		return fmt.Errorf("failed to subscribe to invalidations: %w", err)
	}

	c.logger.Info().Msg("account cache started")

	return nil
}

// Close gracefully shuts down the cache.
func (c *accountCache) Close() error {
	if c.cancelFn != nil {
		c.cancelFn()
	}

	c.logger.Info().Msg("account cache stopped")

	return nil
}

// Get retrieves a public key using L1 → L2 → L3 fallback pattern.
// Note: force parameter is accepted for interface compatibility but ignored for account cache
// since public keys are immutable and never need forced refresh.
func (c *accountCache) Get(ctx context.Context, address string, force ...bool) (cryptotypes.PubKey, error) {
	start := time.Now()

	// L1: Check local cache (xsync)
	if pubKey, ok := c.localCache.Load(address); ok {
		cacheHits.WithLabelValues(accountCacheType, CacheLevelL1).Inc()
		cacheGetLatency.WithLabelValues(accountCacheType, CacheLevelL1).Observe(time.Since(start).Seconds())
		c.logger.Debug().
			Str("address", address).
			Msg("account cache hit (L1)")
		return pubKey, nil
	}

	// L2: Check Redis cache
	redisKey := c.redisClient.KB().CacheKey(accountCacheType, address)
	data, err := c.redisClient.Get(ctx, redisKey).Bytes()
	if err == nil {
		// Unmarshal proto-encoded public key
		pubKey, err := c.unmarshalPubKey(data)
		if err == nil {
			// Store in L1 for next time
			c.localCache.Store(address, pubKey)
			cacheHits.WithLabelValues(accountCacheType, CacheLevelL2).Inc()
			cacheGetLatency.WithLabelValues(accountCacheType, CacheLevelL2).Observe(time.Since(start).Seconds())

			c.logger.Debug().
				Str("address", address).
				Msg("account cache hit (L2) → stored in L1")

			return pubKey, nil
		} else {
			c.logger.Warn().
				Err(err).
				Str("address", address).
				Msg("failed to unmarshal public key from Redis")
		}
	}

	// L3: Query chain with distributed lock
	pubKey, err := c.queryChainWithLock(ctx, address)
	if err != nil {
		cacheMisses.WithLabelValues(accountCacheType, "l3_error").Inc()
		cacheGetLatency.WithLabelValues(accountCacheType, "l3_error").Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("failed to query account %s: %w", address, err)
	}

	// Store in L2 and L1 (with NO EXPIRY since public keys are immutable)
	if err := c.Set(ctx, address, pubKey, accountCacheTTL); err != nil {
		c.logger.Warn().
			Err(err).
			Str("address", address).
			Msg("failed to cache public key after L3 query")
	} else {
		c.logger.Debug().
			Str("address", address).
			Msg("account cache miss (L3) → stored in L1 and L2")
	}

	cacheMisses.WithLabelValues(accountCacheType, CacheLevelL3).Inc()
	cacheGetLatency.WithLabelValues(accountCacheType, CacheLevelL3).Observe(time.Since(start).Seconds())

	return pubKey, nil
}

// Set stores an account's public key in both L1 and L2 caches.
func (c *accountCache) Set(ctx context.Context, address string, pubKey cryptotypes.PubKey, ttl time.Duration) error {
	// L1: Store in local cache
	c.localCache.Store(address, pubKey)

	// L2: Store in Redis (proto marshaling)
	data, err := c.marshalPubKey(pubKey)
	if err != nil {
		return fmt.Errorf("failed to marshal public key: %w", err)
	}

	redisKey := c.redisClient.KB().CacheKey(accountCacheType, address)
	if err := c.redisClient.Set(ctx, redisKey, data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set Redis cache: %w", err)
	}

	c.logger.Debug().
		Str("address", address).
		Dur("ttl", ttl).
		Msg("account public key cached")

	return nil
}

// Invalidate removes an account's public key from both L1 and L2 caches.
// NOTE: This should rarely be needed since public keys are immutable.
func (c *accountCache) Invalidate(ctx context.Context, address string) error {
	// Remove from L1 (local cache)
	c.localCache.Delete(address)

	// Remove from L2 (Redis)
	redisKey := c.redisClient.KB().CacheKey(accountCacheType, address)
	if err := c.redisClient.Del(ctx, redisKey).Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Str("address", address).
			Msg("failed to delete account from Redis")
	}

	// Publish invalidation event to other instances
	payload := fmt.Sprintf(`{"address": "%s"}`, address)
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, accountCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Str("address", address).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(accountCacheType, SourceManual).Inc()

	c.logger.Info().
		Str("address", address).
		Msg("account public key invalidated")

	return nil
}

// Refresh is not applicable for account cache (no periodic refresh needed).
// Public keys are immutable, so there's nothing to refresh.
func (c *accountCache) Refresh(ctx context.Context) error {
	// No-op: public keys don't change
	return nil
}

// InvalidateAll clears the entire cache (both L1 and L2).
func (c *accountCache) InvalidateAll(ctx context.Context) error {
	// Clear L1 (local cache)
	c.localCache.Clear()

	// Clear L2 (Redis) - delete all keys matching prefix
	pattern := c.redisClient.KB().CacheKey(accountCacheType, "*")
	iter := c.redisClient.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		if err := c.redisClient.Del(ctx, iter.Val()).Err(); err != nil {
			c.logger.Warn().
				Err(err).
				Str("key", iter.Val()).
				Msg("failed to delete account key from Redis")
		}
	}
	if err := iter.Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("error scanning Redis keys during InvalidateAll")
	}

	// Publish invalidation event to other instances
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, accountCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(accountCacheType, SourceManual).Inc()

	c.logger.Info().Msg("account cache invalidated (all entries)")

	return nil
}

// WarmupFromRedis populates L1 cache from Redis on startup.
func (c *accountCache) WarmupFromRedis(ctx context.Context, knownAddresses []string) error {
	c.logger.Info().
		Int("count", len(knownAddresses)).
		Msg("warming up account cache from Redis")

	warmedCount := 0
	for _, address := range knownAddresses {
		redisKey := c.redisClient.KB().CacheKey(accountCacheType, address)
		data, err := c.redisClient.Get(ctx, redisKey).Bytes()
		if err != nil {
			// Key doesn't exist in Redis, skip
			continue
		}

		pubKey, err := c.unmarshalPubKey(data)
		if err != nil {
			c.logger.Warn().
				Err(err).
				Str("address", address).
				Msg("failed to unmarshal public key during warmup")
			continue
		}

		c.localCache.Store(address, pubKey)
		warmedCount++
	}

	c.logger.Info().
		Int("warmed", warmedCount).
		Int("total", len(knownAddresses)).
		Msg("account cache warmup complete")

	return nil
}

// queryChainWithLock queries the chain with distributed locking to prevent
// duplicate queries from multiple instances.
func (c *accountCache) queryChainWithLock(ctx context.Context, address string) (cryptotypes.PubKey, error) {
	lockKey := c.redisClient.KB().CacheLockKey(accountCacheType, address)

	// Try to acquire distributed lock
	locked, err := c.redisClient.SetNX(ctx, lockKey, "1", 5*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer c.redisClient.Del(ctx, lockKey)

	if !locked {
		// Another instance is querying, wait and retry L2
		lockAcquisitions.WithLabelValues(accountCacheType, "contended").Inc()
		c.logger.Debug().
			Str("address", address).
			Msg("another instance is querying account, waiting")
		time.Sleep(5 * time.Millisecond)

		// Retry L2 after waiting
		redisKey := c.redisClient.KB().CacheKey(accountCacheType, address)
		data, err := c.redisClient.Get(ctx, redisKey).Bytes()
		if err == nil {
			pubKey, err := c.unmarshalPubKey(data)
			if err == nil {
				cacheHits.WithLabelValues(accountCacheType, CacheLevelL2Retry).Inc()
				return pubKey, nil
			}
		}

		// If still not in Redis, query chain anyway
	} else {
		lockAcquisitions.WithLabelValues(accountCacheType, "acquired").Inc()
	}

	// Query chain
	chainQueries.WithLabelValues("account").Inc()
	chainStart := time.Now()

	pubKey, err := c.queryClient.GetPubKeyFromAddress(ctx, address)
	chainQueryLatency.WithLabelValues("account").Observe(time.Since(chainStart).Seconds())

	if err != nil {
		chainQueryErrors.WithLabelValues("account").Inc()
		return nil, err
	}

	return pubKey, nil
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *accountCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received account invalidation event")

	// Parse payload to get address
	var event struct {
		Address string `json:"address"`
	}

	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		// Empty payload means invalidate all
		if payload == "{}" {
			c.localCache.Clear()
			cacheInvalidations.WithLabelValues(accountCacheType, SourcePubSub).Inc()
			return nil
		}

		c.logger.Warn().
			Err(err).
			Str("payload", payload).
			Msg("failed to parse invalidation event")
		return err
	}

	// Remove from L1 (local cache)
	if event.Address != "" {
		c.localCache.Delete(event.Address)
	}

	cacheInvalidations.WithLabelValues(accountCacheType, SourcePubSub).Inc()

	return nil
}

// marshalPubKey marshals a public key to bytes for Redis storage.
func (c *accountCache) marshalPubKey(pubKey cryptotypes.PubKey) ([]byte, error) {
	// Public keys implement proto.Message, so we can marshal them directly
	return pubKey.Bytes(), nil
}

// unmarshalPubKey unmarshals a public key from bytes.
func (c *accountCache) unmarshalPubKey(data []byte) (cryptotypes.PubKey, error) {
	// Public keys are stored as raw bytes (33 bytes for compressed secp256k1)
	// We assume all keys are secp256k1 since that's what Pocket Network uses
	if len(data) != 33 {
		return nil, fmt.Errorf("invalid public key length: expected 33 bytes, got %d", len(data))
	}

	return &secp256k1.PubKey{Key: data}, nil
}
