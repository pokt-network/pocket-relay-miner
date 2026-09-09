//go:build test

package miner

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// seedWithCachedTx stores an entry that already carries a signed transaction,
// the way one looks after a submission that went out.
func seedWithCachedTx(t *testing.T, h *reconcilerHarness, sessionID string) {
	t.Helper()
	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:            []byte(sessionID),
		SubmitHeight:        testSubmit,
		TxHash:              "tx-" + sessionID,
		OrigTxHash:          "tx-" + sessionID,
		SignedBytes:         []byte("cached-signed-tx"),
		SignedTimeoutAt:     time.Now().Add(time.Hour).UnixNano(),
		SignedTimeoutHeight: 4321,
	})
	require.NoError(t, err)
	require.NoError(t, h.store.Put(t.Context(), RebroadcastPhaseProof, hSupplier, hEnd, sessionID, b))
}

// A rejected transaction must be DISCARDED, not re-sent for the rest of the
// window.
//
// This is the property that makes re-injection safe on its own, and the one the
// feature shipped without: the bytes carry the same everything, so a chain that
// refused them refuses them again. With resends running on every block that is
// one wasted attempt per block until the window closes -- and the resend never
// signs a valid replacement, because it keeps finding a cached transaction to
// send. The budget does not save it either: a re-injected duplicate can be
// answered with code 19, which is exempt from counting.
//
// The assertion is on the ENTRY AFTER the failure and not on what the call
// returned, deliberately. The return value was always right; what was wrong was
// what got written down, and only the next block reads that.
func TestReconciler_AFailedResendDiscardsTheCachedTx(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("CheckTx refused: invalid signature")
	seedWithCachedTx(t, h, "s1")

	h.r.OnBlock(testSubmit + 1)

	entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Empty(t, entry.SignedBytes,
		"a transaction the chain refused must not be re-injected: the next resend "+
			"has to sign a fresh one, which is what it did before any of this existed")
	require.Zero(t, entry.SignedTimeoutAt,
		"and its deadline goes with it, so nothing can read a stale one as alive")
	require.Zero(t, entry.SignedTimeoutHeight)
}

// The control on the other side: a SUCCESSFUL resend keeps what it sent.
//
// Without it, an implementation that discarded on every path would satisfy the
// case above and quietly turn every resend back into a signature -- the feature
// removed while all its tests stayed green.
func TestReconciler_ASuccessfulResendKeepsTheCachedTx(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.sends = tx.SignedTxPayload{
		Bytes:         []byte("what-went-out"),
		TimeoutAt:     time.Now().Add(time.Hour),
		TimeoutHeight: 4321,
	}
	seedWithCachedTx(t, h, "s1")

	h.r.OnBlock(testSubmit + 1)

	entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Equal(t, []byte("what-went-out"), entry.SignedBytes,
		"the entry must carry what actually went out, or the next resend signs "+
			"again for nothing")
	require.Equal(t, int64(4321), entry.SignedTimeoutHeight)
}

// The two answers that mean NOTHING HAPPENED keep the bytes, and this is where
// discarding would do real harm rather than merely waste a signature.
//
// Already-queued says the node holds THESE EXACT BYTES right now. Discarding
// them makes the next resend sign a second transaction while the first is still
// in that mempool -- two live transactions for one claim, which is the
// duplicate the whole mechanism exists to prevent. Saturation never signed nor
// sent anything, so the cached transaction is untouched and still the one in
// flight.
//
// They are the same pair exempt from counting an attempt, and the test pins
// that they stay together: an answer treated as "nothing happened" for the
// budget and as a failure for the cache would spend a signature the counter
// says was never spent.
func TestReconciler_NothingWasSpentKeepsTheCachedTx(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "already queued: the node holds these very bytes", err: tx.ErrTxAlreadyQueued},
		{name: "saturated: it was never signed nor sent", err: tx.ErrTxConcurrencySaturated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newReconcilerHarness(t, 1)
			h.resub.failWith = fmt.Errorf("%w: wrapped", tt.err)
			seedWithCachedTx(t, h, "s1")

			h.r.OnBlock(testSubmit + 1)

			entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
			require.Equal(t, []byte("cached-signed-tx"), entry.SignedBytes,
				"nothing happened to these bytes, so they must survive")
			require.Zero(t, entry.Rebroadcasts,
				"and the budget must not move either -- the counter and the cache "+
					"answer the same question and must not drift apart")
			require.True(t, errors.Is(h.resub.failWith, tt.err))
		})
	}
}
