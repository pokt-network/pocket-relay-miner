//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
)

// The embedded session store applies the same state rules as the Redis one
// (checkStateWrite, canReactivateClaimed), atomically under the backend lock.

func newPebbleSessions(t *testing.T) (*pebbleCommitHarness, SessionStore) {
	t.Helper()
	h := newPebbleCommitHarness(t, "pokt1sessions")
	return h, h.stores.sessions
}

func TestPebbleSessionStore_CreateIfAbsentIsFirstWriteWins(t *testing.T) {
	_, s := newPebbleSessions(t)
	ctx := context.Background()

	first, err := s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "s1", State: SessionStateActive, RelayCount: 1})
	require.NoError(t, err)
	second, err := s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "s1", State: SessionStateActive, RelayCount: 99})
	require.NoError(t, err)

	require.True(t, first)
	require.False(t, second)
	snap, err := s.Get(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, int64(1), snap.RelayCount, "the second create wrote nothing")
	require.False(t, snap.CreatedAt.IsZero())
}

func TestPebbleSessionStore_UpdateStateKeepsTheRules(t *testing.T) {
	_, s := newPebbleSessions(t)
	ctx := context.Background()
	_, err := s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "s1", State: SessionStateClaimed})
	require.NoError(t, err)

	require.ErrorIs(t, s.UpdateState(ctx, "s1", SessionStateClaimWindowClosed), ErrClaimAlreadyOnChain)
	require.ErrorIs(t, s.UpdateState(ctx, "s1", SessionStateActive), ErrSessionNotDeferred, "active only over an unsent claim")
	require.ErrorContains(t, s.UpdateState(ctx, "missing", SessionStateClaimed), "session not found")

	require.NoError(t, s.UpdateState(ctx, "s1", SessionStateProving))
	snap, err := s.Get(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, SessionStateProving, snap.State)
}

func TestPebbleSessionStore_ReactivateClaimedOnlyFromAnUnclaimedState(t *testing.T) {
	_, s := newPebbleSessions(t)
	ctx := context.Background()
	_, err := s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "s1", State: SessionStateClaimTxError})
	require.NoError(t, err)
	_, err = s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "s2", State: SessionStateProved})
	require.NoError(t, err)

	flipped, err := s.ReactivateClaimed(ctx, "s1", []byte("root"), "tx1")
	require.NoError(t, err)
	require.True(t, flipped)
	snap, err := s.Get(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, SessionStateClaimed, snap.State)
	require.Equal(t, []byte("root"), snap.ClaimedRootHash)
	require.Equal(t, "tx1", snap.ClaimTxHash)

	flipped, err = s.ReactivateClaimed(ctx, "s2", []byte("root"), "tx2")
	require.NoError(t, err)
	require.False(t, flipped, "a proved session is past claimed: nothing written")
}

func TestPebbleSessionStore_IncrementRefusesATerminalSession(t *testing.T) {
	_, s := newPebbleSessions(t)
	ctx := context.Background()
	_, err := s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "live", State: SessionStateActive})
	require.NoError(t, err)
	_, err = s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "done", State: SessionStateProved})
	require.NoError(t, err)

	require.NoError(t, s.IncrementRelayCount(ctx, "live", 7))
	require.ErrorIs(t, s.IncrementRelayCount(ctx, "done", 7), ErrSessionTerminal)
	require.ErrorContains(t, s.IncrementRelayCount(ctx, "missing", 7), "session not found")
	snap, err := s.Get(ctx, "live")
	require.NoError(t, err)
	require.Equal(t, int64(1), snap.RelayCount)
	require.Equal(t, uint64(7), snap.TotalComputeUnits)
}

