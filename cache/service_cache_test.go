//go:build test

package cache

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// frozenServiceQueryClient models the query layer's in-process cache: once it
// serves a value it keeps serving it (frozen) until InvalidateService is called.
// This is the exact behavior that caused the CUPR incident — a force refresh that
// does not invalidate the query layer re-reads the stale value forever.
type frozenServiceQueryClient struct {
	chainCUPR       uint64  // the current on-chain value
	served          *uint64 // what GetService returns until invalidated
	invalidateCalls int
}

func (f *frozenServiceQueryClient) GetService(_ context.Context, serviceID string) (*sharedtypes.Service, error) {
	if f.served == nil {
		v := f.chainCUPR
		f.served = &v
	}
	return &sharedtypes.Service{Id: serviceID, ComputeUnitsPerRelay: *f.served}, nil
}

func (f *frozenServiceQueryClient) InvalidateService(_ string) {
	f.invalidateCalls++
	f.served = nil // next GetService re-reads the (possibly changed) chain value
}

func newTestRedis(t *testing.T) *redisutil.Client {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	client, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestServiceCache_ForceRefresh_PicksUpCUPRChange is the end-to-end regression
// test for the CUPR incident: a leader force-refresh (Get with force=true) must
// invalidate the query layer so an on-chain compute_units_per_relay change
// propagates, instead of being frozen for the process lifetime.
func TestServiceCache_ForceRefresh_PicksUpCUPRChange(t *testing.T) {
	client := newTestRedis(t)
	fq := &frozenServiceQueryClient{chainCUPR: 6276}
	sc := NewServiceCache(logging.NewLoggerFromConfig(logging.DefaultConfig()), client, fq)

	ctx := context.Background()
	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Close() })

	// Initial lazy load caches the old CUPR.
	svc, err := sc.Get(ctx, "seda")
	require.NoError(t, err)
	require.Equal(t, uint64(6276), svc.GetComputeUnitsPerRelay())

	// On-chain CUPR changes mid-session.
	fq.chainCUPR = 6312

	// Leader force-refresh (what CacheOrchestrator.RefreshEntity does each block).
	svc, err = sc.Get(ctx, "seda", true)
	require.NoError(t, err)
	require.Equal(t, uint64(6312), svc.GetComputeUnitsPerRelay(),
		"force refresh must invalidate the query layer and pick up the new CUPR")
	require.GreaterOrEqual(t, fq.invalidateCalls, 1,
		"force refresh must call InvalidateService on the query layer")
}
