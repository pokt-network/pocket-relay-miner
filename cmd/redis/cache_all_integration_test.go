//go:build test

package redis

import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// countingAppQueryClient is the L3 (chain) stub for the real application cache.
// It returns a fixed valid application and counts GetApplication calls so the
// test can prove a post-cleanup cold read regenerates the entry from chain
// (i.e. cleanup left no stale/broken L2 behind, only a regenerable gap).
type countingAppQueryClient struct {
	getCalls atomic.Int64
}

func (q *countingAppQueryClient) GetApplication(_ context.Context, address string) (*apptypes.Application, error) {
	q.getCalls.Add(1)
	return &apptypes.Application{Address: address}, nil
}

func (q *countingAppQueryClient) InvalidateApplication(_ string) {}

// TestCleanupAll_LiveReadersUnaffected proves the `--type all` hot-safe cleanup
// (invalidateAllTypes in cache_all.go) is safe to run while live relayer-side
// caches are being read concurrently. It wires the REAL cache.SupplierCache and
// cache.ApplicationCache exactly as cmd_relayer.go does (shared Redis client,
// FailOpen supplier cache, ha:supplier prefix) against miniredis, hammers them
// from 8 reader goroutines, fires the cleanup once mid-flight, and asserts the
// observable safety contract holds under the race detector:
//
//   - healthy suppliers never read back nil/empty (any nil is the 503 window),
//   - the contaminated supplier entry is deleted,
//   - healthy supplier entries survive (their L2 is preserved, so reads keep
//     serving even when the {} clear-all empties L1),
//   - the application L2 entry is deleted by the cleanup and the application
//     remains regenerable from L3.
//
// Overlap guarantee (not best-effort): the publishClearAll seam runs after
// every deletion and before invalidateAllTypes returns. The test wraps it to
// (1) assert the deletions are already visible at that instant and (2) block
// until every reader completes at least one further full iteration — so every
// reader provably reads AFTER the deletes while the cleanup call is still
// in flight. Readers only stop after the cleanup returns. No sleeps.
//
// The {} clear-all delivery itself is asynchronous pub/sub; its L1-clearing
// semantics are covered deterministically by direct handler tests in
// cache/invalidation_clearall_test.go. This test's safety contract holds
// regardless of when (or whether) the event arrives: healthy supplier L2 is
// preserved, so even an instantly-cleared L1 re-hydrates from L2.
func TestCleanupAll_LiveReadersUnaffected(t *testing.T) {
	client, mr := newTestCacheClient(t)
	logger := logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "text", Async: false})
	ctx := context.Background()

	// --- REAL supplier cache: same wiring as cmd_relayer.go (FailOpen, ha:supplier). ---
	supplierCache := cache.NewSupplierCache(logger, client.Client, cache.SupplierCacheConfig{
		FailOpen: true,
	})
	require.NoError(t, supplierCache.Start(ctx))
	t.Cleanup(func() { _ = supplierCache.Close() })

	healthy := []string{"pokt1healthy1", "pokt1healthy2", "pokt1healthy3"}
	for _, addr := range healthy {
		require.NoError(t, supplierCache.SetSupplierState(ctx, &cache.SupplierState{
			OperatorAddress: addr,
			Staked:          true,
			Status:          cache.SupplierStatusActive,
			Services:        []string{"svc1"},
		}))
	}

	// Contaminated entry written DIRECTLY to Redis (bypasses SetSupplierState):
	// staked+active with empty services — exactly the artifact cleanup targets.
	const contamAddr = "pokt1contam"
	contamKey := "ha:supplier:" + contamAddr
	contamJSON, err := json.Marshal(cache.SupplierState{
		OperatorAddress: contamAddr,
		Staked:          true,
		Status:          cache.SupplierStatusActive,
		Services:        []string{},
	})
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, contamKey, contamJSON, 0).Err())
	require.True(t, mr.Exists(contamKey), "contaminated entry must be present before cleanup")

	// --- REAL application cache with a counting L3 stub. ---
	appStub := &countingAppQueryClient{}
	appCache := cache.NewApplicationCache(logger, client.Client, appStub)
	require.NoError(t, appCache.Start(ctx))
	t.Cleanup(func() { _ = appCache.Close() })

	const appAddr = "pokt1app"
	appL2Key := client.KB().CacheKey("application", appAddr)

	// Warm the app cache: one Get populates L2 + L1 via exactly one L3 query.
	warm, err := appCache.Get(ctx, appAddr)
	require.NoError(t, err)
	require.Equal(t, appAddr, warm.GetAddress())
	require.Equal(t, int64(1), appStub.getCalls.Load(), "warmup should hit L3 exactly once")
	require.True(t, mr.Exists(appL2Key), "warmup should populate application L2")

	// --- Concurrent readers + one cleanup, with a provable overlap. ---
	const readers = 8

	// A supplier read "fails" (would 503 in production) if it returns nil, an
	// error, or an empty services list. An app read "fails" if it errors.
	var supplierReadFailures atomic.Int64
	var appReadFailures atomic.Int64

	var stop atomic.Bool
	iterCounts := make([]atomic.Int64, readers)

	var ready sync.WaitGroup // released once every reader has done one iteration
	var done sync.WaitGroup  // released when every reader has finished
	ready.Add(readers)
	done.Add(readers)

	for i := 0; i < readers; i++ {
		idx := i
		supplierReader := i%2 == 0
		go func() {
			defer done.Done()
			for j := 0; !stop.Load(); j++ {
				if supplierReader {
					addr := healthy[j%len(healthy)]
					state, gErr := supplierCache.GetSupplierState(ctx, addr)
					if gErr != nil || state == nil || len(state.Services) == 0 {
						supplierReadFailures.Add(1)
					}
				} else {
					if _, gErr := appCache.Get(ctx, appAddr); gErr != nil {
						appReadFailures.Add(1)
					}
				}
				iterCounts[idx].Add(1)
				if j == 0 {
					ready.Done()
				}
			}
		}()
	}

	// Wrap the publish seam (runs after ALL deletions, before the cleanup call
	// returns) to pin the overlap and the post-delete state deterministically.
	origPublish := publishClearAll
	t.Cleanup(func() { publishClearAll = origPublish })

	var contamGoneAtPublish, appL2GoneAtPublish, healthyPresentAtPublish bool
	publishClearAll = func(pctx context.Context, c *DebugRedisClient, s cleanupScope) int {
		// (b')(c')(d') Deletions must already be visible here — before any
		// subscriber is told to drop L1 (invariant 4, asserted mid-flight).
		contamGoneAtPublish = !mr.Exists(contamKey)
		appL2GoneAtPublish = !mr.Exists(appL2Key)
		healthyPresentAtPublish = true
		for _, addr := range healthy {
			if !mr.Exists("ha:supplier:" + addr) {
				healthyPresentAtPublish = false
			}
		}

		// Force every reader through at least one full iteration AFTER the
		// deletes while the cleanup is still in flight: the overlap can never
		// be vacuous. Bounded spin, no sleeps.
		deadline := time.Now().Add(10 * time.Second)
		snapshot := make([]int64, readers)
		for i := range iterCounts {
			snapshot[i] = iterCounts[i].Load()
		}
		for i := range iterCounts {
			for iterCounts[i].Load() <= snapshot[i] {
				if time.Now().After(deadline) {
					t.Error("reader stalled: no iteration completed after deletions")
					break
				}
				runtime.Gosched()
			}
		}

		return origPublish(pctx, c, s)
	}

	// Fire the cleanup once while all readers are actively looping.
	ready.Wait()
	require.NoError(t, invalidateAllTypes(ctx, client, false /*dryRun*/, true /*yes*/))
	stop.Store(true)
	done.Wait()

	// (a) No 503 window: healthy suppliers never read back nil/empty, app reads
	//     never error, throughout the concurrent cleanup (including the forced
	//     post-delete iterations).
	assert.Equal(t, int64(0), supplierReadFailures.Load(),
		"healthy suppliers must never read back nil/empty while cleanup runs")
	assert.Equal(t, int64(0), appReadFailures.Load(),
		"application reads must never error while cleanup runs")

	// (b)(c)(d) State at publish time (mid-cleanup, post-delete): contaminated
	// gone, app L2 gone, healthy suppliers intact. Checked at publish time
	// because post-cleanup reader traffic legitimately repopulates app L2 from
	// L3 — asserting non-existence after the readers stop would race.
	assert.True(t, contamGoneAtPublish, "contaminated supplier entry must be deleted before the clear-all publish")
	assert.True(t, appL2GoneAtPublish, "application L2 key must be deleted before the clear-all publish")
	assert.True(t, healthyPresentAtPublish, "healthy supplier keys must survive the delete phase")

	// (c) Healthy supplier keys still present after everything settles.
	for _, addr := range healthy {
		assert.True(t, mr.Exists("ha:supplier:"+addr), "healthy supplier key must survive cleanup: "+addr)
	}

	// (e) Healthy supplier reads still serve after cleanup, with full state.
	for _, addr := range healthy {
		state, gErr := supplierCache.GetSupplierState(ctx, addr)
		require.NoError(t, gErr)
		require.NotNil(t, state, "healthy supplier must still resolve after cleanup: "+addr)
		assert.Equal(t, []string{"svc1"}, state.Services, "preserved supplier must keep its services: "+addr)
	}

	// (f) Application is regenerable: evict L1 (public API), then a cold read
	// must resolve via L3/L2. Reader goroutines may already have regenerated
	// L2 after the cleanup — that IS the regenerable contract — so assert the
	// read succeeds and total L3 traffic stayed sane (warmup + at least the
	// post-cleanup refills), not an exact count.
	require.NoError(t, appCache.Invalidate(ctx, appAddr))
	regen, err := appCache.Get(ctx, appAddr)
	require.NoError(t, err)
	require.Equal(t, appAddr, regen.GetAddress())
	assert.GreaterOrEqual(t, appStub.getCalls.Load(), int64(2),
		"post-cleanup application reads must be served by regenerating from L3")
	assert.True(t, mr.Exists(appL2Key), "regeneration should repopulate application L2")
}
