//go:build test

package relayer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionHeightsPlausible is the F3 unit contract: legitimate session headers
// (active and grace-period) pass; obviously-bogus ones that would otherwise drive
// a pre-signature at-height chain query are rejected — all without a chain call.
func TestSessionHeightsPlausible(t *testing.T) {
	const arrival = int64(1_000_000)

	testCases := []struct {
		name          string
		start         int64
		end           int64
		arrival       int64
		wantPlausible bool
	}{
		// Legitimate: active session straddling the current height.
		{name: "active session", start: arrival - 10, end: arrival + 10, arrival: arrival, wantPlausible: true},
		// Legitimate: freshly-opened session whose start is the current height.
		{name: "fresh session at head", start: arrival, end: arrival + 20, arrival: arrival, wantPlausible: true},
		// Legitimate: grace-period relay for a just-ended session.
		{name: "grace period ended session", start: arrival - 30, end: arrival - 5, arrival: arrival, wantPlausible: true},
		// Legitimate: relayer lags a couple blocks behind a fresh session.
		{name: "minor arrival lag", start: arrival + 3, end: arrival + 23, arrival: arrival, wantPlausible: true},

		// Bogus structural.
		{name: "zero start", start: 0, end: 20, arrival: arrival, wantPlausible: false},
		{name: "negative start", start: -5, end: 20, arrival: arrival, wantPlausible: false},
		{name: "end equals start", start: 100, end: 100, arrival: arrival, wantPlausible: false},
		{name: "end before start", start: 200, end: 100, arrival: arrival, wantPlausible: false},

		// Bogus: absurd session length.
		{name: "absurd length", start: arrival, end: arrival + maxPlausibleSessionLengthBlocks + 1, arrival: arrival, wantPlausible: false},

		// Bogus: attacker-chosen far-future / far-past heights (the amplification vector).
		{name: "far future start", start: arrival + 5_000_000, end: arrival + 5_000_020, arrival: arrival, wantPlausible: false},
		{name: "far past end", start: 1, end: 21, arrival: arrival, wantPlausible: false},
		{name: "ancient session", start: 500_000, end: 500_020, arrival: arrival, wantPlausible: false},

		// Boot window: no block seen yet -> allowed through (later validation decides),
		// but still structurally sane.
		{name: "boot window sane header", start: 100, end: 120, arrival: 0, wantPlausible: true},
		{name: "boot window bogus structure", start: 120, end: 100, arrival: 0, wantPlausible: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionHeightsPlausible(tc.start, tc.end, tc.arrival)
			require.Equal(t, tc.wantPlausible, got,
				"sessionHeightsPlausible(start=%d, end=%d, arrival=%d)", tc.start, tc.end, tc.arrival)
		})
	}
}

// TestSessionHeightsPlausible_CollapsesAttackerSpace proves the bound reduces the
// attacker-usable distinct-height space to a bounded band around the arrival
// height, instead of the whole int64 range — the property that neutralises the
// pre-auth at-height amplification.
func TestSessionHeightsPlausible_CollapsesAttackerSpace(t *testing.T) {
	const arrival = int64(1_000_000)

	// Every plausible start height must lie within the bounded band.
	lowBound := arrival - maxSessionLookbackBlocks - maxPlausibleSessionLengthBlocks
	highBound := arrival + maxSessionLookaheadBlocks

	for _, start := range []int64{
		lowBound - 1,          // just below the band
		highBound + 1,         // just above the band
		1,                     // genesis-ish
		arrival + 100_000_000, // wildly future
	} {
		// Pair each with a minimal valid-length session; only the band membership
		// should decide the outcome for the far-out ones.
		require.False(t, sessionHeightsPlausible(start, start+10, arrival),
			"start=%d outside the plausible band must be rejected", start)
	}
}