// A session nothing has written to for its TTL is gone, as its Redis key would
// be, and its dedup marks with it.
func TestPebbleSessionStore_AnExpiredSessionIsGoneWithItsMarks(t *testing.T) {
	h, s := newPebbleSessions(t)
	ctx := context.Background()
	_, err := s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "old", State: SessionStateActive})
	require.NoError(t, err)
	_, err = s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: "new", State: SessionStateActive})
	require.NoError(t, err)
	h.markDone("old", []byte("hash"))
	// Age "old" past its TTL by writing its snapshot directly.
	sessions := s.(*pebbleSessionStore)
	old, err := s.Get(ctx, "old")
	require.NoError(t, err)
	old.LastUpdatedAt = time.Now().Add(-sessions.ttl - time.Minute)
	h.backend.mu.Lock()
	require.NoError(t, sessions.writeLocked(old, old))
	h.backend.mu.Unlock()

	all, err := s.GetBySupplier(ctx)
	require.NoError(t, err)

	require.Len(t, all, 1)
	require.Equal(t, "new", all[0].SessionID)
	gone, err := s.Get(ctx, "old")
	require.NoError(t, err)
	require.Nil(t, gone)
	require.Zero(t, h.marked("old"), "its marks are deleted with it")
}

func TestPebbleSessionStore_GetByStateFilters(t *testing.T) {
	_, s := newPebbleSessions(t)
	ctx := context.Background()
	for id, state := range map[string]SessionState{"a": SessionStateActive, "b": SessionStateClaimed, "c": SessionStateActive} {
		_, err := s.CreateIfAbsent(ctx, &SessionSnapshot{SessionID: id, State: state})
		require.NoError(t, err)
	}

	active, err := s.GetByState(ctx, SessionStateActive)

	require.NoError(t, err)
	ids := []string{active[0].SessionID, active[1].SessionID}
	require.ElementsMatch(t, []string{"a", "c"}, ids)
	require.Len(t, active, 2)
}

func TestPebbleDeduplicator_MarksOnceAndForgetsAfterTheTTL(t *testing.T) {
	h, _ := newPebbleSessions(t)
	d := h.backend.deduplicator()
	ctx := context.Background()

	added, err := d.MarkProcessed(ctx, []byte("r1"), "s1")
	require.NoError(t, err)
	require.True(t, added)
	added, err = d.MarkProcessed(ctx, []byte("r1"), "s1")
	require.NoError(t, err)
	require.False(t, added, "the second mark of the same relay is not new")
	dup, err := d.IsDuplicate(ctx, []byte("r1"), "s1")
	require.NoError(t, err)
	require.True(t, dup)

	require.NoError(t, d.CleanupSession(ctx, "s1"))
	dup, err = d.IsDuplicate(ctx, []byte("r1"), "s1")
	require.NoError(t, err)
	require.False(t, dup, "cleaned up")

	// Past the TTL the marks no longer count, as the Redis set would be gone.
	_, err = d.MarkProcessed(ctx, []byte("r2"), "s2")
	require.NoError(t, err)
	b := h.store.DB().NewBatch()
	require.NoError(t, b.Set(dedupTTLKey("s2"), make([]byte, 8), nil)) // expiry at the epoch
	require.NoError(t, h.store.Commit(b))
	dup, err = d.IsDuplicate(ctx, []byte("r2"), "s2")
	require.NoError(t, err)
	require.False(t, dup)
}

// eachSessionStore runs body against both backends' session stores.
func eachSessionStore(t *testing.T, body func(t *testing.T, s SessionStore)) {
	t.Run("redis", func(t *testing.T) {
		client, _ := newTestRedis(t)
		s := NewRedisSessionStore(testLogger(), client, SessionStoreConfig{SupplierAddress: "pokt1save", SessionTTL: time.Hour})
		t.Cleanup(func() { _ = s.Close() })
		body(t, s)
	})
	t.Run("pebble", func(t *testing.T) {
		_, s := newPebbleSessions(t)
		body(t, s)
	})
}

// A Save from a snapshot read before relays were counted must not take the
// count back: the lifecycle saves snapshots it read under another lock.
func TestSessionStore_SaveKeepsTheCountedRelays(t *testing.T) {
	eachSessionStore(t, func(t *testing.T, s SessionStore) {
		ctx := context.Background()
		stale := &SessionSnapshot{SessionID: "s1", SupplierOperatorAddress: "pokt1save", State: SessionStateActive}
		_, err := s.CreateIfAbsent(ctx, stale)
		require.NoError(t, err)
		require.NoError(t, s.IncrementRelayCount(ctx, "s1", 7))
		require.NoError(t, s.IncrementRelayCount(ctx, "s1", 5))

		stale.State = SessionStateClaimed
		stale.ClaimTxHash = "ABC"
		require.NoError(t, s.Save(ctx, stale))

		snap, err := s.Get(ctx, "s1")
		require.NoError(t, err)
		require.Equal(t, int64(2), snap.RelayCount)
		require.Equal(t, uint64(12), snap.TotalComputeUnits)
		require.Equal(t, SessionStateClaimed, snap.State, "the metadata is written")
		require.Equal(t, "ABC", snap.ClaimTxHash)
	})
}

