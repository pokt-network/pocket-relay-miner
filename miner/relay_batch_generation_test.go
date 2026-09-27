//go:build test

package miner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A tree evicted after corruption is replaced, at the next relay, by one
// resumed from the session's live_root -- which covers the relays up to its
// last checkpoint, not the ones the batch still held. The flush checkpointed
// the NEW tree and acknowledged the batch, and those relays were gone: in no
// tree, and never delivered again.

// TestRelayBatch_ARelayOfAnEvictedTreeIsHandedBackNotAcknowledged: k1..k3 in
// the batch, the tree evicted, k4 into its replacement, then the flush.
func TestRelayBatch_ARelayOfAnEvictedTreeIsHandedBackNotAcknowledged(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_gen_evicted", "sess-batch-gen-evicted"
	w := newBatchWorker(t, client, supplier, "a")
	ids := w.publish(4)
	payload := func(i int) string { return fmt.Sprintf("gen-%d", i) }
	for i := 0; i < 3; i++ {
		w.deliver(w.msg(ids[i], sessionID, payload(i), 100))
	}
	require.Equal(t, 3, w.held(sessionID), "premise: k1..k3 wait in the batch")
	w.smst.evictCorruptSession(w.ctx, sessionID, "test")
	w.deliver(w.msg(ids[3], sessionID, payload(3), 100)) // into a tree resumed from live_root
	released := relayBatchReleasedTotal.WithLabelValues(supplier, relayBatchReleasedTreeReplaced)
	before := testutil.ToFloat64(released)

	w.batch.FlushAll(w.ctx)

	require.Equal(t, int64(3), w.streamLen(), "k1..k3 are handed back, not acknowledged: still in the stream")
	require.Equal(t, int64(3), w.pending(), "and still pending, for a redelivery")
	underA, err := client.XPendingExt(w.ctx, &redis.XPendingExtArgs{
		Stream: w.stream, Group: w.group, Start: "-", End: "+", Count: 10, Consumer: "a",
	}).Result()
	require.NoError(t, err)
	require.Empty(t, underA, "released, not just dropped from the batch: nothing stays under the consumer's name")
	require.Equal(t, before+3, testutil.ToFloat64(released), "counted as handed back for a replaced tree")

	// The next owner takes them and finishes them: its tree holds all four.
	next := newBatchWorker(t, client, supplier, "b")
	claimed, _, err := client.XAutoClaimJustID(w.ctx, &redis.XAutoClaimArgs{
		Stream: w.stream, Group: w.group, Consumer: "b", MinIdle: 0, Start: "0", Count: 10,
	}).Result()
	require.NoError(t, err)
	require.ElementsMatch(t, ids[:3], claimed)
	for i, id := range ids[:3] {
		m := next.msg(id, sessionID, payload(i), 100)
		m.IsReclaim = true
		next.deliver(m)
	}
	next.batch.FlushAll(next.ctx)
	require.Zero(t, next.pending(), "the next owner settled them")
	require.Equal(t, uint64(4), leavesAfterRestart(t, client, supplier, sessionID),
		"k2 and k3 must not be lost with the tree that held them")
}

// TestRelayBatch_WithNoEvictionTheSameFlushAcknowledgesEveryRelay is the
// control: one tree, one generation, nothing handed back.
func TestRelayBatch_WithNoEvictionTheSameFlushAcknowledgesEveryRelay(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_gen_control", "sess-batch-gen-control"
	w := newBatchWorker(t, client, supplier, "a")
	ids := w.publish(4)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("gen-%d", i), 100))
	}
	released := relayBatchReleasedTotal.WithLabelValues(supplier, relayBatchReleasedTreeReplaced)
	before := testutil.ToFloat64(released)

	w.batch.FlushAll(w.ctx)

	require.Zero(t, w.streamLen(), "one tree: every relay is acknowledged")
	require.Equal(t, before, testutil.ToFloat64(released))
	require.Equal(t, uint64(4), leavesAfterRestart(t, client, supplier, sessionID))
}

// TestRelayBatch_ANonResidentTreeHandsTheBatchBackAndCountsIt: evicted, and no
// relay since, so nothing replaced it.
func TestRelayBatch_ANonResidentTreeHandsTheBatchBackAndCountsIt(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_gen_gone", "sess-batch-gen-gone"
	w := newBatchWorker(t, client, supplier, "a")
	ids := w.publish(2)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("gone-%d", i), 100))
	}
	w.smst.evictCorruptSession(w.ctx, sessionID, "test")
	released := relayBatchReleasedTotal.WithLabelValues(supplier, relayBatchReleasedNotResident)
	before := testutil.ToFloat64(released)

	w.batch.FlushAll(w.ctx)

	require.Equal(t, int64(2), w.streamLen(), "handed back")
	require.Equal(t, before+2, testutil.ToFloat64(released), "and counted")
}

// TestUpdateTreeGen_TheGenerationIsTheTreesAndChangesWithIt: UpdateTreeGen and
// CheckpointLiveRoot report the same number for one tree, and a tree that
// replaces an evicted one gets another.
func TestUpdateTreeGen_TheGenerationIsTheTreesAndChangesWithIt(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1tree_gen", "sess-tree-gen"
	m := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	ctx := t.Context()

	g1, err := m.UpdateTreeGen(ctx, sessionID, []byte("k1"), []byte("v"), 1)
	require.NoError(t, err)
	g1b, err := m.UpdateTreeGen(ctx, sessionID, []byte("k2"), []byte("v"), 1)
	require.NoError(t, err)
	require.Equal(t, g1, g1b, "one tree, one generation")
	resident, gc, err := m.CheckpointLiveRoot(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, resident)
	require.Equal(t, g1, gc, "the checkpoint reports the generation of the tree it wrote")

	m.evictCorruptSession(ctx, sessionID, "test")
	g2, err := m.UpdateTreeGen(ctx, sessionID, []byte("k3"), []byte("v"), 1)
	require.NoError(t, err)
	require.NotEqual(t, g1, g2, "the tree that replaces an evicted one is another generation")
}

// TestSMSTManager_EveryResidentTreeGetsAGeneration reads smst_manager.go: every
// assignment into m.trees must be addTreeLocked's, or a tree becomes resident
// without a generation of its own and a replaced tree can share a number with
// the one it replaced.
func TestSMSTManager_EveryResidentTreeGetsAGeneration(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "smst_manager.go", nil, 0)
	require.NoError(t, err)
	var inAdd, elsewhere int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok {
					continue
				}
				sel, ok := idx.X.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "trees" {
					continue
				}
				if fn.Name.Name == "addTreeLocked" {
					inAdd++
				} else {
					elsewhere++
					t.Errorf("%s assigns into m.trees directly; use addTreeLocked", fn.Name.Name)
				}
			}
			return true
		})
	}
	require.Equal(t, 1, inAdd, "control: the walk must find addTreeLocked's own assignment")
	require.Zero(t, elsewhere)
}
