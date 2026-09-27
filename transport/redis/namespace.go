package redis

import (
	"fmt"
	"strings"

	"github.com/pokt-network/pocket-relay-miner/config"
)

// The key layout below the operator's base prefix is OURS, not configuration.
//
// Every segment here used to be an operator knob, and that is precisely how two
// families could be made to collide: SupplierStateKey took its segment from the
// config while the registry family hardcoded "suppliers", so supplier_prefix:
// "suppliers" produced one key with two different structs writing it, and
// cache_prefix: "supplier" made SupplierStatePattern() ("ha:supplier:*") match
// every cache key. A knob that can be turned until it equals another family's
// constant is the whole bug class.
//
// With only the base configurable the collision cannot be expressed: two
// families differ on a literal in the segment right after the base, under any
// base. The base is safe by position -- it is the first segment of every key, so
// two bases are two disjoint keyspaces, which is the only thing an operator
// actually needs (one Redis, several fleets).
//
// These live with the KeyBuilder on purpose. They used to live in config, which
// meant the package that does NOT build keys owned the strings that define them
// -- that split is how one family drifted to a hardcoded literal while its twin
// still read the config field.
//
// CHANGING ANY VALUE HERE IS A BREAKING, CROSS-VERSION CHANGE: a mixed fleet
// only keeps working while every binary builds the same strings. The golden
// tests in namespace_test.go pin all of them.
const (
	segmentCache         = "cache"
	segmentEvents        = "events"
	segmentStreams       = "relays"
	segmentMiner         = "miner"
	segmentSupplier      = "supplier"
	segmentMeter         = "meter"
	segmentParams        = "params"
	segmentConsumerGroup = "miners"
)

// KeyBuilder builds Redis keys with configured prefixes.
// This eliminates hardcoded "ha:" strings scattered throughout the codebase.
type KeyBuilder struct {
	ns config.RedisNamespaceConfig
}

// NewKeyBuilder creates a new KeyBuilder for the given namespace. Only the base
// prefix comes from the operator; every segment below it is a constant above, so
// an empty or partial namespace can never produce a key with an empty segment
// ("prod::application:x").
func NewKeyBuilder(ns config.RedisNamespaceConfig) *KeyBuilder {
	return &KeyBuilder{ns: ns.WithDefaults()}
}

// CacheKey builds a cache key for an entity.
// Format: {base}:{cache}:{entityType}:{key}
// Example: "ha:cache:application:pokt1abc..."
func (kb *KeyBuilder) CacheKey(entityType, key string) string {
	return fmt.Sprintf("%s:%s:%s:%s", kb.ns.BasePrefix, segmentCache, entityType, key)
}

// CacheLockKey builds a distributed lock key for cache population.
// Format: {base}:{cache}:lock:{entityType}:{key}
// Example: "ha:cache:lock:application:pokt1abc..."
func (kb *KeyBuilder) CacheLockKey(entityType, key string) string {
	return fmt.Sprintf("%s:%s:lock:%s:%s", kb.ns.BasePrefix, segmentCache, entityType, key)
}

// CacheKnownKey builds a key for tracking known entities of a type.
// Format: {base}:{cache}:known:{entityType}
// Example: "ha:cache:known:applications"
func (kb *KeyBuilder) CacheKnownKey(entityType string) string {
	return fmt.Sprintf("%s:%s:known:%s", kb.ns.BasePrefix, segmentCache, entityType)
}

// EventChannel builds a pub/sub channel name for cache invalidation.
// Format: {base}:{events}:cache:{cacheType}:invalidate
// Example: "ha:events:cache:application:invalidate"
func (kb *KeyBuilder) EventChannel(cacheType, event string) string {
	return fmt.Sprintf("%s:%s:cache:%s:%s", kb.ns.BasePrefix, segmentEvents, cacheType, event)
}

