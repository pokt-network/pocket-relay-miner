//go:build test

package miner

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func inspectedIDs(sessions []map[string]string) []string {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s["session_id"])
	}
	sort.Strings(ids)
	return ids
}

// Sessions are listed per supplier and per state, past-TTL ones left out, and
// listing deletes nothing: GetBySupplier deletes the expired sessions it walks
// past, an inspection must not change what it looks at.
func TestPebbleInspectSessions_ListsBySupplierAndStateAndDeletesNothing(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1inspecta")
	other := newPebbleCommitHarnessOn(t, h, "pokt1inspectb")
	h.createSession("s-active", SessionStateActive)
	h.createSession("s-failed", SessionStateClaimTxError)
	other.createSession("s-other", SessionStateClaimTxError)

	// A session nothing has written to for longer than its TTL.
	stale := &SessionSnapshot{SessionID: "s-stale", SupplierOperatorAddress: h.supplier,
		State: SessionStateClaimTxError, LastUpdatedAt: time.Now().Add(-sessionTTL(0) - time.Hour)}
	data, err := json.Marshal(stale)
	require.NoError(t, err)
	require.NoError(t, h.store.DB().Set([]byte(pebbleSessionPrefix+h.supplier+"\x00s-stale"), data, nil))
	onDisk := h.count([]byte(pebbleSessionPrefix + h.supplier + "\x00"))
	require.Equal(t, int64(3), onDisk, "premise: the stale session is on disk")

	all, err := h.backend.InspectSessions(h.supplier, "")
	require.NoError(t, err)
	require.Equal(t, []string{"s-active", "s-failed"}, inspectedIDs(all))

	failed, err := h.backend.InspectSessions(h.supplier, SessionStateClaimTxError)
	require.NoError(t, err)
	require.Equal(t, []string{"s-failed"}, inspectedIDs(failed))
	require.Equal(t, string(SessionStateClaimTxError), failed[0]["state"])
	require.Equal(t, SnapshotFields(h.snapshot("s-failed")), failed[0], "every field, as the Redis hash holds them")

	none, err := h.backend.InspectSessions("pokt1nobody", "")
	require.NoError(t, err)
	require.NotNil(t, none, "an empty list, not null: a consumer reads an array either way")
	require.Empty(t, none)

	require.Equal(t, onDisk, h.count([]byte(pebbleSessionPrefix+h.supplier+"\x00")), "inspecting deleted a session")
}

// A session prints with the fields, and the values, its Redis hash holds, so
// a consumer of `redis sessions --json` reads standalone the same way.
func TestSnapshotFields_AreTheRedisSessionHash(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewRedisSessionStore(testLogger(), client, SessionStoreConfig{SupplierAddress: resumeSupplier})
	ctx := context.Background()
	snap := claimedSnapshot(SessionStateClaimed)
	snap.SessionID = "sess-fields"
	snap.RelayCount = 7
	snap.TotalComputeUnits = 70
	require.NoError(t, store.Save(ctx, snap))

	saved, err := store.Get(ctx, snap.SessionID)
	require.NoError(t, err)
	hash, err := client.HGetAll(ctx, client.KB().MinerSessionKey(resumeSupplier, snap.SessionID)).Result()
	require.NoError(t, err)
	require.NotEmpty(t, hash, "premise: the session is a hash in Redis")
	require.Equal(t, hash, SnapshotFields(saved))
}

// A session's dedup marks: how many, a sample of their hashes, and their TTL.
func TestPebbleInspectDedup_CountsSamplesAndReadsTheTTL(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1inspectdedup")
	h.createSession("s1", SessionStateActive)
	hashes := [][]byte{{0x01, 0xaa}, {0x02, 0xbb}, {0x03, 0xcc}}
	for _, hash := range hashes {
		h.markDone("s1", hash)
	}
	h.markDone("s2", []byte{0x09})

	view, err := h.backend.InspectDedup("s1", 2)
	require.NoError(t, err)
	require.Equal(t, "s1", view.SessionID)
	require.Equal(t, 3, view.Count)
	require.Equal(t, []string{hex.EncodeToString(hashes[0]), hex.EncodeToString(hashes[1])}, view.Sample)
	require.True(t, view.Live)
	require.Greater(t, view.ExpiresAtUnixMs, time.Now().UnixMilli())

	empty, err := h.backend.InspectDedup("s-none", 10)
	require.NoError(t, err)
	require.Equal(t, DedupView{SessionID: "s-none", Sample: []string{}}, empty)
}

