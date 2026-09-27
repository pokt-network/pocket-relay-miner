//go:build test

package miner

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// The reconciler side of the ledger: who closes a balance, and who must not.
// These pin settleLedgerOutcome, which is the only place a balance CLOSES.

// ledgerSession puts a session in Redis with a known weight and hands back the
// SupplierManager whose ownership map settleLedgerOutcome reads.
func ledgerSession(t *testing.T, f *handlerTestFixture, sessionID, service string, relays int64, cu uint64) *SupplierManager {
	t.Helper()
	require.NoError(t, f.sessionStore.Save(f.ctx, &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: f.supplierAddr,
		ServiceID:               service,
		State:                   SessionStateClaimed,
		RelayCount:              relays,
		TotalComputeUnits:       cu,
	}))
	return f.worker.supplierManager
}

func upoktProved(supplier, service string) float64 {
	return testutil.ToFloat64(upoktProvedTotal.WithLabelValues(supplier, service))
}

func upoktClaimed(supplier, service string) float64 {
	return testutil.ToFloat64(upoktClaimedTotal.WithLabelValues(supplier, service))
}

func unresolvedClosed(supplier, service, phase string) float64 {
	return testutil.ToFloat64(upoktUnresolvedResolvedTotal.WithLabelValues(supplier, service, phase))
}

func unresolvedOpen(supplier, service, phase string) float64 {
	return testutil.ToFloat64(upoktUnresolvedOpenedTotal.WithLabelValues(supplier, service, phase))
}

// A proof the chain holds closes the unresolved balance AND credits `proved`.
// Both halves matter: closing without crediting makes the money vanish from the
// book, crediting without closing counts it twice.
func TestSettleLedgerOutcome_ProofFound_ClosesTheBalanceAndCreditsProved(t *testing.T) {
	const service = "svc-1"
	const phase = string(RebroadcastPhaseProof)
	f := newHandlerTestFixture(t, "pokt1ledger_rec_found")
	supplier := f.supplierAddr
	mgr := ledgerSession(t, f, "sess-rec-found", service, 6, 6_000_000)

	openBefore := unresolvedOpen(supplier, service, phase)
	doneBefore := unresolvedClosed(supplier, service, phase)
	provedBefore := upoktProved(supplier, service)

	// OrigTxHash empty: the submission never confirmed, which is exactly the
	// case whose revenue was not counted at submission time.
	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseProof, rebroadcastEntry{ServiceID: service},
		supplier, "sess-rec-found", inclusionFound)

	require.Equal(t, doneBefore+6, unresolvedClosed(supplier, service, phase),
		"the pending balance must come down, or it stays up forever and reads as a permanent gap")
	require.Equal(t, provedBefore+6, upoktProved(supplier, service),
		"and the money has to land somewhere: the chain holds this proof")
	require.Equal(t, openBefore, unresolvedOpen(supplier, service, phase),
		"resolving must never re-open")
}

// A proof the chain executed and REFUSED closes the balance into `lost`. Without
// this branch a rejected proof's balance never comes down, and a series whose
// job is to reveal a gap acquires a gap of its own.
func TestSettleLedgerOutcome_ProofRejected_ClosesTheBalanceIntoLost(t *testing.T) {
	const service = "svc-1"
	const phase = string(RebroadcastPhaseProof)
	f := newHandlerTestFixture(t, "pokt1ledger_rec_rejected")
	supplier := f.supplierAddr
	mgr := ledgerSession(t, f, "sess-rec-rejected", service, 4, 4_000_000)

	doneBefore := unresolvedClosed(supplier, service, phase)
	lostBefore := testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, inclusionRejected))
	provedBefore := upoktProved(supplier, service)

	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseProof, rebroadcastEntry{ServiceID: service},
		supplier, "sess-rec-rejected", inclusionRejected)

	require.Equal(t, doneBefore+4, unresolvedClosed(supplier, service, phase),
		"a rejection IS an answer, so the balance closes")
	require.Equal(t, lostBefore+4,
		testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, inclusionRejected)),
		"the chain refused it: this money was in the book and it is gone")
	require.Equal(t, provedBefore, upoktProved(supplier, service),
		"a refused proof is not a proved one")
}