// StreamPrefix returns the stream namespace prefix.
// Format: {base}:{streams}
// Example: "ha:relays"
func (kb *KeyBuilder) StreamPrefix() string {
	return fmt.Sprintf("%s:%s", kb.ns.BasePrefix, segmentStreams)
}

// StreamPattern returns the SCAN glob that matches every supplier relay stream
// in this namespace.
//
// It exists so nobody hand-builds `StreamPrefix() + ":*"`. The key-literal
// convention check cannot see a pattern assembled that way -- it matches literal
// prefixes, not concatenations -- so a hand-built pattern passes CI silently and
// then matches nothing on a deployment with a custom base prefix.
// Format: {base}:{streams}:*
// Example: "ha:relays:*"
func (kb *KeyBuilder) StreamPattern() string {
	return fmt.Sprintf("%s:%s:*", kb.ns.BasePrefix, segmentStreams)
}

// ConsumerGroup returns the consumer group name for Redis Streams.
// Format: {base}-{consumer_group_prefix}
// Example: "ha-miners"
func (kb *KeyBuilder) ConsumerGroup() string {
	return fmt.Sprintf("%s-%s", kb.ns.BasePrefix, segmentConsumerGroup)
}

// MinerSessionKey builds a key for session metadata.
// Format: {base}:{miner}:sessions:{supplier}:{sessionID}
// Example: "ha:miner:sessions:pokt1xyz:session123"
func (kb *KeyBuilder) MinerSessionKey(supplier, sessionID string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:%s", kb.ns.BasePrefix, segmentMiner, supplier, sessionID)
}

// SupplierKeyPrefix returns the base prefix for supplier keys.
// Format: {base}:{supplier}
// Example: "ha:supplier"
func (kb *KeyBuilder) SupplierKeyPrefix() string {
	return fmt.Sprintf("%s:%s", kb.ns.BasePrefix, segmentSupplier)
}

// StreamKey builds the relay WAL stream key for one supplier.
// Format: {base}:{streams}:{supplierOperatorAddress}
// Example: "ha:relays:pokt1abc"
func (kb *KeyBuilder) StreamKey(supplierOperatorAddress string) string {
	return fmt.Sprintf("%s:%s:%s", kb.ns.BasePrefix, segmentStreams, supplierOperatorAddress)
}

// StreamAddress extracts the supplier operator address a StreamKey was built
// for, given a key that matched StreamPattern(). Returns ("", false) if key
// is not one of this namespace's stream keys.
//
// It exists so nobody hand-builds StreamPrefix()+":" to strip a scanned key
// back to its address — the same class of drift StreamPattern() was added
// to stop for the SCAN side (review 2026-08-20, LOW; recurred on the trim
// side in the very commit meant to demonstrate the lesson, review
// 2026-08-21).
func (kb *KeyBuilder) StreamAddress(key string) (string, bool) {
	addr := strings.TrimPrefix(key, kb.StreamPrefix()+":")
	if addr == key || addr == "" {
		return "", false
	}
	return addr, true
}

// SuppliersRegistryIndexKey returns the index key for suppliers registry: the
// set of supplier addresses THIS FLEET handles.
//
// It is the only ha:suppliers:* key. The plural is a set; the singular
// (SupplierStateKey, ha:supplier:{addr}) is one supplier's network state. There
// used to be a per-supplier value here too, differing from the cache key by one
// letter, with no readers and a latent collision: back when each family took its
// segment from config, SupplierStateKey read ns.SupplierPrefix while this family
// hardcoded "suppliers", so supplier_prefix: "suppliers" made the two identical.
// Both are constants now, which is what makes that unrepresentable. Do not
// add a per-supplier key under this prefix; supplier state has a home.
//
// That removes the EXACT-key collision. A glob one survives under the same
// setting: SupplierStatePattern() is "ha:suppliers:*", which matches THIS key,
// so nothing may scan-and-delete by that pattern without checking what it
// actually matched.
// Format: {base}:suppliers:index
// Example: "ha:suppliers:index"
func (kb *KeyBuilder) SuppliersRegistryIndexKey() string {
	return fmt.Sprintf("%s:suppliers:index", kb.ns.BasePrefix)
}

