//go:build test

package relayer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// Every relay counter that more than one transport moves is ONE series with
// rpc_type as a label. These tests read the series carrying the transport's own
// label, because a test on a total passes while a transport is missing from it.

func TestRPCTypeFrom_IsUnknownUntilSet(t *testing.T) {
	require.Equal(t, metricLabelUnknown, RPCTypeFrom(context.Background()))
	require.Equal(t, metricLabelUnknown, RPCTypeFrom(WithRPCType(context.Background(), "")),
		"an empty label is not a transport")
	require.Equal(t, BackendTypeGRPC, RPCTypeFrom(WithRPCType(context.Background(), BackendTypeGRPC)))
}

func TestRelaysDropped_APublishFailureCountsUnderItsTransport(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		p := &ProxyServer{logger: testLogger(), relayProcessor: &recordingProcessor{}, publisher: countPublished(failingPublisher{})}
		dropped := relaysDropped.WithLabelValues("svc-drop-http", BackendTypeJSONRPC, dropReasonPublishFailed)
		before := testutil.ToFloat64(dropped)

		p.executePublish(context.Background(), publishTask{serviceID: "svc-drop-http", supplierAddr: "pokt1drophttp", rpcType: BackendTypeJSONRPC})

		require.Equal(t, before+1, testutil.ToFloat64(dropped))
	})

	t.Run("websocket", func(t *testing.T) {
		verifyNoBridgeGoroutines(t)
		backendURL, _, _ := countingWSBackend(t)
		pipeline, _, _ := newOwnerTestPipeline(t)
		supplier, signer := newSupplier(t)
		relayerConn, _ := newGatewaySideHarness(t)

		bridge, err := NewWebSocketBridge(
			testLogger(), relayerConn, backendURL, simWSTestService, "", atHeight(100),
			&recordingProcessor{}, countPublished(failingPublisher{}), signer, http.Header{},
			nil, pipeline, 5*time.Second, false, nil, "", nil,
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = bridge.Close() })
		bridge.owner.Store(&supplier)
		dropped := relaysDropped.WithLabelValues(simWSTestService, BackendTypeWebSocket, dropReasonPublishFailed)
		before := testutil.ToFloat64(dropped)

		bridge.emitRelay(ownerTestRelay("ws-drop", supplier), &servicetypes.RelayResponse{}, []byte(`{"ok":true}`))

		require.Equal(t, before+1, testutil.ToFloat64(dropped))
	})

	t.Run("grpc", func(t *testing.T) {
		fx := newGRPCPublishFixture(t, newOKBackend(t).URL)
		fx.svc.publisher = countPublished(failingPublisher{})
		dropped := relaysDropped.WithLabelValues("develop-http", BackendTypeGRPC, dropReasonPublishFailed)
		before := testutil.ToFloat64(dropped)

		require.NoError(t, fx.svc.handleSendRelay(fx.stream))

		require.Equal(t, before+1, testutil.ToFloat64(dropped))
	})
}

// zeroTargetDifficulty makes every relay miss the target: no hash is below zero.
type zeroTargetDifficulty struct{}

func (zeroTargetDifficulty) GetTargetHash(context.Context, string, int64) ([]byte, error) {
	return make([]byte, 32), nil
}

