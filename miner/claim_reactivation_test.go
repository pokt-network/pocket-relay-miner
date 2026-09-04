//go:build test

package miner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// The defect these tests pin, in one sentence: a claim whose broadcast reported
// failure can still LAND on-chain via the inclusion reconciler, and until this
// edge existed nobody told the lifecycle — so the session stayed terminal in
// claim_tx_error, the proof never went out, and the chain slashed a claim we
// had actually submitted. The reconciler turned a lost reward into a slash.
//
// Each test below injects ONE part of the fix. They are deliberately
// independent: if removing part A also reddens the test for part B, one of the
// two assertions is not proving what it claims to.

// reactivationTestRoot returns a well-formed SMST root. The length matters:
// decodeSnapshot drops a root that is not SMSTRootLen, so a short literal in a
// test would silently read back as nil and prove nothing.
func reactivationTestRoot(tag string) []byte {
	root := make([]byte, SMSTRootLen)
	copy(root, tag)
	return root
}

func reactivationTestSnapshot(sessionID string, state SessionState) *SessionSnapshot {
	return &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-test",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   state,
		RelayCount:              7,
		TotalComputeUnits:       70,
		CreatedAt:               time.Now(),
		LastUpdatedAt:           time.Now(),
	}
}

// PART 1 — the guard. ReactivateClaimed must accept exactly the states that are
// BEHIND `claimed` and refuse every state at or past it.
//
// The guard cannot be written as "not terminal": claim_tx_error IS terminal and
// is the whole point. And it cannot be written as "terminal only": a session
// still `active` or `claiming` whose claim landed is equally behind the chain.
//
// Injection that reddens this and nothing else: delete the state comparison
// chain from reactivateClaimedScript. Every "refuse" row then flips.
func TestReactivateClaimed_AcceptsOnlyStatesBehindClaimed(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		state     SessionState
		wantFlip  bool
		wantState SessionState
	}{
		// Behind `claimed` — the chain knows something the miner does not.
		{"claim_tx_error is the measured case", SessionStateClaimTxError, true, SessionStateClaimed},
		{"claim_missing: a later observation overrides an earlier one", SessionStateClaimMissing, true, SessionStateClaimed},
		{"claim_window_closed but the claim did land", SessionStateClaimWindowClosed, true, SessionStateClaimed},
		{"claiming: local state simply lagged", SessionStateClaiming, true, SessionStateClaimed},
		{"active: local state lagged further", SessionStateActive, true, SessionStateClaimed},

		// At or past `claimed` — writing `claimed` would be a REGRESSION.
		{"claimed is a no-op, not a rewrite", SessionStateClaimed, false, SessionStateClaimed},
		{"proving must not be pulled back", SessionStateProving, false, SessionStateProving},
		{"proved must not be pulled back", SessionStateProved, false, SessionStateProved},
		{"probabilistic_proved must not be pulled back", SessionStateProbabilisticProved, false, SessionStateProbabilisticProved},
		{"proof_tx_error is downstream", SessionStateProofTxError, false, SessionStateProofTxError},
		{"proof_window_closed is downstream and true", SessionStateProofWindowClosed, false, SessionStateProofWindowClosed},
		{"claim_skipped: we chose not to claim", SessionStateClaimSkipped, false, SessionStateClaimSkipped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := setupTestSessionStore(t)
			defer func() { _ = store.Close() }()

			sessionID := "session-" + string(tt.state)
			require.NoError(t, store.Save(ctx, reactivationTestSnapshot(sessionID, tt.state)))

			flipped, err := store.ReactivateClaimed(ctx, sessionID, reactivationTestRoot("guard"), "TXHASH")
			require.NoError(t, err)
			require.Equal(t, tt.wantFlip, flipped, "flip decision")

			got, err := store.Get(ctx, sessionID)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Equal(t, tt.wantState, got.State, "state after the call")

			if !tt.wantFlip {
				// A refusal must write NOTHING — not the fields, not a fresh
				// claim tx hash over whatever was there.
				require.Empty(t, got.ClaimTxHash, "a refused reactivation must not write the tx hash")
			}
		})
	}
}

