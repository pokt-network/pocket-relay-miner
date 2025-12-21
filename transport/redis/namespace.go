package redis

import (
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/config"
)

// KeyBuilder builds Redis keys with configured prefixes.
// This eliminates hardcoded "ha:" strings scattered throughout the codebase.
type KeyBuilder struct {
	ns config.RedisNamespaceConfig
}

// NewKeyBuilder creates a new KeyBuilder with the given namespace configuration.
func NewKeyBuilder(ns config.RedisNamespaceConfig) *KeyBuilder {
	return &KeyBuilder{ns: ns}
}

// CacheKey builds a cache key for an entity.
// Format: {base}:{cache}:{entityType}:{key}
// Example: "ha:cache:application:pokt1abc..."
func (kb *KeyBuilder) CacheKey(entityType, key string) string {
	return fmt.Sprintf("%s:%s:%s:%s", kb.ns.BasePrefix, kb.ns.CachePrefix, entityType, key)
}

// CacheLockKey builds a distributed lock key for cache population.
// Format: {base}:{cache}:lock:{entityType}:{key}
// Example: "ha:cache:lock:application:pokt1abc..."
func (kb *KeyBuilder) CacheLockKey(entityType, key string) string {
	return fmt.Sprintf("%s:%s:lock:%s:%s", kb.ns.BasePrefix, kb.ns.CachePrefix, entityType, key)
}

// CacheKnownKey builds a key for tracking known entities of a type.
// Format: {base}:{cache}:known:{entityType}
// Example: "ha:cache:known:applications"
func (kb *KeyBuilder) CacheKnownKey(entityType string) string {
	return fmt.Sprintf("%s:%s:known:%s", kb.ns.BasePrefix, kb.ns.CachePrefix, entityType)
}

// EventChannel builds a pub/sub channel name for cache invalidation.
// Format: {base}:{events}:cache:{cacheType}:invalidate
// Example: "ha:events:cache:application:invalidate"
func (kb *KeyBuilder) EventChannel(cacheType, event string) string {
	return fmt.Sprintf("%s:%s:cache:%s:%s", kb.ns.BasePrefix, kb.ns.EventsPrefix, cacheType, event)
}

// EventChannelPrefix builds a prefix for all event channels.
// Format: {base}:{events}
// Example: "ha:events"
func (kb *KeyBuilder) EventChannelPrefix() string {
	return fmt.Sprintf("%s:%s", kb.ns.BasePrefix, kb.ns.EventsPrefix)
}

// StreamPrefix returns the stream namespace prefix.
// Format: {base}:{streams}
// Example: "ha:relays"
func (kb *KeyBuilder) StreamPrefix() string {
	return fmt.Sprintf("%s:%s", kb.ns.BasePrefix, kb.ns.StreamsPrefix)
}

// ConsumerGroup returns the consumer group name for Redis Streams.
// Format: {base}-{consumer_group_prefix}
// Example: "ha-miners"
func (kb *KeyBuilder) ConsumerGroup() string {
	return fmt.Sprintf("%s-%s", kb.ns.BasePrefix, kb.ns.ConsumerGroupPrefix)
}

// StreamKey builds a Redis Stream key for a supplier.
// Format: {base}:{streams}:{supplier}
// Example: "ha:relays:pokt1xyz..."
func (kb *KeyBuilder) StreamKey(supplier string) string {
	return fmt.Sprintf("%s:%s:%s", kb.ns.BasePrefix, kb.ns.StreamsPrefix, supplier)
}

// MinerSessionKey builds a key for session metadata.
// Format: {base}:{miner}:sessions:{supplier}:{sessionID}
// Example: "ha:miner:sessions:pokt1xyz:session123"
func (kb *KeyBuilder) MinerSessionKey(supplier, sessionID string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:%s", kb.ns.BasePrefix, kb.ns.MinerPrefix, supplier, sessionID)
}

// MinerSessionIndexKey builds a key for the session index set.
// Format: {base}:{miner}:sessions:{supplier}:index
// Example: "ha:miner:sessions:pokt1xyz:index"
func (kb *KeyBuilder) MinerSessionIndexKey(supplier string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:index", kb.ns.BasePrefix, kb.ns.MinerPrefix, supplier)
}

// MinerSessionStateKey builds a key for sessions by state.
// Format: {base}:{miner}:sessions:{supplier}:state:{state}
// Example: "ha:miner:sessions:pokt1xyz:state:active"
func (kb *KeyBuilder) MinerSessionStateKey(supplier, state string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:state:%s", kb.ns.BasePrefix, kb.ns.MinerPrefix, supplier, state)
}

