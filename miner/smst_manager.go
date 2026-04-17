package miner

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/smt"
	"github.com/pokt-network/smt/kvstore"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
)

const (
	// FlushPollInterval is the time to wait for in-flight UpdateTree calls to complete
	// during the sealing process.
	FlushPollInterval = 10 * time.Millisecond

	// RedisScanBatchSize is the number of keys to scan per Redis SCAN iteration
	// when warming up SMST trees from Redis.
	RedisScanBatchSize = 100

	// DefaultLiveRootCheckpointInterval is the number of UpdateTree calls
	// between writes of the intermediate root to Redis. This is the bound
	// on relay loss if a leader dies between checkpoints: up to
	// (interval - 1) relays committed to the nodes hash but not yet
	// represented in a stored root. Lower is safer; higher is faster.
	// At 1000 RPS/supplier with interval=10: 100 writes/s instead of 1000.
	DefaultLiveRootCheckpointInterval = 10
)

// RedisSMSTManagerConfig contains configuration for the SMST manager.
type RedisSMSTManagerConfig struct {
	// SupplierAddress is the supplier this manager is for.
	SupplierAddress string

	// CacheTTL is how long to keep SMST data in Redis (backup if manual cleanup fails).
	CacheTTL time.Duration

	// LiveRootCheckpointInterval is the number of UpdateTree calls between
	// writes of the intermediate root to Redis. See
	// DefaultLiveRootCheckpointInterval for the trade-off. Zero means use
	// the default (10). Set to 1 to checkpoint on every relay (safest,
	// slowest).
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
}

