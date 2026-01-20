package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	// DefaultSupplierKeyPrefix is the default Redis key prefix for supplier state.
	DefaultSupplierKeyPrefix = "ha:supplier"

	// SupplierStatusActive indicates the supplier is active and accepting relays.
	SupplierStatusActive = "active"

	// SupplierStatusUnstaking indicates the supplier is unstaking.
	// Check UnstakeSessionEndHeight to determine if still in grace period.
	SupplierStatusUnstaking = "unstaking"

	// SupplierStatusNotStaked indicates the address is in keys but NOT staked on-chain.
	// This allows operators to see which keys are not yet staked as suppliers.
	SupplierStatusNotStaked = "not_staked"

	// Cache type for pub/sub and metrics
	supplierCacheType = "supplier"

	// Lock key prefix for distributed locking during L3 query
	// supplierLockPrefix = "ha:cache:lock:supplier:"

	// Default TTL for supplier cache (5 minutes)
	// supplierCacheTTL = 5 * time.Minute
)

// SupplierState represents the cached state of a supplier.
// This is stored in Redis and shared between miner (writer) and relayer (reader).
type SupplierState struct {
	// Status is the supplier's current status: "active", "unstaking", or "not_staked"
	Status string `json:"status"`

	// Staked indicates if this address is staked as a supplier on-chain.
	// false means the address is in the keys file but NOT staked on-chain.
	// This allows the miner to track all configured keys and report staking status.
	Staked bool `json:"staked"`

	// Services is the list of service IDs this supplier is staked for.
	// Empty if not staked (Staked=false).
	Services []string `json:"services"`

	// UnstakeSessionEndHeight is the session end height when unstaking takes effect.
	// 0 means the supplier is active (not unstaking).
	// >0 means the supplier is unstaking and will be fully unstaked at this session end height.
	//
	// TODO_IMPROVE: Handle unstaking grace period properly.
	// When supplier unstakes, they should continue serving until current session ends.
	// The unstaking takes effect at the next session boundary to prevent
	// breaking sessions halfway for both gateway and supplier.
	// This requires comparing against current session end height.
	UnstakeSessionEndHeight uint64 `json:"unstake_session_end_height"`

	// OperatorAddress is the supplier's operator address.
	OperatorAddress string `json:"operator_address"`

	// OwnerAddress is the supplier's owner address.
	OwnerAddress string `json:"owner_address"`

	// LastUpdated is the Unix timestamp when this state was last updated.
	LastUpdated int64 `json:"last_updated"`

	// LastRefreshSessionStart is the session start height when staking status was last checked.
	// Staking status is only refreshed at session boundaries to avoid unnecessary queries.
	LastRefreshSessionStart int64 `json:"last_refresh_session_start,omitempty"`

	// UpdatedBy identifies which miner instance updated this state (for debugging).
	UpdatedBy string `json:"updated_by,omitempty"`
}

// IsActive returns true if the supplier is active and should accept relays.
// TODO_IMPROVE: Add session height comparison for proper unstaking grace period.
// For now, any unstaking supplier is considered inactive.
func (s *SupplierState) IsActive() bool {
	return s.Staked && s.Status == SupplierStatusActive && s.UnstakeSessionEndHeight == 0
}

// IsActiveForService returns true if the supplier is active for the given service.
func (s *SupplierState) IsActiveForService(serviceID string) bool {
	if !s.IsActive() {
		return false
	}
	for _, svc := range s.Services {
		if svc == serviceID {
			return true
		}
	}
	return false
}

