//go:build test

package miner

import (
	"context"
	"errors"
	"testing"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// Two writers settle a session that reached claiming with no hash: the
// lifecycle, on a "no claim" chain read, books it claim_window_closed and
// deletes its tree; the inclusion reconciler, on seeing the claim included,
// books it claimed. A claim can still land IN block close, so a read at close
// could say "no claim" and be overtaken -- the claimed session was then
// stomped back to a failure and lost its tree.

func TestLuaHoldsClaimOnChainMatchesGo(t *testing.T) {
	client, _ := newTestRedis(t)
	states := []SessionState{
		SessionStateActive, SessionStateClaiming, SessionStateClaimed,
		SessionStateClaimWindowClosed, SessionStateClaimTxError, SessionStateClaimMissing,
		SessionStateClaimSkipped, SessionStateProving, SessionStateProved,
		SessionStateProbabilisticProved, SessionStateProofWindowClosed, SessionStateProofTxError,
	}
	for _, s := range states {
		got, err := client.Eval(context.Background(),
			luaHoldsClaimOnChain+"\nreturn holds_claim_on_chain(ARGV[1]) and 1 or 0", nil, string(s)).Int64()
		require.NoError(t, err)
		require.Equalf(t, s.HoldsClaimOnChain(), got == 1, "state %q: Lua and Go disagree", s)
	}
}

func TestUpdateState_AClaimPhaseFailureNeverUndoesAClaim(t *testing.T) {
	client, _ := newTestRedis(t)
	store := NewRedisSessionStore(testLogger(), client, SessionStoreConfig{SupplierAddress: resumeSupplier})
	ctx := context.Background()

	for _, held := range []SessionState{SessionStateClaimed, SessionStateProving, SessionStateProved} {
		for _, failure := range []SessionState{SessionStateClaimWindowClosed, SessionStateClaimTxError} {
			s := claimedSnapshot(held)
			s.SessionID = "sess-" + string(held) + "-" + string(failure)
			require.NoError(t, store.Save(ctx, s))

			err := store.UpdateState(ctx, s.SessionID, failure)

			require.Truef(t, errors.Is(err, ErrClaimAlreadyOnChain), "%s over %s: err=%v", failure, held, err)
			got, gErr := store.Get(ctx, s.SessionID)
			require.NoError(t, gErr)
			require.Equalf(t, held, got.State, "%s over %s: nothing written", failure, held)
		}
	}

	// Controls: the guard refuses only claim-phase failures over a held claim.
	claiming := resumeSnapshot(SessionStateClaiming)
	claiming.SessionID = "sess-claiming"
	require.NoError(t, store.Save(ctx, claiming))
	require.NoError(t, store.UpdateState(ctx, claiming.SessionID, SessionStateClaimWindowClosed), "a claiming session is still booked closed")

	missing := claimedSnapshot(SessionStateClaimed)
	missing.SessionID = "sess-claimed-missing"
	require.NoError(t, store.Save(ctx, missing))
	require.NoError(t, store.UpdateState(ctx, missing.SessionID, SessionStateClaimMissing),
		"claimed -> claim_missing is the reconciler's own verdict and stays allowed")
}

// A resumed claiming session read "no claim" AT close is not booked: from
// close+1 it is.
func TestSweep_ANoClaimReadAtCloseIsNotFinal(t *testing.T) {
	claimClose := sharedtypes.GetClaimWindowCloseHeight(resumeParams(t), resumeSessionEnd)
	m, store, cb := startAfterRestart(t, resumeSnapshot(SessionStateClaiming), claimClose-1)
	t.Cleanup(func() { _ = m.Close() })

	m.checkSessionTransitions(context.Background(), claimClose)
	m.transitionSubpool.StopAndWait()
	got, err := store.Get(context.Background(), "sess-resume")
	require.NoError(t, err)
	require.NotEqual(t, SessionStateClaimWindowClosed, got.State, "at close a claim can still land: not booked")
	require.Empty(t, cb.observedSessions(), "and the chain is not asked a question whose answer is not final")
}

func TestSweep_ANoClaimReadAfterCloseIsBooked(t *testing.T) {
	claimClose := sharedtypes.GetClaimWindowCloseHeight(resumeParams(t), resumeSessionEnd)
	m, store, cb := startAfterRestart(t, resumeSnapshot(SessionStateClaiming), claimClose-1)
	t.Cleanup(func() { _ = m.Close() })

	m.checkSessionTransitions(context.Background(), claimClose+1)
	requireStateEventually(t, store, "sess-resume", SessionStateClaimWindowClosed, "control: at close+1 the read is final")
	require.Equal(t, []string{"sess-resume"}, cb.observedSessions())
	m.transitionSubpool.StopAndWait()
	_, stillMarked := m.resumedUnsentClaims.Load("sess-resume")
	require.False(t, stillMarked, "a settled session leaves the resumed-claims set with it")
}

// The reconciler books the claim between the lifecycle's "no claim" read and
// its terminal write. The write must be refused, not stomp the claim.
func TestSweep_AClaimBookedAfterTheReadIsNotUndone(t *testing.T) {
	claimClose := sharedtypes.GetClaimWindowCloseHeight(resumeParams(t), resumeSessionEnd)
	cb := newResumeCallback()
	cb.observeClaim = func(ctx context.Context, snapshot *SessionSnapshot) (bool, error) {
		// The reconciler lands in between: same Redis, same session.
		booked, err := cb.store.ReactivateClaimed(ctx, snapshot.SessionID, make([]byte, 48), "CLAIMTX")
		if err != nil || !booked {
			return false, errors.New("premise: the reconciler could not book the session claimed")
		}
		return false, nil
	}
	m, store, _ := startAfterRestartWith(t, cb, resumeSnapshot(SessionStateClaiming), claimClose-1)
	t.Cleanup(func() { _ = m.Close() })

	m.checkSessionTransitions(context.Background(), claimClose+1)
	m.transitionSubpool.StopAndWait()

	require.Equal(t, []string{"sess-resume"}, cb.observedSessions(), "premise: the chain was asked")
	got, err := store.Get(context.Background(), "sess-resume")
	require.NoError(t, err)
	require.Equal(t, SessionStateClaimed, got.State, "the claim the reconciler booked stands")
}
