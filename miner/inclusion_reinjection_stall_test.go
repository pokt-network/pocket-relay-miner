//go:build test

package miner

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

func seedStuck(t *testing.T, h *reconcilerHarness, sessionID string, lastAttempt int64) {
	t.Helper()
	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:            []byte(sessionID),
		SubmitHeight:        testSubmit,
		TxHash:              "tx-" + sessionID,
		OrigTxHash:          "tx-" + sessionID,
		LastAttemptHeight:   lastAttempt,
		SignedBytes:         []byte("cached-signed-tx"),
		SignedTimeoutAt:     time.Now().Add(time.Hour).UnixNano(),
		SignedTimeoutHeight: 4321,
	})
	require.NoError(t, err)
	require.NoError(t, h.store.Put(t.Context(), RebroadcastPhaseProof, hSupplier, hEnd, sessionID, b))
}

// Re-injecting forever is stopped by a height, because nothing else can stop it.
//
// CometBFT keeps a transaction in its mempool cache after it COMMITS
// SUCCESSFULLY, so re-sending those bytes answers "I already hold this" for as
// long as the entry lives. That answer preserves the bytes and is exempt from
// counting an attempt -- each correct on its own -- and together they make an
// entry that re-injects on every block with its budget frozen at the starting
// value. MaxRebroadcasts never bites because the counter never moves.
//
// So the bound is measured in BLOCKS OF GETTING NOWHERE, using the height of
// the last attempt that actually spent budget: it stands still exactly while
// this is happening, which is what makes it a clock for it.
func TestReconciler_ReinjectingTooLongDiscardsTheBytes(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("%w: tx already in mempool", tx.ErrTxAlreadyQueued)
	// Last real attempt well behind: this entry has been echoing since.
	seedStuck(t, h, "s1", testSubmit)

	h.r.OnBlock(testSubmit + reinjectionStallBlocks)

	entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Empty(t, entry.SignedBytes,
		"after enough blocks of the same echo the bytes must go, so the next resend "+
			"signs a transaction with a NEW nonce -- one the node has no cached answer for")
	require.Zero(t, entry.Rebroadcasts,
		"and this still must not count as an attempt: nothing was transmitted, which "+
			"is why the counter could never have bounded this in the first place")
}

// The control, and it is what keeps the bound from becoming a timer that kills
// healthy re-injection: one block of "already queued" changes nothing.
//
// Without it, a bound of zero would satisfy the case above and turn every
// re-injection into a signature -- the feature removed while its tests stayed
// green.
func TestReconciler_OneBlockOfAlreadyQueuedKeepsTheBytes(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("%w: tx already in mempool", tx.ErrTxAlreadyQueued)
	seedStuck(t, h, "s1", testSubmit)

	h.r.OnBlock(testSubmit + 1)

	entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Equal(t, []byte("cached-signed-tx"), entry.SignedBytes,
		"the node holding the transaction is the EXPECTED answer while it is in "+
			"flight: cutting at the first one would sign a duplicate for every resend")
}

// An entry that never had a counted attempt still gets a clock.
//
// LastAttemptHeight is zero until something spends budget, and zero must not be
// read as "height 0" -- that would make every such entry look stuck since the
// genesis block and discard its bytes on the first resend. The submit height is
// the honest starting point.
func TestReconciler_TheStallClockFallsBackToTheSubmitHeight(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("%w: tx already in mempool", tx.ErrTxAlreadyQueued)
	seedStuck(t, h, "s1", 0) // never a counted attempt

	h.r.OnBlock(testSubmit + 1)

	entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Equal(t, []byte("cached-signed-tx"), entry.SignedBytes,
		"one block after submitting is not stuck, and reading the zero as a real "+
			"height would have discarded on the very first resend")
}
