//go:build test

package miner

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
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

// The guard is NOT conditioned on the session being unknown. A session sitting
// in a non-terminal state past its claim window -- active or claiming, because
// the lifecycle sweep has not reached it yet -- is not caught by the
// IsTerminal() case above, and without this a relay would be added to a tree
// nothing will ever claim. Live evidence for the shape: a gate run dropped 6
// relays on the seal alone; those arrive in exactly this window.
func TestHandleRelay_KnownButStillOpenSessionPastClaimWindow_IsDropped(t *testing.T) {
	const supplier = "pokt1late_window_active"
	f := newHandlerTestFixture(t, supplier)
	f.coordinator.SetClaimWindowClosedFn(func(int64) bool { return true })

	// Give the session a snapshot in a NON-terminal state, the way the sweep
	// leaves it between the window closing and the transition firing.
	require.NoError(t, f.coordinator.OnSessionCreated(
		f.ctx, "sess-active-past-window", supplier, "svc-1", "pokt1app", 1, 10))
	snapshot, err := f.sessionStore.Get(f.ctx, "sess-active-past-window")
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.False(t, snapshot.State.IsTerminal(),
		"precondition: the session must NOT be terminal, or the other case would catch it")

	msg := newStreamMessage(supplier, "sess-active-past-window", "payload", 7)
	rejectedBefore := testutil.ToFloat64(relaysRejected.WithLabelValues(supplier, "claim_window_closed", "svc-1"))
	addedBefore := testutil.ToFloat64(relaysAddedToSMST.WithLabelValues(supplier, "svc-1"))

	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, msg))

	require.Equal(t, addedBefore,
		testutil.ToFloat64(relaysAddedToSMST.WithLabelValues(supplier, "svc-1")),
		"no SMST work for a relay that cannot reach a claim")
	require.Equal(t, rejectedBefore+1,
		testutil.ToFloat64(relaysRejected.WithLabelValues(supplier, "claim_window_closed", "svc-1")))
}

// A pre-submission abort records the loss itself. It must do so only when the
// terminal mark actually took: OnClaimWindowClosed returns early on a Redis
// failure, before the callback that removes the session from the sweep's
// tracking, so the sweep would transition it later and record the SAME relays a
// second time.
func TestMarkAndCountClaimWindowClosed_DoesNotRecordWhenTheMarkFails(t *testing.T) {
	const supplier = "pokt1mark_fails"
	f := newHandlerTestFixture(t, supplier)

	lc := &LifecycleCallback{
		logger:             logging.ForComponent(zerolog.Nop(), "lifecycle_callback_test"),
		smstManager:        f.smstMgr,
		sessionCoordinator: f.coordinator,
	}
	snapshot := &SessionSnapshot{
		SessionID:               "sess-mark-fails",
		SupplierOperatorAddress: supplier,
		ServiceID:               "svc-1",
		RelayCount:              5,
		TotalComputeUnits:       5_000_000,
	}

	before := testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, "svc-1", "claim_window_closed"))

	// Break every Redis command so UpdateState fails inside the coordinator.
	f.failRedis.Fail("LOADING Redis is loading the dataset in memory")
	lc.markAndCountClaimWindowClosed(f.ctx, snapshot)
	f.failRedis.Clear()

	require.Equal(t, before,
		testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, "svc-1", "claim_window_closed")),
		"the mark failed, so the sweep will record this session later; recording here too double counts")
}

func TestMarkAndCountClaimWindowClosed_RecordsWhenTheMarkTakes(t *testing.T) {
	const supplier = "pokt1mark_takes"
	f := newHandlerTestFixture(t, supplier)

	lc := &LifecycleCallback{
		logger:             logging.ForComponent(zerolog.Nop(), "lifecycle_callback_test"),
		smstManager:        f.smstMgr,
		sessionCoordinator: f.coordinator,
	}
	require.NoError(t, f.coordinator.OnSessionCreated(
		f.ctx, "sess-mark-takes", supplier, "svc-1", "pokt1app", 1, 10))

	snapshot := &SessionSnapshot{
		SessionID:               "sess-mark-takes",
		SupplierOperatorAddress: supplier,
		ServiceID:               "svc-1",
		RelayCount:              5,
		TotalComputeUnits:       5_000_000,
	}

	before := testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, "svc-1", "claim_window_closed"))
	lc.markAndCountClaimWindowClosed(f.ctx, snapshot)
	require.Equal(t, before+5,
		testutil.ToFloat64(relaysLostTotal.WithLabelValues(supplier, "svc-1", "claim_window_closed")),
		"the mark took, so this is the only place the loss is recorded")
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