// PART 2 — the fields. Reactivating without ClaimedRootHash produces a session
// that reaches proving and then FAILS there: resolveClaimedRoot falls back to
// the SMST and a rehydration miss ends in proof_tx_error. So the root must land
// in the SAME write as the state, and it must survive a later Save.
//
// Injection that reddens this and nothing else: drop 'claimed_root_hash' from
// the HSET in reactivateClaimedScript.
func TestReactivateClaimed_WritesClaimFieldsWithTheState(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestSessionStore(t)
	defer func() { _ = store.Close() }()

	const sessionID = "session-fields"
	root := reactivationTestRoot("accepted-by-the-chain")
	require.NoError(t, store.Save(ctx, reactivationTestSnapshot(sessionID, SessionStateClaimTxError)))

	flipped, err := store.ReactivateClaimed(ctx, sessionID, root, "TX-REBROADCAST")
	require.NoError(t, err)
	require.True(t, flipped)

	got, err := store.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, SessionStateClaimed, got.State)
	require.Equal(t, root, got.ClaimedRootHash, "the claimed root must be the one the chain accepted")
	require.Equal(t, "TX-REBROADCAST", got.ClaimTxHash)

	// The counters belong to the IncrementRelayCount script and must be
	// untouched: they are what the claim was built from.
	require.Equal(t, int64(7), got.RelayCount)
	require.Equal(t, uint64(70), got.TotalComputeUnits)

	// The per-state index must follow the hash, or `redis sessions --state`
	// and GetByState report a session that is no longer there.
	byState, err := store.GetByState(ctx, SessionStateClaimed)
	require.NoError(t, err)
	require.Len(t, byState, 1)
	require.Equal(t, sessionID, byState[0].SessionID)

	stale, err := store.GetByState(ctx, SessionStateClaimTxError)
	require.NoError(t, err)
	require.Empty(t, stale, "the old state index must not keep the session")
}

// PART 3 — the re-tracking, and the trap inside it.
//
// Writing Redis is not enough: the session was deleted from activeSessions when
// it went terminal, and loadExistingSessions runs ONCE from Start, so nothing
// brings it back inside a live process except the created-callback (wired to
// TrackSession).
//
// The trap: TrackSession saves the snapshot it is HANDED, and Save HDELs empty
// optional fields. Handing it a snapshot built from the observation rather than
// the stored one would erase the claim_tx_hash and claimed_root_hash that were
// just written — leaving a `claimed` session that cannot prove. This test reads
// the fields off the snapshot the callback receives, which is exactly where
// that mistake shows up.
//
// Injection that reddens this and nothing else: skip the createdCallback call
// in OnClaimObservedOnChain.
func TestOnClaimObservedOnChain_ReTracksWithTheClaimFieldsIntact(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestSessionStore(t)
	defer func() { _ = store.Close() }()

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()

	var tracked []*SessionSnapshot
	coord.SetOnSessionCreatedCallback(func(_ context.Context, snap *SessionSnapshot) error {
		tracked = append(tracked, snap)
		return nil
	})

	const sessionID = "session-retrack"
	root := reactivationTestRoot("must-survive-tracking")
	require.NoError(t, store.Save(ctx, reactivationTestSnapshot(sessionID, SessionStateClaimTxError)))

	require.NoError(t, coord.OnClaimObservedOnChain(ctx, sessionID, root, "TX-1"))

	require.Len(t, tracked, 1, "the session must be handed back to the lifecycle")
	require.Equal(t, SessionStateClaimed, tracked[0].State)
	require.Equal(t, root, tracked[0].ClaimedRootHash,
		"the re-tracked snapshot must carry the root, or the proof cannot be built")
	require.Equal(t, "TX-1", tracked[0].ClaimTxHash)

	// And the store must still hold them after the callback ran.
	got, err := store.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, root, got.ClaimedRootHash, "re-tracking must not erase what the CAS wrote")
}

// A repeated observation must not re-track a second time. This is not
// hypothetical: the reconciler records the outcome and clears the entry in two
// separate operations, so a failed clear re-delivers the same observation on
// the next block.
func TestOnClaimObservedOnChain_SecondObservationIsANoOp(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestSessionStore(t)
	defer func() { _ = store.Close() }()

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()

	calls := 0
	coord.SetOnSessionCreatedCallback(func(context.Context, *SessionSnapshot) error {
		calls++
		return nil
	})

	const sessionID = "session-twice"
	require.NoError(t, store.Save(ctx, reactivationTestSnapshot(sessionID, SessionStateClaimTxError)))

	root := reactivationTestRoot("observed-twice")
	require.NoError(t, coord.OnClaimObservedOnChain(ctx, sessionID, root, "TX-1"))
	require.NoError(t, coord.OnClaimObservedOnChain(ctx, sessionID, root, "TX-1"))

	require.Equal(t, 1, calls, "a re-delivered observation must not re-track the session again")
}

