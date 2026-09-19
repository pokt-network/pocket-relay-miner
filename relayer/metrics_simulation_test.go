//go:build test

package relayer

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestSimulatedRelaysMetric_IsolatedAndLabeled proves the simulated-relay
// counter increments independently under its four bounded labels and that
// incrementing it does NOT touch any real-relay counter. This is the metric
// half of the "simulation never pollutes real metrics" guarantee.
func TestSimulatedRelaysMetric_IsolatedAndLabeled(t *testing.T) {
	// Deltas, not absolute values. These counters are package-level and outlive
	// the test, so a second run of this same test starts from whatever the first
	// one left behind: asserting 2 passed once and then read 4, and then 6.
	//
	// The real-relay counter below was already measured as a delta; the
	// simulated ones were not, and that is the whole defect -- the test asserted
	// a total it does not own. What it means to prove is that ITS increments
	// land on the right series, which is a difference.
	success := simulatedRelaysTotal.WithLabelValues("jsonrpc", "svc", "pokt1sup", "success")
	failed := simulatedRelaysTotal.WithLabelValues("grpc", "svc", "pokt1sup", "verify_failed")

	realBefore := testutil.ToFloat64(relaysServed.WithLabelValues("svc", "3", "200"))
	successBefore := testutil.ToFloat64(success)
	failedBefore := testutil.ToFloat64(failed)

	success.Inc()
	success.Inc()
	failed.Inc()

	require.Equal(t, float64(2), testutil.ToFloat64(success)-successBefore,
		"two increments under one label set must move that series by exactly two")
	require.Equal(t, float64(1), testutil.ToFloat64(failed)-failedBefore,
		"a different label set must move independently")

	// The real counter is untouched by simulated activity.
	require.Equal(t, realBefore,
		testutil.ToFloat64(relaysServed.WithLabelValues("svc", "3", "200")),
		"simulated relays must not increment real relay counters")
}

// TestSimulatedRelayDuration_Observes proves the histogram accepts observations
// under its two bounded labels without panicking.
func TestSimulatedRelayDuration_Observes(t *testing.T) {
	simulatedRelayDuration.WithLabelValues("websocket", "svc").Observe(0.012)
	require.GreaterOrEqual(t,
		testutil.CollectAndCount(simulatedRelayDuration), 1,
		"histogram must expose at least the observed series")
}
