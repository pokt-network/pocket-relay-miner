//go:build test

package tx

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// TestMetricNamesTheLiveGateQueries pins the FULLY QUALIFIED names of the
// series scripts/gates/live.sh asks Prometheus for, and asserts the gate file
// actually contains them.
//
// It exists because of a defect that shipped past level 2: the gate queried
// ha_miner_tx_broadcast_rejections_total and ha_miner_tx_broadcasts_total, and
// neither exists -- this package registers under namespace "ha" + subsystem
// "tx", and MinerFactory adds no prefix. A PromQL query for a series that does
// not exist returns ZERO, not an error, so the check read "no collisions"
// forever and could never go red, and its positive control read "nothing
// measured" forever. Level 2 does not execute live.sh, so nothing caught it.
//
// The name is assembled by promauto from three separate fields, so no amount of
// reading the Name: line tells you what the series is called. This asserts it
// from the registry, which is the only place that knows.
func TestMetricNamesTheLiveGateQueries(t *testing.T) {
	// Touch the counters so they are registered with at least one child;
	// a CounterVec with no children gathers nothing.
	txBroadcastsTotal.WithLabelValues("pokt1metricnames")
	txBroadcastRejections.WithLabelValues("claim", "sdk", "18")

	families, err := observability.MinerRegistry.Gather()
	require.NoError(t, err)

	exposed := make(map[string]struct{}, len(families))
	for _, f := range families {
		exposed[f.GetName()] = struct{}{}
	}

	gate, err := os.ReadFile("../scripts/gates/live.sh")
	require.NoError(t, err, "the live gate must be readable: it is what these names are for")

	for _, name := range []string{
		"ha_tx_broadcasts_total",
		"ha_tx_broadcast_rejections_total",
	} {
		require.Containsf(t, exposed, name,
			"the binary does not expose %q. Renaming a metric without updating "+
				"scripts/gates/live.sh does not fail the gate -- it makes the gate read zero forever", name)
		require.Truef(t, strings.Contains(string(gate), name),
			"scripts/gates/live.sh does not query %q. If the check moved, move this "+
				"assertion with it; if it was deleted, delete this too -- do not leave a "+
				"name pinned that nothing reads", name)
	}
}
