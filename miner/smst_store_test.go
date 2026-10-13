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

// eachSMSTStore runs body against both backends' session-tree stores.
func eachSMSTStore(t *testing.T, supplier string, body func(t *testing.T, store smstStore)) {
	t.Run("redis", func(t *testing.T) {
		client, _ := newTestRedis(t)
		body(t, newRedisSMSTStore(client, supplier))
	})
	t.Run("pebble", func(t *testing.T) {
		h := newPebbleCommitHarness(t, supplier)
		body(t, h.backend.smstStore(supplier))
	})
}

func smstManagerOver(store smstStore, supplier string) *RedisSMSTManager {
	return newSMSTManager(zerolog.Nop(), store, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
}

// A tree built, claimed, proved by another manager over the same store,
// compacted to its leaves and proved again: every proof verifies the way the
// chain verifies it, on either backend.
func TestSMSTStore_ATreeIsClaimedProvedAndCompactedOnBothBackends(t *testing.T) {
	const supplier, sessionID = "pokt1smststore", "sess-store"
	eachSMSTStore(t, supplier, func(t *testing.T, store smstStore) {
		ctx := context.Background()
		root := claimColdTree(t, ctx, smstManagerOver(store, supplier), sessionID, coldRelays(21, 200))
		path := coldPaths(22, 1)[0]

		failover := smstManagerOver(store, supplier)
		proofBz, err := failover.ProveClosest(ctx, sessionID, path)
		require.NoError(t, err)
		ok, _ := chainVerifies(t, proofBz, root)
		require.True(t, ok, "a proof from the stored nodes")

		result, err := failover.CompactColdTree(ctx, sessionID)
		require.NoError(t, err)
		require.Equal(t, coldCompacted, result)
		compacted, err := store.compacted(ctx, sessionID)
		require.NoError(t, err)
		require.True(t, compacted)

		proofBz, err = smstManagerOver(store, supplier).ProveClosest(ctx, sessionID, path)
		require.NoError(t, err)
		ok, _ = chainVerifies(t, proofBz, root)
		require.True(t, ok, "a proof from the leaves blob")

		require.NoError(t, failover.DeleteTree(ctx, sessionID))
		for _, rec := range smstAllRecords {
			exists, err := store.exists(ctx, rec, sessionID)
			require.NoError(t, err)
			require.False(t, exists, "record %d after DeleteTree", rec)
		}
	})
}

func TestSMSTStore_RecordsBehaveAsRedisKeys(t *testing.T) {
	const supplier, sessionID = "pokt1smstrec", "sess-rec"
	eachSMSTStore(t, supplier, func(t *testing.T, store smstStore) {
		ctx := context.Background()
		_, err := store.get(ctx, smstStats, sessionID)
		require.ErrorIs(t, err, errSMSTRecordAbsent)
		head, err := store.head(ctx, smstLeaves, sessionID, 4)
		require.NoError(t, err)
		require.Empty(t, head, "head of an absent record")
		require.NoError(t, store.expire(ctx, smstStats, sessionID, time.Hour), "expire of an absent record does nothing")

		require.NoError(t, store.set(ctx, smstStats, sessionID, []byte("12:34"), time.Hour))
		value, err := store.get(ctx, smstStats, sessionID)
		require.NoError(t, err)
		require.Equal(t, "12:34", string(value))
		head, err = store.head(ctx, smstStats, sessionID, 2)
		require.NoError(t, err)
		require.Equal(t, "12", string(head))
		require.NoError(t, store.set(ctx, smstClaimedRoot, sessionID, []byte("root"), time.Hour))

		n, err := store.del(ctx, sessionID, smstStats, smstLiveRoot)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the stats existed")
		exists, err := store.exists(ctx, smstStats, sessionID)
		require.NoError(t, err)
		require.False(t, exists)

		require.NoError(t, store.expire(ctx, smstClaimedRoot, sessionID, 0))
		exists, err = store.exists(ctx, smstClaimedRoot, sessionID)
		require.NoError(t, err)
		require.False(t, exists, "a TTL of zero deletes, as EXPIRE 0 does")
	})
}

func TestSMSTStore_ConditionalWrites(t *testing.T) {
	const supplier, sessionID = "pokt1smstcas", "sess-cas"
	eachSMSTStore(t, supplier, func(t *testing.T, store smstStore) {
		ctx := context.Background()
		set, err := store.setLiveRootIfUnchanged(ctx, sessionID, []byte("r1"), nil, time.Hour)
		require.NoError(t, err)
		require.True(t, set, "absent, as expected")
		set, err = store.setLiveRootIfUnchanged(ctx, sessionID, []byte("r2"), []byte("other"), time.Hour)
		require.NoError(t, err)
		require.False(t, set, "another writer's root is not overwritten")
		set, err = store.setLiveRootIfUnchanged(ctx, sessionID, []byte("r2"), []byte("r1"), time.Hour)
		require.NoError(t, err)
		require.True(t, set)
		live, err := store.get(ctx, smstLiveRoot, sessionID)
		require.NoError(t, err)
		require.Equal(t, "r2", string(live))

		nodes := store.nodes(ctx, sessionID)
		require.NoError(t, nodes.Set([]byte{1}, []byte{0, 1, 2}))
		require.NoError(t, store.set(ctx, smstLeaves, sessionID, []byte("blob"), time.Hour))
		deleted, err := store.deleteNodesIfLeaves(ctx, sessionID, []byte("another blob"))
		require.NoError(t, err)
		require.False(t, deleted, "the blob changed: the nodes stay")
		sessions, err := store.sessionsWithNodes(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{sessionID}, sessions)
		deleted, err = store.deleteNodesIfLeaves(ctx, sessionID, []byte("blob"))
		require.NoError(t, err)
		require.True(t, deleted)
		compacted, err := store.compacted(ctx, sessionID)
		require.NoError(t, err)
		require.True(t, compacted)
		sessions, err = store.sessionsWithNodes(ctx)
		require.NoError(t, err)
		require.Empty(t, sessions)
	})
}

// The claimed root is fsynced when it is written: an OS crash right after it
// cannot take back the root a claim was built from.
func TestPebbleSMSTStore_TheClaimedRootSurvivesAnOSCrash(t *testing.T) {
	fs := vfs.NewStrictMem()
	require.NoError(t, fs.MkdirAll("db", 0o755))
	root, err := fs.OpenDir("")
	require.NoError(t, err)
	require.NoError(t, root.Sync())
	require.NoError(t, root.Close())
	open := func() (*pebblestore.Store, smstStore) {
		db, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: "db", FS: fs, SyncInterval: time.Hour})
		require.NoError(t, err)
		backend := NewPebbleStoreBackend(zerolog.Nop(), db, pebblequeue.NewBroker(zerolog.Nop(), db, nil, "ha:relays"), SupplierManagerConfig{})
		return db, backend.smstStore("pokt1crash")
	}
	ctx := context.Background()
	db, store := open()
	require.NoError(t, store.set(ctx, smstStats, "s1", []byte("1:1"), 0))
	require.NoError(t, store.set(ctx, smstClaimedRoot, "s1", []byte("the-root"), 0))
	require.NoError(t, store.set(ctx, smstLiveRoot, "s1", []byte("after"), 0))

	fs.SetIgnoreSyncs(true)
	_ = db.Close()
	fs.ResetToSyncedState()
	fs.SetIgnoreSyncs(false)
	db, store = open()
	defer func() { _ = db.Close() }()

	got, err := store.get(ctx, smstClaimedRoot, "s1")
	require.NoError(t, err)
	require.Equal(t, "the-root", string(got))
	_, err = store.get(ctx, smstStats, "s1")
	require.NoError(t, err, "an earlier write is kept with it: the log is a prefix")
	_, err = store.get(ctx, smstLiveRoot, "s1")
	require.ErrorIs(t, err, errSMSTRecordAbsent, "control: a later write with no sync is lost")
}