// Reactivating without a root produces a session that reaches proving and then
// dies there. Refusing is the honest outcome: the session stays terminal and
// visibly so, rather than becoming a `claimed` session that cannot prove.
func TestOnClaimObservedOnChain_RefusesWithoutARoot(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestSessionStore(t)
	defer func() { _ = store.Close() }()

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()

	const sessionID = "session-no-root"
	require.NoError(t, store.Save(ctx, reactivationTestSnapshot(sessionID, SessionStateClaimTxError)))

	err := coord.OnClaimObservedOnChain(ctx, sessionID, nil, "TX-1")
	require.Error(t, err)

	got, gErr := store.Get(ctx, sessionID)
	require.NoError(t, gErr)
	require.Equal(t, SessionStateClaimTxError, got.State, "a refusal must leave the state alone")
}

// PART 4 — what the reactivation buys. A session put back into `claimed` is
// picked up by the height-driven state machine, so it proves inside the window
// and is honestly recorded as failed after it. This is the "and then the
// lifecycle does the rest" claim, checked rather than asserted.
func TestReactivatedSession_IsDrivenByHeightNotByArrivalTime(t *testing.T) {
	m := &SessionLifecycleManager{logger: testLogger()}
	params := &sharedtypes.Params{
		NumBlocksPerSession:          10,
		ClaimWindowOpenOffsetBlocks:  1,
		ClaimWindowCloseOffsetBlocks: 4,
		ProofWindowOpenOffsetBlocks:  0,
		ProofWindowCloseOffsetBlocks: 4,
	}
	snapshot := reactivationTestSnapshot("session-driven", SessionStateClaimed)

	proofOpen := sharedtypes.GetProofWindowOpenHeight(params, snapshot.SessionEndHeight)
	proofClose := sharedtypes.GetProofWindowCloseHeight(params, snapshot.SessionEndHeight)

	state, action := m.determineTransition(snapshot, proofOpen, params)
	require.Equal(t, SessionStateProving, state, "inside the proof window the session must go prove")
	require.Equal(t, "proof_window_open", action)

	state, _ = m.determineTransition(snapshot, proofClose, params)
	require.Equal(t, SessionStateProofWindowClosed, state,
		"after the window it must terminate honestly, not stay immortal")
}

// The reactivation must be first-writer-wins under real concurrency, and that
// is load-bearing rather than stylistic.
//
// The easy argument is that it cannot happen: a supplier is owned through a
// SetNX lease (supplier_claimer.go), so one replica holds its coordinator. That
// argument has a hole, measured 2026-09-04 and filed as queue item 35: when a
// lease is STOLEN, renewAllClaims deletes the supplier from the claimer's own
// map and returns without calling onReleaseFn — the only path to
// removeSupplier — so the losing replica keeps it in m.suppliers, which is
// exactly the map the reconciler's ownership filter reads
// (supplier_manager.go:2557). In that state both replicas reconcile the same
// supplier and both can observe the same inclusion.
//
// A read-then-write guard (the shape OnClaimTxError and OnSessionClaimed use)
// would let both win and both submit a proof. The Lua CAS cannot: Redis runs
// one script at a time, so exactly one caller sees a state behind `claimed`.
func TestReactivateClaimed_ConcurrentObserversProduceExactlyOneFlip(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestSessionStore(t)
	defer func() { _ = store.Close() }()

	const sessionID = "session-concurrent"
	require.NoError(t, store.Save(ctx, reactivationTestSnapshot(sessionID, SessionStateClaimTxError)))

	const observers = 16
	root := reactivationTestRoot("one-winner-only")

	var flips atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < observers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			flipped, err := store.ReactivateClaimed(ctx, sessionID, root, "TX-RACE")
			if err == nil && flipped {
				flips.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, int32(1), flips.Load(),
		"exactly one observer may flip the session, or two replicas both submit a proof")

	got, err := store.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, SessionStateClaimed, got.State)
	require.Equal(t, root, got.ClaimedRootHash)
}
