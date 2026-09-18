//go:build test

package miner

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// THE LEDGER IDENTITY
//
//	claimed = proved + lost + (unresolved_opened - unresolved_resolved)
//
// Money enters the book in exactly one place -- a claim transaction the chain
// accepted -- and leaves it through exactly one of three doors. Work that never
// entered the book is counted apart, in the forgone family, precisely so that it
// cannot be subtracted from a total it was never in.
//
// Measured before this existed: a run that settled 500 of 500 claims with zero
// expired and zero slash reported claimed 6742.81, proved 4277.10, lost 3034.77
// -- a residual of -569.06, 108.4%. The same session was counted proved AND
// lost, and claim-side failures were counted lost without ever having been
// counted claimed. These tests pin each rule that makes the residual zero.

// ledgerProbe reads every series a verdict can touch for one (supplier,
// service) pair, so a test can assert where money went AND where it did not.
// Asserting only the series you expect to move is how a change that writes to
// two doors at once passes.
type ledgerProbe struct {
	supplier, service string

	claimed, proved              float64
	lost, forgone                float64
	unresolvedOpen, unresolvedOK float64
	sessions                     float64
}

func readLedger(supplier, service, reason, phase string) ledgerProbe {
	return ledgerProbe{
		supplier: supplier,
		service:  service,

		claimed:  testutil.ToFloat64(upoktClaimedTotal.WithLabelValues(supplier, service)),
		proved:   testutil.ToFloat64(upoktProvedTotal.WithLabelValues(supplier, service)),
		lost:     testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, reason)),
		forgone:  testutil.ToFloat64(upoktForgoneTotal.WithLabelValues(supplier, service, reason)),
		sessions: testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, service, reason)),

		unresolvedOpen: testutil.ToFloat64(upoktUnresolvedOpenedTotal.WithLabelValues(supplier, service, phase)),
		unresolvedOK:   testutil.ToFloat64(upoktUnresolvedResolvedTotal.WithLabelValues(supplier, service, phase)),
	}
}

// relaysProbe is the workload view of the same thing. relays_lost_total read
// 3,034,769 on a run with no loss at all, so it gets its own assertions.
func relaysLost(supplier, service, reason string) float64 {
	return testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, service, reason))
}

func relaysForgone(supplier, service, reason string) float64 {
	return testutil.ToFloat64(relaysForgoneTotal.WithLabelValues(supplier, service, reason))
}

// ---------------------------------------------------------------------------
// The four verdicts
// ---------------------------------------------------------------------------

// A retryable claim submission failure is an ATTEMPT. It counts the session and
// NO money: the claim never reached the chain, so the money was never in the
// book, and the reconciler will either put it there (on_chain_found) or write it
// off (on_chain_missing). Putting uPOKT here is what made upokt_lost_total read
// 45% of revenue on a run that lost nothing.
func TestClaimTxError_Resolvable_CountsTheSessionAndNoMoney(t *testing.T) {
	const supplier, service = "pokt1ledger_claim_attempt", "svc-1"
	before := readLedger(supplier, service, "claim_tx_error", string(RebroadcastPhaseClaim))
	relaysLostBefore := relaysLost(supplier, service, "claim_tx_error")

	RecordClaimTxError(supplier, service, true, 9, 9_000_000)

	after := readLedger(supplier, service, "claim_tx_error", string(RebroadcastPhaseClaim))
	require.Equal(t, before.sessions+1, after.sessions,
		"the session must still be counted: an operator watching a window needs to see the failure")
	require.Equal(t, before.lost, after.lost, "a retryable attempt is not a loss")
	require.Equal(t, before.forgone, after.forgone,
		"nor is it forgone yet: the reconciler has not answered")
	require.Equal(t, before.unresolvedOpen, after.unresolvedOpen,
		"and it opens no balance: this money was never in the book, so there is nothing to hold")
	require.Equal(t, relaysLostBefore, relaysLost(supplier, service, "claim_tx_error"),
		"relays_lost_total is the series that read 3,034,769 on a run with no loss; an attempt must not touch it")
}