// SupplierCache provides read/write access to the shared supplier state cache.
//
// Cache levels:
// - L1: In-memory cache using xsync.MapOf for lock-free concurrent access
// - L2: Redis cache with JSON marshaling
//
// The cache subscribes to pub/sub invalidation events to stay synchronized
// across all instances.
type SupplierCache struct {
	logger    logging.Logger
	redis     *redisutil.Client
	keyPrefix string
	failOpen  bool

	// L1: In-memory cache (xsync for lock-free performance)
	localCache *xsync.Map[string, *SupplierState]

	// Pub/sub
	pubsub *redis.PubSub

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// SupplierCacheConfig contains configuration for SupplierCache.
type SupplierCacheConfig struct {
	// KeyPrefix is the Redis key prefix for supplier state.
	KeyPrefix string

	// FailOpen determines behavior when Redis is unavailable.
	// If true, treat supplier as active when cache unavailable (safer for traffic).
	// If false, treat supplier as inactive when cache unavailable (safer for validation).
	FailOpen bool
}

// NewSupplierCache creates a new SupplierCache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
func NewSupplierCache(
	logger logging.Logger,
	redisClient *redisutil.Client,
	config SupplierCacheConfig,
) *SupplierCache {
	keyPrefix := config.KeyPrefix
	if keyPrefix == "" {
		keyPrefix = DefaultSupplierKeyPrefix
	}

	return &SupplierCache{
		logger:     logging.ForComponent(logger, logging.ComponentQuerySupplier),
		redis:      redisClient,
		keyPrefix:  keyPrefix,
		failOpen:   config.FailOpen,
		localCache: xsync.NewMap[string, *SupplierState](),
	}
}

// supplierKey returns the Redis key for a supplier's state.
func (c *SupplierCache) supplierKey(operatorAddress string) string {
	return fmt.Sprintf("%s:%s", c.keyPrefix, operatorAddress)
}

// GetSupplierState retrieves a supplier's state from the cache using L1 → L2 fallback.
// Returns nil if the supplier is not in the cache.
// If Redis is unavailable and FailOpen is true, returns a synthetic "active" state.
func (c *SupplierCache) GetSupplierState(ctx context.Context, operatorAddress string) (*SupplierState, error) {
	start := time.Now()

	// L1: Check local cache (xsync)
	if cached, ok := c.localCache.Load(operatorAddress); ok {
		cacheHits.WithLabelValues(supplierCacheType, CacheLevelL1).Inc()
		cacheGetLatency.WithLabelValues(supplierCacheType, CacheLevelL1).Observe(time.Since(start).Seconds())
		c.logger.Debug().
			Str(logging.FieldSupplierOperator, operatorAddress).
			Msg("supplier cache hit (L1)")
		return cached, nil
	}

	// L2: Check Redis cache
	key := c.supplierKey(operatorAddress)
	data, err := c.redis.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			// Supplier not in cache
			cacheMisses.WithLabelValues(supplierCacheType, "l2_not_found").Inc()
			cacheGetLatency.WithLabelValues(supplierCacheType, "l2_not_found").Observe(time.Since(start).Seconds())
			return nil, nil
		}

		// Redis error
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSupplierOperator, operatorAddress).
			Bool("fail_open", c.failOpen).
			Msg("failed to get supplier state from cache")

		if c.failOpen {
			// Return synthetic active state to avoid blocking traffic
			c.logger.Warn().
				Str(logging.FieldSupplierOperator, operatorAddress).
				Msg("fail-open: treating supplier as active due to cache error")
			cacheGetLatency.WithLabelValues(supplierCacheType, "l2_error").Observe(time.Since(start).Seconds())
			return &SupplierState{
				Status:          SupplierStatusActive,
				OperatorAddress: operatorAddress,
			}, nil
		}

		cacheGetLatency.WithLabelValues(supplierCacheType, "l2_error").Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("failed to get supplier state: %w", err)
	}

	var state SupplierState
	if err := json.Unmarshal(data, &state); err != nil {
		cacheGetLatency.WithLabelValues(supplierCacheType, "l2_error").Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("failed to unmarshal supplier state: %w", err)
	}

	// Store in L1 for next time
	c.localCache.Store(operatorAddress, &state)
	cacheHits.WithLabelValues(supplierCacheType, CacheLevelL2).Inc()
	cacheGetLatency.WithLabelValues(supplierCacheType, CacheLevelL2).Observe(time.Since(start).Seconds())

	c.logger.Debug().
		Str(logging.FieldSupplierOperator, operatorAddress).
		Msg("supplier cache hit (L2) → stored in L1")

	return &state, nil
}

