//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/pool"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// queueFullRejections reads the publish_queue_full rejection counter of one series.
func queueFullRejections(serviceID, rpcType string) float64 {
	return testutil.ToFloat64(relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonPublishQueueFull))
}

// TestHTTPQueueFullRefusesBeforeTheEagerMeterCharges proves the HTTP gate sits
// before the eager meter: a relay refused because the batch queue is full is
// never charged and never reaches the backend. The second request, with the
// queue drained, is the control: the same relay DOES charge the same meter, so
// the missing counter above is the gate's doing and not a meter that was never
// live.
func TestHTTPQueueFullRefusesBeforeTheEagerMeterCharges(t *testing.T) {
	ctx := context.Background()

	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := newSimHTTPFixture(t, backend.URL, ValidationModeEager)

	meterRedis, _ := newTestRedis(t)
	app := &fakeAppClient{addr: f.appAddr}
	app.stakeUpokt.Store(1_000_000)
	meter := NewRelayMeter(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		meterRedis,
		app,
		nil,
		&fakeSessionClient{numSuppliers: 1},
		nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 1,
			ComputeUnitCostGranularity:     1,
		}},
		nil,
		staticServiceFactor{f: 1},
		RelayMeterConfig{},
	)
	require.NoError(t, meter.Start(ctx))
	t.Cleanup(func() { _ = meter.Close() })
	newChargeWriter(t, meter, meterRedis)
	f.proxy.SetRelayMeter(meter)

	// Rejects every relay that reaches it, so the control stops right after the
	// meter instead of running the publish path this fixture does not wire.
	validator := &neverCallValidator{}
	f.proxy.validator = validator

	var full atomic.Bool
	full.Store(true)
	f.proxy.SetPublishQueueFull(full.Load)

	const sessionID = "sess-publish-queue-full"
	body := f.buildSignedSimBody(t, f.appAddr, simTestService, sessionID)
	admitted := eagerAdmissions(simTestService)
	before := queueFullRejections(simTestService, BackendTypeJSONRPC)

	w := f.post(t, body, false)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
	require.Equal(t, before+1, queueFullRejections(simTestService, BackendTypeJSONRPC))
	require.Equal(t, admitted, eagerAdmissions(simTestService), "a relay refused by the queue gate must not reach the meter")
	require.Equal(t, int32(0), validator.calls.Load(), "a refused relay must not reach validation")
	require.Equal(t, int32(0), backendHits.Load(), "a refused relay must not reach the backend")
	require.Equal(t, int32(0), f.pub.calls.Load())

	full.Store(false)
	w = f.post(t, body, false)

	require.Equal(t, http.StatusForbidden, w.Code, "control: the relay passes the gate, is admitted by the meter, then fails validation; body=%s", w.Body.String())
	require.Equal(t, int32(1), validator.calls.Load())
	require.Equal(t, admitted+1, eagerAdmissions(simTestService), "control: with the queue drained the same relay must reach the eager meter")
	require.Equal(t, before+1, queueFullRejections(simTestService, BackendTypeJSONRPC), "control: the gate must not count an admitted relay")
}

// TestGRPCQueueFullRefusesBeforeTheBackend proves the gRPC gate refuses with
// Unavailable before the backend, mining and publishing, and without sending the
// client a relay response.
func TestGRPCQueueFullRefusesBeforeTheBackend(t *testing.T) {
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	fx := newGRPCPublishFixture(t, backend.URL)
	fx.svc.publishQueueFull = func() bool { return true }
	before := queueFullRejections(fx.serviceID, BackendTypeGRPC)

	err := fx.svc.handleSendRelay(fx.stream)

	require.Equal(t, codes.Unavailable, status.Code(err), "err=%v", err)
	require.Equal(t, before+1, queueFullRejections(fx.serviceID, BackendTypeGRPC))
	require.Equal(t, int32(0), backendHits.Load(), "a refused relay must not reach the backend")
	require.Equal(t, int32(0), fx.proc.calls.Load())
	require.Equal(t, int32(0), fx.pub.calls.Load())
	require.Empty(t, fx.stream.sent)
}

// TestGRPCServiceSeesTheProxyQueueGate proves InitGRPCHandler hands the service
// the proxy's gate, and that the gate is read when a relay arrives rather than
// copied at init: set after InitGRPCHandler, the service still sees it.
func TestGRPCServiceSeesTheProxyQueueGate(t *testing.T) {
	p := &ProxyServer{
		logger:        testLogger(),
		config:        &Config{Services: map[string]ServiceConfig{"svc": {}}},
		relayPipeline: NewRelayPipeline(acceptAnyValidator{}, nil, testLogger()),
	}
	require.NoError(t, p.InitGRPCHandler())
	require.NotNil(t, p.grpcRelayService.publishQueueFull, "InitGRPCHandler must wire the queue gate")
	require.False(t, p.grpcRelayService.publishQueueFull(), "no gate set admits everything")

	p.SetPublishQueueFull(func() bool { return true })
	require.True(t, p.grpcRelayService.publishQueueFull(), "a gate set after init must still be seen")
}

// TestWebSocketQueueFullRefusesTheUpgrade proves the WebSocket gate answers a
// new connection with HTTP 503 before the upgrade.
func TestWebSocketQueueFullRefusesTheUpgrade(t *testing.T) {
	ep, err := pool.NewBackendEndpoint("ws1", "ws://unreachable.invalid:8545")
	require.NoError(t, err)
	require.True(t, ep.IsHealthy(), "precondition: the fast-fail check must let this through")
	wsPool := pool.NewPool(
		"develop-websocket:websocket",
		[]*pool.BackendEndpoint{ep},
		&pool.FirstHealthySelector{},
		"first_healthy(test)",
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
	proxy.SetPublishQueueFull(func() bool { return true })

	srv := wsTestServer(t, proxy.WebSocketHandler())
	before := queueFullRejections("develop-websocket", BackendTypeWebSocket)

	headers := http.Header{}
	headers.Set("Target-Service-Id", "develop-websocket")
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), headers)
	if conn != nil {
		_ = conn.Close()
	}
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "the connection must not be upgraded")
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, before+1, queueFullRejections("develop-websocket", BackendTypeWebSocket))
}

// eagerAdmissions counts the relays the meter admitted within their budget. A
// relay that fails validation after admission gives its reservation back, so the
// consumed counter cannot show that it reached the meter; this series can.
func eagerAdmissions(serviceID string) float64 {
	return testutil.ToFloat64(relayMeterConsumptions.WithLabelValues(serviceID, "within_limit"))
}
