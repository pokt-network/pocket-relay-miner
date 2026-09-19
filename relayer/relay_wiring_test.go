//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// TestInitGRPCHandlerRefusesWithoutAPipeline pins the order in which the relayer
// wires gRPC. The service copies the pipeline when it is built, so building it
// before InitializeRelayPipeline left every gRPC relay unvalidated and unmetered
// for the life of the process. That order is now a startup error.
func TestInitGRPCHandlerRefusesWithoutAPipeline(t *testing.T) {
	p := &ProxyServer{
		logger: testLogger(),
		config: &Config{Services: map[string]ServiceConfig{"svc": {}}},
	}

	require.Error(t, p.InitGRPCHandler())
	require.Nil(t, p.grpcRelayService, "no service may be built without the pipeline")
}

// TestInitializeRelayPipelineRefusesMissingDependencies pins that a relayer missing
// a pipeline dependency fails to start instead of warning and serving on.
func TestInitializeRelayPipelineRefusesMissingDependencies(t *testing.T) {
	p := &ProxyServer{logger: testLogger()}

	err := p.InitializeRelayPipeline()
	require.Error(t, err)
	require.Contains(t, err.Error(), "has_meter=false")
	require.Nil(t, p.relayPipeline)
}

// TestGRPCRelayWithABadSignatureIsRefusedThroughTheWiring drives a gRPC relay
// through the proxy's own initialisation, in the order the relayer runs it, with
// a validator that rejects: nothing reaches the backend, nothing is published.
func TestGRPCRelayWithABadSignatureIsRefusedThroughTheWiring(t *testing.T) {
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := newSimHTTPFixture(t, backend.URL, ValidationModeEager)
	pipeline, _, _ := newOwnerTestPipeline(t)
	f.proxy.validator = rejectAllValidator{}
	f.proxy.relayMeter = pipeline.relayMeter
	f.proxy.relayProcessor = &recordingProcessor{}

	require.NoError(t, f.proxy.InitializeRelayPipeline())
	require.NoError(t, f.proxy.InitGRPCHandler())

	payload, err := proto.Marshal(&sdktypes.POKTHTTPRequest{
		Method: http.MethodPost,
		Url:    "/",
		BodyBz: []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`),
	})
	require.NoError(t, err)
	stream := &mockServerStream{
		ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs("rpc-type", "3")),
		req: &servicetypes.RelayRequest{
			Meta: servicetypes.RelayRequestMetadata{
				SessionHeader: &sessiontypes.SessionHeader{
					ApplicationAddress:      ownerTestAppAddr,
					ServiceId:               simTestService,
					SessionId:               "bad-signature-wiring",
					SessionStartBlockHeight: 100,
					SessionEndBlockHeight:   110,
				},
				SupplierOperatorAddress: f.supplierAddr,
			},
			Payload: payload,
		},
	}
	rejected := relaysRejected.WithLabelValues(simTestService, BackendTypeGRPC, rejectReasonValidationFailed)
	before := testutil.ToFloat64(rejected)

	err = f.proxy.grpcRelayService.handleSendRelay(stream)

	require.Equal(t, codes.PermissionDenied, status.Code(err), "err=%v", err)
	require.Equal(t, before+1, testutil.ToFloat64(rejected))
	require.Equal(t, int32(0), backendHits.Load(), "a relay that fails validation must not reach the backend")
	require.Equal(t, int32(0), f.pub.calls.Load(), "and must not be published")
	require.Empty(t, stream.sent)
}

// TestGRPCRefusesARelayWithoutAPipeline proves the service's own branch is closed:
// with no pipeline, a relay is refused before the backend instead of served free.
// The queue is full at the same time: as in HTTP, a process wired without a
// pipeline must name that, not the queue.
func TestGRPCRefusesARelayWithoutAPipeline(t *testing.T) {
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	fx := newGRPCPublishFixture(t, backend.URL)
	fx.svc.relayPipeline = nil
	fx.svc.publishQueueFull = func() bool { return true }
	rejected := relaysRejected.WithLabelValues(fx.serviceID, BackendTypeGRPC, rejectReasonMeteringNotConfigured)
	queueFull := relaysRejected.WithLabelValues(fx.serviceID, BackendTypeGRPC, rejectReasonPublishQueueFull)
	before, queueBefore := testutil.ToFloat64(rejected), testutil.ToFloat64(queueFull)

	err := fx.svc.handleSendRelay(fx.stream)

	require.Equal(t, codes.Internal, status.Code(err), "err=%v", err)
	require.Equal(t, before+1, testutil.ToFloat64(rejected))
	require.Equal(t, queueBefore, testutil.ToFloat64(queueFull), "the missing pipeline is checked before the queue")
	require.Equal(t, int32(0), backendHits.Load())
	require.Equal(t, int32(0), fx.proc.calls.Load())
	require.Equal(t, int32(0), fx.pub.calls.Load())
	require.Empty(t, fx.stream.sent)
}

// TestWebSocketClosesAFrameWithoutAPipeline proves the bridge's branch is closed:
// a real frame with no pipeline closes the connection before the backend is dialled.
func TestWebSocketClosesAFrameWithoutAPipeline(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	backendURL, dials, _ := countingWSBackend(t)
	supplier, signer := newSupplier(t)
	pub := &recordingPublisher{}

	relayerConn, gwClient := newGatewaySideHarness(t)
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", atHeight(100),
		&recordingProcessor{}, pub, signer, http.Header{},
		nil, nil, 2*time.Second, false, nil, "", nil,
	)
	require.NoError(t, err)
	go bridge.Run()
	t.Cleanup(func() { _ = bridge.Close() })

	rejected := relaysRejected.WithLabelValues(simWSTestService, BackendTypeWebSocket, rejectReasonMeteringNotConfigured)
	before := testutil.ToFloat64(rejected)

	sendRelay(t, gwClient, ownerTestRelay("no-pipeline", supplier))

	require.NoError(t, gwClient.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, data, readErr := gwClient.ReadMessage()
	require.Error(t, readErr, "no response may reach the gateway; got %q", string(data))
	require.True(t, websocket.IsCloseError(readErr, CloseInternalError), "the connection must be closed with a reason, got %v", readErr)
	require.Equal(t, before+1, testutil.ToFloat64(rejected))
	require.Equal(t, int32(0), dials.Load(), "the backend must not be dialled")
	require.Equal(t, int32(0), pub.calls.Load())
}

// TestGRPCClientGoneDuringValidationIsNotAValidationFailure pins the admission side
// of a disconnect: a check that fails because the client already left is counted as
// the disconnect, so validation_failed keeps meaning a relay that did not verify.
func TestGRPCClientGoneDuringValidationIsNotAValidationFailure(t *testing.T) {
	fx := newGRPCPublishFixture(t, newOKBackend(t).URL)
	fx.svc.relayPipeline = NewRelayPipeline(rejectAllValidator{}, nil, testLogger())
	ctx, cancel := context.WithCancel(metadata.NewIncomingContext(context.Background(), metadata.Pairs("rpc-type", "3")))
	cancel()
	fx.stream.ctx = ctx

	gone := relaysRejected.WithLabelValues(fx.serviceID, BackendTypeGRPC, rejectReasonClientDisconnected)
	failed := relaysRejected.WithLabelValues(fx.serviceID, BackendTypeGRPC, rejectReasonValidationFailed)
	goneBefore, failedBefore := testutil.ToFloat64(gone), testutil.ToFloat64(failed)

	err := fx.svc.handleSendRelay(fx.stream)

	require.Equal(t, codes.Canceled, status.Code(err), "err=%v", err)
	require.Equal(t, goneBefore+1, testutil.ToFloat64(gone))
	require.Equal(t, failedBefore, testutil.ToFloat64(failed))
	require.Equal(t, int32(0), fx.pub.calls.Load())
}