// TestRelaysSkippedDifficulty_CountsUnderTheTransportOnTheContext drives the real
// processor: the skip is counted inside ProcessRelay, which every transport
// calls and none of them can label except through the context.
func TestRelaysSkippedDifficulty_CountsUnderTheTransportOnTheContext(t *testing.T) {
	rp := NewRelayProcessor(testLogger(), nil, nil, nil)
	rp.SetDifficultyProvider(zeroTargetDifficulty{})
	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      "pokt1skipapp",
				ServiceId:               "svc-skip",
				SessionId:               "skip-session",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   110,
			},
			SupplierOperatorAddress: "pokt1skipsupplier",
		},
		Payload: []byte("payload"),
	}
	reqBz, err := req.Marshal()
	require.NoError(t, err)

	skipped := relaysSkippedDifficulty.WithLabelValues("svc-skip", BackendTypeWebSocket)
	unknown := relaysSkippedDifficulty.WithLabelValues("svc-skip", metricLabelUnknown)
	before, unknownBefore := testutil.ToFloat64(skipped), testutil.ToFloat64(unknown)

	msg, err := rp.ProcessRelay(WithRPCType(context.Background(), BackendTypeWebSocket), reqBz, []byte(`{"ok":true}`), "pokt1skipsupplier", "svc-skip", 105)

	require.NoError(t, err)
	require.Nil(t, msg, "premise: a zero target makes the relay miss the difficulty")
	require.Equal(t, before+1, testutil.ToFloat64(skipped))
	require.Equal(t, unknownBefore, testutil.ToFloat64(unknown), "the skip must not also land in unknown")
}

// blockingPublisher announces that Publish was entered and then holds it.
type blockingPublisher struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingPublisher) Publish(context.Context, *transport.MinedRelayMessage) error {
	close(p.entered)
	<-p.release
	return nil
}
func (p *blockingPublisher) Close() error { return nil }

func histogramSampleCount(t *testing.T, serviceID, rpcType string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	obs := relayLatency.WithLabelValues(serviceID, rpcType)
	require.NoError(t, obs.(interface{ Write(*dto.Metric) error }).Write(m))
	return m.GetHistogram().GetSampleCount()
}

// TestGRPCRelayLatency_IsObservedBeforeThePublish holds the publish open and
// reads the histogram while it is held. Observed after the publish, the sample
// would not exist yet at that moment.
func TestGRPCRelayLatency_IsObservedBeforeThePublish(t *testing.T) {
	fx := newGRPCPublishFixture(t, newOKBackend(t).URL)
	bp := &blockingPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	fx.svc.publisher = countPublished(bp)
	before := histogramSampleCount(t, "develop-http", BackendTypeGRPC)

	done := make(chan error, 1)
	go func() { done <- fx.svc.handleSendRelay(fx.stream) }()
	<-bp.entered

	got := histogramSampleCount(t, "develop-http", BackendTypeGRPC)
	close(bp.release)
	require.NoError(t, <-done)
	require.Equal(t, before+1, got,
		"relay_latency_seconds{rpc_type=grpc} must be observed before the publish, like HTTP")
}

type failingRecvStream struct{ *mockServerStream }

func (failingRecvStream) RecvMsg(interface{}) error {
	return errors.New("injected: the stream broke on read")
}

type failingSendStream struct{ *mockServerStream }

func (failingSendStream) SendMsg(interface{}) error {
	return errors.New("injected: the stream broke on write")
}

// rejectAllValidator refuses every request. It stands on its own rather than
// embedding acceptAnyValidator: RelayValidator is a one-method interface now
// that the block-height setter and getter are gone, and this type overrides that
// one method, so the embed contributed nothing but a name.
type rejectAllValidator struct{}

func (rejectAllValidator) ValidateRelayRequest(context.Context, *servicetypes.RelayRequest, int64) error {
	return errors.New("injected: invalid ring signature")
}

