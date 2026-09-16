//go:build test

package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// recordingBackoff is a loadBackoff that records its waits instead of sleeping.
func recordingBackoff() (*loadBackoff, func() []time.Duration) {
	var mu sync.Mutex
	var waits []time.Duration
	b := &loadBackoff{sleep: func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
	}}
	return b, func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), waits...)
	}
}

func TestWebSocketLoad_BacksOffWhileTheRelayerRefuses(t *testing.T) {
	var upgrades, refusals atomic.Int64
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upgrades.Load() > 0 {
			refusals.Add(1)
			http.Error(w, "storage saturated", http.StatusTooManyRequests)
			return
		}
		upgrades.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "storage saturated"))
	}))
	defer srv.Close()

	const count = 12
	setLoadGlobals(t, "ws"+strings.TrimPrefix(srv.URL, "http"), count, 1)
	backoff, waits := recordingBackoff()
	deps := fakeChainDeps()
	deps.backoff = backoff

	_, stats, err := runWebSocketLoad(context.Background(), logging.NewLoggerFromConfig(logging.DefaultConfig()), deps, []string{"pokt1supplier"})
	require.NoError(t, err)

	got := waits()
	require.Len(t, got, count, "LINK ws-backoff: the close 1013 and every refused redial wait before the next try")
	require.Equal(t, int64(count), backoff.Waits())
	require.Equal(t, int64(count-1), refusals.Load(), "control: every redial after the first connection was refused")
	for i, d := range got {
		require.LessOrEqual(t, d, loadBackoffMax, "wait %d stays under the cap", i)
		require.GreaterOrEqual(t, d, loadBackoffMin/2, "wait %d is at least half the first delay", i)
	}
	require.Greater(t, got[len(got)-1], 4*got[0], "LINK ws-backoff-grows: the waits grow while the refusals continue")
	require.Contains(t, stats.summary(), "backoff waits 12")
}

func TestLoadBackoff_DoublesToTheCapAndResetsOnSuccess(t *testing.T) {
	b, waits := recordingBackoff()
	for i := 0; i < 12; i++ {
		b.refused()
	}
	got := waits()
	require.LessOrEqual(t, got[len(got)-1], loadBackoffMax)
	require.GreaterOrEqual(t, got[len(got)-1], loadBackoffMax/2, "LINK backoff-cap: after enough refusals the wait sits at the cap")
	b.succeeded()
	b.refused()
	require.LessOrEqual(t, waits()[len(got)], loadBackoffMin, "LINK backoff-reset: a success starts over")
}

func TestIsGRPCRefusal(t *testing.T) {
	require.True(t, isGRPCRefusal(status.Error(codes.ResourceExhausted, "storage saturated")))
	require.True(t, isGRPCRefusal(status.Error(codes.Unavailable, "not admitting")))
	require.False(t, isGRPCRefusal(status.Error(codes.PermissionDenied, "validation failed")))
	require.False(t, isGRPCRefusal(nil))
}
