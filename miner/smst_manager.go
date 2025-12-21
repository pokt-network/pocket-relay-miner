package miner

import (
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
	claimedRoot    []byte
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

	if tree.claimedRoot != nil {
		return fmt.Errorf("session %s has already been claimed, cannot update", sessionID)
	}

	if err := tree.trie.Update(key, value, weight); err != nil {
		return fmt.Errorf("failed to update SMST: %w", err)
	}

	// Enable pipelining to batch Set() operations during Commit()
	// This reduces 10-20 Redis round trips (20-40ms) to a single HSET (2-3ms)
	if redisStore, ok := tree.store.(*RedisMapStore); ok {
		redisStore.BeginPipeline()
	}

	// Commit to persist dirty nodes to Redis (critical for HA)
	if err := tree.trie.Commit(); err != nil {
		return fmt.Errorf("failed to commit SMST to Redis: %w", err)
	}

	// Flush buffered operations to Redis
	if redisStore, ok := tree.store.(*RedisMapStore); ok {
		if err := redisStore.FlushPipeline(); err != nil {
			return fmt.Errorf("failed to flush pipeline: %w", err)
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
		tree.claimedRoot = tree.trie.Root()
	}

	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Int("root_hash_len", len(tree.claimedRoot)).
		Msg("flushed SMST")

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

	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Int("proof_len", len(proofBz)).
		Msg("generated proof")

	return proofBz, nil
}

// SetTreeTTL sets a TTL on the Redis SMST hash for a session.
// This is called after successful settlement to ensure cleanup without losing proof data prematurely.
func (m *RedisSMSTManager) SetTreeTTL(ctx context.Context, sessionID string, ttl time.Duration) error {
	hashKey := m.redisClient.KB().SMSTNodesKey(sessionID)

	if err := m.redisClient.Expire(ctx, hashKey, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL on SMST: %w", err)
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

	// Remove from Redis
	hashKey := m.redisClient.KB().SMSTNodesKey(sessionID)
	if err := m.redisClient.Del(ctx, hashKey).Err(); err != nil {
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
func (m *RedisSMSTManager) GetTreeStats(sessionID string) (count uint64, sum uint64, err error) {
	m.treesMu.RLock()
	tree, exists := m.trees[sessionID]
	m.treesMu.RUnlock()

	if !exists {
		return 0, 0, fmt.Errorf("session %s not found", sessionID)
	}

	tree.mu.Lock()
	defer tree.mu.Unlock()

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
