//go:build test

package relayer

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestHTTPWithoutAMeterRefusesBeforeServing proves a relayer with no meter wired
// refuses every relay at admission, in both validation modes. Optimistic is the
// one that matters: it meters after the response is sent, so a check there could
// only serve the relay and then fail to charge it.
func TestHTTPWithoutAMeterRefusesBeforeServing(t *testing.T) {
	for _, mode := range []ValidationMode{ValidationModeEager, ValidationModeOptimistic} {
		t.Run(string(mode), func(t *testing.T) {
			var backendHits atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				backendHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer backend.Close()

			f := newSimHTTPFixture(t, backend.URL, mode)
			require.Nil(t, f.proxy.relayMeter, "precondition: the fixture wires no meter")

			// A full queue at the same time: a process wired without a meter must name
			// that, not the queue.
			f.proxy.SetPublishQueueFull(func() bool { return true })

			body := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-no-meter-"+string(mode))
			rejected := relaysRejected.WithLabelValues(simTestService, BackendTypeJSONRPC, rejectReasonMeteringNotConfigured)
			queueFull := relaysRejected.WithLabelValues(simTestService, BackendTypeJSONRPC, rejectReasonPublishQueueFull)
			before, queueBefore := testutil.ToFloat64(rejected), testutil.ToFloat64(queueFull)

			w := f.post(t, body, false)

			require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
			require.Equal(t, before+1, testutil.ToFloat64(rejected))
			require.Equal(t, queueBefore, testutil.ToFloat64(queueFull), "the missing meter is checked before the queue")
			require.Equal(t, int32(0), backendHits.Load(), "a relay nothing would charge must not be served")
			require.Equal(t, int32(0), f.pub.calls.Load())
		})
	}
}
