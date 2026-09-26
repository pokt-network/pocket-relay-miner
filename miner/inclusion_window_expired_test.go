//go:build test

package miner

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// rebroadcastResults returns what the harness captured from recordRebroadcast,
// as "supplier/result" strings.
//
// It reads the HARNESS, not the Prometheus counter. The first version of this
// test asserted on proofRebroadcastsTotal and went red for the wrong reason: the
// harness substitutes its own recordRebroadcast, so the real counter is never
// touched on this path and no assertion about it could ever have meant anything.
// A green there would have been worse than the red.
func rebroadcastResults(t *testing.T, h *reconcilerHarness) []string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.rebroadcast...)
}

// A resend the chain refused because the window had closed must be recorded as
// its own outcome, not as a transport error.
//
// Both used to arrive as result="error", which put "the node was unreachable"
// -- something the next block may fix -- in the same bucket as "these bytes can
// never be accepted again", which nothing fixes. An operator reading that metric
// cannot tell a flapping endpoint from a resend calendar that runs too late, and
// those have opposite remedies.
//
// The window is not exotic: RebroadcastSafetyBlocks defaults to 1, so
// canRebroadcast authorises a resend as late as windowClose-2, leaving the
// transaction two blocks to land.
func TestReconciler_WindowExpiredResendIsNotCountedAsATransportError(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("%w: %w", tx.ErrTxWindowExpired, &tx.TxRejection{
		Stage:     tx.TxStageCheckTx,
		ABCICode:  30,
		Codespace: "sdk",
		RawLog:    "block height: 121, timeout height: 120: tx timeout height",
	})
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testSubmit + 1) // past the one-block grace: the resend fires

	require.Equal(t, 1, h.resub.attemptCount(), "the resend must have been attempted once")
	require.Equal(t, []string{hSupplier + "/window_closed"}, rebroadcastResults(t, h),
		"a rejection on timeout height must be recorded as window_closed, not as a transport error")
}

// The control, and it carries as much weight as the case above: a classifier
// answering "window_closed" to everything would satisfy that test on its own.
// An ordinary failure must still be an error.
//
// It also pins the half of the behaviour that deliberately did NOT change. The
// attempt is counted either way -- a doomed resend that stopped consuming its
// budget would re-fire on every block until the window closed, which is the
// policy written where that counter lives. Only the label moved.
func TestReconciler_OrdinaryResendFailureIsStillATransportError(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("connection refused")
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testSubmit + 1)

	require.Equal(t, []string{hSupplier + "/error"}, rebroadcastResults(t, h),
		"a transport failure must stay in the error bucket")
	require.Equal(t, 1, h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1").Rebroadcasts,
		"the attempt is still counted: that policy is unchanged by the new label")
}
