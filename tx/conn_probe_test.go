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
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// failureCount reads the probe-failure counter for one reason. The counter is
// process-global, so tests read a DELTA around the action under test rather
// than an absolute.
func failureCount(t *testing.T, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(txConnProbeFailures.WithLabelValues(reason))
}

// newProbeClient builds a TxClient that owns its connection to srv, which is
// the production shape after the tx client stopped borrowing the query
// connection.
func newProbeClient(t *testing.T, endpoint string, interval time.Duration) *TxClient {
	t.Helper()

	tc, err := NewTxClient(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		setupTestKeyManager(t),
		TxClientConfig{
			GRPCEndpoint:      endpoint,
			ChainID:           "test-chain",
			ConnProbeInterval: interval,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })
	return tc
}

// TestVerifyConnSucceedsAgainstALiveNode pins the startup check's happy path:
// it must issue a real RPC, not merely observe that grpc.NewClient returned.
func TestVerifyConnSucceedsAgainstALiveNode(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	tc := newProbeClient(t, srv.address, time.Hour)

	before := srv.authServer.ParamsCalls()
	require.NoError(t, tc.VerifyConn(context.Background()))
	require.Greater(t, srv.authServer.ParamsCalls(), before,
		"VerifyConn returned nil without reaching the server: it is not proving anything")
}

// TestVerifyConnFailsAgainstADeadEndpoint is the case the check exists for.
// grpc.NewClient is lazy, so a connection to nothing constructs happily and
// only fails when someone finally uses it -- which, without this, is the first
// claim, inside a closing window.
func TestVerifyConnFailsAgainstADeadEndpoint(t *testing.T) {
	// Port 1 on loopback: reserved, and nothing in CI listens there.
	tc := newProbeClient(t, "127.0.0.1:1", time.Hour)

	err := tc.VerifyConn(context.Background())
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrTxConnMisconfigured),
		"an unreachable node must NOT be classified as misconfigured: it is the case where starting anyway is correct")
}

// TestProbeClassifiesByCodeNotMessage pins the classification, which decides
// whether the miner refuses to start. It is asserted per code, because the
// whole point is that the decision never reads message text.
func TestProbeClassifiesByCodeNotMessage(t *testing.T) {
	tests := []struct {
		name           string
		code           codes.Code
		misconfigured  bool
		whyItIsThatWay string
	}{
		{
			name:           "unimplemented is a wrong endpoint",
			code:           codes.Unimplemented,
			misconfigured:  true,
			whyItIsThatWay: "it speaks gRPC but does not serve the auth module: not a Pocket node",
		},
		{
			name:           "unauthenticated is rejected credentials",
			code:           codes.Unauthenticated,
			misconfigured:  true,
			whyItIsThatWay: "retrying cannot supply different credentials",
		},
		{
			name:           "unavailable is a blip",
			code:           codes.Unavailable,
			misconfigured:  false,
			whyItIsThatWay: "a node that is down, and a TLS handshake failure, are the same code here",
		},
		{
			name:           "deadline exceeded is a blip",
			code:           codes.DeadlineExceeded,
			misconfigured:  false,
			whyItIsThatWay: "a slow node is not a broken config",
		},
		{
			name:           "internal is a blip",
			code:           codes.Internal,
			misconfigured:  false,
			whyItIsThatWay: "the node's problem, not ours",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := setupMockGRPCServer(t)
			t.Cleanup(srv.cleanup)
			srv.authServer.SetParamsErr(status.Error(tt.code, "deliberate"))

			tc := newProbeClient(t, srv.address, time.Hour)

			err := tc.VerifyConn(context.Background())
			require.Error(t, err)
			require.Equal(t, tt.misconfigured, errors.Is(err, ErrTxConnMisconfigured), tt.whyItIsThatWay)
		})
	}
}

// TestProbeBoundsItsOwnWait proves the probe carries its own deadline rather
// than inheriting the caller's, which here is context.Background().
//
// This is not only so a hung probe fails loudly. It is what keeps the ping
// accounting at 1:1: the server forgives ONE ping per write it makes, a
// healthy probe produces one ping, and a probe that hangs past the keepalive
// interval produces a second with no write in between -- three of those earn a
// GOAWAY. See transport/grpcconn.
//
// It costs one txConnProbeTimeout of wall clock on purpose: shortening it by
// handing VerifyConn a context that expires sooner would assert the TEST's
// deadline and pass with the code's deadline deleted.
func TestProbeBoundsItsOwnWait(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	release := srv.authServer.BlockParams()
	defer close(release)

	tc := newProbeClient(t, srv.address, time.Hour)

	// VerifyConn runs off to the side so a probe WITHOUT its own deadline
	// fails by naming the defect instead of hanging: a straight call would
	// never reach the assertions below, and the only red would be a package
	// timeout ten minutes later with nothing in it about deadlines.
	result := make(chan error, 1)
	go func() { result <- tc.VerifyConn(context.Background()) }()

	select {
	case err := <-result:
		require.Error(t, err)
		require.Equal(t, codes.DeadlineExceeded, status.Code(err),
			"the probe ended for some other reason than its own deadline")
	case <-time.After(txConnProbeTimeout + 3*time.Second):
		t.Fatal("the probe outlived its deadline: it is inheriting the caller's context, which here never expires")
	}
}

