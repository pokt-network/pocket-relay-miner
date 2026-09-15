package miner

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/smt/kvstore"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// RedisMapStore implements kvstore.MapStore using Redis hashes with pipelining optimization.
// This enables shared storage across HA instances, avoiding local disk IOPS issues.
//
// The RedisMapStore uses a single Redis hash to store all key-value pairs for a session's SMST.
// This provides O(1) access for Get/Set/Delete operations and enables instant failover since
// all instances can access the same Redis data.
//
// Pipelining Optimization:
// During SMST Commit(), the library calls Set() 10-20 times for dirty nodes and
// Delete() on any node that became an orphan during the Update. Instead of 10-20
// round trips (20-40ms), we buffer operations and flush in one HSET (2-3ms).
// This provides 8-10× speedup for relay processing.
//
// Deferred orphan deletion (HA correctness):
// Unlike Set(), orphan Delete() calls MUST NOT execute on every Update or they
// break the checkpointed live_root. When the live_root is checkpointed only
// every N updates, deleting orphans in-between invalidates the children of the
// previous checkpoint — the next leader to resume from live_root then panics
// in smt.parseSumTrieNode on an empty slice (see Anaski 2026-04-17 panic).
//
// The fix: buffer orphan digests across Updates and only flush them at the
// live_root checkpoint boundary, atomically with the new live_root SET
// (see FlushOrphansWithLiveRoot). Between boundaries orphan bytes linger in
// the nodes hash — harmless bloat that is wiped by DeleteTree at cleanup.
//
// Redis Hash Structure:
//
//	Key: Built via KeyBuilder.SMSTNodesKey(supplierAddress, sessionID)
//	Fields: hex-encoded SMST node keys
//	Values: raw SMST node data
type RedisMapStore struct {
	redisClient *redisutil.Client
	hashKey     string // Redis hash key built via KeyBuilder.SMSTNodesKey()
	ctx         context.Context

	// Pipeline buffers — separated because they have different lifetimes.
	//   pipelineBuffer is flushed by every FlushPipeline (one round-trip per
	//   UpdateTree) and emptied only by a write that succeeded, so the nodes
	//   of a failed flush go with the next one.
	//   orphanBuffer accumulates across Updates until FlushOrphansWithLiveRoot
	//   at a checkpoint boundary, so live_root always references nodes that
	//   are still present in the hash.
	pipelineMu      sync.Mutex
	pipelineEnabled bool                // true when buffering Set()/Delete() calls
	pipelineBuffer  map[string][]byte   // field -> value (new-node writes)
	orphanBuffer    map[string]struct{} // field set (orphan deletes pending checkpoint)
}

// NewRedisMapStore creates a new Redis-backed MapStore for a (supplier, session) pair.
// The store uses a Redis hash to persist SMST nodes, enabling shared access across HA instances.
//
// Parameters:
//   - ctx: Context for Redis operations
//   - redisClient: Redis client (supports standalone, sentinel, and cluster)
//   - supplierAddress: Supplier operator address — required to namespace the hash per
//     supplier so distinct suppliers participating in the same session do not
//     overwrite each other's SMST nodes.
//   - sessionID: Unique session identifier used to namespace the Redis hash
//
// Returns:
//
//	A MapStore implementation backed by Redis
func NewRedisMapStore(
	ctx context.Context,
	redisClient *redisutil.Client,
	supplierAddress string,
	sessionID string,
) kvstore.MapStore {
	return &RedisMapStore{
		redisClient:    redisClient,
		hashKey:        redisClient.KB().SMSTNodesKey(supplierAddress, sessionID),
		ctx:            ctx,
		pipelineBuffer: make(map[string][]byte),
		orphanBuffer:   make(map[string]struct{}),
	}
}