// THE QUALIFIER THAT KEEPS THE NORMAL PATH HONEST.
//
// The SUCCESS paths persist a rebroadcast entry too, so on_chain_found also fires
// for sessions whose revenue was already counted at submission. Those entries
// carry a non-empty OrigTxHash. Without testing it, the reconciler would count
// every normal claim and every normal proof a second time -- which is the same
// defect this whole change removes, reintroduced by its own fix.
func TestSettleLedgerOutcome_AConfirmedSubmissionIsNotCountedAgain(t *testing.T) {
	const service = "svc-1"
	const phase = string(RebroadcastPhaseProof)
	f := newHandlerTestFixture(t, "pokt1ledger_rec_confirmed")
	supplier := f.supplierAddr
	mgr := ledgerSession(t, f, "sess-rec-confirmed", service, 9, 9_000_000)

	provedBefore := upoktProved(supplier, service)
	claimedBefore := upoktClaimed(supplier, service)
	doneBefore := unresolvedClosed(supplier, service, phase)

	confirmed := rebroadcastEntry{ServiceID: service, TxHash: "ABC123", OrigTxHash: "ABC123"}
	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseProof, confirmed, supplier, "sess-rec-confirmed", inclusionFound)
	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseClaim, confirmed, supplier, "sess-rec-confirmed", inclusionFound)

	require.Equal(t, provedBefore, upoktProved(supplier, service),
		"this proof was accepted on submit and already counted proved; counting it again doubles it")
	require.Equal(t, claimedBefore, upoktClaimed(supplier, service),
		"same on the claim side: a confirmed claim was already counted at submission")
	require.Equal(t, doneBefore, unresolvedClosed(supplier, service, phase),
		"and no balance was ever opened for it, so there is none to close")
}

// THE MISSING WRITE ON THE RECOVERY PATH.
//
// A claim whose submission never confirmed was never counted into `claimed`, yet
// the reconciler reactivates it and the session goes on to be proved -- crediting
// `proved` revenue the book never recorded as claimed. That asymmetry is the
// other half of the measured residual, and this is where it is repaired: if the
// chain HOLDS the claim, "uPOKT claimed" is literally true.
func TestSettleLedgerOutcome_ClaimFoundOnRecovery_CreditsClaimed(t *testing.T) {
	const service = "svc-1"
	f := newHandlerTestFixture(t, "pokt1ledger_rec_claim")
	supplier := f.supplierAddr
	mgr := ledgerSession(t, f, "sess-rec-claim", service, 7, 7_000_000)

	claimedBefore := upoktClaimed(supplier, service)
	forgoneBefore := testutil.ToFloat64(upoktForgoneTotal.WithLabelValues(supplier, service, inclusionFound))

	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseClaim, rebroadcastEntry{ServiceID: service},
		supplier, "sess-rec-claim", inclusionFound)

	require.Equal(t, claimedBefore+7, upoktClaimed(supplier, service),
		"the chain holds the claim, so the book has to record it as claimed before anything proves it")
	require.Equal(t, forgoneBefore,
		testutil.ToFloat64(upoktForgoneTotal.WithLabelValues(supplier, service, inclusionFound)),
		"a claim that landed is not forgone")
}