// TestProbeTickKeepsProbingWhileIdle is the tick's reason to exist: with no
// caller traffic at all, RPCs must keep reaching the node, so a connection
// that dies while idle is found BEFORE a window instead of inside one.
//
// The assertion is on GROWTH from the count VerifyConn already left behind,
// and that is the whole design of this test. "at least one Params call" would
// pass with the tick deleted, because the startup verification makes one --
// the same shape that made three earlier injections in this branch come back
// green: an assertion that counts occurrences cannot say WHICH occurred.
func TestProbeTickKeepsProbingWhileIdle(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	seen := srv.authServer.NotifyParams(8)
	tc := newProbeClient(t, srv.address, 5*time.Millisecond)

	require.NoError(t, tc.VerifyConn(context.Background()))
	<-seen // the startup verification's own call
	afterStartup := srv.authServer.ParamsCalls()

	// Two more, without this test issuing a single RPC of its own.
	for range 2 {
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
			t.Fatal("no probe reached the server while idle: the tick is not running")
		}
	}

	require.Greater(t, srv.authServer.ParamsCalls(), afterStartup,
		"probes did not grow past what startup verification left: the tick is not probing")
}

// TestProbeTickRecordsFailures proves the tick is DETECTION and not merely
// traffic: a probe that fails has to leave something an operator can alert on.
func TestProbeTickRecordsFailures(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	srv.authServer.SetParamsErr(status.Error(codes.Unavailable, "deliberate"))

	before := failureCount(t, probeReasonTick)

	seen := srv.authServer.NotifyParams(8)
	tc := newProbeClient(t, srv.address, 5*time.Millisecond)
	defer func() { _ = tc.Close() }()

	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no probe reached the server")
	}

	require.Eventually(t, func() bool {
		return failureCount(t, probeReasonTick) > before
	}, 5*time.Second, 5*time.Millisecond,
		"a failing probe incremented nothing: the failure is invisible")
}

// TestCloseWaitsForAnInFlightBroadcast closes the window this commit would
// otherwise open.
//
// CreateClaims checks `closed` on entry and then broadcasts, and the two are
// not atomic. That was harmless while the tx client borrowed the query
// connection -- Close() closed nothing -- and stops being harmless the moment
// it owns one: a Close() landing in between would pull the connection out from
// under a claim already on its way to the chain.
func TestCloseWaitsForAnInFlightBroadcast(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	supplierAddr := "pokt1supplier123"
	srv.addAccount(supplierAddr, 1, 0)
	km := setupTestKeyManager(t, supplierAddr)
	t.Cleanup(func() { _ = km.Close() })

	tc, err := NewTxClient(logging.NewLoggerFromConfig(logging.DefaultConfig()), km, TxClientConfig{
		GRPCEndpoint:      srv.address,
		ChainID:           "test-chain",
		GasLimit:          100000,
		GasPrice:          parseGasPrice(t, "0.001upokt"),
		ConnProbeInterval: time.Hour,
	})
	require.NoError(t, err)

	release := srv.txServer.BlockBroadcast()
	seen := srv.txServer.NotifyBroadcast(4)

	broadcastDone := make(chan struct{})
	go func() {
		defer close(broadcastDone)
		_, _ = tc.CreateClaims(context.Background(), supplierAddr, 1000,
			[]*prooftypes.MsgCreateClaim{generateTestClaim(t, supplierAddr, "session-1")})
	}()

	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the broadcast never reached the server")
	}

	closed := make(chan error, 1)
	go func() { closed <- tc.Close() }()

	select {
	case <-closed:
		t.Fatal("Close() returned with a broadcast in flight: it would have closed the connection underneath it")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close() never returned after the broadcast was released")
	}
	<-broadcastDone
}

// TestCloseStopsTheProbe: the probe goroutine must not outlive the client.
//
// The assertion is that the goroutine has ALREADY EXITED when Close() returns,
// and getting here took two wrong assertions worth recording.
//
// "no more calls reach the server" passes with the cancellation deleted: a
// leaked goroutine finds the connection closed and fails instantly, so it
// reaches the server exactly as rarely as one that stopped. "the failure
// counter stops moving" passes too, and for a sharper reason -- a closed
// grpc.ClientConn returns codes.Canceled, which probeOnce deliberately does
// not count. Both assertions were counting a CONSEQUENCE that the leak happens
// not to produce; the thing Close() actually promises is that the goroutine is
// gone, and that is what this reads.
func TestCloseStopsTheProbe(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	seen := srv.authServer.NotifyParams(16)
	tc := newProbeClient(t, srv.address, 5*time.Millisecond)

	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no probe reached the server")
	}

	require.NoError(t, tc.Close())

	// No polling and no window: Close() waited, so by the time it returned the
	// channel is closed or the goroutine is still out there.
	select {
	case <-tc.probeDone:
	default:
		t.Fatal("the probe goroutine was still running when Close() returned: it outlived the client")
	}
}

// TestCloseDoesNotCountItsOwnCancellation: shutting down cancels the probe's
// context, and counting that would move the "the connection died silently"
// metric on every deploy -- exactly when an operator cannot afford it to be
// noise.
func TestCloseDoesNotCountItsOwnCancellation(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	release := srv.authServer.BlockParams()
	seen := srv.authServer.NotifyParams(8)

	tc := newProbeClient(t, srv.address, 5*time.Millisecond)
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no probe reached the server")
	}

	before := failureCount(t, probeReasonTick)

	// Close cancels the parked probe. The server call then returns the
	// context error, and that must not be counted: it is our own shutdown.
	closed := make(chan error, 1)
	go func() { closed <- tc.Close() }()
	close(release)

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close() never returned")
	}

	require.Equal(t, before, failureCount(t, probeReasonTick),
		"shutdown incremented the probe-failure counter: every deploy would look like a dead connection")
}
