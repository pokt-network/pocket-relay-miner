//go:build test

package miner

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// seedUnsentClaim stores a session whose claim did not go out: claiming, no
// claim tx hash, weighing 3 relays and 3 uPOKT under perMillionPricer.
func seedUnsentClaim(t *testing.T, store SessionStore, id, service string) {
	t.Helper()
	created, err := store.CreateIfAbsent(context.Background(), &SessionSnapshot{
		SessionID: id, SupplierOperatorAddress: "pokt1test", ServiceID: service,
		SessionStartHeight: 100, SessionEndHeight: 110, State: SessionStateClaiming,
		RelayCount: 3, TotalComputeUnits: 3_000_000,
	})
	require.NoError(t, err)
	require.True(t, created)
}

// txErrorProbe reads, for one reason, the failed and reinstated sessions and
// the forgone and reinstated relays and uPOKT.
type txErrorProbe struct {
	failed, reinstated                                 float64
	relaysForgone, relaysBack, upoktForgone, upoktBack float64
}

func readTxErrorProbe(service, reason string) txErrorProbe {
	return txErrorProbe{
		failed:        testutil.ToFloat64(sessionsFailedTotal.WithLabelValues("pokt1test", service, reason)),
		reinstated:    testutil.ToFloat64(sessionsReinstatedTotal.WithLabelValues("pokt1test", service, reason)),
		relaysForgone: testutil.ToFloat64(relaysForgoneTotal.WithLabelValues("pokt1test", service, reason)),
		relaysBack:    testutil.ToFloat64(relaysReinstatedTotal.WithLabelValues("pokt1test", service, reason, ClaimMissingForgone)),
		upoktForgone:  testutil.ToFloat64(upoktForgoneTotal.WithLabelValues("pokt1test", service, reason)),
		upoktBack:     testutil.ToFloat64(upoktReinstatedTotal.WithLabelValues("pokt1test", service, reason, ClaimMissingForgone)),
	}
}

func (p txErrorProbe) minus(q txErrorProbe) txErrorProbe {
	return txErrorProbe{
		failed: p.failed - q.failed, reinstated: p.reinstated - q.reinstated,
		relaysForgone: p.relaysForgone - q.relaysForgone, relaysBack: p.relaysBack - q.relaysBack,
		upoktForgone: p.upoktForgone - q.upoktForgone, upoktBack: p.upoktBack - q.upoktBack,
	}
}

// A retryable claim_tx_error counted the session and no money. When the chain
// is seen to hold the claim after all, the session is taken back and no money
// is: failed minus reinstated is 0.
func TestClaimTxError_AReinstatedAttemptIsTakenBackWithoutMoney(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedUnsentClaim(t, store, "sess-attempt", service)
		before := readTxErrorProbe(service, "claim_tx_error")

		require.NoError(t, coord.OnClaimTxError(ctx, "sess-attempt", ClaimTxErrorAttempt))
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-attempt", make([]byte, SMSTRootLen), "deadbeef"))

		d := readTxErrorProbe(service, "claim_tx_error").minus(before)
		require.Equal(t, 1.0, d.failed, "the failure is counted once")
		require.Equal(t, 1.0, d.reinstated, "and taken back once: failed minus reinstated is 0")
		require.Zero(t, d.relaysForgone, "an attempt counts no money")
		require.Zero(t, d.relaysBack, "so its reinstatement takes none back")
		require.Zero(t, d.upoktBack)
	})
}

// A claim_tx_error nothing would answer counted its money forgone: that is
// what its reinstatement takes back.
func TestClaimTxError_AReinstatedForgoneSessionIsTakenBackFromForgone(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedUnsentClaim(t, store, "sess-forgone", service)
		before := readTxErrorProbe(service, "claim_tx_error")

		require.NoError(t, coord.OnClaimTxError(ctx, "sess-forgone", ClaimTxErrorForgone))
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-forgone", make([]byte, SMSTRootLen), "deadbeef"))

		d := readTxErrorProbe(service, "claim_tx_error").minus(before)
		require.Equal(t, 1.0, d.failed)
		require.Equal(t, 1.0, d.reinstated)
		require.Equal(t, 3.0, d.relaysForgone, "CONTROL: the failure counted its relays forgone")
		require.Equal(t, d.relaysForgone, d.relaysBack, "net forgone relays are back to 0")
		require.Equal(t, 3.0, d.upoktForgone, "CONTROL: and its uPOKT")
		require.Equal(t, d.upoktForgone, d.upoktBack, "net forgone uPOKT is back to 0")
	})
}

// An ejected claim with no way back rests in claim_tx_error but was counted
// under claim_ejected_unrecoverable: its reinstatement reverses that reason.
func TestClaimTxError_AReinstatedEjectionIsTakenBackUnderItsOwnReason(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedUnsentClaim(t, store, "sess-ejected", service)
		before := readTxErrorProbe(service, "claim_ejected_unrecoverable")
		txBefore := readTxErrorProbe(service, "claim_tx_error")

		require.NoError(t, coord.OnClaimTxError(ctx, "sess-ejected", ClaimTxErrorEjectedUnrecoverable))
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-ejected", make([]byte, SMSTRootLen), "deadbeef"))

		d := readTxErrorProbe(service, "claim_ejected_unrecoverable").minus(before)
		require.Equal(t, 1.0, d.failed)
		require.Equal(t, 1.0, d.reinstated)
		require.Equal(t, 3.0, d.upoktForgone, "CONTROL: the ejection counted its uPOKT forgone")
		require.Equal(t, d.upoktForgone, d.upoktBack)
		require.Equal(t, d.relaysForgone, d.relaysBack)
		require.Equal(t, txErrorProbe{}, readTxErrorProbe(service, "claim_tx_error").minus(txBefore),
			"nothing is counted or taken back under claim_tx_error")
	})
}

