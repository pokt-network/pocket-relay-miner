//go:build test

package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPickRandomSupplier_StaysInTheSet is the floor: whatever is returned must
// be one of the session's suppliers. Returning anything else would send a signed,
// metered relay to an address that is not serving this session.
func TestPickRandomSupplier_StaysInTheSet(t *testing.T) {
	suppliers := []string{"pokt1aaa", "pokt1bbb", "pokt1ccc"}
	seen := map[string]bool{}
	for _, s := range suppliers {
		seen[s] = true
	}

	for i := 0; i < 200; i++ {
		got, err := pickRandomSupplier(suppliers)
		require.NoError(t, err)
		require.True(t, seen[got], "picked %q, which is not a session supplier", got)
	}
}

// TestPickRandomSupplier_IsActuallyRandom is the assertion that matters, and the
// one a suppliers[0] implementation fails. --all-suppliers means "do not make me
// name one"; always returning the same element would silently turn the flag into
// a fixed choice and drain that supplier's per-session claimable budget.
//
// 200 draws over 3 suppliers: the probability that a uniform picker misses any
// given supplier is (2/3)^200, far below any flake threshold, so this is
// deterministic in practice rather than merely likely.
func TestPickRandomSupplier_IsActuallyRandom(t *testing.T) {
	suppliers := []string{"pokt1aaa", "pokt1bbb", "pokt1ccc"}

	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		got, err := pickRandomSupplier(suppliers)
		require.NoError(t, err)
		counts[got]++
	}

	require.Len(t, counts, len(suppliers),
		"every supplier must come up over 200 draws; got %v -- a picker that "+
			"always returns the same element defeats the whole point of the flag", counts)
}

// TestPickRandomSupplier_SingleSupplier covers the degenerate set: one supplier
// is picked every time, with no error.
func TestPickRandomSupplier_SingleSupplier(t *testing.T) {
	got, err := pickRandomSupplier([]string{"pokt1only"})
	require.NoError(t, err)
	require.Equal(t, "pokt1only", got)
}

// TestPickRandomSupplier_EmptyIsAnError guards the case the caller cannot
// currently produce: an empty set must not panic on big.NewInt(0).
func TestPickRandomSupplier_EmptyIsAnError(t *testing.T) {
	_, err := pickRandomSupplier(nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "no suppliers")
}

// TestShouldPinLocalnetSupplier pins the precedence that shipped broken: the
// --localnet default runs ~170 lines before the random pick, so if it fills in
// supplier1 whenever the address is empty, --all-suppliers is dead under
// --localnet -- which is how the live gate and every local test invoke the CLI.
//
// The bug was invisible to a unit test of the picker itself: that helper was
// correct and simply never called. This is the assertion that would have caught
// it.
func TestShouldPinLocalnetSupplier(t *testing.T) {
	tests := []struct {
		name         string
		currentAddr  string
		allSuppliers bool
		simulate     bool
		want         bool
	}{
		{name: "no supplier, no flag: localnet fills in its default", want: true},
		{name: "no supplier, --all-suppliers: leave it empty for the random pick", allSuppliers: true, want: false},
		{name: "explicit --supplier wins over the localnet default", currentAddr: "pokt1explicit", want: false},
		{name: "explicit --supplier wins over --all-suppliers too", currentAddr: "pokt1explicit", allSuppliers: true, want: false},
		// --simulate resolves BEFORE the random pick and rejects an empty
		// address, so suppressing the default would turn a working invocation
		// into a startup error. A simulated relay wants the simulation
		// identity's supplier, not an arbitrary one.
		{name: "--simulate keeps the default even with --all-suppliers", allSuppliers: true, simulate: true, want: true},
		{name: "--simulate alone still gets the default", simulate: true, want: true},
		{name: "--simulate does not override an explicit --supplier", currentAddr: "pokt1explicit", simulate: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want,
				shouldPinLocalnetSupplier(tt.currentAddr, tt.allSuppliers, tt.simulate))
		})
	}
}
