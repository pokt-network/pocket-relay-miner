package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/cosmos/gogoproto/proto"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
)

const (
	// Redis key prefix for application cache
	applicationCachePrefix = "ha:cache:application:"

	// Lock key prefix for distributed locking during L3 query
	applicationLockPrefix = "ha:cache:lock:application:"

	// Cache type for pub/sub and metrics
	applicationCacheType = "application"

	// Default TTL for application cache (5 minutes)
	applicationCacheTTL = 5 * time.Minute
)

// applicationCache implements KeyedEntityCache[string, *apptypes.Application]
// for caching application data.
//
// Cache levels:
// - L1: In-memory cache using xsync.MapOf for lock-free concurrent access
// - L2: Redis cache with proto marshaling
// - L3: Chain query via ApplicationQueryClient
//
// The cache subscribes to pub/sub invalidation events to stay synchronized
// across all instances.
type applicationCache struct {
	logger      logging.Logger
	redisClient redis.UniversalClient
	queryClient ApplicationQueryClient

	// L1: In-memory cache (xsync for lock-free performance)
	localCache *xsync.Map[string, *apptypes.Application]

	// Pub/sub
	pubsub *redis.PubSub

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// ApplicationQueryClient defines the interface for querying applications from the chain.
type ApplicationQueryClient interface {
	// GetApplication queries an application by address from the chain.
	GetApplication(ctx context.Context, address string) (*apptypes.Application, error)
}

// NewApplicationCache creates a new application cache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
func NewApplicationCache(
	logger logging.Logger,
	redisClient redis.UniversalClient,
	queryClient ApplicationQueryClient,
) KeyedEntityCache[string, *apptypes.Application] {
	return &applicationCache{
		logger:      logging.ForComponent(logger, logging.ComponentQueryApp),
		redisClient: redisClient,
		queryClient: queryClient,
		localCache:  xsync.NewMap[string, *apptypes.Application](),
	}
}

// Start initializes the cache and subscribes to pub/sub invalidation events.
func (c *applicationCache) Start(ctx context.Context) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	// Subscribe to invalidation events
	if err := SubscribeToInvalidations(
		c.ctx,
		c.redisClient,
		c.logger,
		applicationCacheType,
		c.handleInvalidation,
	); err != nil {
		return fmt.Errorf("failed to subscribe to invalidations: %w", err)
	}

	c.logger.Info().Msg("application cache started")

	return nil
}

// Close gracefully shuts down the cache.
func (c *applicationCache) Close() error {
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.wg.Wait()

	if c.pubsub != nil {
		_ = c.pubsub.Close()
	}

	c.logger.Info().Msg("application cache stopped")

	return nil
}

// Get retrieves an application using L1 → L2 → L3 fallback pattern.
func (c *applicationCache) Get(ctx context.Context, appAddress string) (*apptypes.Application, error) {
	// L1: Check local cache (xsync)
	if cached, ok := c.localCache.Load(appAddress); ok {
		cacheHits.WithLabelValues(applicationCacheType, CacheLevelL1).Inc()
		return cached, nil
	}

	// L2: Check Redis cache
	redisKey := applicationCachePrefix + appAddress
	data, err := c.redisClient.Get(ctx, redisKey).Bytes()
	if err == nil {
		var app apptypes.Application
		if err := proto.Unmarshal(data, &app); err == nil {
			// Store in L1 for next time
			c.localCache.Store(appAddress, &app)
			cacheHits.WithLabelValues(applicationCacheType, CacheLevelL2).Inc()

			c.logger.Debug().
				Str(logging.FieldAppAddress, appAddress).
				Msg("application cache hit (L2)")

			return &app, nil
		} else {
			c.logger.Warn().
				Err(err).
				Str(logging.FieldAppAddress, appAddress).
				Msg("failed to unmarshal application from Redis")
		}
	}

	// L3: Query chain with distributed lock
	app, err := c.queryChainWithLock(ctx, appAddress)
	if err != nil {
		cacheMisses.WithLabelValues(applicationCacheType, "l3_error").Inc()
		return nil, fmt.Errorf("failed to query application %s: %w", appAddress, err)
	}

	// Store in L2 and L1
	if err := c.Set(ctx, appAddress, app, applicationCacheTTL); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldAppAddress, appAddress).
			Msg("failed to cache application after L3 query")
	}

	cacheMisses.WithLabelValues(applicationCacheType, "l3").Inc()
	c.logger.Debug().
		Str(logging.FieldAppAddress, appAddress).
		Msg("application cache miss (L3)")

	return app, nil
}

// Set stores an application in both L1 and L2 caches.
func (c *applicationCache) Set(ctx context.Context, appAddress string, app *apptypes.Application, ttl time.Duration) error {
	// L1: Store in local cache
	c.localCache.Store(appAddress, app)

	// L2: Store in Redis (proto marshaling)
	data, err := proto.Marshal(app)
	if err != nil {
		return fmt.Errorf("failed to marshal application: %w", err)
	}

	redisKey := applicationCachePrefix + appAddress
	if err := c.redisClient.Set(ctx, redisKey, data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set Redis cache: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldAppAddress, appAddress).
		Dur("ttl", ttl).
		Msg("application cached")

	return nil
}

