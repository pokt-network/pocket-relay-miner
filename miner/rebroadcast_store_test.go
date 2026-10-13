//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/storage/kv"
)

func newRebroadcastStoreForTest(t *testing.T) *RebroadcastStore {
	t.Helper()
	rc, _ := newTestRedis(t)
	return NewRebroadcastStore(rc, time.Hour)
}

// eachRebroadcastStore runs body against both backends' rebroadcast stores.
func eachRebroadcastStore(t *testing.T, body func(t *testing.T, s RebroadcastStorage)) {
	t.Run("redis", func(t *testing.T) { body(t, newRebroadcastStoreForTest(t)) })
	t.Run("pebble", func(t *testing.T) {
		h := newPebbleCommitHarness(t, "pokt1rebroadcast")
		body(t, newPebbleRebroadcastStore(h.store, time.Hour))
	})
}

func TestRebroadcastStore_PutListDelete(t *testing.T) {
	eachRebroadcastStore(t, func(t *testing.T, s RebroadcastStorage) {
		ctx := context.Background()
		const supplier = "pokt1abc"
		const sessionEnd = int64(120)

		require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, supplier, sessionEnd, "s1", []byte("payload-1")))
		require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, supplier, sessionEnd, "s2", []byte("payload-2")))

		got, err := s.List(ctx, RebroadcastPhaseProof, supplier, sessionEnd)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, []byte("payload-1"), got["s1"])
		require.Equal(t, []byte("payload-2"), got["s2"])

		// Group registered in the index for failover recovery.
		groups, err := s.ActiveGroups(ctx, RebroadcastPhaseProof)
		require.NoError(t, err)
		require.Equal(t, []RebroadcastGroup{{Supplier: supplier, SessionEnd: sessionEnd}}, groups)

		// Delete one — group still present.
		require.NoError(t, s.Delete(ctx, RebroadcastPhaseProof, supplier, sessionEnd, "s1"))
		got, err = s.List(ctx, RebroadcastPhaseProof, supplier, sessionEnd)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Contains(t, got, "s2")

		groups, err = s.ActiveGroups(ctx, RebroadcastPhaseProof)
		require.NoError(t, err)
		require.Len(t, groups, 1, "group still has one pending session")

		// Delete last — group de-registered from index.
		require.NoError(t, s.Delete(ctx, RebroadcastPhaseProof, supplier, sessionEnd, "s2"))
		got, err = s.List(ctx, RebroadcastPhaseProof, supplier, sessionEnd)
		require.NoError(t, err)
		require.Empty(t, got)

		groups, err = s.ActiveGroups(ctx, RebroadcastPhaseProof)
		require.NoError(t, err)
		require.Empty(t, groups, "empty group must be removed from the index")
	})
}

// Claim and proof phases are isolated (separate keyspaces).
func TestRebroadcastStore_PhaseIsolation(t *testing.T) {
	eachRebroadcastStore(t, func(t *testing.T, s RebroadcastStorage) {
		ctx := context.Background()
		const supplier = "pokt1abc"
		const sessionEnd = int64(120)

		require.NoError(t, s.Put(ctx, RebroadcastPhaseClaim, supplier, sessionEnd, "s1", []byte("claim")))
		require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, supplier, sessionEnd, "s1", []byte("proof")))

		claims, err := s.List(ctx, RebroadcastPhaseClaim, supplier, sessionEnd)
		require.NoError(t, err)
		require.Equal(t, []byte("claim"), claims["s1"])

		proofs, err := s.List(ctx, RebroadcastPhaseProof, supplier, sessionEnd)
		require.NoError(t, err)
		require.Equal(t, []byte("proof"), proofs["s1"])

		proofGroups, err := s.ActiveGroups(ctx, RebroadcastPhaseProof)
		require.NoError(t, err)
		require.Len(t, proofGroups, 1)
	})
}

// ActiveGroups round-trips supplier+sessionEnd parsing across multiple groups.
func TestRebroadcastStore_ActiveGroupsParsing(t *testing.T) {
	eachRebroadcastStore(t, func(t *testing.T, s RebroadcastStorage) {
		ctx := context.Background()

		require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "pokt1aaa", 60, "s1", []byte("x")))
		require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "pokt1bbb", 120, "s2", []byte("y")))

		groups, err := s.ActiveGroups(ctx, RebroadcastPhaseProof)
		require.NoError(t, err)
		require.Len(t, groups, 2)
		seen := map[string]int64{}
		for _, g := range groups {
			seen[g.Supplier] = g.SessionEnd
		}
		require.Equal(t, int64(60), seen["pokt1aaa"])
		require.Equal(t, int64(120), seen["pokt1bbb"])
	})
}

// CleanupIfEmpty reaps an index member whose group hash is gone (TTL-expired),
// without touching a group that still has live payloads.
func TestRebroadcastStore_CleanupIfEmpty(t *testing.T) {
	s := newRebroadcastStoreForTest(t)
	ctx := context.Background()

	// Group with a live payload: cleanup must NOT remove it from the index.
	require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "pokt1live", 60, "s1", []byte("p")))
	require.NoError(t, s.CleanupIfEmpty(ctx, RebroadcastPhaseProof, "pokt1live", 60))
	groups, err := s.ActiveGroups(ctx, RebroadcastPhaseProof)
	require.NoError(t, err)
	require.Len(t, groups, 1, "group with live payload must remain registered")

	// Simulate a TTL-expired group: index member present but hash gone. Register
	// via Put, then delete only the hash field-by-field is overkill — instead add
	// a second group and drop its hash directly through the client.
	require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "pokt1ghost", 60, "s2", []byte("p")))
	require.NoError(t, s.redisClient.Del(ctx, s.groupKey(RebroadcastPhaseProof, "pokt1ghost", 60)).Err())

	// Now ghost is in the index but its hash is gone.
	require.NoError(t, s.CleanupIfEmpty(ctx, RebroadcastPhaseProof, "pokt1ghost", 60))
	groups, err = s.ActiveGroups(ctx, RebroadcastPhaseProof)
	require.NoError(t, err)
	require.Len(t, groups, 1, "ghost group must be reaped from the index")
	require.Equal(t, "pokt1live", groups[0].Supplier)
}

