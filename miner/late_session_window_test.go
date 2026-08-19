//go:build test

package miner

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// A relay whose session no longer exists locally and whose claim window has
// already closed used to be processed like any other: an SMST tree was built
// for it and OnRelayProcessed CREATED a session, because the only guard in
// handleRelay reads snapshot.State.IsTerminal() and is skipped entirely when
// there is no snapshot. The lifecycle sweep would then carry that session to
// claim_window_closed and delete the tree again — work done for nothing, and
// silently, since the sweep recorded no metric either.
//
// These tests pin both halves: the relay is dropped and counted before any
// SMST work, and a session that times out through the sweep records the loss.

func TestClaimWindowClosed_AnswersFalseWhenItCannotTell(t *testing.T) {
	c := NewSessionCoordinator(zerolog.Nop(), nil, SMSTRecoveryConfig{SupplierAddress: "pokt1x"})

	require.False(t, c.ClaimWindowClosed(100),
		"no predicate wired must answer false: dropping on an unknown is how served work stops being paid")

	c.SetClaimWindowClosedFn(func(int64) bool { return true })
	require.False(t, c.ClaimWindowClosed(0), "height 0 carries no information")
	require.False(t, c.ClaimWindowClosed(-1), "negative height carries no information")
	require.True(t, c.ClaimWindowClosed(100), "a wired predicate decides for a real height")
}

func TestClaimWindowClosed_PassesTheSessionEndHeightThrough(t *testing.T) {
	c := NewSessionCoordinator(zerolog.Nop(), nil, SMSTRecoveryConfig{SupplierAddress: "pokt1x"})

	var seen int64
	c.SetClaimWindowClosedFn(func(h int64) bool {
		seen = h
		return false
	})
	require.False(t, c.ClaimWindowClosed(4242))
	require.Equal(t, int64(4242), seen)
}

func TestHandleRelay_UnknownSessionPastClaimWindow_DropsWithoutCreatingASession(t *testing.T) {
	const supplier = "pokt1late_window_drop"
	f := newHandlerTestFixture(t, supplier)
	f.coordinator.SetClaimWindowClosedFn(func(int64) bool { return true })

	msg := newStreamMessage(supplier, "sess-past-window", "payload", 7)

	rejectedBefore := testutil.ToFloat64(relaysRejected.WithLabelValues(supplier, "claim_window_closed", "svc-1"))
	addedBefore := testutil.ToFloat64(relaysAddedToSMST.WithLabelValues(supplier, "svc-1"))

	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, msg),
		"an unpayable relay is ACKed, not retried forever")

	// No session was created: this is the whole point — the sweep must not be
	// handed a session it can only fail.
	snapshot, err := f.sessionStore.Get(f.ctx, "sess-past-window")
	require.NoError(t, err)
	require.Nil(t, snapshot, "a session past its claim window must never be created")

	// And no SMST work was done for it.
	nodes, err := f.redisClient.Exists(f.ctx, f.redisClient.KB().SMSTNodesKey(supplier, "sess-past-window")).Result()
	require.NoError(t, err)
	require.Zero(t, nodes, "no SMST tree may be built for an unpayable relay")

	require.Equal(t, addedBefore,
		testutil.ToFloat64(relaysAddedToSMST.WithLabelValues(supplier, "svc-1")),
		"relays_added_to_smst_total must not move")
	require.Equal(t, rejectedBefore+1,
		testutil.ToFloat64(relaysRejected.WithLabelValues(supplier, "claim_window_closed", "svc-1")),
		"the drop must be announced, not silent")
}

func TestHandleRelay_UnknownSessionInsideClaimWindow_StillCreatesTheSession(t *testing.T) {
	const supplier = "pokt1late_window_keep"
	f := newHandlerTestFixture(t, supplier)
	f.coordinator.SetClaimWindowClosedFn(func(int64) bool { return false })

	msg := newStreamMessage(supplier, "sess-in-window", "payload", 7)
	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, msg))

	snapshot, err := f.sessionStore.Get(f.ctx, "sess-in-window")
	require.NoError(t, err)
	require.NotNil(t, snapshot,
		"the guard must reject only past-window sessions; a normal first relay still creates one")
}

func TestHandleRelay_UnknownSessionWithNoPredicate_StillCreatesTheSession(t *testing.T) {
	const supplier = "pokt1late_window_unwired"
	f := newHandlerTestFixture(t, supplier)
	// No SetClaimWindowClosedFn: the miner runs without chain clients.

	msg := newStreamMessage(supplier, "sess-unwired", "payload", 7)
	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, msg))

	snapshot, err := f.sessionStore.Get(f.ctx, "sess-unwired")
	require.NoError(t, err)
	require.NotNil(t, snapshot, "an unwired predicate must never cost a relay")
}

// The sweep in SessionLifecycleManager decides a window timeout on height alone
// and records nothing; OnClaimWindowClosed is the only place both it and the
// cleanup pass through, so the loss is recorded there.
func TestOnClaimWindowClosed_RecordsTheLoss(t *testing.T) {
	const supplier = "pokt1window_metric_claim"
	f := newHandlerTestFixture(t, supplier)

	lc := &LifecycleCallback{
		logger:      logging.ForComponent(zerolog.Nop(), "lifecycle_callback_test"),
		smstManager: f.smstMgr,
	}
	snapshot := &SessionSnapshot{
		SessionID:               "sess-window-metric",
		SupplierOperatorAddress: supplier,
		ServiceID:               "svc-1",
		RelayCount:              4,
		TotalComputeUnits:       4_000_000,
	}

	sessionsBefore := testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, "svc-1", "claim_window_closed"))
	relaysBefore := testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, "svc-1", "claim_window_closed"))

	require.NoError(t, lc.OnClaimWindowClosed(context.Background(), snapshot))

	require.Equal(t, sessionsBefore+1,
		testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, "svc-1", "claim_window_closed")),
		"a session lost to a closed claim window must be counted")
	require.Equal(t, relaysBefore+4,
		testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, "svc-1", "claim_window_closed")),
		"the relays it carried must be counted, not just the session")
}

func TestOnProofWindowClosed_RecordsTheLoss(t *testing.T) {
	const supplier = "pokt1window_metric_proof"
	f := newHandlerTestFixture(t, supplier)

	lc := &LifecycleCallback{
		logger:      logging.ForComponent(zerolog.Nop(), "lifecycle_callback_test"),
		smstManager: f.smstMgr,
	}
	snapshot := &SessionSnapshot{
		SessionID:               "sess-proof-window-metric",
		SupplierOperatorAddress: supplier,
		ServiceID:               "svc-1",
		RelayCount:              3,
		TotalComputeUnits:       3_000_000,
	}

	sessionsBefore := testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, "svc-1", "proof_window_closed"))
	relaysBefore := testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, "svc-1", "proof_window_closed"))

	require.NoError(t, lc.OnProofWindowClosed(context.Background(), snapshot))

	require.Equal(t, sessionsBefore+1,
		testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, "svc-1", "proof_window_closed")))
	require.Equal(t, relaysBefore+3,
		testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, "svc-1", "proof_window_closed")))
}