// Get retrieves a value from the Redis hash.
//
// The key is hex-encoded before being used as a Redis hash field name,
// since Redis requires string field names but SMST keys are byte arrays.
//
// Missing-key behavior: returns (nil, ErrSMSTNodeMissing) when the hash
// field is absent. The smt library's resolveSumNode/resolveNode check
// the placeholder digest *before* calling Get, so a missing non-
// placeholder digest is always data corruption (orphan HDEL race,
// Redis eviction, manual surgery, two-leader window, etc.) — never a
// normal empty-subtree. Returning an error here stops the library from
// passing a zero-length slice to parseSumTrieNode / parseTrieNode and
// panicking in isLeafNode on data[:1]. The error propagates cleanly
// through resolveSumNode -> update -> Update back to UpdateTree, which
// logs and returns ErrSMSTNodeMissing without tumbling the goroutine.
// The official reference implementation (smt/kvstore/simplemap) also
// returns an error on missing keys (ErrKVStoreKeyNotFound), so this
// brings us in line with the canonical contract.
func (s *RedisMapStore) Get(key []byte) ([]byte, error) {
	start := time.Now()
	defer func() {
		observability.SMSTStoreOperationDuration.WithLabelValues("get").Observe(time.Since(start).Seconds())
	}()

	// Convert key to hex string for Redis field name
	field := hex.EncodeToString(key)

	val, err := s.redisClient.HGet(s.ctx, s.hashKey, field).Bytes()
	if err == redis.Nil {
		observability.SMSTStoreOperations.WithLabelValues("get", "not_found").Inc()
		return nil, fmt.Errorf("%w: field=%s hash=%s", ErrSMSTNodeMissing, field, s.hashKey)
	}
	if err != nil {
		observability.SMSTStoreOperations.WithLabelValues("get", "error").Inc()
		observability.SMSTStoreErrors.WithLabelValues("get", "store_error").Inc()
		return nil, err
	}
	// Defense-in-depth: a zero-length payload would also panic the smt
	// library (data[:1] in isLeafNode). Reject explicitly so we never
	// hand an empty slice up the stack.
	if len(val) == 0 {
		observability.SMSTStoreOperations.WithLabelValues("get", "not_found").Inc()
		return nil, fmt.Errorf("%w: empty payload for field=%s hash=%s",
			ErrSMSTNodeMissing, field, s.hashKey)
	}
	observability.SMSTStoreOperations.WithLabelValues("get", "success").Inc()
	return val, nil
}

// Set stores a value in the Redis hash.
//
// The key is hex-encoded before being used as a Redis hash field name.
// If the key already exists, its value is overwritten.
//
// When pipelining is enabled (via BeginPipeline), Set() buffers the operation
// instead of executing it immediately. Call FlushPipeline() to execute all buffered operations.
func (s *RedisMapStore) Set(key, value []byte) error {
	field := hex.EncodeToString(key)

	// Check if we're in pipeline mode
	s.pipelineMu.Lock()
	if s.pipelineEnabled {
		// Buffer the operation instead of executing immediately.
		// Make a copy of value to avoid memory aliasing issues.
		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)
		s.pipelineBuffer[field] = valueCopy
		// If the field was previously marked for deletion (unlikely — SMT
		// node digests are content-addressed — but possible on hash reuse),
		// un-orphan it so the pending HDEL doesn't wipe the value we just
		// wrote when the next checkpoint flushes.
		delete(s.orphanBuffer, field)
		s.pipelineMu.Unlock()
		return nil
	}
	// Non-pipeline mode still has to respect the cross-update orphanBuffer:
	// if a prior pipelined Update marked this field for deferred HDEL and
	// the caller now rewrites the same digest via a direct HSET (ClearAll
	// callers, tests, future direct writers), the pending HDEL at the next
	// FlushOrphansWithLiveRoot would silently wipe the value we just wrote.
	// Drop the orphan record under the lock before releasing it so the
	// invariant (orphanBuffer = digests that are safe to HDEL) holds in
	// both modes.
	delete(s.orphanBuffer, field)
	s.pipelineMu.Unlock()

	// Not in pipeline mode, execute immediately
	start := time.Now()
	defer func() {
		observability.SMSTStoreOperationDuration.WithLabelValues("set").Observe(time.Since(start).Seconds())
	}()

	err := s.redisClient.HSet(s.ctx, s.hashKey, field, value).Err()
	if err != nil {
		observability.SMSTStoreOperations.WithLabelValues("set", "error").Inc()
		observability.SMSTStoreErrors.WithLabelValues("set", "store_error").Inc()
		return err
	}
	observability.SMSTStoreOperations.WithLabelValues("set", "success").Inc()
	return nil
}