// Deleting the last field atomically de-registers the group (Lua path).
func TestRebroadcastStore_DeleteLastFieldDeregisters(t *testing.T) {
	s := newRebroadcastStoreForTest(t)
	ctx := context.Background()
	require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "pokt1abc", 60, "s1", []byte("p")))
	require.NoError(t, s.Delete(ctx, RebroadcastPhaseProof, "pokt1abc", 60, "s1"))

	groups, err := s.ActiveGroups(ctx, RebroadcastPhaseProof)
	require.NoError(t, err)
	require.Empty(t, groups, "draining the last field must de-register the group")
	// Hash key auto-removed by HDEL of last field.
	exists, err := s.redisClient.Exists(ctx, s.groupKey(RebroadcastPhaseProof, "pokt1abc", 60)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), exists)
}

// Nil store is safe (optional component).
func TestRebroadcastStore_NilSafe(t *testing.T) {
	var s *RebroadcastStore
	ctx := context.Background()
	require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "x", 1, "s", []byte("p")))
	got, err := s.List(ctx, RebroadcastPhaseProof, "x", 1)
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, s.Delete(ctx, RebroadcastPhaseProof, "x", 1, "s"))
	groups, err := s.ActiveGroups(ctx, RebroadcastPhaseProof)
	require.NoError(t, err)
	require.Nil(t, groups)
}

// Concurrent Put/Delete/List with the race detector.
func TestRebroadcastStore_Concurrent(t *testing.T) {
	eachRebroadcastStore(t, func(t *testing.T, s RebroadcastStorage) {
		ctx := context.Background()
		const supplier = "pokt1abc"
		const sessionEnd = int64(120)

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				sid := fmt.Sprintf("s%d", n)
				_ = s.Put(ctx, RebroadcastPhaseProof, supplier, sessionEnd, sid, []byte("p"))
				_, _ = s.List(ctx, RebroadcastPhaseProof, supplier, sessionEnd)
				_ = s.Delete(ctx, RebroadcastPhaseProof, supplier, sessionEnd, sid)
			}(i)
		}
		wg.Wait()
	})
}

// A group past its expiry reads as empty and drops out of ActiveGroups, as a
// Redis group whose hash expired is empty and is reaped from the index.
func TestPebbleRebroadcastStore_AnExpiredGroupIsGone(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1rebroadcast")
	s := newPebbleRebroadcastStore(h.store, time.Hour)
	ctx := context.Background()
	require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "pokt1live", 60, "s1", []byte("p")))
	require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "pokt1ghost", 60, "s2", []byte("p")))
	batch := h.store.DB().NewBatch()
	_ = batch.Set(s.groupKey(RebroadcastPhaseProof, "pokt1ghost", 60), kv.EncodeValue(time.Now().Add(-time.Minute), nil), nil)
	require.NoError(t, h.store.Commit(batch))

	got, err := s.List(ctx, RebroadcastPhaseProof, "pokt1ghost", 60)
	require.NoError(t, err)
	require.Empty(t, got)
	groups, err := s.ActiveGroups(ctx, RebroadcastPhaseProof)
	require.NoError(t, err)
	require.Equal(t, []RebroadcastGroup{{Supplier: "pokt1live", SessionEnd: 60}}, groups)
	require.Zero(t, h.count(s.payloadPrefix(RebroadcastPhaseProof, "pokt1ghost", 60)), "the expired group's payloads are deleted")

	require.NoError(t, s.Put(ctx, RebroadcastPhaseProof, "pokt1ghost", 60, "s3", []byte("q")))
	got, err = s.List(ctx, RebroadcastPhaseProof, "pokt1ghost", 60)
	require.NoError(t, err)
	require.Equal(t, map[string][]byte{"s3": []byte("q")}, got, "a Put after the expiry starts a new group")
}

// A Put into a group past its expiry starts a new group, as HSET after the hash
// expired does: the old payloads are not part of it.
func TestPebbleRebroadcastStore_APutAfterTheExpiryStartsANewGroup(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1rebroadcast")
	s := newPebbleRebroadcastStore(h.store, time.Hour)
	ctx := context.Background()
	require.NoError(t, s.Put(ctx, RebroadcastPhaseClaim, "pokt1again", 60, "old", []byte("p")))
	batch := h.store.DB().NewBatch()
	_ = batch.Set(s.groupKey(RebroadcastPhaseClaim, "pokt1again", 60), kv.EncodeValue(time.Now().Add(-time.Minute), nil), nil)
	require.NoError(t, h.store.Commit(batch))

	require.NoError(t, s.Put(ctx, RebroadcastPhaseClaim, "pokt1again", 60, "new", []byte("q")))

	got, err := s.List(ctx, RebroadcastPhaseClaim, "pokt1again", 60)
	require.NoError(t, err)
	require.Equal(t, map[string][]byte{"new": []byte("q")}, got)
}
