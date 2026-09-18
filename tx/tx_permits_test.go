//go:build test

package tx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
)

// newPermitClient builds a client with n permits against srv, shaped like
// production: it owns its connection.
func newPermitClient(t *testing.T, srv *testGRPCServer, n int) *TxClient {
	t.Helper()

	srv.addAccount(budgetTestSupplier, 1, 0)
	km := setupTestKeyManager(t, budgetTestSupplier)
	t.Cleanup(func() { _ = km.Close() })

	tc, err := NewTxClient(logging.NewLoggerFromConfig(logging.DefaultConfig()), km, TxClientConfig{
		BlockTimeProvider: testBlockTime(),
		GRPCEndpoint:      srv.address,
		ChainID:           "test-chain",
		GasLimit:          100000,
		GasPrice:          parseGasPrice(t, "0.001upokt"),
		MaxConcurrent:     n,
		ConnProbeInterval: time.Hour,
	})
	require.NoError(t, err)
	return tc
}

// waitForPermitWaiters blocks until n callers are queued for a permit, so a
// test never has to sleep to know the queue is in the state it wants.
func waitForPermitWaiters(t *testing.T, tc *TxClient, n int64) {
	t.Helper()
	require.Eventually(t, func() bool { return tc.permitWaiters.Load() == n }, 5*time.Second, time.Millisecond,
		"expected %d caller(s) queued for a permit, got %d", n, tc.permitWaiters.Load())
}

// TestPermitOrdersCallersBeyondTheLimit is criterion 3, and it is written as an
// ORDER rather than as a wait.
//
// "the N+1st waits and then proceeds" does not discriminate: WITHOUT a
// semaphore it also proceeds, so a test that asserts it finishes is green with
// the defect present. And it cannot be a duration -- this repository's first
// rule is that no test may be flaky. What only the semaphore can produce is the
// ORDER: the N+1st cannot complete before one of the N releases.
func TestPermitOrdersCallersBeyondTheLimit(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	release := srv.txServer.BlockBroadcast()
	seen := srv.txServer.NotifyBroadcast(8)

	tc := newPermitClient(t, srv, 1)
	t.Cleanup(func() { _ = tc.Close() })

	// One in flight, parked inside the server, holding the only permit.
	first := make(chan error, 1)
	go func() { first <- claim(tc, context.Background(), t) }()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the first broadcast never reached the server")
	}

	// The second cannot even reach the server: no permit is free.
	second := make(chan error, 1)
	go func() { second <- claim(tc, context.Background(), t) }()
	waitForPermitWaiters(t, tc, 1)

	select {
	case <-second:
		t.Fatal("the second call completed while the only permit was held: there is no bound on concurrency")
	default:
	}

	// Releasing the first is what lets the second run -- that ordering, and not
	// any amount of elapsed time, is the assertion.
	close(release)
	require.NoError(t, <-first)
	select {
	case err := <-second:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the second call never completed after a permit was freed")
	}
}

// TestQueuedCallerAtCloseNeverReachesTheNetwork is criterion 10.
//
// It is the behavioural form of "the permit wait does not hold a lock", which
// is not observable -- but its consequence is, and walking the queue shows it
// exactly. With the fix, Close() flips the flag and THEN queues its own
// acquire, so a caller already waiting wakes up, re-reads the flag, hands the
// permit back and returns closed WITHOUT broadcasting. With the wait held under
// a read lock, Close() cannot even take the write lock to set the flag, so that
// same caller reads closed == false and broadcasts into a connection that is
// about to shut.
//
// So the discriminator is not how long Close() takes. It is whether the queued
// caller reaches the chain -- deterministic, and with no clock in it.
func TestQueuedCallerAtCloseNeverReachesTheNetwork(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	release := srv.txServer.BlockBroadcast()
	seen := srv.txServer.NotifyBroadcast(8)

	tc := newPermitClient(t, srv, 1)

	first := make(chan error, 1)
	go func() { first <- claim(tc, context.Background(), t) }()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the first broadcast never reached the server")
	}
	broadcastsBefore := srv.getBroadcastCount()

	queued := make(chan error, 1)
	go func() { queued <- claim(tc, context.Background(), t) }()
	waitForPermitWaiters(t, tc, 1)

	closed := make(chan error, 1)
	go func() { closed <- tc.Close() }()

	// Let the in-flight one finish. Its permit goes to the queued caller, which
	// must now discover the client is closed rather than broadcast.
	close(release)
	require.NoError(t, <-first)

	select {
	case err := <-queued:
		require.Error(t, err, "the queued caller broadcast into a closing client")
	case <-time.After(5 * time.Second):
		t.Fatal("the queued caller never returned")
	}
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close() never returned")
	}

	require.Equal(t, broadcastsBefore, srv.getBroadcastCount(),
		"a caller queued at the moment of Close() still reached the chain")
}

