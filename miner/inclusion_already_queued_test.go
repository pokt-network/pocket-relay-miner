//go:build test

package miner

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// A node answering "I already hold this transaction" must not cost a resend.
//
// This is the expensive half of classifying code 19, and the quiet one. Getting
// the metric label wrong is noise an operator can work around; counting the
// attempt spends one of the few resends a claim has WITHOUT a single packet
// leaving for the chain, and it looks like progress while it does it. Once
// resends run on every block this is the commonest answer there is -- re-sending
// to the same node while its mempool still holds the transaction -- so a claim
// whose transaction was already on its way would exhaust its whole budget on
// nothing.
//
// WHICH HALF THIS IS. It starts from a sentinel handed to it, so it proves what
// the reconciler DOES with one -- and nothing about whether a code 19 from the
// chain ever becomes that sentinel. That half lives in
// tx/tx_rejection_classification_test.go, which drives a real broadcast, and it
// is not an academic split: while this file was the only coverage, making
// isAlreadyQueuedRejection return false unconditionally left every test in ./tx/
// and ./miner/ green. Read as the whole picture, the fixture below is exactly
// the shape that hides that.
//
// The attempt count is read back FROM THE STORE rather than from the harness,
// deliberately: the store is real here, so this asserts what a promoted replica
// would find after a failover, which is the property that matters. The metric
// label below is read from the harness, which substitutes recordRebroadcast --
// so that assertion covers what the reconciler HANDS OVER, and nothing about
// Prometheus.
func TestReconciler_AlreadyQueuedDoesNotSpendAResend(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("%w: %w", tx.ErrTxAlreadyQueued, &tx.TxRejection{
		Stage:     tx.TxStageCheckTx,
		ABCICode:  19,
		Codespace: "sdk",
		RawLog:    "tx already in mempool",
	})
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testSubmit + 1) // past the one-block grace: the resend fires

	require.Equal(t, 1, h.resub.attemptCount(),
		"the resend must have been attempted: this is about what it COSTS, not whether it ran")

	entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Zero(t, entry.Rebroadcasts,
		"a transaction the node already holds must not consume one of the few resends "+
			"a claim gets: nothing was transmitted and nothing changed")
	require.Zero(t, entry.LastAttemptHeight,
		"and the height of the last counted attempt must not move either: it "+
			"records when the budget was last spent, so marking a send that never "+
			"left would date an attempt that did not happen")

	require.Equal(t, []string{hSupplier + "/already_queued"}, rebroadcastResults(t, h),
		"and it must not land in the error bucket, which would bury real failures "+
			"under a rate that only says the loop is working")
}

// The control, and it is what stops the exemption from swallowing real failures:
// an ordinary rejection must still cost its attempt and still be an error.
//
// Without it, a change that exempted EVERYTHING would satisfy the case above --
// and that is exactly the change worth fearing here, because a resend budget
// that is never consumed looks identical to one that is working.
func TestReconciler_AnOrdinaryRejectionStillSpendsAResend(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("connection refused")
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testSubmit + 1)

	entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Equal(t, 1, entry.Rebroadcasts,
		"a genuine failure must still spend its attempt, or a persistently failing "+
			"resend re-fires on every block until the window closes")
	require.Equal(t, testSubmit+1, entry.LastAttemptHeight,
		"and it must date the entry at the height that attempt went out")
	require.Equal(t, []string{hSupplier + "/error"}, rebroadcastResults(t, h))
}
