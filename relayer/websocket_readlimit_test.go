//go:build test

package relayer

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// shrinkWSMaxMessageBytes lowers the inbound frame cap for one test and restores
// it afterwards. Must be called BEFORE the bridge is built: NewWebSocketBridge
// reads the value once, into SetReadLimit on both connections.
//
// Safe as a package-var mutation because no test in this package calls
// t.Parallel(), so tests never overlap.
func shrinkWSMaxMessageBytes(t *testing.T, limit int64) {
	t.Helper()
	prev := wsMaxMessageBytes
	wsMaxMessageBytes = limit
	t.Cleanup(func() { wsMaxMessageBytes = prev })
}

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
	// The cap under test is a threshold, not a magnitude: what matters is that a
	// frame ABOVE it is refused. Exercising that at the production 15MB made the
	// test depend on pushing 15MB through loopback before a deadline — under a
	// slow runner the write stalls, no close frame is ever read, and the test
	// fails with an i/o timeout on behaviour that is actually correct.
	shrinkWSMaxMessageBytes(t, 64*1024)

	f := newSimWSFixture(t)

	// 0xFF bytes cannot decode as a protobuf RelayRequest (0xFF is an unending
	// varint continuation), guaranteeing the raw-forward path.
	oversized := bytes.Repeat([]byte{0xFF}, int(wsMaxMessageBytes)+1024)

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
	shrinkWSMaxMessageBytes(t, 64*1024)

	f := newSimWSFixture(t)

	// Just under the cap — the tightest frame that must still be served, which is
	// the boundary an off-by-one or an order-of-magnitude slip would break.
	large := bytes.Repeat([]byte{0xFF}, int(wsMaxMessageBytes)-1)

	require.NoError(t, f.gwClient.SetWriteDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, f.gwClient.WriteMessage(websocket.BinaryMessage, large),
		"a frame one byte under the cap must be accepted")

	// Unparseable as a RelayRequest, so it is raw-forwarded: the backend
	// receiving it is the proof the frame survived the read limit.
	require.Equal(t, websocket.BinaryMessage, awaitBackendFrameType(t, f),
		"a frame under the cap must be forwarded to the backend")
}

// TestCloseInfoForReadError pins the close code the bridge reports for each class
// of readLoop failure, independently of the socket-level test above.
//
// The socket test cannot prove this on its own: gorilla emits its own 1009 from
// advanceFrame before ReadMessage returns, so the peer may observe the right code
// even if the bridge's own close path reports the wrong one. This asserts the
// bridge's decision directly.
func TestCloseInfoForReadError(t *testing.T) {
	testCases := []struct {
		name     string
		err      error
		wantCode int
		wantText string
	}{
		{
			// A peer close frame must win: this is what carries session rollover
			// (4000 SessionExpired from PATH) through to the backend.
			name:     "peer close frame propagates its own code",
			err:      &websocket.CloseError{Code: CloseSessionExpired, Text: "session ended"},
			wantCode: CloseSessionExpired,
			wantText: "session ended",
		},
		{
			name:     "peer close frame with a standard code propagates too",
			err:      &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "bye"},
			wantCode: websocket.CloseNormalClosure,
			wantText: "bye",
		},
		{
			// The regression: ErrReadLimit is not a CloseError, so it used to fall
			// through to 1001 and tell the peer the far side went away.
			name:     "read limit breach reports message too big",
			err:      websocket.ErrReadLimit,
			wantCode: CloseMessageTooBig,
			wantText: "message too big",
		},
		{
			name:     "wrapped read limit breach is still recognised",
			err:      fmt.Errorf("gateway read failed: %w", websocket.ErrReadLimit),
			wantCode: CloseMessageTooBig,
			wantText: "message too big",
		},
		{
			name:     "any other read error falls back to going away",
			err:      errors.New("read tcp 127.0.0.1:1234->127.0.0.1:5678: i/o timeout"),
			wantCode: CloseGoingAway,
			wantText: "peer disconnected",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotCode, gotText := closeInfoForReadError(tc.err)
			require.Equal(t, tc.wantCode, gotCode, "close code")
			require.Equal(t, tc.wantText, gotText, "close reason")
		})
	}
}
