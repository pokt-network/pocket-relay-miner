//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// provingWithProofSent is a session whose proof transaction the mempool
// accepted -- its hash is in the snapshot -- still in proving: what a process
// killed between the broadcast and the write of proved leaves in Redis.
func provingWithProofSent() *SessionSnapshot {
	s := claimedSnapshot(SessionStateProving)
	s.ProofTxHash = "PROOFTX"
	return s
}

func requireStateEventually(t *testing.T, store *RedisSessionStore, sessionID string, want SessionState, why string) {
	t.Helper()
	require.Eventually(t, func() bool {
		snap, err := store.Get(context.Background(), sessionID)
		return err == nil && snap != nil && snap.State == want
	}, 10*time.Second, 5*time.Millisecond, why)
}

// TestProvingWithProofSentIsBookedProvedAfterRestart is the 58 of 2026-09-22:
// after a kill -9, sessions in proving whose proof was already on chain stayed
// in proving forever. The lifecycle judged them proved at the proof window's
// close (11878e5) and its dispatcher had no batch for that verdict, so nothing
// ever wrote it. A new process on the same Redis, at the close, must book the
// session proved -- and must not send its proof again.
func TestProvingWithProofSentIsBookedProvedAfterRestart(t *testing.T) {
	proofClose := sharedtypes.GetProofWindowCloseHeight(resumeParams(t), resumeSessionEnd)
	m, store, cb := startAfterRestart(t, provingWithProofSent(), proofClose)
	t.Cleanup(func() { _ = m.Close() })

	requireStateEventually(t, store, "sess-resume", SessionStateProved,
		"a proof the mempool accepted is proved at the window's close, restart or not")
	// The state is persisted BEFORE the callbacks run (executeTransition), so
	// seeing proved in Redis does not mean the callback has run yet.
	require.Eventually(t, func() bool { return len(cb.provedSessions()) > 0 }, 10*time.Second, 5*time.Millisecond, "the proved callback runs")
	require.Equal(t, []string{"sess-resume"}, cb.provedSessions(), "and the proved callback runs, once")
	_, proofs := cb.sent()
	require.Empty(t, proofs, "the proof already sent is not sent again")
}

// TestProvingWithProofSentIsBookedProvedWithoutRestart shows the defect was not
// about restarting: a running manager that reaches the window's close with the
// same session drops the same verdict.
func TestProvingWithProofSentIsBookedProvedWithoutRestart(t *testing.T) {
	params := resumeParams(t)
	proofClose := sharedtypes.GetProofWindowCloseHeight(params, resumeSessionEnd)
	m, store, cb := startAfterRestart(t, provingWithProofSent(), proofClose-1)
	t.Cleanup(func() { _ = m.Close() })
	snap, err := store.Get(context.Background(), "sess-resume")
	require.NoError(t, err)
	require.Equal(t, SessionStateProving, snap.State, "premise: before the close it waits in proving")

	m.checkSessionTransitions(context.Background(), proofClose)

	requireStateEventually(t, store, "sess-resume", SessionStateProved, "at the close it is booked proved")
	// The state is persisted BEFORE the callbacks run (executeTransition), so
	// seeing proved in Redis does not mean the callback has run yet.
	require.Eventually(t, func() bool { return len(cb.provedSessions()) > 0 }, 10*time.Second, 5*time.Millisecond, "the proved callback runs")
	require.Equal(t, []string{"sess-resume"}, cb.provedSessions())
}

// TestEveryTransitionVerdictIsDispatched pins the class, not the instance: every
// state determineTransition can return for a session is carried out, so a verdict
// is never computed and dropped. Each case drives the real check pass and reads
// the state back from Redis; the counter of undispatched verdicts stays at zero.
func TestEveryTransitionVerdictIsDispatched(t *testing.T) {
	params := resumeParams(t)
	claimClose := sharedtypes.GetClaimWindowCloseHeight(params, resumeSessionEnd)
	proofClose := sharedtypes.GetProofWindowCloseHeight(params, resumeSessionEnd)
	proofSent := provingWithProofSent()
	proofNotSent := claimedSnapshot(SessionStateProving)
	claimingNotSent := resumeSnapshot(SessionStateClaiming)
	claimingNotSent.ClaimTxHash = "CLAIMTX"

	cases := []struct {
		name     string
		snapshot *SessionSnapshot
		height   int64
		want     SessionState
	}{
		{"proving, proof sent, at the close", proofSent, proofClose, SessionStateProved},
		// Loaded with no proof sent, it is resumed to claimed at startup; past
		// the proof window that verdict is the timeout.
		{"proving with no proof sent, past the proof window", proofNotSent, proofClose, SessionStateProofWindowClosed},
		{"claiming past the claim window", claimingNotSent, claimClose, SessionStateClaimWindowClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			undispatched := sessionTransitionsUndispatched.WithLabelValues(resumeSupplier, string(tc.want))
			before := testutil.ToFloat64(undispatched)
			m, store, _ := startAfterRestart(t, tc.snapshot, tc.height)
			t.Cleanup(func() { _ = m.Close() })

			requireStateEventually(t, store, tc.snapshot.SessionID, tc.want, "the verdict is carried out")
			require.Equal(t, before, testutil.ToFloat64(undispatched), "and nothing was left undispatched")
		})
	}
}
