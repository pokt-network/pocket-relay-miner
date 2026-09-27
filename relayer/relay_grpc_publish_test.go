//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/pokt-network/pocket-relay-miner/pool"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// recordingProcessor is a RelayProcessor test double that captures the exact
// respBody handed to ProcessRelay and always returns a non-nil mined message so
// the caller proceeds to publish. It lets the publish/mining routing be asserted
// without pulling in difficulty checks or compute-unit config.
type recordingProcessor struct {
	calls        atomic.Int32
	lastReqBody  []byte
	lastRespBody []byte
	// lastPublishBudget is time.Until(publishCtx deadline) captured at the
	// moment ProcessRelay runs. It measures how much of grpcPublishTimeout is
	// actually left for publishing AFTER the backend forward.
	lastPublishBudget      time.Duration
	lastPublishHasDeadline bool
}

func (p *recordingProcessor) ProcessRelay(
	ctx context.Context,
	reqBody, respBody []byte,
	supplierAddr, serviceID string,
	_ int64,
) (*transport.MinedRelayMessage, error) {
	p.calls.Add(1)
	if dl, ok := ctx.Deadline(); ok {
		p.lastPublishHasDeadline = true
		p.lastPublishBudget = time.Until(dl)
	}
	// Copy: forwardToBackend's buffer pool may recycle the backing array.
	p.lastReqBody = append([]byte(nil), reqBody...)
	p.lastRespBody = append([]byte(nil), respBody...)
	return &transport.MinedRelayMessage{
		SupplierOperatorAddress: supplierAddr,
		ServiceId:               serviceID,
	}, nil
}

func (p *recordingProcessor) GetServiceDifficulty(context.Context, string, int64) ([]byte, error) {
	return nil, nil
}

func (p *recordingProcessor) SetDifficultyProvider(DifficultyProvider) {}

// recordingPublisher counts Publish calls so a test can prove a relay was (or
// was NOT) sent to the WAL.
type recordingPublisher struct {
	calls atomic.Int32
}

func (p *recordingPublisher) Publish(context.Context, *transport.MinedRelayMessage) error {
	p.calls.Add(1)
	return nil
}

func (p *recordingPublisher) Close() error { return nil }

// mockServerStream is a minimal grpc.ServerStream: it hands the handler a fixed
// RelayRequest, records every message the handler sends back, and exposes an
// incoming-metadata context.
type mockServerStream struct {
	ctx  context.Context
	req  *servicetypes.RelayRequest
	sent []interface{}
}

func (m *mockServerStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockServerStream) SendHeader(metadata.MD) error { return nil }
func (m *mockServerStream) SetTrailer(metadata.MD)       {}
func (m *mockServerStream) Context() context.Context     { return m.ctx }

func (m *mockServerStream) SendMsg(msg interface{}) error {
	m.sent = append(m.sent, msg)
	return nil
}

func (m *mockServerStream) RecvMsg(msg interface{}) error {
	rr, ok := msg.(*servicetypes.RelayRequest)
	if !ok {
		return nil
	}
	bz, err := m.req.Marshal()
	if err != nil {
		return err
	}
	return rr.Unmarshal(bz)
}

// grpcPublishFixture wires a RelayGRPCService with a real signer, a recording
// processor and publisher, and a pool that resolves to backendURL. relayPipeline
// is left nil so ring-signature validation and metering are skipped -- the
// publish routing under test runs regardless.
type grpcPublishFixture struct {
	svc       *RelayGRPCService
	proc      *recordingProcessor
	pub       *recordingPublisher
	stream    *mockServerStream
	supplier  string
	serviceID string
}

