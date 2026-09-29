//go:build test

package relayer

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// identityRelay is ownerTestRelay with the service, the application and the
// session window chosen by the test.
func identityRelay(sessionID, supplier, service, app string, start, end int64) *servicetypes.RelayRequest {
	req := ownerTestRelay(sessionID, supplier)
	req.Meta.SessionHeader.ServiceId = service
	req.Meta.SessionHeader.ApplicationAddress = app
	req.Meta.SessionHeader.SessionStartBlockHeight = start
	req.Meta.SessionHeader.SessionEndBlockHeight = end
	return req
}

func rejectedWS(reason string) float64 {
	return testutil.ToFloat64(relaysRejected.WithLabelValues(simWSTestService, "websocket", reason))
}

// requireClosedForValidation reads the next message and requires it to be the
// relayer closing the connection with the client-verdict code.
func requireClosedForValidation(t *testing.T, conn *websocket.Conn, what string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, data, readErr := conn.ReadMessage()
	require.Error(t, readErr, "%s must not be served; got %q", what, string(data))
	require.True(t, websocket.IsCloseError(readErr, CloseValidationFailed),
		"the connection closes with the client-verdict code, got %v", readErr)
}

// TestWebSocketClosesWhenAFrameNamesADifferentService is the regression test for
// issue #47. A connection opened for one service mined a frame whose session
// header named another: the leaf went into the other service's tree weighted
// with this service's compute units, and the chain refuses that claim. The
// frame was also forwarded to this service's backend.
func TestWebSocketClosesWhenAFrameNamesADifferentService(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	backendURL, backendHits, _ := newSimWSBackendServer(t)
	supplier, signer := newSupplier(t)
	published := newPublishSignal()
	conn := newPublishingV1Bridge(t, backendURL, signer, pipeline, published)
	before := rejectedWS(rejectReasonServiceChanged)

	sendRelay(t, conn, identityRelay("svc-change", supplier, simWSTestService, ownerTestAppAddr, 91, 100))
	readServedResponse(t, conn)
	published.await(t, 1)

	sendRelay(t, conn, identityRelay("svc-change", supplier, "other-service", ownerTestAppAddr, 91, 100))
	requireClosedForValidation(t, conn, "a frame for another service")

	require.Equal(t, before+1, rejectedWS(rejectReasonServiceChanged),
		"the refusal is counted under its own bounded reason, labelled with the connection's service")
	// The harm in #47 was downstream of the gate: the frame reached this
	// service's backend and was mined. The close must come before both.
	require.Equal(t, int32(1), backendHits.Load(), "the refused frame reached the backend")
	require.Empty(t, published.published, "the refused frame was mined and published")
}

// TestWebSocketRefusesAFirstFrameForAnotherService: the service is fixed by the
// handshake, so even the first frame cannot choose it.
func TestWebSocketRefusesAFirstFrameForAnotherService(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	backendURL, _, _ := newSimWSBackendServer(t)
	supplier, signer := newSupplier(t)
	conn := newV1ShapedBridge(t, backendURL, signer, pipeline)
	before := rejectedWS(rejectReasonServiceChanged)

	sendRelay(t, conn, identityRelay("svc-first", supplier, "other-service", ownerTestAppAddr, 91, 100))
	requireClosedForValidation(t, conn, "a first frame for another service")

	require.Equal(t, before+1, rejectedWS(rejectReasonServiceChanged))
}

// TestWebSocketClosesWhenAFrameNamesADifferentApplication: the first frame
// adopts the application, and a later frame cannot switch it.
func TestWebSocketClosesWhenAFrameNamesADifferentApplication(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	backendURL, _, _ := newSimWSBackendServer(t)
	supplier, signer := newSupplier(t)
	conn := newV1ShapedBridge(t, backendURL, signer, pipeline)
	before := rejectedWS(rejectReasonApplicationChanged)

	sendRelay(t, conn, identityRelay("app-change", supplier, simWSTestService, ownerTestAppAddr, 91, 100))
	readServedResponse(t, conn)

	sendRelay(t, conn, identityRelay("app-change", supplier, simWSTestService, "pokt1otherapp", 91, 100))
	requireClosedForValidation(t, conn, "a frame for another application")

	require.Equal(t, before+1, rejectedWS(rejectReasonApplicationChanged))
}

// TestWebSocketServesTheNextSessionOnThePinnedIdentity is the control against
// pinning too much: during this connection's grace window a frame of the next
// session arrives with the same supplier, service and application, and that is
// not a reason to close it.
func TestWebSocketServesTheNextSessionOnThePinnedIdentity(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	backendURL, _, _ := newSimWSBackendServer(t)
	supplier, signer := newSupplier(t)
	conn := newV1ShapedBridge(t, backendURL, signer, pipeline)

	reasons := []string{rejectReasonServiceChanged, rejectReasonApplicationChanged, rejectReasonSupplierChanged}
	before := make([]float64, len(reasons))
	for i, r := range reasons {
		before[i] = rejectedWS(r)
	}

	sendRelay(t, conn, identityRelay("next-session-1", supplier, simWSTestService, ownerTestAppAddr, 91, 100))
	readServedResponse(t, conn)
	sendRelay(t, conn, identityRelay("next-session-2", supplier, simWSTestService, ownerTestAppAddr, 101, 110))
	readServedResponse(t, conn)

	for i, r := range reasons {
		require.Equal(t, before[i], rejectedWS(r), "reason %s moved on a frame of the next session", r)
	}
}

// TestWebSocketAnEmptyFirstApplicationIsStillAdopted: "" is a value a frame can
// carry, so it must not read as "nothing adopted yet" and let the next frame
// choose the application.
func TestWebSocketAnEmptyFirstApplicationIsStillAdopted(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	backendURL, _, _ := newSimWSBackendServer(t)
	supplier, signer := newSupplier(t)
	conn := newV1ShapedBridge(t, backendURL, signer, pipeline)
	before := rejectedWS(rejectReasonApplicationChanged)

	sendRelay(t, conn, identityRelay("app-empty", supplier, simWSTestService, "", 91, 100))
	readServedResponse(t, conn)

	sendRelay(t, conn, identityRelay("app-empty", supplier, simWSTestService, ownerTestAppAddr, 91, 100))
	requireClosedForValidation(t, conn, "a frame naming an application after an empty one")

	require.Equal(t, before+1, rejectedWS(rejectReasonApplicationChanged))
}
