//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

func newRebroadcastStoreForTest(t *testing.T) *RebroadcastStore {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rc, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	return NewRebroadcastStore(rc, time.Hour)
}

func TestRebroadcastStore_PutListDelete(t *testing.T) {
	s := newRebroadcastStoreForTest(t)
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
}

// Claim and proof phases are isolated (separate keyspaces).
func TestRebroadcastStore_PhaseIsolation(t *testing.T) {
	s := newRebroadcastStoreForTest(t)
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
}

// ActiveGroups round-trips supplier+sessionEnd parsing across multiple groups.
func TestRebroadcastStore_ActiveGroupsParsing(t *testing.T) {
	s := newRebroadcastStoreForTest(t)
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
	s := newRebroadcastStoreForTest(t)
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
}
