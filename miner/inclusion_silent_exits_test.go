//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// dropped and abandoned read the counters this file exists to pin. They take
// the phase and cause explicitly because the whole point of the labels is that
// a reader can tell WHICH silent exit happened, not merely that one did.
func dropped(phase RebroadcastPhase, cause string) float64 {
	return testutil.ToFloat64(inclusionEntryDroppedTotal.WithLabelValues(string(phase), cause))
}

func clearFailed(phase RebroadcastPhase) float64 {
	return testutil.ToFloat64(inclusionClearFailedTotal.WithLabelValues(string(phase)))
}

func abandoned(phase RebroadcastPhase, cause string) float64 {
	return testutil.ToFloat64(inclusionGroupAbandonedTotal.WithLabelValues(string(phase), cause))
}

// TestCorruptEntry_DegradedModeIsClearedAndCounted is the one that was losing
// work rather than only visibility.
//
// The degraded branch met an undecodable entry with a BARE continue: no log, no
// clear, no outcome, no counter. Because it did not clear, the same unreadable
// payload was met again on every block for as long as the on-chain query kept
// failing with the window open, and nothing anywhere said so.
func TestCorruptEntry_DegradedModeIsClearedAndCounted(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	require.NoError(t, h.store.Put(context.Background(),
		RebroadcastPhaseProof, hSupplier, hEnd, "bad", []byte("not-json")))

	// Degraded mode: the query fails while the window is still open.
	h.onChainErr = fmt.Errorf("node down")
	before := dropped(RebroadcastPhaseProof, dropCauseCorrupt)

	h.r.OnBlock(testSubmit)

	require.Equal(t, before+1, dropped(RebroadcastPhaseProof, dropCauseCorrupt),
		"the drop must be counted under its own cause")
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd),
		"the entry must be CLEARED: leaving it is what made this repeat every block")
	require.Empty(t, h.getOutcomes(),
		"a corrupt entry has no ServiceID, so it cannot carry a normal outcome")
}

// TestCorruptEntry_NormalPathIsCounted pins the path that already logged and
// cleared. It was not losing work, but a Warn on a per-block path is not
// something an operator can alert on or count, so "never" and "every block"
// produced the same signal.
func TestCorruptEntry_NormalPathIsCounted(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	require.NoError(t, h.store.Put(context.Background(),
		RebroadcastPhaseProof, hSupplier, hEnd, "bad", []byte("not-json")))

	before := dropped(RebroadcastPhaseProof, dropCauseCorrupt)
	h.r.OnBlock(testMid)

	require.Equal(t, before+1, dropped(RebroadcastPhaseProof, dropCauseCorrupt))
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd))
}

// TestPassAbandoned_UnreadableIndexIsCountedByCause covers the WIDEST silent
// exit of the set, and it was found by this test failing.
//
// The test was first written against store.List, and it went red: with the
// store gone, ActiveGroups fails FIRST and runPass returns before a single
// group is reconciled, so List is never reached. That return abandoned every
// group of the phase at once with nothing but a log line — a pass-level
// abandonment that no one had enumerated, one level above the group-level ones.
//
// It asserts the CAUSE, not merely that some counter moved: a pass that could
// not read the index and a group whose payloads could not be listed mean
// different things to an operator, and one undifferentiated counter would pass
// a weaker test while telling nobody which to go look at.
func TestPassAbandoned_UnreadableIndexIsCountedByCause(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	beforeUnreadable := abandoned(RebroadcastPhaseProof, abandonCauseIndexUnreadable)
	beforeList := abandoned(RebroadcastPhaseProof, abandonCauseListFailed)

	require.NoError(t, h.rc.Close())
	h.r.OnBlock(testMid)

	require.Equal(t, beforeUnreadable+1, abandoned(RebroadcastPhaseProof, abandonCauseIndexUnreadable),
		"a phase whose index cannot be read abandons every group and must say so")
	require.Equal(t, beforeList, abandoned(RebroadcastPhaseProof, abandonCauseListFailed),
		"and must NOT be attributed to the per-group payload listing, which was never reached")
}

// TestActiveGroups_MalformedIndexMemberIsCounted covers the exit that lives
// outside reconcileGroup and is the widest of them all: a member the index
// cannot parse means that GROUP is never reconciled again, so every pending
// claim or proof under it silently stops being checked.
func TestActiveGroups_MalformedIndexMemberIsCounted(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	// A member with no separator at all: the supplier/sessionEnd split fails.
	require.NoError(t, h.rc.SAdd(context.Background(),
		h.rc.KB().RebroadcastIndexKey(string(RebroadcastPhaseProof)), "no-separator").Err())

	before := abandoned(RebroadcastPhaseProof, abandonCauseIndexMalformed)
	groups, err := h.store.ActiveGroups(context.Background(), RebroadcastPhaseProof)
	require.NoError(t, err)

	require.Equal(t, before+1, abandoned(RebroadcastPhaseProof, abandonCauseIndexMalformed),
		"skipping an index member abandons its whole group and must be counted")
	require.Len(t, groups, 1, "the well-formed group must still come back")
}

