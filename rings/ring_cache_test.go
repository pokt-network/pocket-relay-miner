//go:build test

package rings

import (
	"context"
	"sync"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	"github.com/stretchr/testify/require"
)

// countingAccountQuerier counts GetPubKeyFromAddress calls so a test can tell a
// cache hit (no recompute) from a miss (recompute re-queries every ring member).
type countingAccountQuerier struct {
	mockAccountQuerier
	mu   sync.Mutex
	hits int
}

func (c *countingAccountQuerier) GetPubKeyFromAddress(ctx context.Context, address string) (cryptotypes.PubKey, error) {
	c.mu.Lock()
	c.hits++
	c.mu.Unlock()
	return c.mockAccountQuerier.GetPubKeyFromAddress(ctx, address)
}

func (c *countingAccountQuerier) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

// newTestRingClientForCache builds a ringClient with one app + one gateway and a
// counting account querier.
func newTestRingClientForCache(t *testing.T) (*ringClient, string) {
	t.Helper()

	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	gwPrivKey := secp256k1.GenPrivKey()
	gwPubKey := gwPrivKey.PubKey()
	gwAddress := createTestAddress(gwPubKey)

	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{gwAddress},
	}
	pubKeys := map[string]cryptotypes.PubKey{
		appAddress: appPubKey,
		gwAddress:  gwPubKey,
	}

	accQuerier := &countingAccountQuerier{mockAccountQuerier: mockAccountQuerier{pubKeys: pubKeys}}
	rc := NewRingClient(newNopLogger(), &mockApplicationQuerier{app: app}, accQuerier, &mockSharedQuerier{}).(*ringClient)
	return rc, appAddress
}

// rangeCount counts live entries in the ring points cache.
func rangeCount(rc *ringClient) int {
	n := 0
	rc.ringPointsCache.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestRingPointsCache_HitWithinTTL proves a second lookup for the same
// (app, sessionEndHeight) within the TTL is served from cache (no recompute).
func TestRingPointsCache_HitWithinTTL(t *testing.T) {
	ctx := context.Background()
	rc, appAddress := newTestRingClientForCache(t)
	acc := rc.accountQuerier.(*countingAccountQuerier)

	_, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, 100)
	require.NoError(t, err)
	firstHits := acc.callCount()
	require.Greater(t, firstHits, 0, "first lookup must query the account client (cache miss)")

	_, err = rc.getRingPointsForAddressAtHeight(ctx, appAddress, 100)
	require.NoError(t, err)
	require.Equal(t, firstHits, acc.callCount(),
		"second lookup within TTL must hit cache and not re-query the account client")
}

// TestRingPointsCache_RecomputesAfterTTL proves an entry past its TTL floor is
// treated as a miss and recomputed (re-queries the account client). No sleep:
// ringPointsCacheTTL is shrunk to 0 so any entry is immediately expired.
func TestRingPointsCache_RecomputesAfterTTL(t *testing.T) {
	ctx := context.Background()
	rc, appAddress := newTestRingClientForCache(t)
	acc := rc.accountQuerier.(*countingAccountQuerier)

	orig := ringPointsCacheTTL
	ringPointsCacheTTL = 0
	defer func() { ringPointsCacheTTL = orig }()

	_, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, 100)
	require.NoError(t, err)
	firstHits := acc.callCount()

	_, err = rc.getRingPointsForAddressAtHeight(ctx, appAddress, 100)
	require.NoError(t, err)
	require.Equal(t, firstHits*2, acc.callCount(),
		"with TTL=0 every lookup is expired and must recompute (re-query the account client)")
}

