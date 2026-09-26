//go:build test

package relayer

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRedisPoolFollowsTheWorkers is the whole point of sizing.go: the pool and
// the workers are ONE number, so moving the workers moves the pool with no
// second edit. Before this, the pool was a literal 50 in transport/redis and the
// workers were NumCPU*8 in the relayer's startup, and they had drifted.
func TestRedisPoolFollowsTheWorkers(t *testing.T) {
	small := ComputeWorkerSizing(2)
	large := ComputeWorkerSizing(16)

	require.Greater(t, large.Master, small.Master, "more processors, more workers")
	require.Greater(t, large.RedisPoolSize(), small.RedisPoolSize(),
		"and the Redis pool must move with them: if this holds while the workers grow, "+
			"the pool has been decoupled from what uses it")

	// The relationship is exact, not merely monotonic: the pool is what the
	// BOUNDED Redis users need plus the margin. An assertion on "bigger" alone
	// would pass on any formula that happens to increase.
	require.Equal(t, large.Validation+large.Publish+redisPoolMargin, large.RedisPoolSize())
}

// TestMetricsWorkersAreNotInThePool pins the one subpool that is deliberately
// absent from the pool size. internal/conventions has the other half: a rule
// that goes red if the metrics recorder ever imports a Redis package.
func TestMetricsWorkersAreNotInThePool(t *testing.T) {
	s := ComputeWorkerSizing(16)
	require.Positive(t, s.Metrics, "the metrics subpool exists")
	require.Equal(t, s.Validation+s.Publish+redisPoolMargin, s.RedisPoolSize(),
		"and it is NOT a term in the pool: it issues no Redis command")
}

// TestSizingForProcessFollowsGOMAXPROCSNotNumCPU is the assertion that the
// relayer sizes itself from what the RUNTIME will schedule on, not from the
// machine's core count.
//
// It sets GOMAXPROCS to a value deliberately different from NumCPU, which is
// the only way the two can be told apart: on a machine whose CPU limit equals
// its core count -- every developer laptop, and the CI box -- NumCPU and
// GOMAXPROCS(0) agree, and a test that does not force them apart passes with
// either one. That is green by indistinguishability, not by correctness.
//
// Not parallel, and it restores the previous value: GOMAXPROCS is process-wide.
func TestSizingForProcessFollowsGOMAXPROCSNotNumCPU(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("needs at least 2 cores to make GOMAXPROCS and NumCPU differ")
	}
	prev := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	// 1 is different from NumCPU on any machine this can run on.
	runtime.GOMAXPROCS(1)
	got := ComputeWorkerSizingForProcess()

	require.Equal(t, ComputeWorkerSizing(1), got,
		"the process sizing must follow GOMAXPROCS")
	require.NotEqual(t, ComputeWorkerSizing(runtime.NumCPU()), got,
		"and it must NOT follow NumCPU: a pod limited to fewer cores than its node "+
			"would build workers for the node, which is how 144 workers ended up on 2 cores")
}

// TestSizingFloorsAtOne covers the small end: a pod with a fractional CPU limit
// rounds GOMAXPROCS to 1, and a subpool of zero workers accepts tasks that never
// run.
func TestSizingFloorsAtOne(t *testing.T) {
	for _, procs := range []int{0, -1, 1} {
		s := ComputeWorkerSizing(procs)
		require.GreaterOrEqual(t, s.Validation, 1, "procs=%d", procs)
		require.GreaterOrEqual(t, s.Publish, 1, "procs=%d", procs)
		require.GreaterOrEqual(t, s.Metrics, 1, "procs=%d", procs)
	}
}

// TestSizingFromMasterMatchesComputeWorkerSizing pins that the proxy, which
// derives the split from the pool capacity it was handed, gets the SAME split
// the startup used. Two copies of 70/20/10 is how they would drift.
func TestSizingFromMasterMatchesComputeWorkerSizing(t *testing.T) {
	for _, procs := range []int{1, 2, 8, 18} {
		full := ComputeWorkerSizing(procs)
		derived := SizingFromMaster(full.Master)
		require.Equal(t, full, derived, "procs=%d", procs)
	}
}
