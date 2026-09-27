//go:build test

package relayer

// Cardinality guard for relays_dropped_total. The `application` label is an
// on-chain bech32 address — an unbounded set on a public deployment. On a
// Counter it creates a permanent series per app → Prometheus TSDB OOM.
// We assert the scraped exposition (what Prometheus sees) carries only the
// bounded (service_id, reason) labels.

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

func scrapeRelayerRegistry(t *testing.T) string {
	t.Helper()
	gather := prometheus.Gatherers{observability.RelayerRegistry}
	handler := promhttp.HandlerFor(gather, promhttp.HandlerOpts{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

func relayerMetricFamily(body, metricName string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metricName) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// TestRelaysDropped_NoApplicationLabel proves drops for two distinct apps
// (same service+reason) collapse to ONE series — application is not a label.
func TestRelaysDropped_NoApplicationLabel(t *testing.T) {
	const svc = "svc-drop-card"
	const reason = dropReasonValidationFailed

	// Two drops on the same service+reason but different apps must collapse
	// to one series once `application` is no longer a label.
	relaysDropped.WithLabelValues(svc, reason).Inc()
	relaysDropped.WithLabelValues(svc, reason).Inc()

	body := scrapeRelayerRegistry(t)
	const metric = "ha_relayer_relays_dropped_total"
	fam := relayerMetricFamily(body, metric)

	require.NotContainsf(t, fam, "application=",
		"relays_dropped must not carry an application label:\n%s", fam)

	n := 0
	for _, line := range strings.Split(fam, "\n") {
		if strings.HasPrefix(line, metric) && !strings.HasPrefix(line, "# ") &&
			strings.Contains(line, `service_id="`+svc+`"`) {
			n++
		}
	}
	require.Equalf(t, 1, n,
		"relays_dropped must be bounded by (service_id,reason), got %d series:\n%s", n, fam)
}
