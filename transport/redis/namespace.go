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

// NewKeyBuilder creates a new KeyBuilder with the given namespace
// configuration. Empty fields are defaulted individually (WithDefaults), so
// a partially-specified namespace can never produce keys with empty
// segments ("prod::application:x").
func NewKeyBuilder(ns config.RedisNamespaceConfig) *KeyBuilder {
	return &KeyBuilder{ns: ns.WithDefaults()}
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

// MinerSessionKey builds a key for session metadata.
// Format: {base}:{miner}:sessions:{supplier}:{sessionID}
// Example: "ha:miner:sessions:pokt1xyz:session123"
func (kb *KeyBuilder) MinerSessionKey(supplier, sessionID string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:%s", kb.ns.BasePrefix, kb.ns.MinerPrefix, supplier, sessionID)
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

// ParamsSharedCacheKey builds the key for cached shared params singleton.
// Format: {base}:{cache}:shared_params
// Example: "ha:cache:shared_params"
func (kb *KeyBuilder) ParamsSharedCacheKey() string {
	return fmt.Sprintf("%s:%s:shared_params", kb.ns.BasePrefix, kb.ns.CachePrefix)
}

// ParamsSessionKey builds the key for the cached session params singleton.
// Format: {base}:{cache}:session_params
// Example: "ha:cache:session_params"
func (kb *KeyBuilder) ParamsSessionKey() string {
	return fmt.Sprintf("%s:%s:session_params", kb.ns.BasePrefix, kb.ns.CachePrefix)
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

// MeterActiveSessionsKey builds the key for the set tracking active session IDs.
// Used for O(1) counting via SCARD instead of O(N) SCAN.
// Format: {base}:{meter}:active_sessions
// Example: "ha:meter:active_sessions"
func (kb *KeyBuilder) MeterActiveSessionsKey() string {
	return fmt.Sprintf("%s:%s:active_sessions", kb.ns.BasePrefix, kb.ns.MeterPrefix)
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

// SMSTNodesKey builds the key for SMST tree nodes hash.
// Format: {base}:smst:{supplierAddress}:{sessionID}:nodes
// Example: "ha:smst:pokt1abc:session123:nodes"
//
// The supplier address MUST be part of the key. Multiple suppliers can
// participate in the same session, and each has its own distinct SMST
// tree. Keying only by sessionID caused a last-write-wins collision that
// drained supplier stake on leader failover (see 2026-04-16 incident).
func (kb *KeyBuilder) SMSTNodesKey(supplierAddress, sessionID string) string {
	return fmt.Sprintf("%s:smst:%s:%s:nodes", kb.ns.BasePrefix, supplierAddress, sessionID)
}

// SMSTNodesPattern builds the pattern for scanning all SMST node keys.
// Format: {base}:smst:*:*:nodes
// Example: "ha:smst:*:*:nodes"
func (kb *KeyBuilder) SMSTNodesPattern() string {
	return fmt.Sprintf("%s:smst:*:*:nodes", kb.ns.BasePrefix)
}

// SMSTNodesPrefix builds the prefix for SMST node keys (for extracting supplier + sessionID).
// Format: {base}:smst:
// Example: "ha:smst:"
//
// Callers parse the suffix as "{supplierAddress}:{sessionID}:nodes".
func (kb *KeyBuilder) SMSTNodesPrefix() string {
	return fmt.Sprintf("%s:smst:", kb.ns.BasePrefix)
}

// SMSTRootKey builds the key for storing the claimed root hash.
// Format: {base}:smst:{supplierAddress}:{sessionID}:root
// Example: "ha:smst:pokt1abc:session123:root"
func (kb *KeyBuilder) SMSTRootKey(supplierAddress, sessionID string) string {
	return fmt.Sprintf("%s:smst:%s:%s:root", kb.ns.BasePrefix, supplierAddress, sessionID)
}

// SMSTStatsKey builds the key for storing tree statistics (count and sum).
// Format: {base}:smst:{supplierAddress}:{sessionID}:stats
// Example: "ha:smst:pokt1abc:session123:stats"
func (kb *KeyBuilder) SMSTStatsKey(supplierAddress, sessionID string) string {
	return fmt.Sprintf("%s:smst:%s:%s:stats", kb.ns.BasePrefix, supplierAddress, sessionID)
}

// SMSTLiveRootKey builds the key for the intermediate (pre-flush) root of
// an actively-updating SMST. It is written on every UpdateTree so that,
// when a leader dies mid-session, the follower promoted to leader can
// resume the tree at this checkpoint via ImportSparseMerkleSumTrie -
// preserving every relay the dead leader had committed to the shared nodes
// hash but not yet flushed.
//
// Once FlushTree runs, SMSTRootKey (the stable claimed root) supersedes
// this value. Callers that reload a tree from Redis must prefer
// SMSTRootKey and only fall back to SMSTLiveRootKey when no claimed root
// is present (mid-session resume).
//
// Format: {base}:smst:{supplierAddress}:{sessionID}:live_root
// Example: "ha:smst:pokt1abc:session123:live_root"
func (kb *KeyBuilder) SMSTLiveRootKey(supplierAddress, sessionID string) string {
	return fmt.Sprintf("%s:smst:%s:%s:live_root", kb.ns.BasePrefix, supplierAddress, sessionID)
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

// SupplierParamsInvalidateChannel builds the pub/sub channel for supplier
// module param invalidations. NONSTANDARD scheme (predates EventChannel):
// the subscriber (RedisSupplierParamCache, wired by the miner leader) has
// always listened on {base}:{events}:{cache}:invalidate:supplier_params —
// the string is frozen for mixed-fleet compatibility.
// Format: {base}:{events}:{cache}:invalidate:supplier_params
// Example: "ha:events:cache:invalidate:supplier_params"
func (kb *KeyBuilder) SupplierParamsInvalidateChannel() string {
	return fmt.Sprintf("%s:%s:%s:invalidate:supplier_params", kb.ns.BasePrefix, kb.ns.EventsPrefix, kb.ns.CachePrefix)
}

// SharedParamsHeightInvalidateChannel builds the pub/sub channel for the
// relayer's height-keyed shared-params cache (RedisSharedParamCache).
// NONSTANDARD scheme; payload is a numeric height, not JSON. Frozen string.
// Format: {base}:{events}:invalidate:params
// Example: "ha:events:invalidate:params"
func (kb *KeyBuilder) SharedParamsHeightInvalidateChannel() string {
	return fmt.Sprintf("%s:%s:invalidate:params", kb.ns.BasePrefix, kb.ns.EventsPrefix)
}

// SessionRewardableChannel builds the pub/sub channel for session
// rewardable-state updates (RedisSessionCache, relayer). Frozen string.
// Format: {base}:{events}:session:rewardable
// Example: "ha:events:session:rewardable"
func (kb *KeyBuilder) SessionRewardableChannel() string {
	return fmt.Sprintf("%s:%s:session:rewardable", kb.ns.BasePrefix, kb.ns.EventsPrefix)
}

// MinerLeaderPrefix builds the key prefix for per-supplier leader election.
// Format: {base}:{miner}:leader
// Example: "ha:miner:leader"
func (kb *KeyBuilder) MinerLeaderPrefix() string {
	return fmt.Sprintf("%s:%s:leader", kb.ns.BasePrefix, kb.ns.MinerPrefix)
}

// MinerDedupPrefix builds the key prefix for relay deduplication sets.
// Format: {base}:{miner}:dedup
// Example: "ha:miner:dedup"
func (kb *KeyBuilder) MinerDedupPrefix() string {
	return fmt.Sprintf("%s:%s:dedup", kb.ns.BasePrefix, kb.ns.MinerPrefix)
}

// MinerSessionStateIndexKey builds the key for the per-state session index.
// Format: {base}:{miner}:sessions:{supplier}:state:{state}
// Example: "ha:miner:sessions:pokt1abc:state:proved"
func (kb *KeyBuilder) MinerSessionStateIndexKey(supplier, state string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:state:%s", kb.ns.BasePrefix, kb.ns.MinerPrefix, supplier, state)
}

// MinerSessionsIndexKey builds the key for a supplier's session-ID index.
// Format: {base}:{miner}:sessions:{supplier}:index
// Example: "ha:miner:sessions:pokt1abc:index"
func (kb *KeyBuilder) MinerSessionsIndexKey(supplier string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:index", kb.ns.BasePrefix, kb.ns.MinerPrefix, supplier)
}

// TxTrackKey builds the key for claim/proof submission tracking.
// The "tx:track" segment is literal (no configurable sub-prefix existed
// historically); only the base prefix is namespaced.
// Format: {base}:tx:track:{supplier}:{sessionEndHeight}:{sessionID}
// Example: "ha:tx:track:pokt1abc:100:sess1"
func (kb *KeyBuilder) TxTrackKey(supplier string, sessionEndHeight int64, sessionID string) string {
	return fmt.Sprintf("%s:tx:track:%s:%d:%s", kb.ns.BasePrefix, supplier, sessionEndHeight, sessionID)
}

// TxTrackPattern builds the SCAN pattern for a supplier's submission tracking.
// Format: {base}:tx:track:{supplier}:*
// Example: "ha:tx:track:pokt1abc:*"
func (kb *KeyBuilder) TxTrackPattern(supplier string) string {
	return fmt.Sprintf("%s:tx:track:%s:*", kb.ns.BasePrefix, supplier)
}

// TxTrackAllPattern builds the SCAN pattern for every supplier's submission
// tracking (the debug CLI's unfiltered list).
// Format: {base}:tx:track:*
// Example: "ha:tx:track:*"
func (kb *KeyBuilder) TxTrackAllPattern() string {
	return fmt.Sprintf("%s:tx:track:*", kb.ns.BasePrefix)
}

// AllKeysPattern builds the SCAN pattern matching every key in the namespace.
// Used by the debug flush command's "delete everything" path.
// Format: {base}:*
// Example: "ha:*"
func (kb *KeyBuilder) AllKeysPattern() string {
	return fmt.Sprintf("%s:*", kb.ns.BasePrefix)
}

// RebroadcastKey builds the per-group payload-hash key for the inclusion
// reconciler's rebroadcast store. The phase is wrapped in a Redis Cluster
// hash tag ({phase}) so a phase's group hashes and its index set resolve to
// the same slot (required for the multi-key MULTI/EXEC and Lua); on
// standalone Redis the braces are inert.
// Format: {base}:{miner}:rebroadcast:{'{'}{phase}{'}'}:{supplier}:{sessionEnd}
// Example: "ha:miner:rebroadcast:{claim}:pokt1abc:100"
func (kb *KeyBuilder) RebroadcastKey(phase, supplier string, sessionEnd int64) string {
	return fmt.Sprintf("%s:%s:rebroadcast:{%s}:%s:%d", kb.ns.BasePrefix, kb.ns.MinerPrefix, phase, supplier, sessionEnd)
}

// RebroadcastIndexKey builds the per-phase group index set key for the
// rebroadcast store. The phase hash tag matches RebroadcastKey so both live
// in the same cluster slot.
// Format: {base}:{miner}:rebroadcast:{'{'}{phase}{'}'}:index
// Example: "ha:miner:rebroadcast:{claim}:index"
func (kb *KeyBuilder) RebroadcastIndexKey(phase string) string {
	return fmt.Sprintf("%s:%s:rebroadcast:{%s}:index", kb.ns.BasePrefix, kb.ns.MinerPrefix, phase)
}

// MinerDedupSessionKey builds the per-session relay deduplication set key.
// Format: {base}:{miner}:dedup:session:{sessionID}
// Example: "ha:miner:dedup:session:sess1"
func (kb *KeyBuilder) MinerDedupSessionKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:dedup:session:%s", kb.ns.BasePrefix, kb.ns.MinerPrefix, sessionID)
}

// MeterSessionKey builds the per-session relay metering hash key.
// Format: {base}:{meter}:{sessionID}
// Example: "ha:meter:sess1"
func (kb *KeyBuilder) MeterSessionKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:%s", kb.ns.BasePrefix, kb.ns.MeterPrefix, sessionID)
}

// AppStakeKey builds the per-application stake-tracking key used by the meter
// subsystem. The "app_stake" segment is literal (no configurable sub-prefix).
// Format: {base}:app_stake:{appAddr}
// Example: "ha:app_stake:app1"
func (kb *KeyBuilder) AppStakeKey(appAddr string) string {
	return fmt.Sprintf("%s:app_stake:%s", kb.ns.BasePrefix, appAddr)
}

// ServiceComputeUnitsKey builds the per-service compute-units key used by the
// meter subsystem. The "service"/"compute_units" segments are literal.
// Format: {base}:service:{serviceID}:compute_units
// Example: "ha:service:svc1:compute_units"
func (kb *KeyBuilder) ServiceComputeUnitsKey(serviceID string) string {
	return fmt.Sprintf("%s:service:%s:compute_units", kb.ns.BasePrefix, serviceID)
}
