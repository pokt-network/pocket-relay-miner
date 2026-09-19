//go:build test

package relayer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/require"
)

// The relayer's shutdown used to return without waiting for anything: p.wg
// covers only the ListenAndServe goroutine, and ListenAndServe returns
// immediately when Shutdown is called. WebSocket bridges are worse than merely
// unwaited -- they are hijacked connections, so http.Server.Shutdown does not
// track them at all, and they are the transport that keeps publishing longest.
//
// These tests are about the drain itself. The server is a bare http.Server on
// purpose: Shutdown on one that never served returns at once, which leaves the
// bridge counter as the only thing the test is measuring.

func drainTestProxy(t *testing.T) *ProxyServer {
	t.Helper()
	return &ProxyServer{
		logger:  testLogger(),
		server:  &http.Server{},
		bridges: xsync.NewMap[*WebSocketBridge, struct{}](),
	}
}

// deadlineGate holds the gateway connection's FIRST future read deadline until
// the test lets it through, and reports the shutdown's nudge on its way past.
//
// Those two writes are the whole of the race this file used to leave open.
// awaitFirstFrame arms a deadline firstFrameWait away; closeWithReason nudges
// the deadline to now so a parked read returns. Whichever runs LAST decides:
// arm-then-nudge unblocks the bridge on its own, nudge-then-arm erases the
// signal and the bridge waits out two minutes. A test that does not fix that
// order measures whichever order the scheduler happened to pick -- r1 measured
// this tooth green in 3 runs of 5 for exactly that reason.
//
// The gate pins the order that needs the fix: reached says the bridge is ON the
// arming line and has not run it, nudged says the shutdown has already landed.
type deadlineGate struct {
	net.Conn
	armed     atomic.Bool
	reached   chan struct{}
	proceed   chan struct{}
	nudged    chan struct{}
	gateOnce  sync.Once
	nudgeOnce sync.Once
	openOnce  sync.Once
}

// arm starts observing. The upgrade sets deadlines of its own before the
// connection is hijacked, and none of them are the line under test.
func (g *deadlineGate) arm() { g.armed.Store(true) }

// open releases a held bridge. Idempotent because both the test and its cleanup
// call it: a bridge left parked in the gate is a leaked goroutine holding a
// socket, and a failed assertion returns through the cleanup.
func (g *deadlineGate) open() { g.openOnce.Do(func() { close(g.proceed) }) }

func (g *deadlineGate) SetReadDeadline(t time.Time) error {
	if !g.armed.Load() {
		return g.Conn.SetReadDeadline(t)
	}
	if t.After(time.Now()) {
		g.gateOnce.Do(func() {
			close(g.reached)
			<-g.proceed
		})
		return g.Conn.SetReadDeadline(t)
	}
	// A deadline in the past is the shutdown nudge, never the bridge arming
	// itself: firstFrameWait is two minutes.
	//
	// Applied to the socket FIRST and announced AFTER. Announced first -- which
	// is what this did -- the test is free to release the held future deadline
	// while this call has not yet reached the socket, and the two writes then
	// race: the future deadline can land LAST, which is the one order that does
	// not need the fix at all. Measured at -count=20: 1 escape in 20, passing in
	// the control's own time because the bridge simply left.
	err := g.Conn.SetReadDeadline(t)
	g.nudgeOnce.Do(func() { close(g.nudged) })
	return err
}

// gatingListener puts a gate around every connection it accepts. The upgrade
// hijacks exactly this net.Conn, so the bridge's gateway connection writes its
// read deadlines through the gate.
type gatingListener struct {
	net.Listener
	gates chan *deadlineGate
}

func (l *gatingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	g := &deadlineGate{
		Conn:    c,
		reached: make(chan struct{}),
		proceed: make(chan struct{}),
		nudged:  make(chan struct{}),
	}
	select {
	case l.gates <- g:
	default:
	}
	return g, nil
}

// newGatedLifecycleBridge is newLifecycleBridge with the relayer-side socket
// behind a deadlineGate. The rest of the wiring is deliberately identical -- a
// real listener, a real upgrade, a real hijacked socket -- because the read
// deadline is the only thing this test needs to order.
func newGatedLifecycleBridge(t *testing.T, backendURL string) (*WebSocketBridge, *deadlineGate) {
	t.Helper()

	gates := make(chan *deadlineGate, 1)
	connCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCh <- c
	}))
	srv.Listener = &gatingListener{Listener: srv.Listener, gates: gates}
	srv.Start()
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	// Closed by the cleanup and NEVER during the test: closing it would end the
	// bridge's read on its own, which is the very thing being measured.
	t.Cleanup(func() { _ = client.Close() })

	var relayerConn *websocket.Conn
	select {
	case relayerConn = <-connCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for gateway-side upgrade")
	}

	var gate *deadlineGate
	select {
	case gate = <-gates:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never handed over a gated connection")
	}
	t.Cleanup(gate.open)

	pipeline, _, _ := newOwnerTestPipeline(t)
	_, signer := newSupplier(t)
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", atHeight(100),
		&recordingProcessor{}, &recordingPublisher{}, signer, http.Header{},
		nil, pipeline, 5*time.Second, false, nil, "", func(int, error) {},
	)
	require.NoError(t, err)
	return bridge, gate
}

