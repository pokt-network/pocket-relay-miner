package miner

// Cold tree compaction: a claimed SMST stored as its leaves only.
//
// Once a session's claim is sent its tree never changes again, and the only
// thing still read from it is one proof. The nodes hash holds every leaf plus
// every inner and extension node; the leaves alone rebuild the same root, since
// a sum trie's structure is a function of its leaf paths. So the tree is stored
// as a versioned header and a compressed frame of its leaves, the blob is read
// back and rebuilt against the claimed root, and only on a match is the nodes
// hash deleted. A proof then rebuilds the tree in memory, checks the root again,
// proves, and discards the rebuild; nothing is written back to Redis.
//
// It always runs, and a miner binary without this code cannot prove a compacted
// tree: it has nothing that reads the blob, and walking the trie finds no nodes.
// Rolling back past it while compacted sessions await their proof loses those
// proofs.
//
// Rebuilt nodes are not always byte-identical to the originals: an extension
// node carries path bits outside its bounds that depend on insertion order, and
// the hash does not record that order. Measured on real claimed trees: the
// rebuilt root was the claimed root, every proof verified, and the bytes that
// differed were only those bits, in a proof's SiblingData, which the verifier
// hashes within bounds.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/pokt-network/smt/kvstore"
	"github.com/pokt-network/smt/kvstore/simplemap"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	// coldLeavesVersion is the blob layout below. A reader refuses any other
	// value rather than guess at it.
	coldLeavesVersion = 1
	// coldLeavesCodecZstd marks the leaf frame as one zstd frame.
	coldLeavesCodecZstd = 1
	// coldLeavesHeaderLen: version(1) codec(1) count(8) sum(8) root(SMSTRootLen).
	coldLeavesHeaderLen = 2 + 8 + 8 + SMSTRootLen

	// coldLeafPathLen and coldLeafMetaLen describe an encoded leaf node:
	// 0x00 | path(32) | value | weight(8) | count(8), value being the raw relay
	// bytes because the miner's value hasher is nil.
	coldLeafPathLen = 32
	coldLeafMetaLen = 16

	// coldLeavesScanCount is the HSCAN batch. A nodes hash of 100k leaves is
	// ~120 MB; one HGETALL would build that reply on Redis's only thread.
	coldLeavesScanCount = 1000

	// coldDecodedMaxBytes bounds what a blob may decompress to. 100k leaves of
	// ~700 B serialize to ~74 MB (synthetic measurement), so 1 GiB is far above
	// any tree this miner builds and still stops a corrupt frame from asking
	// for more.
	coldDecodedMaxBytes = 1 << 30

	// coldCompactionAttempts and coldCompactionRetryDelay bound the retries of
	// a compaction that failed on something transient (a rejected SET under
	// maxmemory, a read error, a missing claimed_root). The claim-to-proof
	// distance is tens of blocks, so ten tries thirty seconds apart cover the
	// part of it where freeing memory matters (inferred, not measured).
	coldCompactionAttempts   = 10
	coldCompactionRetryDelay = 30 * time.Second

	// coldCompactionWorkers sizes the process-wide compaction subpool (see
	// NewSupplierManager). Not measured under load.
	coldCompactionWorkers = 2
)

// coldCompactionResult is the label value of SMSTColdCompactions.
type coldCompactionResult string

const (
	coldCompacted        coldCompactionResult = "compacted"
	coldAlreadyCompacted coldCompactionResult = "already_compacted"
	coldNoTree           coldCompactionResult = "no_tree"
	coldNotReady         coldCompactionResult = "not_ready"
	coldReadFailed       coldCompactionResult = "read_failed"
	coldSetFailed        coldCompactionResult = "set_failed"
	coldDeleteFailed     coldCompactionResult = "delete_failed"
	coldMismatch         coldCompactionResult = "mismatch"
)

// retryable reports whether a later attempt can change the outcome. A mismatch
// cannot: the same hash rebuilds the same wrong root.
func (r coldCompactionResult) retryable() bool {
	switch r {
	case coldNotReady, coldReadFailed, coldSetFailed, coldDeleteFailed:
		return true
	default:
		return false
	}
}

