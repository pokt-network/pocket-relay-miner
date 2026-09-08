//go:build test

package miner

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// The invariant: what the chain will bill for a claim is Count() of the root we
// flush, and what we counted while serving is the session coordinator's
// RelayCount. When the first is smaller, we performed work that will not be
// paid for, and claim_leaf_collapse_total is the only thing that says so --
// docs/CLAIM_LEAF_MODEL.md tells operators to watch exactly this counter.
//
// This test covers the recorder's decision. That the CLAIM PATH still calls it
// is covered by the deadcode gate, not here: RecordClaimLeafStats was written
// in April 2026, its only caller was deleted with claim_pipeline.go in June,
// and for four months the metric stayed declared, documented and silent. A unit
// test would not have caught that -- the function kept passing its own test
// while nothing in production reached it.
func TestRecordClaimLeafStats_CountsOnlyTheShortfall(t *testing.T) {
	const (
		supplier = "pokt1supplierclaimleaf"
		service  = "develop-http"
	)

	cases := []struct {
		name         string
		leaves       int64
		attempts     int64
		wantCollapse float64
	}{
		{
			name:         "shortfall: the FINDING's shape -- coordinator counted 4, root holds 1",
			leaves:       1,
			attempts:     4,
			wantCollapse: 1,
		},
		{
			name:         "healthy: every counted relay became a leaf",
			leaves:       4,
			attempts:     4,
			wantCollapse: 0,
		},
		{
			name:         "more leaves than counted is not a shortfall and must not fire",
			leaves:       5,
			attempts:     4,
			wantCollapse: 0,
		},
		{
			name:         "empty tree with relays counted is the extreme case, and it must fire",
			leaves:       0,
			attempts:     7,
			wantCollapse: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := "session-" + tc.name
			before := testutil.ToFloat64(claimLeafCollapseTotal.WithLabelValues(supplier, service))

			RecordClaimLeafStats(supplier, service, session, tc.leaves, tc.attempts)
			t.Cleanup(func() { ClearClaimLeafStats(supplier, service, session) })

			after := testutil.ToFloat64(claimLeafCollapseTotal.WithLabelValues(supplier, service))
			require.Equal(t, tc.wantCollapse, after-before,
				"collapse counter moved by the wrong amount for leaves=%d attempts=%d",
				tc.leaves, tc.attempts)

			require.Equal(t, float64(tc.leaves),
				testutil.ToFloat64(claimNumLeaves.WithLabelValues(supplier, service, session)),
				"claim_num_leaves must hold what the chain will bill")
			require.Equal(t, float64(tc.attempts),
				testutil.ToFloat64(claimRelayAttempts.WithLabelValues(supplier, service, session)),
				"claim_relay_attempts must hold what the coordinator counted")
		})
	}
}
