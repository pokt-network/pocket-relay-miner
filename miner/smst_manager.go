// Package miner, SMST subsystem.
//
// # Operator runbook: diagnosing claim/ComputeUnits loss
//
// If on-chain ComputeUnits are lower than the supplier's actual RPS
// would suggest, walk these signals in order:
//
//  1. Miner metrics (exposed at the miner's /metrics endpoint):
//     - ha_smst_panics_recovered_total{supplier,operation}: any
//     non-zero rate means the SMT library panicked on corrupt state
//     (missing node, malformed payload) and the defensive boundary
//     caught it. Root cause is almost always Redis-side data loss.
//     - ha_smst_corruption_evictions_total{supplier,reason}: a
//     session was dropped from memory because its tree was deemed
//     unreliable. The session's relays get undercounted in the
//     claim (only post-eviction relays make it in).
//
//  2. Miner logs (grep these strings):
//     - "SMT library panic recovered at miner boundary" — full stack
//     and smt_op field show which call tripped.
//     - "SMST node missing from store" — ErrSMSTNodeMissing bubbled
//     up; a Redis hash field that should be present was absent.
//     - "evicted corrupt SMST session from memory" — emitted on every
//     eviction with the reason label (update_tree_corruption,
//     flush_tree_corruption, prove_closest_corruption, ...).
//
//  3. Redis server state:
//     - INFO memory → evicted_keys should be 0. Any non-zero means
//     Redis ran out of memory and evicted keys; if the nodes hash
//     is among them, every in-flight session on this supplier
//     corrupts simultaneously.
//     - CONFIG GET maxmemory-policy → must be "noeviction". Any
//     *-lru, *-lfu, or *-random policy will silently evict SMST
//     data under memory pressure and cause this exact failure mode.
//     - CONFIG GET maxmemory → size for the supplier footprint:
//     ~200KB per active session (nodes hash + dedup set +
//     snapshot) × concurrent sessions × active suppliers, plus
//     stream backlogs.
//
//  4. Miner config (config.miner.yaml):
//     - cache_ttl: must exceed the longest session lifecycle
//     (session window + claim window + proof window + buffer).
//     With sliding-TTL refresh (FlushOrphansWithLiveRoot), this
//     auto-extends while relays keep coming in, but a session
//     that idles past cache_ttl will still expire — keep ≥ 2h.
//     - smst_live_root_checkpoint_interval: defaults to 10. Sized
//     against the fact that protocol difficulty bounds how many
//     relays reach the tree, so losing up to 9 per active session
//     on a restart is proportionally small. Lower to 1 for exact
//     claim fidelity at the cost of 10× more TxPipeline round-trips
//     to Redis.
package miner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/smt"
	"github.com/pokt-network/smt/kvstore"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
)

const (
	// FlushPollInterval is the time to wait for in-flight UpdateTree calls to complete
	// during the sealing process.
	FlushPollInterval = 10 * time.Millisecond

	// RedisScanBatchSize is the number of keys to scan per Redis SCAN iteration
	// when warming up SMST trees from Redis.
	RedisScanBatchSize = 100

	// DefaultLiveRootCheckpointInterval is the number of UpdateTree
	// calls between writes of the intermediate root to Redis. This is
	// the bound on relay loss if the miner dies between checkpoints:
	// up to (interval - 1) relays that were committed to the nodes
	// hash but not yet represented in a stored live_root get dropped
	// on resume.
	//
	// Default 10. Rationale: protocol relay-mining difficulty scales
	// with aggregate network RPS, so the count of relays that actually
	// meet difficulty and reach UpdateTree stays bounded regardless of
	// how much raw traffic the relayer signs. Trees tend toward similar
	// sizes across load levels, which makes a worst-case 9-relay loss
	// per session per process restart proportionally small — while the
	// 10× reduction in TxPipeline round-trips (HDEL orphans + SET
	// live_root + EXPIRE × 2) meaningfully cuts Redis load, which
	// matters given the larger Redis footprint in recent releases
	// (HINCRBY session counters, dedup sets, stream backlogs, etc.).
	//
	// Operators who value zero-loss-per-restart over Redis throughput
	// can set smst_live_root_checkpoint_interval: 1 in
	// config.miner.yaml.
	DefaultLiveRootCheckpointInterval = 10
)

// flushTreeSealWaitHook is a test-only hook that fires during FlushTree's
// Phase-3 unlock-and-wait window, after rootAfterSeal has been captured and
// before the lock is re-acquired for rootAfterWait. Production code leaves
// this nil; tests can install a function to force a mismatch between the
// two captured roots so the Phase-4 resolution branch can be exercised.
//
// Never set this outside of tests.
var flushTreeSealWaitHook func(sessionID string)

// RedisSMSTManagerConfig contains configuration for the SMST manager.
type RedisSMSTManagerConfig struct {
	// SupplierAddress is the supplier this manager is for.
	SupplierAddress string

	// CacheTTL is how long to keep SMST data in Redis (backup if manual cleanup fails).
	CacheTTL time.Duration

	// LiveRootCheckpointInterval is the number of UpdateTree calls
	// between writes of the intermediate root to Redis. See
	// DefaultLiveRootCheckpointInterval for the trade-off and the
	// current default value. Zero falls back to the default. Raise
	// this only if Redis write throughput is the bottleneck — the
	// loss bound per process restart is (interval - 1) relays per
	// active session.
	LiveRootCheckpointInterval int
}

// liveRootInterval returns the configured checkpoint interval or the
// default if unset. Always >= 1.
func (m *RedisSMSTManager) liveRootInterval() int {
	if m.config.LiveRootCheckpointInterval > 0 {
		return m.config.LiveRootCheckpointInterval
	}
	return DefaultLiveRootCheckpointInterval
}

// runSMSTSafely invokes fn at the boundary between the miner and the
// pokt-network/smt library and converts any panic from the library
// into ErrSMSTPanicRecovered. This is the defensive barrier that
// guarantees a corrupt Redis hash (missing inner nodes, truncated
// payloads, internal library assertion violations) cannot tumble the
// relay-consumer goroutine — the supplier would stop processing
// relays entirely until a miner restart, which is what the Anaski
// 2026-04-17/19 incidents produced.
//
// The recovered value is logged with a full stack trace so the
// corruption is still observable in Loki. A metric
// smst_panics_recovered_total is incremented per supplier/operation so
// alerts can fire on any non-zero rate. The returned error is a
// wrapped ErrSMSTPanicRecovered which IsPermanentSMSTError treats as
// "drop this relay, keep serving others" (the relay could be retried
// but the session's in-memory tree is evicted first so subsequent
// relays start from Redis state).
func (m *RedisSMSTManager) runSMSTSafely(sessionID, op string, fn func() error) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		stack := debug.Stack()
		m.logger.Error().
			Str(logging.FieldSessionID, sessionID).
			Str("smt_op", op).
			Str("panic_value", fmt.Sprintf("%v", r)).
			Bytes("stack", stack).
			Msg("SMT library panic recovered at miner boundary — treating as data corruption")
		observability.SMSTPanicsRecovered.
			WithLabelValues(m.config.SupplierAddress, op).Inc()
		err = fmt.Errorf("%w: op=%s panic=%v", ErrSMSTPanicRecovered, op, r)
	}()
	return fn()
}