// errColdRootMismatch is returned when leaves rebuild a root other than the
// claimed one.
var errColdRootMismatch = errors.New("leaves do not rebuild the claimed root")

// coldLeaf is one leaf of a claimed tree.
type coldLeaf struct {
	path   []byte
	value  []byte
	weight uint64
}

// coldIdentityPathHasher inserts a leaf at its stored path. The leaf node
// stores the path, not the key it was hashed from, so a rebuild cannot rehash.
type coldIdentityPathHasher struct{}

func (coldIdentityPathHasher) Path(key []byte) []byte { return key }
func (coldIdentityPathHasher) PathSize() int          { return coldLeafPathLen }

var (
	coldCodecOnce    sync.Once
	coldEncoder      *zstd.Encoder
	coldDecoder      *zstd.Decoder
	errColdCodecInit error
)

// coldCodec returns the process-wide zstd encoder and decoder. klauspost's
// EncodeAll and DecodeAll are documented as callable concurrently.
func coldCodec() (*zstd.Encoder, *zstd.Decoder, error) {
	coldCodecOnce.Do(func() {
		coldEncoder, errColdCodecInit = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if errColdCodecInit != nil {
			return
		}
		coldDecoder, errColdCodecInit = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(coldDecodedMaxBytes))
	})
	return coldEncoder, coldDecoder, errColdCodecInit
}

// encodeColdLeaves builds the blob: header, then one zstd frame of the leaves
// sorted by path, each as uvarint(len(value)) | path | weight | value.
func encodeColdLeaves(root []byte, leaves []coldLeaf) ([]byte, error) {
	if len(root) != SMSTRootLen {
		return nil, fmt.Errorf("root has length %d, expected %d", len(root), SMSTRootLen)
	}
	enc, _, err := coldCodec()
	if err != nil {
		return nil, fmt.Errorf("zstd codec: %w", err)
	}
	sort.Slice(leaves, func(i, j int) bool { return bytes.Compare(leaves[i].path, leaves[j].path) < 0 })

	size := 0
	for _, l := range leaves {
		size += binary.MaxVarintLen64 + coldLeafPathLen + 8 + len(l.value)
	}
	raw := make([]byte, 0, size)
	var weight [8]byte
	for _, l := range leaves {
		raw = binary.AppendUvarint(raw, uint64(len(l.value)))
		raw = append(raw, l.path...)
		binary.BigEndian.PutUint64(weight[:], l.weight)
		raw = append(raw, weight[:]...)
		raw = append(raw, l.value...)
	}

	blob := make([]byte, coldLeavesHeaderLen, coldLeavesHeaderLen+len(raw)/4)
	blob[0] = coldLeavesVersion
	blob[1] = coldLeavesCodecZstd
	root48 := smt.MerkleSumRoot(root)
	count, _ := root48.Count() //nolint:errcheck // length checked above
	sum, _ := root48.Sum()     //nolint:errcheck // length checked above
	binary.BigEndian.PutUint64(blob[2:10], count)
	binary.BigEndian.PutUint64(blob[10:18], sum)
	copy(blob[18:], root)
	return enc.EncodeAll(raw, blob), nil
}