// MinerDedupKey builds a key for relay deduplication.
// Format: {base}:{miner}:dedup:session:{sessionID}
// Example: "ha:miner:dedup:session:session123"
func (kb *KeyBuilder) MinerDedupKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:dedup:session:%s", kb.ns.BasePrefix, kb.ns.MinerPrefix, sessionID)
}

// MinerLeaderKey builds the global leader election key.
// Format: {base}:{miner}:global_leader
// Example: "ha:miner:global_leader"
func (kb *KeyBuilder) MinerLeaderKey() string {
	return fmt.Sprintf("%s:%s:global_leader", kb.ns.BasePrefix, kb.ns.MinerPrefix)
}

// MinerSMSTNodesKey builds a key for SMST nodes.
// Format: {base}:smst:{sessionID}:nodes
// Example: "ha:smst:session123:nodes"
// Note: SMST uses direct base prefix for historical reasons
func (kb *KeyBuilder) MinerSMSTNodesKey(sessionID string) string {
	return fmt.Sprintf("%s:smst:%s:nodes", kb.ns.BasePrefix, sessionID)
}

// SupplierKey builds a key for supplier registry data.
// Format: {base}:{supplier}:{address}
// Example: "ha:supplier:pokt1xyz..."
func (kb *KeyBuilder) SupplierKey(address string) string {
	return fmt.Sprintf("%s:%s:%s", kb.ns.BasePrefix, kb.ns.SupplierPrefix, address)
}

// SupplierIndexKey builds the global supplier index key.
// Format: {base}:{supplier}:index
// Example: "ha:supplier:index"
func (kb *KeyBuilder) SupplierIndexKey() string {
	return fmt.Sprintf("%s:%s:index", kb.ns.BasePrefix, kb.ns.SupplierPrefix)
}

// SupplierKeyPrefix returns the base prefix for supplier keys.
// Format: {base}:{supplier}
// Example: "ha:supplier"
func (kb *KeyBuilder) SupplierKeyPrefix() string {
	return fmt.Sprintf("%s:%s", kb.ns.BasePrefix, kb.ns.SupplierPrefix)
}

// SuppliersRegistryPrefix returns the prefix for suppliers registry.
// Format: {base}:suppliers
// Example: "ha:suppliers"
func (kb *KeyBuilder) SuppliersRegistryPrefix() string {
	return fmt.Sprintf("%s:suppliers", kb.ns.BasePrefix)
}

// SuppliersRegistryIndexKey returns the index key for suppliers registry.
// Format: {base}:suppliers:index
// Example: "ha:suppliers:index"
func (kb *KeyBuilder) SuppliersRegistryIndexKey() string {
	return fmt.Sprintf("%s:suppliers:index", kb.ns.BasePrefix)
}

// CachePrefix returns the full cache prefix.
// Format: {base}:{cache}
// Example: "ha:cache"
func (kb *KeyBuilder) CachePrefix() string {
	return fmt.Sprintf("%s:%s", kb.ns.BasePrefix, kb.ns.CachePrefix)
}

// EventsCachePrefix returns the pub/sub prefix for cache events.
// Format: {base}:{events}:{cache}
// Example: "ha:events:cache"
func (kb *KeyBuilder) EventsCachePrefix() string {
	return fmt.Sprintf("%s:%s:%s", kb.ns.BasePrefix, kb.ns.EventsPrefix, kb.ns.CachePrefix)
}

// MinerSessionsPrefix returns the prefix for miner session store.
// Format: {base}:{miner}:sessions
// Example: "ha:miner:sessions"
func (kb *KeyBuilder) MinerSessionsPrefix() string {
	return fmt.Sprintf("%s:%s:sessions", kb.ns.BasePrefix, kb.ns.MinerPrefix)
}

// GlobalLeaderKey returns the key for global leader election.
// Format: {base}:{miner}:global_leader
// Example: "ha:miner:global_leader"
func (kb *KeyBuilder) GlobalLeaderKey() string {
	return fmt.Sprintf("%s:%s:global_leader", kb.ns.BasePrefix, kb.ns.MinerPrefix)
}

// MeterKey builds a key for relay metering data.
// Format: {base}:{meter}:{sessionID}
// Example: "ha:meter:session123"
func (kb *KeyBuilder) MeterKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:%s", kb.ns.BasePrefix, kb.ns.MeterPrefix, sessionID)
}

