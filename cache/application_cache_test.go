//go:build test

package cache

import (
	"context"
	"testing"
	"time"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// frozenApplicationQueryClient models the query layer's in-process cache: once it
// serves a value it keeps serving it (frozen) until InvalidateApplication is
// called. This mirrors frozenServiceQueryClient and reproduces the exact behavior
// that would freeze a stale application (stake/delegations) for the process
// lifetime — a force/lazy refresh that does not invalidate the query layer
// re-reads the stale value forever.
type frozenApplicationQueryClient struct {
	// chainDelegatees is the current on-chain delegatee gateway set.
	chainDelegatees []string
	// served is what GetApplication returns until invalidated.
	served          *[]string
	invalidateCalls int
}

func (f *frozenApplicationQueryClient) GetApplication(_ context.Context, address string) (*apptypes.Application, error) {
	if f.served == nil {
		v := append([]string(nil), f.chainDelegatees...)
		f.served = &v
	}
	return &apptypes.Application{
		Address:                   address,
		DelegateeGatewayAddresses: append([]string(nil), *f.served...),
	}, nil
}

func (f *frozenApplicationQueryClient) InvalidateApplication(_ string) {
	f.invalidateCalls++
	f.served = nil // next GetApplication re-reads the (possibly changed) chain value
}

// TestApplicationCache_L1RefreshesAfterTTL is the regression test for the L1
// no-TTL freeze. The cache-pkg applicationCache L1 (in-process xsync map) had NO
// TTL, so once the relayer cached an application its stake/delegation set was
// frozen for the process lifetime: when pub/sub invalidation does not fire (the
// leader's known-applications set is empty), Get kept serving the stale value and
// never followed the on-chain state. The fix ages L1 entries out after
// applicationCacheL1TTL so Get falls through to L2/L3 and follows the on-chain
// application WITHOUT a pod restart. This test drives that change against the REAL
// application cache with miniredis.
func TestApplicationCache_L1RefreshesAfterTTL(t *testing.T) {
	client := newTestRedis(t)
	fq := &frozenApplicationQueryClient{chainDelegatees: []string{"gw-a"}}
	ac := NewApplicationCache(logging.NewLoggerFromConfig(logging.DefaultConfig()), client, fq)

	ctx := context.Background()
	require.NoError(t, ac.Start(ctx))
	t.Cleanup(func() { _ = ac.Close() })

	// Use a large L1 TTL while we prove caching; restore the package default after.
	origTTL := applicationCacheL1TTL
	applicationCacheL1TTL = time.Hour
	t.Cleanup(func() { applicationCacheL1TTL = origTTL })

	const addr = "pokt1app"

	// Initial lazy load caches delegatees=[gw-a] in L1 (and L2).
	app, err := ac.Get(ctx, addr)
	require.NoError(t, err)
	require.Equal(t, []string{"gw-a"}, app.GetDelegateeGatewayAddresses())

	// On-chain delegation set changes mid-flight. Reproduce the downstream a
	// stale-L1 re-read hits in production steady state: L2 emptied and the L3 query
	// client past its own TTL (the frozen stub re-reads the chain value once
	// InvalidateApplication fires).
	fq.chainDelegatees = []string{"gw-a", "gw-b"}
	fq.InvalidateApplication(addr)
	require.NoError(t, client.Del(ctx, client.KB().CacheKey(applicationCacheType, addr)).Err())

	// Within the (huge) L1 TTL: Get must still serve the cached delegation set,
	// even though the downstream value already changed. Proves L1 actually caches.
	app, err = ac.Get(ctx, addr)
	require.NoError(t, err)
	require.Equal(t, []string{"gw-a"}, app.GetDelegateeGatewayAddresses(),
		"L1 must keep serving the cached application while the entry is within applicationCacheL1TTL")

	// Expire L1: the next Get must treat L1 as a miss, re-read downstream, and pick
	// up the new on-chain delegation set — the exact regression this test guards.
	applicationCacheL1TTL = 0
	app, err = ac.Get(ctx, addr)
	require.NoError(t, err)
	require.Equal(t, []string{"gw-a", "gw-b"}, app.GetDelegateeGatewayAddresses(),
		"after applicationCacheL1TTL elapses, L1 must refresh and follow the on-chain application")
}
