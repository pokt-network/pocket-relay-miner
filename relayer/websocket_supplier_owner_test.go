//go:build test

package relayer

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// acceptAnyValidator admits every request. These tests are about WHICH supplier
// the connection is charged and mined against, not about admission, and a real
// validator would need a chain-backed session cache to say yes at all.
type acceptAnyValidator struct{}

func (acceptAnyValidator) ValidateRelayRequest(context.Context, *servicetypes.RelayRequest, int64) error {
	return nil
}

const ownerTestAppAddr = "pokt1app"

// newOwnerTestPipeline wires a RelayPipeline whose meter is real and backed by
// the test Redis, so the KEY its charges land on is observable. Everything the
// meter needs from the chain is faked; the app has enough stake that the budget
// never runs out inside a test.
func newOwnerTestPipeline(t *testing.T) (*RelayPipeline, *redisutil.Client, string) {
	t.Helper()
	pipeline, redisClient, prefix, _ := newOwnerTestPipelineWithCharges(t)
	return pipeline, redisClient, prefix
}

// newOwnerTestPipelineWithCharges is newOwnerTestPipeline plus the writer that
// puts the meter's served charges in Redis, for the tests that read them.
func newOwnerTestPipelineWithCharges(t *testing.T) (*RelayPipeline, *redisutil.Client, string, *chargeWriter) {
	t.Helper()
	logger := testLogger()
	redisClient, prefix := newTestRedis(t)

	app := &fakeAppClient{addr: ownerTestAppAddr}
	app.stakeUpokt.Store(1_000_000)
	meter := NewRelayMeter(
		logger, redisClient, app, nil,
		&fakeSessionClient{numSuppliers: 1}, nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 1,
			ComputeUnitCostGranularity:     1,
		}}, nil, staticServiceFactor{f: 1}, RelayMeterConfig{},
	)
	require.NoError(t, meter.Start(context.Background()))
	t.Cleanup(func() { _ = meter.Close() })
	charges := newChargeWriter(t, meter, redisClient)

	return NewRelayPipeline(acceptAnyValidator{}, meter, logger), redisClient, prefix, charges
}

// newSupplier returns a fresh supplier address and a signer that holds its key.
func newSupplier(t *testing.T) (string, *ResponseSigner) {
	t.Helper()
	priv := secp256k1.GenPrivKey()
	addr := cosmostypes.AccAddress(priv.PubKey().Address()).String()
	signer, err := NewResponseSigner(testLogger(), map[string]cryptotypes.PrivKey{addr: priv})
	require.NoError(t, err)
	return addr, signer
}

// newSageShapedBridge builds a bridge the way SAGE's handshake leaves it: with
// NO supplier address, because sage sends only Target-Service-Id, App-Address
// and Rpc-Type (measured 2026-09-03 in ~/development/sage, ws_relayer.go).
func newSageShapedBridge(
	t *testing.T,
	backendURL string,
	signer *ResponseSigner,
	pipeline *RelayPipeline,
) *websocket.Conn {
	t.Helper()
	return newPublishingSageBridge(t, backendURL, signer, pipeline, &recordingPublisher{})
}

// newPublishingSageBridge is newSageShapedBridge with the publisher the test
// observes.
func newPublishingSageBridge(
	t *testing.T,
	backendURL string,
	signer *ResponseSigner,
	pipeline *RelayPipeline,
	publisher transport.MinedRelayPublisher,
) *websocket.Conn {
	t.Helper()
	relayerConn, gwClient := newGatewaySideHarness(t)
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService,
		"", // sage sends no Pocket-Supplier-Address
		atHeight(100),
		&recordingProcessor{}, publisher, signer, http.Header{},
		nil, pipeline, 2*time.Second, false, nil, "", nil,
	)
	require.NoError(t, err)
	runBridge(t, bridge)
	return gwClient
}

func ownerTestRelay(sessionID, supplier string) *servicetypes.RelayRequest {
	return &servicetypes.RelayRequest{
		Payload: []byte(`{"jsonrpc":"2.0","method":"eth_subscribe","params":["newHeads"],"id":1}`),
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      ownerTestAppAddr,
				ServiceId:               simWSTestService,
				SessionId:               sessionID,
				SessionStartBlockHeight: 91,
				SessionEndBlockHeight:   100,
			},
			SupplierOperatorAddress: supplier,
		},
	}
}

