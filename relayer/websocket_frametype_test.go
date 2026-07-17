//go:build test

package relayer

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	"github.com/stretchr/testify/require"
)

// awaitBackendFrameType returns the frame type of the next message the fake
// backend received, failing the test rather than hanging if none arrives.
func awaitBackendFrameType(t *testing.T, f *simWSFixture) int {
	t.Helper()
	select {
	case mt := <-f.backendTypes:
		return mt
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the backend to receive a frame")
		return 0
	}
}

// assertFrameTypeRoundTrip drives one relay through a real bridge and proves
// the frame type survives both hops: the type the client sent is the type the
// backend receives, and the type the backend replied with is the type the
// gateway receives.
//
// This is a regression test for the bridge hardcoding websocket.BinaryMessage
// on both data-frame writes. No relay protobuf carries a frame type, so the
// WebSocket envelope is the only channel for it -- a hardcoded write destroys
// information nothing downstream can rebuild. It ran text-only against the
// gateway hop as well, so a browser doing JSON.parse(e.data) got a Blob and
// threw. Both directions are covered so the fix cannot degenerate into
// hardcoding TextMessage instead.
//
// Each hop echoes the type it just read from that side; the two hops are
// asserted independently because they cannot be paired -- one eth_subscribe
// request yields N backend pushes. This mirrors poktroll
// (pkg/relayer/proxy/websockets/bridge.go) and PATH (websockets/bridge.go),
// both of which preserve symmetrically.
func assertFrameTypeRoundTrip(t *testing.T, frameType int) {
	t.Helper()
	f := newSimWSFixture(t)

	rr := f.buildSignedRelay(t, FormatSimSessionID(f.clock(), "frametype"))
	bz, err := rr.Marshal()
	require.NoError(t, err)

	require.NoError(t, f.gwClient.WriteMessage(frameType, bz))

	// Hop 1: client -> backend. The fake backend echoes the type it read, so
	// this also sets up hop 2's inbound type.
	require.Equal(t, frameType, awaitBackendFrameType(t, f),
		"backend must receive the frame type the client sent, not a hardcoded one")

	// Hop 2: backend -> gateway.
	require.NoError(t, f.gwClient.SetReadDeadline(time.Now().Add(5*time.Second)))
	gotType, respBz, err := f.gwClient.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, frameType, gotType,
		"gateway must receive the frame type the backend replied with, not a hardcoded one")

	// The frame is still a signed RelayResponse: preserving the type must not
	// change what is carried. A binary protobuf riding a text frame is the
	// whole stack's load-bearing hack (RFC 6455 5.6 wants text frames to be
	// valid UTF-8; nothing in this stack validates it) -- matching poktroll is
	// the point, so this asserts the payload survives rather than objecting.
	var resp servicetypes.RelayResponse
	require.NoError(t, resp.Unmarshal(respBz), "response must unmarshal as a RelayResponse")
	require.NotEmpty(t, resp.Meta.SupplierOperatorSignature, "response must still be supplier-signed")
	require.Contains(t, string(resp.Payload), `"result":"0x1"`, "response must carry the backend payload")
}

// TestWebSocketBridge_PreservesTextFrameType is the case the hardcoded
// BinaryMessage broke: a text-sending client (any browser) got binary back.
func TestWebSocketBridge_PreservesTextFrameType(t *testing.T) {
	assertFrameTypeRoundTrip(t, websocket.TextMessage)
}

// TestWebSocketBridge_PreservesBinaryFrameType pins the other half, so the fix
// is "echo the inbound type" and not "hardcode the other constant".
func TestWebSocketBridge_PreservesBinaryFrameType(t *testing.T) {
	assertFrameTypeRoundTrip(t, websocket.BinaryMessage)
}
