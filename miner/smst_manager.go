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

// RedisSMSTManagerConfig contains configuration for the SMST manager.
type RedisSMSTManagerConfig struct {
	// SupplierAddress is the supplier this manager is for.
	SupplierAddress string

	// CacheTTL is how long to keep SMST data in Redis (backup if manual cleanup fails).
	CacheTTL time.Duration
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
	mu             sync.Mutex
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

// GetOrCreateTree returns the SMST for a session, creating it if it doesn't exist.
// Trees are backed by Redis for shared storage across HA instances.
func (m *RedisSMSTManager) GetOrCreateTree(ctx context.Context, sessionID string) (*redisSMST, error) {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()

	if tree, exists := m.trees[sessionID]; exists {
		return tree, nil
	}

	// Create a new Redis-backed tree
	store := NewRedisMapStore(ctx, m.redisClient, sessionID)
	trie := smt.NewSparseMerkleSumTrie(store, protocol.NewTrieHasher(), protocol.SMTValueHasher())

	tree := &redisSMST{
		sessionID: sessionID,
		trie:      trie,
		store:     store,
	}

	m.trees[sessionID] = tree

	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Msg("created new Redis-backed SMST")

	return tree, nil
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

	// Set TTL as backup safety net (auto-expire if manual cleanup fails)
	// Manual deletion happens in OnSessionSettled/OnSessionExpired
	if m.config.CacheTTL > 0 {
		hashKey := m.redisClient.KB().SMSTNodesKey(sessionID)
		if err := m.redisClient.Expire(ctx, hashKey, m.config.CacheTTL).Err(); err != nil {
			// Log but don't fail - TTL is just a safety net
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Msg("failed to set SMST TTL (non-fatal)")
		}
	}

	return nil
}

// FlushTree flushes the SMST for a session and returns the root hash.
// After flushing, no more updates can be made to the tree.
// Uses two-phase sealing to prevent race conditions with late relays.
func (m *RedisSMSTManager) FlushTree(ctx context.Context, sessionID string) (rootHash []byte, err error) {
	m.treesMu.RLock()
	tree, exists := m.trees[sessionID]
	m.treesMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("session %s not found", sessionID)
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
		time.Sleep(10 * time.Millisecond)
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

	// Store the claimed root in Redis for HA failover recovery
	rootKey := m.redisClient.KB().SMSTRootKey(sessionID)
	if err := m.redisClient.Set(ctx, rootKey, tree.claimedRoot, 0).Err(); err != nil {
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to store claimed root in Redis (non-fatal)")
		// Continue anyway - root is in memory
	}

	// Store count and sum in Redis for HA warmup
	statsKey := m.redisClient.KB().SMSTStatsKey(sessionID)
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
		return nil, fmt.Errorf("session %s not found", sessionID)
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
		return nil, fmt.Errorf("session %s not found", sessionID)
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

// SetTreeTTL sets a TTL on the Redis SMST hash, root, and stats for a session.
// This is called after successful settlement to ensure cleanup without losing proof data prematurely.
func (m *RedisSMSTManager) SetTreeTTL(ctx context.Context, sessionID string, ttl time.Duration) error {
	hashKey := m.redisClient.KB().SMSTNodesKey(sessionID)
	rootKey := m.redisClient.KB().SMSTRootKey(sessionID)
	statsKey := m.redisClient.KB().SMSTStatsKey(sessionID)

	// Set TTL on nodes hash, root, and stats
	if err := m.redisClient.Expire(ctx, hashKey, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL on SMST nodes: %w", err)
	}
	if err := m.redisClient.Expire(ctx, rootKey, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL on SMST root: %w", err)
	}
	if err := m.redisClient.Expire(ctx, statsKey, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL on SMST stats: %w", err)
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

	// Remove nodes hash, root, and stats from Redis
	hashKey := m.redisClient.KB().SMSTNodesKey(sessionID)
	rootKey := m.redisClient.KB().SMSTRootKey(sessionID)
	statsKey := m.redisClient.KB().SMSTStatsKey(sessionID)
	if err := m.redisClient.Del(ctx, hashKey, rootKey, statsKey).Err(); err != nil {
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

// WarmupFromRedis scans Redis for existing SMST keys and loads them into memory.
// This is called on startup or leader election to restore state after restart/failover.
func (m *RedisSMSTManager) WarmupFromRedis(ctx context.Context) (int, error) {
	m.logger.Info().Msg("warming up SMST trees from Redis")

	// Scan for SMST keys matching pattern ha:smst:*:nodes
	var cursor uint64
	var loadedCount int
	smstPrefix := m.redisClient.KB().SMSTNodesPrefix()

	for {
		keys, nextCursor, err := m.redisClient.Scan(ctx, cursor, m.redisClient.KB().SMSTNodesPattern(), 100).Result()
		if err != nil {
			return loadedCount, fmt.Errorf("failed to scan Redis for SMST keys: %w", err)
		}

		for _, hashKey := range keys {
			// Extract session ID from key: {prefix}:smst:{sessionID}:nodes
			// Remove prefix and suffix ":nodes"
			sessionID := strings.TrimPrefix(hashKey, smstPrefix)
			sessionID = strings.TrimSuffix(sessionID, ":nodes")

			// Check if tree already exists in memory
			m.treesMu.RLock()
			_, exists := m.trees[sessionID]
			m.treesMu.RUnlock()

			if exists {
				continue // Skip - already loaded
			}

			// Create RedisMapStore for this session (doesn't load data yet)
			store := NewRedisMapStore(ctx, m.redisClient, sessionID)

			// Create SMST with the Redis store
			// The SMT library will lazy-load nodes from Redis as needed
			trie := smt.NewSparseMerkleSumTrie(store, protocol.NewTrieHasher(), protocol.SMTValueHasher())

			tree := &redisSMST{
				sessionID: sessionID,
				trie:      trie,
				store:     store,
			}

			// Restore the claimed root from Redis if it exists
			rootKey := m.redisClient.KB().SMSTRootKey(sessionID)
			rootBytes, err := m.redisClient.Get(ctx, rootKey).Bytes()
			if err == nil && len(rootBytes) > 0 {
				tree.claimedRoot = rootBytes
				m.logger.Debug().
					Str(logging.FieldSessionID, sessionID).
					Str("root_hash_hex", fmt.Sprintf("%x", rootBytes)).
					Msg("restored claimed root from Redis")

				// Restore count and sum from Redis
				statsKey := m.redisClient.KB().SMSTStatsKey(sessionID)
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