// With no rebroadcast store nothing will ever answer for this session, so the
// attempt is a verdict: the work is forgone now rather than silently.
func TestClaimTxError_NoResolver_IsForgoneNotAnAttempt(t *testing.T) {
	const supplier, service = "pokt1ledger_claim_noresolver", "svc-1"
	before := readLedger(supplier, service, "claim_tx_error", string(RebroadcastPhaseClaim))

	RecordClaimTxError(supplier, service, false, 4, 4_000_000)

	after := readLedger(supplier, service, "claim_tx_error", string(RebroadcastPhaseClaim))
	require.Equal(t, before.sessions+1, after.sessions)
	require.Equal(t, before.forgone+4, after.forgone,
		"no resolver exists, so this is final: count it where work that never entered the book is counted")
	require.Equal(t, before.lost, after.lost,
		"forgone and not lost: it was never in upokt_claimed_total, so subtracting it from claimed is wrong")
	require.Equal(t, before.unresolvedOpen, after.unresolvedOpen,
		"a balance nothing can close must never be opened: that is the signal this family exists to avoid faking")
}

// A retryable PROOF submission failure is different from the claim side in the
// one way that matters: the money IS in the book, because a proof is only
// attempted for a claim the chain accepted. So it cannot be an attempt with no
// money -- it has to leave through a named door, and `unresolved` is that door.
func TestProofTxError_Resolvable_OpensUnresolvedAndNotLoss(t *testing.T) {
	const supplier, service = "pokt1ledger_proof_unresolved", "svc-1"
	before := readLedger(supplier, service, "proof_tx_error", string(RebroadcastPhaseProof))
	relaysLostBefore := relaysLost(supplier, service, "proof_tx_error")

	RecordProofTxError(supplier, service, true, 6, 6_000_000)

	after := readLedger(supplier, service, "proof_tx_error", string(RebroadcastPhaseProof))
	require.Equal(t, before.sessions+1, after.sessions)
	require.Equal(t, before.unresolvedOpen+6, after.unresolvedOpen,
		"the money waits for the chain's answer instead of being declared lost by a submission that may yet land")
	require.Equal(t, before.lost, after.lost,
		"this is the write that made one session count lost and proved at once")
	require.Equal(t, before.unresolvedOK, after.unresolvedOK,
		"opening a balance must not also close it")
	require.Equal(t, relaysLostBefore, relaysLost(supplier, service, "proof_tx_error"),
		"and the workload view must not move either")
}

// No resolver means nothing will ever answer, and an unresolved balance that can
// never close is a worse lie than a loss. The money is in the book, so it is
// LOST here and not forgone.
func TestProofTxError_NoResolver_IsLostNotUnresolved(t *testing.T) {
	const supplier, service = "pokt1ledger_proof_noresolver", "svc-1"
	before := readLedger(supplier, service, "proof_tx_error", string(RebroadcastPhaseProof))

	RecordProofTxError(supplier, service, false, 5, 5_000_000)

	after := readLedger(supplier, service, "proof_tx_error", string(RebroadcastPhaseProof))
	require.Equal(t, before.sessions+1, after.sessions)
	require.Equal(t, before.lost+5, after.lost,
		"in the book and nothing will answer: that is a loss")
	require.Equal(t, before.unresolvedOpen, after.unresolvedOpen,
		"a balance nothing can close must never be opened")
	require.Equal(t, before.forgone, after.forgone,
		"the claim WAS accepted, so this money entered the book: it is not forgone")
}

// ---------------------------------------------------------------------------
// The window sweeps, and the tx hash that discriminates
// ---------------------------------------------------------------------------