// Delete removes a key from the Redis hash.
//
// If the key doesn't exist, this operation is a no-op and returns nil.
//
// Pipeline mode (the only mode the SMST manager uses): the delete is deferred
// to the orphanBuffer and applied atomically at the next live_root checkpoint
// via FlushOrphansWithLiveRoot. Deleting orphans immediately would corrupt
// the previous checkpoint's tree — the SMT library deletes orphaned inner
// nodes on every Commit, and a follower resuming from a stale live_root
// then panics on a zero-length slice when traversing into the missing child.
//
// Non-pipeline mode keeps the immediate HDEL for ClearAll / direct callers
// (unused today by the SMST manager but kept for the kvstore interface).
func (s *RedisMapStore) Delete(key []byte) error {
	field := hex.EncodeToString(key)

	s.pipelineMu.Lock()
	if s.pipelineEnabled {
		// Defer orphan delete. Drop any in-flight Set() for the same field
		// so a mid-Update "set then delete same digest" ends up as a delete.
		delete(s.pipelineBuffer, field)
		s.orphanBuffer[field] = struct{}{}
		s.pipelineMu.Unlock()
		return nil
	}
	// Non-pipeline mode symmetry: a direct HDEL for a field that is also
	// buffered for HSET in pipelineBuffer would be undone by the next
	// FlushPipeline. Drop any matching pipelined write under the lock so
	// "direct delete wins" in the same way "direct set wins over orphan"
	// above. This only matters if someone interleaves direct and pipelined
	// calls on the same store — today that is hypothetical, but keeping
	// the two modes symmetric avoids a subtle foot-gun for future callers.
	delete(s.pipelineBuffer, field)
	s.pipelineMu.Unlock()

	start := time.Now()
	defer func() {
		observability.SMSTStoreOperationDuration.WithLabelValues("delete").Observe(time.Since(start).Seconds())
	}()

	err := s.redisClient.HDel(s.ctx, s.hashKey, field).Err()
	if err != nil {
		observability.SMSTStoreOperations.WithLabelValues("delete", "error").Inc()
		observability.SMSTStoreErrors.WithLabelValues("delete", "store_error").Inc()
		return err
	}
	observability.SMSTStoreOperations.WithLabelValues("delete", "success").Inc()
	return nil
}

// Len returns the number of keys in the Redis hash.
//
// This operation is O(1) as it uses Redis's HLEN command.
func (s *RedisMapStore) Len() (int, error) {
	start := time.Now()
	defer func() {
		observability.SMSTStoreOperationDuration.WithLabelValues("len").Observe(time.Since(start).Seconds())
	}()

	count, err := s.redisClient.HLen(s.ctx, s.hashKey).Result()
	if err != nil {
		observability.SMSTStoreOperations.WithLabelValues("len", "error").Inc()
		observability.SMSTStoreErrors.WithLabelValues("len", "store_error").Inc()
		return 0, err
	}
	observability.SMSTStoreOperations.WithLabelValues("len", "success").Inc()
	return int(count), nil
}

// ClearAll deletes the entire Redis hash.
//
// This is an atomic operation that removes all SMST nodes for the session.
// After calling ClearAll, Len() will return 0.
func (s *RedisMapStore) ClearAll() error {
	start := time.Now()
	defer func() {
		observability.SMSTStoreOperationDuration.WithLabelValues("clear_all").Observe(time.Since(start).Seconds())
	}()

	err := s.redisClient.Del(s.ctx, s.hashKey).Err()
	if err != nil {
		observability.SMSTStoreOperations.WithLabelValues("clear_all", "error").Inc()
		observability.SMSTStoreErrors.WithLabelValues("clear_all", "store_error").Inc()
		return err
	}
	observability.SMSTStoreOperations.WithLabelValues("clear_all", "success").Inc()
	return nil
}

// BeginPipeline enables buffering mode for Set()/Delete() operations.
// All subsequent Set() calls will be buffered until FlushPipeline() is called.
// Delete() calls accumulate in the orphanBuffer across multiple Updates and
// are flushed atomically with the next live_root SET by FlushOrphansWithLiveRoot.
// This is used during SMST Commit() to batch 10-20 HSET operations into a
// single round trip while keeping orphan deletions deferred for HA correctness.
func (s *RedisMapStore) BeginPipeline() {
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()

	s.pipelineEnabled = true
	// The orphan buffer must persist across BeginPipeline calls — it is owned
	// by the checkpoint cycle, not the Update cycle. Clearing it here would
	// silently drop pending HDELs.
	//
	// The Set buffer used to be reset here too. It no longer is: a buffer that
	// is not empty at this point holds the nodes of a FlushPipeline that
	// failed. Commit marked those nodes persisted before handing them over, so
	// the trie never sends them again, and compaction trusts that mark to drop
	// a leaf's in-memory value -- reset here, that leaf ends up in neither
	// memory nor Redis and its proof fails. Kept, they go with this Update's
	// flush.
	// s.pipelineBuffer = make(map[string][]byte)
}

