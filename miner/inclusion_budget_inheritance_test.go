//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedWithBudget stores an entry carrying the broadcast budget its original
// submission was born with. The harness's own seed helper deliberately does not
// set it, so the two cases below differ in exactly that one field.
func seedWithBudget(t *testing.T, h *reconcilerHarness, sessionID string, submitHeight int64, budget time.Duration, regime string) {
	t.Helper()
	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:       []byte(sessionID),
		SubmitHeight:   submitHeight,
		TimeoutSeconds: int64(budget / time.Second),
		TimeoutRegime:  regime,
	})
	require.NoError(t, err)
	require.NoError(t, h.store.Put(context.Background(), RebroadcastPhaseProof, hSupplier, hEnd, sessionID, b))
}

// A resend must carry the budget of the submission it replaces, not one of its
// own.
//
// This is the property the whole deadline change rests on: every attempt inside
// one window is born with the SAME number in front of it. The resend cannot
// derive that number -- it has no access to shared params, so it does not know
// how long the window is -- so the value travels with the persisted entry, and
// nothing but this test says it arrives.
//
// Losing it would be neither a crash nor a loss of money: a resend that fell
// back to the ceiling is still broadcast, and timeout_height still stops it at
// the window close. What would be lost is the invariant, silently, leaving it
// alive only in a comment.
func TestReconciler_ResendInheritsTheOriginalBudget(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	seedWithBudget(t, h, "s1", testSubmit, 100*time.Second, "window")

	h.r.OnBlock(testSubmit + 1) // past the one-block grace: the resend fires

	require.Equal(t, 1, h.resub.attemptCount(), "the resend must have been attempted once")

	h.resub.mu.Lock()
	gotTimeout, gotRegime := h.resub.lastTimeout, h.resub.lastRegime
	h.resub.mu.Unlock()

	require.Equal(t, 100*time.Second, gotTimeout,
		"the resend must be handed the budget stored on the entry, not one it derived")
	require.Equal(t, "window", gotRegime,
		"and the regime that produced it, so the metric does not report a fallback that never happened")
}

// The control, and it pins the other half: an entry with no stored budget --
// written before the field existed, or rewritten by an older binary that dropped
// what its struct could not see -- must reach the resubmitter as an ABSENT
// budget rather than as an invented one.
//
// Both halves are needed. Without the case above, a reconciler that always
// passed zero would pass; without this one, one that invented a plausible
// duration would pass, and the fallback below it could never tell "nothing was
// stored" from "100 seconds were stored".
//
// WHAT THIS TEST DOES NOT COVER, and the boundary is worth naming because the
// first version of it asserted across the line: the DEGRADATION itself --
// falling back to the chain ceiling under the "unknown" regime -- lives inside
// SupplierManager.ResubmitMessage, which is exactly what the harness replaces
// with a mock. So this file can prove what the reconciler HANDS OVER and nothing
// about what the real resubmitter does with it. Measured, not assumed: asserting
// a positive duration here fails with "0s", because zero is precisely what the
// reconciler passes and the fallback is on the other side of the seam.
func TestReconciler_ResendWithoutAStoredBudgetPassesItThroughAsAbsent(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit) // no budget on the entry

	h.r.OnBlock(testSubmit + 1)

	require.Equal(t, 1, h.resub.attemptCount())

	h.resub.mu.Lock()
	gotTimeout, gotRegime := h.resub.lastTimeout, h.resub.lastRegime
	h.resub.mu.Unlock()

	require.Zero(t, gotTimeout,
		"an entry with no stored budget must arrive as absent, so the resubmitter "+
			"can tell it apart from a real one and fall back audibly")
	require.Empty(t, gotRegime,
		"and with no regime either: inventing one here would report a fallback "+
			"that had not happened yet")
}