// evictCorruptSessionLocked drops only the session's in-memory cached
// tree. Redis-backed state (nodes hash, live_root, claimed_root, stats)
// is deliberately preserved so the next UpdateTree's GetOrCreateTree can
// attempt resumeTreeFromRedisLocked:
//
//   - If live_root's subtree is still intact in Redis (the corruption was
//     in-memory only, e.g. an spurious library assertion caught by
//     runSMSTSafely), resume succeeds and the session keeps processing
//     relays with its full pre-corruption count.
//   - If Redis state is ALSO corrupt, resumeTreeFromRedisLocked's
//     runSMSTSafely wrapper on ImportSparseMerkleSumTrie catches the
//     import panic, deletes the poisonous root key (live_root or
//     claimed_root), and returns nil. The caller then creates a fresh
//     empty tree. No data recoverable, but the session unwedges instead
//     of looping evictions forever.
//
// Why not delete the nodes hash eagerly: in the common case the nodes
// hash is mostly intact and only one node is missing. Wiping all of it
// guarantees full data loss for the session, which defeats the purpose
// of having a persistent backing store. The CacheTTL (sliding) will
// reclaim any abandoned bloat.
//
// Operators debugging corruption should correlate these signals:
//   - Metric smst_corruption_evictions_total{supplier,reason} rate > 0
//   - Log line "evicted corrupt SMST session" (this function)
//   - Redis INFO: evicted_keys counter climbing (maxmemory eviction is
//     the most common root cause)
//   - Redis CONFIG GET maxmemory-policy — MUST be "noeviction" for
//     correctness; any *-lru/*-lfu/*-random will silently evict the
//     nodes hash mid-session and corrupt the SMST.
//
// Caller must hold m.treesMu.
func (m *RedisSMSTManager) evictCorruptSessionLocked(ctx context.Context, sessionID, reason string) {
	delete(m.trees, sessionID)

	observability.SMSTCorruptionEvictions.
		WithLabelValues(m.config.SupplierAddress, reason).Inc()

	// Track consecutive evictions; UpdateTree resets this to 0 on success.
	m.evictionCounts[sessionID]++
	consecutive := m.evictionCounts[sessionID]

	// Below the threshold: preserve Redis so a transient in-memory failure
	// can recover from the backing store on the next UpdateTree.
	if consecutive < persistentCorruptionThreshold {
		m.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Str("reason", reason).
			Int("consecutive_evictions", consecutive).
			Int("purge_threshold", persistentCorruptionThreshold).
			Msg("evicted corrupt SMST session from memory — next UpdateTree will attempt Redis resume")
		return
	}

	// Persistent corruption: Redis state itself is poisoned (e.g. pre-TTL
	// legacy keys whose nodes hash diverged from live_root, or a mid-write
	// crash that left dangling references). Preserving it only feeds the
	// evict→resume→fail loop. Purge the 4 session-scoped keys so the next
	// UpdateTree creates a fresh tree. Data loss is bounded to the
	// session's current leaf count — the same loss incurred on any
	// mid-session HA failover without a live_root, and far cheaper than
	// the alternative (leaking a hot-loop session indefinitely).
	supplier := m.config.SupplierAddress
	keys := []string{
		m.redisClient.KB().SMSTRootKey(supplier, sessionID),     // claimed_root
		m.redisClient.KB().SMSTLiveRootKey(supplier, sessionID), // live_root
		m.redisClient.KB().SMSTStatsKey(supplier, sessionID),    // stats
		m.redisClient.KB().SMSTNodesKey(supplier, sessionID),    // nodes hash
	}
	delCount, delErr := m.redisClient.Del(ctx, keys...).Result()

	observability.SMSTCorruptionPurged.
		WithLabelValues(supplier, reason).Inc()

	logEvent := m.logger.Warn().
		Str(logging.FieldSessionID, sessionID).
		Str("reason", reason).
		Int("consecutive_evictions", consecutive).
		Int("purge_threshold", persistentCorruptionThreshold).
		Int64("keys_deleted", delCount)
	if delErr != nil {
		logEvent.Err(delErr)
	}
	logEvent.Msg("ESCALATED: purged Redis-backed SMST state after repeated corruption evictions — next UpdateTree will start a fresh tree (bounded session-level data loss)")

	// Reset the counter: future evictions on this session start from 0
	// since the backing state is now clean.
	delete(m.evictionCounts, sessionID)
}

// evictCorruptSession is the exported variant that handles its own lock.
func (m *RedisSMSTManager) evictCorruptSession(ctx context.Context, sessionID, reason string) {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()
	m.evictCorruptSessionLocked(ctx, sessionID, reason)
}

// resetEvictionCount zeroes the consecutive-corruption counter for a
// session. Called after any SMST operation that proves the session is
// healthy end-to-end (UpdateTree success), so a future transient
// corruption gets the full persistentCorruptionThreshold budget before
// escalating to a Redis purge.
func (m *RedisSMSTManager) resetEvictionCount(sessionID string) {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()
	delete(m.evictionCounts, sessionID)
}

// isSMSTCorruption returns true for every error class the defensive
// layer treats as "tree state is unreliable — drop in-memory cache and
// start over". Redis transport errors are explicitly excluded because
// they are transient and retry-safe.
func isSMSTCorruption(err error) bool {
	if err == nil {
		return false
	}
	if IsRetryableError(err) {
		return false
	}
	return errors.Is(err, ErrSMSTNodeMissing) ||
		errors.Is(err, ErrSMSTPanicRecovered)
}

// redisSMST holds a Redis-backed sparse merkle sum trie for a session.
type redisSMST struct {
	sessionID      string
	trie           smt.SparseMerkleSumTrie
	store          kvstore.MapStore
	sealing        bool   // Set to true when seal process starts (blocks all new updates)
	claimedRoot    []byte // Set after sealing completes and root is verified stable
	claimedCount   uint64 // Cached count after flush (for HA warmup)
	claimedSum     uint64 // Cached sum after flush (for HA warmup)
	proofPath      []byte
	compactProofBz []byte

	// updateCount is the running tally of UpdateTree calls against this
	// tree instance. It drives live_root checkpointing (first update and
	// every LiveRootCheckpointInterval updates after) so HA failover can
	// resume the tree with at most interval-1 relays lost.
	updateCount uint64

	mu sync.Mutex
}

// persistentCorruptionThreshold is the number of consecutive corruption
// evictions for the same session after which evictCorruptSessionLocked
// escalates from memory-only eviction to a full Redis purge. The first
// N-1 attempts preserve Redis so transient in-memory panics can be
// recovered from the backing store; once the same session has failed
// N times in a row it means the Redis state itself is poisoned and
// preserving it only produces an infinite evict→resume→fail loop.
//
// Picked as 3 because the designed-for-transient path (a single
// spurious library assertion caught by runSMSTSafely) resolves on
// attempt 2 at the latest; any session that's still evicting on
// attempt 3 is persistently corrupt and needs a fresh start.
const persistentCorruptionThreshold = 3

// RedisSMSTManager manages Redis-backed SMST trees for sessions.
// It implements the SMSTManager interface used by LifecycleCallback.
// This enables shared storage across HA instances for instant failover.
type RedisSMSTManager struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	config      RedisSMSTManagerConfig

	// Per-session SMST trees (cached in memory, but backed by Redis)
	trees   map[string]*redisSMST
	treesMu sync.RWMutex

	// Consecutive corruption-eviction counter per session. Incremented
	// by evictCorruptSessionLocked, reset to 0 on every successful
	// UpdateTree. Protected by treesMu.
	evictionCounts map[string]int
}

