//go:build test

package miner

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func windowClosedProbe(service, from string) (sessions, upokt float64) {
	reason := string(SessionStateClaimWindowClosed)
	return testutil.ToFloat64(sessionsReinstatedTotal.WithLabelValues("pokt1test", service, reason)),
		testutil.ToFloat64(upoktReinstatedTotal.WithLabelValues("pokt1test", service, reason, from))
}

func seedWindowClosed(t *testing.T, store SessionStore, id, service, claimTxHash string) {
	t.Helper()
	created, err := store.CreateIfAbsent(context.Background(), &SessionSnapshot{
		SessionID: id, SupplierOperatorAddress: "pokt1test", ServiceID: service,
		SessionStartHeight: 100, SessionEndHeight: 110, State: SessionStateClaimWindowClosed,
		ClaimTxHash: claimTxHash, RelayCount: 3, TotalComputeUnits: 3_000_000,
	})
	require.NoError(t, err)
	require.True(t, created)
}

// A claim_window_closed session was counted forgone (no claim tx hash, the
// path every caller takes). When the inclusion reconciler later finds its claim
// and reactivates it, the reconciler credits claimed and the session is proved:
// the forgone it was counted in is taken back, once.
func TestClaimWindowClosed_AReactivatedForgoneSessionIsTakenBackOnce(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedWindowClosed(t, store, "sess-cwc", service, "")
		sessions, upokt := windowClosedProbe(service, ClaimMissingForgone)

		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-cwc", make([]byte, SMSTRootLen), "TXFOUND"))
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-cwc", make([]byte, SMSTRootLen), "TXFOUND"))

		gotSessions, gotUpokt := windowClosedProbe(service, ClaimMissingForgone)
		require.Equal(t, sessions+1, gotSessions, "one session taken back, once")
		require.InDelta(t, upokt+3, gotUpokt, 1e-9, "its forgone money, at its price, taken back")
		_, lost := windowClosedProbe(service, ClaimMissingLost)
		require.Zero(t, lost, "it was never counted lost")
	})
}

// The defensive branch: a claim_window_closed session that carried a claim tx
// hash was counted lost, and is taken back from lost.
func TestClaimWindowClosed_AReactivatedLostSessionIsTakenBackFromLost(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedWindowClosed(t, store, "sess-cwc-lost", service, "deadbeef")
		_, before := windowClosedProbe(service, ClaimMissingLost)
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-cwc-lost", make([]byte, SMSTRootLen), "deadbeef"))
		_, after := windowClosedProbe(service, ClaimMissingLost)
		require.InDelta(t, before+3, after, 1e-9)
	})
}

// markedBeforeFlip is a session store where the claim window closes on the
// session between the coordinator's read and its flip.
type markedBeforeFlip struct{ SessionStore }

func (s markedBeforeFlip) ReactivateClaimed(ctx context.Context, id string, root []byte, tx string) (Reactivation, error) {
	if err := s.UpdateState(ctx, id, SessionStateClaimWindowClosed); err != nil {
		return Reactivation{}, err
	}
	return s.SessionStore.ReactivateClaimed(ctx, id, root, tx)
}

// Which book to take back from comes from the flip, not from the read before
// it: a session read as claiming and marked claim_window_closed in between is
// still taken back.
func TestClaimWindowClosed_MarkedBetweenTheReadAndTheFlipIsStillTakenBack(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, _ *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		created, err := store.CreateIfAbsent(ctx, &SessionSnapshot{
			SessionID: "sess-race", SupplierOperatorAddress: "pokt1test", ServiceID: service,
			SessionStartHeight: 100, SessionEndHeight: 110, State: SessionStateClaiming,
			RelayCount: 3, TotalComputeUnits: 3_000_000,
		})
		require.NoError(t, err)
		require.True(t, created)
		coord := NewSessionCoordinator(testLogger(), markedBeforeFlip{store}, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
		coord.SetPricer(perMillionPricer{})
		sessions, _ := windowClosedProbe(service, ClaimMissingForgone)

		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-race", make([]byte, SMSTRootLen), "TXFOUND"))

		got, _ := windowClosedProbe(service, ClaimMissingForgone)
		require.Equal(t, sessions+1, got, "the flip came from claim_window_closed, whatever the read before it saw")
	})
}
