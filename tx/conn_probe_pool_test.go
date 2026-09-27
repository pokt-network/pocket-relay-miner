//go:build test

package tx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport/grpcconn"
)

// newPoolClientAgainst builds a transaction client pointed at a live mock node,
// so the probe is a real RPC and its result is what moves member health.
func newPoolClientAgainst(t *testing.T, address string) *TxClient {
	t.Helper()

	tc, err := NewTxClient(logging.Logger{}, nil, TxClientConfig{
		BlockTimeProvider: testBlockTime(),
		GRPCEndpoint:      address,
		ChainID:           "test",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })
	return tc
}

// TestResizeConnPool_WarmUpProbesEveryMemberItAdds closes the seam between
// sizing and health.
//
// pick() is covered with MarkHealth called by hand, and ResizeConnPool is
// covered for the size it produces. Neither proves that the warm-up ACTUALLY
// PROBES -- a warm-up that marked members healthy without an RPC satisfies both
// and leaves an unverified connection serving claims. Counting the probes that
// reached the server is what tells the two apart.
func TestResizeConnPool_WarmUpProbesEveryMemberItAdds(t *testing.T) {
	server := setupMockGRPCServer(t)
	t.Cleanup(server.cleanup)

	tc := newPoolClientAgainst(t, server.address)
	require.Equal(t, grpcconn.DefaultPoolFloor, tc.pool.Len())
	require.Empty(t, tc.pool.HealthyMembers(), "premise: nothing has been probed yet")

	before := server.authServer.ParamsCalls()

	// 400 leases is five connections, so three are added to the floor of two.
	require.NoError(t, tc.ResizeConnPool(context.Background(), 400))
	require.Equal(t, 5, tc.pool.Len())

	require.Equal(t, before+3, server.authServer.ParamsCalls(),
		"the warm-up must probe exactly the members it added -- no more, and above all no fewer")

	healthy := tc.pool.HealthyMembers()
	require.Len(t, healthy, 3,
		"only the warmed members may be healthy: the original two were never probed")
	for _, m := range healthy {
		require.GreaterOrEqual(t, m.Index, grpcconn.DefaultPoolFloor,
			"member %d was healthy without a warm-up probe", m.Index)
	}
}

// TestProbePool_HealthComesFromTheProbeResult is the other half of the same
// seam: the probe must move health in BOTH directions, and a probe that marked
// everything healthy would pass every size and routing test in this tree.
func TestProbePool_HealthComesFromTheProbeResult(t *testing.T) {
	server := setupMockGRPCServer(t)
	t.Cleanup(server.cleanup)

	tc := newPoolClientAgainst(t, server.address)
	require.NoError(t, tc.ResizeConnPool(context.Background(), 400))

	ctx := context.Background()
	require.NoError(t, tc.probeConn(ctx))
	require.Len(t, tc.pool.HealthyMembers(), tc.pool.Len(),
		"a sweep against a healthy node must leave every member healthy")

	server.authServer.SetParamsErr(errors.New("node refused the probe"))
	require.Error(t, tc.probeConn(ctx),
		"with every member failing, the sweep must report that nothing can serve")
	require.Empty(t, tc.pool.HealthyMembers(),
		"a member whose probe failed must not stay healthy")

	server.authServer.SetParamsErr(nil)
	require.NoError(t, tc.probeConn(ctx))
	require.Len(t, tc.pool.HealthyMembers(), tc.pool.Len(),
		"recovery must return members to service without a restart")
}

// NOT COVERED, deliberately: a mixed sweep, one member healthy and another not.
// Every member dials the same Target, so producing it needs a second server and
// a pool split across two endpoints -- machinery this design does not have and
// would exist only for the test. What is covered is that health follows the
// probe result in both directions, and that pick routes on health; the mixed
// case is those two facts composed.

// TestProbeFanOut_KeepsASweepInsideTheInterval pins the ARITHMETIC the comment
// promises rather than the constant it produces.
//
// A number in a comment is not checkable, and this one is load bearing twice
// over: too small and the sweep outruns the tick that drives it, too large and
// the burst grows with the operator's fleet against a node whose tolerance for
// concurrency is written down here as unmeasured.
func TestProbeFanOut_KeepsASweepInsideTheInterval(t *testing.T) {
	// Derived from the production constants, not restated: the slack absorbs
	// probes that answer slower than the timeout allows for, so a sweep does
	// not finish exactly as the next tick arrives.
	sweepBudget := DefaultTxConnProbeInterval - txConnProbeTimeout

	sweep := func(members int) time.Duration {
		return time.Duration(members) * txConnProbeTimeout / time.Duration(probeFanOut(members))
	}

	for _, members := range []int{1, 2, 10, 63, 64, 65, 126, 626, 703, 704} {
		require.LessOrEqual(t, sweep(members), sweepBudget,
			"a sweep of %d members must fit the budget with %d workers", members, probeFanOut(members))
		require.LessOrEqual(t, probeFanOut(members), maxProbeConcurrency,
			"the fan-out must stay bounded: it is what keeps the burst off the node")
	}

	// The documented degradation point. Past it the sweep stretches and the
	// ticker drops ticks, which is the accepted failure -- it is asserted so
	// that anyone who moves the constant sees where the promise stops holding.
	require.Greater(t, sweep(705), sweepBudget,
		"704 members is where the comment says the budget stops covering a sweep")
	require.Equal(t, maxProbeConcurrency, probeFanOut(705))
}

// TestProbeFanOut_NeverStartsIdleWorkers keeps the small case honest: a pool of
// two must not spin up sixty-four workers to probe two connections.
func TestProbeFanOut_NeverStartsIdleWorkers(t *testing.T) {
	for _, members := range []int{1, 2, 5, 63} {
		require.Equal(t, members, probeFanOut(members))
	}
}
