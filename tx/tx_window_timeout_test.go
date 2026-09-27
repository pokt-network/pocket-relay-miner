//go:build test

package tx

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The three networks this runs on, plus the cases that decide the SHAPE of the
// formula rather than one of its values.
//
// Both directions matter and only one of them is obvious. A window SHORTER than
// the ceiling must keep its own length: taking the ceiling there would hand
// localnet a 590 s deadline for a 100 s window. A window LONGER than the ceiling
// must be cut, and that is the one that costs money if it is wrong -- mainnet's
// 10 x 60 s is 600 s exactly, the value the chain refuses with "unordered tx ttl
// exceeds 10m0s".
//
// Together they pin `min`. Swapping it for `max` leaves the short-window rows
// green and breaks only mainnet's, which is why that row is not decoration and
// why an injection flipping the comparison has to be read against the whole
// table rather than one case.
func TestWindowTimeout(t *testing.T) {
	for _, tc := range []struct {
		name        string
		blocks      int64
		blockTime   int64
		wantTimeout time.Duration
		wantRegime  string
	}{
		{
			// 10 x 60 = 600 s, over the ceiling. The chain refuses 600.
			name:   "mainnet: the window exceeds the ceiling, so the ceiling decides",
			blocks: 10, blockTime: 60,
			wantTimeout: DefaultTxTimeoutMax, wantRegime: TimeoutRegimeCeiling,
		},
		{
			name:   "beta: the window fits",
			blocks: 10, blockTime: 30,
			wantTimeout: 300 * time.Second, wantRegime: TimeoutRegimeWindow,
		},
		{
			name:   "localnet: the window fits and is short",
			blocks: 10, blockTime: 10,
			wantTimeout: 100 * time.Second, wantRegime: TimeoutRegimeWindow,
		},
		{
			// poktroll's own defaults are 3 blocks for the claim window and 4
			// for the proof window -- NOT the 10 mainnet governs them to. A
			// literal 10 in the formula would silently give this network a
			// deadline more than three times its own window.
			name:   "chain defaults: a 3-block claim window is not 10",
			blocks: 3, blockTime: 60,
			wantTimeout: 180 * time.Second, wantRegime: TimeoutRegimeWindow,
		},
		{
			name:   "zero block time cannot be measured",
			blocks: 10, blockTime: 0,
			wantTimeout: DefaultTxTimeoutMax, wantRegime: TimeoutRegimeUnknown,
		},
		{
			name:   "zero window cannot be measured",
			blocks: 0, blockTime: 60,
			wantTimeout: DefaultTxTimeoutMax, wantRegime: TimeoutRegimeUnknown,
		},
		{
			// A negative reaches here only through a defect upstream, and the
			// point is that it lands in the COUNTED regime instead of producing
			// a plausible duration nobody questions.
			name:   "negative window cannot be measured",
			blocks: -1, blockTime: 60,
			wantTimeout: DefaultTxTimeoutMax, wantRegime: TimeoutRegimeUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, regime := WindowTimeout(tc.blocks, tc.blockTime)
			require.Equal(t, tc.wantTimeout, got, "duration")
			require.Equal(t, tc.wantRegime, regime, "regime")
		})
	}
}

// The boundary gets its own test because the table cannot express it: the
// ceiling is 589.99 s, not a whole number of seconds, so no blocks x blockTime
// product can land exactly on it.
//
// That has a consequence worth stating rather than discovering later: `>` and
// `>=` are INDISTINGUISHABLE here, and no test in this file can separate them.
// Measured, not assumed -- flipping the comparison leaves every assertion green.
// It is not a gap in the tests but a property of the units: the inputs are whole
// seconds and the ceiling is not, so the equality branch is unreachable, and
// even if it were reached both spellings return the same duration and differ
// only in the regime label.
//
// THAT UNREACHABILITY HAS A PREMISE, and it is ours, not the chain's: the
// ceiling is 600s - 10s - 10ms, and the 10ms is txNonceSpread. Remove the spread
// from that derivation -- one line, and one somebody could plausibly touch
// believing it only concerns the nonce -- and the ceiling becomes exactly 590s,
// at which point a 10-block window of 59s hits it dead on and the equality
// branch is live. Measured: 10 x 59s = 9m50s = the spread-less ceiling, exactly.
// So this is not "the number happens to be awkward"; it is a property that
// depends on a constant three lines above it, and whoever changes that constant
// has to come back here.
//
// What IS checkable, and what this pins, is that each neighbour falls on the
// right side: one second under stays a window, one second over is cut. That also
// catches the subtraction quietly losing the nonce spread, which would move the
// ceiling by 10 ms and take the second case with it.
func TestWindowTimeout_CeilingBoundary(t *testing.T) {
	got, regime := WindowTimeout(589, 1) // 589s, under 589.99s
	require.Equal(t, 589*time.Second, got,
		"a window below the ceiling keeps its own length")
	require.Equal(t, TimeoutRegimeWindow, regime)

	got, regime = WindowTimeout(590, 1) // 590s, just over
	require.Equal(t, DefaultTxTimeoutMax, got,
		"a window above the ceiling is cut to it")
	require.Equal(t, TimeoutRegimeCeiling, regime)
}

// Whatever the formula returns must be acceptable to the chain, in every regime
// and for every input.
//
// This is the property tx_timeout_max_seconds used to guard, and it guarded only
// itself: the floor was applied WITHOUT being capped, so a large enough
// tx_timeout_min_seconds sailed past the cap and had every claim and proof
// rejected -- no revenue, then PROOF_MISSING forfeits. Deriving the deadline is
// what turns that from a configuration the operator had to get right into an
// invariant nobody can switch off.
func TestWindowTimeout_NeverExceedsTheChainCeiling(t *testing.T) {
	const cosmosHardLimit = 10 * time.Minute

	for _, blocks := range []int64{-1, 0, 1, 3, 4, 10, 100, 100000} {
		for _, blockTime := range []int64{0, 1, 10, 30, 60, 600} {
			got, regime := WindowTimeout(blocks, blockTime)
			require.Less(t, got, cosmosHardLimit,
				"blocks=%d blockTime=%d regime=%s produced a deadline the chain "+
					"rejects with \"unordered tx ttl exceeds 10m0s\"", blocks, blockTime, regime)
			require.Positive(t, got,
				"blocks=%d blockTime=%d produced a non-positive deadline", blocks, blockTime)
		}
	}
}