// NewRedisSMSTManager creates a new Redis-backed SMST manager.
// The manager stores SMST nodes in Redis, enabling shared storage across HA instances.
func NewRedisSMSTManager(
	logger logging.Logger,
	redisClient *redisutil.Client,
	config RedisSMSTManagerConfig,
) *RedisSMSTManager {
	return &RedisSMSTManager{
		logger:         logging.ForSupplierComponent(logger, "smst_manager", config.SupplierAddress),
		redisClient:    redisClient,
		config:         config,
		trees:          make(map[string]*redisSMST),
		evictionCounts: make(map[string]int),
	}
}

// GetOrCreateTree returns the SMST for a session. If the tree is not in
// local memory, it first tries to resume from Redis: a claimed_root (if
// the session was already flushed by some prior leader) or a live_root
// checkpointed by a previous leader that was processing the same session
// before dying. Only if neither exists is a brand-new empty tree created.
//
// This is the HA-failover-safe entry point for the relay path. Starting
// an empty tree when Redis has an in-progress state would silently
// discard the relays the dead leader had already committed (see
// scripts/test-quantitative-failover.sh KILL_TARGET=leader scenario).
func (m *RedisSMSTManager) GetOrCreateTree(ctx context.Context, sessionID string) (*redisSMST, error) {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()

	if tree, exists := m.trees[sessionID]; exists {
		return tree, nil
	}

	// Try to resume an existing tree from Redis before creating a fresh one.
	// Prefer claimed_root (post-flush, sealed) over live_root (mid-session).
	if resumed := m.resumeTreeFromRedisLocked(ctx, sessionID); resumed != nil {
		m.trees[sessionID] = resumed
		return resumed, nil
	}

	// No Redis state — create a new empty tree. The store scopes Redis keys
	// to (supplier, sessionID) so that multiple suppliers participating in
	// the same session do NOT overwrite each other's SMST nodes.
	store := NewRedisMapStore(ctx, m.redisClient, m.config.SupplierAddress, sessionID)
	trie := smt.NewSparseMerkleSumTrie(store, protocol.NewTrieHasher(), protocol.SMTValueHasher())

	tree := &redisSMST{
		sessionID: sessionID,
		trie:      trie,
		store:     store,
	}

	m.trees[sessionID] = tree

	// Set TTL on the SMST hash key at creation time (not per-relay).
	// This is a backup safety net; manual deletion happens in OnSessionProved.
	if m.config.CacheTTL > 0 {
		hashKey := m.redisClient.KB().SMSTNodesKey(m.config.SupplierAddress, sessionID)
		if err := m.redisClient.Expire(ctx, hashKey, m.config.CacheTTL).Err(); err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Msg("failed to set SMST TTL on creation (non-fatal)")
		}
	}

	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Msg("created new Redis-backed SMST")

	return tree, nil
}

// resumeTreeFromRedisLocked attempts to import a tree from Redis for HA
// failover recovery. Must be called with m.treesMu held.
//
// Lookup order:
//  1. SMSTRootKey (claimed_root) — tree is post-flush, sealed. Returns a
//     tree with claimedRoot set so late UpdateTree calls are correctly
//     rejected with ErrSessionClaimed.
//  2. SMSTLiveRootKey (live_root) — tree was actively updated by a prior
//     leader that died mid-session. Import at the checkpoint root so new
//     relays extend the dead leader's work rather than start from empty.
//  3. No state — returns nil; caller creates a fresh tree.
//
// Returns nil on any Redis error or missing state; callers treat that as
// "no existing state" and proceed.
func (m *RedisSMSTManager) resumeTreeFromRedisLocked(ctx context.Context, sessionID string) *redisSMST {
	// 1) Claimed root (post-flush)
	claimedKey := m.redisClient.KB().SMSTRootKey(m.config.SupplierAddress, sessionID)
	if claimedRoot, err := m.redisClient.Get(ctx, claimedKey).Bytes(); err == nil && len(claimedRoot) > 0 {
		if !isValidSMSTRoot(claimedRoot) {
			m.logger.Warn().
				Str(logging.FieldSessionID, sessionID).
				Int("got_len", len(claimedRoot)).
				Int("want_len", SMSTRootLen).
				Str("claimed_root_hex", fmt.Sprintf("%x", claimedRoot)).
				Msg("corrupt claimed_root in Redis (wrong length) - deleting and starting fresh")
			// Discard the corrupt key so we fall through to live_root or a fresh tree.
			// Passing a short root to ImportSparseMerkleSumTrie panics inside the smt
			// library when it tries to split the payload into hash/count/sum segments.
			if delErr := m.redisClient.Del(ctx, claimedKey).Err(); delErr != nil {
				m.logger.Warn().Err(delErr).Str(logging.FieldSessionID, sessionID).
					Msg("failed to delete corrupt claimed_root (non-fatal, continuing)")
			}
		} else {
			store := NewRedisMapStore(ctx, m.redisClient, m.config.SupplierAddress, sessionID)
			var trie smt.SparseMerkleSumTrie
			if importErr := m.runSMSTSafely(sessionID, "import_claimed", func() error {
				trie = smt.ImportSparseMerkleSumTrie(store, protocol.NewTrieHasher(), claimedRoot, protocol.SMTValueHasher())
				return nil
			}); importErr != nil {
				m.logger.Error().
					Err(importErr).
					Str(logging.FieldSessionID, sessionID).
					Msg("ImportSparseMerkleSumTrie panicked on claimed_root — deleting key and starting fresh")
				if delErr := m.redisClient.Del(ctx, claimedKey).Err(); delErr != nil {
					m.logger.Warn().Err(delErr).Str(logging.FieldSessionID, sessionID).
						Msg("failed to delete poisonous claimed_root (non-fatal)")
				}
				return nil
			}
			tree := &redisSMST{
				sessionID:   sessionID,
				trie:        trie,
				store:       store,
				claimedRoot: claimedRoot,
			}
			// Restore count/sum from stats for observability; trie itself knows them.
			if statsVal, statsErr := m.redisClient.Get(ctx,
				m.redisClient.KB().SMSTStatsKey(m.config.SupplierAddress, sessionID)).Result(); statsErr == nil {
				_, _ = fmt.Sscanf(statsVal, "%d:%d", &tree.claimedCount, &tree.claimedSum)
			}
			m.logger.Info().
				Str(logging.FieldSessionID, sessionID).
				Str("claimed_root_hex", fmt.Sprintf("%x", claimedRoot)).
				Msg("resumed SMST from claimed_root (session was already flushed)")
			return tree
		}
	}

	// 2) Live root (mid-session checkpoint from previous leader)
	liveKey := m.redisClient.KB().SMSTLiveRootKey(m.config.SupplierAddress, sessionID)
	if liveRoot, err := m.redisClient.Get(ctx, liveKey).Bytes(); err == nil && len(liveRoot) > 0 {
		if !isValidSMSTRoot(liveRoot) {
			m.logger.Warn().
				Str(logging.FieldSessionID, sessionID).
				Int("got_len", len(liveRoot)).
				Int("want_len", SMSTRootLen).
				Str("live_root_hex", fmt.Sprintf("%x", liveRoot)).
				Msg("corrupt live_root in Redis (wrong length) - deleting and starting fresh")
			if delErr := m.redisClient.Del(ctx, liveKey).Err(); delErr != nil {
				m.logger.Warn().Err(delErr).Str(logging.FieldSessionID, sessionID).
					Msg("failed to delete corrupt live_root (non-fatal, continuing)")
			}
			// Fall through: caller creates a fresh tree. The bounded relay loss
			// on a fresh start is the same as any mid-session HA failover.
			return nil
		}
		store := NewRedisMapStore(ctx, m.redisClient, m.config.SupplierAddress, sessionID)
		var trie smt.SparseMerkleSumTrie
		if importErr := m.runSMSTSafely(sessionID, "import_live", func() error {
			trie = smt.ImportSparseMerkleSumTrie(store, protocol.NewTrieHasher(), liveRoot, protocol.SMTValueHasher())
			return nil
		}); importErr != nil {
			m.logger.Error().
				Err(importErr).
				Str(logging.FieldSessionID, sessionID).
				Msg("ImportSparseMerkleSumTrie panicked on live_root — deleting key and starting fresh")
			if delErr := m.redisClient.Del(ctx, liveKey).Err(); delErr != nil {
				m.logger.Warn().Err(delErr).Str(logging.FieldSessionID, sessionID).
					Msg("failed to delete poisonous live_root (non-fatal)")
			}
			return nil
		}
		tree := &redisSMST{
			sessionID: sessionID,
			trie:      trie,
			store:     store,
		}
		m.logger.Info().
			Str(logging.FieldSessionID, sessionID).
			Str("live_root_hex", fmt.Sprintf("%x", liveRoot)).
			Msg("resumed SMST from live_root (mid-session HA failover)")
		return tree
	}

	return nil
}

