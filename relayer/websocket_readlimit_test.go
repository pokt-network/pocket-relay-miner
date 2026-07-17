//go:build test

package relayer

import (
	"bytes"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestWebSocketBridge_OversizedGatewayFrameRejected proves an inbound frame
// larger than wsMaxMessageBytes is refused and the bridge torn down, rather
// than being buffered whole in memory.
//
// This is the pre-auth OOM path: gorilla's default read limit is unlimited and
// readLoop allocates the frame before handleGatewayMessage checks a ring
// signature, so any unauthenticated peer (CheckOrigin accepts every origin,
// validateAndLogWebSocketHandshake never rejects) could OOM the relayer with a
// single frame.
//
// The frame is deliberately unparseable as a RelayRequest, which routes it down
// handleGatewayMessage's raw forwardToBackend path. That makes backendHits the
// load-bearing assertion: with no read limit the oversized frame is forwarded
// to the backend, so this test fails without the fix rather than passing on a
// technicality.
func TestWebSocketBridge_OversizedGatewayFrameRejected(t *testing.T) {
	f := newSimWSFixture(t)

	// 0xFF bytes cannot decode as a protobuf RelayRequest (0xFF is an unending
	// varint continuation), guaranteeing the raw-forward path.
	oversized := bytes.Repeat([]byte{0xFF}, wsMaxMessageBytes+1024)

	// The write may fail rather than succeed: the relayer stops reading and
	// closes as soon as the limit is passed, so the client can see a broken
	// pipe mid-write. Both outcomes mean the frame was refused, so the write
	// result is not asserted -- the close code and the backend are.
	_ = f.gwClient.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = f.gwClient.WriteMessage(websocket.BinaryMessage, oversized)

	require.NoError(t, f.gwClient.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, err := f.gwClient.ReadMessage()
	require.Error(t, err, "relayer must close the connection on an oversized frame")

	// 1009 is gorilla's specific response to exceeding the read limit. Asserting
	// the code (not merely "an error") proves the read limit is what rejected
	// the frame, rather than the connection dying for an unrelated reason.
	require.True(t, websocket.IsCloseError(err, CloseMessageTooBig),
		"connection must close with 1009 (message too big), got: %v", err)

	require.Equal(t, int32(0), f.backendHits.Load(),
		"an oversized frame must never reach the backend")
}

// TestWebSocketBridge_MaxSizeFrameStillAccepted pins the boundary from the
// other side: a large frame under the cap must still be served. A limit set too
// low (or off by an order of magnitude) would pass the rejection test above
// while silently breaking legitimate large responses, which is the expensive
// failure -- it looks like the backend misbehaving, not like a cap.
func TestWebSocketBridge_MaxSizeFrameStillAccepted(t *testing.T) {
	f := newSimWSFixture(t)

	// Comfortably large (1MB) but well under the 15MB cap: proves the limit is
	// not merely rejecting anything bigger than a typical relay payload.
	large := bytes.Repeat([]byte{0xFF}, 1<<20)

	require.NoError(t, f.gwClient.SetWriteDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, f.gwClient.WriteMessage(websocket.BinaryMessage, large),
		"a 1MB frame is under the cap and must be accepted")

	// Unparseable as a RelayRequest, so it is raw-forwarded: the backend
	// receiving it is the proof the frame survived the read limit.
	require.Equal(t, websocket.BinaryMessage, awaitBackendFrameType(t, f),
		"a frame under the cap must be forwarded to the backend")
}