// The claim tx hash is read off the snapshot, never inferred. Empty means no
// claim transaction ever went out, so this session was never in
// upokt_claimed_total: counting it lost is exactly what drove the residual
// negative. Non-empty means the claim was accepted, the money IS in the book,
// and a window closing on it now means it will never be proved.
func TestClaimWindowClosed_TheTxHashDecidesForgoneOrLost(t *testing.T) {
	t.Run("empty hash is forgone", func(t *testing.T) {
		const supplier, service = "pokt1ledger_cwc_forgone", "svc-1"
		before := readLedger(supplier, service, "claim_window_closed", string(RebroadcastPhaseClaim))
		relaysForgoneBefore := relaysForgone(supplier, service, "claim_window_closed")
		relaysLostBefore := relaysLost(supplier, service, "claim_window_closed")

		RecordClaimWindowClosed(supplier, service, "", 3, 3_000_000)

		after := readLedger(supplier, service, "claim_window_closed", string(RebroadcastPhaseClaim))
		require.Equal(t, before.sessions+1, after.sessions)
		require.Equal(t, before.forgone+3, after.forgone,
			"no claim reached the chain, so this work never entered the book")
		require.Equal(t, before.lost, after.lost,
			"counting it lost is what makes claimed - proved - lost - unresolved go negative")
		require.Equal(t, relaysForgoneBefore+3, relaysForgone(supplier, service, "claim_window_closed"),
			"the workload view follows the money view")
		require.Equal(t, relaysLostBefore, relaysLost(supplier, service, "claim_window_closed"),
			"and relays_lost_total stays untouched")
	})

	t.Run("non-empty hash is lost", func(t *testing.T) {
		const supplier, service = "pokt1ledger_cwc_lost", "svc-1"
		before := readLedger(supplier, service, "claim_window_closed", string(RebroadcastPhaseClaim))

		RecordClaimWindowClosed(supplier, service, "DEADBEEF", 3, 3_000_000)

		after := readLedger(supplier, service, "claim_window_closed", string(RebroadcastPhaseClaim))
		require.Equal(t, before.lost+3, after.lost,
			"the chain accepted this claim, so the money is in the book and this is a real loss")
		require.Equal(t, before.forgone, after.forgone)
	})
}

// The proof side reads ProofTxHash, and its non-empty case is NOT a loss for a
// reason that is easy to get backwards: a submitted proof was already counted
// into upokt_proved_total at submission, so counting anything here would be the
// session leaving the book through a second door. Measured 2026-09-17: 12
// proof_window_closed sessions whose proof was already on chain.
func TestProofWindowClosed_TheTxHashDecidesLostOrAttempt(t *testing.T) {
	t.Run("empty hash is lost", func(t *testing.T) {
		const supplier, service = "pokt1ledger_pwc_lost", "svc-1"
		before := readLedger(supplier, service, "proof_window_closed", string(RebroadcastPhaseProof))

		RecordProofWindowClosed(supplier, service, "", 8, 8_000_000)

		after := readLedger(supplier, service, "proof_window_closed", string(RebroadcastPhaseProof))
		require.Equal(t, before.sessions+1, after.sessions)
		require.Equal(t, before.lost+8, after.lost,
			"the claim was accepted and no proof ever went out: the book loses this")
		require.Equal(t, before.forgone, after.forgone,
			"it entered the book, so it is not forgone")
		require.Equal(t, before.unresolvedOpen, after.unresolvedOpen,
			"nothing is pending: no proof was ever submitted, so there is no answer to wait for")
	})

	t.Run("non-empty hash counts the session and no money", func(t *testing.T) {
		const supplier, service = "pokt1ledger_pwc_attempt", "svc-1"
		before := readLedger(supplier, service, "proof_window_closed", string(RebroadcastPhaseProof))

		RecordProofWindowClosed(supplier, service, "CAFEBABE", 8, 8_000_000)

		after := readLedger(supplier, service, "proof_window_closed", string(RebroadcastPhaseProof))
		require.Equal(t, before.sessions+1, after.sessions,
			"the operator still sees the window closed")
		require.Equal(t, before.lost, after.lost,
			"this proof was submitted, so upokt_proved_total already counted it; "+
				"counting it lost too is the double count that read 108.4%")
		require.Equal(t, before.unresolvedOpen, after.unresolvedOpen,
			"and it must not open a balance either, for the same reason: proved already took it")
		require.Equal(t, before.forgone, after.forgone)
	})
}