// NewRedisSMSTManager creates a new Redis-backed SMST manager.
// The manager stores SMST nodes in Redis, enabling shared storage across HA instances.
func NewRedisSMSTManager(
	logger logging.Logger,
	redisClient *redisutil.Client,
	config RedisSMSTManagerConfig,
) *RedisSMSTManager {
	return &RedisSMSTManager{
		logger:      logging.ForSupplierComponent(logger, "smst_manager", config.SupplierAddress),
		redisClient: redisClient,
		config:      config,
		trees:       make(map[string]*redisSMST),
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
			trie := smt.ImportSparseMerkleSumTrie(store, protocol.NewTrieHasher(), claimedRoot, protocol.SMTValueHasher())
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
		trie := smt.ImportSparseMerkleSumTrie(store, protocol.NewTrieHasher(), liveRoot, protocol.SMTValueHasher())
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
func (m *RedisSMSTManager) UpdateTree(ctx context.Context, sessionID string, key, value []byte, weight uint64) error {
	tree, err := m.GetOrCreateTree(ctx, sessionID)
	if err != nil {
		return err
	}

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

	if err := tree.trie.Update(key, value, weight); err != nil {
		return fmt.Errorf("%w: %v", ErrSMSTUpdateFailed, err)
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

	// Commit to persist dirty nodes to Redis (critical for HA)
	if err := tree.trie.Commit(); err != nil {
		return fmt.Errorf("%w: %v", ErrSMSTCommitFailed, err)
	}

	// Flush buffered operations to Redis
	// NOTE: FlushPipeline errors are Redis errors and should be retryable.
	// We wrap with ErrSMSTCommitFailed so it's classified as permanent if not a Redis error.
	if redisStore, ok := tree.store.(*RedisMapStore); ok {
		if err := redisStore.FlushPipeline(); err != nil {
			// Return wrapped error - IsRetryableError will check for net.Error underneath
			return fmt.Errorf("%w: flush pipeline: %v", ErrSMSTCommitFailed, err)
		}
	}

	// Checkpoint the current intermediate root to Redis so a follower
	// promoted mid-session can resume the tree at this point via
	// ImportSparseMerkleSumTrie. Without this checkpoint, the new leader's
	// GetOrCreateTree would start from an empty root while the dead
	// leader's relay nodes remain orphaned in the shared nodes hash,
	// producing claims that undercount by up to ~50% depending on kill
	// timing (see scripts/test-quantitative-failover.sh).
	//
	// Writing on every relay is safe but would add one Redis SET per
	// relay — a large cost at production RPS. Instead, checkpoint on the
	// FIRST update (so low-traffic sessions still have a resume point)
	// and then every LiveRootCheckpointInterval updates. The worst-case
	// relay loss on a mid-session kill is therefore at most
	// (interval - 1) relays between the last checkpoint and the kill.
	//
	// A failure is non-fatal: the relay is already in the nodes hash, we
	// just degrade HA recovery for the current checkpoint window.
	tree.updateCount++
	interval := uint64(m.liveRootInterval())
	if tree.updateCount == 1 || tree.updateCount%interval == 0 {
		liveRootKey := m.redisClient.KB().SMSTLiveRootKey(m.config.SupplierAddress, sessionID)
		// Explicit []byte conversion: trie.Root() returns smt.MerkleSumRoot
		// which go-redis does not know how to marshal directly.
		rootBytes := []byte(tree.trie.Root())
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
			// Atomic: HDEL accumulated orphans + SET live_root in one
			// MULTI/EXEC. Before this: live_root points to the previous
			// checkpoint whose nodes are still in the hash (orphans
			// deferred). After: live_root points to the new checkpoint
			// whose nodes were written by the FlushPipeline calls above,
			// and the orphans are gone. Never a moment where live_root
			// references a deleted subtree — that race caused the
			// Anaski 2026-04-17 panic in parseSumTrieNode.
			if err := redisStore.FlushOrphansWithLiveRoot(ctx, liveRootKey, rootBytes); err != nil {
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
func (m *RedisSMSTManager) FlushTree(ctx context.Context, sessionID string) (rootHash []byte, err error) {
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

		// Phase 2: READ initial state (after seal is active)
		rootAfterSeal := tree.trie.Root()
		countAfterSeal := tree.trie.MustCount()
		sumAfterSeal := tree.trie.MustSum()

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
		time.Sleep(FlushPollInterval)
		tree.mu.Lock()

		// Phase 4: VERIFY - re-read and ensure nothing changed during wait
		rootAfterWait := tree.trie.Root()
		countAfterWait := tree.trie.MustCount()
		sumAfterWait := tree.trie.MustSum()

		if !bytes.Equal(rootAfterSeal, rootAfterWait) {
			m.logger.Error().
				Str(logging.FieldSessionID, sessionID).
				Str("root_after_seal_hex", fmt.Sprintf("%x", rootAfterSeal)).
				Str("root_after_wait_hex", fmt.Sprintf("%x", rootAfterWait)).
				Uint64("count_after_seal", countAfterSeal).
				Uint64("count_after_wait", countAfterWait).
				Uint64("sum_after_seal", sumAfterSeal).
				Uint64("sum_after_wait", sumAfterWait).
				Msg("SEAL RACE DETECTED: root changed after seal - in-flight relay completed during wait!")
			return nil, fmt.Errorf("session %s: root changed after seal (sealed=%x, after_wait=%x) - in-flight relay modified tree",
				sessionID, rootAfterSeal, rootAfterWait)
		}

		m.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str("verified_root_hex", fmt.Sprintf("%x", rootAfterWait)).
			Msg("seal verified - root stable after wait, no in-flight relays")

		// Phase 5: Seal complete - save verified stable root
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
	if err := m.redisClient.Set(ctx, rootKey, tree.claimedRoot, 0).Err(); err != nil {
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to store claimed root in Redis (non-fatal)")
		// Continue anyway - root is in memory
	}

	// Store count and sum in Redis for HA warmup
	statsKey := m.redisClient.KB().SMSTStatsKey(m.config.SupplierAddress, sessionID)
	statsValue := fmt.Sprintf("%d:%d", tree.claimedCount, tree.claimedSum)
	if err := m.redisClient.Set(ctx, statsKey, statsValue, 0).Err(); err != nil {
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

	// Return current root even if not flushed
	return tree.trie.Root(), nil
}

// ProveClosest generates a proof for the closest leaf to the given path.
func (m *RedisSMSTManager) ProveClosest(ctx context.Context, sessionID string, path []byte) (proofBytes []byte, err error) {
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

	// CRITICAL: Verify current tree root matches claimed root
	// If they don't match, proof will be invalid
	currentRoot := tree.trie.Root()
	if !bytes.Equal(currentRoot, tree.claimedRoot) {
		m.logger.Error().
			Str(logging.FieldSessionID, sessionID).
			Str("claimed_root_hex", fmt.Sprintf("%x", tree.claimedRoot)).
			Str("current_root_hex", fmt.Sprintf("%x", currentRoot)).
			Msg("ROOT MISMATCH: current tree root != claimed root - proof will be INVALID!")
		return nil, fmt.Errorf("session %s: current root (%x) does not match claimed root (%x) - tree was modified after sealing",
			sessionID, currentRoot, tree.claimedRoot)
	}

	// Generate the proof
	proof, err := tree.trie.ProveClosest(path)
	if err != nil {
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
	trie := smt.ImportSparseMerkleSumTrie(
		store,
		protocol.NewTrieHasher(),
		rootBytes,
		protocol.SMTValueHasher(),
	)

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

			// Check if tree already exists in memory
			m.treesMu.RLock()
			_, exists := m.trees[sessionID]
			m.treesMu.RUnlock()

			if exists {
				continue // Skip - already loaded
			}

			// Create RedisMapStore for this session (doesn't load data yet)
			store := NewRedisMapStore(ctx, m.redisClient, m.config.SupplierAddress, sessionID)

			// Create SMST with the Redis store
			// The SMT library will lazy-load nodes from Redis as needed
			trie := smt.NewSparseMerkleSumTrie(store, protocol.NewTrieHasher(), protocol.SMTValueHasher())

			tree := &redisSMST{
				sessionID: sessionID,
				trie:      trie,
				store:     store,
			}

			// Restore the claimed root from Redis if it exists
			rootKey := m.redisClient.KB().SMSTRootKey(m.config.SupplierAddress, sessionID)
			rootBytes, err := m.redisClient.Get(ctx, rootKey).Bytes()
			if err == nil && len(rootBytes) > 0 && !isValidSMSTRoot(rootBytes) {
				// Corrupt root — delete and treat as unflushed. The fresh tree
				// created above will accept new relays; the session effectively
				// restarts, which is the same outcome as any failover recovery.
				m.logger.Warn().
					Str(logging.FieldSessionID, sessionID).
					Int("got_len", len(rootBytes)).
					Int("want_len", SMSTRootLen).
					Str("root_hex", fmt.Sprintf("%x", rootBytes)).
					Msg("corrupt claimed_root during warmup - deleting")
				if delErr := m.redisClient.Del(ctx, rootKey).Err(); delErr != nil {
					m.logger.Warn().Err(delErr).Str(logging.FieldSessionID, sessionID).
						Msg("failed to delete corrupt claimed_root (non-fatal)")
				}
				rootBytes = nil
			}
			if err == nil && len(rootBytes) > 0 {
				tree.claimedRoot = rootBytes
				m.logger.Debug().
					Str(logging.FieldSessionID, sessionID).
					Str("root_hash_hex", fmt.Sprintf("%x", rootBytes)).
					Msg("restored claimed root from Redis")

				// Restore count and sum from Redis
				statsKey := m.redisClient.KB().SMSTStatsKey(m.config.SupplierAddress, sessionID)
				statsValue, statsErr := m.redisClient.Get(ctx, statsKey).Result()
				if statsErr == nil {
					var count, sum uint64
					if _, parseErr := fmt.Sscanf(statsValue, "%d:%d", &count, &sum); parseErr == nil {
						tree.claimedCount = count
						tree.claimedSum = sum
						m.logger.Debug().
							Str(logging.FieldSessionID, sessionID).
							Uint64("count", count).
							Uint64("sum", sum).
							Msg("restored tree stats from Redis")
					}
				}
			} else if err != nil {
				m.logger.Debug().
					Err(err).
					Str(logging.FieldSessionID, sessionID).
					Msg("no claimed root found in Redis (tree not yet flushed)")
			}

			m.treesMu.Lock()
			m.trees[sessionID] = tree
			m.treesMu.Unlock()

			loadedCount++

			m.logger.Debug().
				Str(logging.FieldSessionID, sessionID).
				Msg("warmed up SMST from Redis")
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

	// Otherwise query the trie directly
	count = tree.trie.MustCount()
	sum = tree.trie.MustSum()

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
