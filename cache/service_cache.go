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
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

const (
	// Redis key prefix for service cache
	serviceCachePrefix = "ha:cache:service:"

	// Lock key prefix for distributed locking during L3 query
	serviceLockPrefix = "ha:cache:lock:service:"

	// Cache type for pub/sub and metrics
	serviceCacheType = "service"

	// Default TTL for service cache (5 minutes)
	serviceCacheTTL = 5 * time.Minute
)

// serviceCache implements KeyedEntityCache[string, *sharedtypes.Service]
// for caching service metadata (compute units, relay difficulty, etc.).
//
// Cache levels:
// - L1: In-memory cache using xsync.MapOf for lock-free concurrent access
// - L2: Redis cache with proto marshaling
// - L3: Chain query via ServiceQueryClient
//
// The cache subscribes to pub/sub invalidation events to stay synchronized
// across all instances.
type serviceCache struct {
	logger      logging.Logger
	redisClient redis.UniversalClient
	queryClient ServiceQueryClient

	// L1: In-memory cache (xsync for lock-free performance)
	localCache *xsync.Map[string, *sharedtypes.Service]

	// Pub/sub
	pubsub *redis.PubSub

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// ServiceQueryClient defines the interface for querying services from the chain.
type ServiceQueryClient interface {
	// GetService queries a service by ID from the chain.
	GetService(ctx context.Context, serviceID string) (*sharedtypes.Service, error)
}

// NewServiceCache creates a new service cache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
func NewServiceCache(
	logger logging.Logger,
	redisClient redis.UniversalClient,
	queryClient ServiceQueryClient,
) KeyedEntityCache[string, *sharedtypes.Service] {
	return &serviceCache{
		logger:      logging.ForComponent(logger, logging.ComponentQueryService),
		redisClient: redisClient,
		queryClient: queryClient,
		localCache:  xsync.NewMap[string, *sharedtypes.Service](),
	}
}

// Start initializes the cache and subscribes to pub/sub invalidation events.
func (c *serviceCache) Start(ctx context.Context) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	// Subscribe to invalidation events
	if err := SubscribeToInvalidations(
		c.ctx,
		c.redisClient,
		c.logger,
		serviceCacheType,
		c.handleInvalidation,
	); err != nil {
		return fmt.Errorf("failed to subscribe to invalidations: %w", err)
	}

	c.logger.Info().Msg("service cache started")

	return nil
}

// Close gracefully shuts down the cache.
func (c *serviceCache) Close() error {
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.wg.Wait()

	if c.pubsub != nil {
		_ = c.pubsub.Close()
	}

	c.logger.Info().Msg("service cache stopped")

	return nil
}

// Get retrieves a service using L1 → L2 → L3 fallback pattern.
func (c *serviceCache) Get(ctx context.Context, serviceID string) (*sharedtypes.Service, error) {
	// L1: Check local cache (xsync)
	if cached, ok := c.localCache.Load(serviceID); ok {
		cacheHits.WithLabelValues(serviceCacheType, CacheLevelL1).Inc()
		return cached, nil
	}

	// L2: Check Redis cache
	redisKey := serviceCachePrefix + serviceID
	data, err := c.redisClient.Get(ctx, redisKey).Bytes()
	if err == nil {
		var svc sharedtypes.Service
		if err := proto.Unmarshal(data, &svc); err == nil {
			// Store in L1 for next time
			c.localCache.Store(serviceID, &svc)
			cacheHits.WithLabelValues(serviceCacheType, CacheLevelL2).Inc()

			c.logger.Debug().
				Str(logging.FieldServiceID, serviceID).
				Msg("service cache hit (L2)")

			return &svc, nil
		} else {
			c.logger.Warn().
				Err(err).
				Str(logging.FieldServiceID, serviceID).
				Msg("failed to unmarshal service from Redis")
		}
	}

	// L3: Query chain with distributed lock
	svc, err := c.queryChainWithLock(ctx, serviceID)
	if err != nil {
		cacheMisses.WithLabelValues(serviceCacheType, "l3_error").Inc()
		return nil, fmt.Errorf("failed to query service %s: %w", serviceID, err)
	}

	// Store in L2 and L1
	if err := c.Set(ctx, serviceID, svc, serviceCacheTTL); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Msg("failed to cache service after L3 query")
	}

	cacheMisses.WithLabelValues(serviceCacheType, "l3").Inc()
	c.logger.Debug().
		Str(logging.FieldServiceID, serviceID).
		Msg("service cache miss (L3)")

	return svc, nil
}

// Set stores a service in both L1 and L2 caches.
func (c *serviceCache) Set(ctx context.Context, serviceID string, svc *sharedtypes.Service, ttl time.Duration) error {
	// L1: Store in local cache
	c.localCache.Store(serviceID, svc)

	// L2: Store in Redis (proto marshaling)
	data, err := proto.Marshal(svc)
	if err != nil {
		return fmt.Errorf("failed to marshal service: %w", err)
	}

	redisKey := serviceCachePrefix + serviceID
	if err := c.redisClient.Set(ctx, redisKey, data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set Redis cache: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldServiceID, serviceID).
		Dur("ttl", ttl).
		Msg("service cached")

	return nil
}

