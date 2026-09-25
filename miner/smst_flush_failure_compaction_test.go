//go:build test

package miner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/pokt-network/smt"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// A failed FlushPipeline leaves the trie in a state compaction can misread.
//
// Commit marks a node persisted once the store's Set returns without error, and
// in pipeline mode Set only buffers the write; FlushPipeline is what sends it.
// The leaf is compacted right after Commit, before the flush, so when that
// flush fails the buffered node is the only copy of the leaf's bytes. The store
// must therefore keep those nodes buffered for the next write and serve them
// from the buffer until then: dropped, the leaf would exist nowhere.
//
// An update writes nothing: the tree is committed once per relay batch, before
// a root is stored or a relay acknowledged. The scenarios commit where the
// batch would.
//
// Each scenario walks the chain one link at a time and asserts every link on
// its own, with compaction enabled and, as the control, with the compactor
// hidden from the manager.

// noCompactor hides CompactPersistedLeaves from commitLocked's type assertion:
// the method set of a struct embedding an interface is that interface's, so
// the manager sees a trie without the capability and never compacts. Every
// other trie method still reaches the real trie.
type noCompactor struct {
	smt.SparseMerkleSumTrie
}

type flushFailureRelay struct {
	key    []byte
	value  []byte
	weight uint64
}

func newFlushFailureRelay(label string, weight uint64) flushFailureRelay {
	key := sha256.Sum256([]byte("flush-failure-" + label))
	return flushFailureRelay{
		key:    key[:],
		value:  bytes.Repeat([]byte(label), 64),
		weight: weight,
	}
}

// update hands the manager a fresh copy of the value on every call. SMST.Update
// appends weight and count to the slice it is given, so a shared backing array
// would let one call's append leak into the next call's "same" value.
func (r flushFailureRelay) update(ctx context.Context, mgr *RedisSMSTManager, sessionID string) error {
	return mgr.UpdateTree(ctx, sessionID, bytes.Clone(r.key), bytes.Clone(r.value), r.weight)
}

// leafField is the Redis hash field Commit writes this relay's leaf under: the
// hex of the leaf digest, which for a sum trie is
// sha256(0x00 || path || value || weight || count) || weight || count, with
// path = sha256(key). It is computed here because the trie does not export it;
// seed asserts the formula against a leaf written cleanly before any link is
// examined.
func (r flushFailureRelay) leafField() string {
	var weightBz, countBz [8]byte
	binary.BigEndian.PutUint64(weightBz[:], r.weight)
	binary.BigEndian.PutUint64(countBz[:], 1)

	path := sha256.Sum256(r.key)
	preimage := make([]byte, 0, 1+len(path)+len(r.value)+16)
	preimage = append(preimage, 0x00)
	preimage = append(preimage, path[:]...)
	preimage = append(preimage, r.value...)
	preimage = append(preimage, weightBz[:]...)
	preimage = append(preimage, countBz[:]...)

	digest := sha256.Sum256(preimage)
	field := make([]byte, 0, len(digest)+16)
	field = append(field, digest[:]...)
	field = append(field, weightBz[:]...)
	field = append(field, countBz[:]...)
	return hex.EncodeToString(field)
}

func (r flushFailureRelay) path() []byte {
	p := sha256.Sum256(r.key)
	return p[:]
}

type flushFailureHarness struct {
	t         *testing.T
	ctx       context.Context
	client    *redisutil.Client
	mgr       *RedisSMSTManager
	fail      *testredis.FailSwitch
	tree      *redisSMST
	sessionID string
	supplier  string
	compact   bool
}

func newFlushFailureHarness(t *testing.T, compact bool) *flushFailureHarness {
	t.Helper()
	ctx := context.Background()
	client, _ := newTestRedis(t)

	const supplier = "pokt1flush_failure_supplier"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        0,
		// No update checkpoints on its own; every checkpoint is driven
		// explicitly, so the state between "the flush wrote X" and "the
		// orphan HDEL ran" can be observed.
		LiveRootCheckpointInterval: 1_000_000,
	})

	sessionID := fmt.Sprintf("sess-flush-failure-compact-%v", compact)
	tree, err := mgr.GetOrCreateTree(ctx, sessionID)
	require.NoError(t, err)
	if !compact {
		tree.trie = noCompactor{tree.trie}
	}

	return &flushFailureHarness{
		t:         t,
		ctx:       ctx,
		client:    client,
		mgr:       mgr,
		fail:      testredis.NewFailSwitch(client),
		tree:      tree,
		sessionID: sessionID,
		supplier:  supplier,
		compact:   compact,
	}
}