// MeterAppStakeKey builds a key for application stake cache.
// Format: {base}:app_stake:{appAddress}
// Example: "ha:app_stake:pokt1abc..."
// Note: Uses direct base prefix for historical reasons
func (kb *KeyBuilder) MeterAppStakeKey(appAddress string) string {
	return fmt.Sprintf("%s:app_stake:%s", kb.ns.BasePrefix, appAddress)
}

// MeterServiceComputeUnitsKey builds a key for service compute units cache.
// Format: {base}:service:{serviceID}:compute_units
// Example: "ha:service:eth-mainnet:compute_units"
// Note: Uses direct base prefix for historical reasons
func (kb *KeyBuilder) MeterServiceComputeUnitsKey(serviceID string) string {
	return fmt.Sprintf("%s:service:%s:compute_units", kb.ns.BasePrefix, serviceID)
}

// ParamsSharedKey builds the key for cached shared params.
// Format: {base}:{params}:shared
// Example: "ha:params:shared"
func (kb *KeyBuilder) ParamsSharedKey() string {
	return fmt.Sprintf("%s:%s:shared", kb.ns.BasePrefix, kb.ns.ParamsPrefix)
}

// ParamsSessionKey builds the key for cached session params.
// Format: {base}:{params}:session
// Example: "ha:params:session"
func (kb *KeyBuilder) ParamsSessionKey() string {
	return fmt.Sprintf("%s:%s:session", kb.ns.BasePrefix, kb.ns.ParamsPrefix)
}

// ParamsProofKey builds the key for cached proof params.
// Format: {base}:{cache}:proof_params
// Example: "ha:cache:proof_params"
func (kb *KeyBuilder) ParamsProofKey() string {
	return fmt.Sprintf("%s:%s:proof_params", kb.ns.BasePrefix, kb.ns.CachePrefix)
}

// ParamsProofLockKey builds the lock key for proof params cache population.
// Format: {base}:{cache}:lock:proof_params
// Example: "ha:cache:lock:proof_params"
func (kb *KeyBuilder) ParamsProofLockKey() string {
	return fmt.Sprintf("%s:%s:lock:proof_params", kb.ns.BasePrefix, kb.ns.CachePrefix)
}

// ParamsSessionCacheKey builds the key for cached session params.
// Format: {base}:{cache}:session_params
// Example: "ha:cache:session_params"
func (kb *KeyBuilder) ParamsSessionCacheKey() string {
	return fmt.Sprintf("%s:%s:session_params", kb.ns.BasePrefix, kb.ns.CachePrefix)
}

// ParamsSessionLockKey builds the lock key for session params cache population.
// Format: {base}:{cache}:lock:session_params
// Example: "ha:cache:lock:session_params"
func (kb *KeyBuilder) ParamsSessionLockKey() string {
	return fmt.Sprintf("%s:%s:lock:session_params", kb.ns.BasePrefix, kb.ns.CachePrefix)
}

// ParamsSharedCacheKey builds the key for cached shared params singleton.
// Format: {base}:{cache}:shared_params
// Example: "ha:cache:shared_params"
func (kb *KeyBuilder) ParamsSharedCacheKey() string {
	return fmt.Sprintf("%s:%s:shared_params", kb.ns.BasePrefix, kb.ns.CachePrefix)
}

// ParamsSharedLockKey builds the lock key for shared params cache population.
// Format: {base}:{cache}:lock:shared_params
// Example: "ha:cache:lock:shared_params"
func (kb *KeyBuilder) ParamsSharedLockKey() string {
	return fmt.Sprintf("%s:%s:lock:shared_params", kb.ns.BasePrefix, kb.ns.CachePrefix)
}

// MeterCleanupChannel builds the pub/sub channel for meter cleanup events.
// Format: {base}:{meter}:cleanup
// Example: "ha:meter:cleanup"
func (kb *KeyBuilder) MeterCleanupChannel() string {
	return fmt.Sprintf("%s:%s:cleanup", kb.ns.BasePrefix, kb.ns.MeterPrefix)
}

// SupplierUpdateChannel builds the pub/sub channel for supplier updates.
// Format: {base}:{events}:supplier_update
// Example: "ha:events:supplier_update"
func (kb *KeyBuilder) SupplierUpdateChannel() string {
	return fmt.Sprintf("%s:%s:supplier_update", kb.ns.BasePrefix, kb.ns.EventsPrefix)
}