// TestRingPointsCache_EvictsOldHeights proves the cache is bounded: storing an
// entry far above older ones prunes entries more than ringPointsCacheKeepHeights
// below the newest session end height.
func TestRingPointsCache_EvictsOldHeights(t *testing.T) {
	ctx := context.Background()
	rc, appAddress := newTestRingClientForCache(t)

	// Seed an old entry.
	oldHeight := int64(100)
	_, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, oldHeight)
	require.NoError(t, err)
	require.Equal(t, 1, rangeCount(rc), "one entry after first store")

	// Store an entry far above the window — old entry must be evicted.
	newHeight := oldHeight + ringPointsCacheKeepHeights + 50
	_, err = rc.getRingPointsForAddressAtHeight(ctx, appAddress, newHeight)
	require.NoError(t, err)

	require.Equal(t, 1, rangeCount(rc), "old out-of-window entry must be pruned on store")

	// The surviving entry must be the new one.
	_, ok := rc.ringPointsCache.Load(ringPointsCacheKey{appAddress: appAddress, sessionEndHeight: newHeight})
	require.True(t, ok, "newest entry must remain")
	_, ok = rc.ringPointsCache.Load(ringPointsCacheKey{appAddress: appAddress, sessionEndHeight: oldHeight})
	require.False(t, ok, "old entry must be gone")
}

// TestRingPointsCache_ConcurrentReadWrite exercises the ring points cache under
// simultaneous reads and writes across many distinct (app, sessionEndHeight)
// keys, with the race detector. The cache is on the 1000+ RPS hot path (Load on
// every relay verify, Store + prune on every miss), so a data race or a panic
// from the eviction Range/Delete racing a Load/Store would be a production
// outage. Asserts no panic/race and that the cache stays bounded under churn.
func TestRingPointsCache_ConcurrentReadWrite(t *testing.T) {
	ctx := context.Background()
	rc, appAddress := newTestRingClientForCache(t)

	const (
		workers     = 16
		baseHeight  = int64(1000)
		windowSpan  = int64(800) // > keepHeights(600) so eviction is exercised
		opsPerRound = 4
	)
	// All workers walk the SAME advancing height window (the realistic hot path:
	// many goroutines verifying relays for the same handful of recent session
	// heights). Overlapping keys maximize Load/Store/evict contention on the same
	// sync.Map entries while the window advancing past keepHeights drives eviction.
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for h := baseHeight; h < baseHeight+windowSpan; h++ {
				for op := 0; op < opsPerRound; op++ {
					if _, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, h); err != nil {
						t.Errorf("unexpected error at height %d: %v", h, err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	// The concurrent storm proved no race/panic. The cache can transiently hold up
	// to `windowSpan` distinct heights (Store overwrites the same key, so it never
	// exceeds the heights touched — i.e. no per-op leak), but under concurrency a
	// single prune pass may not fully evict. Now advance the high-water
	// SEQUENTIALLY past the window to force deterministic prune passes, then assert
	// it settles back to the keepHeights window. (Precise single-threaded eviction
	// is also covered by TestRingPointsCache_EvictsOldHeights.)
	require.LessOrEqual(t, rangeCount(rc), int(windowSpan)+workers,
		"cache must not exceed the distinct heights touched (no per-op leak)")
	for h := baseHeight + windowSpan; h <= baseHeight+windowSpan+int64(ringPointsCacheKeepHeights)+50; h++ {
		_, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, h)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, rangeCount(rc), int(ringPointsCacheKeepHeights)+8,
		"after the high-water advances past the window, eviction must settle to keepHeights")
}

// TestRingPointsCache_KeepsInWindowHeights proves entries within the height
// window are retained (not over-evicted).
func TestRingPointsCache_KeepsInWindowHeights(t *testing.T) {
	ctx := context.Background()
	rc, appAddress := newTestRingClientForCache(t)

	base := int64(10_000)
	// Three heights all within ringPointsCacheKeepHeights of each other.
	heights := []int64{base, base + 100, base + 200}
	for _, h := range heights {
		_, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, h)
		require.NoError(t, err)
	}

	require.Equal(t, len(heights), rangeCount(rc),
		"all in-window entries must be retained")
	for _, h := range heights {
		_, ok := rc.ringPointsCache.Load(ringPointsCacheKey{appAddress: appAddress, sessionEndHeight: h})
		require.True(t, ok, "in-window height %d must remain", h)
	}
}