// UpdateTree adds a relay to the SMST for a session.
func (m *RedisSMSTManager) UpdateTree(ctx context.Context, sessionID string, key, value []byte, weight uint64) (err error) {
	tree, err := m.GetOrCreateTree(ctx, sessionID)
	if err != nil {
		return err
	}

	// Ensure any corruption detected inside this call results in the
	// session being evicted so the next relay starts from a consistent
	// Redis state instead of the poisoned in-memory tree.
	defer func() {
		if isSMSTCorruption(err) {
			m.evictCorruptSession(ctx, sessionID, "update_tree_corruption")
		}
	}()

	tree.mu.Lock()
	defer tree.mu.Unlock()

	// CRITICAL: Reject updates if session is sealing or already claimed
	// The sealing flag prevents race conditions during claim window
	if tree.sealing {
		return ErrSessionSealing
	}
	if tree.claimedRoot != nil {
		return ErrSessionClaimed
	}

	// trie.Update traverses the tree via store.Get; a missing inner
	// node or a malformed payload returns an error from our MapStore
	// (ErrSMSTNodeMissing) or can still panic inside the library on an
	// edge case the defensive Get does not catch. runSMSTSafely turns
	// either outcome into a returned error without crashing the
	// supplier's consume goroutine.
	if err := m.runSMSTSafely(sessionID, "update", func() error {
		return tree.trie.Update(key, value, weight)
	}); err != nil {
		// Double %w so errors.Is walks past the outer sentinel into
		// the inner ErrSMSTNodeMissing / ErrSMSTPanicRecovered chain.
		// isSMSTCorruption (in the defer above) depends on that.
		return fmt.Errorf("%w: %w", ErrSMSTUpdateFailed, err)
	}

	// CRITICAL: Log successful SMST update for debugging
	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Uint64("weight", weight).
		Int("key_len", len(key)).
		Msg("SMST updated with relay")

	// Enable pipelining to batch Set() operations during Commit()
	// This reduces 10-20 Redis round trips (20-40ms) to a single HSET (2-3ms)
	if redisStore, ok := tree.store.(*RedisMapStore); ok {
		redisStore.BeginPipeline()
	}

	// Commit persists dirty nodes to Redis (critical for HA) and is the
	// other library call that can panic on corrupt state (recursive
	// traversal of dirty children + store.Set).
	if err := m.runSMSTSafely(sessionID, "commit", func() error {
		return tree.trie.Commit()
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrSMSTCommitFailed, err)
	}

	// Flush buffered operations to Redis
	// NOTE: FlushPipeline errors are Redis errors and should be retryable.
	// We wrap with ErrSMSTCommitFailed so it's classified as permanent if not a Redis error.
	if redisStore, ok := tree.store.(*RedisMapStore); ok {
		if err := redisStore.FlushPipeline(); err != nil {
			// Double %w so IsRetryableError can reach the underlying
			// net.Error / Redis error through the sentinel wrapper.
			return fmt.Errorf("%w: flush pipeline: %w", ErrSMSTCommitFailed, err)
		}
	}

	// Full write path (Update + Commit + FlushPipeline) succeeded end-to-
	// end — the session is proven healthy against both the in-memory trie
	// and the Redis backing store. Reset the consecutive-eviction counter
	// so a future corruption event starts at 1 and has the full threshold
	// budget before escalating to a Redis purge. Placed AFTER Commit/
	// FlushPipeline (not after Update alone) because corruption shapes
	// that only manifest in Commit's recursive dirty-child traversal
	// would otherwise reset the counter on every relay and never
	// escalate, reproducing the same infinite-loop bug this escalation
	// was introduced to fix.
	m.resetEvictionCount(sessionID)

	// Checkpoint the current intermediate root to Redis so a follower
	// promoted mid-session can resume the tree at this point via
	// ImportSparseMerkleSumTrie. Without this checkpoint, the new leader's
	// GetOrCreateTree would start from an empty root while the dead
	// leader's relay nodes remain orphaned in the shared nodes hash,
	// producing claims that undercount by up to ~50% depending on kill
	// timing (see scripts/test-quantitative-failover.sh).
	//
	// Default interval is 10 — checkpointing every 10 updates instead
	// of every update keeps Redis write amplification low. The worst
	// case relay loss on a mid-session process death is (interval - 1)
	// relays, which is proportionally small given that protocol
	// difficulty bounds how many relays reach the tree in the first
	// place. Operators who need exact claim fidelity can lower it via
	// smst_live_root_checkpoint_interval.
	//
	// A failure is non-fatal: the relay is already in the nodes hash, we
	// just degrade HA recovery for the current checkpoint window.
	tree.updateCount++
	interval := uint64(m.liveRootInterval())
	if tree.updateCount == 1 || tree.updateCount%interval == 0 {
		liveRootKey := m.redisClient.KB().SMSTLiveRootKey(m.config.SupplierAddress, sessionID)
		// Explicit []byte conversion: trie.Root() returns smt.MerkleSumRoot
		// which go-redis does not know how to marshal directly. Guarded
		// with runSMSTSafely because Root() hashes the (possibly
		// corrupted) dirty-child subtree and can panic on a malformed
		// encoding just like Update/Commit.
		var rootBytes []byte
		if err := m.runSMSTSafely(sessionID, "root", func() error {
			rootBytes = []byte(tree.trie.Root())
			return nil
		}); err != nil {
			// Corruption at Root() means we cannot safely persist a
			// live_root. Skip the checkpoint and let the outer deferred
			// eviction drop the session on return.
			return err
		}
		if !isValidSMSTRoot(rootBytes) {
			// Defensive: never persist a root we wouldn't be willing to read back.
			// Keeps the Redis invariant "live_root is always SMSTRootLen or absent".
			m.logger.Warn().
				Str(logging.FieldSessionID, sessionID).
				Int("got_len", len(rootBytes)).
				Int("want_len", SMSTRootLen).
				Uint64("update_count", tree.updateCount).
				Msg("trie.Root() returned unexpected length - skipping live_root checkpoint")
		} else if redisStore, ok := tree.store.(*RedisMapStore); ok {
			// Atomic: HDEL accumulated orphans + SET live_root + EXPIRE
			// on both keys, in one MULTI/EXEC. Before this: live_root
			// points to the previous checkpoint whose nodes are still
			// in the hash (orphans deferred). After: live_root points
			// to the new checkpoint whose nodes were written by the
			// FlushPipeline calls above, orphans are gone, and the
			// sliding TTL keeps both keys alive as long as the session
			// keeps receiving relays — preventing the "nodes hash
			// expires while live_root and in-memory tree still think
			// it's valid" corruption shape on long-lived sessions.
			if err := redisStore.FlushOrphansWithLiveRoot(ctx, liveRootKey, rootBytes, m.config.CacheTTL); err != nil {
				m.logger.Warn().
					Err(err).
					Str(logging.FieldSessionID, sessionID).
					Uint64("update_count", tree.updateCount).
					Msg("failed to atomically flush orphans + live_root (HA resume degraded, orphans retained for next checkpoint)")
			}
		} else {
			// Non-Redis store path (test doubles etc.) — preserve old behaviour.
			if err := m.redisClient.Set(ctx, liveRootKey, rootBytes, 0).Err(); err != nil {
				m.logger.Warn().
					Err(err).
					Str(logging.FieldSessionID, sessionID).
					Uint64("update_count", tree.updateCount).
					Msg("failed to checkpoint live root (HA resume degraded)")
			}
		}
	}

	// TTL is set once at tree creation in GetOrCreateTree (not per-relay).

	return nil
}

