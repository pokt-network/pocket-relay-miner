package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// rolloverServer is a relayer stand-in that ends a session the way
// relayer/websocket.go handleSessionExpiration does: a signed 410 "session
// expired" frame and then close 4000 on every connection of that session.
//
// It answers the first `servedBeforeBorder` relays. From then on every
// connection accepted BEFORE the border is cut at its next relay, and every
// connection accepted after it belongs to the new session and is always
// answered. The cut happens on the connection's own goroutine, at the relay
// that reaches it, so an answer and a cut never race on one connection.
type rolloverServer struct {
	servedBeforeBorder int
	// closeOnly skips the 410 frame: the relayer sends none on a connection
	// that never carried a relay (sendSessionExpirationMessage, latestReq nil).
	closeOnly bool

	mu        sync.Mutex
	delivered int  // relays answered with a success, both sessions
	cut       int  // relays that reached a connection after its session ended
	border    bool // the first session is over
	accepted  int  // connections accepted after the border
	handlers  sync.WaitGroup
}

func (s *rolloverServer) handler(t *testing.T) http.HandlerFunc {
	upgrader := websocket.Upgrader{}
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		s.handlers.Add(1)
		defer s.handlers.Done()
		defer func() { _ = conn.Close() }()

		s.mu.Lock()
		oldSession := !s.border
		if !oldSession {
			s.accepted++
		}
		s.mu.Unlock()

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			s.mu.Lock()
			if oldSession && s.delivered >= s.servedBeforeBorder {
				s.border = true
				s.cut++
				s.mu.Unlock()
				if !s.closeOnly {
					_ = conn.WriteMessage(websocket.BinaryMessage, sessionExpiredResponse(t))
				}
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "session expired"))
				// Keep reading until the client hangs up: whatever it still
				// sends on this connection is counted as cut, not lost in a reset.
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					s.mu.Lock()
					s.cut++
					s.mu.Unlock()
				}
			}
			s.delivered++
			s.mu.Unlock()
			if err := conn.WriteMessage(websocket.BinaryMessage, successResponse(t)); err != nil {
				return
			}
		}
	}
}

func successResponse(t *testing.T) []byte {
	return relayResponseBytes(t, &sdktypes.POKTHTTPResponse{
		StatusCode: http.StatusOK,
		BodyBz:     []byte(`{"jsonrpc":"2.0","result":"0x1","id":1}`),
	}, nil)
}

// sessionExpiredResponse is what relayer/signer.go BuildErrorRelayResponse
// produces for the relayer's session expiry (relayer/websocket.go:1362-1366).
func sessionExpiredResponse(t *testing.T) []byte {
	return relayResponseBytes(t, &sdktypes.POKTHTTPResponse{
		StatusCode: http.StatusGone,
		BodyBz:     []byte(`{"error":"session expired"}`),
	}, &servicetypes.RelayMinerError{Code: http.StatusGone, Description: "session expired"})
}

func relayResponseBytes(t *testing.T, payload *sdktypes.POKTHTTPResponse, minerErr *servicetypes.RelayMinerError) []byte {
	payloadBz, err := proto.Marshal(payload)
	require.NoError(t, err)
	resp := &servicetypes.RelayResponse{Payload: payloadBz, RelayMinerError: minerErr}
	bz, err := resp.Marshal()
	require.NoError(t, err)
	return bz
}

// fakeChainDeps signs nothing and verifies nothing: the pool is what is under
// test, and the relay bytes only have to reach the server and come back.
func fakeChainDeps() wsLoadDeps {
	return wsLoadDeps{
		build: func(context.Context, string) ([]byte, error) { return []byte("relay"), nil },
		verify: func(_ context.Context, _ string, bz []byte) (*servicetypes.RelayResponse, error) {
			resp := &servicetypes.RelayResponse{}
			return resp, resp.Unmarshal(bz)
		},
	}
}

func setLoadGlobals(t *testing.T, url string, count, concurrency int) {
	origURL, origSvc, origSup, origAll := RelayRelayerURL, RelayServiceID, RelaySupplierAddr, RelayAllSuppliers
	origCount, origConc, origRPS, origTimeout := RelayCount, RelayConcurrency, RelayRPS, RelayTimeout
	t.Cleanup(func() {
		RelayRelayerURL, RelayServiceID, RelaySupplierAddr, RelayAllSuppliers = origURL, origSvc, origSup, origAll
		RelayCount, RelayConcurrency, RelayRPS, RelayTimeout = origCount, origConc, origRPS, origTimeout
	})
	resetSimulationFlags(t)
	RelayRelayerURL, RelayServiceID, RelaySupplierAddr, RelayAllSuppliers = url, "svc-test", "pokt1supplier", false
	RelayCount, RelayConcurrency, RelayRPS, RelayTimeout = count, concurrency, 0, 5
}

