//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/pool"
)

// TestHealthCheck_RecoveryIsReportedEvenAfterAutoRecoveryTimeout proves the
// health checker's recovery transition (Info log + gauge=1) survives the
// half-open auto-recovery window. The old code computed
// `wasUnhealthy := !endpoint.IsHealthy()`, and IsHealthy MUTATES: with the
// recovery timeout already elapsed, the read itself flipped the endpoint
// healthy, wasUnhealthy came back false, and the transition was swallowed —
// no "backend became healthy" log, gauge stuck at 0. The observer must use
// the pure read (CurrentlyHealthy).
func TestHealthCheck_RecoveryIsReportedEvenAfterAutoRecoveryTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.HealthyThreshold = 2

	// Down, with the auto-recovery timeout ALREADY elapsed when the probes run.
	ep.SetRecoveryTimeout(1 * time.Millisecond)
	ep.SetUnhealthy()
	time.Sleep(5 * time.Millisecond)

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil, "")
	ctx := context.Background()

	// endpointLabel falls back to BackendURL when the endpoint name is empty;
	// newTestEndpoint derives the name from the URL host, so use ep.Name.
	gauge := backendHealthStatus.WithLabelValues("svc:jsonrpc", ep.Name)
	gauge.Set(0)

	// Two successful probes reach healthy_threshold=2: the transition MUST be
	// reported even though IsHealthy() would have auto-recovered in between.
	hc.checkPool(ctx, "svc:jsonrpc", config)
	hc.checkPool(ctx, "svc:jsonrpc", config)

	require.True(t, ep.CurrentlyHealthy())
	require.Equal(t, float64(1), testutil.ToFloat64(gauge),
		"the recovery transition must set the health gauge to 1 — a mutating "+
			"read of IsHealthy swallows it")
}

// TestHealthCheck_RecoveryIsReportedWhenTheServingPathRecoveredFirst covers
// the other order, which the test above does not reach.
//
// IsHealthy runs on the SELECTION path, where a relay picks a backend. When the
// recovery timeout has elapsed it flips the endpoint healthy right there and
// leaves pendingRecovery set, because the selection path has nowhere to report
// a transition; pool.RecordResult publishes it on the first success.
//
// If the checker's probe lands in between, it used to lose the event twice
// over: CurrentlyHealthy() already reads true, so wasUnhealthy is false and
// nothing is logged — and SetHealthy() clears pendingRecovery, so RecordResult
// has nothing left to publish either. The backend is up, the gauge says 0, and
// no line anywhere says otherwise. The checker must therefore CONSUME the
// pending mark and report it as its own.
func TestHealthCheck_RecoveryIsReportedWhenTheServingPathRecoveredFirst(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.HealthyThreshold = 1

	// Down, with an already-elapsed recovery window. 1ns needs no sleep: the
	// next clock read is past it.
	ep.SetRecoveryTimeout(1)
	ep.SetUnhealthy()

	backend := &BackendHealth{
		ServiceID:  "svc-serving-path-first",
		BackendURL: server.URL,
		endpoint:   ep,
	}
	// endpointLabel falls back to the redacted BackendURL only when the
	// endpoint has no name; newTestEndpoint derives one from the URL host, so
	// the label is ep.Name.
	gauge := backendHealthStatus.WithLabelValues(backend.ServiceID, ep.Name)
	gauge.Set(0)

	// The SERVING path recovers it first, exactly as a relay selecting a
	// backend would.
	require.True(t, ep.IsHealthy(), "the elapsed recovery window must half-open the endpoint")
	require.True(t, ep.CurrentlyHealthy(), "…and that is now the endpoint's plain state")

	// Now the checker's probe succeeds.
	hc.recordSuccess(backend, config)

	require.Equal(t, float64(1), testutil.ToFloat64(gauge),
		"the recovery must reach the gauge even though the serving path flipped the flag first")
}