// FlushTree flushes the SMST for a session and returns the root hash.
// After flushing, no more updates can be made to the tree.
// Uses two-phase sealing to prevent race conditions with late relays.
//
// If the tree is not in local memory (HA failover: this miner was just
// promoted and the previous leader had been handling this session),
// FlushTree attempts to resume it from Redis via the same lookup used
// by GetOrCreateTree: claimed_root first, then live_root. This covers
// the edge case where the session ends exactly when the leader dies
// and no in-memory tree was ever built on the survivor.
//
// The defer at the top mirrors UpdateTree: any corruption signal
// surfaced by the runSMSTSafely-wrapped library calls below triggers
// eviction of the in-memory session so subsequent attempts start from
// a consistent Redis state (or fail cleanly without tree rot).
func (m *RedisSMSTManager) FlushTree(ctx context.Context, sessionID string) (rootHash []byte, err error) {
	defer func() {
		if isSMSTCorruption(err) {
			m.evictCorruptSession(ctx, sessionID, "flush_tree_corruption")
		}
	}()
	m.treesMu.RLock()
	tree, exists := m.trees[sessionID]
	m.treesMu.RUnlock()

	if !exists {
		// Lazy-load from Redis for HA failover (session ended during kill window).
		m.treesMu.Lock()
		// Re-check under write lock to avoid racing with another goroutine.
		if existing, ok := m.trees[sessionID]; ok {
			tree = existing
		} else if resumed := m.resumeTreeFromRedisLocked(ctx, sessionID); resumed != nil {
			m.trees[sessionID] = resumed
			tree = resumed
		}
		m.treesMu.Unlock()

		if tree == nil {
			return nil, fmt.Errorf("session %s not found", sessionID)
		}
	}

	tree.mu.Lock()
	defer tree.mu.Unlock()

	if tree.claimedRoot == nil {
		// CRITICAL: Two-phase seal to prevent race conditions with late relays
		// Phase 1: SEAL FIRST - set flag to block new updates (while holding lock)
		tree.sealing = true

		// Phase 2: READ initial state (after seal is active). Root/Count/Sum
		// all traverse the trie and can panic on corrupt state, so we run
		// them under runSMSTSafely.
		var (
			rootAfterSeal  []byte
			countAfterSeal uint64
			sumAfterSeal   uint64
		)
		if err := m.runSMSTSafely(sessionID, "seal_read", func() error {
			rootAfterSeal = tree.trie.Root()
			countAfterSeal = tree.trie.MustCount()
			sumAfterSeal = tree.trie.MustSum()
			return nil
		}); err != nil {
			return nil, err
		}

		m.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str("sealed_root_initial_hex", fmt.Sprintf("%x", rootAfterSeal)).
			Uint64("count", countAfterSeal).
			Uint64("sum", sumAfterSeal).
			Msg("seal activated - captured initial state")

		// Phase 3: WAIT - release lock briefly to allow in-flight UpdateTree calls to complete
		// Any relay that was already inside UpdateTree (passed sealing check) will finish.
		// Any new relay will be rejected by the sealing flag.
		tree.mu.Unlock()
		if hook := flushTreeSealWaitHook; hook != nil {
			hook(sessionID)
		}
		time.Sleep(FlushPollInterval)
		tree.mu.Lock()

		// Phase 4: VERIFY - re-read and ensure nothing changed during wait
		var (
			rootAfterWait  []byte
			countAfterWait uint64
			sumAfterWait   uint64
		)
		if err := m.runSMSTSafely(sessionID, "seal_verify", func() error {
			rootAfterWait = tree.trie.Root()
			countAfterWait = tree.trie.MustCount()
			sumAfterWait = tree.trie.MustSum()
			return nil
		}); err != nil {
			return nil, err
		}

		if !bytes.Equal(rootAfterSeal, rootAfterWait) {
			// The sealing flag is set to true under tree.mu BEFORE
			// rootAfterSeal is captured, and UpdateTree re-checks it
			// under the same lock before touching the trie. So a
			// legitimate in-flight relay cannot land during the wait
			// window — if the two captured roots disagree, something
			// else is going on (spurious library internal state read, a
			// subtle sealing-flag bug elsewhere, or a direct trie
			// mutation that bypassed the sealing check).
			//
			// In every one of those cases the RIGHT thing to do is
			// trust rootAfterWait: we now hold the lock again, sealing
			// is true, no further writer can touch the tree, and the
			// value we read matches what will be the final sealed
			// state. Returning an error instead — the old behaviour —
			// would abandon the session entirely: the tree stays in
			// Redis marked as mid-flush, future leaders see the
			// claimed_root-less state, the MsgCreateClaim never gets
			// sent, and the supplier silently loses the relays this
			// session already committed. That failure mode shows up as
			// on-chain ComputeUnits dropping while relay RPS is
			// healthy, which is exactly the regression we are fixing.
			//
			// Log at WARN with enough context (both roots plus
			// count/sum deltas) that any real sealing-flag bug elsewhere
			// in the stack still surfaces via alerts, but do not block
			// the claim on it.
			m.logger.Warn().
				Str(logging.FieldSessionID, sessionID).
				Str("root_after_seal_hex", fmt.Sprintf("%x", rootAfterSeal)).
				Str("root_after_wait_hex", fmt.Sprintf("%x", rootAfterWait)).
				Uint64("count_after_seal", countAfterSeal).
				Uint64("count_after_wait", countAfterWait).
				Uint64("sum_after_seal", sumAfterSeal).
				Uint64("sum_after_wait", sumAfterWait).
				Msg("seal-wait root mismatch: trusting post-wait reading (see FlushTree docs) - investigate if this fires in production")
		} else {
			m.logger.Debug().
				Str(logging.FieldSessionID, sessionID).
				Str("verified_root_hex", fmt.Sprintf("%x", rootAfterWait)).
				Msg("seal verified - root stable after wait, no in-flight relays")
		}

		// Phase 5: Seal complete - save verified stable root. Always
		// use the post-wait reading because it was taken under the
		// re-acquired lock and reflects the authoritative sealed state.
		tree.claimedRoot = rootAfterWait
		tree.claimedCount = countAfterWait
		tree.claimedSum = sumAfterWait
	}

	// Store the claimed root in Redis for HA failover recovery.
	// Only persist if it matches the expected shape — a short root here would
	// poison future resume attempts and panic inside smt.ImportSparseMerkleSumTrie
	// on the next leader.
	rootKey := m.redisClient.KB().SMSTRootKey(m.config.SupplierAddress, sessionID)
	if !isValidSMSTRoot(tree.claimedRoot) {
		m.logger.Error().
			Str(logging.FieldSessionID, sessionID).
			Int("got_len", len(tree.claimedRoot)).
			Int("want_len", SMSTRootLen).
			Msg("refusing to persist claimed_root with unexpected length - claim cannot be safely proved")
		return nil, fmt.Errorf("session %s: claimed_root has invalid length %d, expected %d", sessionID, len(tree.claimedRoot), SMSTRootLen)
	}
	// Persist claimed_root with the SAME sliding TTL as the nodes hash.
	// Using TTL=0 here (the old behaviour) made claimed_root outlive the
	// nodes hash on any crash between FlushTree and proof submission:
	// the nodes hash kept getting its TTL refreshed by
	// FlushOrphansWithLiveRoot during active UpdateTree calls, but once
	// the session stopped receiving relays the nodes hash aged out while
	// claimed_root stayed persistent. On leader resume, loadTreeFromRedis
	// imported at claimed_root, ProveClosest traversed into the missing
	// nodes via the defensive store.Get (ErrSMSTNodeMissing), the proof
	// failed, and the session went to proof-missing — slashing stake.
	// The invariant claimed_root's TTL is always ≥ nodes-hash TTL is
	// maintained by (a) writing claimed_root with CacheTTL here and (b)
	// refreshing it on every loadTreeFromRedis.
	if err := m.redisClient.Set(ctx, rootKey, tree.claimedRoot, m.config.CacheTTL).Err(); err != nil {
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to store claimed root in Redis (non-fatal)")
		// Continue anyway - root is in memory
	}

	// Store count and sum in Redis for HA warmup, with the same sliding TTL
	// so stats cannot outlive the tree they describe.
	statsKey := m.redisClient.KB().SMSTStatsKey(m.config.SupplierAddress, sessionID)
	statsValue := fmt.Sprintf("%d:%d", tree.claimedCount, tree.claimedSum)
	if err := m.redisClient.Set(ctx, statsKey, statsValue, m.config.CacheTTL).Err(); err != nil {
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to store tree stats in Redis (non-fatal)")
	}

	// CRITICAL: Log SMST root hash in hex for debugging invalid proofs
	m.logger.Info().
		Str(logging.FieldSessionID, sessionID).
		Str("root_hash_hex", fmt.Sprintf("%x", tree.claimedRoot)).
		Int("root_hash_len", len(tree.claimedRoot)).
		Msg("flushed SMST - root hash ready for claim")

	return tree.claimedRoot, nil
}