func sendRelay(t *testing.T, conn *websocket.Conn, req *servicetypes.RelayRequest) {
	t.Helper()
	bz, err := req.Marshal()
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, bz))
}

// readServedResponse reads the signed RelayResponse the bridge writes back, and
// is the synchronisation point for everything that had to happen first:
// MeterRelay checks the frame before it is forwarded, and the frame is forwarded
// before the backend can reply. No sleep, and no poll -- a response on the wire
// IS the proof the check ran. It is not the proof the response was charged: that
// happens after the write, so a test reading charges waits for the publish.
func readServedResponse(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, err := conn.ReadMessage()
	require.NoError(t, err, "the relay was not served, so nothing downstream of the meter ran")
}

// TestWebSocketMetersAgainstTheSupplierThatOwnsTheConnection is the regression
// test for a shared budget, not for a mis-labelled key.
//
// The bridge used to meter with the supplier from its HANDSHAKE, and sage sends
// none, so relayCtx.SupplierAddress was "". RelayMeter addresses its counter as
// {session}:{supplier}:consumed, so every sage connection on one session wrote
// to the SAME "{session}::consumed" key. Measured before the fix, two suppliers
// on one session: one key reading 2. The per-supplier limit is derived by
// dividing the application stake by the session's supplier count, so sharing
// one counter throttles the session to roughly 1/N of what it is owed.
//
// Asserting on the Redis KEY and not on a call argument is what makes it a test
// of the consequence: the key is the budget.
func TestWebSocketMetersAgainstTheSupplierThatOwnsTheConnection(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const sessionID = "owner-metering"
	pipeline, rc, prefix, charges := newOwnerTestPipelineWithCharges(t)
	backendURL, _, _ := newSimWSBackendServer(t)

	supplierA, signerA := newSupplier(t)
	supplierB, signerB := newSupplier(t)

	// The response is charged after it is written, so the charge is waited for on
	// the publish that follows it, not on the read.
	publishedA := newPublishSignal()
	connA := newPublishingSageBridge(t, backendURL, signerA, pipeline, publishedA)
	sendRelay(t, connA, ownerTestRelay(sessionID, supplierA))
	readServedResponse(t, connA)
	publishedA.await(t, 1)

	publishedB := newPublishSignal()
	connB := newPublishingSageBridge(t, backendURL, signerB, pipeline, publishedB)
	sendRelay(t, connB, ownerTestRelay(sessionID, supplierB))
	readServedResponse(t, connB)
	publishedB.await(t, 1)
	charges.flush()

	// Two suppliers, two budgets. Before the fix this pattern matched ONE key.
	keys, err := rc.Keys(context.Background(), prefix+":meter:"+sessionID+":*:consumed").Result()
	require.NoError(t, err)
	require.ElementsMatch(t,
		[]string{
			rc.KB().MeterConsumedKey(sessionID, supplierA),
			rc.KB().MeterConsumedKey(sessionID, supplierB),
		},
		keys,
		"each supplier meters against its own key; a shared key is a shared budget")

	for _, supplier := range []string{supplierA, supplierB} {
		consumed, err := rc.Get(context.Background(), rc.KB().MeterConsumedKey(sessionID, supplier)).Result()
		require.NoError(t, err)
		require.NotEqual(t, "0", consumed,
			"supplier %s consumed nothing, so the relay was metered somewhere else", supplier)
	}
}