// inRedis reports whether the relay's leaf is in the session's nodes hash.
func (h *flushFailureHarness) inRedis(r flushFailureRelay) bool {
	h.t.Helper()
	hashKey := h.client.KB().SMSTNodesKey(h.supplier, h.sessionID)
	ok, err := h.client.HExists(h.ctx, hashKey, r.leafField()).Result()
	require.NoError(h.t, err)
	return ok
}

// inMemory reports whether the relay's value is reachable without Redis: a
// resident leaf, or a compacted one whose node the store still holds buffered
// after a failed flush. Get is run with every Redis command failing, so a
// compacted leaf whose node was written resolves from the store and fails, and
// the probe cannot hydrate the leaf it is looking at.
func (h *flushFailureHarness) inMemory(r flushFailureRelay) bool {
	h.t.Helper()
	h.fail.Fail("probe: redis unreachable")
	defer h.fail.Clear()
	value, _, err := h.tree.trie.Get(r.key)
	return err == nil && bytes.Equal(value, r.value)
}

// orphanPending reports whether the relay's leaf is queued for HDEL at the next
// checkpoint.
func (h *flushFailureHarness) orphanPending(r flushFailureRelay) bool {
	h.t.Helper()
	store, ok := h.tree.store.(*RedisMapStore)
	require.True(h.t, ok, "the tree must be backed by the real RedisMapStore")
	store.pipelineMu.Lock()
	defer store.pipelineMu.Unlock()
	_, pending := store.orphanBuffer[r.leafField()]
	return pending
}

// seed writes clean relays and runs the two controls every link below depends
// on. Without them, a wrong digest formula would read as "X is not in Redis",
// and a probe that cannot tell resident from compacted would read as whatever
// the scenario expected.
func (h *flushFailureHarness) seed(n int) {
	h.t.Helper()
	var first flushFailureRelay
	for i := 0; i < n; i++ {
		r := newFlushFailureRelay(fmt.Sprintf("seed-%d", i), uint64(i+1))
		require.NoError(h.t, r.update(h.ctx, h.mgr, h.sessionID))
		if i == 0 {
			first = r
		}
	}
	resident, _, err := h.mgr.CheckpointLiveRoot(h.ctx, h.sessionID)
	require.NoError(h.t, err)
	require.True(h.t, resident)

	require.True(h.t, h.inRedis(first),
		"CONTROL: a cleanly written leaf must be found under the computed field; "+
			"if not, the digest formula is wrong and every inRedis assertion below is meaningless")
	require.Equal(h.t, !h.compact, h.inMemory(first),
		"CONTROL: with compaction the seed leaf's value must be gone from memory, without it resident; "+
			"if not, the in-memory probe cannot tell the two apart")
}

// failFlush puts the relay in the tree, makes the commit that follows fail on
// its FlushPipeline, and asserts the failure came from the flush, not from
// anything earlier in the commit.
func (h *flushFailureHarness) failFlush(r flushFailureRelay) {
	h.t.Helper()
	require.NoError(h.t, r.update(h.ctx, h.mgr, h.sessionID), "LINK 1: an update writes nothing, so it cannot fail on Redis")
	h.fail.Fail("injected: flush pipeline failed")
	resident, err := h.mgr.CommitTree(h.ctx, h.sessionID)
	h.fail.Clear()

	require.True(h.t, resident, "LINK 1: the tree is resident")
	require.Error(h.t, err, "LINK 1: the injected failure must surface from the commit")
	require.ErrorIs(h.t, err, ErrSMSTCommitFailed, "LINK 1: the failure must be classified as a commit failure")
	require.Contains(h.t, err.Error(), "flush pipeline",
		"LINK 1: the failure must come from FlushPipeline, not from an earlier Redis call")
}

