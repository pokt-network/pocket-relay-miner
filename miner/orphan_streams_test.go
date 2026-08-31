//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// scanWith is the plain miner SCAN, bound to a client.
func scanWith(client *redisutil.Client) ScanFunc {
	return func(ctx context.Context, pattern string) ([]string, error) {
		return ScanKeys(ctx, client, pattern)
	}
}

// seedStream creates a relay stream for addr with one entry.
func seedStream(t *testing.T, client *redisutil.Client, addr string) {
	t.Helper()
	require.NoError(t, client.XAdd(context.Background(), &redis.XAddArgs{
		Stream: client.KB().StreamKey(addr),
		Values: map[string]any{"data": []byte("x")},
	}).Err())
}

// TestOrphanStreamAddressesNeedsBothSourcesOfKnown is the test the whole feature
// rests on, because the cost of getting it wrong is asymmetric: an orphan missed
// is a stream that lingers, while a live supplier misreported as an orphan is an
// operator being invited to delete relays nobody has been paid for.
//
// Both sources are checked in the same test on purpose. Two separate tests, each
// seeding one source, would BOTH pass against an implementation that read only
// the source it happened to seed.
func TestOrphanStreamAddressesNeedsBothSourcesOfKnown(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)

	const (
		fromIndex = "pokt1known_via_registry_index"
		fromCache = "pokt1known_via_supplier_cache"
		orphan    = "pokt1nobody_claims_me"
	)

	// Known only through the registry index: a supplier with a live pipeline.
	require.NoError(t, client.SAdd(ctx, client.KB().SuppliersRegistryIndexKey(), fromIndex).Err())
	// Known only through the supplier cache: a supplier this fleet tore down. Its
	// registry entry is gone, but the cache entry is kept deliberately -- it is
	// the chain's answer, and the relayer needs it to keep refusing relays.
	require.NoError(t, client.Set(ctx, client.KB().SupplierStateKey(fromCache), `{"status":"unstaking"}`, 0).Err())

	for _, addr := range []string{fromIndex, fromCache, orphan} {
		seedStream(t, client, addr)
	}

	orphans, err := OrphanStreamAddresses(ctx, client, scanWith(client))
	require.NoError(t, err)

	require.Equal(t, []string{orphan}, orphans,
		"exactly one stream is orphaned. %s is known through the registry index and %s only "+
			"through the supplier cache; dropping either source would report a live supplier as "+
			"an orphan and invite an operator to delete its unpaid relays", fromIndex, fromCache)
}

// TestOrphanStreamAddressesIgnoresForeignKeys guards the prefix arithmetic.
//
// The classification strips the stream prefix off each key, and a key that does
// not carry that prefix must be skipped rather than yield a bogus address.
func TestOrphanStreamAddressesIgnoresForeignKeys(t *testing.T) {
	ctx := context.Background()
	client, prefix := newTestRedis(t)

	// A key inside the namespace that is NOT a relay stream.
	require.NoError(t, client.Set(ctx, prefix+":something:else", "v", 0).Err())

	orphans, err := OrphanStreamAddresses(ctx, client, scanWith(client))
	require.NoError(t, err)
	require.Empty(t, orphans, "only keys under the relay-stream prefix may be classified")
}

// TestOrphanStreamAddressesOnACleanDeployment covers the ordinary case, where
// every stream belongs to a supplier that is still around.
func TestOrphanStreamAddressesOnACleanDeployment(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)

	for _, addr := range []string{"pokt1a", "pokt1b", "pokt1c"} {
		require.NoError(t, client.SAdd(ctx, client.KB().SuppliersRegistryIndexKey(), addr).Err())
		seedStream(t, client, addr)
	}

	orphans, err := OrphanStreamAddresses(ctx, client, scanWith(client))
	require.NoError(t, err)
	require.Empty(t, orphans)
}

// TestOrphanStreamAddressesOnAnEmptyDeployment covers cold start: no streams and
// no registry index key at all, which Redis reports as a missing key rather than
// an empty set.
func TestOrphanStreamAddressesOnAnEmptyDeployment(t *testing.T) {
	client, _ := newTestRedis(t)

	orphans, err := OrphanStreamAddresses(context.Background(), client, scanWith(client))
	require.NoError(t, err, "a deployment with no streams yet is not an error")
	require.Empty(t, orphans)
}

// TestKnownSupplierAddressesReadsAMissingIndexAsEmpty states the cold-start
// behaviour of the union directly, so it cannot regress into an error that would
// make the sweep give up and leave the gauge stale.
func TestKnownSupplierAddressesReadsAMissingIndexAsEmpty(t *testing.T) {
	client, _ := newTestRedis(t)

	known, err := KnownSupplierAddresses(context.Background(), client, scanWith(client))
	require.NoError(t, err)
	require.Empty(t, known)
}

// TestOrphanStreamAddressesSeesADecommissionedSupplierOnceItsCacheEntryExpires
// is HIGH-2's other half (review 2026-08-20): with HIGH-1 fixed, a supplier
// cache entry no longer written (its key removed from this miner's keyring
// mid-teardown) now carries a bounded TTL instead of none, so it eventually
// falls out of KnownSupplierAddresses on its own, and the stream nobody
// consumes anymore becomes visible to the orphan detector -- the one scenario
// the feature exists to catch. (Contrast with TestOrphanStreamAddressesNeeds
// BothSourcesOfKnown above, whose fromCache entry is a TEST FIXTURE written
// with Set(..., 0) directly, on purpose, to exercise the union regardless of
// TTL -- not a claim that production entries are still permanent.)
//
// ageKeyTo(..., 0), not a sleep: Rule #1 (CLAUDE.md) forbids time.Sleep for
// synchronization, and internal/conventions' sleep allowlist would reject a
// new one. The remaining TTL is the observable this test is about, and
// setting it to 0 collapses "wait for the TTL to elapse" into "the key is
// gone now" with no timing window to be flaky on.
func TestOrphanStreamAddressesSeesADecommissionedSupplierOnceItsCacheEntryExpires(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)

	const decommissioned = "pokt1decommissioned_stale_cache"
	seedStream(t, client, decommissioned)

	require.NoError(t, client.Set(ctx, client.KB().SupplierStateKey(decommissioned),
		`{"status":"unstaking","staked":true}`, time.Minute).Err())

	orphans, err := OrphanStreamAddresses(ctx, client, scanWith(client))
	require.NoError(t, err)
	require.Empty(t, orphans,
		"while the cache entry is still within its TTL, the supplier stays known -- "+
			"an active supplier mid-unbonding must not be misreported as orphaned")

	ageKeyTo(t, client, client.KB().SupplierStateKey(decommissioned), 0)

	orphans, err = OrphanStreamAddresses(ctx, client, scanWith(client))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{decommissioned}, orphans,
		"once the cache entry's TTL elapses with nothing left to refresh it, the "+
			"detector must finally see the abandoned stream -- before HIGH-1's TTL "+
			"fix, this entry would never expire and this stream would never be found")
}