// CachePrefix returns the full cache prefix.
// Format: {base}:{cache}
// Example: "ha:cache"
func (kb *KeyBuilder) CachePrefix() string {
	return fmt.Sprintf("%s:%s", kb.ns.BasePrefix, segmentCache)
}

// ParamsSharedAtHeightKey builds the relayer-side shared-params cache key for
// one block height (immutable snapshot; ParamsSharedCacheKey is the miner-side
// singleton holding the latest value).
// Format: {base}:{cache}:params:shared:{height}
func (kb *KeyBuilder) ParamsSharedAtHeightKey(height int64) string {
	return fmt.Sprintf("%s:%s:params:shared:%d", kb.ns.BasePrefix, segmentCache, height)
}

// ParamsSharedAtHeightLockKey builds the distributed-lock key guarding one
// height's shared-params refresh.
// Format: {base}:{cache}:lock:params:shared:{height}
func (kb *KeyBuilder) ParamsSharedAtHeightLockKey(height int64) string {
	return fmt.Sprintf("%s:%s:lock:params:shared:%d", kb.ns.BasePrefix, segmentCache, height)
}

// ParamsSupplierKey builds the relayer-side supplier-params cache key
// (singleton, not height-based).
// Format: {base}:{cache}:params:supplier
func (kb *KeyBuilder) ParamsSupplierKey() string {
	return fmt.Sprintf("%s:%s:params:supplier", kb.ns.BasePrefix, segmentCache)
}

// ParamsSupplierLockKey builds the distributed-lock key guarding the
// supplier-params refresh.
// Format: {base}:{cache}:lock:params:supplier
func (kb *KeyBuilder) ParamsSupplierLockKey() string {
	return fmt.Sprintf("%s:%s:lock:params:supplier", kb.ns.BasePrefix, segmentCache)
}

// SessionCacheKey builds the relayer-side session cache key for an
// (application, service) pair at a session-start height.
// Format: {base}:{cache}:session:{app}:{service}:{height}
func (kb *KeyBuilder) SessionCacheKey(appAddr, serviceID string, height int64) string {
	return fmt.Sprintf("%s:%s:session:%s:%s:%d", kb.ns.BasePrefix, segmentCache, appAddr, serviceID, height)
}

// MinerSessionsPrefix returns the prefix for miner session store.
// Format: {base}:{miner}:sessions
// Example: "ha:miner:sessions"
func (kb *KeyBuilder) MinerSessionsPrefix() string {
	return fmt.Sprintf("%s:%s:sessions", kb.ns.BasePrefix, segmentMiner)
}

// GlobalLeaderKey returns the key for global leader election.
// Format: {base}:{miner}:global_leader
// Example: "ha:miner:global_leader"
func (kb *KeyBuilder) GlobalLeaderKey() string {
	return fmt.Sprintf("%s:%s:global_leader", kb.ns.BasePrefix, segmentMiner)
}

// ParamsProofKey builds the key for cached proof params.
// Format: {base}:{cache}:proof_params
// Example: "ha:cache:proof_params"
func (kb *KeyBuilder) ParamsProofKey() string {
	return fmt.Sprintf("%s:%s:proof_params", kb.ns.BasePrefix, segmentCache)
}

// ParamsProofLockKey builds the lock key for proof params cache population.
// Format: {base}:{cache}:lock:proof_params
// Example: "ha:cache:lock:proof_params"
func (kb *KeyBuilder) ParamsProofLockKey() string {
	return fmt.Sprintf("%s:%s:lock:proof_params", kb.ns.BasePrefix, segmentCache)
}