// FlushPipeline executes all buffered Set() operations in a single Redis HSET command.
// This provides 8-10× speedup compared to individual HSET calls during SMST Commit().
//
// Only the pipelineBuffer (new-node writes) is flushed here. The orphanBuffer
// is intentionally left untouched so it can be flushed atomically with the
// next live_root checkpoint via FlushOrphansWithLiveRoot.
//
// After flushing, pipeline mode is disabled and subsequent Set()/Delete()
// calls execute immediately.
func (s *RedisMapStore) FlushPipeline() error {
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()

	// Pipeline mode ends whether or not the write succeeds.
	s.pipelineEnabled = false
	if err := s.writePendingNodesLocked(); err != nil {
		return fmt.Errorf("failed to flush pipeline: %w", err)
	}
	return nil
}

// FlushPendingNodes wrote the nodes a failed FlushPipeline left buffered, for a
// writer of a root to call before storing the root. Every such writer now
// commits the tree first (RedisSMSTManager.commitLocked), and that commit's
// FlushPipeline writes the whole buffer, the failed write's nodes included.
//
// func (s *RedisMapStore) FlushPendingNodes() error {
// 	s.pipelineMu.Lock()
// 	defer s.pipelineMu.Unlock()
// 	return s.writePendingNodesLocked()
// }

// nodesWriteChunkBytes bounds the bytes one HSET of a nodes write carries. A
// relay batch commits up to relayBatchCap leaves at once, each holding the raw
// relay bytes, and a single HSET of all of them is one command the
// single-threaded Redis runs start to end before serving anyone else. Split,
// other clients' commands interleave between the pieces, which still travel in
// one round trip. The size is not measured: it was picked against relays of
// about 1 KiB (a median measured in an earlier load, not here) times the cap.
const nodesWriteChunkBytes = 256 << 10

// writePendingNodesLocked sends pipelineBuffer to the nodes hash, as HSETs of at
// most nodesWriteChunkBytes each in one round trip, and empties it only if every
// piece was written; on failure the nodes stay buffered for the next write, and
// the pieces that did land are written again, which HSET makes harmless. The
// caller holds pipelineMu.
func (s *RedisMapStore) writePendingNodesLocked() error {
	if len(s.pipelineBuffer) == 0 {
		return nil
	}

	start := time.Now()
	defer func() {
		observability.SMSTStoreOperationDuration.WithLabelValues("flush_pipeline").Observe(time.Since(start).Seconds())
	}()

	// Build field-value pairs for HSET
	// Redis HSET accepts: HSET key field1 value1 field2 value2 ...
	var chunks [][]interface{}
	args := make([]interface{}, 0, len(s.pipelineBuffer)*2)
	chunkBytes := 0
	for field, value := range s.pipelineBuffer {
		if len(args) > 0 && chunkBytes+len(field)+len(value) > nodesWriteChunkBytes {
			chunks = append(chunks, args)
			args = make([]interface{}, 0, len(s.pipelineBuffer)*2-len(args))
			chunkBytes = 0
		}
		args = append(args, field, value)
		chunkBytes += len(field) + len(value)
	}
	chunks = append(chunks, args)

	// Execute batched HSET
	var err error
	if len(chunks) == 1 {
		err = s.redisClient.HSet(s.ctx, s.hashKey, chunks[0]...).Err()
	} else {
		_, err = s.redisClient.Pipelined(s.ctx, func(pipe redis.Pipeliner) error {
			for _, chunk := range chunks {
				pipe.HSet(s.ctx, s.hashKey, chunk...)
			}
			return nil
		})
	}
	if err != nil {
		observability.SMSTStoreOperations.WithLabelValues("flush_pipeline", "error").Inc()
		observability.SMSTStoreErrors.WithLabelValues("flush_pipeline", "store_error").Inc()
		return err
	}

	// Track metrics (count as bulk operation)
	observability.SMSTStoreOperations.WithLabelValues("flush_pipeline", "success").Inc()
	observability.SMSTStoreOperations.WithLabelValues("set", "success").Add(float64(len(s.pipelineBuffer)))

	s.pipelineBuffer = make(map[string][]byte)
	return nil
}