// A claim the chain does not hold, whose submission never confirmed, never
// entered the book. It is FORGONE and not lost: `lost` is subtracted from
// `claimed`, and this money was never in `claimed`.
func TestSettleLedgerOutcome_ClaimMissing_IsForgoneNotLost(t *testing.T) {
	const service = "svc-1"
	f := newHandlerTestFixture(t, "pokt1ledger_rec_claimmissing")
	supplier := f.supplierAddr
	mgr := ledgerSession(t, f, "sess-rec-claimmissing", service, 3, 3_000_000)

	forgoneBefore := testutil.ToFloat64(upoktForgoneTotal.WithLabelValues(supplier, service, inclusionMissing))
	lostBefore := testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, inclusionMissing))
	claimedBefore := upoktClaimed(supplier, service)

	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseClaim, rebroadcastEntry{ServiceID: service},
		supplier, "sess-rec-claimmissing", inclusionMissing)

	require.Equal(t, forgoneBefore+3,
		testutil.ToFloat64(upoktForgoneTotal.WithLabelValues(supplier, service, inclusionMissing)),
		"work served whose claim never landed must stay visible somewhere")
	require.Equal(t, lostBefore,
		testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, inclusionMissing)),
		"never in the book, so never subtractable from claimed")
	require.Equal(t, claimedBefore, upoktClaimed(supplier, service))
}

// ONLY THE REPLICA THAT OWNS A SUPPLIER REPORTS ITS ACCOUNTING. The predicate is
// the same m.suppliers.Load that the reconciler's own ownership filter uses -- a
// second predicate answering the same question is how two of them drift apart --
// and without it two replicas would report the same money.
func TestSettleLedgerOutcome_AnUnownedSupplierIsNotRecorded(t *testing.T) {
	const service = "svc-1"
	const phase = string(RebroadcastPhaseProof)
	f := newHandlerTestFixture(t, "pokt1ledger_rec_owner")
	mgr := ledgerSession(t, f, "sess-rec-unowned", service, 5, 5_000_000)

	const foreign = "pokt1ledger_rec_not_ours"
	doneBefore := unresolvedClosed(foreign, service, phase)
	provedBefore := upoktProved(foreign, service)

	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseProof, rebroadcastEntry{ServiceID: service},
		foreign, "sess-rec-unowned", inclusionFound)

	require.Equal(t, doneBefore, unresolvedClosed(foreign, service, phase),
		"this replica does not hold that supplier, so it is not its accounting to report")
	require.Equal(t, provedBefore, upoktProved(foreign, service))
}

// poll_error is not an answer. The reconciler keeps the entry and asks again next
// block, so moving the ledger here would settle a session once per failed poll.
func TestSettleLedgerOutcome_PollErrorIsNotAnAnswer(t *testing.T) {
	const service = "svc-1"
	const phase = string(RebroadcastPhaseProof)
	f := newHandlerTestFixture(t, "pokt1ledger_rec_pollerr")
	supplier := f.supplierAddr
	mgr := ledgerSession(t, f, "sess-rec-pollerr", service, 5, 5_000_000)

	doneBefore := unresolvedClosed(supplier, service, phase)

	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseProof, rebroadcastEntry{ServiceID: service},
		supplier, "sess-rec-pollerr", inclusionPollErr)
	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseProof, rebroadcastEntry{ServiceID: service},
		supplier, "sess-rec-pollerr", inclusionPollErr)

	require.Equal(t, doneBefore, unresolvedClosed(supplier, service, phase),
		"two failed polls must not settle the session twice, nor once")
}

// A session whose snapshot is gone cannot be weighed, and inventing a weight is
// worse than the absence: the unresolved balance stays open, which is exactly the
// visible signal this family exists to provide.
func TestSettleLedgerOutcome_NoSnapshotLeavesTheBalanceOpen(t *testing.T) {
	const service = "svc-1"
	const phase = string(RebroadcastPhaseProof)
	f := newHandlerTestFixture(t, "pokt1ledger_rec_nosnap")
	supplier := f.supplierAddr
	mgr := ledgerSession(t, f, "sess-rec-present", service, 5, 5_000_000)

	doneBefore := unresolvedClosed(supplier, service, phase)
	provedBefore := upoktProved(supplier, service)

	mgr.settleLedgerOutcome(f.ctx, RebroadcastPhaseProof, rebroadcastEntry{ServiceID: service},
		supplier, "sess-rec-absent", inclusionFound)

	require.Equal(t, doneBefore, unresolvedClosed(supplier, service, phase),
		"no snapshot, no weight: closing a balance by a guessed amount is worse than leaving it open")
	require.Equal(t, provedBefore, upoktProved(supplier, service))
}