// TestResendNeverQueuesForAPermit: the safety net must not queue behind the
// traffic it came to rescue. A resend runs on the reconciler's per-group
// budget, and spending that budget waiting means leaving without reaching the
// chain -- in saturation, which is exactly when the resend exists.
func TestResendNeverQueuesForAPermit(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	release := srv.txServer.BlockBroadcast()
	defer close(release)
	seen := srv.txServer.NotifyBroadcast(8)

	tc := newPermitClient(t, srv, 1)
	t.Cleanup(func() { _ = tc.Close() })

	go func() { _ = claim(tc, context.Background(), t) }()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the first broadcast never reached the server")
	}

	// No permit is free. A waiting caller would block here for as long as the
	// broadcast lasts; this one must come back now, and say why.
	//
	// Off to the side so a resend that DOES wait fails by naming the defect
	// instead of hanging until the package timeout, where the red says nothing
	// about queueing.
	result := make(chan error, 1)
	go func() { result <- claim(tc, WithoutPermitWait(context.Background()), t) }()

	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrTxConcurrencySaturated,
			"the resend failed with an error nobody can distinguish from a chain rejection")
	case <-time.After(3 * time.Second):
		t.Fatal("the resend queued for a permit: the safety net is waiting behind the traffic it came to rescue")
	}
	require.Zero(t, tc.permitWaiters.Load(), "the resend joined the queue")
}

// TestSaturationIsDistinguishableFromRejection: the sentinel carries the one
// distinction the resend counter depends on. MaxRebroadcasts is a small number,
// so counting an attempt that never left the process burns the only resend a
// claim had, on nothing.
func TestSaturationIsDistinguishableFromRejection(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	srv.setBroadcastError(status.Error(codes.Unavailable, "chain said no"))

	tc := newPermitClient(t, srv, 1)
	t.Cleanup(func() { _ = tc.Close() })

	rejected := claim(tc, context.Background(), t)
	require.Error(t, rejected)
	require.False(t, errors.Is(rejected, ErrTxConcurrencySaturated),
		"a chain rejection was classified as saturation: the resend would not be counted and would repeat forever")
}

// TestSaturationIsAudible: the error tells the CALLER what happened; this is
// what tells the OPERATOR. Without it a starving resend is silent — the
// permit-wait histogram cannot carry it, because a caller that refuses to
// queue waited zero and would land in the same bucket as a healthy acquire.
func TestSaturationIsAudible(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	release := srv.txServer.BlockBroadcast()
	defer close(release)
	seen := srv.txServer.NotifyBroadcast(8)

	tc := newPermitClient(t, srv, 1)
	t.Cleanup(func() { _ = tc.Close() })

	go func() { _ = claim(tc, context.Background(), t) }()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the first broadcast never reached the server")
	}

	before := testutil.ToFloat64(txPermitSaturatedTotal.WithLabelValues(permitDidNotWait))
	require.ErrorIs(t, claim(tc, WithoutPermitWait(context.Background()), t), ErrTxConcurrencySaturated)
	require.Greater(t, testutil.ToFloat64(txPermitSaturatedTotal.WithLabelValues(permitDidNotWait)), before,
		"a resend starved on saturation and nothing an operator can alert on moved")
}

// TestFailureCountersExistBeforeTheyFire is the assertion the previous two
// commits were missing.
//
// A *Vec with no children exports nothing, so a counter whose job is to make a
// silent failure audible is itself absent until the failure happens — and "no
// data" on a dashboard reads as "not instrumented", which is exactly what it
// must never be confused with. The rule was written in the first commit of this
// branch (observability/grpcstats.go) and then not applied by the two that
// added a CounterVec each.
//
// It asks the REGISTRY, not the source: a call to WithLabelValues in the code
// proves nothing about whether it ran.
func TestFailureCountersExistBeforeTheyFire(t *testing.T) {
	for _, tt := range []struct {
		name   string
		metric string
		labels []string
	}{
		{"probe failures", "ha_tx_conn_probe_failures_total", []string{probeReasonStartup, probeReasonTick}},
		{"permit saturation", "ha_tx_permit_saturated_total", []string{permitWaited, permitDidNotWait}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			families, err := observability.MinerRegistry.Gather()
			require.NoError(t, err)

			var found []string
			for _, f := range families {
				if f.GetName() != tt.metric {
					continue
				}
				for _, m := range f.GetMetric() {
					for _, l := range m.GetLabel() {
						found = append(found, l.GetValue())
					}
				}
			}
			require.NotEmpty(t, found, "%s exports no series at all: a dashboard cannot tell a healthy fleet from an uninstrumented one", tt.metric)
			for _, want := range tt.labels {
				require.Contains(t, found, want,
					"%s has no child for %q until the failure occurs, so an alert on it returns no data rather than zero", tt.metric, want)
			}
		})
	}
}
