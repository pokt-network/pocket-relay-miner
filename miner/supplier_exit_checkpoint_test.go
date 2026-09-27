//go:build test

package miner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pokt-network/smt"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Relays finished one at a time are acknowledged as they are processed, and
// nothing writes a live_root for them until the supplier exits. A supplier torn
// down with its trees uncovered left the next owner resuming from a live_root
// that missed the relays since -- and nothing delivers an acknowledged entry again. The
// teardown now writes every tree's covering live_root before the lease goes.

const exitCheckpointFailedMetric = "ha_miner_smst_exit_checkpoint_failed_total"

// liveRootLeaves is how many relays the stored live_root covers: the count is
// part of the root. Zero when there is none.
func liveRootLeaves(ctx context.Context, client *redisutil.Client, supplier, sessionID string) (uint64, error) {
	root, err := client.Get(ctx, client.KB().SMSTLiveRootKey(supplier, sessionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return smt.MerkleSumRoot(root).Count()
}

func requireLiveRootLeaves(t *testing.T, client *redisutil.Client, supplier, sessionID string) uint64 {
	t.Helper()
	n, err := liveRootLeaves(context.Background(), client, supplier, sessionID)
	require.NoError(t, err)
	return n
}

// relaysOneByOne processes n relays of the session one at a time, before the
// supplier is let go.
func (f *drainLeaseFixture) relaysOneByOne(t *testing.T, sessionID string, n int) {
	t.Helper()
	w := f.w
	w.state.relayBatch = nil // one at a time: acknowledged as processed
	w.deliverAll(sessionID, w.publish(n))
	require.Zero(t, w.pending(), "premise: every relay is acknowledged: nothing will deliver it again")
	require.Zero(t, requireLiveRootLeaves(t, w.client, w.supplier, sessionID),
		"premise: relays finished one at a time write no live_root; only the exit checkpoint does")
}

// leavesWhenTheLeaseGoes records how many relays live_root covers right before
// the lease-delete script runs; -1 until it does.
func (f *drainLeaseFixture) leavesWhenTheLeaseGoes(sessionID string) *atomic.Int64 {
	var leaves atomic.Int64
	leaves.Store(-1)
	f.w.client.AddHook(&beforeLeaseDelete{key: f.claimKey, action: func() {
		if n, err := liveRootLeaves(f.w.ctx, f.w.client, f.w.supplier, sessionID); err == nil {
			leaves.Store(int64(n))
		}
	}})
	return &leaves
}

// TestExitCheckpointAll_ARebalanceDrainCoversEveryAcknowledgedRelay: three
// relays acknowledged one at a time. Without the exit checkpoint the next owner
// resumes one of them.
func TestExitCheckpointAll_ARebalanceDrainCoversEveryAcknowledgedRelay(t *testing.T) {
	const supplier, sessionID = "pokt1exitall_release", "sess-exitall-release"
	f := newDrainLeaseFixture(t, supplier)
	f.relaysOneByOne(t, sessionID, 3)
	atDelete := f.leavesWhenTheLeaseGoes(sessionID)

	require.NoError(t, f.claimer.Release(f.w.ctx, supplier, triggerRebalanceRelease))
	close(f.hold)
	f.w.mgr.waitDrains()

	require.Equal(t, int64(3), atDelete.Load(),
		"live_root must cover every acknowledged relay before the lease goes")
	require.Empty(t, f.owner(t), "the drain is over: the lease goes")

	body := scrapeMinerRegistry(t)
	require.Equal(t, 1, countSampleSeries(body, exitCheckpointFailedMetric, supplier),
		"a torn-down supplier exports the failure series even at zero")
	require.Zero(t, testutil.ToFloat64(smstExitCheckpointFailedTotal.WithLabelValues(supplier)))

	require.Equal(t, uint64(3), leavesAfterRestart(t, f.w.client, supplier, sessionID),
		"the next owner must resume a tree holding every relay this one acknowledged")
}

// TestExitCheckpointAll_ShutdownWritesWithTheManagerContextCancelled: Close
// cancels the manager's context before it collects a single supplier, so a
// checkpoint on that context would fail every time, silently.
func TestExitCheckpointAll_ShutdownWritesWithTheManagerContextCancelled(t *testing.T) {
	const supplier, sessionID = "pokt1exitall_close", "sess-exitall-close"
	f := newDrainLeaseFixture(t, supplier)
	mgrCtx, cancelMgr := context.WithCancel(context.Background())
	f.w.mgr.ctx, f.w.mgr.cancelFn = mgrCtx, cancelMgr
	f.relaysOneByOne(t, sessionID, 3)
	atDelete := f.leavesWhenTheLeaseGoes(sessionID)

	closed := make(chan error, 1)
	go func() { closed <- f.w.mgr.Close() }()
	<-f.loopCtx.Done()
	require.Error(t, mgrCtx.Err(), "premise: Close has cancelled the manager's context")
	close(f.hold)
	require.NoError(t, <-closed)

	require.Equal(t, int64(3), atDelete.Load(),
		"the shutdown must write the covering live_root before it deletes the lease")
	require.Equal(t, uint64(3), leavesAfterRestart(t, f.w.client, supplier, sessionID),
		"the next owner must resume a tree holding every relay this one acknowledged")
}

// newOneByOneWorker is a batchWorker whose relays are finished one at a time.
func newOneByOneWorker(t *testing.T, supplier string) *batchWorker {
	t.Helper()
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, supplier, "a")
	w.state.relayBatch = nil
	return w
}

func (w *batchWorker) deliverAll(sessionID string, ids []string) {
	w.t.Helper()
	for _, id := range ids {
		require.True(w.t, w.deliver(w.msg(id, sessionID, sessionID+"-"+id, 100)))
	}
}

// TestCheckpointAllOnExit_DoesNotOverwriteTheNewOwnersLiveRoot: a drain that
// outran its lease budget leaves after the next owner has written its own.
func TestCheckpointAllOnExit_DoesNotOverwriteTheNewOwnersLiveRoot(t *testing.T) {
	const supplier, sessionID = "pokt1exitall_cas", "sess-exitall-cas"
	w := newOneByOneWorker(t, supplier)
	w.deliverAll(sessionID, w.publish(3))

	next := NewRedisSMSTManager(zerolog.Nop(), w.client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	require.NoError(t, next.UpdateTree(w.ctx, sessionID, []byte("new-owner-key"), []byte("v"), 100))
	_, _, cpErr := next.CheckpointLiveRoot(w.ctx, sessionID)
	require.NoError(t, cpErr)
	liveRootKey := w.client.KB().SMSTLiveRootKey(supplier, sessionID)
	newOwners, err := w.client.Get(w.ctx, liveRootKey).Bytes()
	require.NoError(t, err)

	written, failed, err := w.smst.CheckpointAllOnExit(w.ctx)
	require.NoError(t, err)
	require.Zero(t, failed)
	require.Zero(t, written, "a live_root this manager did not write is not its to replace")

	got, err := w.client.Get(w.ctx, liveRootKey).Bytes()
	require.NoError(t, err)
	require.Equal(t, newOwners, got, "the leaving miner overwrote the new owner's live_root")
}

// TestCheckpointAllOnExit_DeletesNoNodeTheNextOwnerStillWalks: the next owner
// imported the 10-relay live_root before this one leaves. The per-relay
// checkpoint deletes the nodes the tree orphaned since -- nodes that tree
// still points at.
func TestCheckpointAllOnExit_DeletesNoNodeTheNextOwnerStillWalks(t *testing.T) {
	const supplier, sessionID = "pokt1exitall_orphans", "sess-exitall-orphans"
	w := newOneByOneWorker(t, supplier)
	ids := w.publish(12)
	var hashes [][]byte
	for i := 0; i < 10; i++ {
		m := w.msg(ids[i], sessionID, fmt.Sprintf("orphan-%d", i), 100)
		hashes = append(hashes, append([]byte(nil), m.Message.RelayHash...))
		require.True(t, w.deliver(m))
	}
	_, _, cpErr := w.smst.CheckpointLiveRoot(w.ctx, sessionID) // the 10-relay live_root
	require.NoError(t, cpErr)

	next := NewRedisSMSTManager(zerolog.Nop(), w.client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	nextTree, err := next.GetOrCreateTree(w.ctx, sessionID) // resumes from the 10-relay live_root
	require.NoError(t, err)
	require.NotNil(t, nextTree.liveRoot, "premise: the next owner resumed from the 10-relay live_root")

	w.deliverAll(sessionID, ids[10:])
	written, _, err := w.smst.CheckpointAllOnExit(w.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, written, "premise: the exit wrote over the 10-relay live_root")

	_, err = next.FlushTree(w.ctx, sessionID)
	require.NoError(t, err)
	for i, h := range hashes {
		_, err := next.ProveClosest(w.ctx, sessionID, h)
		require.NoErrorf(t, err, "relay %d: the next owner's tree lost a node the exit deleted", i)
	}
}

// TestCheckpointAllOnExit_LeavesAloneWhatItHasNoReasonToWrite: a tree being
// sealed or already claimed belongs to its claim, and one its live_root
// already covers needs nothing. The last case is the control that the same
// setup does write.
func TestCheckpointAllOnExit_LeavesAloneWhatItHasNoReasonToWrite(t *testing.T) {
	for _, tc := range []struct {
		name        string
		relays      int
		prepare     func(t *testing.T, w *batchWorker, sessionID string) *RedisSMSTManager
		wantWritten int
		wantLeaves  uint64 // what live_root covers afterwards
	}{
		{
			name:   "a tree being sealed",
			relays: 3,
			prepare: func(t *testing.T, w *batchWorker, sessionID string) *RedisSMSTManager {
				tree := w.smst.trees[sessionID]
				tree.mu.Lock()
				tree.sealing = true // what FlushTree sets before it waits out in-flight updates
				tree.mu.Unlock()
				return w.smst
			},
			wantLeaves: 0, // relays finished one at a time write no live_root
		},
		{
			name:   "a tree resumed from its claim",
			relays: 3,
			prepare: func(t *testing.T, w *batchWorker, sessionID string) *RedisSMSTManager {
				_, err := w.smst.FlushTree(w.ctx, sessionID)
				require.NoError(t, err)
				require.NoError(t, w.client.Del(w.ctx, w.client.KB().SMSTLiveRootKey(w.supplier, sessionID)).Err())
				resumed := NewRedisSMSTManager(zerolog.Nop(), w.client, RedisSMSTManagerConfig{SupplierAddress: w.supplier})
				_, err = resumed.GetOrCreateTree(w.ctx, sessionID)
				require.NoError(t, err)
				tree := resumed.trees[sessionID]
				require.NotNil(t, tree.claimedRoot, "premise: resumed from claimed_root")
				require.False(t, tree.sealing, "premise: only claimedRoot marks it")
				return resumed
			},
			wantLeaves: 0,
		},
		{
			name:   "a tree its live_root already covers",
			relays: 1,
			prepare: func(t *testing.T, w *batchWorker, sessionID string) *RedisSMSTManager {
				resident, _, err := w.smst.CheckpointLiveRoot(w.ctx, sessionID)
				require.NoError(t, err)
				require.True(t, resident)
				return w.smst
			},
			wantLeaves: 1,
		},
		{
			name:        "control: a tree with relays past its live_root",
			relays:      3,
			prepare:     func(_ *testing.T, w *batchWorker, _ string) *RedisSMSTManager { return w.smst },
			wantWritten: 1,
			wantLeaves:  3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const supplier, sessionID = "pokt1exitall_skip", "sess-exitall-skip"
			w := newOneByOneWorker(t, supplier)
			w.deliverAll(sessionID, w.publish(tc.relays))
			mgr := tc.prepare(t, w, sessionID)

			written, failed, err := mgr.CheckpointAllOnExit(w.ctx)
			require.NoError(t, err)
			require.Zero(t, failed)
			require.Equal(t, tc.wantWritten, written)
			require.Equal(t, tc.wantLeaves, requireLiveRootLeaves(t, w.client, supplier, sessionID))
		})
	}
}

// TestCheckpointAllOnExit_TriesEveryTreeAndCountsEachFailure: two trees whose
// live_root cannot be written and one that can. Stopping at the first failure
// would leave the rest uncovered, and count one.
func TestCheckpointAllOnExit_TriesEveryTreeAndCountsEachFailure(t *testing.T) {
	const supplier = "pokt1exitall_fail"
	sessions := []string{"sess-exitall-good", "sess-exitall-bad1", "sess-exitall-bad2"}
	w := newOneByOneWorker(t, supplier)
	ids := w.publish(9)
	for i, id := range ids {
		s := sessions[i%len(sessions)]
		require.True(t, w.deliver(w.msg(id, s, fmt.Sprintf("%s-%d", s, i), 100)))
	}
	for _, s := range sessions[1:] {
		key := w.client.KB().SMSTLiveRootKey(supplier, s)
		require.NoError(t, w.client.Del(w.ctx, key).Err())
		require.NoError(t, w.client.HSet(w.ctx, key, "not", "a string").Err()) // the script's GET fails
	}

	written, failed, err := w.smst.CheckpointAllOnExit(w.ctx)
	require.Equal(t, 2, failed, "every tree is tried: two of them cannot be written")
	require.Error(t, err)
	require.Contains(t, err.Error(), sessions[1], "each error names its session")
	require.Contains(t, err.Error(), sessions[2], "each error names its session")
	require.Equal(t, 1, written)
	require.Equal(t, uint64(3), requireLiveRootLeaves(t, w.client, supplier, sessions[0]),
		"the tree that can be written is, whichever order the trees come in")

	counter := smstExitCheckpointFailedTotal.WithLabelValues(supplier)
	before := testutil.ToFloat64(counter)
	w.mgr.checkpointTreesOnExit(w.state)
	require.Equal(t, 2.0, testutil.ToFloat64(counter)-before, "each tree that could not be written is counted")
	var line string
	for _, l := range strings.Split(w.logs.String(), "\n") {
		if strings.Contains(l, "could not checkpoint every tree on exit") {
			line = l
		}
	}
	require.Contains(t, line, `"level":"warn"`, "an operator must see it without debug logging")
	require.Contains(t, line, sessions[1])
	require.Contains(t, line, sessions[2])
	require.Contains(t, line, `"failed":2`)
}

// TestCheckpointTreesOnExit_WithoutAnSMSTManager: a state built with no SMST
// manager is torn down like any other.
func TestCheckpointTreesOnExit_WithoutAnSMSTManager(t *testing.T) {
	mgr := &SupplierManager{logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		mgr.checkpointTreesOnExit(&SupplierState{OperatorAddr: "pokt1exitall_nil"})
	})
}
