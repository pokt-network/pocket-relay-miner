//go:build test

package relayer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/pool"
)

// nilSelector reports "no endpoint" while the pool's endpoints are healthy.
//
// That is not a contrived state: HasHealthy() is checked BEFORE the upgrade and
// Next() is called AFTER it, so a health flip landing between those two lines
// produces exactly this. The selector reproduces it deterministically instead of
// racing a health check.
type nilSelector struct{}

func (nilSelector) Select([]*pool.BackendEndpoint) int { return -1 }

// TestWebSocketHandlerClosesAnUpgradedConnectionItDoesNotHandOff pins the class
// of defect that four separate point fixes inside the bridge did not cover: an
// early return AFTER the connection is hijacked.
//
// Once Upgrade() succeeds the ResponseWriter is dead -- http.Error writes
// nowhere and net/http logs "WriteHeader on hijacked connection" -- so a bare
// `return` left the client holding an open WebSocket that nothing would ever
// read or close.
//
// The test asserts what the CLIENT observes, because that is the only thing that
// distinguishes "closed" from "leaked": a leaked connection leaves the read
// blocking until the deadline, an owned one fails immediately.
func TestWebSocketHandlerClosesAnUpgradedConnectionItDoesNotHandOff(t *testing.T) {
	verifyNoBridgeGoroutines(t)

	ep, err := pool.NewBackendEndpoint("ws1", "ws://unreachable.invalid:8545")
	require.NoError(t, err)
	// Healthy, so the pre-upgrade fast-fail passes and the handler upgrades.
	require.True(t, ep.IsHealthy(), "precondition: the fast-fail check must let this through")

	wsPool := pool.NewPool(
		"develop-websocket:websocket",
		[]*pool.BackendEndpoint{ep},
		nilSelector{},
		"nil(test)",
	)

	proxy := &ProxyServer{
		logger: testLogger(),
		config: &Config{
			Services: map[string]ServiceConfig{"develop-websocket": {}},
			pools:    map[string]*pool.Pool{"develop-websocket:websocket": wsPool},
		},
		responseSigner: &ResponseSigner{},
		relayProcessor: &noopRelayProcessor{},
		publisher:      &noopPublisher{},
	}

	srv := httptest.NewServer(proxy.WebSocketHandler())
	t.Cleanup(srv.Close)

	headers := http.Header{}
	headers.Set("Target-Service-Id", "develop-websocket")
	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http"), headers)
	require.NoError(t, err, "the handshake must succeed: the leak only exists AFTER the upgrade")
	t.Cleanup(func() { _ = conn.Close() })

	// A leaked connection stays open and this read blocks until the deadline.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, readErr := conn.ReadMessage()
	require.Error(t, readErr, "the relayer must close a connection it upgraded and then abandoned")
	require.False(t, isTimeout(readErr),
		"the read timed out rather than being closed, which is the leak: %v", readErr)
}

// TestWebSocketHandlerClosesAConnectionItRefusesWhileDraining is the same class
// of defect one step deeper: an early return after the hijack AND after the
// bridge exists.
//
// The shutdown drain added that return. A handshake landing once the proxy is
// closing is refused rather than counted, because an Add after the drain's Wait
// already reached zero is documented misuse of sync.WaitGroup. Nothing on that
// path releases anything: Run never starts, so its deferred release -- the only
// code that closes these sockets -- never runs, and closeWithReason and Close
// only SIGNAL. So the connection can only fall to the handler's own defer, and
// that defer fires solely while ownership has not yet been transferred.
//
// Measured: with `bridgeOwnsConn = true` above the guard, this read times out.
func TestWebSocketHandlerClosesAConnectionItRefusesWhileDraining(t *testing.T) {
	verifyNoBridgeGoroutines(t)

	ep, err := pool.NewBackendEndpoint("ws1", "ws://unreachable.invalid:8545")
	require.NoError(t, err)
	require.True(t, ep.IsHealthy(), "precondition: the fast-fail check must let this through")

	// A selector that RETURNS an endpoint, unlike the sibling above: this test
	// has to reach the bridge, which is a hundred lines past the upgrade.
	wsPool := pool.NewPool(
		"develop-websocket:websocket",
		[]*pool.BackendEndpoint{ep},
		&pool.FirstHealthySelector{},
		"first-healthy(test)",
	)

	proxy := &ProxyServer{
		logger: testLogger(),
		config: &Config{
			Services: map[string]ServiceConfig{"develop-websocket": {}},
			pools:    map[string]*pool.Pool{"develop-websocket:websocket": wsPool},
		},
		responseSigner: &ResponseSigner{},
		relayProcessor: &noopRelayProcessor{},
		publisher:      &noopPublisher{},
		bridges:        xsync.NewMap[*WebSocketBridge, struct{}](),
	}
	// Already draining when the handshake lands. Set here rather than by calling
	// Close: Close would need a server, the subpools and a budget, and none of
	// them decide this branch -- p.closed alone does.
	proxy.closed = true

	srv := httptest.NewServer(proxy.WebSocketHandler())
	t.Cleanup(srv.Close)

	headers := http.Header{}
	headers.Set("Target-Service-Id", "develop-websocket")
	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http"), headers)
	require.NoError(t, err, "the handshake must succeed: the leak only exists AFTER the upgrade")
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, readErr := conn.ReadMessage()
	require.Error(t, readErr,
		"a handshake refused by the drain must not leave the client holding an open socket")
	require.False(t, isTimeout(readErr),
		"the read timed out rather than being closed, which is the leak: %v", readErr)

	require.Zero(t, proxy.bridges.Size(), "a refused bridge must not be counted")
}

func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	t, ok := err.(timeout)
	return ok && t.Timeout()
}
