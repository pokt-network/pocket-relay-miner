//go:build test

package relayer

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// TestHealthCheckMetrics_NeverCarryARawBackendURL proves the endpoint label
// cannot leak a full backend URL into Prometheus.
//
// The label falls back to BackendURL when no pool endpoint is attached, and a
// backend URL carries operator topology plus, in the path and query, provider
// API keys — CLAUDE.md forbids URLs as labels, and a TSDB keeps a leaked one
// for the retention period. RegisterPool always attaches an endpoint today, so
// this is a latent path; the guard makes the leak impossible rather than
// improbable.
func TestHealthCheckMetrics_NeverCarryARawBackendURL(t *testing.T) {
	const secret = "SUPER-SECRET-API-KEY"

	hc := newTestHealthChecker()
	// No endpoint attached: the legacy path that falls back to BackendURL.
	backend := &BackendHealth{
		ServiceID:  "svc-label-test",
		BackendURL: "https://node.example.com/v3/" + secret + "?apikey=" + secret,
	}
	config := defaultConfig()
	config.UnhealthyThreshold = 1
	config.HealthyThreshold = 1

	hc.recordFailure(backend, config, "probe failed")
	hc.recordSuccess(backend, config)

	families, err := observability.RelayerRegistry.Gather()
	require.NoError(t, err)

	checked := 0
	for _, fam := range families {
		switch fam.GetName() {
		case "ha_relayer_backend_health_status",
			"ha_relayer_health_check_failures_total",
			"ha_relayer_health_check_successes_total":
		default:
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				checked++
				require.NotContains(t, lp.GetValue(), secret,
					"metric %s label %s leaked the backend URL's secret", fam.GetName(), lp.GetName())
				require.False(t, strings.Contains(lp.GetValue(), "?") || strings.Contains(lp.GetValue(), "/v3/"),
					"metric %s label %s carries a URL path/query: %q", fam.GetName(), lp.GetName(), lp.GetValue())
			}
		}
	}
	require.Positive(t, checked, "no health-check metric labels were inspected — the test proved nothing")
}

// compile-time guard: the registry the health-check metrics live in.
var _ prometheus.Gatherer = observability.RelayerRegistry