// The predicate runs for every relay whose session is not already known
// terminal, so on a live fleet it is asked about sessions that have NOT ended
// yet. GetParamsAtHeight has no future-height guard: it would query, get
// today's live params back, and cache them under that future height, which the
// query layer treats as immutable for thirty minutes. The lifecycle sweep reads
// the same cache to compute a session's windows, so a governance change landing
// mid-session would be masked there -- the exact trap
// session_lifecycle_params_height_test.go pins for the sweep.
func TestClaimWindowClosedAt_NeverQueriesParamsAtAFutureHeight(t *testing.T) {
	shared := &recordingSharedQueryClient{
		mockSharedQueryClient: mockSharedQueryClient{
			params: &sharedtypes.Params{
				NumBlocksPerSession:          10,
				ClaimWindowOpenOffsetBlocks:  1,
				ClaimWindowCloseOffsetBlocks: 8,
			},
		},
	}
	m := &SupplierManager{
		logger: logging.ForComponent(zerolog.Nop(), "supplier_manager_test"),
		config: SupplierManagerConfig{
			BlockClient:  &mockBlockClient{currentHeight: 100},
			SharedClient: shared,
		},
	}

	// A session that has not ended yet: answerable without asking anyone.
	require.False(t, m.claimWindowClosedAt(context.Background(), 150),
		"a session that has not ended cannot have a closed claim window")
	require.False(t, m.claimWindowClosedAt(context.Background(), 100),
		"the current height is not past the end height either")

	atHeight, live := shared.snapshot()
	require.Empty(t, atHeight,
		"GetParamsAtHeight must not be called for a future height: it would poison the immutable height-keyed cache")
	require.Zero(t, live, "and no live query either -- the answer needs no params at all")
}

func TestClaimWindowClosedAt_UsesTheAtHeightReadOncePast(t *testing.T) {
	shared := &recordingSharedQueryClient{
		mockSharedQueryClient: mockSharedQueryClient{
			params: &sharedtypes.Params{
				NumBlocksPerSession:          10,
				ClaimWindowOpenOffsetBlocks:  1,
				ClaimWindowCloseOffsetBlocks: 8,
			},
		},
	}
	m := &SupplierManager{
		logger: logging.ForComponent(zerolog.Nop(), "supplier_manager_test"),
		config: SupplierManagerConfig{
			BlockClient:  &mockBlockClient{currentHeight: 100},
			SharedClient: shared,
		},
	}

	// Ends at 50, so the claim window closes at 50 + 1 + 8 = 59, well past 100.
	require.True(t, m.claimWindowClosedAt(context.Background(), 50))
	atHeight, _ := shared.snapshot()
	require.Equal(t, []int64{50}, atHeight,
		"a past height is exactly what the immutable at-height read is for")

	// Ends at 95, window closes at 104 -- still open at height 100.
	require.False(t, m.claimWindowClosedAt(context.Background(), 95))
}

func TestClaimWindowClosedAt_AnswersFalseWithoutClients(t *testing.T) {
	m := &SupplierManager{
		logger: logging.ForComponent(zerolog.Nop(), "supplier_manager_test"),
		config: SupplierManagerConfig{},
	}
	require.False(t, m.claimWindowClosedAt(context.Background(), 50),
		"no clients means it cannot tell, and an unknown must never cost a relay")
}

// A transient store failure must not buy a relay a place in a tree nothing will
// claim. The claim-window verdict reads only the message's session end height
// against the observed block height -- nothing the store could fail to answer --
// so a Redis error is a reason to keep the relay when the window is OPEN, and no
// reason at all to stop knowing what the height already says. Until 2026-08-28
// the store-error case fell through and skipped the check.
func TestHandleRelay_StoreErrorPastClaimWindow_IsStillDropped(t *testing.T) {
	const supplier = "pokt1store_err_past_window"
	f := newHandlerTestFixture(t, supplier)
	f.coordinator.SetClaimWindowClosedFn(func(int64) bool { return true })

	msg := newStreamMessage(supplier, "sess-store-err-past-window", "payload", 7)
	rejectedBefore := testutil.ToFloat64(relaysRejected.WithLabelValues(supplier, "claim_window_closed", "svc-1"))
	addedBefore := testutil.ToFloat64(relaysAddedToSMST.WithLabelValues(supplier, "svc-1"))

	// Break every Redis command so SessionStore.Get returns an error rather than
	// a snapshot -- the transient hiccup the fall-through exists for.
	f.failRedis.Fail("LOADING Redis is loading the dataset in memory")
	err := f.worker.handleRelay(f.ctx, supplier, msg)
	f.failRedis.Clear()

	require.NoError(t, err, "a dropped late relay is ACKed, not retried")
	require.Equal(t, addedBefore,
		testutil.ToFloat64(relaysAddedToSMST.WithLabelValues(supplier, "svc-1")),
		"no SMST work for a relay whose claim window closed, store error or not")
	require.Equal(t, rejectedBefore+1,
		testutil.ToFloat64(relaysRejected.WithLabelValues(supplier, "claim_window_closed", "svc-1")),
		"the drop must be announced with the reason the height gave, not swallowed")
}