// A session fails once: marking it claim_tx_error again counts nothing and
// keeps the verdict its count was made by.
func TestClaimTxError_ASecondMarkCountsNothing(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedUnsentClaim(t, store, "sess-twice", service)
		before := readTxErrorProbe(service, "claim_tx_error")

		require.NoError(t, coord.OnClaimTxError(ctx, "sess-twice", ClaimTxErrorAttempt))
		require.NoError(t, coord.OnClaimTxError(ctx, "sess-twice", ClaimTxErrorForgone))

		d := readTxErrorProbe(service, "claim_tx_error").minus(before)
		require.Equal(t, 1.0, d.failed, "a session fails once")
		require.Zero(t, d.relaysForgone, "the second verdict counted nothing")
		got, err := store.Get(ctx, "sess-twice")
		require.NoError(t, err)
		require.Equal(t, SessionStateClaimTxError, got.State)
		require.Equal(t, ClaimTxErrorAttempt, got.ClaimMissingVerdict, "the verdict the count was made by is kept")
	})
}

// A claim batch that fails every attempt is not a failure for a session another
// miner already claimed: the state is not written over it, so it is not
// counted either.
func TestOnSessionsNeedClaim_ASessionClaimedElsewhereIsNotCountedFailed(t *testing.T) {
	broadcastDown := &tx.TxRejection{Stage: tx.TxStageBroadcast, RawLog: "connection refused by the full node"}
	spy := &batchSpy{errs: []error{broadcastDown, broadcastDown}}
	lc, store, _, _, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb")

	ctx := context.Background()
	claimedElsewhere := *snapshots[0]
	claimedElsewhere.State = SessionStateClaimed
	claimedElsewhere.ClaimTxHash = "0xanotherminer"
	require.NoError(t, store.Save(ctx, &claimedElsewhere))

	failed := func(snap *SessionSnapshot) float64 {
		return testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(snap.SupplierOperatorAddress, snap.ServiceID, "claim_tx_error"))
	}
	beforeClaimed, beforeOther := failed(snapshots[0]), failed(snapshots[1])

	_, _ = lc.OnSessionsNeedClaim(ctx, snapshots)

	require.Len(t, spy.calls, 2, "CONTROL: the batch was sent and retried until the attempts ran out")
	require.Equal(t, beforeOther+1, failed(snapshots[1]), "CONTROL: the session this miner failed is counted")
	require.Equal(t, beforeClaimed, failed(snapshots[0]),
		"the session another miner claimed is not a failed session")
	got, err := store.Get(ctx, snapshots[0].SessionID)
	require.NoError(t, err)
	require.Equal(t, SessionStateClaimed, got.State, "and it is still claimed")
}

// The verdict says how the move into claim_tx_error was counted, so a write
// that moves nothing keeps it: in both stores.
func TestMarkClaimTxError_AWriteThatMovesNothingKeepsTheVerdict(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, _ *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedUnsentClaim(t, store, "sess-parity", service)

		flipped, err := store.MarkClaimTxError(ctx, "sess-parity", ClaimTxErrorAttempt)
		require.NoError(t, err)
		require.True(t, flipped, "CONTROL: the first write moves the session")

		flipped, err = store.MarkClaimTxError(ctx, "sess-parity", ClaimTxErrorForgone)
		require.NoError(t, err)
		require.False(t, flipped, "a second write moves nothing")

		got, err := store.Get(ctx, "sess-parity")
		require.NoError(t, err)
		require.Equal(t, ClaimTxErrorAttempt, got.ClaimMissingVerdict, "and replaces nothing")
	})
}

// unreadableSessions is a session store whose reads fail while its writes work.
type unreadableSessions struct{ SessionStore }

func (unreadableSessions) Get(context.Context, string) (*SessionSnapshot, error) {
	return nil, errReadInjected
}

// A session that could not be read was not counted, so it carries no verdict
// for a reactivation to reverse.
func TestClaimTxError_AnUnreadSessionIsNeitherCountedNorTakenBack(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedUnsentClaim(t, store, "sess-unread", service)
		before := readTxErrorProbe(service, "claim_tx_error")

		blind := NewSessionCoordinator(testLogger(), unreadableSessions{store}, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
		t.Cleanup(func() { _ = blind.Close() })
		blind.SetPricer(perMillionPricer{})
		require.NoError(t, blind.OnClaimTxError(ctx, "sess-unread", ClaimTxErrorForgone))

		got, err := store.Get(ctx, "sess-unread")
		require.NoError(t, err)
		require.Equal(t, SessionStateClaimTxError, got.State, "CONTROL: the state was written")
		require.Empty(t, got.ClaimMissingVerdict, "with no verdict: nothing was counted")

		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-unread", make([]byte, SMSTRootLen), "deadbeef"))
		require.Equal(t, txErrorProbe{}, readTxErrorProbe(service, "claim_tx_error").minus(before),
			"nothing counted, nothing taken back")
	})
}