// decodeColdLeaves parses a blob. Every leaf value is cut with its capacity
// capped: SMST.Update appends weight and count to the value it is given, and
// an uncapped slice of the shared buffer would write over the next leaf.
func decodeColdLeaves(blob []byte) (root []byte, leaves []coldLeaf, err error) {
	if len(blob) < coldLeavesHeaderLen {
		return nil, nil, fmt.Errorf("blob of %d bytes is shorter than its header", len(blob))
	}
	if blob[0] != coldLeavesVersion {
		return nil, nil, fmt.Errorf("unknown leaves blob version %d", blob[0])
	}
	if blob[1] != coldLeavesCodecZstd {
		return nil, nil, fmt.Errorf("unknown leaves blob codec %d", blob[1])
	}
	count := binary.BigEndian.Uint64(blob[2:10])
	sum := binary.BigEndian.Uint64(blob[10:18])
	root = blob[18:coldLeavesHeaderLen:coldLeavesHeaderLen]
	if rootCount, _ := smt.MerkleSumRoot(root).Count(); rootCount != count { //nolint:errcheck // fixed length
		return nil, nil, fmt.Errorf("header count %d does not match its root's %d", count, rootCount)
	}
	if rootSum, _ := smt.MerkleSumRoot(root).Sum(); rootSum != sum { //nolint:errcheck // fixed length
		return nil, nil, fmt.Errorf("header sum %d does not match its root's %d", sum, rootSum)
	}

	_, dec, err := coldCodec()
	if err != nil {
		return nil, nil, fmt.Errorf("zstd codec: %w", err)
	}
	raw, err := dec.DecodeAll(blob[coldLeavesHeaderLen:], nil)
	if err != nil {
		return nil, nil, fmt.Errorf("decompress leaves: %w", err)
	}

	leaves = make([]coldLeaf, 0, count)
	for pos := 0; pos < len(raw); {
		n, read := binary.Uvarint(raw[pos:])
		if read <= 0 {
			return nil, nil, fmt.Errorf("leaf %d: bad value length", len(leaves))
		}
		pos += read
		end := pos + coldLeafPathLen + 8 + int(n)
		if n > uint64(len(raw)) || end > len(raw) {
			return nil, nil, fmt.Errorf("leaf %d: truncated", len(leaves))
		}
		leaves = append(leaves, coldLeaf{
			path:   raw[pos : pos+coldLeafPathLen : pos+coldLeafPathLen],
			weight: binary.BigEndian.Uint64(raw[pos+coldLeafPathLen : pos+coldLeafPathLen+8]),
			value:  raw[pos+coldLeafPathLen+8 : end : end],
		})
		pos = end
	}
	if uint64(len(leaves)) != count {
		return nil, nil, fmt.Errorf("blob holds %d leaves, header says %d", len(leaves), count)
	}
	return root, leaves, nil
}

// rebuildColdTree inserts leaves by path into an in-memory store and returns
// that store and the root. The caller compares the root before trusting it.
func (m *RedisSMSTManager) rebuildColdTree(sessionID string, leaves []coldLeaf) (kvstore.MapStore, []byte, error) {
	store := simplemap.NewSimpleMap()
	var root []byte
	err := m.runSMSTSafely(sessionID, "cold_rebuild", func() error {
		trie := smt.NewSparseMerkleSumTrie(store, protocol.NewTrieHasher(), protocol.SMTValueHasher(),
			smt.WithPathHasher(coldIdentityPathHasher{}))
		for _, l := range leaves {
			if err := trie.Update(l.path, l.value, l.weight); err != nil {
				return err
			}
		}
		if err := trie.Commit(); err != nil {
			return err
		}
		root = trie.Root()
		return nil
	})
	return store, root, err
}

// readColdLeaves reads every leaf node of a nodes hash by HSCAN. A field HSCAN
// returns twice is counted once. hashBytes is the field and value bytes read.
func (m *RedisSMSTManager) readColdLeaves(ctx context.Context, hashKey string) (leaves []coldLeaf, hashBytes int, err error) {
	seen := make(map[string]struct{})
	var cursor uint64
	for {
		kvs, next, scanErr := m.redisClient.HScan(ctx, hashKey, cursor, "", coldLeavesScanCount).Result()
		if scanErr != nil {
			return nil, 0, scanErr
		}
		for i := 0; i+1 < len(kvs); i += 2 {
			field, node := kvs[i], kvs[i+1]
			if _, dup := seen[field]; dup {
				continue
			}
			seen[field] = struct{}{}
			hashBytes += len(field) + len(node)
			if len(node) == 0 || node[0] != 0 {
				continue // inner (0x01) or extension (0x02) node
			}
			if len(node) < 1+coldLeafPathLen+coldLeafMetaLen {
				return nil, 0, fmt.Errorf("leaf node %s has %d bytes", field, len(node))
			}
			if leafCount := binary.BigEndian.Uint64([]byte(node[len(node)-8:])); leafCount != 1 {
				return nil, 0, fmt.Errorf("leaf node %s has count %d", field, leafCount)
			}
			valueEnd := len(node) - coldLeafMetaLen
			leaves = append(leaves, coldLeaf{
				path:   []byte(node[1 : 1+coldLeafPathLen]),
				value:  []byte(node[1+coldLeafPathLen : valueEnd]),
				weight: binary.BigEndian.Uint64([]byte(node[valueEnd : valueEnd+8])),
			})
		}
		cursor = next
		if cursor == 0 {
			return leaves, hashBytes, nil
		}
	}
}

