//go:build test

package miner

import (
	"context"
	"errors"
	"testing"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// A process killed after broadcasting a claim and before storing its hash leaves
// the session in claiming with no hash and no rebroadcast entry. The claim may be
// on chain. These tests pin that such a session is asked about on chain before it
// is booked claim_window_closed -- terminal, its tree deleted, while the chain
// waits for its proof -- and that an unanswered question never makes it terminal.

func claimOnChainRoot() []byte {
	root := make([]byte, SMSTRootLen)
	root[0] = 0x2a
	return root
}

// TestResumedUnsentClaimFoundOnChainIsBookedClaimed: loaded in claiming with no
// hash, resumed to active, and restarted at its claim window's close. Before this
// change the lifecycle booked it claim_window_closed without asking. Now the chain
// is asked, and a claim found there books the session claimed with that root.
func TestResumedUnsentClaimFoundOnChainIsBookedClaimed(t *testing.T) {
	claimClose := sharedtypes.GetClaimWindowCloseHeight(resumeParams(t), resumeSessionEnd)
	cb := newResumeCallback()
	cb.observeClaim = func(ctx context.Context, s *SessionSnapshot) (bool, error) {
		// What LifecycleCallback.ObserveClaimOnChain does when GetClaim finds it.
		return cb.store.ReactivateClaimed(ctx, s.SessionID, claimOnChainRoot(), s.ClaimTxHash)
	}
	snapshot := resumeSnapshot(SessionStateClaiming)
	m, store, _ := startAfterRestartWith(t, cb, snapshot, claimClose)
	t.Cleanup(func() { _ = m.Close() })

	requireStateEventually(t, store, snapshot.SessionID, SessionStateClaimed,
		"a claim found on chain books the session claimed, not claim_window_closed")
	got, err := store.Get(context.Background(), snapshot.SessionID)
	require.NoError(t, err)
	require.Equal(t, claimOnChainRoot(), got.ClaimedRootHash, "with the root the chain holds")
	require.Contains(t, cb.observedSessions(), snapshot.SessionID, "the chain was asked")
}

// TestResumedUnsentClaimNotOnChainIsBookedClosed is the control: the same session,
// with no claim on chain, is booked claim_window_closed as before -- after asking.
func TestResumedUnsentClaimNotOnChainIsBookedClosed(t *testing.T) {
	claimClose := sharedtypes.GetClaimWindowCloseHeight(resumeParams(t), resumeSessionEnd)
	cb := newResumeCallback()
	snapshot := resumeSnapshot(SessionStateClaiming)
	m, store, _ := startAfterRestartWith(t, cb, snapshot, claimClose)
	t.Cleanup(func() { _ = m.Close() })

	requireStateEventually(t, store, snapshot.SessionID, SessionStateClaimWindowClosed, "no claim on chain: closed, as before")
	require.Contains(t, cb.observedSessions(), snapshot.SessionID, "but only after asking")
}

// TestResumedUnsentClaimUnansweredStaysUnbooked: when the chain cannot answer, the
// session is not made terminal. It stays as loaded and is asked again next pass.
func TestResumedUnsentClaimUnansweredStaysUnbooked(t *testing.T) {
	claimClose := sharedtypes.GetClaimWindowCloseHeight(resumeParams(t), resumeSessionEnd)
	cb := newResumeCallback()
	cb.observeClaim = func(context.Context, *SessionSnapshot) (bool, error) {
		return false, errors.New("node unreachable")
	}
	snapshot := resumeSnapshot(SessionStateClaiming)
	m, store, _ := startAfterRestartWith(t, cb, snapshot, claimClose)
	t.Cleanup(func() { _ = m.Close() })

	m.checkSessionTransitions(context.Background(), claimClose+1)
	got, err := store.Get(context.Background(), snapshot.SessionID)
	require.NoError(t, err)
	require.Equal(t, SessionStateActive, got.State, "an unanswered question never makes the session terminal")
	require.GreaterOrEqual(t, len(cb.observedSessions()), 2, "and it is asked again on the next pass")
}

// TestObserveClaimOnChain drives the production observer against a real session
// store: found books the session claimed with the chain's root, NotFound is an
// answer, and any other error is returned without touching the session. The
// callback's own window-closed marking asks first and follows the same answers.
func TestObserveClaimOnChain(t *testing.T) {
	cases := []struct {
		name      string
		getClaim  func() (pocktclient.Claim, error)
		wantFound bool
		wantErr   bool
		wantState SessionState
	}{
		{"found", func() (pocktclient.Claim, error) {
			return &prooftypes.Claim{RootHash: claimOnChainRoot()}, nil
		}, true, false, SessionStateClaimed},
		{"not found", func() (pocktclient.Claim, error) {
			return nil, status.Error(codes.NotFound, "claim not found")
		}, false, false, SessionStateActive},
		{"unreachable", func() (pocktclient.Claim, error) {
			return nil, status.Error(codes.Unavailable, "connection refused")
		}, false, true, SessionStateActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			coord, store, _ := setupTestCoordinator(t)
			snapshot := &SessionSnapshot{
				SessionID: "sess-observe", SupplierOperatorAddress: "pokt1test", ServiceID: "svc",
				SessionStartHeight: 91, SessionEndHeight: 100, State: SessionStateActive,
			}
			require.NoError(t, store.Save(ctx, snapshot))
			lc := &LifecycleCallback{
				logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
				config:             DefaultLifecycleCallbackConfig(),
				sessionCoordinator: coord,
				proofQueryClient: &stubProofQueryClient{getClaimFn: func(context.Context, string, string) (pocktclient.Claim, error) {
					return tc.getClaim()
				}},
			}

			found, err := lc.ObserveClaimOnChain(ctx, snapshot)

			require.Equal(t, tc.wantFound, found)
			require.Equal(t, tc.wantErr, err != nil, "err=%v", err)
			got, getErr := store.Get(ctx, snapshot.SessionID)
			require.NoError(t, getErr)
			require.Equal(t, tc.wantState, got.State)
			if tc.wantFound {
				require.Equal(t, claimOnChainRoot(), got.ClaimedRootHash)
			}

			lc.markAndCountClaimWindowClosed(ctx, snapshot)
			got, getErr = store.Get(ctx, snapshot.SessionID)
			require.NoError(t, getErr)
			if tc.wantFound || tc.wantErr {
				require.NotEqual(t, SessionStateClaimWindowClosed, got.State, "found or unanswered: not booked closed")
			} else {
				require.Equal(t, SessionStateClaimWindowClosed, got.State, "control: no claim on chain is booked closed")
			}
		})
	}
}