// GetTreeRoot returns the root hash for an already-flushed session.
func (m *RedisSMSTManager) GetTreeRoot(ctx context.Context, sessionID string) (rootHash []byte, err error) {
	m.treesMu.RLock()
	tree, exists := m.trees[sessionID]
	m.treesMu.RUnlock()

	if !exists {
		// HA failover: tree not in memory, try lazy-load from Redis
		loaded, loadErr := m.loadTreeFromRedis(ctx, sessionID)
		if loadErr != nil {
			return nil, fmt.Errorf("session %s not found in memory or Redis: %w", sessionID, loadErr)
		}
		tree = loaded
		m.logger.Info().
			Str(logging.FieldSessionID, sessionID).
			Msg("lazy-loaded SMST tree from Redis for GetTreeRoot (HA failover recovery)")
	}

	tree.mu.Lock()
	defer tree.mu.Unlock()

	if tree.claimedRoot != nil {
		return tree.claimedRoot, nil
	}

	// Return current root even if not flushed. trie.Root() hashes the
	// tree's dirty children and can panic on corrupt state — wrap it.
	var root []byte
	if err := m.runSMSTSafely(sessionID, "root", func() error {
		root = tree.trie.Root()
		return nil
	}); err != nil {
		m.evictCorruptSession(ctx, sessionID, "get_tree_root_corruption")
		return nil, err
	}
	return root, nil
}

// ProveClosest generates a proof for the closest leaf to the given path.
func (m *RedisSMSTManager) ProveClosest(ctx context.Context, sessionID string, path []byte) (proofBytes []byte, err error) {
	defer func() {
		if isSMSTCorruption(err) {
			m.evictCorruptSession(ctx, sessionID, "prove_closest_corruption")
		}
	}()

	m.treesMu.RLock()
	tree, exists := m.trees[sessionID]
	m.treesMu.RUnlock()

	if !exists {
		// HA failover recovery: the tree exists in Redis but is not in local
		// memory because this miner became leader after the original leader
		// already flushed the SMST. Lazy-load the tree from Redis so proofs
		// can be generated after leadership changes.
		loaded, loadErr := m.loadTreeFromRedis(ctx, sessionID)
		if loadErr != nil {
			return nil, fmt.Errorf("session %s not found in memory or Redis: %w", sessionID, loadErr)
		}
		tree = loaded
		m.logger.Info().
			Str(logging.FieldSessionID, sessionID).
			Str("claimed_root_hex", fmt.Sprintf("%x", tree.claimedRoot)).
			Msg("lazy-loaded SMST tree from Redis for proof generation (HA failover recovery)")
	}

	tree.mu.Lock()
	defer tree.mu.Unlock()

	// A tree needs to be flushed/claimed before generating a proof
	if tree.claimedRoot == nil {
		return nil, fmt.Errorf("session %s has not been claimed yet", sessionID)
	}

	// CRITICAL: Verify current tree root matches claimed root. Root()
	// traverses the tree and can panic on corrupt state, so wrap it.
	var currentRoot []byte
	if err := m.runSMSTSafely(sessionID, "root", func() error {
		currentRoot = tree.trie.Root()
		return nil
	}); err != nil {
		return nil, err
	}
	if !bytes.Equal(currentRoot, tree.claimedRoot) {
		m.logger.Error().
			Str(logging.FieldSessionID, sessionID).
			Str("claimed_root_hex", fmt.Sprintf("%x", tree.claimedRoot)).
			Str("current_root_hex", fmt.Sprintf("%x", currentRoot)).
			Msg("ROOT MISMATCH: current tree root != claimed root - proof will be INVALID!")
		return nil, fmt.Errorf("session %s: current root (%x) does not match claimed root (%x) - tree was modified after sealing",
			sessionID, currentRoot, tree.claimedRoot)
	}

	// Generate the proof. ProveClosest walks from root to leaf through
	// store.Get on every lazy child — exactly the panic-surface we are
	// hardening. Wrap it at the boundary.
	var proof *smt.SparseMerkleClosestProof
	if err := m.runSMSTSafely(sessionID, "prove_closest", func() error {
		p, e := tree.trie.ProveClosest(path)
		if e != nil {
			return e
		}
		proof = p
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to prove closest: %w", err)
	}

	// Compact the proof
	compactProof, err := smt.CompactClosestProof(proof, tree.trie.Spec())
	if err != nil {
		return nil, fmt.Errorf("failed to compact proof: %w", err)
	}

	// Marshal the proof
	proofBz, err := compactProof.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal proof: %w", err)
	}

	// Cache the proof
	tree.proofPath = path
	tree.compactProofBz = proofBz

	// CRITICAL: Log proof details for debugging invalid proofs
	m.logger.Info().
		Str(logging.FieldSessionID, sessionID).
		Str("proof_path_hex", fmt.Sprintf("%x", path)).
		Str("claimed_root_hex", fmt.Sprintf("%x", tree.claimedRoot)).
		Int("proof_len", len(proofBz)).
		Msg("generated proof - verify this matches on-chain claim root")

	return proofBz, nil
}