// Invalidate removes a service from ALL cache levels (L1 + L2 Redis)
// and publishes a pub/sub invalidation event to notify other instances.
func (c *serviceCache) Invalidate(ctx context.Context, serviceID string) error {
	// Remove from L1 (local cache)
	c.localCache.Delete(serviceID)

	// Remove from L2 (Redis)
	redisKey := serviceCachePrefix + serviceID
	if err := c.redisClient.Del(ctx, redisKey).Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Msg("failed to delete from Redis")
	}

	// Publish invalidation event to other instances
	payload := fmt.Sprintf(`{"service_id": "%s"}`, serviceID)
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, serviceCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(serviceCacheType, SourceManual).Inc()

	c.logger.Info().
		Str(logging.FieldServiceID, serviceID).
		Msg("service cache invalidated")

	return nil
}

// Refresh updates the cache from the chain (called by leader only).
// NOTE: Services are discovered dynamically via CacheOrchestrator.RecordDiscoveredService()
// This method is called by the orchestrator with the list of known services.
func (c *serviceCache) Refresh(ctx context.Context) error {
	// This method is intentionally empty because services are refreshed
	// individually by the CacheOrchestrator based on the list of known services.
	// The orchestrator calls Get() for each known service, which triggers L3 queries if needed.
	return nil
}

// InvalidateAll clears the entire cache (both L1 and L2).
func (c *serviceCache) InvalidateAll(ctx context.Context) error {
	// Clear L1 (local cache)
	c.localCache.Clear()

	// Clear L2 (Redis) - delete all keys with the prefix
	iter := c.redisClient.Scan(ctx, 0, serviceCachePrefix+"*", 0).Iterator()
	for iter.Next(ctx) {
		if err := c.redisClient.Del(ctx, iter.Val()).Err(); err != nil {
			c.logger.Warn().
				Err(err).
				Str("key", iter.Val()).
				Msg("failed to delete service from Redis")
		}
	}
	if err := iter.Err(); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to scan Redis keys for service cache")
	}

	// Publish invalidation event (empty payload means invalidate all)
	payload := "{}"
	if err := PublishInvalidation(ctx, c.redisClient, c.logger, serviceCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(serviceCacheType, SourceManual).Inc()

	c.logger.Info().Msg("all service cache invalidated")

	return nil
}

// WarmupFromRedis populates L1 cache from Redis on startup.
func (c *serviceCache) WarmupFromRedis(ctx context.Context, knownServiceIDs []string) error {
	// warmup function
	c.logger.Info().Int("count", len(knownServiceIDs)).Msg("warming up service cache from Redis")

	var wg sync.WaitGroup
	for _, serviceID := range knownServiceIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()

			// Load from Redis (L2) into local cache (L1)
			redisKey := serviceCachePrefix + id
			data, err := c.redisClient.Get(ctx, redisKey).Bytes()
			if err != nil {
				// Key doesn't exist in Redis, skip
				return
			}

			var svc sharedtypes.Service
			if err := proto.Unmarshal(data, &svc); err != nil {
				c.logger.Warn().
					Err(err).
					Str(logging.FieldServiceID, id).
					Msg("failed to unmarshal service during warmup")
				return
			}

			c.localCache.Store(id, &svc)
		}(serviceID)
	}

	wg.Wait()
	c.logger.Info().Msg("service cache warmup complete")

	return nil
}

// queryChainWithLock queries the chain with distributed locking to prevent
// duplicate queries from multiple instances.
func (c *serviceCache) queryChainWithLock(ctx context.Context, serviceID string) (*sharedtypes.Service, error) {
	lockKey := serviceLockPrefix + serviceID

	// Try to acquire distributed lock
	locked, err := c.redisClient.SetNX(ctx, lockKey, "1", 5*time.Second).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer c.redisClient.Del(ctx, lockKey)

	if !locked {
		// Another instance is querying, wait and retry L2
		c.logger.Debug().
			Str(logging.FieldServiceID, serviceID).
			Msg("another instance is querying service, waiting")
		time.Sleep(100 * time.Millisecond)

		// Retry L2 after waiting
		redisKey := serviceCachePrefix + serviceID
		data, err := c.redisClient.Get(ctx, redisKey).Bytes()
		if err == nil {
			var svc sharedtypes.Service
			if err := proto.Unmarshal(data, &svc); err == nil {
				c.localCache.Store(serviceID, &svc)
				cacheHits.WithLabelValues(serviceCacheType, CacheLevelL2Retry).Inc()
				return &svc, nil
			}
		}

		// If still not in Redis, query chain anyway
	}

	// Query chain
	chainQueries.WithLabelValues("service").Inc()

	svc, err := c.queryClient.GetService(ctx, serviceID)
	if err != nil {
		chainQueryErrors.WithLabelValues("service").Inc()
		return nil, err
	}

	return svc, nil
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *serviceCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received service invalidation event")

	// Parse payload to get service_id
	var event struct {
		ServiceID string `json:"service_id"`
	}

	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		// Empty payload means invalidate all
		if payload == "{}" {
			c.localCache.Clear()
			cacheInvalidations.WithLabelValues(serviceCacheType, SourcePubSub).Inc()
			return nil
		}

		c.logger.Warn().
			Err(err).
			Str("payload", payload).
			Msg("failed to parse invalidation event")
		return err
	}

	// Remove from L1 (local cache)
	if event.ServiceID != "" {
		c.localCache.Delete(event.ServiceID)
	}

	cacheInvalidations.WithLabelValues(serviceCacheType, SourcePubSub).Inc()

	return nil
}
