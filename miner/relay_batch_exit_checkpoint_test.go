//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The L3 of df5441c (2026-09-11) lost a relay this way: the supplier's exit
// released its batch, the next owner resumed the tree from a live_root written
// before that relay went in, and the relay's entry came back only after the
// session was sealed. The exit now writes the covering live_root first.

// exitAndRelease runs the supplier's exit as the consume loop does it: with the
// supplier's context already cancelled.
func (w *batchWorker) exitAndRelease() {
	ctx, cancel := context.WithCancel(w.ctx)
	cancel()
	w.mgr.releaseRelayBatchOnExit(ctx, w.state)
}

// TestExitCheckpoint_TheNextOwnerResumesEveryBatchedRelay: three relays, the
// first of which is the last one the per-relay checkpoint covered.
func TestExitCheckpoint_TheNextOwnerResumesEveryBatchedRelay(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID, n = "pokt1exitcp_cover", "sess-exit-cover", 3
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(n)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("cover-%d", i), 100))
	}
	w.exitAndRelease()

	// Checked FIRST, before anything else that could also fail: this fixture
	// never starts a live consume loop -- no reclaimLoop, no deliverOwnPending
	// racing to reclaim them -- so it is the one place ownership can be
	// checked without that race (item 263), and the test that pins
	// releaseRelayBatchOnExit -> ReleaseAll -> ReleaseMessage actually
	// reaching a real XNACK against Redis, not merely clearing the in-memory
	// batch or returning nil early. A later assertion failing first would
	// blame the wrong thing.
	for i, id := range ids {
		require.Emptyf(t, w.ownerOf(t, id), "relay %d (%s): released must mean unowned in Redis, not just cleared from the in-memory batch", i, id)
	}

	require.Equal(t, uint64(n), leavesAfterRestart(t, client, supplier, sessionID),
		"the next owner must resume a tree holding every relay this one inserted before it left")
	require.Equal(t, int64(n), w.pending(), "and the entries are released, not acknowledged (the count alone does not prove ownership, see above)")
}

// TestExitCheckpoint_DeletesNoNodeTheNextOwnerStillWalks: the new owner has
// imported the old live_root before this one leaves. CheckpointLiveRoot would
// delete the nodes this tree orphaned since that root -- nodes the new owner's
// tree still points at.
func TestExitCheckpoint_DeletesNoNodeTheNextOwnerStillWalks(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1exitcp_orphans", "sess-exit-orphans"
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(12)
	var hashes [][]byte
	for i := 0; i < 10; i++ { // the 10th update writes live_root
		m := w.msg(ids[i], sessionID, fmt.Sprintf("orphan-%d", i), 100)
		hashes = append(hashes, append([]byte(nil), m.Message.RelayHash...))
		w.deliver(m)
	}

	next := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	_, err := next.GetOrCreateTree(w.ctx, sessionID) // resumes from the 10-leaf live_root
	require.NoError(t, err)

	for i := 10; i < 12; i++ {
		w.deliver(w.msg(ids[i], sessionID, fmt.Sprintf("orphan-%d", i), 100))
	}
	w.exitAndRelease()

	_, err = next.FlushTree(w.ctx, sessionID)
	require.NoError(t, err)
	for i, h := range hashes {
		_, err := next.ProveClosest(w.ctx, sessionID, h)
		require.NoErrorf(t, err, "relay %d: the next owner's tree lost a node the exit deleted", i)
	}
}

// TestExitCheckpoint_DoesNotOverwriteTheNewOwnersLiveRoot: once the new owner
// has written its own live_root, the leaving one must not roll it back.
func TestExitCheckpoint_DoesNotOverwriteTheNewOwnersLiveRoot(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1exitcp_cas", "sess-exit-cas"
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(3)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("cas-%d", i), 100))
	}

	next := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	require.NoError(t, next.UpdateTree(w.ctx, sessionID, []byte("new-owner-key"), []byte("v"), 100))
	liveRootKey := client.KB().SMSTLiveRootKey(supplier, sessionID)
	newOwners, err := client.Get(w.ctx, liveRootKey).Bytes()
	require.NoError(t, err)

	w.exitAndRelease()

	got, err := client.Get(w.ctx, liveRootKey).Bytes()
	require.NoError(t, err)
	require.Equal(t, newOwners, got, "the leaving miner overwrote the new owner's live_root")
}