// TestClear_FailureIsCountedSoAReEmissionIsAttributable is the dual of "nobody
// is dropped without a verdict": nobody reports twice.
//
// clear only logged. When the delete fails the entry SURVIVES, so the next
// block reconciles it again and emits its outcome a second time — and in the
// claim phase that means a second reactivateClaimedSession whose idempotence
// nobody has verified. The counter does not prevent the duplicate; it makes it
// attributable, which is what this item can honestly deliver: exactly-once
// would need dedup state whose write has the same failure mode as the delete it
// is compensating for.
//
// clear is called directly rather than driven through a pass because a store
// that cannot delete cannot read its index either, so no pass ever reaches the
// delete — the same discovery that turned the group-abandonment test into a
// pass-abandonment one.
func TestClear_FailureIsCountedSoAReEmissionIsAttributable(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	before := clearFailed(RebroadcastPhaseProof)
	beforeCorrupt := dropped(RebroadcastPhaseProof, dropCauseCorrupt)
	require.NoError(t, h.rc.Close())

	// The entry is empty because this case is about the DELETE failing, and a
	// failed delete returns before the cache is consulted at all -- an entry that
	// survives is still a sibling of its own transaction, so its bytes must not
	// be freed.
	h.r.clear(context.Background(), RebroadcastPhaseProof,
		RebroadcastGroup{Supplier: hSupplier, SessionEnd: hEnd}, "s1", rebroadcastEntry{})

	require.Equal(t, before+1, clearFailed(RebroadcastPhaseProof),
		"a delete that failed leaves the entry, so the next block emits its outcome again")
	require.Equal(t, beforeCorrupt, dropped(RebroadcastPhaseProof, dropCauseCorrupt),
		"and it must not be attributed to a corrupt entry")
}

// TestSilentExitSeriesExistBeforeAnythingHappens pins the eager registration,
// which is the one part of this change that protects against a MISSING signal
// rather than a wrong one — and which nothing else here would notice the loss
// of.
//
// A counter child does not exist until its first increment, so without the
// init() a query for these returns no data before the first occurrence, and no
// data reads as "this never happens". That is not a hypothetical: poll_dropped
// was documented as an outcome in two files and never had an emitter, so
// anyone who went looking concluded that saturation does not occur.
//
// It counts SERIES, not values, and that is the point: the values are zero
// either way, so any assertion on them would pass just as well with the
// registration deleted. Emptying the init() loop leaves every other test in
// this file green.
func TestSilentExitSeriesExistBeforeAnythingHappens(t *testing.T) {
	phases := 2 // claim, proof

	require.Equal(t, phases*1, testutil.CollectAndCount(inclusionEntryDroppedTotal),
		"one series per phase per drop cause must exist before any entry is dropped")
	require.Equal(t, phases*1, testutil.CollectAndCount(inclusionClearFailedTotal),
		"one series per phase must exist before any delete fails")
	// Five causes: list_failed, params_failed, index_malformed, index_unreadable
	// and budget_exhausted. The number is written by hand ON PURPOSE — the other
	// side of the comparison is the live registry, so a cause added to the
	// constants and forgotten in the init() loop fails here instead of shipping
	// a series that only appears once the defect has already happened.
	require.Equal(t, phases*5, testutil.CollectAndCount(inclusionGroupAbandonedTotal),
		"one series per phase per abandonment cause must exist before any pass is abandoned")
}

// TestInclusionReadStateSeriesExistBeforeAnyProbe is C3's other half: the signal
// that says whether the post-inclusion read works must not itself be missing
// before the first read.
//
// If the state were only published once a read had happened, then "I have not
// looked yet" and "it works" would be the same absence of data — which is the
// exact way poll_dropped misled, one level further in: a signal that exists to
// reveal an absence, having an absence of its own.
//
// It counts SERIES, because the values are zero either way before a probe runs.
func TestInclusionReadStateSeriesExistBeforeAnyProbe(t *testing.T) {
	require.Equal(t, 3, testutil.CollectAndCount(inclusionReadState),
		"available, unavailable and unknown must all exist before anything is probed")

	require.Equal(t, 2*4, testutil.CollectAndCount(inclusionMissingCauseTotal),
		"every phase/cause pair must exist before the first missing session is classified")
}

// TestSetInclusionReadState_LeavesTheOtherStatesVisible pins that publishing one
// state does not delete the others: a state whose series vanishes cannot be told
// from one that was never registered.
func TestSetInclusionReadState_LeavesTheOtherStatesVisible(t *testing.T) {
	SetInclusionReadState(tx.InclusionReadUnavailable)
	t.Cleanup(func() { SetInclusionReadState(tx.InclusionReadUnknown) })

	require.Equal(t, 3, testutil.CollectAndCount(inclusionReadState))
	require.Equal(t, 1.0, testutil.ToFloat64(inclusionReadState.WithLabelValues(string(tx.InclusionReadUnavailable))))
	require.Equal(t, 0.0, testutil.ToFloat64(inclusionReadState.WithLabelValues(string(tx.InclusionReadAvailable))),
		"the state we are NOT in must read zero, not disappear")
}
