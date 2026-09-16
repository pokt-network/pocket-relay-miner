//go:build test

package relayer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/pool"
)

func storageRejections(serviceID, rpcType string) float64 {
	return testutil.ToFloat64(relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonStorageSaturated))
}

func TestHTTPStorageSaturatedRefusesWith429BeforeReadingTheBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	f := newSimHTTPFixture(t, backend.URL, ValidationModeEager)

	var operable atomic.Bool
	f.proxy.storeOperable = operable.Load
	var queueAsked atomic.Int32
	f.proxy.SetPublishQueueFull(func() bool { queueAsked.Add(1); return false })

	// Not a relay at all: read and parsed, it is refused as invalid.
	garbage := []byte("not a relay request")
	saturated := storageRejections(metricLabelUnknown, metricLabelUnknown)
	invalid := testutil.ToFloat64(relaysRejected.WithLabelValues(metricLabelUnknown, metricLabelUnknown, rejectReasonInvalidRelayRequest))

	w := f.post(t, garbage, false)
	require.Equal(t, http.StatusTooManyRequests, w.Code,
		"LINK http-first: with the store not operable the answer is 429; body=%s", w.Body.String())
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	require.Equal(t, saturated+1, storageRejections(metricLabelUnknown, metricLabelUnknown))
	require.Equal(t, invalid,
		testutil.ToFloat64(relaysRejected.WithLabelValues(metricLabelUnknown, metricLabelUnknown, rejectReasonInvalidRelayRequest)),
		"the body was never read: nothing parsed it")
	require.Zero(t, queueAsked.Load(), "the queue gate was not reached")
	require.Zero(t, f.pub.calls.Load())

	operable.Store(true)
	w = f.post(t, garbage, false)
	require.Equal(t, http.StatusBadRequest, w.Code, "control: with the store operable the same request is read and refused as invalid")
	require.Equal(t, saturated+1, storageRejections(metricLabelUnknown, metricLabelUnknown))
}

func TestGRPCStorageSaturatedRefusesWithResourceExhaustedBeforeAnythingElse(t *testing.T) {
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	fx := newGRPCPublishFixture(t, backend.URL)
	fx.svc.storeSaturated = func() bool { return true }
	var queueAsked atomic.Int32
	fx.svc.publishQueueFull = func() bool { queueAsked.Add(1); return false }
	before := storageRejections(metricLabelUnknown, BackendTypeGRPC)

	err := fx.svc.handleSendRelay(fx.stream)

	require.Equal(t, codes.ResourceExhausted, status.Code(err), "LINK grpc-first: err=%v", err)
	require.Equal(t, before+1, storageRejections(metricLabelUnknown, BackendTypeGRPC))
	require.Zero(t, queueAsked.Load(), "the queue gate was not reached")
	require.Zero(t, backendHits.Load())
	require.Zero(t, fx.proc.calls.Load())
	require.Zero(t, fx.pub.calls.Load())
	require.Empty(t, fx.stream.sent)
}

func TestGRPCServiceSeesTheProxyStorageGate(t *testing.T) {
	p := &ProxyServer{
		logger:        testLogger(),
		config:        &Config{Services: map[string]ServiceConfig{"svc": {}}},
		relayPipeline: NewRelayPipeline(acceptAnyValidator{}, nil, testLogger()),
	}
	require.NoError(t, p.InitGRPCHandler())
	require.False(t, p.grpcRelayService.storeSaturated(), "no store health admits everything")
	p.storeOperable = func() bool { return false }
	require.True(t, p.grpcRelayService.storeSaturated(), "LINK grpc-wiring: the gate set after init is still seen")
}

func TestGRPCALiveRelayIsCutWhenTheStoreCloses(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	defer close(release)

	fx := newGRPCPublishFixture(t, backend.URL)
	cut := testutil.ToFloat64(liveConnectionsCut.WithLabelValues(BackendTypeGRPC))
	p := &ProxyServer{logger: testLogger(), grpcRelayService: fx.svc, bridges: drainTestProxy(t).bridges}

	result := make(chan error, 1)
	go func() { result <- fx.svc.handleSendRelay(fx.stream) }()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay never reached the backend")
	}

	p.cutLiveConnections()

	select {
	case err := <-result:
		require.Equal(t, codes.ResourceExhausted, status.Code(err), "LINK grpc-cut: a live relay is cut; err=%v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("LINK grpc-cut: the live relay was not cut")
	}
	require.Equal(t, cut+1, testutil.ToFloat64(liveConnectionsCut.WithLabelValues(BackendTypeGRPC)))
	require.Zero(t, fx.pub.calls.Load(), "a cut relay publishes nothing")
	require.Zero(t, fx.svc.cutLiveRelays(errStorageSaturated), "a finished relay is no longer tracked")
}

func TestWebSocketStorageSaturatedRefusesTheUpgradeWith429(t *testing.T) {
	ep, err := pool.NewBackendEndpoint("ws1", "ws://unreachable.invalid:8545")
	require.NoError(t, err)
	wsPool := pool.NewPool("develop-websocket:websocket", []*pool.BackendEndpoint{ep}, &pool.FirstHealthySelector{}, "first_healthy(test)")
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
	var queueAsked atomic.Int32
	proxy.SetPublishQueueFull(func() bool { queueAsked.Add(1); return true })
	proxy.storeOperable = func() bool { return false }

	srv := httptest.NewServer(proxy.WebSocketHandler())
	t.Cleanup(srv.Close)
	before := storageRejections(metricLabelUnknown, BackendTypeWebSocket)

	headers := http.Header{}
	headers.Set("Target-Service-Id", "develop-websocket")
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), headers)
	if conn != nil {
		_ = conn.Close()
	}
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "the connection must not be upgraded")
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "LINK ws-first: the upgrade is answered 429")
	require.Equal(t, before+1, storageRejections(metricLabelUnknown, BackendTypeWebSocket))
	require.Zero(t, queueAsked.Load(), "the storage gate runs before the queue gate")
}

func TestWebSocketALiveBridgeIsCutWhenTheStoreCloses(t *testing.T) {
	backendURL, _, _ := countingWSBackend(t)
	bridge, gate := newGatedLifecycleBridge(t, backendURL)
	t.Cleanup(func() { _ = bridge.Close() })
	p := drainTestProxy(t)
	require.True(t, p.trackBridge(bridge))
	cut := testutil.ToFloat64(liveConnectionsCut.WithLabelValues(BackendTypeWebSocket))

	gate.arm()
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		defer p.untrackBridge(bridge)
		bridge.Run()
	}()
	select {
	case <-gate.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge never armed its first-frame deadline")
	}

	p.cutLiveConnections()
	select {
	case <-gate.nudged:
	case <-time.After(10 * time.Second):
		t.Fatal("LINK ws-cut: the cut never reached the bridge")
	}
	gate.open()

	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("LINK ws-cut: the bridge did not end after the cut")
	}
	reason := bridge.closeReason.Load()
	require.NotNil(t, reason)
	require.Equal(t, CloseTryAgainLater, reason.code, "LINK ws-cut: the bridge is closed as try-again-later")
	require.Equal(t, cut+1, testutil.ToFloat64(liveConnectionsCut.WithLabelValues(BackendTypeWebSocket)))
}