// CompactColdTree stores a claimed session's tree as its leaves blob and deletes
// its nodes hash. The order is what makes it safe: the blob is SET, read back,
// and rebuilt; only a rebuild equal to the claimed_root stored in Redis lets the
// hash go. A failure at any step before that leaves the hash as it was. It does
// not look at the flag: ScheduleColdCompaction does.
func (m *RedisSMSTManager) CompactColdTree(ctx context.Context, sessionID string) (result coldCompactionResult, err error) {
	supplier := m.config.SupplierAddress
	start := time.Now()
	defer func() {
		observability.SMSTColdCompactions.WithLabelValues(supplier, string(result)).Inc()
		observability.SMSTColdDuration.WithLabelValues(supplier, "compact").Observe(time.Since(start).Seconds())
	}()

	kb := m.redisClient.KB()
	nodesKey := kb.SMSTNodesKey(supplier, sessionID)
	leavesKey := kb.SMSTLeavesKey(supplier, sessionID)

	claimedRoot, err := m.redisClient.Get(ctx, kb.SMSTRootKey(supplier, sessionID)).Bytes()
	if errors.Is(err, redis.Nil) || (err == nil && !isValidSMSTRoot(claimedRoot)) {
		return coldNotReady, fmt.Errorf("session %s: no valid claimed_root in Redis", sessionID)
	}
	if err != nil {
		return coldReadFailed, fmt.Errorf("read claimed_root: %w", err)
	}

	present, err := m.redisClient.Exists(ctx, nodesKey).Result()
	if err != nil {
		return coldReadFailed, fmt.Errorf("check nodes hash: %w", err)
	}
	if present == 0 {
		blobs, existsErr := m.redisClient.Exists(ctx, leavesKey).Result()
		if existsErr != nil {
			return coldReadFailed, fmt.Errorf("check leaves blob: %w", existsErr)
		}
		if blobs == 1 {
			return coldAlreadyCompacted, nil
		}
		return coldNoTree, nil
	}

	// A compaction starts only when no proof waits for memory, and holds its
	// turn until the blob is verified, before it takes the tree's lock.
	leafCount, _ := smt.MerkleSumRoot(claimedRoot).Count() //nolint:errcheck // validated above
	_, release, err := m.config.RebuildAdmission.acquire(ctx, rebuildKindCompaction, leafCount*compactionLeafEstimateBytes, 0)
	if err != nil {
		return coldReadFailed, fmt.Errorf("wait for memory to compact: %w", err)
	}
	defer release()

	leaves, hashBytes, err := m.readColdLeaves(ctx, nodesKey)
	if err != nil {
		return coldReadFailed, fmt.Errorf("read leaves: %w", err)
	}
	blob, err := encodeColdLeaves(claimedRoot, leaves)
	if err != nil {
		return coldReadFailed, fmt.Errorf("encode leaves: %w", err)
	}
	// SET is refused under maxmemory; nothing has been deleted yet, so the
	// hash stays whole and a later attempt starts over.
	if err := m.redisClient.Set(ctx, leavesKey, blob, m.config.CacheTTL).Err(); err != nil {
		return coldSetFailed, fmt.Errorf("store leaves blob: %w", err)
	}

	// Verify what Redis holds, not the bytes in hand.
	stored, err := m.redisClient.Get(ctx, leavesKey).Bytes()
	if err != nil {
		return coldReadFailed, fmt.Errorf("read back leaves blob: %w", err)
	}
	if verifyErr := m.verifyColdBlob(sessionID, stored, claimedRoot); verifyErr != nil {
		if delErr := m.redisClient.Unlink(ctx, leavesKey).Err(); delErr != nil {
			verifyErr = errors.Join(verifyErr, fmt.Errorf("unlink mismatched blob: %w", delErr))
		}
		m.logger.Warn().
			Err(verifyErr).
			Str(logging.FieldSessionID, sessionID).
			Int("leaves", len(leaves)).
			Msg("leaves of the claimed SMST do not rebuild its claimed root; keeping the nodes hash")
		return coldMismatch, verifyErr
	}
	release()

	// The resident tree, if any, is held while the hash goes, so a proof that
	// holds it runs entirely before or entirely after; and it is dropped, so
	// the next reader loads the session from Redis and finds the blob.
	m.treesMu.RLock()
	tree := m.trees[sessionID]
	m.treesMu.RUnlock()
	if tree != nil {
		tree.mu.Lock()
		defer tree.mu.Unlock()
	}
	// The hash goes only if the blob Redis holds is still the one verified
	// above, so a compaction of the same session in another process -- one that
	// read the hash while this one was deleting it, and stored a blob of
	// partial leaves -- cannot leave the session with neither.
	//
	// The two writes are not one transaction on purpose: the blob is verified
	// from what Redis stored, which cannot be read inside a MULTI. What that
	// costs is a crash between them, which leaves the blob written and the hash
	// whole -- the safe way round, and the next attempt starts over. Under
	// maxmemory nothing is half done either: measured 2026-09-17 against Redis
	// 8 with maxmemory reached, a SET inside MULTI is refused when it is queued
	// ("OOM command not allowed"), EXEC answers EXECABORT, and the hash is
	// untouched.
	deleted, err := unlinkNodesIfBlobScript.Run(ctx, m.redisClient, []string{nodesKey, leavesKey}, stored).Int64()
	if err != nil {
		return coldDeleteFailed, fmt.Errorf("unlink nodes hash: %w", err)
	}
	if deleted == 0 {
		m.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Msg("the leaves blob changed while the claimed SMST was being compacted; keeping the nodes hash")
		return coldMismatch, fmt.Errorf("session %s: the leaves blob changed while compacting", sessionID)
	}
	if tree != nil {
		m.treesMu.Lock()
		if m.trees[sessionID] == tree {
			delete(m.trees, sessionID)
		}
		m.treesMu.Unlock()
	}

	observability.SMSTColdCompactionBytes.WithLabelValues(supplier, "hash").Add(float64(hashBytes))
	observability.SMSTColdCompactionBytes.WithLabelValues(supplier, "blob").Add(float64(len(blob)))
	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Int("leaves", len(leaves)).
		Int("hash_bytes", hashBytes).
		Int("blob_bytes", len(blob)).
		Dur("took", time.Since(start)).
		Msg("compacted claimed SMST to its leaves")
	return coldCompacted, nil
}