// BlockEventChannel builds the pub/sub channel for block events.
// Format: {base}:{events}:blocks
// Example: "ha:events:blocks"
func (kb *KeyBuilder) BlockEventChannel() string {
	return fmt.Sprintf("%s:%s:blocks", kb.ns.BasePrefix, kb.ns.EventsPrefix)
}

// DiscoveredAppsKey builds the key for discovered applications set.
// Format: {base}:discovered_apps
// Example: "ha:discovered_apps"
// Note: Uses direct base prefix for historical reasons
func (kb *KeyBuilder) DiscoveredAppsKey() string {
	return fmt.Sprintf("%s:discovered_apps", kb.ns.BasePrefix)
}

// SMSTNodesKey builds the key for SMST tree nodes hash.
// Format: {base}:smst:{sessionID}:nodes
// Example: "ha:smst:session123:nodes"
func (kb *KeyBuilder) SMSTNodesKey(sessionID string) string {
	return fmt.Sprintf("%s:smst:%s:nodes", kb.ns.BasePrefix, sessionID)
}

// SMSTNodesPattern builds the pattern for scanning all SMST node keys.
// Format: {base}:smst:*:nodes
// Example: "ha:smst:*:nodes"
func (kb *KeyBuilder) SMSTNodesPattern() string {
	return fmt.Sprintf("%s:smst:*:nodes", kb.ns.BasePrefix)
}

// SMSTNodesPrefix builds the prefix for SMST node keys (for extracting sessionID).
// Format: {base}:smst:
// Example: "ha:smst:"
func (kb *KeyBuilder) SMSTNodesPrefix() string {
	return fmt.Sprintf("%s:smst:", kb.ns.BasePrefix)
}

// ServiceFactorDefaultKey builds the key for the default service factor.
// Format: {base}:service_factor:default
// Example: "ha:service_factor:default"
func (kb *KeyBuilder) ServiceFactorDefaultKey() string {
	return fmt.Sprintf("%s:service_factor:default", kb.ns.BasePrefix)
}

// ServiceFactorServiceKey builds the key for a per-service factor override.
// Format: {base}:service_factor:service:{serviceID}
// Example: "ha:service_factor:service:eth-mainnet"
func (kb *KeyBuilder) ServiceFactorServiceKey(serviceID string) string {
	return fmt.Sprintf("%s:service_factor:service:%s", kb.ns.BasePrefix, serviceID)
}

// MeterLimitKey builds the key for cached effective limit data.
// Format: {base}:{meter}:limit:{appAddress}:{serviceID}:{sessionID}
// Example: "ha:meter:limit:pokt1abc:eth-mainnet:session123"
func (kb *KeyBuilder) MeterLimitKey(appAddress, serviceID, sessionID string) string {
	return fmt.Sprintf("%s:%s:limit:%s:%s:%s", kb.ns.BasePrefix, kb.ns.MeterPrefix, appAddress, serviceID, sessionID)
}

// MeterLimitInvalidateChannel builds the pub/sub channel for meter limit invalidation.
// Format: {base}:{events}:meter:limit:invalidate
// Example: "ha:events:meter:limit:invalidate"
func (kb *KeyBuilder) MeterLimitInvalidateChannel() string {
	return fmt.Sprintf("%s:%s:meter:limit:invalidate", kb.ns.BasePrefix, kb.ns.EventsPrefix)
}

// MinerClaimKey builds the key for supplier claim locks.
// Format: {base}:{miner}:claim:{supplier}
// Example: "ha:miner:claim:pokt1xyz..."
func (kb *KeyBuilder) MinerClaimKey(supplier string) string {
	return fmt.Sprintf("%s:%s:claim:%s", kb.ns.BasePrefix, kb.ns.MinerPrefix, supplier)
}

// MinerActiveSetKey builds the key for tracking active miner instances.
// Format: {base}:{miner}:active
// Example: "ha:miner:active"
// This is a Redis Set containing instance IDs with TTL heartbeat.
func (kb *KeyBuilder) MinerActiveSetKey() string {
	return fmt.Sprintf("%s:%s:active", kb.ns.BasePrefix, kb.ns.MinerPrefix)
}

// MinerInstanceKey builds the key for individual miner instance registration.
// Format: {base}:{miner}:instance:{instanceID}
// Example: "ha:miner:instance:miner-abc123"
// This key has a TTL and acts as a heartbeat for the instance.
func (kb *KeyBuilder) MinerInstanceKey(instanceID string) string {
	return fmt.Sprintf("%s:%s:instance:%s", kb.ns.BasePrefix, kb.ns.MinerPrefix, instanceID)
}