// SetSupplierState stores a supplier's state in both L1 and L2 caches.
// The miner typically calls this to update supplier state.
func (c *SupplierCache) SetSupplierState(ctx context.Context, state *SupplierState) error {
	if state.OperatorAddress == "" {
		return fmt.Errorf("operator_address is required")
	}

	// Update timestamp
	state.LastUpdated = time.Now().Unix()

	// L1: Store in local cache
	c.localCache.Store(state.OperatorAddress, state)

	// L2: Store in Redis
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal supplier state: %w", err)
	}

	key := c.supplierKey(state.OperatorAddress)

	// No TTL - explicit state management only
	if err := c.redis.Set(ctx, key, data, 0).Err(); err != nil {
		return fmt.Errorf("failed to set supplier state: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldSupplierOperator, state.OperatorAddress).
		Str("status", state.Status).
		Uint64("unstake_session_end_height", state.UnstakeSessionEndHeight).
		Msg("updated supplier state in cache")

	return nil
}

// DeleteSupplierState removes a supplier's state from both L1 and L2 caches.
// This should be called when a supplier is fully unstaked.
func (c *SupplierCache) DeleteSupplierState(ctx context.Context, operatorAddress string) error {
	// Remove from L1 (local cache)
	c.localCache.Delete(operatorAddress)

	// Remove from L2 (Redis)
	key := c.supplierKey(operatorAddress)
	if err := c.redis.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete supplier state: %w", err)
	}

	// Publish invalidation event to other instances
	payload := fmt.Sprintf(`{"operator_address": "%s"}`, operatorAddress)
	if err := PublishInvalidation(ctx, c.redis, c.logger, supplierCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSupplierOperator, operatorAddress).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(supplierCacheType, SourceManual).Inc()

	c.logger.Info().
		Str(logging.FieldSupplierOperator, operatorAddress).
		Msg("deleted supplier state from cache")

	return nil
}

// IsSupplierActiveForService checks if a supplier is active for a service.
// This is a convenience method that combines GetSupplierState and IsActiveForService.
// Returns (true, nil) if supplier is active for the service.
// Returns (false, nil) if supplier is not active or not in cache.
// Returns (false, error) if there was a cache error and FailOpen is false.
func (c *SupplierCache) IsSupplierActiveForService(
	ctx context.Context,
	operatorAddress string,
	serviceID string,
) (bool, error) {
	state, err := c.GetSupplierState(ctx, operatorAddress)
	if err != nil {
		return false, err
	}

	if state == nil {
		// Supplier not in cache
		if c.failOpen {
			c.logger.Warn().
				Str("operator_address", operatorAddress).
				Str("service_id", serviceID).
				Msg("fail-open: supplier not in cache, treating as active")
			return true, nil
		}
		return false, nil
	}

	return state.IsActiveForService(serviceID), nil
}

// GetAllSupplierStates returns all supplier states from the cache.
// This is useful for debugging and monitoring.
func (c *SupplierCache) GetAllSupplierStates(ctx context.Context) (map[string]*SupplierState, error) {
	pattern := fmt.Sprintf("%s:*", c.keyPrefix)
	keys, err := c.redis.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list supplier keys: %w", err)
	}

	states := make(map[string]*SupplierState)
	for _, key := range keys {
		data, err := c.redis.Get(ctx, key).Bytes()
		if err != nil {
			if err == redis.Nil {
				continue
			}
			return nil, fmt.Errorf("failed to get supplier state for key %s: %w", key, err)
		}

		var state SupplierState
		if err := json.Unmarshal(data, &state); err != nil {
			c.logger.Warn().
				Err(err).
				Str("key", key).
				Msg("failed to unmarshal supplier state, skipping")
			continue
		}

		states[state.OperatorAddress] = &state
	}

	return states, nil
}