// ParamsSharedCacheKey builds the key for cached shared params singleton.
// Format: {base}:{cache}:shared_params
// Example: "ha:cache:shared_params"
func (kb *KeyBuilder) ParamsSharedCacheKey() string {
	return fmt.Sprintf("%s:%s:shared_params", kb.ns.BasePrefix, segmentCache)
}

// ParamsSharedLockKey builds the lock key for shared params cache population.
// Format: {base}:{cache}:lock:shared_params
// Example: "ha:cache:lock:shared_params"
func (kb *KeyBuilder) ParamsSharedLockKey() string {
	return fmt.Sprintf("%s:%s:lock:shared_params", kb.ns.BasePrefix, segmentCache)
}

// MeterCleanupChannel builds the pub/sub channel for meter cleanup events.
// Format: {base}:{meter}:cleanup
// Example: "ha:meter:cleanup"
func (kb *KeyBuilder) MeterCleanupChannel() string {
	return fmt.Sprintf("%s:%s:cleanup", kb.ns.BasePrefix, segmentMeter)
}

// MeterActiveSessionsKey builds the key for the set tracking active session IDs.
// Used for O(1) counting via SCARD instead of O(N) SCAN.
// Format: {base}:{meter}:active_sessions
// Example: "ha:meter:active_sessions"
func (kb *KeyBuilder) MeterActiveSessionsKey() string {
	return fmt.Sprintf("%s:%s:active_sessions", kb.ns.BasePrefix, segmentMeter)
}

// SimulationReplayKey builds the shared replay-dedup key for a simulated
// relay, keyed by the hex-encoded signature hash. A short TTL (the freshness
// window) is set by the caller so a captured simulated relay cannot be
// replayed across the HA fleet within the window. Shared (Redis) state is
// required for correctness: a per-replica cache would let an attacker replay
// the same request once to each replica.
// Format: {base}:sim:replay:{sigHash}
// Example: "ha:sim:replay:deadbeef"
func (kb *KeyBuilder) SimulationReplayKey(sigHash string) string {
	return fmt.Sprintf("%s:sim:replay:%s", kb.ns.BasePrefix, sigHash)
}

// BlockEventChannel builds the pub/sub channel for block events.
// Format: {base}:{events}:blocks
// Example: "ha:events:blocks"
func (kb *KeyBuilder) BlockEventChannel() string {
	return fmt.Sprintf("%s:%s:blocks", kb.ns.BasePrefix, segmentEvents)
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
	return fmt.Sprintf("%s:%s:claim:%s", kb.ns.BasePrefix, segmentMiner, supplier)
}

// MinerActiveSetKey builds the key for tracking active miner instances.
// Format: {base}:{miner}:active
// Example: "ha:miner:active"
// This is a Redis Set containing instance IDs with TTL heartbeat.
func (kb *KeyBuilder) MinerActiveSetKey() string {
	return fmt.Sprintf("%s:%s:active", kb.ns.BasePrefix, segmentMiner)
}

// MinerInstanceKey builds the key for individual miner instance registration.
// Format: {base}:{miner}:instance:{instanceID}
// Example: "ha:miner:instance:miner-abc123"
// This key has a TTL and acts as a heartbeat for the instance.
func (kb *KeyBuilder) MinerInstanceKey(instanceID string) string {
	return fmt.Sprintf("%s:%s:instance:%s", kb.ns.BasePrefix, segmentMiner, instanceID)
}

// SupplierParamsInvalidateChannel builds the pub/sub channel for supplier
// module param invalidations. NONSTANDARD scheme (predates EventChannel):
// the subscriber (RedisSupplierParamCache, wired by the miner leader) has
// always listened on {base}:{events}:{cache}:invalidate:supplier_params —
// the string is frozen for mixed-fleet compatibility.
// Format: {base}:{events}:{cache}:invalidate:supplier_params
// Example: "ha:events:cache:invalidate:supplier_params"
func (kb *KeyBuilder) SupplierParamsInvalidateChannel() string {
	return fmt.Sprintf("%s:%s:%s:invalidate:supplier_params", kb.ns.BasePrefix, segmentEvents, segmentCache)
}

