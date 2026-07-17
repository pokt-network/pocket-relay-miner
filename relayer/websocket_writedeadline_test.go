//go:build test

package relayer

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// newStalledPeerConn returns a client connection to a WebSocket server that
// completes the handshake and then never reads a single frame, holding the
// connection open until the test ends. This is the shape of a real stalled
// reader: the peer is alive at the TCP level, so nothing errors, but its
// receive buffer fills and never drains.
func newStalledPeerConn(t *testing.T) *websocket.Conn {
	t.Helper()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Deliberately never call ReadMessage. Block until the test is done so
		// the connection stays open rather than being torn down by the handler
		// returning -- a closed peer would produce a write error for the wrong
		// reason and make this test pass vacuously.
		<-release
	}))
	t.Cleanup(srv.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// TestWriteDataFrame_StalledPeerTimesOut proves a peer that stops reading
// cannot block a data-frame write forever.
//
// Without the deadline, gorilla's WriteMessage blocks indefinitely once the
// stalled peer's receive buffer fills: it does not observe a context, so the
// bridge's single messageLoop goroutine can never reach its ctx.Done() select,
// pingLoop cancelling the context cannot free it, and because every write is
// serialised on the same mutex the whole bridge wedges until the OS-level TCP
// timeout. This test would hang rather than fail if the deadline were removed,
// which is why it drives a real stalled peer instead of asserting on the
// deadline value.
func TestWriteDataFrame_StalledPeerTimesOut(t *testing.T) {
	conn := newStalledPeerConn(t)

	var mu sync.Mutex
	// 1MB per frame: kernel socket buffers are finite but can hold a few
	// hundred KB, so the write only blocks once they are full. Looping is what
	// makes this deterministic -- the peer never drains, so buffers cannot
	// recover and some iteration must block into the deadline.
	chunk := make([]byte, 1<<20)

	var err error
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; i < 256; i++ {
		if err = writeDataFrame(conn, &mu, websocket.BinaryMessage, chunk, 50*time.Millisecond); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writes kept succeeding against a peer that never reads: the write deadline is not bounding the write")
		}
	}

	require.Error(t, err, "a write to a stalled peer must eventually fail rather than block forever")

	// Assert it failed for the right reason. Any error would satisfy the check
	// above -- only a timeout proves the deadline is what stopped it, rather
	// than the connection dying for some unrelated reason.
	var netErr net.Error
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(),
		"write must fail with a timeout from the write deadline, got: %v", err)
}

// TestWriteDataFrame_DeadlineDoesNotBreakHealthyWrites pins the other half: the
// deadline is per-write, so a connection that is being read normally keeps
// working across many writes. A deadline set once and never refreshed would
// pass the stalled-peer test above while breaking every long-lived connection
// after the first expiry -- exactly the regression this guards.
func TestWriteDataFrame_DeadlineDoesNotBreakHealthyWrites(t *testing.T) {
	readerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(readerDone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	var mu sync.Mutex
	// A short write wait plus a gap longer than it: if the deadline were set
	// once instead of per write, the second write would land after expiry and
	// fail.
	for i := 0; i < 5; i++ {
		require.NoError(t,
			writeDataFrame(client, &mu, websocket.TextMessage, []byte(`{"jsonrpc":"2.0"}`), 50*time.Millisecond),
			"write %d to a healthy peer must succeed: the deadline must be refreshed per write", i)
		time.Sleep(60 * time.Millisecond)
	}

	require.NoError(t, client.Close())
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the reader to observe the close")
	}
}