// A resolution closes a balance and NEVER counts a second failed session. A
// session fails once; its money can be named twice.
func TestUnresolvedResolution_TouchesNoSessionCounter(t *testing.T) {
	const supplier, service = "pokt1ledger_resolve_sessions", "svc-1"
	before := testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, service, "proof_tx_error"))
	closedBefore := testutil.ToFloat64(upoktUnresolvedResolvedTotal.WithLabelValues(supplier, service, string(RebroadcastPhaseProof)))

	RecordSessionUnresolvedResolved(supplier, service, string(RebroadcastPhaseProof), 2, 2_000_000)

	require.Equal(t, before,
		testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, service, "proof_tx_error")),
		"the session was already counted when the balance was opened; a session does not fail twice")
	require.Equal(t, closedBefore+2,
		testutil.ToFloat64(upoktUnresolvedResolvedTotal.WithLabelValues(supplier, service, string(RebroadcastPhaseProof))),
		"the balance itself must close, or the resolution did nothing at all")
}

// ---------------------------------------------------------------------------
// The identity itself
// ---------------------------------------------------------------------------

// The whole point, as one arithmetic statement, over the life of one session
// that fails its proof submission and is then found on chain by the reconciler.
//
// The criterion is the residual, not any single series: claimed - proved - lost
// - (opened - resolved) must be zero. Asserting the parts one at a time is how
// the -569 survived for so long -- each series looked defensible alone.
func TestMoneyLedgerCloses_OverAFailedProofThatLandsOnRetry(t *testing.T) {
	const supplier, service = "pokt1ledger_closes", "svc-1"
	const cu = 10_000_000
	const relays = 10

	residual := func() float64 {
		claimed := testutil.ToFloat64(upoktClaimedTotal.WithLabelValues(supplier, service))
		proved := testutil.ToFloat64(upoktProvedTotal.WithLabelValues(supplier, service))
		lost := testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, "proof_tx_error"))
		open := testutil.ToFloat64(upoktUnresolvedOpenedTotal.WithLabelValues(supplier, service, string(RebroadcastPhaseProof)))
		done := testutil.ToFloat64(upoktUnresolvedResolvedTotal.WithLabelValues(supplier, service, string(RebroadcastPhaseProof)))
		return claimed - proved - lost - (open - done)
	}

	lostBefore := testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, "proof_tx_error"))

	require.Zero(t, residual(), "an empty book closes")

	// The claim is accepted: the money enters the book.
	RecordRevenueClaimed(supplier, service, cu, relays)
	require.Equal(t, float64(10), residual(),
		"claimed and not yet settled: the residual IS the open balance, and that is the honest reading")

	// The proof submission fails, retryably. The money moves to `unresolved`.
	RecordProofTxError(supplier, service, true, relays, cu)
	require.Zero(t, residual(),
		"claimed = proved + lost + unresolved: the session is accounted for while its fate is unknown")

	// The reconciler finds the proof on chain: the balance closes into proved.
	RecordSessionUnresolvedResolved(supplier, service, string(RebroadcastPhaseProof), relays, cu)
	RecordRevenueProved(supplier, service, cu, relays)
	require.Zero(t, residual(),
		"and it still closes once the chain has answered -- this is the case that read 108.4%")

	require.Equal(t, lostBefore,
		testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, "proof_tx_error")),
		"nothing was lost: a retry that landed must leave no loss behind")
}