// MinerDedupPrefix builds the key prefix for relay deduplication sets.
// Format: {base}:{miner}:dedup
// Example: "ha:miner:dedup"
func (kb *KeyBuilder) MinerDedupPrefix() string {
	return fmt.Sprintf("%s:%s:dedup", kb.ns.BasePrefix, segmentMiner)
}

// MinerSessionStateIndexKey builds the key for the per-state session index.
// Format: {base}:{miner}:sessions:{supplier}:state:{state}
// Example: "ha:miner:sessions:pokt1abc:state:proved"
func (kb *KeyBuilder) MinerSessionStateIndexKey(supplier, state string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:state:%s", kb.ns.BasePrefix, segmentMiner, supplier, state)
}

// MinerSessionsIndexKey builds the key for a supplier's session-ID index.
// Format: {base}:{miner}:sessions:{supplier}:index
// Example: "ha:miner:sessions:pokt1abc:index"
func (kb *KeyBuilder) MinerSessionsIndexKey(supplier string) string {
	return fmt.Sprintf("%s:%s:sessions:%s:index", kb.ns.BasePrefix, segmentMiner, supplier)
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
	return fmt.Sprintf("%s:%s:rebroadcast:{%s}:%s:%d", kb.ns.BasePrefix, segmentMiner, phase, supplier, sessionEnd)
}

// RebroadcastIndexKey builds the per-phase group index set key for the
// rebroadcast store. The phase hash tag matches RebroadcastKey so both live
// in the same cluster slot.
// Format: {base}:{miner}:rebroadcast:{'{'}{phase}{'}'}:index
// Example: "ha:miner:rebroadcast:{claim}:index"
func (kb *KeyBuilder) RebroadcastIndexKey(phase string) string {
	return fmt.Sprintf("%s:%s:rebroadcast:{%s}:index", kb.ns.BasePrefix, segmentMiner, phase)
}

// MinerDedupSessionKey builds the per-session relay deduplication set key.
// Format: {base}:{miner}:dedup:session:{sessionID}
// Example: "ha:miner:dedup:session:sess1"
func (kb *KeyBuilder) MinerDedupSessionKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:dedup:session:%s", kb.ns.BasePrefix, segmentMiner, sessionID)
}

// MeterPrefix returns the prefix every metering key lives under.
// Format: {base}:meter
// Example: "ha:meter"
//
// It exists so nothing compares or rebuilds that prefix by hand: relayer config
// validation compares the retired relay_meter.redis_key_prefix against where
// meter keys actually live, and building "{base}:{meter}" at the call site is
// how that check silently started comparing against "ha:" once the segment
// stopped coming from config.
func (kb *KeyBuilder) MeterPrefix() string {
	return fmt.Sprintf("%s:%s", kb.ns.BasePrefix, segmentMeter)
}

// MeterSessionKey builds the per-session relay metering key PREFIX.
//
// Metering is stored per (session, supplier), not per session: one session is
// served by many suppliers and each meters its own stake independently. This
// method therefore addresses no key on its own -- it is the prefix the
// per-supplier keys hang off, useful for SCAN patterns. Read a supplier's
// metering through MeterMetaKey and MeterConsumedKey.
//
// Format: {base}:{meter}:{sessionID}
// Example: "ha:meter:sess1"
func (kb *KeyBuilder) MeterSessionKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:%s", kb.ns.BasePrefix, segmentMeter, sessionID)
}

// MeterMetaKey builds the per-(session, supplier) metering metadata hash key
// written by the relayer's relay meter.
// Format: {base}:{meter}:{sessionID}:{supplier}:meta
// Example: "ha:meter:sess1:pokt1abc:meta"
func (kb *KeyBuilder) MeterMetaKey(sessionID, supplierAddress string) string {
	return fmt.Sprintf("%s:%s:%s:%s:meta",
		kb.ns.BasePrefix, segmentMeter, sessionID, supplierAddress)
}

