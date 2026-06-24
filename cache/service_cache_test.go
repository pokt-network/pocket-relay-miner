//go:build test

package cache

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// TestServiceCache_L1RefreshesAfterTTL is the regression test for the final CUPR
// gap. The cache-pkg ServiceCache L1 (in-process xsync map) had NO TTL, so once
// the relayer cached a service its compute_units_per_relay was frozen for the
// process lifetime: pub/sub invalidation never fires for services (the leader's
// known-services set is empty), so a new session kept mining the stale CUPR and
// every such claim was skipped on-chain. The fix ages L1 entries out after
// serviceCacheL1TTL so Get falls through to L2/L3 and follows the on-chain CUPR
// WITHOUT a pod restart. This test drives that change against the REAL service
// cache with miniredis — coverage the fakes-only provider test could not give.
func TestServiceCache_L1RefreshesAfterTTL(t *testing.T) {
	client := newTestRedis(t)
	fq := &frozenServiceQueryClient{chainCUPR: 1000}
	sc := NewServiceCache(logging.NewLoggerFromConfig(logging.DefaultConfig()), client, fq)

	ctx := context.Background()
	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Close() })

	// Use a large L1 TTL while we prove caching; restore the package default after.
	origTTL := serviceCacheL1TTL
	serviceCacheL1TTL = time.Hour
	t.Cleanup(func() { serviceCacheL1TTL = origTTL })

	// Initial lazy load caches CUPR=1000 in L1 (and L2).
	svc, err := sc.Get(ctx, "svc")
	require.NoError(t, err)
	require.Equal(t, uint64(1000), svc.GetComputeUnitsPerRelay())

	// On-chain CUPR changes mid-flight. Reproduce the downstream a stale-L1
	// re-read hits in production steady state: L2 empty (the leader never
	// populates ha:cache:service:* for services) and the L3 query client past its
	// own TTL (the frozen stub re-reads the chain value once InvalidateService
	// fires).
	fq.chainCUPR = 1500
	fq.InvalidateService("svc")
	require.NoError(t, client.Del(ctx, client.KB().CacheKey(serviceCacheType, "svc")).Err())

	// Within the (huge) L1 TTL: Get must still serve the cached CUPR, even though
	// the downstream value already changed. Proves L1 actually caches.
	svc, err = sc.Get(ctx, "svc")
	require.NoError(t, err)
	require.Equal(t, uint64(1000), svc.GetComputeUnitsPerRelay(),
		"L1 must keep serving the cached CUPR while the entry is within serviceCacheL1TTL")

	// Expire L1: the next Get must treat L1 as a miss, re-read downstream, and
	// pick up the new on-chain CUPR — the exact regression this test guards.
	serviceCacheL1TTL = 0
	svc, err = sc.Get(ctx, "svc")
	require.NoError(t, err)
	require.Equal(t, uint64(1500), svc.GetComputeUnitsPerRelay(),
		"after serviceCacheL1TTL elapses, L1 must refresh and follow the on-chain CUPR")
}