// verifyColdBlob decodes a blob and rebuilds it; nil only when both the blob's
// own root and the rebuilt one are the claimed root.
func (m *RedisSMSTManager) verifyColdBlob(sessionID string, blob, claimedRoot []byte) error {
	blobRoot, leaves, err := decodeColdLeaves(blob)
	if err != nil {
		return err
	}
	if !bytes.Equal(blobRoot, claimedRoot) {
		return fmt.Errorf("%w: blob header root %x, claimed %x", errColdRootMismatch, blobRoot, claimedRoot)
	}
	_, rebuilt, err := m.rebuildColdTree(sessionID, leaves)
	if err != nil {
		return err
	}
	if !bytes.Equal(rebuilt, claimedRoot) {
		return fmt.Errorf("%w: rebuilt %x, claimed %x", errColdRootMismatch, rebuilt, claimedRoot)
	}
	return nil
}

// ScheduleColdCompaction queues the compaction of a session whose claim was
// just sent. There is no switch: every claimed tree is compacted. A retryable
// failure is tried again after coldCompactionRetryDelay, up to
// coldCompactionAttempts, and not once the session's tree has been deleted or
// the manager closed.
func (m *RedisSMSTManager) ScheduleColdCompaction(ctx context.Context, sessionID string) {
	// The claim path's context ends with the claim; the compaction outlives
	// it, and stops on Close instead.
	m.submitColdCompaction(context.WithoutCancel(ctx), sessionID, 1)
}