// MeterConsumedKey builds the per-(session, supplier) consumed-stake key
// written by the relayer's relay meter.
// Format: {base}:{meter}:{sessionID}:{supplier}:consumed
// Example: "ha:meter:sess1:pokt1abc:consumed"
func (kb *KeyBuilder) MeterConsumedKey(sessionID, supplierAddress string) string {
	return fmt.Sprintf("%s:%s:%s:%s:consumed",
		kb.ns.BasePrefix, segmentMeter, sessionID, supplierAddress)
}

// MeterSessionMetaPattern builds the SCAN pattern matching every supplier's
// metering metadata for one session. A session is served by several suppliers
// and each meters separately, so inspection has to discover them rather than
// address a single key.
// Format: {base}:{meter}:{sessionID}:*:meta
// Example: "ha:meter:sess1:*:meta"
func (kb *KeyBuilder) MeterSessionMetaPattern(sessionID string) string {
	return fmt.Sprintf("%s:%s:%s:*:meta", kb.ns.BasePrefix, segmentMeter, sessionID)
}

// SupplierStateKey builds the key for one supplier's shared state entry.
// Format: {base}:{supplier}:{operatorAddress}
// Example: "ha:supplier:pokt1abc"
func (kb *KeyBuilder) SupplierStateKey(operatorAddress string) string {
	return fmt.Sprintf("%s:%s:%s", kb.ns.BasePrefix, segmentSupplier, operatorAddress)
}

// SupplierStatePattern builds the SCAN pattern for all supplier state entries.
// Format: {base}:{supplier}:*
// Example: "ha:supplier:*"
func (kb *KeyBuilder) SupplierStatePattern() string {
	return fmt.Sprintf("%s:%s:*", kb.ns.BasePrefix, segmentSupplier)
}

// SupplierStateAddress extracts the operator address a SupplierStateKey was
// built for, given a key that matched SupplierStatePattern(). Returns
// ("", false) if key is not one of this namespace's supplier state keys.
//
// It exists for the same reason StreamAddress does: nobody hand-builds
// SupplierKeyPrefix()+":" to strip a scanned key back to its address (review
// 2026-08-20, LOW; recurred review 2026-08-21).
//
// A remainder containing ":" is rejected. A supplier state key has exactly one
// segment after the prefix, so anything else is a key from another family that
// happens to share the prefix -- which is precisely what a namespace could
// produce while the segments were configurable: under cache_prefix "supplier"
// this returned ("application:k1", true) for a CACHE key and called it an
// address. The layout is constant now and the caller scans a pattern that no
// longer reaches other families, so this cannot be hit from either call site;
// it is stated anyway, because an extractor that answers confidently for a key
// it does not own is the kind of thing the next caller trusts.
func (kb *KeyBuilder) SupplierStateAddress(key string) (string, bool) {
	addr := strings.TrimPrefix(key, kb.SupplierKeyPrefix()+":")
	if addr == key || addr == "" || strings.Contains(addr, ":") {
		return "", false
	}
	return addr, true
}

// SMSTSessionNodesPattern builds the SCAN pattern matching a session's SMST
// nodes hashes across all suppliers.
// Format: {base}:smst:*:{sessionID}:nodes
// Example: "ha:smst:*:sess1:nodes"
func (kb *KeyBuilder) SMSTSessionNodesPattern(sessionID string) string {
	return fmt.Sprintf("%s:smst:*:%s:nodes", kb.ns.BasePrefix, sessionID)
}

// LegacyParamsPattern builds the SCAN pattern for legacy metering params.
// Format: {base}:params:*
// Example: "ha:params:*"
func (kb *KeyBuilder) LegacyParamsPattern() string {
	return fmt.Sprintf("%s:%s:*", kb.ns.BasePrefix, segmentParams)
}
