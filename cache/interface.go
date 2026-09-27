package cache

import (
	"context"
	"fmt"
	"time"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

// SharedParamCache provides cached access to on-chain shared parameters.
// It implements a multi-level cache strategy:
// - L1: Local in-memory cache (sync.Map) for sub-microsecond access
// - L2: Redis cache for cross-instance consistency
// - L3: Chain query for cache misses (with distributed locking)
type SharedParamCache interface {
	// GetSharedParams returns the shared module parameters for the given block height.
	// Uses L1 -> L2 -> L3 fallback strategy.
	GetSharedParams(ctx context.Context, height int64) (*sharedtypes.Params, error)

	// GetLatestSharedParams returns the shared module parameters for the latest block.
	// Equivalent to GetSharedParams(ctx, latestBlockHeight).
	GetLatestSharedParams(ctx context.Context) (*sharedtypes.Params, error)

	// Start begins the cache's background processes (pub/sub subscriptions, etc.)
	Start(ctx context.Context) error

	// Close gracefully shuts down the cache.
	Close() error
}

// SupplierParamCache provides cached access to on-chain supplier parameters.
// It implements a multi-level cache strategy:
// - L1: Local in-memory cache for sub-microsecond access
// - L2: Redis cache for cross-instance consistency
// - L3: Chain query for cache misses (with distributed locking)
type SupplierParamCache interface {
	// GetSupplierParams returns the supplier module parameters.
	// Uses L1 -> L2 -> L3 fallback strategy.
	GetSupplierParams(ctx context.Context) (*suppliertypes.Params, error)

	// Refresh updates the cache from the chain (called by leader only).
	// Forces a fresh query and publishes invalidation to other instances.
	Refresh(ctx context.Context) error

	// Start begins the cache's background processes (pub/sub subscriptions, etc.)
	Start(ctx context.Context) error

	// Close gracefully shuts down the cache.
	Close() error
}

// SessionCache provides cached access to on-chain session data.
type SessionCache interface {
	// GetSession returns the session for the given application, service, and block height.
	GetSession(ctx context.Context, appAddress, serviceId string, height int64) (*sessiontypes.Session, error)

	// Start begins the cache's background processes.
	Start(ctx context.Context) error

	// Close gracefully shuts down the cache.
	Close() error
}

// BlockHeightSubscriber provides real-time block height updates across instances.
type BlockHeightSubscriber interface {
	// Subscribe returns a channel that receives new block heights.
	// The channel is closed when the subscriber is stopped.
	Subscribe(ctx context.Context) <-chan BlockEvent

	// PublishBlockHeight publishes a new block height to all subscribers.
	// This should be called by a single instance that watches the chain.
	PublishBlockHeight(ctx context.Context, event BlockEvent) error

	// Start begins listening for block height updates.
	Start(ctx context.Context) error

	// Close gracefully shuts down the subscriber.
	Close() error
}

// BlockEvent represents a new block being committed.
type BlockEvent struct {
	// Height is the block height.
	Height int64 `json:"height"`

	// Hash is the block hash (optional, for validation).
	Hash []byte `json:"hash,omitempty"`

	// Timestamp is when the block was committed.
	Timestamp time.Time `json:"timestamp"`
}

// CacheConfig contains configuration for cache implementations.
type CacheConfig struct {
	// Redis configuration
	RedisURL string

	// CachePrefix is the prefix for all Redis keys.
	// Default: "ha:cache"
	CachePrefix string

	// TTLBlocks is the default TTL in blocks.
	// Default: 1 (parameters change per block)
	TTLBlocks int64

	// BlockTimeSeconds is the assumed block time for TTL calculations.
	// Default: 6
	BlockTimeSeconds int64

	// ExtraGracePeriodBlocks is additional grace period for session caching.
	// Default: 2
	ExtraGracePeriodBlocks int64

	// LockTimeout is how long to wait when acquiring distributed locks.
	// Default: 5s
	LockTimeout time.Duration
}

// BlocksToTTL converts a number of blocks to a time.Duration.
func (c CacheConfig) BlocksToTTL(blocks int64) time.Duration {
	return time.Duration(blocks*c.BlockTimeSeconds) * time.Second
}

// CacheKeys provides helpers for generating Redis cache keys.
type CacheKeys struct {
	Prefix string
}

// SharedParams returns the cache key for shared params at a given height.
func (k CacheKeys) SharedParams(height int64) string {
	return k.Prefix + ":params:shared:" + formatHeight(height)
}

// SharedParamsLock returns the lock key for shared params at a given height.
func (k CacheKeys) SharedParamsLock(height int64) string {
	return k.Prefix + ":lock:params:shared:" + formatHeight(height)
}

// SupplierParams returns the cache key for supplier params (singleton, not height-based).
func (k CacheKeys) SupplierParams() string {
	return k.Prefix + ":params:supplier"
}

// SupplierParamsLock returns the lock key for supplier params.
func (k CacheKeys) SupplierParamsLock() string {
	return k.Prefix + ":lock:params:supplier"
}

// Session returns the cache key for a session.
func (k CacheKeys) Session(appAddr, serviceId string, height int64) string {
	return k.Prefix + ":session:" + appAddr + ":" + serviceId + ":" + formatHeight(height)
}

// formatHeight converts a block height to a string.
func formatHeight(height int64) string {
	return fmt.Sprintf("%d", height)
}

// ========================================================================
// Unified Entity Cache Interfaces (for new cache architecture)
// ========================================================================

// EntityCache is the base interface for all caches in the unified architecture.
// It provides lifecycle management and refresh capabilities.
type EntityCache interface {
	// Start initializes the cache and subscribes to pub/sub events.
	Start(ctx context.Context) error

	// Close shuts down the cache and unsubscribes from events.
	Close() error

	// Refresh updates the cache from the chain (called by leader only).
	// The global leader calls this method periodically to keep caches fresh.
	Refresh(ctx context.Context) error

	// InvalidateAll clears the entire cache (both L1 and L2).
	InvalidateAll(ctx context.Context) error
}

// KeyedEntityCache manages entities indexed by a key (e.g., address, service ID).
// Implements L1 (in-memory) → L2 (Redis) → L3 (chain query) pattern.
//
// Type parameters:
//   - K: The key type (must be comparable, e.g., string)
//   - V: The value type (typically a proto message pointer)
//
// Example usage:
//
//	type ApplicationCache = KeyedEntityCache[string, *apptypes.Application]
type KeyedEntityCache[K comparable, V any] interface {
	EntityCache

	// Get retrieves an entity using L1 → L2 → L3 fallback pattern.
	// If force=true, bypasses L1/L2 cache, queries L3 (chain), stores in L2+L1, and publishes invalidation.
	// Returns an error if the entity doesn't exist or query fails.
	Get(ctx context.Context, key K, force ...bool) (V, error)

	// Set stores an entity in both L1 and L2 caches with the specified TTL.
	Set(ctx context.Context, key K, value V, ttl time.Duration) error

	// Invalidate removes a specific entity from ALL cache levels (L1 + L2 Redis).
	// Also publishes a pub/sub invalidation event to notify other instances.
	Invalidate(ctx context.Context, key K) error
}

// SingletonEntityCache manages a single global entity (e.g., shared params).
// Similar to KeyedEntityCache but without a key - there's only one value.
//
// Type parameter:
//   - V: The value type (typically a proto message pointer)
//
// Example usage:
//
//	type SharedParamsCache = SingletonEntityCache[*sharedtypes.Params]
type SingletonEntityCache[V any] interface {
	EntityCache

	// Get retrieves the singleton entity using L1 → L2 → L3 fallback pattern.
	// If force=true, bypasses L1/L2 cache, queries L3 (chain), stores in L2+L1, and publishes invalidation.
	Get(ctx context.Context, force ...bool) (V, error)

	// Set stores the singleton entity in both L1 and L2 caches with the specified TTL.
	Set(ctx context.Context, value V, ttl time.Duration) error
}