// TestGRPCRejections_CountInRelaysRejectedUnderGRPC drives each rejection of the
// gRPC handler that a fixture can reach and asserts the ONE series moved, under
// rpc_type=grpc and the reason HTTP uses for the same case.
//
// NOT covered here, and said so: meter_error and stake_exhausted need a meter
// that fails or is exhausted, and signing_error needs a signer that holds the
// key and still fails to sign. Their sites are the same one-line replacement as
// the cases below.
func TestGRPCRejections_CountInRelaysRejectedUnderGRPC(t *testing.T) {
	slowRelease := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-slowRelease:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(slowRelease) })

	fiveHundred := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(fiveHundred.Close)

	cases := []struct {
		name      string
		backend   string
		serviceID string // label expected on the rejection
		reason    string
		prepare   func(fx *grpcPublishFixture) interface{}
	}{
		{"recv error", "", metricLabelUnknown, rejectReasonRecvError, func(fx *grpcPublishFixture) interface{} {
			return failingRecvStream{fx.stream}
		}},
		{"missing session header", "", metricLabelUnknown, rejectReasonInvalidRelayRequest, func(fx *grpcPublishFixture) interface{} {
			fx.stream.req.Meta.SessionHeader = nil
			return fx.stream
		}},
		{"missing service id", "", metricLabelUnknown, rejectReasonMissingServiceID, func(fx *grpcPublishFixture) interface{} {
			fx.stream.req.Meta.SessionHeader.ServiceId = ""
			return fx.stream
		}},
		{"missing supplier", "", "develop-http", rejectReasonMissingSupplierAddress, func(fx *grpcPublishFixture) interface{} {
			fx.stream.req.Meta.SupplierOperatorAddress = ""
			return fx.stream
		}},
		{"no signer for supplier", "", "develop-http", rejectReasonNoLocalSigner, func(fx *grpcPublishFixture) interface{} {
			fx.stream.req.Meta.SupplierOperatorAddress = "pokt1notoursupplier"
			return fx.stream
		}},
		{"unknown service", "", "svc-not-configured", rejectReasonUnknownService, func(fx *grpcPublishFixture) interface{} {
			fx.stream.req.Meta.SessionHeader.ServiceId = "svc-not-configured"
			return fx.stream
		}},
		{"validation failed", "", "develop-http", rejectReasonValidationFailed, func(fx *grpcPublishFixture) interface{} {
			fx.svc.relayPipeline = NewRelayPipeline(rejectAllValidator{}, nil, testLogger())
			return fx.stream
		}},
		{"payload does not deserialize", "", "develop-http", rejectReasonInvalidRelayRequest, func(fx *grpcPublishFixture) interface{} {
			fx.stream.req.Payload = []byte{0x0a, 0xff, 0xff, 0xff}
			return fx.stream
		}},
		{"backend unreachable", "http://127.0.0.1:1", "develop-http", rejectReasonBackendNetworkError, func(fx *grpcPublishFixture) interface{} {
			return fx.stream
		}},
		{"backend slower than the service timeout", slow.URL, "develop-http", rejectReasonBackendTimeout, func(fx *grpcPublishFixture) interface{} {
			fx.svc.getServiceTimeout = func(string) time.Duration { return 20 * time.Millisecond }
			return fx.stream
		}},
		{"client gone before the forward", slow.URL, "develop-http", rejectReasonClientDisconnected, func(fx *grpcPublishFixture) interface{} {
			ctx, cancel := context.WithCancel(metadata.NewIncomingContext(context.Background(), metadata.Pairs("rpc-type", "3")))
			cancel()
			fx.stream.ctx = ctx
			return fx.stream
		}},
		{"backend 5xx", fiveHundred.URL, "develop-http", rejectReasonBackend5xx, func(fx *grpcPublishFixture) interface{} {
			return fx.stream
		}},
		{"send error", "", "develop-http", rejectReasonSendError, func(fx *grpcPublishFixture) interface{} {
			return failingSendStream{fx.stream}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := tc.backend
			if backend == "" {
				backend = newOKBackend(t).URL
			}
			fx := newGRPCPublishFixture(t, backend)
			stream := tc.prepare(fx)
			rejected := relaysRejected.WithLabelValues(tc.serviceID, BackendTypeGRPC, tc.reason)
			before := testutil.ToFloat64(rejected)

			switch s := stream.(type) {
			case failingRecvStream:
				_ = fx.svc.handleSendRelay(s)
			case failingSendStream:
				_ = fx.svc.handleSendRelay(s)
			case *mockServerStream:
				_ = fx.svc.handleSendRelay(s)
			default:
				t.Fatalf("unhandled stream type %T", stream)
			}

			require.Equal(t, before+1, testutil.ToFloat64(rejected),
				"relays_rejected_total{rpc_type=grpc, reason=%s} must move exactly once", tc.reason)
		})
	}
}
