//go:build test

package relayer

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// readCloseCodeFromRealPeer opens a real gorilla connection, has the server
// side write the close frame that sanitizeCloseCode produced, and returns what
// a real gorilla READER makes of those bytes.
//
// The peer is the point. A predicate of our own checking a value our own
// predicate produced is circular -- it passes for whatever we happen to write.
// gorilla's isValidReceivedCloseCode is unexported, so the only non-circular
// way to ask "will a gorilla peer accept this frame" is to hand the bytes to
// one and read the error it raises.
func readCloseCodeFromRealPeer(t *testing.T, code int, text string) error {
	t.Helper()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(code, text),
			time.Now().Add(time.Second),
		)
		// Synchronize on state, not on a clock: the socket must outlive the
		// peer's read of the close frame, and this read is what says the peer
		// got there. gorilla's default close handler echoes a close back, and
		// a peer that rejects the code answers a protocol-error close -- both
		// return here, so this unblocks for a pass and for a failure alike.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	url := "ws" + srv.URL[len("http"):]
	peer, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })

	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, readErr := peer.ReadMessage()
	return readErr
}

// TestEveryCloseCodeTheBridgeCanPickIsAcceptedByARealPeer is the test that did
// not exist: nothing asserted anything about the BYTES release() puts on the
// wire, only about the code it recorded internally.
//
// What it protects: gorilla manufactures CloseError{1006} locally for any dead
// TCP with no close frame (conn.go, errUnexpectedEOF) -- an ordinary
// disconnect. That code used to reach both write points unchanged, and 1006 is
// reserved and must not be sent: a receiving gorilla answers a protocol error.
// So a backend that dies made PATH see a protocol violation by the RELAYER, and
// PATH charges that to our endpoint's reputation.
func TestEveryCloseCodeTheBridgeCanPickIsAcceptedByARealPeer(t *testing.T) {
	// Every code the bridge can hand to release(), including the ones gorilla
	// fabricates locally and the ones RFC 6455 marks reserved.
	codes := []int{
		CloseNormalClosure, CloseGoingAway, CloseProtocolError, CloseUnsupportedData,
		CloseNoStatusReceived, CloseAbnormalClosure, CloseInvalidPayload,
		ClosePolicyViolation, CloseMessageTooBig, CloseMandatoryExtension,
		CloseInternalError, CloseServiceRestart, CloseTryAgainLater,
		CloseBadGateway, CloseTLSHandshakeFailed,
		CloseSessionExpired, CloseValidationFailed,
		CloseStakeLimitExceeded, CloseBackendConnectionFailed,
		1004,  // reserved, absent from gorilla's accept map
		3000,  // bottom of the private band
		4999,  // top of the private band
		12345, // nonsense: nothing may put an out-of-range code on the wire
	}

	for _, raw := range codes {
		sanitized, text := sanitizeCloseCode(raw, "closing")
		readErr := readCloseCodeFromRealPeer(t, sanitized, text)

		// The discriminant: a real gorilla peer must read this as a close with
		// the code we sent, never as a protocol error about the code itself.
		var ce *websocket.CloseError
		require.ErrorAs(t, readErr, &ce,
			"raw %d sanitized to %d: a real peer did not read this as a close at all: %v",
			raw, sanitized, readErr)
		require.Equal(t, sanitized, ce.Code,
			"raw %d sanitized to %d, but the peer read %d (%q): gorilla rejected the frame",
			raw, sanitized, ce.Code, ce.Text)
	}
}

// TestSanitizeCloseCodePassesThePrivateBandWhole pins the half that is easy to
// get wrong in the other direction: 3000-4999 is the band gorilla accepts
// wholesale, and the codes in it that matter are the CLIENT's own, which we
// cannot enumerate. Enumerating known Pocket codes and defaulting the rest is
// how a client's private code gets flattened.
func TestSanitizeCloseCodePassesThePrivateBandWhole(t *testing.T) {
	for _, code := range []int{3000, 3999, 4000, 4001, 4500, 4999} {
		got, _ := sanitizeCloseCode(code, "closing")
		require.Equal(t, code, got, "the private band must pass through untouched")
	}
}