// Invalidate removes an application from ALL cache levels (L1 + L2 Redis)
// and publishes a pub/sub invalidation event to notify other instances.
func (c *applicationCache) Invalidate(ctx context.Context, appAddress string) error {
	// Remove from L1 (local cache)
	c.localCache.Delete(appAddress)

	// Remove from L2 (Redis)
	redisKey := applicationCachePrefix + appAddress
	if err := c.redisClient.Del(ctx, redisKey).Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldAppAddress, appAddress).
			Msg("failed to delete from Redis")
	}

	// Publish invalidation event to other instances
	payload := fmt.Sprintf(`{"address": "%s"}`, appAddress)
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, applicationCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldAppAddress, appAddress).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(applicationCacheType, SourceManual).Inc()

	c.logger.Info().
		Str(logging.FieldAppAddress, appAddress).
		Msg("application cache invalidated")

	return nil
}

// Refresh updates the cache from the chain (called by leader only).
// NOTE: Applications are discovered dynamically via CacheOrchestrator.RecordDiscoveredApp()
// This method is called by the orchestrator with the list of known apps.
func (c *applicationCache) Refresh(ctx context.Context) error {
	// This method is intentionally empty because applications are refreshed
	// individually by the CacheOrchestrator based on the list of known apps.
	// The orchestrator calls Get() for each known app, which triggers L3 queries if needed.
	return nil
}

// InvalidateAll clears the entire cache (both L1 and L2).
func (c *applicationCache) InvalidateAll(ctx context.Context) error {
	// Clear L1 (local cache)
	c.localCache.Clear()

	// Clear L2 (Redis) - delete all keys with the prefix
	// Note: This is an expensive operation, consider using a more efficient approach
	// if there are many applications cached.
	iter := c.redisClient.Scan(ctx, 0, applicationCachePrefix+"*", 0).Iterator()
	for iter.Next(ctx) {
		if err := c.redisClient.Del(ctx, iter.Val()).Err(); err != nil {
			c.logger.Warn().
				Err(err).
				Str("key", iter.Val()).
				Msg("failed to delete application from Redis")
		}
	}
	if err := iter.Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to scan Redis keys for application cache")
	}

	// Publish invalidation event (empty payload means invalidate all)
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, applicationCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(applicationCacheType, SourceManual).Inc()

	c.logger.Info().Msg("all application cache invalidated")

	return nil
}

// WarmupFromRedis populates L1 cache from Redis on startup.
func (c *applicationCache) WarmupFromRedis(ctx context.Context, knownAppAddresses []string) error {
	c.logger.Info().
		Int("count", len(knownAppAddresses)).
		Msg("warming up application cache from Redis")

	var wg sync.WaitGroup
	for _, appAddr := range knownAppAddresses {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()

			// Load from Redis (L2) into local cache (L1)
			redisKey := applicationCachePrefix + addr
			data, err := c.redisClient.Get(ctx, redisKey).Bytes()
			if err != nil {
				// Key doesn't exist in Redis, skip
				return
			}

			var app apptypes.Application
			if err := proto.Unmarshal(data, &app); err != nil {
				c.logger.Warn().
					Err(err).
					Str(logging.FieldAppAddress, addr).
					Msg("failed to unmarshal application during warmup")
				return
			}

			c.localCache.Store(addr, &app)
		}(appAddr)
	}

	wg.Wait()
	c.logger.Info().Msg("application cache warmup complete")

	return nil
}

// queryChainWithLock queries the chain with distributed locking to prevent
// duplicate queries from multiple instances.
func (c *applicationCache) queryChainWithLock(ctx context.Context, appAddress string) (*apptypes.Application, error) {
	lockKey := applicationLockPrefix + appAddress

	// Try to acquire distributed lock
	locked, err := c.redisClient.SetNX(ctx, lockKey, "1", 5*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer c.redisClient.Del(ctx, lockKey)

	if !locked {
		// Another instance is querying, wait and retry L2
		c.logger.Debug().
			Str(logging.FieldAppAddress, appAddress).
			Msg("another instance is querying application, waiting")
		time.Sleep(100 * time.Millisecond)

		// Retry L2 after waiting
		redisKey := applicationCachePrefix + appAddress
		data, err := c.redisClient.Get(ctx, redisKey).Bytes()
		if err == nil {
			var app apptypes.Application
			if err := proto.Unmarshal(data, &app); err == nil {
				c.localCache.Store(appAddress, &app)
				cacheHits.WithLabelValues(applicationCacheType, CacheLevelL2Retry).Inc()
				return &app, nil
			}
		}

		// If still not in Redis, query chain anyway
	}

	// Query chain
	chainQueries.WithLabelValues("application").Inc()

	app, err := c.queryClient.GetApplication(ctx, appAddress)
	if err != nil {
		chainQueryErrors.WithLabelValues("application").Inc()
		return nil, err
	}

	return app, nil
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *applicationCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received application invalidation event")

	// Parse payload to get address
	var event struct {
		Address string `json:"address"`
	}

	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		// Empty payload means invalidate all
		if payload == "{}" {
			c.localCache.Clear()
			cacheInvalidations.WithLabelValues(applicationCacheType, SourcePubSub).Inc()
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

	cacheInvalidations.WithLabelValues(applicationCacheType, SourcePubSub).Inc()

	return nil
}
