//go:build test

package relayer

import (
	"context"
	"fmt"
	"github.com/pokt-network/pocket-relay-miner/transport"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// countingWSBackend counts CONNECTIONS, not messages, and tracks how many are
// still OPEN. Both halves matter: the defect this file pins is a TCP+WebSocket
// handshake against the operator's backend performed on the strength of a
// gateway upgrade alone, and a leak is a connection opened and never closed.
// A message counter sees neither.
func countingWSBackend(t *testing.T) (wsURL string, dials, open *atomic.Int32) {
	t.Helper()
	dials, open = &atomic.Int32{}, &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		dials.Add(1)
		open.Add(1)
		defer func() {
			open.Add(-1)
			_ = conn.Close()
		}()
		for {
			mt, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, []byte(`{"jsonrpc":"2.0","result":"0x1","id":1}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), dials, open
}

// heldWSBackend holds every upgrade until the test releases it, so a close can
// be made to land while a dial is genuinely in flight. A duration only
// approximates that window; a channel opens and closes it exactly.
func heldWSBackend(t *testing.T) (wsURL string, opened, open *atomic.Int32, started, release chan struct{}) {
	t.Helper()
	opened, open = &atomic.Int32{}, &atomic.Int32{}
	started, release = make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		conn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		opened.Add(1)
		open.Add(1)
		defer func() {
			open.Add(-1)
			_ = conn.Close()
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), opened, open, started, release
}

type dialRecorder struct {
	calls   atomic.Int32
	lastArg atomic.Int32
}

func (d *dialRecorder) record(statusCode int, _ error) {
	d.calls.Add(1)
	d.lastArg.Store(int32(statusCode))
}

// newLifecycleBridge builds a sage-shaped bridge (no supplier header) against
// the given backend, with a recorder in the circuit breaker's seat. It does NOT
// call Run: each test drives the lifecycle itself, which is the point.
func newLifecycleBridge(t *testing.T, backendURL string) (*WebSocketBridge, *websocket.Conn, *dialRecorder, string) {
	t.Helper()
	pipeline, _, _ := newOwnerTestPipeline(t)
	supplier, signer := newSupplier(t)
	rec := &dialRecorder{}

	relayerConn, gwClient := newGatewaySideHarness(t)
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", 100,
		&recordingProcessor{}, &recordingPublisher{}, signer, http.Header{},
		nil, pipeline, 5*time.Second, false, nil, "", rec.record,
	)
	require.NoError(t, err)
	return bridge, gwClient, rec, supplier
}

// TestBridgeDoesNotDialTheBackendUntilAFrameEarnsIt is the reason the lifecycle
// has two phases.
//
// The bridge used to dial inside its constructor, before a byte had been read,
// and Run then started read loops over a connection that already existed.
// CheckOrigin accepts every origin and the handshake validation is a permissive
// log, so anyone who could complete an upgrade opened a socket against the
// operator's backend without ever sending a relay.
//
// Owner decision 2026-09-03: "en son de proteger el recurso valioso (backend,
// blockchain) no hacemos el handshake al backend hasta no tener validacion del
// supplier address, evitamos un ddos a sus backends sin relays."
func TestBridgeDoesNotDialTheBackendUntilAFrameEarnsIt(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	backendURL, dials, _ := countingWSBackend(t)
	bridge, gwClient, rec, supplier := newLifecycleBridge(t, backendURL)

	go bridge.Run()
	t.Cleanup(func() { _ = bridge.Close() })

	// NOT asserted here: "dials == 0 before the frame". There is no
	// synchronisation point between `go bridge.Run()` and the first frame, so
	// that assertion races the goroutine and passes on timing rather than on
	// behaviour -- measured: it stayed green with the prologue removed and the
	// dial put back at the top of Run. TestBridgeRefusesARawFrameBeforeAnyRelay
	// is the deterministic form of the same claim: it reads the close frame
	// back before asserting the backend was never dialled.
	sendRelay(t, gwClient, ownerTestRelay("earns-it", supplier))
	readServedResponse(t, gwClient)

	require.Equal(t, int32(1), dials.Load(),
		"exactly one backend connection, opened by the frame that earned it")
	require.Equal(t, int32(1), rec.calls.Load(),
		"the breaker is told once, when the dial actually happened")
	require.Equal(t, int32(http.StatusOK), rec.lastArg.Load())
}

// TestBridgeRefusesARawFrameBeforeAnyRelay closes the other half: a frame that
// does not parse as a RelayRequest used to be forwarded raw, so the backend
// could be pushed on a connection that never carried a relay.
func TestBridgeRefusesARawFrameBeforeAnyRelay(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	backendURL, dials, _ := countingWSBackend(t)
	bridge, gwClient, _, _ := newLifecycleBridge(t, backendURL)

	go bridge.Run()
	t.Cleanup(func() { _ = bridge.Close() })

	require.NoError(t, gwClient.WriteMessage(websocket.BinaryMessage, []byte{0xFF, 0xFF, 0xFF}))
	require.NoError(t, gwClient.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, err := gwClient.ReadMessage()
	require.Error(t, err, "a raw frame before any relay must not be served")
	require.True(t, websocket.IsCloseError(err, CloseValidationFailed),
		"it closes with the client-verdict code, got %v", err)
	require.Equal(t, int32(0), dials.Load(), "and it never reached the backend")
}

// TestBridgeClosesAConnectionThatNeverSendsAFrame pins the deadline that had to
// come with the deferred dial, and the shape that lets it be a NATIVE read
// deadline instead of a goroutine.
//
// A read deadline works here only because pingLoop has not started: gorilla's
// default ping handler answers a ping without touching the read deadline, while
// the SetPongHandler pingLoop installs REFRESHES it. So the client below keeps
// answering pings — PATH pings every 27s, sage every 20s — and still hits the
// deadline, which under the old shape needed a fifth goroutine and a flag.
func TestBridgeClosesAConnectionThatNeverSendsAFrame(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	prev := wsFirstFrameWait
	wsFirstFrameWait = 200 * time.Millisecond
	t.Cleanup(func() { wsFirstFrameWait = prev })

	backendURL, dials, _ := countingWSBackend(t)
	bridge, gwClient, rec, _ := newLifecycleBridge(t, backendURL)

	gwClient.SetPingHandler(func(appData string) error {
		return gwClient.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})

	go bridge.Run()
	t.Cleanup(func() { _ = bridge.Close() })

	require.NoError(t, gwClient.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, err := gwClient.ReadMessage()
	require.Error(t, err, "a connection that never sends a frame must be closed")
	require.True(t, websocket.IsCloseError(err, CloseTryAgainLater),
		"closed with the retryable code -- the client is idle, not at fault; got %v", err)

	require.Equal(t, int32(0), dials.Load(), "and it never cost the backend anything")
	require.Equal(t, int32(0), rec.calls.Load())
}

// TestBridgeDoesNotCloseAConnectionThatDidSendAFrame is the other side of the
// deadline, asserted separately because a deadline that fires on every
// connection would kill live subscriptions after two minutes of client silence,
// which is the NORMAL shape of eth_subscribe.
func TestBridgeDoesNotCloseAConnectionThatDidSendAFrame(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	prev := wsFirstFrameWait
	wsFirstFrameWait = 200 * time.Millisecond
	t.Cleanup(func() { wsFirstFrameWait = prev })

	backendURL, dials, _ := countingWSBackend(t)
	bridge, gwClient, _, supplier := newLifecycleBridge(t, backendURL)

	go bridge.Run()
	t.Cleanup(func() { _ = bridge.Close() })

	sendRelay(t, gwClient, ownerTestRelay("stays-open", supplier))
	readServedResponse(t, gwClient)
	require.Equal(t, int32(1), dials.Load())

	require.NoError(t, gwClient.SetReadDeadline(time.Now().Add(4*wsFirstFrameWait)))
	_, _, err := gwClient.ReadMessage()
	require.Error(t, err, "expected the read to time out, not the bridge to close")
	// isTimeout, not IsCloseError(CloseTryAgainLater): the claim is "the bridge
	// did not close this connection", and a close carries whatever code the
	// teardown picked. Asserting the ABSENCE of one specific code passes for a
	// close that used any other -- measured 2026-09-03: applying the
	// first-frame deadline inside readLoop closes with 1001, and this test
	// stayed green. Only "the read timed out" excludes every close.
	require.True(t, isTimeout(err),
		"a connection that already served a relay must not be closed at all: %v", err)
}

// TestBridgeClosesABackendDialThatLandsAfterTheBridgeClosed is the scenario the
// two-phase lifecycle exists to make impossible rather than to guard against.
//
// Under the previous shape the dial ran on the message loop with four goroutines
// already alive, so a close could land while it was in flight: the teardown read
// a nil backendConn, skipped it, and the connection that arrived a moment later
// had nobody left to close it. Here the dial happens before any loop exists and
// release runs from Run's defer, after it -- so the ordering that produced the
// leak is not representable.
func TestBridgeClosesABackendDialThatLandsAfterTheBridgeClosed(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	backendURL, openedConns, openConns, dialStarted, releaseDial := heldWSBackend(t)
	bridge, gwClient, _, supplier := newLifecycleBridge(t, backendURL)

	done := make(chan struct{})
	go func() {
		defer close(done)
		bridge.Run()
	}()

	sendRelay(t, gwClient, ownerTestRelay("dial-race", supplier))

	select {
	case <-dialStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the backend dial never started, so the window under test was never entered")
	}
	require.Equal(t, int32(0), openConns.Load(), "precondition: the upgrade has not completed yet")

	require.NoError(t, bridge.Close())
	close(releaseDial)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after Close")
	}

	// PRECONDITION, and the test is worthless without it: the backend
	// connection has to have been OPENED for "it is closed" to mean anything.
	// Asserting only that the open count is zero passes identically when the
	// dial never landed at all -- which is what the first version of this test
	// did, and it read green with the close removed from release.
	require.Eventually(t, func() bool { return openedConns.Load() == 1 },
		10*time.Second, 20*time.Millisecond,
		"the backend connection never landed, so this test proves nothing")

	// And the leak is measured on OUR side, which is where it actually is.
	// The backend's own connection count is NOT the discriminator: release
	// writes a close FRAME before closing the socket, so the backend hangs up
	// either way and its count reaches zero even when our descriptor is left
	// open. Writing to the connection afterwards is what tells them apart --
	// a closed one refuses.
	require.NotNil(t, bridge.backendConn, "the dial landed, so the bridge must be holding it")

	// SetDeadline on the underlying net.Conn is the discriminator, and picking
	// it took two wrong attempts worth recording. The backend's own connection
	// count is not it: release writes a close FRAME before closing the socket,
	// so the peer hangs up either way. Writing to the websocket is not it
	// either, for the same reason -- a peer that hung up makes the write fail
	// whether or not our descriptor is still open. Only the file descriptor
	// tells the truth: a CLOSED conn returns ErrClosed here, an open-but-dead
	// one returns nil.
	deadlineErr := bridge.backendConn.NetConn().SetDeadline(time.Now())
	require.ErrorIs(t, deadlineErr, net.ErrClosed,
		"the backend descriptor is still open, so it was leaked rather than closed: %v", deadlineErr)

	require.Eventually(t, func() bool { return openConns.Load() == 0 },
		10*time.Second, 20*time.Millisecond,
		"and the backend sees it go too")
}

// panickingValidator panics inside the gateway path, where a malformed frame
// used to take the bridge down. It stands in for the CLASS.
type panickingValidator struct{}

func (panickingValidator) ValidateRelayRequest(context.Context, *servicetypes.RelayRequest) error {
	panic("induced panic on the gateway message path")
}

func (panickingValidator) CheckRewardEligibility(context.Context, *servicetypes.RelayRequest) error {
	return nil
}
func (panickingValidator) GetCurrentBlockHeight() int64 { return 100 }
func (panickingValidator) SetCurrentBlockHeight(int64)  {}

// TestBridgeTearsDownWhenTheGatewayPathPanics pins the teardown net/http will
// not do for us.
//
// Run executes the first frame and the message loop on the hijacked handler
// goroutine, and net/http's conn.serve recovers a panic there and then, for a
// HIJACKED connection, closes nothing. A panic therefore unwound past the
// teardown: the context was never cancelled, wsConnectionsActive was never
// decremented -- wrong for the life of the process -- and the socket stayed open.
//
// Under the new shape the recover lives in release, which is a defer on Run, so
// it is not a fifth thing to remember: it is the same single teardown.
func TestBridgeTearsDownWhenTheGatewayPathPanics(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	backendURL, dials, _ := countingWSBackend(t)
	supplier, signer := newSupplier(t)
	pipeline := NewRelayPipeline(panickingValidator{}, nil, testLogger())

	before := testutil.ToFloat64(wsConnectionsActive.WithLabelValues(simWSTestService))

	relayerConn, gwClient := newGatewaySideHarness(t)
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", 100,
		&recordingProcessor{}, &recordingPublisher{}, signer, http.Header{},
		nil, pipeline, 2*time.Second, false, nil, "", nil,
	)
	require.NoError(t, err)

	done := make(chan struct{})
	panicked := make(chan any, 1)
	go func() {
		defer close(done)
		// Stands in for net/http's conn.serve, which is what catches this in
		// production. Without it a future regression kills the test binary
		// instead of failing this one test.
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		bridge.Run()
	}()

	sendRelay(t, gwClient, ownerTestRelay("panic-teardown", supplier))

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run never returned: the panic left the bridge wedged")
	}

	select {
	case r := <-panicked:
		t.Fatalf("the panic escaped Run instead of being recovered and torn down: %v", r)
	default:
	}

	require.Equal(t, before, testutil.ToFloat64(wsConnectionsActive.WithLabelValues(simWSTestService)),
		"the gauge must come back down, or it is wrong for the life of the process")
	require.Equal(t, int32(0), dials.Load(), "a frame that panicked never reached the backend")

	require.NoError(t, gwClient.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, readErr := gwClient.ReadMessage()
	require.Error(t, readErr, "and the client is told the connection is gone")
}

// pushingWSBackend answers ONE client message with `pushes` messages, which is
// the shape of a subscription: the client asks once and the backend keeps
// sending. The localnet backend does the same thing through a repeat_count
// parameter (tilt/backend-server/main.go), and the live gate does not use it --
// its load client writes once and reads once, so no gate at any level exercises
// this.
func pushingWSBackend(t *testing.T, pushes int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			mt, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			for i := 0; i < pushes; i++ {
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"sequence":%d}}`, i+1)
				if err := conn.WriteMessage(mt, []byte(body)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestBridgeBillsEverySubscriptionPush covers the case a WebSocket exists FOR,
// and that no gate at any level reaches.
//
// A subscription is one RelayRequest followed by many backend pushes, and each
// push is a billable relay: handleBackendMessage pairs every one of them with
// the SAME latestRequest and emits. The live gate's load client writes once and
// reads once, so it proves the request/response shape and says nothing about
// this one.
//
// It matters here specifically because the two-phase lifecycle moved where the
// first frame is handled: setLatestRequest for the establishing relay now runs
// in the synchronous prologue, before any loop exists. If that pairing broke,
// every push after the first would be mined against a nil or stale request --
// and the load gate would stay green.
func TestBridgeBillsEverySubscriptionPush(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const pushes = 5

	pipeline, _, _ := newOwnerTestPipeline(t)
	supplier, signer := newSupplier(t)
	proc, pub := &recordingProcessor{}, &recordingPublisher{}

	relayerConn, gwClient := newGatewaySideHarness(t)
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, pushingWSBackend(t, pushes), simWSTestService, "", 100,
		proc, pub, signer, http.Header{},
		nil, pipeline, 5*time.Second, false, nil, "", nil,
	)
	require.NoError(t, err)
	go bridge.Run()
	t.Cleanup(func() { _ = bridge.Close() })

	// ONE relay request, the subscribe.
	sendRelay(t, gwClient, ownerTestRelay("subscription", supplier))

	// Every push must come back to the client, signed.
	for i := 0; i < pushes; i++ {
		require.NoError(t, gwClient.SetReadDeadline(time.Now().Add(10*time.Second)))
		_, respBz, readErr := gwClient.ReadMessage()
		require.NoError(t, readErr, "push %d never reached the client", i+1)

		var resp servicetypes.RelayResponse
		require.NoError(t, resp.Unmarshal(respBz), "push %d must be a RelayResponse", i+1)
		require.NotEmpty(t, resp.Meta.SupplierOperatorSignature,
			"push %d must be supplier-signed", i+1)
	}

	// And every push must be BILLED: one mined relay each, not one for the
	// subscribe. emitRelay runs after the write to the client, so the counter
	// settles just behind the last read.
	require.Eventually(t, func() bool { return pub.calls.Load() == int32(pushes) },
		10*time.Second, 20*time.Millisecond,
		"expected %d billed relays for %d pushes, got %d", pushes, pushes, pub.calls.Load())
	require.Equal(t, int32(pushes), proc.calls.Load(),
		"each push is mined with its own RelayHash over {Req, Res}")
}

// ctxWatchingPublisher answers the only question the test below asks: was the
// context handed to Publish already dead?
type ctxWatchingPublisher struct {
	calls atomic.Int32
	live  atomic.Bool
}

func (p *ctxWatchingPublisher) Publish(ctx context.Context, _ *transport.MinedRelayMessage) error {
	p.calls.Add(1)
	p.live.Store(ctx.Err() == nil)
	return ctx.Err()
}

func (p *ctxWatchingPublisher) Close() error { return nil }

// TestBridgePublishesARelayItAlreadyServedAfterTheBridgeWasCancelled pins the
// accounting invariant that HTTP and gRPC already hold and the bridge did not.
//
// handleBackendMessage signs the response and writes it to the gateway BEFORE
// calling emitRelay, so by the time emitRelay runs the relay is served and the
// gateway will bill for it. Publishing it to the WAL is what turns it into an
// SMST leaf, a claim and a reward. Those two steps used to share b.ctx, and
// b.ctx is cancelled by closeWithReason -- which readLoop, pingLoop and the
// SessionMonitor callback all reach from goroutines that are NOT serialised
// with messageLoop. A cancel landing in that window made XAdd fail with
// context.Canceled before any network I/O: served, signed, never mined.
// Session rollover fires that cancel on every open connection at once.
//
// The discriminant is the STATE OF THE CONTEXT at Publish, not whether Publish
// was reached: with the defect present Publish is still called, just with a
// context that is already dead, and a stub publisher that ignores ctx would
// stay green through the whole bug.
func TestBridgePublishesARelayItAlreadyServedAfterTheBridgeWasCancelled(t *testing.T) {
	verifyNoBridgeGoroutines(t)

	backendURL, _, _ := countingWSBackend(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	supplier, signer := newSupplier(t)
	relayerConn, _ := newGatewaySideHarness(t)

	pub := &ctxWatchingPublisher{}
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", 100,
		&recordingProcessor{}, pub, signer, http.Header{},
		nil, pipeline, 5*time.Second, false, nil, "", nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bridge.Close() })

	// The frame that earned this connection already went through admission.
	bridge.owner.Store(&supplier)

	// The window: the gateway write has happened, and something outside
	// messageLoop closes the bridge before the publish runs.
	require.NoError(t, bridge.closeWithReason(CloseSessionExpired, "session expired", wsCloseInitiatorRelayer))
	require.Error(t, bridge.ctx.Err(), "the bridge context must be dead for this test to mean anything")

	bridge.emitRelay(ownerTestRelay("already-served", supplier), &servicetypes.RelayResponse{}, []byte(`{"ok":true}`))

	require.Equal(t, int32(1), pub.calls.Load(), "the served relay must still reach the WAL")
	require.True(t, pub.live.Load(),
		"the publish inherited the cancelled bridge context: a served, signed relay that never becomes an SMST leaf")
}
