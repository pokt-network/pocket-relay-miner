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