// loadTreeFromRedis reconstructs an SMST tree from Redis for HA failover.
// When the leader changes after a session has been flushed but before its
// proof is submitted, the new leader does not have the tree in local memory.
// This function lazy-loads the tree from Redis so the new leader can
// continue processing proofs for in-flight sessions.
//
// Returns an error if the required Redis keys (claimed root + stats) are
// missing, which means the tree was never flushed or has already been
// cleaned up.
func (m *RedisSMSTManager) loadTreeFromRedis(ctx context.Context, sessionID string) (*redisSMST, error) {
	// Check if the claimed root exists in Redis — this is the signal that
	// the tree was successfully flushed and is ready for proof generation.
	// Keyed by (supplier, session) so each supplier's tree is isolated.
	rootKey := m.redisClient.KB().SMSTRootKey(m.config.SupplierAddress, sessionID)
	rootBytes, err := m.redisClient.Get(ctx, rootKey).Bytes()
	if err != nil || len(rootBytes) == 0 {
		return nil, fmt.Errorf("claimed root not found in Redis: %w", err)
	}
	if !isValidSMSTRoot(rootBytes) {
		// Corrupt root would panic inside the smt library on import. Delete it
		// and surface an explicit error so the caller transitions the session
		// to proof_missing rather than crashing the process.
		m.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Int("got_len", len(rootBytes)).
			Int("want_len", SMSTRootLen).
			Str("root_hex", fmt.Sprintf("%x", rootBytes)).
			Msg("corrupt claimed_root during loadTreeFromRedis - deleting and failing load")
		if delErr := m.redisClient.Del(ctx, rootKey).Err(); delErr != nil {
			m.logger.Warn().Err(delErr).Str(logging.FieldSessionID, sessionID).
				Msg("failed to delete corrupt claimed_root (non-fatal, continuing)")
		}
		return nil, fmt.Errorf("corrupt claimed_root for session %s: len=%d want=%d", sessionID, len(rootBytes), SMSTRootLen)
	}

	// Create the Redis-backed store (lazy-loads nodes on demand)
	store := NewRedisMapStore(ctx, m.redisClient, m.config.SupplierAddress, sessionID)
	// Import with the known claimed root so the tree knows where to start —
	// nodes are lazy-loaded from Redis as needed during ProveClosest.
	// Using NewSparseMerkleSumTrie instead would produce an empty tree with
	// all-zero root, causing ProveClosest to fail with a "root mismatch" error.
	var trie smt.SparseMerkleSumTrie
	if importErr := m.runSMSTSafely(sessionID, "import_load", func() error {
		trie = smt.ImportSparseMerkleSumTrie(
			store,
			protocol.NewTrieHasher(),
			rootBytes,
			protocol.SMTValueHasher(),
		)
		return nil
	}); importErr != nil {
		if delErr := m.redisClient.Del(ctx, rootKey).Err(); delErr != nil {
			m.logger.Warn().Err(delErr).Str(logging.FieldSessionID, sessionID).
				Msg("failed to delete poisonous claimed_root after import panic")
		}
		return nil, fmt.Errorf("loadTreeFromRedis: %w", importErr)
	}

	tree := &redisSMST{
		sessionID:   sessionID,
		trie:        trie,
		store:       store,
		claimedRoot: rootBytes,
	}

	// Restore count/sum from stats key
	statsKey := m.redisClient.KB().SMSTStatsKey(m.config.SupplierAddress, sessionID)
	if statsValue, statsErr := m.redisClient.Get(ctx, statsKey).Result(); statsErr == nil {
		var count, sum uint64
		if _, parseErr := fmt.Sscanf(statsValue, "%d:%d", &count, &sum); parseErr == nil {
			tree.claimedCount = count
			tree.claimedSum = sum
		}
	}

	// Refresh the sliding TTL on claimed_root + stats so a long-running
	// proof-submission retry loop (or a crash-loop that keeps coming back
	// to this code path) cannot age the keys out mid-flight. The
	// invariant that claimed_root's TTL is always ≥ nodes-hash TTL is
	// maintained jointly with FlushTree's write-with-CacheTTL; the nodes
	// hash's TTL is refreshed separately by UpdateTree +
	// FlushOrphansWithLiveRoot while the session is still receiving
	// relays. Once the session is flushed the nodes hash is no longer
	// refreshed, so this refresh on read is what keeps claimed_root and
	// its backing nodes hash alive while we retry proof submission.
	if m.config.CacheTTL > 0 {
		hashKey := m.redisClient.KB().SMSTNodesKey(m.config.SupplierAddress, sessionID)
		if err := m.redisClient.Expire(ctx, rootKey, m.config.CacheTTL).Err(); err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Msg("failed to refresh claimed_root TTL on resume (non-fatal)")
		}
		if err := m.redisClient.Expire(ctx, statsKey, m.config.CacheTTL).Err(); err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Msg("failed to refresh stats TTL on resume (non-fatal)")
		}
		// Refresh nodes hash too — it may exist even if the sliding-TTL
		// live_root checkpoint stopped firing (post-flush). EXPIRE on a
		// missing key returns 0 without erroring, so this is safe.
		if err := m.redisClient.Expire(ctx, hashKey, m.config.CacheTTL).Err(); err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Msg("failed to refresh nodes-hash TTL on resume (non-fatal)")
		}
	}

	// Store in local cache — use double-check pattern to avoid overwriting
	// if another goroutine loaded it first.
	m.treesMu.Lock()
	if existing, ok := m.trees[sessionID]; ok {
		m.treesMu.Unlock()
		return existing, nil
	}
	m.trees[sessionID] = tree
	m.treesMu.Unlock()

	return tree, nil
}

// SetTreeTTL sets a TTL on the Redis SMST hash, root, and stats for a session.
// This is called after successful settlement to ensure cleanup without losing proof data prematurely.
func (m *RedisSMSTManager) SetTreeTTL(ctx context.Context, sessionID string, ttl time.Duration) error {
	hashKey := m.redisClient.KB().SMSTNodesKey(m.config.SupplierAddress, sessionID)
	rootKey := m.redisClient.KB().SMSTRootKey(m.config.SupplierAddress, sessionID)
	statsKey := m.redisClient.KB().SMSTStatsKey(m.config.SupplierAddress, sessionID)
	liveRootKey := m.redisClient.KB().SMSTLiveRootKey(m.config.SupplierAddress, sessionID)

	// Set TTL on nodes hash, root, stats, and live_root
	if err := m.redisClient.Expire(ctx, hashKey, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL on SMST nodes: %w", err)
	}
	if err := m.redisClient.Expire(ctx, rootKey, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL on SMST root: %w", err)
	}
	if err := m.redisClient.Expire(ctx, statsKey, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL on SMST stats: %w", err)
	}
	// live_root may not exist (if the tree was flushed before any update
	// could checkpoint it, or already deleted). Redis EXPIRE on a missing
	// key returns 0 without erroring, so this is safe.
	if err := m.redisClient.Expire(ctx, liveRootKey, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL on SMST live_root: %w", err)
	}

	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Dur("ttl", ttl).
		Msg("set SMST TTL in Redis")

	return nil
}

