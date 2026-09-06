//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// The twin of claim_cycle_lasterr_test.go / proof_cycle_lasterr_test.go, and
// the reason it exists is worth stating: those two pin that a batch which
// failed once and then SUCCEEDED is not a loss. Nothing pinned the half next to
// it -- that a batch which really fails IS reported. Measured by the
// supervision: adding `lastErr = nil` right after `lastErr = submitErr` in the
// FAILURE branch (the mistake of someone "clearing just in case") left the whole
// suite GREEN, while a group that exhausted its retries reported nothing at all:
// no loss counter, no terminal state, and no rebroadcast entry -- so not even
// the reconciler would rescue it. Silent, total loss, all tests passing.
//
// The discriminating case is not the one that motivated the fix.

// everyAttempt is a failure budget no retry loop can exhaust, which is what
// these tests need and what neither lasterr test can produce: theirs must
// succeed on a later try.
const everyAttempt = 1 << 30

func failureFixture(t *testing.T, sessionID string, state SessionState) (*LifecycleCallback, SessionStore, *RebroadcastStore, *SessionSnapshot) {
	t.Helper()

	store, redisClient := setupTestSessionStore(t)
	t.Cleanup(func() { _ = store.Close() })

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1failure"})
	t.Cleanup(func() { _ = coord.Close() })

	rebroadcast := NewRebroadcastStore(redisClient, time.Hour)

	snap := &SessionSnapshot{
		SessionID:               sessionID,
		SessionEndHeight:        100,
		SessionStartHeight:      81,
		SupplierOperatorAddress: "pokt1failure",
		ServiceID:               "svc",
		RelayCount:              10,
		TotalComputeUnits:       100,
		State:                   state,
		ClaimedRootHash:         make([]byte, SMSTRootLen),
	}
	if err := store.Save(context.Background(), snap); err != nil {
		t.Fatalf("seeding the session store: %v", err)
	}

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:       &defaultParamsShared{},
		smstManager:        provingSMST{},
		supplierClient:     &flakySupplier{failures: everyAttempt},
		serviceClient:      erroringService{},
		rebroadcastStore:   rebroadcast,
		sessionCoordinator: coord,
	}
	return lc, store, rebroadcast, snap
}

// assertReportedAsFailure is the shared body: the three facts a real failure
// must leave behind, checked by session IDENTITY rather than by position.
func assertReportedAsFailure(
	t *testing.T,
	err error,
	sessionStore SessionStore,
	rebroadcast *RebroadcastStore,
	phase RebroadcastPhase,
	sessionID string,
	wantState SessionState,
) {
	t.Helper()

	if err == nil {
		t.Errorf("every attempt failed, so the cycle must report an error")
	}

	if got := stateOf(t, sessionStore, sessionID); got != wantState {
		t.Errorf("a failed submission must leave %q terminal, got %q", wantState, got)
	}

	pending, listErr := rebroadcast.List(context.Background(), phase, "pokt1failure", 100)
	if listErr != nil {
		t.Fatalf("listing the rebroadcast store: %v", listErr)
	}
	raw, ok := pending[sessionID]
	if !ok {
		t.Fatalf(
			"a build-OK submit-FAILED message must be persisted for the reconciler; "+
				"without it nothing resends and the session is forfeited. stored = %v",
			keysOfBytes(pending),
		)
	}
	entry, decErr := unmarshalRebroadcastEntry(raw)
	if decErr != nil {
		t.Fatalf("stored entry will not decode: %v", decErr)
	}
	// Empty is the ORDER: "never broadcast, resend at SubmitHeight+1". Anything
	// else would make the reconciler wait for the window midpoint on a message
	// that was never transmitted.
	if entry.OrigTxHash != "" {
		t.Errorf("nothing was broadcast, so OrigTxHash must be empty, got %q", entry.OrigTxHash)
	}
}

func keysOfBytes(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestOnSessionsNeedClaim_ExhaustedRetriesAreReportedAsFailure(t *testing.T) {
	lc, sessionStore, rebroadcast, snap := failureFixture(t, "session-claimfail-000", SessionStateClaiming)
	blocks := &heightedBlocks{}
	blocks.currentHeight = 103 // inside the claim window (102..106) for end height 100
	lc.blockClient = blocks
	lc.config = LifecycleCallbackConfig{ClaimRetryAttempts: 2, ClaimRetryDelay: time.Millisecond}

	result, err := lc.OnSessionsNeedClaim(context.Background(), []*SessionSnapshot{snap})

	if result.IsClaimed(snap.SessionID) {
		t.Errorf("nothing reached the chain, so no session may be named claimed")
	}
	assertReportedAsFailure(t, err, sessionStore, rebroadcast, RebroadcastPhaseClaim, snap.SessionID, SessionStateClaimTxError)
}

// The proof twin. The supervision did NOT inject this side and assumed it
// matched; measuring it here is the point -- in this file the claim and proof
// cycles are twins by construction, and three separate defects have already
// been found in one and not the other.
func TestOnSessionsNeedProof_ExhaustedRetriesAreReportedAsFailure(t *testing.T) {
	lc, sessionStore, rebroadcast, snap := failureFixture(t, "session-prooffail-000", SessionStateProving)
	blocks := &heightedBlocks{}
	blocks.currentHeight = 108 // inside the proof window (106..110) for end height 100
	lc.blockClient = blocks
	lc.config = LifecycleCallbackConfig{ProofRetryAttempts: 2, ProofRetryDelay: time.Millisecond}

	_, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{snap})

	assertReportedAsFailure(t, err, sessionStore, rebroadcast, RebroadcastPhaseProof, snap.SessionID, SessionStateProofTxError)
}
