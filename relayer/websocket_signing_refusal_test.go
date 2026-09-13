//go:build test

package relayer

import (
	"net/http"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// The bridge used to fall back to an UNSIGNED response when signing failed:
// it built a RelayResponse with no supplier signature, wrote it to the gateway,
// and then billed it. That second half is what makes it more than a protocol
// nicety -- emitRelay mines the RelayHash over {Req, Res}, so the SMST leaf was
// committed to a response nobody signed.
//
// Owner decision 2026-09-03: "respuesta sin firma no existe, siempre con firma,
// si no se puede firmar, se tumba el ws."
//
// Closing is also the only answer that ENDS the failure. Admission already
// refuses a request whose supplier this relayer holds no key for
// (validator.go:112), so reaching the signing error means the key set changed
// mid-connection. The supplier is fixed for the life of a bridge, so the next
// backend push would fail identically -- dropping the message instead would
// leave a subscription pushing into a bin with the client waiting forever.
func TestWebSocketRefusesToServeOrBillAnUnsignableResponse(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	logger := testLogger()

	// A signer that holds SOMEBODY ELSE's key: HasSigner is false for this
	// bridge's supplier, which is the shape a hot key removal leaves behind.
	otherPriv := secp256k1.GenPrivKey()
	otherAddr := cosmostypes.AccAddress(otherPriv.PubKey().Address()).String()
	signer, err := NewResponseSigner(logger, map[string]cryptotypes.PrivKey{otherAddr: otherPriv})
	require.NoError(t, err)

	supplierPriv := secp256k1.GenPrivKey()
	supplierAddr := cosmostypes.AccAddress(supplierPriv.PubKey().Address()).String()

	backendURL, _, _ := newSimWSBackendServer(t)
	relayerConn, gwClient := newGatewaySideHarness(t)
	proc, pub := &recordingProcessor{}, &recordingPublisher{}
	pipeline, redisClient, _, charges := newOwnerTestPipelineWithCharges(t)

	bridge, err := NewWebSocketBridge(
		logger, relayerConn, backendURL, simWSTestService, supplierAddr, 1,
		proc, pub, signer, http.Header{}, nil, pipeline,
		2*time.Second, false, nil, "", nil,
	)
	require.NoError(t, err)
	go bridge.Run()
	t.Cleanup(func() { _ = bridge.Close() })

	before := testutil.ToFloat64(relaysRejected.WithLabelValues(
		simWSTestService, "websocket", rejectReasonSigningError))
	checked := relayMeterConsumptions.WithLabelValues(simWSTestService, "within_limit")
	checksBefore := testutil.ToFloat64(checked)

	req := &servicetypes.RelayRequest{
		Payload: []byte(`{"jsonrpc":"2.0","method":"eth_subscribe","params":["newHeads"],"id":1}`),
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      ownerTestAppAddr,
				ServiceId:               simWSTestService,
				SessionId:               "sign-refusal",
				SessionStartBlockHeight: 1,
				SessionEndBlockHeight:   2,
			},
			SupplierOperatorAddress: supplierAddr,
		},
	}
	bz, err := req.Marshal()
	require.NoError(t, err)
	require.NoError(t, gwClient.WriteMessage(websocket.BinaryMessage, bz))

	// NOTHING is written, and the connection is closed: the next read is the
	// close frame rather than a response. Asserting on the read is what makes
	// "nothing was served" observable -- a call count would only say the code
	// believed it served nothing.
	require.NoError(t, gwClient.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, data, readErr := gwClient.ReadMessage()
	require.Error(t, readErr,
		"no response may reach the gateway when it cannot be signed; got %q", string(data))
	require.True(t, websocket.IsCloseError(readErr, CloseInternalError),
		"the connection must be closed with a reason, got %v", readErr)

	require.Equal(t, int32(0), proc.calls.Load(),
		"an unsigned response must never be mined: the RelayHash covers {Req, Res}")
	require.Equal(t, int32(0), pub.calls.Load(),
		"and it must never reach the WAL, which is what bills it")

	require.Equal(t, before+1, testutil.ToFloat64(relaysRejected.WithLabelValues(
		simWSTestService, "websocket", rejectReasonSigningError)),
		"the refusal is a per-connection event with a bounded reason, so it is counted")

	// The frame went through the real budget check before the backend, and the
	// response that could not be signed was not charged: the close frame is
	// written after the message loop returned, so the charge would already be in
	// the ledger.
	require.Equal(t, checksBefore+1, testutil.ToFloat64(checked),
		"the frame must have been checked by the meter before the backend")
	charges.flush()
	require.Zero(t, consumedIn(t, redisClient, pipeline.relayMeter.consumedKey("sign-refusal", supplierAddr)),
		"a response that was never served must not be charged")
}