// TestCloseWaitsForALiveBridge is criterion 2: Close must not return while a
// bridge can still publish.
//
// Run() returning is the exact line that matters -- it fires after release(),
// which closes the connections and then waits on the bridge's own WaitGroup, so
// it is the first moment at which that bridge cannot produce another relay.
func TestCloseWaitsForALiveBridge(t *testing.T) {
	backendURL, _, _ := countingWSBackend(t)
	bridge, gate := newGatedLifecycleBridge(t, backendURL)
	t.Cleanup(func() { _ = bridge.Close() })
	p := drainTestProxy(t)

	require.True(t, p.trackBridge(bridge), "an open proxy must admit a bridge")

	gate.arm()
	var runReturned atomic.Bool
	go func() {
		defer p.untrackBridge(bridge)
		defer runReturned.Store(true)
		bridge.Run() // ends when the drain signals it
	}()

	// Park the bridge ON the line that arms its first-frame deadline. Held here
	// it has not yet erased anything, so the shutdown below is guaranteed to
	// reach it FIRST -- the order that needs the fix, and the one a duration
	// cannot pin.
	select {
	case <-gate.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge never armed its first-frame deadline")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	closeErr := make(chan error, 1)
	go func() { closeErr <- p.Close(ctx) }()

	// Released only once the nudge has landed, so the deadline the bridge is
	// about to arm lands ON TOP of it: from here the bridge can only leave by
	// re-reading its context.
	select {
	case <-gate.nudged:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain never signalled the bridge")
	}
	gate.open()

	require.NoError(t, <-closeErr)

	require.True(t, runReturned.Load(),
		"Close returned while a bridge was still running: that bridge can still serve and "+
			"publish a relay, and the caller is about to close the publisher under it")
	require.Zero(t, p.bridges.Size(), "a drained proxy must hold no bridges")
}

// TestCloseIsBoundedByItsDeadline is criteria 3 and 4 together.
//
// A bridge that never finishes must cost the shutdown its BUDGET and not its
// own timeout: left alone, one parked in awaitFirstFrame is bounded only by
// wsFirstFrameWait, which is two minutes -- four times the budget the relayer
// gives its whole shutdown.
func TestCloseIsBoundedByItsDeadline(t *testing.T) {
	backendURL, _, _ := countingWSBackend(t)
	bridge, _, _, _ := newLifecycleBridge(t, backendURL)
	t.Cleanup(func() { _ = bridge.Close() })
	p := drainTestProxy(t)

	// Tracked and never untracked by a Run of its own: the bridge the drain
	// cannot wait out. Releasing it afterwards is the test's job -- production
	// releases it because the cut closes its connections and Run returns.
	require.True(t, p.trackBridge(bridge))
	t.Cleanup(func() { p.untrackBridge(bridge) })

	const budget = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	require.NoError(t, p.Close(ctx))
	elapsed := time.Since(start)

	require.Less(t, elapsed, 5*time.Second,
		"Close must give up at its deadline; waiting on the bridge's own timeout would be "+
			"wsFirstFrameWait, four times the whole shutdown budget")
	require.GreaterOrEqual(t, elapsed, budget,
		"and it must actually wait the budget, not skip the drain")
}

// TestABridgeArrivingWhileClosingIsRefused is criterion 5, and the reason the
// Add sits under the same mutex that flips closed.
//
// sync.WaitGroup documents that an Add racing a Wait which already reached zero
// is misuse. It panics, in the shutdown path, where a panic is the whole process.
func TestABridgeArrivingWhileClosingIsRefused(t *testing.T) {
	backendURL, _, _ := countingWSBackend(t)
	bridge, _, _, _ := newLifecycleBridge(t, backendURL)
	t.Cleanup(func() { _ = bridge.Close() })
	p := drainTestProxy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, p.Close(ctx))

	require.False(t, p.trackBridge(bridge),
		"a handshake that lands while the proxy is draining must be refused, not counted")
	require.Zero(t, p.bridges.Size())
}