// TestWebSocketLoad_SurvivesSessionRollover drives the load test's pool across
// a session border and checks the books balance: every relay is a success, an
// error or lost to the rollover, and each count matches what the server did.
func TestWebSocketLoad_SurvivesSessionRollover(t *testing.T) {
	for _, tc := range []struct {
		name      string
		closeOnly bool
	}{
		{name: "410 then close 4000", closeOnly: false},
		{name: "close 4000 alone", closeOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const count, concurrency, beforeBorder = 40, 4, 10
			srv := &rolloverServer{servedBeforeBorder: beforeBorder, closeOnly: tc.closeOnly}
			ts := httptest.NewServer(srv.handler(t))
			defer ts.Close()
			setLoadGlobals(t, ts.URL, count, concurrency)

			metrics, stats, err := runWebSocketLoad(context.Background(), zerolog.Nop(), fakeChainDeps(), []string{RelaySupplierAddr})
			require.NoError(t, err)
			srv.handlers.Wait()

			for msg := range metrics.errors {
				require.NotContains(t, msg, "close sent",
					"a dead connection went back to the pool: a write after its close frame failed")
			}
			require.Zero(t, metrics.errorCount, "no relay should fail outright across a rollover, got %v", metrics.errors)
			require.Equal(t, int64(srv.cut), stats.lost.Load(), "Lost must be exactly the relays the server cut")
			require.Equal(t, srv.delivered, metrics.successCount, "Successful must be exactly what the server answered")
			require.Greater(t, metrics.successCount, beforeBorder, "relays must be served again after the border")
			require.Equal(t, count, metrics.successCount+metrics.errorCount+int(stats.lost.Load()),
				"every relay asked for is a success, an error or lost to the rollover")
			// Each old connection is cut once and redialed once: the pool is
			// FIFO, and 30 relays remain for 4 slots.
			require.Equal(t, int64(concurrency), stats.lost.Load(),
				"each old connection must lose exactly one relay; more means a connection was read again after its session ended")
			require.Equal(t, int64(concurrency), stats.redialsAfterRollover.Load())
			require.Equal(t, concurrency, srv.accepted, "each redial is one new connection on the server")
			require.Zero(t, stats.redialsAfterError.Load())
			require.Zero(t, stats.dialFailures.Load())
		})
	}
}

// TestWebSocketLoad_BackendGoneIsAnError: a 410 the BACKEND answered is a
// failed relay on a healthy connection, not the relayer ending the session.
func TestWebSocketLoad_BackendGoneIsAnError(t *testing.T) {
	backendGone := relayResponseBytes(t, &sdktypes.POKTHTTPResponse{
		StatusCode: http.StatusGone,
		BodyBz:     []byte(`gone`),
	}, nil)
	var accepted atomic.Int64
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted.Add(1)
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, backendGone); err != nil {
				return
			}
		}
	}))
	defer ts.Close()
	const count, concurrency = 12, 3
	setLoadGlobals(t, ts.URL, count, concurrency)

	metrics, stats, err := runWebSocketLoad(context.Background(), zerolog.Nop(), fakeChainDeps(), []string{RelaySupplierAddr})
	require.NoError(t, err)

	require.Equal(t, count, metrics.errorCount, "every backend 410 is an error")
	require.Zero(t, stats.lost.Load(), "a backend 410 is not the relayer ending the session")
	require.Zero(t, stats.redialsAfterRollover.Load()+stats.redialsAfterError.Load(), "the connection was healthy and stays in use")
	require.Equal(t, int64(concurrency), accepted.Load())
	for msg := range metrics.errors {
		require.Contains(t, msg, "backend HTTP 410")
	}
}

// TestWebSocketLoad_UnansweredRelayTimesOut: a relayer that neither answers
// nor closes costs the relay its --timeout, as an error, and the connection is
// redialed; the worker does not wait forever.
func TestWebSocketLoad_UnansweredRelayTimesOut(t *testing.T) {
	var accepted atomic.Int64
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted.Add(1)
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer ts.Close()
	setLoadGlobals(t, ts.URL, 2, 1)

	// The request context inherits this shorter deadline, so no test waits
	// out a whole --timeout second.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	type result struct {
		metrics *RelayMetrics
		stats   *wsPoolStats
		err     error
	}
	done := make(chan result, 1)
	go func() {
		m, s, err := runWebSocketLoad(ctx, zerolog.Nop(), fakeChainDeps(), []string{RelaySupplierAddr})
		done <- result{m, s, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a relay the relayer never answered held its worker past its deadline")
	}
	require.NoError(t, res.err)

	require.Equal(t, 2, res.metrics.errorCount)
	for msg := range res.metrics.errors {
		require.Contains(t, msg, "i/o timeout")
	}
	require.Equal(t, int64(1), res.stats.redialsAfterError.Load(), "the timed-out connection is dialed again for the next relay")
	require.Zero(t, res.stats.lost.Load())
	require.Equal(t, int64(2), accepted.Load())
}