// The abort paths pass the SNAPSHOT'S hash through, and that argument is the
// whole discriminator: a site that passed a constant instead would give the same
// verdict to a session whose proof went out and one whose proof never did.
//
// Driven through markAndCountProofWindowClosed rather than the recorder, because
// the recorder is already pinned above -- what is unpinned is the call site
// reading the right field off the snapshot.
func TestMarkAndCountProofWindowClosed_ASubmittedProofIsNotCountedLost(t *testing.T) {
	const supplier, service = "pokt1ledger_mark_submitted", "svc-1"
	const sessionID = "sess-mark-submitted"
	f := newHandlerTestFixture(t, supplier)

	lc := &LifecycleCallback{
		logger:             logging.ForComponent(zerolog.Nop(), "lifecycle_callback_test"),
		smstManager:        f.smstMgr,
		sessionCoordinator: f.coordinator,
	}
	require.NoError(t, f.coordinator.OnSessionCreated(f.ctx, sessionID, supplier, service, "pokt1app", 1, 10))

	// The proof WAS submitted: upokt_proved_total already counted this session at
	// submission, so the window closing on it must not name a second exit.
	snapshot := &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: supplier,
		ServiceID:               service,
		RelayCount:              6,
		TotalComputeUnits:       6_000_000,
		ClaimTxHash:             "CLAIMOK",
		ProofTxHash:             "PROOFOK",
	}

	lostBefore := relaysLost(supplier, service, "proof_window_closed")
	sessionsBefore := testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, service, "proof_window_closed"))

	lc.markAndCountProofWindowClosed(f.ctx, snapshot)

	require.Equal(t, sessionsBefore+1,
		testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, service, "proof_window_closed")),
		"the operator still has to see that the window closed")
	require.Equal(t, lostBefore, relaysLost(supplier, service, "proof_window_closed"),
		"this call site must pass the snapshot's own ProofTxHash: a submitted proof was already "+
			"counted proved, and counting it lost here is the 108.4% double count")
}

// ---------------------------------------------------------------------------
// The handover: count AFTER the transition is written
// ---------------------------------------------------------------------------

