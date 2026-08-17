//go:build test

package miner

// Cardinality guards for money-path counters. session_id / height /
// on-chain address are UNBOUNDED label values; on a Counter they create a
// new time-series per value that can never be DeleteLabelValues'd, so they
// accumulate for the process lifetime → Prometheus TSDB OOM at scale.
//
// These tests scrape MinerRegistry through a real promhttp handler — the
// exact wiring cmd_miner.go serves at /metrics — and assert the offending
// labels are absent and the series count stays bounded across distinct
// sessions/heights. Observing the scraped exposition (the consumer) rather
// than the internal collector is deliberate: it proves what Prometheus sees.

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// scrapeMinerRegistry dumps MinerRegistry in Prometheus text format,
// mirroring the miner's /metrics endpoint.
func scrapeMinerRegistry(t *testing.T) string {
	t.Helper()
	gather := prometheus.Gatherers{observability.MinerRegistry}
	handler := promhttp.HandlerFor(gather, promhttp.HandlerOpts{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, "/metrics must return 200")
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

// countSampleSeries counts sample lines (excluding # HELP/# TYPE) of
// metricName whose label set contains needle (use "" to count all).
func countSampleSeries(body, metricName, needle string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metricName) || strings.HasPrefix(line, "# ") {
			continue
		}
		// metricName must be followed by '{' or ' ' (avoid prefix collisions
		// like relays_added_to_smst_total vs a hypothetical _..._created).
		rest := line[len(metricName):]
		if len(rest) == 0 || (rest[0] != '{' && rest[0] != ' ') {
			continue
		}
		if needle == "" || strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// TestRelaysAddedToSMST_BoundedCardinality proves two relays on the same
// (supplier, service) but different sessions collapse to ONE series.
func TestRelaysAddedToSMST_BoundedCardinality(t *testing.T) {
	supplier := "pokt1card_added_" + fmt.Sprintf("%d", time.Now().UnixNano())
	const svc = "svc-card-add"

	// Two relays, two distinct sessions — must collapse to one series.
	RecordRelayAddedToSMST(supplier, svc)
	RecordRelayAddedToSMST(supplier, svc)

	body := scrapeMinerRegistry(t)
	const metric = "ha_miner_relays_added_to_smst_total"
	fam := extractMetric(body, metric)

	got := countSampleSeries(body, metric, fmt.Sprintf("supplier=%q", supplier))
	require.Equalf(t, 1, got,
		"relays_added_to_smst must be bounded by (supplier,service), got %d series:\n%s", got, fam)
	require.NotContainsf(t, fam, "session_id=",
		"relays_added_to_smst must not carry a session_id label:\n%s", fam)
}

// TestRelaysFailedSMST_BoundedCardinality proves the same for the failure
// counter (which additionally keeps the bounded `reason` label).
func TestRelaysFailedSMST_BoundedCardinality(t *testing.T) {
	supplier := "pokt1card_failed_" + fmt.Sprintf("%d", time.Now().UnixNano())
	const svc = "svc-card-fail"
	const reason = "transient_error"

	// Two relays, two distinct sessions, same reason — one series.
	RecordRelayFailedSMST(supplier, svc, reason)
	RecordRelayFailedSMST(supplier, svc, reason)

	body := scrapeMinerRegistry(t)
	const metric = "ha_miner_relays_failed_smst_total"
	fam := extractMetric(body, metric)

	got := countSampleSeries(body, metric, fmt.Sprintf("supplier=%q", supplier))
	require.Equalf(t, 1, got,
		"relays_failed_smst must be bounded by (supplier,service,reason), got %d series:\n%s", got, fam)
	require.NotContainsf(t, fam, "session_id=",
		"relays_failed_smst must not carry a session_id label:\n%s", fam)
}

// TestBlockResultsRetries_NoHeightLabel proves retries at distinct heights
// collapse to a single series (height is monotonic → unbounded).
func TestBlockResultsRetries_NoHeightLabel(t *testing.T) {
	// Two retries at distinct heights — must collapse to one series.
	RecordBlockResultsRetry()
	RecordBlockResultsRetry()

	body := scrapeMinerRegistry(t)
	const metric = "ha_miner_block_results_retries_total"
	fam := extractMetric(body, metric)

	require.NotContainsf(t, fam, "height=",
		"block_results_retries must not carry a height label:\n%s", fam)
	got := countSampleSeries(body, metric, "")
	require.Equalf(t, 1, got,
		"block_results_retries must be a single series, got %d:\n%s", got, fam)
}

// TestDedupMetrics_NoSessionIDLabel drives the real deduplicator consumer
// path for two sessions and proves the dedup counters carry no session_id.
func TestDedupMetrics_NoSessionIDLabel(t *testing.T) {
	d, mr := setupTestDeduplicator(t)
	defer mr.Close()
	ctx := context.Background()

	_, _ = d.IsDuplicate(ctx, []byte("h1"), "dedup-sess-A")
	_, _ = d.IsDuplicate(ctx, []byte("h2"), "dedup-sess-B")
	require.NoError(t, d.MarkProcessed(ctx, []byte("h1"), "dedup-sess-A"))
	require.NoError(t, d.MarkProcessed(ctx, []byte("h2"), "dedup-sess-B"))

	body := scrapeMinerRegistry(t)
	for _, m := range []string{
		"ha_miner_dedup_misses_total",
		"ha_miner_dedup_marked_total",
		"ha_miner_dedup_redis_cache_hits_total",
	} {
		fam := extractMetric(body, m)
		require.NotContainsf(t, fam, "session_id=", "%s must not carry session_id label:\n%s", m, fam)
	}
}