// commit writes the tree's uncommitted nodes, as the relay batch does before it
// stores a root or acknowledges a relay.
func (h *flushFailureHarness) commit() {
	h.t.Helper()
	resident, err := h.mgr.CommitTree(h.ctx, h.sessionID)
	require.NoError(h.t, err)
	require.True(h.t, resident)
}

// proveAndVerify checks the relay's proof against the sealed root, through the
// manager's own ProveClosest.
func (h *flushFailureHarness) proveAndVerify(r flushFailureRelay, root []byte) error {
	h.t.Helper()
	proofBz, err := h.mgr.ProveClosest(h.ctx, h.sessionID, r.path())
	if err != nil {
		return fmt.Errorf("ProveClosest: %w", err)
	}

	var compactProof smt.SparseCompactMerkleClosestProof
	if err := compactProof.Unmarshal(proofBz); err != nil {
		return fmt.Errorf("unmarshal proof: %w", err)
	}
	spec := h.tree.trie.Spec()
	proof, err := smt.DecompactClosestProof(&compactProof, spec)
	if err != nil {
		return fmt.Errorf("decompact proof: %w", err)
	}
	valid, err := smt.VerifyClosestProof(proof, root, spec)
	if err != nil {
		return fmt.Errorf("verify proof: %w", err)
	}
	if !valid {
		return fmt.Errorf("proof does not verify against the sealed root")
	}
	if !bytes.HasPrefix(proof.ClosestValueHash, r.value) {
		return fmt.Errorf("proof carries a different value than the relay inserted")
	}
	return nil
}

func compactionLabel(compact bool) string {
	if compact {
		return "compaction"
	}
	return "no-compaction"
}

// S1: the chain as it was first described. A's flush fails, A is redelivered
// immediately, the next checkpoint runs, and A is proven.
func TestFlushFailure_RedeliveredImmediately(t *testing.T) {
	for _, compact := range []bool{true, false} {
		t.Run(compactionLabel(compact), func(t *testing.T) {
			h := newFlushFailureHarness(t, compact)
			h.seed(8)
			a := newFlushFailureRelay("relay-a", 7)

			// LINK 1
			h.failFlush(a)
			t.Logf("LINK 1: inRedis(A)=%v inMemory(A)=%v", h.inRedis(a), h.inMemory(a))
			require.False(t, h.inRedis(a), "LINK 1: A's leaf must not have reached Redis")
			require.True(t, h.inMemory(a),
				"LINK 1: A's value must still be reachable without Redis -- its node stays buffered after the failed flush")

			// LINK 2
			require.NoError(t, a.update(h.ctx, h.mgr, h.sessionID), "LINK 2: the redelivered A must be accepted")
			h.commit()
			t.Logf("LINK 2: inRedis(A)=%v orphanPending(A)=%v", h.inRedis(a), h.orphanPending(a))
			require.True(t, h.inRedis(a), "LINK 2: the redelivery's flush must write A's leaf")
			require.False(t, h.orphanPending(a),
				"LINK 2: A's leaf must not be queued for HDEL -- the redelivery's Set must cancel the Delete of the leaf it replaced")

			// LINK 3
			resident, _, err := h.mgr.CheckpointLiveRoot(h.ctx, h.sessionID)
			require.NoError(t, err)
			require.True(t, resident)
			t.Logf("LINK 3: inRedis(A) after checkpoint=%v", h.inRedis(a))
			require.True(t, h.inRedis(a), "LINK 3: the checkpoint's orphan HDEL must not remove A's leaf")

			// LINK 4
			t.Logf("LINK 4: inMemory(A)=%v", h.inMemory(a))
			require.Equal(t, !compact, h.inMemory(a),
				"LINK 4: with compaction A's value leaves memory, without it stays")

			// LINK 5
			root, err := h.mgr.FlushTree(h.ctx, h.sessionID)
			require.NoError(t, err)
			err = h.proveAndVerify(a, root)
			t.Logf("LINK 5: proof of A: %v", err)
			require.NoError(t, err, "LINK 5: A's proof must verify")
		})
	}
}

