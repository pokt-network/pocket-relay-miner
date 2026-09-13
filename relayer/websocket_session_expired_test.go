//go:build test

package relayer

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestWebSocketSessionExpiredDispatchesReason proves the WebSocket classifier
// dispatches an expired-session validation failure to relays_rejected_total
// under "session_expired", through the real bridge (handleGatewayMessage),
// not just at the validator/sentinel level -- same rationale as the HTTP and
// gRPC siblings of this test: a wrapper along the way that drops the cause
// would make errors.Is here return false, and this dispatch would never fire
// without anything going red.
func TestWebSocketSessionExpiredDispatchesReason(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const sessionID = "sess-session-expired"

	meter := newAlwaysAllowMeter(t, ownerTestAppAddr)
	pipeline := NewRelayPipeline(sessionExpiredValidator{}, meter, testLogger())
	backendURL, _, _ := newSimWSBackendServer(t)
	supplier, signer := newSupplier(t)

	conn := newSageShapedBridge(t, backendURL, signer, pipeline)

	expired := relaysRejected.WithLabelValues(simWSTestService, "websocket", rejectReasonSessionExpired)
	genericBefore := testutil.ToFloat64(relaysRejected.WithLabelValues(simWSTestService, "websocket", rejectReasonValidationFailed))
	expiredBefore := testutil.ToFloat64(expired)

	sendRelay(t, conn, ownerTestRelay(sessionID, supplier))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, err := conn.ReadMessage()
	require.Error(t, err, "an expired session must not be served")
	require.True(t, websocket.IsCloseError(err, CloseValidationFailed),
		"closes with the validation-failure code, got %v", err)

	require.Equal(t, expiredBefore+1, testutil.ToFloat64(expired),
		"session_expired must move by exactly one")
	require.Equal(t, genericBefore, testutil.ToFloat64(relaysRejected.WithLabelValues(simWSTestService, "websocket", rejectReasonValidationFailed)),
		"the generic validation_failed reason must NOT move for this rejection")
}