// FlushOrphansWithLiveRoot atomically applies all buffered orphan
// deletions, sets the live_root key, and refreshes the TTL on both the
// nodes hash and the live_root key in a single Redis MULTI/EXEC
// transaction.
//
// Consistency anchor: before this transaction runs, live_root points to
// the previous checkpoint (whose nodes are still in the hash because
// orphans are deferred); after it runs, live_root points to the new
// checkpoint (whose nodes were written by earlier FlushPipeline calls)
// and the superseded orphans are gone.
//
// Sliding TTL: the nodes hash TTL is set once at GetOrCreateTree time
// (smst_manager.go). Without refresh, a session whose relay stream
// keeps the tree active longer than cacheTTL sees its nodes hash expire
// in Redis while the in-memory tree still thinks every node is present
// — the next traversal then hits a missing digest and would panic in
// parseSumTrieNode (now surfaces as ErrSMSTNodeMissing via our Get).
// Refreshing both keys here makes the TTL a sliding window as long as
// the session is active, and cleanly lets them expire together once
// the session goes silent without a DeleteTree.
//
// Must be called with pipeline mode OFF (after FlushPipeline).
// cacheTTL == 0 disables the TTL refresh (used in tests and for
// operators who want the nodes hash to persist indefinitely).
// On transaction failure the orphanBuffer is preserved so the next
// checkpoint can retry, and live_root stays at its previous value.
func (s *RedisMapStore) FlushOrphansWithLiveRoot(
	ctx context.Context,
	liveRootKey string,
	liveRoot []byte,
	cacheTTL time.Duration,
) error {
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()

	// Nodes before the root. A failed FlushPipeline can have left nodes that
	// this root references only in the buffer; they are written first, outside
	// the MULTI below, because nodes no root points at are harmless and a root
	// without its nodes is not. If they cannot be written, nothing else is
	// sent: the orphans are kept and live_root stays at its previous value.
	if err := s.writePendingNodesLocked(); err != nil {
		return fmt.Errorf("write buffered nodes before live_root: %w", err)
	}

	start := time.Now()
	defer func() {
		observability.SMSTStoreOperationDuration.
			WithLabelValues("flush_orphans_live_root").Observe(time.Since(start).Seconds())
	}()

	pipe := s.redisClient.TxPipeline()

	orphanCount := len(s.orphanBuffer)
	if orphanCount > 0 {
		fields := make([]string, 0, orphanCount)
		for f := range s.orphanBuffer {
			fields = append(fields, f)
		}
		pipe.HDel(ctx, s.hashKey, fields...)
	}
	pipe.Set(ctx, liveRootKey, liveRoot, 0)

	// Sliding TTL on both keys: as long as UpdateTree keeps firing, the
	// TTL gets pushed out. When the session goes idle without a
	// DeleteTree (crash, abandoned, etc.) the keys expire together so
	// live_root never outlives the nodes it references.
	if cacheTTL > 0 {
		pipe.Expire(ctx, s.hashKey, cacheTTL)
		pipe.Expire(ctx, liveRootKey, cacheTTL)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		observability.SMSTStoreOperations.
			WithLabelValues("flush_orphans_live_root", "error").Inc()
		observability.SMSTStoreErrors.
			WithLabelValues("flush_orphans_live_root", "store_error").Inc()
		// Preserve orphanBuffer so the next checkpoint can retry.
		return fmt.Errorf("atomic orphan+live_root flush: %w", err)
	}

	observability.SMSTStoreOperations.
		WithLabelValues("flush_orphans_live_root", "success").Inc()
	if orphanCount > 0 {
		observability.SMSTStoreOperations.
			WithLabelValues("delete", "success").Add(float64(orphanCount))
	}

	s.orphanBuffer = make(map[string]struct{})
	return nil
}

// Verify interface compliance at compile time.
var _ kvstore.MapStore = (*RedisMapStore)(nil)