// DeleteTree removes the SMST for a session from both memory and Redis.
func (m *RedisSMSTManager) DeleteTree(ctx context.Context, sessionID string) error {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()

	// Remove from memory
	delete(m.trees, sessionID)
	// Drop any accumulated corruption-eviction counter for this session
	// so the per-session map does not leak entries across the full
	// session lifecycle for sessions that had any eviction history.
	delete(m.evictionCounts, sessionID)

	// Remove nodes hash, root, stats, and live_root from Redis. Keys are
	// scoped by (supplier, sessionID), so this delete only affects THIS
	// supplier — other suppliers participating in the same session are
	// unaffected.
	hashKey := m.redisClient.KB().SMSTNodesKey(m.config.SupplierAddress, sessionID)
	rootKey := m.redisClient.KB().SMSTRootKey(m.config.SupplierAddress, sessionID)
	statsKey := m.redisClient.KB().SMSTStatsKey(m.config.SupplierAddress, sessionID)
	liveRootKey := m.redisClient.KB().SMSTLiveRootKey(m.config.SupplierAddress, sessionID)
	if err := m.redisClient.Del(ctx, hashKey, rootKey, statsKey, liveRootKey).Err(); err != nil {
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to delete SMST from Redis")
		// Don't return error - memory cleanup succeeded
	}

	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Msg("deleted SMST from memory and Redis")

	return nil
}

// GetTreeCount returns the number of trees being managed.
func (m *RedisSMSTManager) GetTreeCount() int {
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	return len(m.trees)
}

// WarmupFromRedis scans Redis for existing SMST keys and bulk-loads them into memory.
//
// NOTE: This function is currently NOT called on startup or leader change —
// lazy-loading (via GetOrCreateTree for relays, loadTreeFromRedis for proofs)
// is preferred because it avoids a full Redis SCAN (which can take 10-20 min
// at scale). It remains available for scenarios where an eager warmup is
// needed (e.g., operational tooling). If you're debugging missing-session
// issues after a leader change, check the lazy-load paths in ProveClosest /
// GetTreeRoot / loadTreeFromRedis first.
func (m *RedisSMSTManager) WarmupFromRedis(ctx context.Context) (int, error) {
	m.logger.Info().Msg("warming up SMST trees from Redis")

	// Scan for SMST keys matching pattern {base}:smst:*:*:nodes and keep
	// only those belonging to THIS manager's supplier.
	var cursor uint64
	var loadedCount int
	smstPrefix := m.redisClient.KB().SMSTNodesPrefix()

	for {
		keys, nextCursor, err := m.redisClient.Scan(ctx, cursor, m.redisClient.KB().SMSTNodesPattern(), RedisScanBatchSize).Result()
		if err != nil {
			return loadedCount, fmt.Errorf("failed to scan Redis for SMST keys: %w", err)
		}

		for _, hashKey := range keys {
			// Parse key as: {prefix}{supplierAddress}:{sessionID}:nodes
			suffix := strings.TrimPrefix(hashKey, smstPrefix)
			suffix = strings.TrimSuffix(suffix, ":nodes")
			// suffix is now "{supplierAddress}:{sessionID}"
			colonIdx := strings.IndexByte(suffix, ':')
			if colonIdx <= 0 || colonIdx == len(suffix)-1 {
				m.logger.Debug().Str("key", hashKey).Msg("skipping malformed SMST key during warmup")
				continue
			}
			keySupplier := suffix[:colonIdx]
			sessionID := suffix[colonIdx+1:]

			// Only warm up trees for THIS supplier — other suppliers have
			// their own RedisSMSTManager instance.
			if keySupplier != m.config.SupplierAddress {
				continue
			}

			// Mirror GetOrCreateTree's resume semantics: prefer claimed_root
			// (post-flush, sealed), fall back to live_root (mid-session
			// checkpoint), and only then create an empty tree. The old
			// behaviour here was to unconditionally call
			// NewSparseMerkleSumTrie — that silently reset every in-progress
			// session to zero relays, so any caller of WarmupFromRedis (an
			// ops script, a future eager-warmup wiring, a debug tool)
			// produced total data loss for those sessions.
			//
			// We lock the map once per session so that (a) the exists check
			// and the resume attempt are atomic against concurrent
			// GetOrCreateTree calls and (b) resumeTreeFromRedisLocked's
			// precondition ("caller holds m.treesMu") is satisfied.
			m.treesMu.Lock()
			if _, exists := m.trees[sessionID]; exists {
				m.treesMu.Unlock()
				continue // Skip - already loaded
			}

			if resumed := m.resumeTreeFromRedisLocked(ctx, sessionID); resumed != nil {
				m.trees[sessionID] = resumed
				m.treesMu.Unlock()
				loadedCount++
				m.logger.Debug().
					Str(logging.FieldSessionID, sessionID).
					Msg("warmed up SMST from Redis (resumed)")
				continue
			}

			// No usable claimed_root or live_root. Create a fresh empty
			// tree so the session can accept new relays — the nodes hash
			// is still in Redis (that's what the scan matched on) but
			// without a root anchor we cannot reconstruct prior state.
			// This matches GetOrCreateTree's final branch.
			store := NewRedisMapStore(ctx, m.redisClient, m.config.SupplierAddress, sessionID)
			trie := smt.NewSparseMerkleSumTrie(store, protocol.NewTrieHasher(), protocol.SMTValueHasher())
			m.trees[sessionID] = &redisSMST{
				sessionID: sessionID,
				trie:      trie,
				store:     store,
			}
			m.treesMu.Unlock()

			loadedCount++
			m.logger.Debug().
				Str(logging.FieldSessionID, sessionID).
				Msg("warmed up SMST from Redis (empty tree — no root in Redis)")
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	m.logger.Info().
		Int("loaded_trees", loadedCount).
		Msg("SMST warmup complete")

	return loadedCount, nil
}

// GetTreeStats returns statistics for a session tree.
// If the tree has been flushed, returns cached values from the claimed root.
// Otherwise, queries the trie directly.
func (m *RedisSMSTManager) GetTreeStats(sessionID string) (count uint64, sum uint64, err error) {
	m.treesMu.RLock()
	tree, exists := m.trees[sessionID]
	m.treesMu.RUnlock()

	if !exists {
		return 0, 0, fmt.Errorf("session %s not found", sessionID)
	}

	tree.mu.Lock()
	defer tree.mu.Unlock()

	// If tree has been flushed, use cached values (works after HA warmup)
	if tree.claimedRoot != nil {
		return tree.claimedCount, tree.claimedSum, nil
	}

	// MustCount/MustSum traverse the trie and can panic on corrupt
	// state. Wrap so GetTreeStats never takes down a caller goroutine.
	if sErr := m.runSMSTSafely(sessionID, "stats", func() error {
		count = tree.trie.MustCount()
		sum = tree.trie.MustSum()
		return nil
	}); sErr != nil {
		return 0, 0, sErr
	}

	return count, sum, nil
}

// Close cleans up all managed trees.
func (m *RedisSMSTManager) Close() error {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()

	m.trees = make(map[string]*redisSMST)

	m.logger.Info().Msg("SMST manager closed")
	return nil
}

// Ensure RedisSMSTManager implements SMSTManager
var _ SMSTManager = (*RedisSMSTManager)(nil)
