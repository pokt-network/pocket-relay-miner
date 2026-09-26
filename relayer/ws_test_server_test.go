//go:build test

package relayer

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// wsTestServer starts an httptest.Server and, at cleanup, waits for every
// handler invocation to RETURN before the test is allowed to end.
//
// The wait is the whole point, and srv.Close() does not provide it. A WebSocket
// handler hijacks the connection, and the stdlib is explicit about what that
// costs: net/http/server.go leaves a hijacked connection without a final
// ConnState hook, and httptest/server.go calls its WaitGroup's Done on
// StateHijacked -- at the INSTANT of the upgrade, before the handler has done
// any of its work. So Close() returns knowing nothing about the body of the
// handler, and nothing orders what the handler reads against what the NEXT test
// writes.
//
// Measured 2026-09-20 (item 390): with `-race -count=5 ./relayer/`, about one
// run in five reported a DATA RACE between
// TestBridgeClosesAConnectionThatNeverSendsAFrame writing wsFirstFrameWait and
// NewWebSocketBridge reading it from the handler goroutine of
// TestWebSocketHandlerClosesAConnectionItRefusesWhileDraining. The race
// detector showed that reader as `(finished)`: the goroutine was already done.
// Nothing had SURVIVED -- what was missing was an edge saying so. This
// WaitGroup is that edge.
//
// The ordering of the two cleanups is load-bearing and is why Close is
// registered second: t.Cleanup runs LIFO, so Close runs first and the wait
// second. That order is also what makes the WaitGroup safe to Add inside the
// handler. Once Close has returned, the listener is shut and no new invocation
// can begin, while any invocation that will ever run has already reached its
// Add -- a hijacking handler reaches Add before the upgrade the client is
// waiting on, and a non-hijacking one is waited for by Close itself.
func wsTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()

	var wg sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Add(1)
		defer wg.Done()
		h.ServeHTTP(w, r)
	}))

	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			// A handler that never returns is a real defect and must be named,
			// not waited on forever until the whole package times out with a
			// stack dump nobody reads. It means the handler took the path that
			// runs the bridge, and the test has to end the bridge before it
			// ends itself.
			t.Error("a WebSocket handler was still running 10s after the server closed: " +
				"the test must end whatever keeps it running before it returns")
		}
	})
	t.Cleanup(srv.Close)

	return srv
}

// runBridge starts a bridge and, at cleanup, closes it and WAITS for Run to
// return before the test ends.
//
// The wait is the point, and Close does not provide it: Close "only SIGNALS"
// -- its own words, websocket.go:1704-1706 -- and Run's deferred release does
// the work afterwards. Ten tests started a bridge with `go bridge.Run()` and a
// cleanup that only closed it, so ten Run goroutines outlived their tests.
//
// A bridge that outlives its test is not idle: Run is what serves frames, and
// serving one calls the meter, which increments
// relayMeterConsumptions{service,"within_limit"} (relay_meter.go:431). That
// counter is process-global and twelve test files share the same service ID, so
// a straggler lands an increment inside the window of whatever test runs next.
// Measured 2026-09-20: TestAWebSocketChargesEachBackendMessageAndNotTheFrame
// asserts `checksBefore+1` on exactly that series and failed about once in
// twenty-five repetitions -- a test claiming exclusivity over a counter it does
// not own.
//
// Same shape as wsTestServer above and same reason: the fix is an edge, not a
// prevention. Close is registered second so LIFO runs it first and the wait
// second, which is also what makes the wait terminate.
func runBridge(t *testing.T, b *WebSocketBridge) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run()
	}()

	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("a bridge's Run did not return 10s after Close: the test cannot " +
				"end while it may still serve a frame and charge it against the next test")
		}
	})
	t.Cleanup(func() { _ = b.Close() })
}