// TestAHandshakeDoesNotBlockWhileTheDrainWaits is criterion 6, and it is the
// only thing standing between the explicit Unlock in Close and the next cleanup
// that puts `defer p.mu.Unlock()` back where it used to be.
//
// The comment on that Unlock names the danger, and until this test nothing
// exercised it: measured by r1 against the frozen tree, restoring the defer left
// RUN=20 PASS=20 FAIL=0 with no panic and no DATA RACE, and the whole package
// green. The reason is untrackBridge, which takes no mutex at all -- a bridge
// FINISHING never blocks, so only a handshake ARRIVING can meet the deadlock,
// and TestABridgeArrivingWhileClosingIsRefused calls trackBridge after Close has
// already returned.
func TestAHandshakeDoesNotBlockWhileTheDrainWaits(t *testing.T) {
	backendURL, _, _ := countingWSBackend(t)
	bridge, gate := newGatedLifecycleBridge(t, backendURL)
	t.Cleanup(func() { _ = bridge.Close() })
	p := drainTestProxy(t)

	// Tracked and never Run, so nothing untracks it: the drain sits on
	// bridgeWG.Wait() for its whole budget, and that wait IS the window a
	// handshake has to land in.
	require.True(t, p.trackBridge(bridge))
	t.Cleanup(func() { p.untrackBridge(bridge) })

	gate.arm()

	const budget = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	closeErr := make(chan error, 1)
	go func() { closeErr <- p.Close(ctx) }()

	// The nudge proves Close is PAST the mutex and inside the drain: signalBridges
	// runs there, and closeWithReason pushes the read deadline to now. Without
	// waiting for it the handshake below could arrive before Close had taken the
	// mutex at all, and would be answered by a proxy that is simply still open --
	// a green that proves nothing.
	select {
	case <-gate.nudged:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain never signalled the bridge")
	}

	// A zero bridge is enough: a closed proxy refuses it before it is ever
	// stored, so what is measured here is whether the call RETURNS.
	admitted := make(chan bool, 1)
	go func() { admitted <- p.trackBridge(&WebSocketBridge{}) }()

	select {
	case ok := <-admitted:
		require.False(t, ok, "a handshake landing while the proxy drains must be refused")
	case <-time.After(budget / 3):
		t.Fatal("trackBridge blocked while the drain was waiting: Close is holding p.mu across " +
			"the wait, so a handshake in flight cannot be answered until the budget expires")
	}

	require.NoError(t, <-closeErr)
}

// closeErrListener is a listener whose Close reports a failure, which is what a
// real one does when it is closed twice.
//
// Its Accept also reports that Serve reached the accept loop. Serve registers
// the listener -- trackListener(&l, true) at net/http/server.go:3417 -- BEFORE
// the accept loop at :3433, so a drain that starts after this signal is guaranteed to
// find a listener to close and therefore to see the error. Without it the drain
// can run first, Shutdown closes nothing, returns nil, and the injection this
// test exists to catch is never exercised: r1 measured it green 2 runs in 5.
type closeErrListener struct {
	net.Listener
	err        error
	accepting  chan struct{}
	acceptOnce sync.Once
}

func (l *closeErrListener) Accept() (net.Conn, error) {
	l.acceptOnce.Do(func() { close(l.accepting) })
	return l.Listener.Accept()
}

func (l *closeErrListener) Close() error {
	_ = l.Listener.Close()
	return l.err
}

// TestDrainDoesNotCutBecauseShutdownReturnedAnError is the short circuit that
// never shows on the happy path.
//
// http.Server.Shutdown can return non-nil on a drain that finished perfectly:
// it ends with `if s.closeIdleConns() { return lnerr }`, and lnerr comes from
// closing every listener still in s.listeners. A listener leaves that map only
// when Serve returns, so the Shutdown goroutine in Start racing this one closes
// the same listener twice and collects "use of closed network connection".
//
// Deciding to cut on that error cuts healthy gRPC streams because a listener was
// closed twice. The decision is the BUDGET: did the waits finish first.
func TestDrainDoesNotCutBecauseShutdownReturnedAnError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	sentinel := errors.New("use of closed network connection")
	errLn := &closeErrListener{Listener: ln, err: sentinel, accepting: make(chan struct{})}
	p := drainTestProxy(t)
	go func() { _ = p.server.Serve(errLn) }()

	select {
	case <-errLn.accepting:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never reached its accept loop, so Shutdown would have no listener to close")
	}

	// Nothing is in flight, so the drain finishes well inside the budget -- and
	// Shutdown still returns the listener's error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.False(t, p.drain(ctx),
		"the drain finished inside its budget, so nothing may be cut: a failing listener "+
			"Close says nothing about whether work is still in flight")
	require.NoError(t, ctx.Err(), "the budget must not have expired, or this proves nothing")
}
