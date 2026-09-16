package relayer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

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

// DefaultServiceFactorMissingTTL is how long the client remembers that Redis
// holds no factor under a key.
//
// Seconds, not minutes: the factor caps what a relay may be charged, so while a
// negative entry stands, relays for that service are priced by the conservative
// fallback. The miner publishes an invalidation on every factor it writes and
// handleInvalidation drops the entry at once, so this TTL is only the backstop
// for the event that never arrived -- a dropped message, or a subscriber that
// was reconnecting.
const DefaultServiceFactorMissingTTL = 5 * time.Second

// serviceFactorEntry is one L1 slot: either the factor read from Redis, or the
// fact that Redis holds no key for it.
//
// The absence is cached because GetServiceFactor runs once per relay: with no
// key in Redis, remembering only the hits sent one GET per relay (measured at
// 1.858 GET/s against 1.855 relays/s on a live run).
type serviceFactorEntry struct {
	// data is nil on a negative entry: Redis answered that the key is absent.
	data *ServiceFactorData

	// missingUntil bounds a NEGATIVE entry and is the zero time on a positive
	// one. A positive entry does not expire -- it is dropped by the miner's
	// pub/sub invalidation, which is what keeps a price change immediate.
	missingUntil time.Time
}

// ServiceFactorClient reads service factor configuration from Redis.
// The miner publishes service factors, and relayers consume them for relay metering.
type ServiceFactorClient struct {
	logger      logging.Logger
	redisClient *redisutil.Client

	// L1 cache for service factors (lock-free)
	defaultFactorCache *xsync.Map[string, serviceFactorEntry] // Key: "default"
	serviceFactorCache *xsync.Map[string, serviceFactorEntry] // Key: serviceID

	// missingTTL bounds how long a negative entry is honoured.
	missingTTL time.Duration

	// now is the clock the negative entries expire against. It is captured at
	// construction, on the caller's goroutine, so a test that replaces it is
	// ordered with every read that follows.
	now func() time.Time

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.RWMutex
	closed   bool
}

// NewServiceFactorClient creates a new service factor client.
//
// missingTTL bounds how long "Redis holds no factor for this key" is
// remembered; a value of zero or less takes DefaultServiceFactorMissingTTL.
func NewServiceFactorClient(
	logger logging.Logger,
	redisClient *redisutil.Client,
	missingTTL time.Duration,
) *ServiceFactorClient {
	if missingTTL <= 0 {
		missingTTL = DefaultServiceFactorMissingTTL
	}

	return &ServiceFactorClient{
		logger:             logging.ForComponent(logger, logging.ComponentServiceFactorClient),
		redisClient:        redisClient,
		defaultFactorCache: xsync.NewMap[string, serviceFactorEntry](),
		serviceFactorCache: xsync.NewMap[string, serviceFactorEntry](),
		missingTTL:         missingTTL,
		now:                time.Now,
	}
}

// missingEntry builds a negative entry that stands until missingTTL elapses.
func (c *ServiceFactorClient) missingEntry() serviceFactorEntry {
	return serviceFactorEntry{missingUntil: c.now().Add(c.missingTTL)}
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
//
// A per-service entry recording ABSENCE means "this service has no override",
// so it skips the per-service GET and still resolves the default. Only a
// negative default entry ends the lookup.
func (c *ServiceFactorClient) GetServiceFactor(ctx context.Context, serviceID string) (float64, bool) {
	// Check L1 cache for per-service override
	knownMissing := false
	if entry, ok := c.serviceFactorCache.Load(serviceID); ok {
		if entry.data != nil {
			return entry.data.Factor, true
		}
		knownMissing = c.now().Before(entry.missingUntil)
	}

	if !knownMissing {
		// Try to fetch from Redis (L2)
		key := c.serviceFactorServiceKey(serviceID)
		data, err := c.redisClient.Get(ctx, key).Bytes()
		switch {
		case err == nil:
			var factorData ServiceFactorData
			if json.Unmarshal(data, &factorData) == nil {
				// Store in L1 cache
				c.serviceFactorCache.Store(serviceID, serviceFactorEntry{data: &factorData})
				return factorData.Factor, true
			}
			// A value that does not parse is a defect in the producer, not an
			// absence: it is left uncached so it keeps reaching Redis and
			// resolves the moment the miner rewrites the key.
		case errors.Is(err, redis.Nil):
			c.serviceFactorCache.Store(serviceID, c.missingEntry())
		default:
			// A timeout or a lost connection is not an absence. Remembering it
			// would price every relay of this service off one blink of Redis.
			c.logger.Debug().
				Err(err).
				Str("service_id", serviceID).
				Msg("failed to get service factor from Redis")
		}
	}

	// Check L1 cache for default
	if entry, ok := c.defaultFactorCache.Load("default"); ok {
		if entry.data != nil {
			return entry.data.Factor, true
		}
		if c.now().Before(entry.missingUntil) {
			return 0, false
		}
	}

	// Try to fetch default from Redis (L2)
	key := c.serviceFactorDefaultKey()
	data2, err := c.redisClient.Get(ctx, key).Bytes()
	switch {
	case err == nil:
		var factorData ServiceFactorData
		if json.Unmarshal(data2, &factorData) == nil {
			// Store in L1 cache
			c.defaultFactorCache.Store("default", serviceFactorEntry{data: &factorData})
			return factorData.Factor, true
		}
	case errors.Is(err, redis.Nil):
		c.defaultFactorCache.Store("default", c.missingEntry())
	default:
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
			c.defaultFactorCache.Store("default", serviceFactorEntry{data: &factorData})
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
