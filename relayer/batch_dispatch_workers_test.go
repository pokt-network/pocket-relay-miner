//go:build test

package relayer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBatchDispatchWorkersIsAQuarterOfTheProcessorsBetweenTwoAndEight pins the
// bounds: a small pod still gets two writers, a large node never more than eight.
func TestBatchDispatchWorkersIsAQuarterOfTheProcessorsBetweenTwoAndEight(t *testing.T) {
	cases := []struct{ procs, want int }{
		{0, 2}, {1, 2}, {8, 2}, {9, 2}, {12, 3}, {18, 4}, {32, 8}, {33, 8}, {64, 8},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, BatchDispatchWorkers(tc.procs), "procs=%d", tc.procs)
	}
}
