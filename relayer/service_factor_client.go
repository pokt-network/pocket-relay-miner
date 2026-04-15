package relayer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// ServiceFactorData is the data stored in Redis for a service factor.
// This struct must match the one in miner/service_factor_registry.go.
type ServiceFactorData struct {
	Factor    float64 `json:"factor"`
	UpdatedAt int64   `json:"updated_at"`
}

// ServiceFactorClient reads service factor configuration from Redis.
// The miner publishes service factors, and relayers consume them for relay metering.
type ServiceFactorClient struct {
	logger      logging.Logger
	redisClient *redisutil.Client

	// L1 cache for service factors (lock-free)
	defaultFactorCache *xsync.Map[string, *ServiceFactorData] // Key: "default"
	serviceFactorCache *xsync.Map[string, *ServiceFactorData] // Key: serviceID

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.RWMutex
	closed   bool
}

// NewServiceFactorClient creates a new service factor client.
func NewServiceFactorClient(
	logger logging.Logger,
	redisClient *redisutil.Client,
) *ServiceFactorClient {
	return &ServiceFactorClient{
		logger:             logging.ForComponent(logger, logging.ComponentServiceFactorClient),
		redisClient:        redisClient,
		defaultFactorCache: xsync.NewMap[string, *ServiceFactorData](),
		serviceFactorCache: xsync.NewMap[string, *ServiceFactorData](),
	}
}

// Start begins the service factor client, subscribing to invalidation events.
//
// The client does three things on Start:
//  1. Preloads the default service_factor from Redis into L1 (per-service
//     overrides are lazy-loaded on first request in GetServiceFactor).
//  2. Subscribes to the service_factor pub/sub invalidation channel so
//     that changes published by the miner are reflected immediately in
//     this relayer's L1 cache. Without this subscription the L1 cache
//     would serve stale values until the relayer process restarts.
//  3. The subscription reconnects automatically (via cache.SubscribeToInvalidations)
//     if Redis goes down and comes back.
func (c *ServiceFactorClient) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}

	c.ctx, c.cancelFn = context.WithCancel(ctx)
	c.mu.Unlock()

	// Load initial values from Redis
	c.refreshAll(c.ctx)

	// Subscribe to miner-published invalidation events so hot updates to
	// service_factor config are picked up without requiring a relayer
	// restart. The subscription is handled in a goroutine with automatic
	// reconnection; errors here are only from the initial setup.
	if err := cache.SubscribeToInvalidations(
		c.ctx,
		c.redisClient,
		c.logger,
		cache.ServiceFactorCacheType,
		c.handleInvalidation,
	); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to subscribe to service_factor invalidation events — L1 cache will not hot-reload")
	}

	c.logger.Info().Msg("service factor client started")

	return nil
}

// handleInvalidation is the pub/sub message handler for the service_factor
// invalidation channel. Payload format matches
// cache.ServiceFactorInvalidationPayload:
//   - empty service_id → invalidate the default L1 entry
//   - non-empty service_id → invalidate that specific per-service L1 entry
//
// After invalidation, the next call to GetServiceFactor for the affected
// key will miss L1, fall through to Redis, and repopulate L1 with the
// fresh value.
func (c *ServiceFactorClient) handleInvalidation(_ context.Context, rawPayload string) error {
	var payload cache.ServiceFactorInvalidationPayload
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		// Unknown payload shape: be defensive and invalidate everything
		// so we don't serve stale data.
		c.InvalidateCache()
		c.logger.Warn().
			Err(err).
			Str("payload", rawPayload).
			Msg("service_factor invalidation payload could not be parsed — invalidated entire L1 cache as a precaution")
		return nil
	}

	if payload.ServiceID == "" {
		c.defaultFactorCache.Clear()
		c.logger.Info().
			Str("scope", "default").
			Msg("service_factor L1 cache invalidated via pub/sub — next GetServiceFactor call will reload from Redis")
	} else {
		c.serviceFactorCache.Delete(payload.ServiceID)
		c.logger.Info().
			Str("scope", "service").
			Str("service_id", payload.ServiceID).
			Msg("service_factor L1 cache invalidated via pub/sub — next GetServiceFactor call will reload from Redis")
	}
	return nil
}

