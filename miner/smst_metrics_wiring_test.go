//go:build test

package miner

// End-to-end wiring verification for the new SMST defensive metrics.
// Declaring a metric in observability/metrics.go only creates the
// collector; for an operator alert rule to fire the metric must also
// (a) be registered in a registry the miner's /metrics endpoint serves
// and (b) be incremented by the code path it claims to observe.
//
// These tests scrape MinerRegistry through a real promhttp handler —
// the exact wiring cmd_miner.go plugs into combinedRegistry — and
// assert that a forced panic / eviction produces the expected
// ha_smst_panics_recovered_total and
// ha_smst_corruption_evictions_total samples.

import (
	"io"
	"net/http/httptest"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// scrapeMinerMetrics returns the text-format dump from a promhttp
// handler bound to the MinerRegistry, replicating cmd_miner.go's
// combinedRegistry exposition.
func (s *RedisSMSTTestSuite) scrapeMinerMetrics() string {
	gather := prometheus.Gatherers{observability.MinerRegistry}
	handler := promhttp.HandlerFor(gather, promhttp.HandlerOpts{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(rec, req)
	s.Require().Equal(200, rec.Code, "/metrics must return 200")
	body, err := io.ReadAll(rec.Body)
	s.Require().NoError(err)
	return string(body)
}

// TestMetrics_PanicsRecoveredWiring verifies that runSMSTSafely's
// increment lands in MinerRegistry and becomes scrapeable at /metrics.
func (s *RedisSMSTTestSuite) TestMetrics_PanicsRecoveredWiring() {
	supplier := "pokt1wiring_panic"
	mgr := s.createTestRedisSMSTManager(supplier)

	// Force the recover path: pass a fn that panics. runSMSTSafely
	// catches, logs, increments SMSTPanicsRecovered, returns error.
	err := mgr.runSMSTSafely("session_wiring_panic", "test_op", func() error {
		panic("synthetic for metric wiring test")
	})
	s.Require().Error(err, "runSMSTSafely must surface the recovered panic as an error")
	s.Require().ErrorIs(err, ErrSMSTPanicRecovered,
		"returned error must satisfy errors.Is(ErrSMSTPanicRecovered)")

	body := s.scrapeMinerMetrics()
	s.Require().Contains(body,
		`ha_smst_panics_recovered_total{operation="test_op",supplier="pokt1wiring_panic"} 1`,
		"panics_recovered metric must be scrapeable with exact labels & value")
}

// TestMetrics_CorruptionEvictionsWiring verifies that evictCorruptSession's
// increment lands in MinerRegistry and is scrapeable.
func (s *RedisSMSTTestSuite) TestMetrics_CorruptionEvictionsWiring() {
	supplier := "pokt1wiring_evict"
	sessionID := "session_wiring_evict"
	mgr := s.createTestRedisSMSTManager(supplier)

	// Seed one update so the session exists in m.trees.
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
		[]byte("k"), []byte("v"), 10))

	mgr.evictCorruptSession(s.ctx, sessionID, "wiring_test_reason")

	body := s.scrapeMinerMetrics()
	// The exact sample line proves: registered in MinerRegistry,
	// exposed with the metrics namespace + subsystem + name, and
	// labelled with (supplier, reason) in that order.
	needle := `ha_smst_corruption_evictions_total{reason="wiring_test_reason",supplier="pokt1wiring_evict"} 1`
	s.Require().Truef(strings.Contains(body, needle),
		"corruption_evictions metric missing or mis-labelled in /metrics dump\nwant: %s\ngot metric block containing:\n%s",
		needle, extractMetric(body, "ha_smst_corruption_evictions_total"))
}

// extractMetric is a tiny helper to surface only the relevant metric
// family when a wiring assertion fails, so the error message stays
// readable instead of dumping the full scrape.
func extractMetric(body, metricName string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metricName) ||
			strings.HasPrefix(line, "# HELP "+metricName) ||
			strings.HasPrefix(line, "# TYPE "+metricName) {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return "<metric family not present at all>"
	}
	return strings.Join(out, "\n")
}