// S2: other relays land between A's failed flush and its redelivery, and the
// redelivery only arrives once the session is sealed, so it is rejected and A
// is never rewritten.
func TestFlushFailure_OtherRelaysLandFirstAndRedeliveryMissesTheSeal(t *testing.T) {
	for _, compact := range []bool{true, false} {
		t.Run(compactionLabel(compact), func(t *testing.T) {
			h := newFlushFailureHarness(t, compact)
			h.seed(8)
			a := newFlushFailureRelay("relay-a", 7)

			// LINK 1
			h.failFlush(a)
			require.False(t, h.inRedis(a), "LINK 1: A's leaf must not have reached Redis")
			require.True(t, h.inMemory(a), "LINK 1: A's value must still be reachable without Redis")

			// Other relays land. Their Commit leaves A's leaf alone (it is
			// marked persisted) and their compaction pass runs.
			for i := 0; i < 5; i++ {
				b := newFlushFailureRelay(fmt.Sprintf("relay-b-%d", i), uint64(i+10))
				require.NoError(t, b.update(h.ctx, h.mgr, h.sessionID))
			}
			h.commit()
			t.Logf("LINK 2': after other relays inRedis(A)=%v", h.inRedis(a))
			require.True(t, h.inRedis(a), "LINK 2': A's leaf travels with the next successful flush")

			resident, _, err := h.mgr.CheckpointLiveRoot(h.ctx, h.sessionID)
			require.NoError(t, err)
			require.True(t, resident)
			t.Logf("LINK 3': after checkpoint inRedis(A)=%v", h.inRedis(a))

			// LINK 4: the value is either still resident or, if compaction
			// dropped it, in neither memory nor Redis.
			inMem := h.inMemory(a)
			inRds := h.inRedis(a)
			t.Logf("LINK 4': inMemory(A)=%v inRedis(A)=%v -> value exists nowhere: %v",
				inMem, inRds, !inMem && !inRds)

			root, err := h.mgr.FlushTree(h.ctx, h.sessionID)
			require.NoError(t, err)

			redelivery := a.update(h.ctx, h.mgr, h.sessionID)
			t.Logf("sealed; redelivery of A: %v", redelivery)
			require.True(t, IsPermanentSMSTError(redelivery),
				"the redelivery must be rejected as permanent once sealed, so the worker acks and drops it")

			// LINK 5
			err = h.proveAndVerify(a, root)
			t.Logf("LINK 5': proof of A: %v", err)
			require.NoError(t, err,
				"LINK 5': A is in the sealed root, so its proof must verify -- a failure here is a claim that cannot be proven")
		})
	}
}

// S3: as S2, but the redelivery lands before the seal. Says whether the
// redelivery closes the window S2 leaves open.
func TestFlushFailure_OtherRelaysLandFirstThenRedeliveryBeforeSeal(t *testing.T) {
	for _, compact := range []bool{true, false} {
		t.Run(compactionLabel(compact), func(t *testing.T) {
			h := newFlushFailureHarness(t, compact)
			h.seed(8)
			a := newFlushFailureRelay("relay-a", 7)

			h.failFlush(a)
			for i := 0; i < 5; i++ {
				b := newFlushFailureRelay(fmt.Sprintf("relay-b-%d", i), uint64(i+10))
				require.NoError(t, b.update(h.ctx, h.mgr, h.sessionID))
			}
			h.commit()
			t.Logf("before redelivery: inMemory(A)=%v inRedis(A)=%v", h.inMemory(a), h.inRedis(a))

			require.NoError(t, a.update(h.ctx, h.mgr, h.sessionID), "the redelivered A must be accepted")
			h.commit()
			t.Logf("after redelivery: inRedis(A)=%v orphanPending(A)=%v", h.inRedis(a), h.orphanPending(a))
			require.True(t, h.inRedis(a), "the redelivery's flush must write A's leaf")

			resident, _, err := h.mgr.CheckpointLiveRoot(h.ctx, h.sessionID)
			require.NoError(t, err)
			require.True(t, resident)
			t.Logf("after checkpoint: inRedis(A)=%v", h.inRedis(a))
			require.True(t, h.inRedis(a), "the checkpoint must not remove A's rewritten leaf")

			root, err := h.mgr.FlushTree(h.ctx, h.sessionID)
			require.NoError(t, err)
			err = h.proveAndVerify(a, root)
			t.Logf("proof of A: %v", err)
			require.NoError(t, err, "A's proof must verify once the redelivery has rewritten its leaf")
		})
	}
}