// A write that records a claim tx hash is fsynced before Save returns, so an
// OS crash right after the broadcast cannot forget the claim was sent.
func TestPebbleSessionStore_ATxHashSurvivesAnOSCrash(t *testing.T) {
	fs := vfs.NewStrictMem()
	require.NoError(t, fs.MkdirAll("db", 0o755))
	root, err := fs.OpenDir("")
	require.NoError(t, err)
	require.NoError(t, root.Sync())
	require.NoError(t, root.Close())
	open := func() (*pebblestore.Store, SessionStore) {
		store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: "db", FS: fs, SyncInterval: time.Hour})
		require.NoError(t, err)
		backend := NewPebbleStoreBackend(zerolog.Nop(), store, pebblequeue.NewBroker(zerolog.Nop(), store, nil, "ha:relays"), SupplierManagerConfig{BlockTimeSeconds: 30})
		return store, backend.sessionStore("pokt1sync")
	}
	ctx := context.Background()
	store, s := open()

	claimed := &SessionSnapshot{SessionID: "claimed", State: SessionStateClaimed, ClaimTxHash: "ABC"}
	require.NoError(t, s.Save(ctx, &SessionSnapshot{SessionID: "plain", State: SessionStateActive}))
	require.NoError(t, s.Save(ctx, claimed))
	require.NoError(t, s.Save(ctx, &SessionSnapshot{SessionID: "after", State: SessionStateActive}))

	fs.SetIgnoreSyncs(true)
	_ = store.Close()
	fs.ResetToSyncedState()
	fs.SetIgnoreSyncs(false)
	store, s = open()
	defer func() { _ = store.Close() }()

	snap, err := s.Get(ctx, "claimed")
	require.NoError(t, err)
	require.NotNil(t, snap, "the claim tx hash write was fsynced")
	require.Equal(t, "ABC", snap.ClaimTxHash)
	plain, err := s.Get(ctx, "plain")
	require.NoError(t, err)
	require.NotNil(t, plain, "an earlier write is kept with it: the log is a prefix")
	after, err := s.Get(ctx, "after")
	require.NoError(t, err)
	require.Nil(t, after, "control: a write with no tx hash is not fsynced, so the crash took it")
}

// What no supplier's scan reaches is swept by any scan: an expired session of
// a supplier no longer served, and the expired marks of a session that has no
// snapshot. Live state is kept.
func TestPebbleBackend_AScanSweepsWhatNoSupplierScanReaches(t *testing.T) {
	h, s := newPebbleSessions(t)
	ctx := context.Background()
	gone := h.backend.sessionStore("pokt1removed").(*pebbleSessionStore)
	old := time.Now().Add(-gone.ttl - time.Minute)
	h.backend.mu.Lock()
	expired := &SessionSnapshot{SessionID: "stale", State: SessionStateActive, LastUpdatedAt: old}
	require.NoError(t, gone.writeLocked(expired, expired))
	batch := h.store.DB().NewBatch()
	h.backend.dedup.markLocked(batch, "stale", []byte("h1"), old)
	h.backend.dedup.markLocked(batch, "orphan", []byte("h2"), old.Add(-h.backend.dedupCf.ttl()))
	h.backend.dedup.markLocked(batch, "live", []byte("h3"), time.Now())
	require.NoError(t, h.store.Commit(batch))
	h.backend.mu.Unlock()
	require.Equal(t, int64(1), h.marked("orphan"), "premise: the orphan's mark is stored")

	_, err := s.GetBySupplier(ctx)
	require.NoError(t, err)

	require.Zero(t, h.count([]byte(pebbleSessionPrefix+"pokt1removed\x00")), "the removed supplier's expired session")
	require.Zero(t, h.marked("stale"))
	require.Zero(t, h.marked("orphan"), "marks with no snapshot, past their TTL")
	require.Equal(t, int64(1), h.marked("live"), "live marks are kept")
}
