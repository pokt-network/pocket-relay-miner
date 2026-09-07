//go:build test

package relayer

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// relays_served_total is the ONE series for "a relay reached the client", and
// these tests exist because a single total cannot tell a connected transport
// from a disconnected one. Each asserts the series carrying ITS OWN rpc_type
// moved -- a test on the total passes while a transport is silently missing,
// which is exactly the state this metric was in: gRPC and WebSocket each kept a
// private counter, so `sum(relays_served_total)` read 288 against the miner's
// 768 and looked like 480 lost relays.
//
// Reading the wrong series does not look like an error. It looks like loss.

func servedCount(t *testing.T, serviceID, rpcType, statusCode string) float64 {
	t.Helper()
	return testutil.ToFloat64(relaysServed.WithLabelValues(serviceID, rpcType, statusCode))
}

// TestRelaysServed_WebSocketCountsUnderItsOwnTransport drives the real bridge
// path. emitRelay is called by handleBackendMessage on the line after the
// gateway write, and only when that write succeeded, so reaching it means the
// client has the response -- that is what "served" means here.
func TestRelaysServed_WebSocketCountsUnderItsOwnTransport(t *testing.T) {
	verifyNoBridgeGoroutines(t)

	backendURL, _, _ := countingWSBackend(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	supplier, signer := newSupplier(t)
	relayerConn, _ := newGatewaySideHarness(t)

	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", 100,
		&recordingProcessor{}, &ctxWatchingPublisher{}, signer, http.Header{},
		nil, pipeline, 5*time.Second, false, nil, "", nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bridge.Close() })
	bridge.owner.Store(&supplier)

	before := servedCount(t, simWSTestService, BackendTypeWebSocket, statusCodeNoHTTP)

	bridge.emitRelay(ownerTestRelay("ws-served", supplier), &servicetypes.RelayResponse{}, []byte(`{"ok":true}`))

	after := servedCount(t, simWSTestService, BackendTypeWebSocket, statusCodeNoHTTP)
	require.Equal(t, before+1, after,
		"a WebSocket relay written to the client must count under rpc_type=websocket; "+
			"without this the transport is invisible in relays_served_total and reads as loss")
}

// TestRelaysServed_GRPCCountsUnderItsOwnTransport is the twin, through the real
// handler. It also pins the placement: the increment sits after SendMsg, so a
// relay whose publish later fails is still in the drop rate's denominator.
func TestRelaysServed_GRPCCountsUnderItsOwnTransport(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x10","id":1}`))
	}))
	defer backend.Close()

	fx := newGRPCPublishFixture(t, backend.URL)

	before := servedCount(t, "develop-http", BackendTypeGRPC, statusCodeNoHTTP)

	require.NoError(t, fx.svc.handleSendRelay(fx.stream))

	after := servedCount(t, "develop-http", BackendTypeGRPC, statusCodeNoHTTP)
	require.Equal(t, before+1, after,
		"a gRPC relay sent to the client must count under rpc_type=grpc")
}