func (m *RedisSMSTManager) submitColdCompaction(ctx context.Context, sessionID string, attempt int) {
	task := func() {
		if m.closed.Load() || m.SessionDeleted(sessionID) {
			return
		}
		result, err := m.CompactColdTree(ctx, sessionID)
		if err != nil {
			m.logger.Debug().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Str("result", string(result)).
				Int("attempt", attempt).
				Msg("cold SMST compaction did not complete")
		}
		if !result.retryable() || attempt >= coldCompactionAttempts {
			return
		}
		m.coldAfterFunc(m.coldRetryDelay, func() {
			m.submitColdCompaction(ctx, sessionID, attempt+1)
		})
	}
	if m.config.ColdCompactionPool == nil {
		task()
		return
	}
	m.config.ColdCompactionPool.Submit(task)
}

// unlinkNodesIfBlobScript deletes a claimed tree's nodes hash only while the
// leaves blob is the one the caller verified, so two compactions of the same
// session cannot leave it with neither.
//
// KEYS[1] = nodes hash, KEYS[2] = leaves blob
// ARGV[1] = the blob as read back and verified
var unlinkNodesIfBlobScript = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then
	return 0
end
redis.call('UNLINK', KEYS[1])
return 1
`)

// coldTreeCompacted reports whether the session's tree is stored as a leaves
// blob only: the blob present and the nodes hash absent.
func (m *RedisSMSTManager) coldTreeCompacted(ctx context.Context, sessionID string) (bool, error) {
	kb := m.redisClient.KB()
	pipe := m.redisClient.Pipeline()
	nodes := pipe.Exists(ctx, kb.SMSTNodesKey(m.config.SupplierAddress, sessionID))
	blob := pipe.Exists(ctx, kb.SMSTLeavesKey(m.config.SupplierAddress, sessionID))
	if _, err := pipe.Exec(ctx); err != nil {
		return false, fmt.Errorf("check compacted SMST: %w", err)
	}
	return blob.Val() == 1 && nodes.Val() == 0, nil
}

// proveClosestFromLeaves rebuilds the claimed tree from its leaves blob in
// memory, checks the rebuilt root against claimedRoot, and proves on it. The
// load waits on RebuildAdmission, which bounds how many trees are in memory at
// once when a whole cohort of sessions reaches its proof window.
func (m *RedisSMSTManager) proveClosestFromLeaves(ctx context.Context, sessionID string, claimedRoot, path []byte) ([]byte, error) {
	supplier := m.config.SupplierAddress
	start := time.Now()
	result := "failed"
	defer func() {
		observability.SMSTColdRebuilds.WithLabelValues(supplier, result).Inc()
		observability.SMSTColdDuration.WithLabelValues(supplier, "rebuild").Observe(time.Since(start).Seconds())
	}()

	var proofBz []byte
	// loaded tells the admission this tree is in memory; set once admitted.
	loaded := func() {}
	rebuildAndProve := func() error {
		blob, err := m.redisClient.Get(ctx, m.redisClient.KB().SMSTLeavesKey(supplier, sessionID)).Bytes()
		if errors.Is(err, redis.Nil) {
			result = "missing"
			return fmt.Errorf("session %s: nodes hash and leaves blob are both absent", sessionID)
		}
		if err != nil {
			return fmt.Errorf("read leaves blob: %w", err)
		}
		_, leaves, err := decodeColdLeaves(blob)
		if err != nil {
			return fmt.Errorf("decode leaves blob: %w", err)
		}
		store, rebuilt, err := m.rebuildColdTree(sessionID, leaves)
		loaded()
		if err != nil {
			return fmt.Errorf("rebuild from leaves: %w", err)
		}
		if !bytes.Equal(rebuilt, claimedRoot) {
			result = "mismatch"
			m.logger.Error().
				Str(logging.FieldSessionID, sessionID).
				Str("claimed_root_hex", fmt.Sprintf("%x", claimedRoot)).
				Str("rebuilt_root_hex", fmt.Sprintf("%x", rebuilt)).
				Msg("ROOT MISMATCH: SMST rebuilt from its leaves blob != claimed root - no proof generated")
			return fmt.Errorf("session %s: %w: rebuilt %x, claimed %x", sessionID, errColdRootMismatch, rebuilt, claimedRoot)
		}
		trie := smt.ImportSparseMerkleSumTrie(store, protocol.NewTrieHasher(), claimedRoot, protocol.SMTValueHasher())
		var proof *smt.SparseMerkleClosestProof
		if err := m.runSMSTSafely(sessionID, "prove_closest_cold", func() error {
			p, proveErr := trie.ProveClosest(path)
			proof = p
			return proveErr
		}); err != nil {
			return fmt.Errorf("failed to prove closest: %w", err)
		}
		proofBz, err = marshalClosestProof(proof, trie.Spec())
		if err != nil {
			return err
		}
		result = "ok"
		return nil
	}

	// Admitted before the blob is read, so a tree waiting for memory does not
	// hold it, and the queue orders every waiter by value. The next tree is
	// asked once this one is loaded.
	estimate, err := m.coldRebuildEstimate(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	value, _ := smt.MerkleSumRoot(claimedRoot).Sum() //nolint:errcheck // claimedRoot validated at load
	var release func()
	loaded, release, err = m.config.RebuildAdmission.acquire(ctx, rebuildKindProof, estimate, value)
	if err != nil {
		return nil, fmt.Errorf("wait for memory to rebuild session %s: %w", sessionID, err)
	}
	defer release()

	if err := rebuildAndProve(); err != nil {
		return nil, err
	}
	m.logger.Info().
		Str(logging.FieldSessionID, sessionID).
		Str("proof_path_hex", fmt.Sprintf("%x", path)).
		Str("claimed_root_hex", fmt.Sprintf("%x", claimedRoot)).
		Int("proof_len", len(proofBz)).
		Msg("generated proof from SMST rebuilt from its leaves blob")
	return proofBz, nil
}

// coldRebuildEstimate is the heap a rebuild of the session's blob holds, read
// from the blob's header without fetching the blob: every leaf value twice plus
// rebuildLeafOverheadBytes per leaf. The frame's content size is what the
// values decode to; the encoder writes it for any frame of 256 B or more, and a
// smaller frame is too small to matter. A missing blob estimates zero, and the
// rebuild reports it.
func (m *RedisSMSTManager) coldRebuildEstimate(ctx context.Context, sessionID string) (uint64, error) {
	head, err := m.redisClient.GetRange(ctx, m.redisClient.KB().SMSTLeavesKey(m.config.SupplierAddress, sessionID),
		0, coldLeavesHeaderLen+zstd.HeaderMaxSize-1).Bytes()
	if err != nil {
		return 0, fmt.Errorf("read leaves blob header: %w", err)
	}
	if len(head) < coldLeavesHeaderLen {
		return 0, nil
	}
	count := binary.BigEndian.Uint64(head[2:10])
	var frame zstd.Header
	var decoded uint64
	if frame.Decode(head[coldLeavesHeaderLen:]) == nil && frame.HasFCS {
		decoded = frame.FrameContentSize
	}
	return 2*decoded + count*rebuildLeafOverheadBytes, nil
}

// marshalClosestProof compacts and marshals a closest proof the way the chain
// decodes it.
func marshalClosestProof(proof *smt.SparseMerkleClosestProof, spec *smt.TrieSpec) ([]byte, error) {
	compactProof, err := smt.CompactClosestProof(proof, spec)
	if err != nil {
		return nil, fmt.Errorf("failed to compact proof: %w", err)
	}
	proofBz, err := compactProof.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal proof: %w", err)
	}
	return proofBz, nil
}
