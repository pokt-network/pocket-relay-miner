//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedSharingTx stores one entry that points at a given transaction hash, the
// way a batched claim does: every session of the batch carries the SAME hash.
func seedSharingTx(t *testing.T, h *reconcilerHarness, sessionID, txHash string) {
	t.Helper()
	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:     []byte(sessionID),
		SubmitHeight: testSubmit,
		TxHash:       txHash,
		OrigTxHash:   txHash,
	})
	require.NoError(t, err)
	require.NoError(t, h.store.Put(context.Background(), RebroadcastPhaseProof, hSupplier, hEnd, sessionID, b))
}

// The cached bytes survive until the LAST entry holding them settles.
//
// This is the whole reason the release asks the store instead of just deleting:
// a claim transaction carries a batch and leaves one entry per session, every
// one of them naming the same hash, so those bytes belong to the TRANSACTION.
// Freeing them when the first session settles is not dangerous -- the others
// fall back to signing, which is what they do today -- but it gives the saving
// up precisely on the batched path, where one transaction covers the most
// sessions. That failure is invisible from the outside: everything still works,
// just with N-1 signatures nobody asked for.
func TestReconciler_CachedTxSurvivesUntilTheLastSiblingSettles(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	ctx := context.Background()
	const sharedHash = "SHARED-TX-HASH"

	seedSharingTx(t, h, "s1", sharedHash)
	seedSharingTx(t, h, "s2", sharedHash)
	require.NoError(t, h.store.PutSignedTx(ctx, sharedHash, []byte("signed"), time.Now().Add(time.Minute), 0))

	entry := rebroadcastEntry{TxHash: sharedHash, OrigTxHash: sharedHash}

	h.r.clear(ctx, RebroadcastPhaseProof, RebroadcastGroup{Supplier: hSupplier, SessionEnd: hEnd}, "s1", entry)

	got, _, _, err := h.store.GetSignedTx(ctx, sharedHash)
	require.NoError(t, err)
	require.NotNil(t, got,
		"s2 is still pending on the SAME transaction: freeing here would make its "+
			"next resend sign a fresh one for no reason")

	h.r.clear(ctx, RebroadcastPhaseProof, RebroadcastGroup{Supplier: hSupplier, SessionEnd: hEnd}, "s2", entry)

	got, _, _, err = h.store.GetSignedTx(ctx, sharedHash)
	require.NoError(t, err)
	require.Nil(t, got,
		"with the last sibling gone the transaction can never be re-injected, so "+
			"its bytes are rubbish from this instant and must not wait for the TTL")
}

// The control, and it is what stops the check from becoming a leak: a lone
// entry frees its bytes on the first clear.
//
// Without it, an implementation that never freed anything -- or one whose
// sibling search always found a match -- would satisfy the case above and quietly
// leave every payload to expire, which is exactly the behaviour this replaces.
func TestReconciler_ALoneEntryFreesItsCachedTxImmediately(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	ctx := context.Background()
	const hash = "LONE-TX-HASH"

	seedSharingTx(t, h, "only", hash)
	require.NoError(t, h.store.PutSignedTx(ctx, hash, []byte("signed"), time.Now().Add(time.Minute), 0))

	h.r.clear(ctx, RebroadcastPhaseProof, RebroadcastGroup{Supplier: hSupplier, SessionEnd: hEnd}, "only",
		rebroadcastEntry{TxHash: hash, OrigTxHash: hash})

	got, _, _, err := h.store.GetSignedTx(ctx, hash)
	require.NoError(t, err)
	require.Nil(t, got, "nothing else pointed at it, so it goes with the entry")
}

// A sibling on a DIFFERENT transaction does not hold the bytes hostage.
//
// The group is (phase, supplier, sessionEnd) and it can hold entries from more
// than one transaction -- every resend that signed rather than re-injected left
// one. A check that asked "is the group empty" instead of "does anyone still
// name THIS hash" would keep every payload alive until the whole group drained,
// turning the release into a slower TTL.
func TestReconciler_ASiblingOnAnotherTxDoesNotHoldTheBytes(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	ctx := context.Background()
	const mine, theirs = "MY-TX", "THEIR-TX"

	seedSharingTx(t, h, "s1", mine)
	seedSharingTx(t, h, "s2", theirs)
	require.NoError(t, h.store.PutSignedTx(ctx, mine, []byte("signed"), time.Now().Add(time.Minute), 0))

	h.r.clear(ctx, RebroadcastPhaseProof, RebroadcastGroup{Supplier: hSupplier, SessionEnd: hEnd}, "s1",
		rebroadcastEntry{TxHash: mine, OrigTxHash: mine})

	got, _, _, err := h.store.GetSignedTx(ctx, mine)
	require.NoError(t, err)
	require.Nil(t, got,
		"s2 belongs to another transaction, so it is not a sibling and must not "+
			"delay this release")
}

// The reconciler drives a store that is not the Redis one, end to end.
//
// This is the checkable form of "is it really an interface": the store below
// shares no code and no dependency with RebroadcastStore, it is handed in
// through the constructor, and the release path frees through it. If this stops
// compiling or stops passing, the seam has been tied back to the concrete type
// -- which is the failure this test exists to catch, because it is silent
// everywhere else.
func TestReconciler_ReleasesThroughANonRedisSignedTxStore(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	ctx := context.Background()
	const hash = "MEM-TX"

	mem := newMemRebroadcastStore()
	h.r.store = mem // the constructor takes it too; this pins the field
	require.NoError(t, mem.PutSignedTx(ctx, hash, []byte("signed"), time.Now().Add(time.Minute), 0))

	seedSharingTx(t, h, "only", hash)
	h.r.clear(ctx, RebroadcastPhaseProof, RebroadcastGroup{Supplier: hSupplier, SessionEnd: hEnd}, "only",
		rebroadcastEntry{TxHash: hash, OrigTxHash: hash})

	got, _, _, err := mem.GetSignedTx(ctx, hash)
	require.NoError(t, err)
	require.Nil(t, got,
		"the release must go through whatever implementation was injected, not "+
			"through the Redis store the reconciler happens to also hold")
}
