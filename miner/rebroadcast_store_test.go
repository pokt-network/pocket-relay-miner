//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newRebroadcastStoreForTest(t *testing.T) *RebroadcastStore {
	t.Helper()
	rc, _ := newTestRedis(t)
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

// The signed-tx cache round-trips, and a miss is not an error.
//
// The distinction is the whole point of the pair: "nobody stored these" and
// "Redis would not answer" both end in the same action -- sign a fresh
// transaction -- but only the second is worth an operator's attention, so they
// must not arrive as the same value. A Get that folded the miss into an error
// would make every first resend look like a Redis fault; one that folded a
// fault into an empty result would hide a real outage behind extra signatures.
func TestRebroadcastStore_SignedTxRoundTripAndMiss(t *testing.T) {
	s := newRebroadcastStoreForTest(t)
	ctx := context.Background()
	const hash = "9F2CDEADBEEF"

	deadline := time.Now().Add(9 * time.Minute).Truncate(time.Nanosecond)

	got, _, _, err := s.GetSignedTx(ctx, hash)
	require.NoError(t, err, "a miss is not a failure: nothing was stored yet")
	require.Nil(t, got)

	require.NoError(t, s.PutSignedTx(ctx, hash, []byte("signed-bytes"), deadline, 0))

	got, gotDeadline, _, err := s.GetSignedTx(ctx, hash)
	require.NoError(t, err)
	require.True(t, gotDeadline.Equal(deadline),
		"the deadline must come back WITH the bytes: it is sealed inside them and "+
			"nothing on the entry can reconstruct it, so bytes without it cannot be "+
			"checked for expiry before re-injection")
	require.Equal(t, []byte("signed-bytes"), got,
		"the bytes must come back BYTE-IDENTICAL: re-injecting anything else is a "+
			"different transaction with a different hash, which is exactly what "+
			"re-injection exists to avoid")

	require.NoError(t, s.DeleteSignedTx(ctx, hash))

	got, _, _, err = s.GetSignedTx(ctx, hash)
	require.NoError(t, err)
	require.Nil(t, got, "after discarding, the next resend must fall back to signing")
}

// Discarding bytes that were never stored is a no-op, and that is load-bearing
// rather than mere tolerance: the caller invalidates on EVERY failure without
// first asking whether anything is there, so a delete-of-nothing happens on the
// common path. Making it an error would turn the ordinary first failure of an
// entry that never cached anything into a reported fault.
func TestRebroadcastStore_DiscardingNothingIsNotAnError(t *testing.T) {
	s := newRebroadcastStoreForTest(t)
	require.NoError(t, s.DeleteSignedTx(context.Background(), "NEVER-STORED"))
}

// A nil store and an empty hash are inert on all three methods.
//
// The nil case is not hypothetical: the rebroadcast store is optional wiring --
// the lifecycle already guards its other calls with `lc.rebroadcastStore != nil`
// -- and the empty hash arrives on the path that matters most, an original
// submission whose broadcast never returned a hash to key anything by.
func TestRebroadcastStore_SignedTxNilAndEmptyAreInert(t *testing.T) {
	ctx := context.Background()

	var nilStore *RebroadcastStore
	require.NoError(t, nilStore.PutSignedTx(ctx, "H", []byte("x"), time.Now(), 0))
	require.NoError(t, nilStore.DeleteSignedTx(ctx, "H"))
	got, _, _, err := nilStore.GetSignedTx(ctx, "H")
	require.NoError(t, err)
	require.Nil(t, got)

	// The empty hash is asserted against the KEYSPACE and not through GetSignedTx,
	// and that is not fussiness -- it is the only way to see it.
	//
	// Measured: with the Get guard in place, removing the PUT guard changes
	// nothing this test could observe through Get, because Get refuses to look
	// up an empty hash and never finds what Put wrote. The two guards mask each
	// other, so an assertion phrased through Get pins neither. What a missing Put
	// guard actually does is write bytes under a key nobody can ever name --
	// leaked until the TTL, and invisible to every reader -- so the question has
	// to be asked of Redis directly: did a key appear?
	rc, _ := newTestRedis(t)
	s := NewRebroadcastStore(rc, time.Hour)
	require.NoError(t, s.PutSignedTx(ctx, "", []byte("x"), time.Now(), 0))
	keys, err := rc.Keys(ctx, rc.KB().TxSignedBytesKey("*")).Result()
	require.NoError(t, err)
	require.Empty(t, keys,
		"an empty hash must write NOTHING: a key with no hash in it can never be "+
			"looked up again, so those bytes are leaked rather than cached")

	require.NoError(t, s.DeleteSignedTx(ctx, ""))
	got, _, _, err = s.GetSignedTx(ctx, "")
	require.NoError(t, err)
	require.Nil(t, got, "no hash means nothing to look up, not an error")
}

// The cap admits its own boundary and excludes one byte past it.
//
// Both halves are needed and the boundary is where the value is. A test that
// only stored something small would pass against a cap of any size, including
// one so tight it excludes every real proof -- and a cap that silently excluded
// everything looks identical, from the outside, to one that never fires. That
// is exactly the failure the counter beside the constant exists to make
// visible, and this is its compile-time half.
//
// Not being cached is NOT an error: the resend signs a fresh transaction, which
// is what it does today, so PutSignedTx returns nil and the caller has nothing
// different to do. Asserting the error is nil is therefore part of the contract
// and not incidental.
func TestRebroadcastStore_SignedTxSizeCap(t *testing.T) {
	s := newRebroadcastStoreForTest(t)
	ctx := context.Background()

	atCap := make([]byte, maxSignedTxCacheBytes)
	require.NoError(t, s.PutSignedTx(ctx, "AT-CAP", atCap, time.Now(), 0))
	got, _, _, err := s.GetSignedTx(ctx, "AT-CAP")
	require.NoError(t, err)
	require.Len(t, got, maxSignedTxCacheBytes,
		"the cap is inclusive: a payload of exactly the limit is still cached")

	overCap := make([]byte, maxSignedTxCacheBytes+1)
	require.NoError(t, s.PutSignedTx(ctx, "OVER-CAP", overCap, time.Now(), 0),
		"exceeding the cap is a decision, not a failure: the caller has nothing to handle")
	got, _, _, err = s.GetSignedTx(ctx, "OVER-CAP")
	require.NoError(t, err)
	require.Nil(t, got,
		"one byte past the limit must not be cached at all -- half a payload would "+
			"be worse than none, and a resend that finds nothing signs exactly as it does today")
}