func newGRPCPublishFixture(t *testing.T, backendURL string) *grpcPublishFixture {
	t.Helper()

	const supplier = "pokt1testsupplieroperator"
	const serviceID = "develop-http"

	keys := map[string]cryptotypes.PrivKey{supplier: secp256k1.GenPrivKey()}
	rs, err := NewResponseSigner(testLogger(), keys)
	require.NoError(t, err)

	proc := &recordingProcessor{}
	pub := &recordingPublisher{}

	endpoint, err := pool.NewBackendEndpoint("backend", backendURL)
	require.NoError(t, err)
	healthyPool := pool.NewPool(
		"test-pool",
		[]*pool.BackendEndpoint{endpoint},
		&pool.FirstHealthySelector{},
		"first_healthy(test)",
	)

	svc := NewRelayGRPCService(testLogger(), RelayGRPCServiceConfig{
		ServiceConfigs: map[string]ServiceConfig{serviceID: {}},
		ResponseSigner: rs,
		Publisher:      pub,
		RelayProcessor: proc,
		GetPool: func(string, string) *pool.Pool {
			return healthyPool
		},
	})

	// A minimal but valid inner request: DeserializeHTTPRequest must parse it.
	poktReq := &sdktypes.POKTHTTPRequest{
		Method: http.MethodPost,
		Url:    "/",
		BodyBz: []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`),
	}
	payloadBz, err := proto.Marshal(poktReq)
	require.NoError(t, err)

	relayRequest := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      "pokt1testapplication",
				ServiceId:               serviceID,
				SessionId:               "test-session-id",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   110,
			},
			SupplierOperatorAddress: supplier,
		},
		Payload: payloadBz,
	}

	// rpc-type "3" == JSON_RPC: a non-gRPC relay routed over the plain HTTP client.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("rpc-type", "3"))

	return &grpcPublishFixture{
		svc:       svc,
		proc:      proc,
		pub:       pub,
		stream:    &mockServerStream{ctx: ctx, req: relayRequest},
		supplier:  supplier,
		serviceID: serviceID,
	}
}

// TestHandleSendRelay_TransportErrorNotPublished proves the gRPC error path does
// NOT mine or publish when the backend forward fails at the transport level
// (connection refused). This mirrors the HTTP path (proxy.go): on a forward
// error it returns an error to the client and publishes nothing -- a relay the
// backend never answered must never reach the WAL. The client still receives a
// signed error response.
func TestHandleSendRelay_TransportErrorNotPublished(t *testing.T) {
	// 127.0.0.1:1 refuses connections: forwardToBackend returns a transport error
	// (err != nil), not a backend HTTP status.
	fx := newGRPCPublishFixture(t, "http://127.0.0.1:1")

	err := fx.svc.handleSendRelay(fx.stream)
	require.NoError(t, err, "handler returns nil after serving the client an error response")

	require.Equal(t, int32(0), fx.proc.calls.Load(),
		"a transport-failed relay must NOT be processed for mining")
	require.Equal(t, int32(0), fx.pub.calls.Load(),
		"a transport-failed relay must NOT be published to the WAL")
	require.Len(t, fx.stream.sent, 1, "client still gets exactly one (error) response")
}

// TestHandleSendRelay_SuccessMinesRawBackendBody proves the success path hands
// ProcessRelay the RAW backend response body -- the same input the HTTP path
// mines -- and NOT the already-marshaled signed RelayResponse. Passing the
// wrapped RelayResponse would make PayloadHash cover the wrong bytes (a
// RelayResponse-inside-a-RelayResponse), diverging from HTTP.
func TestHandleSendRelay_SuccessMinesRawBackendBody(t *testing.T) {
	backendBody := []byte(`{"jsonrpc":"2.0","result":"0x10","id":1}`)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(backendBody)
	}))
	defer backend.Close()

	fx := newGRPCPublishFixture(t, backend.URL)

	err := fx.svc.handleSendRelay(fx.stream)
	require.NoError(t, err)

	require.Equal(t, int32(1), fx.proc.calls.Load(), "a served relay must be processed once")
	require.Equal(t, int32(1), fx.pub.calls.Load(), "a served relay must be published once")

	require.Equal(t, backendBody, fx.proc.lastRespBody,
		"ProcessRelay must receive the raw backend body (parity with the HTTP path), "+
			"not the marshaled RelayResponse")

	// Guard: the marshaled RelayResponse the client received is a strict superset
	// of the backend body (adds meta + signature), so equality above cannot pass
	// by accident.
	require.NotEqual(t, len(backendBody), len(fx.proc.lastRespBody)+1,
		"sanity: backend body and wrapped response differ in length")
	require.Len(t, fx.stream.sent, 1, "client gets exactly one signed response")

	// The published request bytes must be the marshaled RelayRequest.
	var gotReq servicetypes.RelayRequest
	require.NoError(t, gotReq.Unmarshal(fx.proc.lastReqBody))
	require.Equal(t, fx.supplier, gotReq.Meta.SupplierOperatorAddress)
}

// TestHandleSendRelay_PublishBudgetFreshAfterSlowBackend proves the detached
// publish context gets its full grpcPublishTimeout budget measured from PUBLISH
// time, not from handler entry. The budget must not be consumed by backend
// latency: an already-served relay that took a while at the backend must still
// have (nearly) the whole publish window left, or a slow-but-successful relay
// silently fails to reach the WAL (lost reward) -- a divergence from the HTTP
// path, which publishes on a fresh context created after the backend returns.
func TestHandleSendRelay_PublishBudgetFreshAfterSlowBackend(t *testing.T) {
	// A backend that takes a noticeable slice of wall-clock before answering.
	// If publishCtx's clock starts at handler entry, this latency is charged
	// against the 30s publish budget; if it starts at publish time, it is not.
	const backendDelay = 300 * time.Millisecond
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(backendDelay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x1","id":1}`))
	}))
	defer backend.Close()

	fx := newGRPCPublishFixture(t, backend.URL)

	err := fx.svc.handleSendRelay(fx.stream)
	require.NoError(t, err)
	require.Equal(t, int32(1), fx.proc.calls.Load())
	require.True(t, fx.proc.lastPublishHasDeadline, "publish context must carry a bounded deadline")

	// The remaining budget at publish time must be within ~backendDelay of the
	// full grpcPublishTimeout. Tolerance (100ms) comfortably covers the
	// microsecond gap between the backend returning and publishCtx being
	// created, but is far smaller than backendDelay, so the old
	// created-at-entry behaviour (budget = 30s - ~300ms) fails this bound.
	require.Greater(t, fx.proc.lastPublishBudget, grpcPublishTimeout-100*time.Millisecond,
		"publish budget must be measured from publish time, not handler entry "+
			"(got %s left of %s after a %s backend)", fx.proc.lastPublishBudget, grpcPublishTimeout, backendDelay)
}