// GetServiceFactor returns the service factor for a given service ID.
// It checks L1 cache first, then falls back to L2 (Redis).
// Returns (factor, true) if found, (0, false) if not configured.
func (c *ServiceFactorClient) GetServiceFactor(ctx context.Context, serviceID string) (float64, bool) {
	// Check L1 cache for per-service override
	if data, ok := c.serviceFactorCache.Load(serviceID); ok {
		return data.Factor, true
	}

	// Try to fetch from Redis (L2)
	key := c.serviceFactorServiceKey(serviceID)
	data, err := c.redisClient.Get(ctx, key).Bytes()
	if err == nil {
		var factorData ServiceFactorData
		if json.Unmarshal(data, &factorData) == nil {
			// Store in L1 cache
			c.serviceFactorCache.Store(serviceID, &factorData)
			return factorData.Factor, true
		}
	} else if !errors.Is(err, redis.Nil) {
		c.logger.Debug().
			Err(err).
			Str("service_id", serviceID).
			Msg("failed to get service factor from Redis")
	}

	// Check L1 cache for default
	if d, ok := c.defaultFactorCache.Load("default"); ok {
		return d.Factor, true
	}

	// Try to fetch default from Redis (L2)
	key = c.serviceFactorDefaultKey()
	data2, err := c.redisClient.Get(ctx, key).Bytes()
	if err == nil {
		var factorData ServiceFactorData
		if json.Unmarshal(data2, &factorData) == nil {
			// Store in L1 cache
			c.defaultFactorCache.Store("default", &factorData)
			return factorData.Factor, true
		}
	} else if !errors.Is(err, redis.Nil) {
		c.logger.Debug().
			Err(err).
			Msg("failed to get default service factor from Redis")
	}

	return 0, false
}

// HasServiceFactor returns true if a service factor is configured (either per-service or default).
func (c *ServiceFactorClient) HasServiceFactor(ctx context.Context, serviceID string) bool {
	_, found := c.GetServiceFactor(ctx, serviceID)
	return found
}

// InvalidateCache clears the L1 cache, forcing the next read to fetch from Redis.
func (c *ServiceFactorClient) InvalidateCache() {
	c.defaultFactorCache.Clear()
	c.serviceFactorCache.Clear()

	c.logger.Debug().Msg("service factor cache invalidated")
}

// InvalidateServiceCache invalidates the cache for a specific service.
func (c *ServiceFactorClient) InvalidateServiceCache(serviceID string) {
	c.serviceFactorCache.Delete(serviceID)

	c.logger.Debug().
		Str("service_id", serviceID).
		Msg("service factor cache invalidated for service")
}

// refreshAll refreshes all service factors from Redis.
func (c *ServiceFactorClient) refreshAll(ctx context.Context) {
	// Refresh default
	key := c.serviceFactorDefaultKey()
	data, err := c.redisClient.Get(ctx, key).Bytes()
	if err == nil {
		var factorData ServiceFactorData
		if json.Unmarshal(data, &factorData) == nil {
			c.defaultFactorCache.Store("default", &factorData)
			c.logger.Debug().
				Float64("factor", factorData.Factor).
				Msg("loaded default service factor from Redis")
		}
	} else if !errors.Is(err, redis.Nil) {
		c.logger.Debug().
			Err(err).
			Msg("failed to load default service factor from Redis")
	}
}

// Close gracefully shuts down the service factor client.
func (c *ServiceFactorClient) Close() error {
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

	c.logger.Info().Msg("service factor client closed")
	return nil
}

// Redis key helpers - delegate to KeyBuilder for consistency
func (c *ServiceFactorClient) serviceFactorDefaultKey() string {
	return c.redisClient.KB().ServiceFactorDefaultKey()
}

func (c *ServiceFactorClient) serviceFactorServiceKey(serviceID string) string {
	return c.redisClient.KB().ServiceFactorServiceKey(serviceID)
}
