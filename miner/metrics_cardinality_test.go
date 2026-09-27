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

// TestClaimsProofsCreated_BoundedLabels proves the pre-submit attempt counters
// (claims_created_total / proofs_created_total) are bounded by (supplier,
// service_id) and carry no session_id label.
func TestClaimsProofsCreated_BoundedLabels(t *testing.T) {
	sup := "pokt1created_" + fmt.Sprintf("%d", time.Now().UnixNano())
	RecordClaimCreated(sup, "svc-c")
	RecordClaimCreated(sup, "svc-c") // same labels -> still 1 series
	RecordProofCreated(sup, "svc-p")
	body := scrapeMinerRegistry(t)
	for _, m := range []string{"ha_miner_claims_created_total", "ha_miner_proofs_created_total"} {
		fam := extractMetric(body, m)
		require.NotContainsf(t, fam, "session_id=", "%s must not carry session_id:\n%s", m, fam)
	}
	require.Equal(t, 1, countSampleSeries(body, "ha_miner_claims_created_total", fmt.Sprintf("supplier=%q", sup)))
}

// TestDedupMetrics_NoSessionIDLabel drives the real deduplicator consumer
// path for two sessions and proves the dedup counters carry no session_id.
func TestDedupMetrics_NoSessionIDLabel(t *testing.T) {
	d, mr := setupTestDeduplicator(t)
	defer mr.Close()
	ctx := context.Background()

	_, _ = d.IsDuplicate(ctx, []byte("h1"), "dedup-sess-A")
	_, _ = d.IsDuplicate(ctx, []byte("h2"), "dedup-sess-B")
	mustMarkProcessed(t, d, ctx, []byte("h1"), "dedup-sess-A")
	mustMarkProcessed(t, d, ctx, []byte("h2"), "dedup-sess-B")

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