// A session's money is counted inside the terminal callback. If the state
// transition that makes the session terminal is written AFTER that, a write
// failure leaves Redis saying `claimed` while this process has already counted
// the money -- and whoever takes the supplier over reads that state,
// loadExistingSessions accepts it because it is not terminal, its sweep reaches
// the same verdict, and the same money is counted a second time by a different
// process.
//
// The assertion is THE TOTAL, deliberately. "The old owner counted nothing" and
// "the new owner counted once" are two facts, and a fix that made NOBODY count
// would satisfy the first one alone.
func TestExecuteTransition_HandoverCountsTheMoneyExactlyOnce(t *testing.T) {
	const supplier, service = "pokt1ledger_handover", "svc-1"
	const sessionID = "sess-handover"
	const relays = int64(7)

	f := newHandlerTestFixture(t, supplier)

	lc := &LifecycleCallback{
		logger:      logging.ForComponent(zerolog.Nop(), "lifecycle_callback_test"),
		smstManager: f.smstMgr,
	}

	// The session is in the book: its claim was accepted, and no proof ever went
	// out. Its proof window closing is a real loss of real money.
	require.NoError(t, f.coordinator.OnSessionCreated(f.ctx, sessionID, supplier, service, "pokt1app", 1, 10))
	snapshot := &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: supplier,
		ServiceID:               service,
		State:                   SessionStateClaimed,
		RelayCount:              relays,
		TotalComputeUnits:       7_000_000,
		ClaimTxHash:             "ACCEPTED",
	}

	newManager := func() *SessionLifecycleManager {
		return &SessionLifecycleManager{
			logger:         logging.ForComponent(zerolog.Nop(), "lifecycle_manager_test"),
			config:         SessionLifecycleConfig{SupplierAddress: supplier},
			sessionStore:   f.sessionStore,
			callback:       lc,
			activeSessions: xsync.NewMap[string, *SessionSnapshot](),
		}
	}

	lostBefore := relaysLost(supplier, service, "proof_window_closed")

	// --- The old owner, with Redis refusing the state write. ---
	oldOwner := newManager()
	oldOwner.activeSessions.Store(sessionID, snapshot)

	f.failRedis.Fail("LOADING Redis is loading the dataset in memory")
	oldOwner.executeTransition(f.ctx, snapshot, SessionStateProofWindowClosed, "test_handover")
	f.failRedis.Clear()

	require.Equal(t, lostBefore, relaysLost(supplier, service, "proof_window_closed"),
		"the state did not reach Redis, so nothing may be counted: the session is still somebody's to settle")
	_, stillTracked := oldOwner.activeSessions.Load(sessionID)
	require.True(t, stillTracked,
		"and it must stay tracked, or this replica forgets a session it never settled")
	require.Equal(t, SessionStateClaimed, snapshot.State,
		"the in-memory state must not advance past a write that failed, or the next sweep skips it")

	// --- The new owner takes over and reads the session out of Redis. ---
	reloaded, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, reloaded, "the session is still in Redis for the next owner to find")
	require.False(t, reloaded.State.IsTerminal(),
		"non-terminal is what makes loadExistingSessions pick it up, which is why a premature count double counts")
	reloaded.RelayCount = relays
	reloaded.TotalComputeUnits = 7_000_000

	newOwner := newManager()
	newOwner.activeSessions.Store(sessionID, reloaded)
	newOwner.executeTransition(f.ctx, reloaded, SessionStateProofWindowClosed, "test_handover")

	require.Equal(t, lostBefore+float64(relays), relaysLost(supplier, service, "proof_window_closed"),
		"ACROSS BOTH OWNERS the money moved exactly one snapshot weight -- not zero, not twice")
	_, stillThere := newOwner.activeSessions.Load(sessionID)
	require.False(t, stillThere, "a settled terminal session leaves the tracking")
}

// A transition whose state write succeeds counts once, and this is the control
// for the test above: without it, a change that made executeTransition never
// count at all would leave that test green.
func TestExecuteTransition_CountsOnceWhenTheWriteTakes(t *testing.T) {
	const supplier, service = "pokt1ledger_transition_ok", "svc-1"
	const sessionID = "sess-transition-ok"

	f := newHandlerTestFixture(t, supplier)
	require.NoError(t, f.coordinator.OnSessionCreated(f.ctx, sessionID, supplier, service, "pokt1app", 1, 10))

	lc := &LifecycleCallback{
		logger:      logging.ForComponent(zerolog.Nop(), "lifecycle_callback_test"),
		smstManager: f.smstMgr,
	}
	mgr := &SessionLifecycleManager{
		logger:         logging.ForComponent(zerolog.Nop(), "lifecycle_manager_test"),
		config:         SessionLifecycleConfig{SupplierAddress: supplier},
		sessionStore:   f.sessionStore,
		callback:       lc,
		activeSessions: xsync.NewMap[string, *SessionSnapshot](),
	}

	snapshot := &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: supplier,
		ServiceID:               service,
		State:                   SessionStateClaimed,
		RelayCount:              5,
		TotalComputeUnits:       5_000_000,
		ClaimTxHash:             "ACCEPTED",
	}
	mgr.activeSessions.Store(sessionID, snapshot)

	before := relaysLost(supplier, service, "proof_window_closed")
	mgr.executeTransition(context.Background(), snapshot, SessionStateProofWindowClosed, "test_ok")

	require.Equal(t, before+5, relaysLost(supplier, service, "proof_window_closed"),
		"the write took, so the verdict is recorded exactly once")

	persisted, err := f.sessionStore.Get(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, SessionStateProofWindowClosed, persisted.State,
		"and the state that authorises the count is the one in Redis")
}