// Start initializes the cache and subscribes to pub/sub invalidation events.
func (c *SupplierCache) Start(ctx context.Context) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	// Subscribe to invalidation events
	if err := SubscribeToInvalidations(
		c.ctx,
		c.redis,
		c.logger,
		supplierCacheType,
		c.handleInvalidation,
	); err != nil {
		return fmt.Errorf("failed to subscribe to invalidations: %w", err)
	}

	c.logger.Info().Msg("supplier cache started")

	return nil
}

// Close gracefully shuts down the cache.
func (c *SupplierCache) Close() error {
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.wg.Wait()

	if c.pubsub != nil {
		_ = c.pubsub.Close()
	}

	c.logger.Info().Msg("supplier cache stopped")

	return nil
}

// WarmupFromRedis populates L1 cache from Redis on startup.
// If knownSupplierAddresses is nil or empty, automatically discovers all suppliers in Redis.
func (c *SupplierCache) WarmupFromRedis(ctx context.Context, knownSupplierAddresses []string) error {
	// If no addresses provided, discover all suppliers in Redis
	if len(knownSupplierAddresses) == 0 {
		c.logger.Info().Msg("discovering all suppliers in Redis for warmup")
		pattern := fmt.Sprintf("%s:*", c.keyPrefix)
		keys, err := c.redis.Keys(ctx, pattern).Result()
		if err != nil {
			return fmt.Errorf("failed to discover suppliers: %w", err)
		}

		// Extract operator addresses from keys (ha:supplier:{operator} -> {operator})
		for _, key := range keys {
			// Remove prefix to get operator address
			addr := strings.TrimPrefix(key, c.keyPrefix+":")
			if addr != key { // Ensure prefix was found and removed
				knownSupplierAddresses = append(knownSupplierAddresses, addr)
			}
		}
		c.logger.Info().Int("discovered_suppliers", len(knownSupplierAddresses)).Msg("discovered suppliers in Redis")
	} else {
		c.logger.Info().Int("count", len(knownSupplierAddresses)).Msg("warming up supplier cache from Redis")
	}

	if len(knownSupplierAddresses) == 0 {
		c.logger.Info().Msg("no suppliers found in Redis, skipping warmup")
		return nil
	}

	var wg sync.WaitGroup
	for _, operatorAddr := range knownSupplierAddresses {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()

			// Load from Redis (L2) into local cache (L1)
			key := c.supplierKey(addr)
			data, err := c.redis.Get(ctx, key).Bytes()
			if err != nil {
				// Key doesn't exist in Redis, skip
				return
			}

			var state SupplierState
			if err := json.Unmarshal(data, &state); err != nil {
				c.logger.Warn().
					Err(err).
					Str(logging.FieldSupplierOperator, addr).
					Msg("failed to unmarshal supplier state during warmup")
				return
			}

			c.localCache.Store(addr, &state)
			c.logger.Debug().
				Str(logging.FieldSupplierOperator, addr).
				Int("services", len(state.Services)).
				Msg("loaded supplier into cache during warmup")
		}(operatorAddr)
	}

	wg.Wait()
	c.logger.Info().Int("loaded_count", len(knownSupplierAddresses)).Msg("supplier cache warmup complete")

	return nil
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *SupplierCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received supplier invalidation event")

	// Parse payload to get operator_address
	var event struct {
		OperatorAddress string `json:"operator_address"`
	}

	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		// Empty payload means invalidate all
		if payload == "{}" {
			c.localCache.Clear()
			cacheInvalidations.WithLabelValues(supplierCacheType, SourcePubSub).Inc()
			return nil
		}

		c.logger.Warn().
			Err(err).
			Str("payload", payload).
			Msg("failed to parse invalidation event")
		return err
	}

	// Remove from L1 (local cache)
	if event.OperatorAddress != "" {
		c.localCache.Delete(event.OperatorAddress)
	}

	cacheInvalidations.WithLabelValues(supplierCacheType, SourcePubSub).Inc()

	return nil
}