// Nodes whose TTL ran out read as absent and are deleted by the sweep, as an
// expired Redis hash is gone; nodes within their TTL are kept.
func TestPebbleSMSTStore_TheSweepDeletesExpiredNodesAndRecords(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1sweep")
	store := h.backend.smstStore("pokt1sweep").(*pebbleSMSTStore)
	ctx := context.Background()
	require.NoError(t, store.nodes(ctx, "old").Set([]byte{1}, []byte{0, 1}))
	require.NoError(t, store.nodes(ctx, "live").Set([]byte{1}, []byte{0, 1}))
	h.backend.mu.Lock()
	batch := h.store.DB().NewBatch()
	past := time.Now().Add(-time.Minute)
	store.writeRecord(batch, smstNodes, "old", nil, past)
	store.writeRecord(batch, smstLiveRoot, "old", []byte("r"), past)
	store.writeRecord(batch, smstNodes, "live", nil, time.Now().Add(time.Hour))
	require.NoError(t, h.store.Commit(batch))
	h.backend.mu.Unlock()

	exists, err := store.exists(ctx, smstNodes, "old")
	require.NoError(t, err)
	require.False(t, exists, "expired nodes read as absent before the sweep")

	h.backend.mu.Lock()
	require.NoError(t, h.backend.sweepLocked(time.Now(), time.Hour))
	h.backend.mu.Unlock()

	require.Zero(t, h.count(store.nodesPrefix("old")), "the expired nodes are deleted")
	require.Zero(t, h.count([]byte(pebbleSMSTRecordPrefix+store.sessionPart("old"))), "and their records")
	require.Equal(t, int64(1), h.count(store.nodesPrefix("live")), "live nodes are kept")
}

// An expired live root is absent to the exit checkpoint, as an expired Redis
// key is to its script: a stale value neither matches nor blocks.
func TestPebbleSMSTStore_AnExpiredLiveRootReadsAsAbsent(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1expired")
	store := h.backend.smstStore("pokt1expired").(*pebbleSMSTStore)
	ctx := context.Background()
	h.backend.mu.Lock()
	batch := h.store.DB().NewBatch()
	store.writeRecord(batch, smstLiveRoot, "s1", []byte("stale"), time.Now().Add(-time.Minute))
	require.NoError(t, h.store.Commit(batch))
	h.backend.mu.Unlock()

	set, err := store.setLiveRootIfUnchanged(ctx, "s1", []byte("new"), []byte("stale"), time.Hour)
	require.NoError(t, err)
	require.False(t, set, "the expired value does not match")
	set, err = store.setLiveRootIfUnchanged(ctx, "s1", []byte("new"), nil, time.Hour)
	require.NoError(t, err)
	require.True(t, set, "absent, as expected")
}
