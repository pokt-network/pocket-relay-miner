package cache

import (
	"context"
	"fmt"
	"time"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
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

	// InvalidateSharedParams invalidates the cached shared params for a specific height.
	// Call this when you know params have changed (e.g., governance proposal passed).
	InvalidateSharedParams(ctx context.Context, height int64) error

	// Start begins the cache's background processes (pub/sub subscriptions, etc.)
	Start(ctx context.Context) error

	// Close gracefully shuts down the cache.
	Close() error
}

// SessionCache provides cached access to on-chain session data.
type SessionCache interface {
	// GetSession returns the session for the given application, service, and block height.
	GetSession(ctx context.Context, appAddress, serviceId string, height int64) (*sessiontypes.Session, error)

	// GetSessionValidation returns cached validation result for a session.
	// Returns nil if no cached result exists.
	GetSessionValidation(ctx context.Context, appAddress, serviceId string, height int64) (*SessionValidationResult, error)

	// SetSessionValidation caches a session validation result.
	SetSessionValidation(ctx context.Context, result *SessionValidationResult) error

	// IsSessionRewardable checks if a session is still eligible for rewards.
	// Returns true if the session has not been marked as non-rewardable.
	IsSessionRewardable(ctx context.Context, sessionId string) bool

	// MarkSessionNonRewardable marks a session as no longer eligible for rewards.
	// This is broadcast to all instances via pub/sub.
	MarkSessionNonRewardable(ctx context.Context, sessionId string, reason string) error

	// Start begins the cache's background processes.
	Start(ctx context.Context) error

	// Close gracefully shuts down the cache.
	Close() error
}

// SessionValidationResult contains the result of validating a session.
type SessionValidationResult struct {
	// AppAddress is the application address.
	AppAddress string `json:"app_address"`

	// ServiceId is the service ID.
	ServiceId string `json:"service_id"`

	// BlockHeight is the block height at which validation was performed.
	BlockHeight int64 `json:"block_height"`

	// SessionID is the session ID from the query result.
	SessionID string `json:"session_id"`

	// SessionEndHeight is the session end block height.
	SessionEndHeight int64 `json:"session_end_height"`

	// IsValid indicates whether the session is valid.
	IsValid bool `json:"is_valid"`

	// FailureReason explains why validation failed (if not valid).
	FailureReason string `json:"failure_reason,omitempty"`

	// ValidatedAt is the Unix timestamp when validation occurred.
	ValidatedAt int64 `json:"validated_at"`
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

	// PubSubPrefix is the prefix for all Redis pub/sub channels.
	// Default: "ha:events"
	PubSubPrefix string

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

// DefaultCacheConfig returns sensible default cache configuration.
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		CachePrefix:            "ha:cache",
		PubSubPrefix:           "ha:events",
		TTLBlocks:              1,
		BlockTimeSeconds:       30,
		ExtraGracePeriodBlocks: 2,
		LockTimeout:            5 * time.Second,
	}
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

// Session returns the cache key for a session.
func (k CacheKeys) Session(appAddr, serviceId string, height int64) string {
	return k.Prefix + ":session:" + appAddr + ":" + serviceId + ":" + formatHeight(height)
}

// SessionByID returns the cache key for a session by session ID.
// Use this for caching sessions since session ID is constant for the session duration.
func (k CacheKeys) SessionByID(sessionID string) string {
	return k.Prefix + ":session:id:" + sessionID
}

// SessionValidation returns the cache key for session validation result.
func (k CacheKeys) SessionValidation(appAddr, serviceId string, height int64) string {
	return k.Prefix + ":session:validation:" + appAddr + ":" + serviceId + ":" + formatHeight(height)
}

// SessionRewardable returns the cache key for session rewardability flag.
func (k CacheKeys) SessionRewardable(sessionId string) string {
	return k.Prefix + ":session:rewardable:" + sessionId
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

// LeaderElector manages global leadership for cache refresh and other leader-only operations.
// This is the "lighthouse" component - all components check this to determine if they should
// perform leader-only operations.
type LeaderElector interface {
	// IsLeader returns true if this instance is the current leader.
	// This is the primary method used by all components to check leadership.
	IsLeader() bool

	// OnElected is called when this instance becomes leader.
	// Implementations can use this for initialization that should only happen on the leader.
	OnElected(ctx context.Context) error

	// OnLost is called when this instance loses leadership.
	// Implementations can use this for cleanup when stepping down.
	OnLost(ctx context.Context) error
}
