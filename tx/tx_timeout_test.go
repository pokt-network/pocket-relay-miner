//go:build test

package tx

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestComputeEffectiveTxTimeout_HardLimitSafety is the regression guard
// for the `unordered tx ttl exceeds 10m0s` CheckTx rejection observed
// by operators after build 4b09780. The previous defaults handed the
// chain a timeoutTimestamp that sat exactly at the cosmos-sdk 10-minute
// hard ceiling, and any amount of clock skew between miner host and
// validator pushed CheckTx over the edge.
//
// The fix: subtract a configurable clock-skew buffer from the raw
// window-based deadline BEFORE clamping, and default the absolute max
// to 500 ms below the cosmos-sdk hard limit.
func TestComputeEffectiveTxTimeout_HardLimitSafety(t *testing.T) {
	const (
		hardLimit = 10 * time.Minute
		min       = 2 * time.Minute                  // DefaultTxTimeoutMin
		max       = hardLimit - 500*time.Millisecond // DefaultTxTimeoutMax
		skew      = 60 * time.Second                 // DefaultTxTimeoutClockSkewBuffer
		fallback  = 2 * time.Minute                  // DefaultTxTimeoutDefault
	)

	tests := []struct {
		name           string
		raw            time.Duration
		wantTimeout    time.Duration
		wantSource     string
		lowerThanLimit bool // true iff the result leaves room under hardLimit
	}{
		{
			name:        "no raw window falls back to default",
			raw:         0,
			wantTimeout: fallback,
			wantSource:  "default",
		},
		{
			name:        "raw smaller than min after skew clamps up to min",
			raw:         30 * time.Second, // 30s - 60s = -30s → clamp to min
			wantTimeout: min,
			wantSource:  "min_clamp",
		},
		{
			name:           "typical mid-window deadline subtracts skew cleanly",
			raw:            5 * time.Minute, // 300 - 60 = 240s = 4min
			wantTimeout:    5*time.Minute - skew,
			wantSource:     "window",
			lowerThanLimit: true,
		},
		{
			name: "raw at the cosmos-sdk hard limit lands safely under via skew subtraction",
			raw:  hardLimit, // exactly 10min raw
			// hardLimit - skew = 9m, inside [min, max] → "window" path
			// lands at 9min. Crucially, NOT at hardLimit: jitter cannot
			// push CheckTx over the 10m ceiling.
			wantTimeout:    hardLimit - skew,
			wantSource:     "window",
			lowerThanLimit: true,
		},
		{
			name:           "raw above hard limit still produces a timeout strictly below hard limit",
			raw:            2 * hardLimit,
			wantTimeout:    max,
			wantSource:     "max_clamp",
			lowerThanLimit: true,
		},
		{
			name:           "raw slightly above (max + skew) clamps to max, NOT to raw-skew",
			raw:            max + skew + time.Second, // would yield max+1s after skew — must clamp
			wantTimeout:    max,
			wantSource:     "max_clamp",
			lowerThanLimit: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source := computeEffectiveTxTimeout(tt.raw, skew, min, max, fallback)
			require.Equal(t, tt.wantSource, source, "source classification wrong for raw=%s", tt.raw)
			require.Equal(t, tt.wantTimeout, got, "effective timeout wrong for raw=%s", tt.raw)
			if tt.lowerThanLimit {
				// The entire reason this helper exists: never hand the
				// chain a timeoutTimestamp at or past the hard limit.
				require.Less(t, got, hardLimit,
					"effective timeout must stay strictly below the cosmos-sdk 10m hard limit (got %s)", got)
			}
		})
	}
}

// TestComputeEffectiveTxTimeout_SkewSubtractionOrder proves that the
// skew is subtracted BEFORE clamping, not after. If the order were
// reversed (clamp → subtract), a raw deadline of exactly max would
// survive as max and only get the skew treatment post-facto, but the
// hard-limit safety of the max ceiling would be broken. We verify the
// window path produces (raw - skew) for an in-range input.
func TestComputeEffectiveTxTimeout_SkewSubtractionOrder(t *testing.T) {
	const (
		min      = 1 * time.Minute
		max      = 9 * time.Minute
		skew     = 45 * time.Second
		fallback = 2 * time.Minute
	)

	// Pick a raw that is comfortably inside the window so no clamping
	// happens. The result MUST be raw - skew exactly.
	raw := 5 * time.Minute
	got, source := computeEffectiveTxTimeout(raw, skew, min, max, fallback)

	require.Equal(t, "window", source)
	require.Equal(t, raw-skew, got,
		"effective timeout must equal (raw - skew) inside the window; got %s", got)
}

// TestComputeEffectiveTxTimeout_ZeroSkew mirrors an operator who
// explicitly sets tx_timeout_clock_skew_buffer_seconds: 0 (tightly
// co-located miner + validator, no jitter). The adjusted path should
// pass raw through unchanged, still clamped by [min, max].
func TestComputeEffectiveTxTimeout_ZeroSkew(t *testing.T) {
	got, source := computeEffectiveTxTimeout(
		5*time.Minute, 0, 1*time.Minute, 9*time.Minute, 2*time.Minute,
	)
	require.Equal(t, "window", source)
	require.Equal(t, 5*time.Minute, got)
}

// TestDefaultTxTimeout_Max_BelowCosmosHardLimit pins the invariant
// that the compiled-in default max is strictly less than 10m. If
// someone ever bumps DefaultTxTimeoutMax to 10m this test fails and
// tells them why they shouldn't.
func TestDefaultTxTimeout_Max_BelowCosmosHardLimit(t *testing.T) {
	require.Less(t, DefaultTxTimeoutMax, 10*time.Minute,
		"DefaultTxTimeoutMax must stay strictly below 10m — the cosmos-sdk "+
			"unordered-TX hard limit — so clock jitter cannot push CheckTx "+
			"past `unordered tx ttl exceeds 10m0s`")
	require.GreaterOrEqual(t, DefaultTxTimeoutMax, 10*time.Minute-1*time.Second,
		"DefaultTxTimeoutMax should stay within 1s of the hard limit — too "+
			"much headroom wastes the session's claim/proof window budget")
}
