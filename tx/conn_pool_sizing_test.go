package tx

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport/grpcconn"
)

// newPoolSizingClient builds a transaction client against an endpoint nothing
// is listening on. grpc.NewClient is lazy, so the pool is real, the warm-up
// probes fail fast, and the members are still added -- which is what this file
// measures.
func newPoolSizingClient(t *testing.T, maxConcurrent int) *TxClient {
	t.Helper()

	tc, err := NewTxClient(logging.Logger{}, nil, TxClientConfig{
		GRPCEndpoint:  "127.0.0.1:59999",
		ChainID:       "test",
		MaxConcurrent: maxConcurrent,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })
	return tc
}

// TestResizeConnPool_SizesByLeasesAndIgnoresThePermitCap is the assertion that
// the permit cap does not reach the pool.
//
// It cannot be made against SizeFor, which takes one argument and so satisfies
// it trivially. The risk lives at the CALL SITE: sizing against the configured
// cap was the shape proposed first, and it is wrong because an operator who
// raises the cap would then have to restart to get the connections. This test
// pins the decision where it can actually be undone.
func TestResizeConnPool_SizesByLeasesAndIgnoresThePermitCap(t *testing.T) {
	const claimed = 400
	want := grpcconn.SizeFor(claimed)
	require.Equal(t, 5, want, "premise: 400 leases is five connections")

	for _, maxConcurrent := range []int{1, 32, 100000} {
		tc := newPoolSizingClient(t, maxConcurrent)
		require.NoError(t, tc.ResizeConnPool(context.Background(), claimed))
		require.Equal(t, want, tc.pool.Len(),
			"the pool must size by leases alone; a cap of %d must not change it", maxConcurrent)
	}
}

// TestResizeConnPool_GrowsOnlyUpward pins that a shrinking lease count leaves
// the connections in place: closing one with streams in flight would need the
// same drain as shutdown, to reclaim 64KB.
func TestResizeConnPool_GrowsOnlyUpward(t *testing.T) {
	tc := newPoolSizingClient(t, 32)

	require.NoError(t, tc.ResizeConnPool(context.Background(), 400))
	require.Equal(t, 5, tc.pool.Len())

	require.NoError(t, tc.ResizeConnPool(context.Background(), 10))
	require.Equal(t, 5, tc.pool.Len(), "there is no shrink path")
}

// TestResizeConnPool_StartsAtTheFloorBecauseTheCountIsNotKnownYet is C7: the
// pool cannot be sized at construction.
//
// The transaction client is built before the supplier manager, which builds the
// claimer inside its own Start, so at construction there is nobody to ask and
// the count would be zero. Anything that measured a fully sized pool without
// going through a resize would be measuring a path production never takes.
func TestResizeConnPool_StartsAtTheFloorBecauseTheCountIsNotKnownYet(t *testing.T) {
	tc := newPoolSizingClient(t, 32)
	require.Equal(t, grpcconn.DefaultPoolFloor, tc.pool.Len(),
		"a freshly built client has no lease count available to size with")

	require.NoError(t, tc.ResizeConnPool(context.Background(), 771))
	require.Equal(t, 10, tc.pool.Len(), "the size only becomes real after a resize")
}
