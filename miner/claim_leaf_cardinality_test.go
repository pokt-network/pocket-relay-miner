//go:build test

package miner

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// TestEveryTerminalSkipReasonReleasesSessionGauges is a SOURCE-level guard, and
// it is source-level on purpose: the defect it closes was not a wrong helper, it
// was a helper that was never called.
//
// RecordClaimLeafStats fires in the claim build (lifecycle_callback.go, Phase 2)
// BEFORE Phase 3 decides the tree is empty, and both gauges it sets carry
// session_id. Every skipReason arm therefore ends a session that already has two
// unbounded series attached, and each arm must release them -- either by calling
// OnClaimSkipped (which reaches ClearSessionMetrics -> ClearClaimLeafStats) or by
// clearing directly.
//
// Measured 2026-08-30: the "empty_tree" arm was an empty case body with a
// comment, so a supplier whose session mined zero relays leaked two series per
// session, forever, in a long-lived process. A unit test of ClearClaimLeafStats
// would have passed throughout -- that function was always correct.
func TestEveryTerminalSkipReasonReleasesSessionGauges(t *testing.T) {
	src, err := os.ReadFile("lifecycle_callback.go")
	require.NoError(t, err, "the file this guard reads must exist")

	// Isolate the skipReason switch: from `switch r.skipReason {` to the line
	// that closes it at the same indentation.
	start := strings.Index(string(src), "switch r.skipReason {")
	require.Greater(t, start, 0,
		"the skipReason switch was renamed or removed; this guard needs updating, "+
			"not deleting -- the cardinality risk did not go away with the name")
	body := string(src)[start:]
	end := strings.Index(body, "\n\t\t\t}")
	require.Greater(t, end, 0, "could not find the end of the skipReason switch")
	body = body[:end]

	arms := regexp.MustCompile(`(?m)^\t{3}case ([^:]+):`).FindAllStringSubmatch(body, -1)
	require.NotEmpty(t, arms, "the switch must have at least one case")

	// Split the switch into per-arm bodies so each is checked on its own.
	idx := regexp.MustCompile(`(?m)^\t{3}case [^:]+:`).FindAllStringIndex(body, -1)
	for i, m := range idx {
		stop := len(body)
		if i+1 < len(idx) {
			stop = idx[i+1][0]
		}
		arm := body[m[0]:stop]
		name := strings.TrimSpace(arms[i][1])

		releases := strings.Contains(arm, "OnClaimSkipped(") ||
			strings.Contains(arm, "ClearClaimLeafStats(") ||
			strings.Contains(arm, "ClearSessionMetrics(")
		require.True(t, releases,
			"skipReason arm %s ends a session without releasing its session_id-labelled "+
				"gauges: it must call OnClaimSkipped, ClearSessionMetrics or "+
				"ClearClaimLeafStats. RecordClaimLeafStats already ran for this session "+
				"before the skip was decided, so nothing else will ever delete them.\n\narm:\n%s",
			name, arm)
	}
}

// TestClearClaimLeafStats_RemovesTheSeriesFromTheScrape is the other half: the
// release path must actually make the series disappear from what Prometheus
// sees, not merely zero it. Observed through the real /metrics exposition,
// because a gauge Set to 0 is still a live series.
func TestClearClaimLeafStats_RemovesTheSeriesFromTheScrape(t *testing.T) {
	const (
		supplier  = "pokt1cardinality"
		serviceID = "svc-cardinality"
		sessionID = "session-that-must-not-persist"
	)
	t.Cleanup(func() { ClearClaimLeafStats(supplier, serviceID, sessionID) })

	RecordClaimLeafStats(supplier, serviceID, sessionID, 0, 7)
	require.Contains(t, scrapeMinerRegistry(t), sessionID,
		"precondition: the gauges must be visible before the clear, or this test "+
			"proves nothing about the clear")

	ClearClaimLeafStats(supplier, serviceID, sessionID)
	require.NotContains(t, scrapeMinerRegistry(t), sessionID,
		"the session_id series must be GONE from the scrape, not set to zero")

	// The bounded counter is the alertable signal and must survive: an empty
	// tree with relays counted is the extreme shortfall, and dropping the
	// per-session detail must not drop the fact that it happened.
	require.Contains(t, scrapeMinerRegistry(t), "claim_leaf_collapse_total",
		"the supplier+service counter is bounded and must outlive the clear")
	_ = observability.MinerRegistry
}