// TestRelaysServed_NoParallelPerTransportCounter freezes Jorge's decision so it
// cannot regress in silence: ONE series for "served", with the protocol as a
// LABEL, not one series per protocol.
//
// It reads the SOURCE and not the runtime registry, and that is not a
// convenience. The first version scraped the registry, and a Prometheus
// CounterVec with no observed label values is ABSENT from the exposition: run
// alone it examined ZERO families and passed, and run beside the two tests above
// it examined ten. A guard whose coverage depends on which other tests ran is
// worse than none -- it reports green over a set it never looked at. Measured,
// not reasoned: 43 HELP lines alone, 48 together.
//
// It is an allowlist and not a pattern, because no textual property separates
// the good from the bad: several legitimate families say "served" in their help
// (undeclared transport, boot-window optimistic, dropped, unmetered,
// reward-ineligible), and two per-transport families are deliberately KEPT --
// grpc_relays_published_total and websocket_relays_emitted_total count the
// PUBLISHED hop, the other end of the drop rate, not a duplicate of served.
// Freezing the set with a reason per entry is the idiom internal/conventions
// already uses for the bare `go` statements.
//
// The cost of not having this is measured: with a private counter per transport,
// sum(relays_served_total) read 288 against the miner's 768 and looked like 480
// lost relays. Reading the wrong series does not look like an error. It looks
// like loss.
func TestRelaysServed_NoParallelPerTransportCounter(t *testing.T) {
	// name -> why it may exist alongside relays_served_total.
	allowed := map[string]string{
		"relays_served_total":                "THE canonical served counter; rpc_type is a label ON it, not a series beside it",
		"relays_served_optimistically_total": "a SUBSET of served (owned supplier not yet in the registry), not a second total",
		"grpc_relays_published_total":        "PUBLISHED hop, not served: the other end of the drop rate",
		"websocket_relays_emitted_total":     "PUBLISHED hop for WebSocket, same reason",
		"relays_published_total":             "mined relays reaching the store: a later hop still",
		"simulated_relays_total":             "simulated traffic, isolated from every real counter by contract (docs/simulated-relays.md)",
		"relays_received_total":              "inbound, a different event from served",
		"relays_rejected_total":              "refused BEFORE serving",
		"relays_dropped_total":               "served but not mined",
		"relays_not_rewardable_total":        "served but past the session grace period",
		"relays_skipped_difficulty_total":    "served and below mining difficulty",
		"relays_mined_total":                 "met difficulty and was mined",
	}

	src, err := os.ReadFile("metrics.go")
	require.NoError(t, err)

	names := regexp.MustCompile(`Name:\s+"([a-z0-9_]+)"`).FindAllStringSubmatch(string(src), -1)

	// A guard that examined nothing is the failure this test was rewritten to
	// remove. Assert it looked, before asserting what it found.
	require.Greater(t, len(names), 20,
		"the scan found almost no metric names -- the pattern stopped matching and this guard is examining nothing")

	examined := 0
	for _, m := range names {
		name := m[1]
		if !strings.Contains(name, "relays") {
			continue
		}
		examined++
		if _, ok := allowed[name]; !ok {
			t.Errorf(
				"metric %q counts relays and is not in the frozen set. If it counts SERVED relays it "+
					"must not exist -- served is ONE series with rpc_type as a label. If it counts a "+
					"different hop, add it here WITH the reason: that sentence is the whole guard.",
				name,
			)
		}
	}
	require.Greater(t, examined, 5, "no relay counters were examined; the filter is wrong")
}

// TestRelaysServed_SimulatedWebSocketRelayDoesNotCount holds the isolation
// contract on the transport that gets it by PLACEMENT rather than by structure.
//
// docs/simulated-relays.md is explicit that a simulated relay "never touches the
// counters that measure real traffic". HTTP satisfies that structurally --
// proxy.go:903 diverts to serveSimulatedHTTP, a different function, so the real
// counter is unreachable -- and gRPC satisfies it because its increment sits in
// handleSendRelay and not in serveSimulatedGRPC. WebSocket has neither defence:
// emitRelay is ONE function that serves both, and the only thing separating them
// is where the increment sits relative to the `if b.simulated` return.
//
// The first version of this change put it above that guard. The three tests
// beside this one all stayed green, because none of them drives a simulated
// bridge -- the contract was held by a comment.
func TestRelaysServed_SimulatedWebSocketRelayDoesNotCount(t *testing.T) {
	verifyNoBridgeGoroutines(t)

	backendURL, _, _ := countingWSBackend(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	supplier, signer := newSupplier(t)
	relayerConn, _ := newGatewaySideHarness(t)

	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", 100,
		&recordingProcessor{}, &ctxWatchingPublisher{}, signer, http.Header{},
		nil, pipeline, 5*time.Second, true /* simulated */, nil, "", nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bridge.Close() })
	bridge.owner.Store(&supplier)

	before := servedCount(t, simWSTestService, BackendTypeWebSocket, statusCodeNoHTTP)

	bridge.emitRelay(ownerTestRelay("ws-simulated", supplier), &servicetypes.RelayResponse{}, []byte(`{"ok":true}`))

	after := servedCount(t, simWSTestService, BackendTypeWebSocket, statusCodeNoHTTP)
	require.Equal(t, before, after,
		"a SIMULATED WebSocket relay must leave relays_served_total flat; "+
			"docs/simulated-relays.md makes that a contract, not a preference")
}