// TestWebSocketClosesWhenAFrameNamesADifferentSupplier pins the second half of
// the owner rule: adoption happens once, and every frame after it must name the
// same supplier.
//
// Without it a single connection could mine relays for supplier A and then for
// supplier B while the response signer and the backend headers stayed bound to
// A -- the bridge would sign as one identity and bill another.
func TestWebSocketClosesWhenAFrameNamesADifferentSupplier(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const sessionID = "owner-change"
	pipeline, _, _ := newOwnerTestPipeline(t)
	backendURL, _, _ := newSimWSBackendServer(t)

	supplierA, signerA := newSupplier(t)
	otherPriv := secp256k1.GenPrivKey()
	supplierB := cosmostypes.AccAddress(otherPriv.PubKey().Address()).String()

	conn := newSageShapedBridge(t, backendURL, signerA, pipeline)

	before := testutil.ToFloat64(relaysRejected.WithLabelValues(
		simWSTestService, "websocket", rejectReasonSupplierChanged))

	// First frame adopts supplierA as the owner and is served.
	sendRelay(t, conn, ownerTestRelay(sessionID, supplierA))
	readServedResponse(t, conn)

	// Second frame names a different supplier on the same connection.
	sendRelay(t, conn, ownerTestRelay(sessionID, supplierB))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, data, readErr := conn.ReadMessage()
	require.Error(t, readErr,
		"a frame naming another supplier must not be served; got %q", string(data))
	require.True(t, websocket.IsCloseError(readErr, CloseValidationFailed),
		"the connection closes with the client-verdict code, got %v", readErr)

	require.Equal(t, before+1, testutil.ToFloat64(relaysRejected.WithLabelValues(
		simWSTestService, "websocket", rejectReasonSupplierChanged)),
		"the refusal is counted under its own bounded reason")
}

// TestWebSocketClosesWhenAFrameNamesNoSupplier closes the third door: a frame
// with an empty supplier can no longer establish an empty owner, which is the
// exact state that produced the shared meter key.
func TestWebSocketClosesWhenAFrameNamesNoSupplier(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	backendURL, _, _ := newSimWSBackendServer(t)
	_, signer := newSupplier(t)

	conn := newSageShapedBridge(t, backendURL, signer, pipeline)

	before := testutil.ToFloat64(relaysRejected.WithLabelValues(
		simWSTestService, "websocket", rejectReasonMissingSupplierAddress))

	sendRelay(t, conn, ownerTestRelay("owner-empty", ""))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, data, readErr := conn.ReadMessage()
	require.Error(t, readErr,
		"a frame with no supplier must not be served; got %q", string(data))
	require.True(t, websocket.IsCloseError(readErr, CloseValidationFailed),
		"the connection closes with the client-verdict code, got %v", readErr)

	require.Equal(t, before+1, testutil.ToFloat64(relaysRejected.WithLabelValues(
		simWSTestService, "websocket", rejectReasonMissingSupplierAddress)),
		"the refusal is counted under its own bounded reason")
}

// TestWebSocketClosesWhenAFrameCarriesNoSessionHeader closes a remote panic that
// sat one line below the owner gate.
//
// The RelayContext built just after this gate dereferences Meta.SessionHeader
// unguarded, and a remote peer chooses the frame's shape. net/http recovers the
// handler goroutine so the process survives, but the BRIDGE did not tear itself
// down: net/http does not close a hijacked connection, so the gauge, the socket
// and the SessionMonitor registration all stayed.
func TestWebSocketClosesWhenAFrameCarriesNoSessionHeader(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	backendURL, _, _ := newSimWSBackendServer(t)
	supplier, signer := newSupplier(t)

	conn := newSageShapedBridge(t, backendURL, signer, pipeline)

	before := testutil.ToFloat64(relaysRejected.WithLabelValues(
		simWSTestService, "websocket", rejectReasonInvalidRelayRequest))

	headerless := &servicetypes.RelayRequest{
		Payload: []byte(`{"jsonrpc":"2.0","method":"eth_subscribe","id":1}`),
		Meta: servicetypes.RelayRequestMetadata{
			SupplierOperatorAddress: supplier, // passes the supplier half of the gate
		},
	}
	sendRelay(t, conn, headerless)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, data, readErr := conn.ReadMessage()
	require.Error(t, readErr,
		"a frame with no session header must not be served; got %q", string(data))
	require.True(t, websocket.IsCloseError(readErr, CloseValidationFailed),
		"it closes with the client-verdict code rather than panicking, got %v", readErr)

	require.Equal(t, before+1, testutil.ToFloat64(relaysRejected.WithLabelValues(
		simWSTestService, "websocket", rejectReasonInvalidRelayRequest)),
		"the refusal is counted under its own bounded reason")
}