// Each supplier's tree for a session: nodes, roots and stats; a supplier with
// no tree for it is left out.
func TestPebbleInspectSMST_ShowsEachSuppliersTree(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1smsta")
	ctx := context.Background()
	a := h.backend.smstStore("pokt1smsta").(*pebbleSMSTStore)
	b := h.backend.smstStore("pokt1smstb").(*pebbleSMSTStore)
	require.NoError(t, a.nodes(ctx, "s1").Set([]byte{1}, []byte{0, 1}))
	require.NoError(t, a.nodes(ctx, "s1").Set([]byte{2}, []byte{0, 2}))
	require.NoError(t, a.set(ctx, smstLiveRoot, "s1", []byte{0xab}, time.Hour))
	require.NoError(t, b.set(ctx, smstClaimedRoot, "s1", []byte{0xcd}, time.Hour))
	require.NoError(t, b.set(ctx, smstStats, "s1", []byte("5:50"), time.Hour))
	require.NoError(t, b.set(ctx, smstLiveRoot, "s2", []byte{0xef}, time.Hour))

	trees, err := h.backend.InspectSMST("", "s1")
	require.NoError(t, err)
	sort.Slice(trees, func(i, j int) bool { return trees[i].Supplier < trees[j].Supplier })
	require.Equal(t, []SMSTView{
		{Supplier: "pokt1smsta", SessionID: "s1", NodeCount: 2, LiveRoot: "ab"},
		{Supplier: "pokt1smstb", SessionID: "s1", ClaimedRoot: "cd", Stats: "5:50"},
	}, trees)

	one, err := h.backend.InspectSMST("pokt1smstb", "s1")
	require.NoError(t, err)
	require.Equal(t, []SMSTView{{Supplier: "pokt1smstb", SessionID: "s1", ClaimedRoot: "cd", Stats: "5:50"}}, one)

	none, err := h.backend.InspectSMST("", "s-none")
	require.NoError(t, err)
	require.NotNil(t, none)
	require.Empty(t, none)
}

// Inspecting takes no lock: it reads while the miner writes sessions, dedup
// marks and trees, and every answer is a consistent list. Run with -race.
func TestPebbleInspect_ReadsWhileTheMinerWrites(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1inspectrace")
	ctx := context.Background()
	smst := h.backend.smstStore(h.supplier)
	const writes = 100
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		for i := 0; i < writes; i++ {
			id := "s" + strconv.Itoa(i)
			if _, err := h.stores.sessions.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: id, SupplierOperatorAddress: h.supplier, State: SessionStateActive}); err != nil {
				writeErr <- err
				return
			}
			if err := h.stores.sessions.UpdateState(ctx, id, SessionStateClaimTxError); err != nil {
				writeErr <- err
				return
			}
			if _, err := h.backend.deduplicator().MarkProcessed(ctx, []byte{byte(i)}, id); err != nil {
				writeErr <- err
				return
			}
			if err := smst.set(ctx, smstLiveRoot, id, []byte{byte(i)}, time.Hour); err != nil {
				writeErr <- err
				return
			}
		}
	}()
	reads := 0
	for done := false; !done; reads++ {
		select {
		case err, open := <-writeErr:
			require.NoError(t, err)
			done = !open
		default:
		}
		sessions, err := h.backend.InspectSessions(h.supplier, "")
		require.NoError(t, err)
		for _, s := range sessions {
			require.NotEmpty(t, s["session_id"])
			require.Contains(t, []string{string(SessionStateActive), string(SessionStateClaimTxError)}, s["state"])
		}
		_, err = h.backend.InspectDedup("s0", 5)
		require.NoError(t, err)
		_, err = h.backend.InspectSMST("", "s0")
		require.NoError(t, err)
	}
	all, err := h.backend.InspectSessions(h.supplier, SessionStateClaimTxError)
	require.NoError(t, err)
	require.Len(t, all, writes, "control: every write landed while the reads ran")
	require.Greater(t, reads, 1)
}
