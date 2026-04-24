//go:build test

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// newTestSupplierCache wires a SupplierCache against a fresh miniredis
// instance. Returns the cache, the redis client, and the miniredis handle
// for direct manipulation. All three are cleaned up by t.Cleanup.
func newTestSupplierCache(t *testing.T) (*SupplierCache, *redisutil.Client, *miniredis.Miniredis) {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	cache := NewSupplierCache(logger, client, SupplierCacheConfig{
		KeyPrefix: DefaultSupplierKeyPrefix,
		FailOpen:  false,
	})

	return cache, client, mr
}

// writeSupplierToRedis marshals a SupplierState directly into miniredis so
// tests can simulate entries written by another (possibly buggy) producer.
func writeSupplierToRedis(t *testing.T, mr *miniredis.Miniredis, state *SupplierState) {
	t.Helper()
	data, err := json.Marshal(state)
	require.NoError(t, err)
	mr.Set(fmt.Sprintf("%s:%s", DefaultSupplierKeyPrefix, state.OperatorAddress), string(data))
}

func TestIsContaminated(t *testing.T) {
	cases := []struct {
		name  string
		state SupplierState
		want  bool
	}{
		{
			name:  "contaminated: staked+active+empty services",
			state: SupplierState{Staked: true, Status: SupplierStatusActive, Services: nil},
			want:  true,
		},
		{
			name:  "clean: staked+active with services",
			state: SupplierState{Staked: true, Status: SupplierStatusActive, Services: []string{"svc1"}},
			want:  false,
		},
		{
			name:  "legitimate: unstaked with empty services",
			state: SupplierState{Staked: false, Status: SupplierStatusNotStaked, Services: nil},
			want:  false,
		},
		{
			name:  "legitimate: unstaking preserves services",
			state: SupplierState{Staked: true, Status: SupplierStatusUnstaking, Services: []string{"svc1"}},
			want:  false,
		},
		{
			name:  "not contaminated: staked+unstaking+empty services (not matching active)",
			state: SupplierState{Staked: true, Status: SupplierStatusUnstaking, Services: nil},
			want:  false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.state.isContaminated())
		})
	}
}

func TestGetSupplierState_ContaminatedInL1_EvictsAndReportsMiss(t *testing.T) {
	cache, _, _ := newTestSupplierCache(t)
	ctx := context.Background()

	const addr = "pokt1contaminatedL1"
	contaminated := &SupplierState{
		OperatorAddress: addr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{},
	}

	// Seed L1 directly (simulates a pre-fix binary's L1 population).
	cache.localCache.Store(addr, contaminated)

	before := testutil.ToFloat64(supplierContaminated.WithLabelValues("l1_read"))

	state, err := cache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.Nil(t, state, "contaminated L1 hit must be reported as miss")

	after := testutil.ToFloat64(supplierContaminated.WithLabelValues("l1_read"))
	require.Equal(t, before+1, after, "l1_read counter must be incremented")

	// L1 entry must be evicted.
	_, ok := cache.localCache.Load(addr)
	require.False(t, ok, "contaminated L1 entry must be evicted")
}

func TestGetSupplierState_ContaminatedInL2_TreatedAsMissNoL1Populate(t *testing.T) {
	cache, _, mr := newTestSupplierCache(t)
	ctx := context.Background()

	const addr = "pokt1contaminatedL2"
	writeSupplierToRedis(t, mr, &SupplierState{
		OperatorAddress: addr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{},
	})

	before := testutil.ToFloat64(supplierContaminated.WithLabelValues("l2_read"))

	state, err := cache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.Nil(t, state, "contaminated L2 read must be reported as miss")

	after := testutil.ToFloat64(supplierContaminated.WithLabelValues("l2_read"))
	require.Equal(t, before+1, after, "l2_read counter must be incremented")

	// L1 must NOT be populated with the contaminated entry.
	_, ok := cache.localCache.Load(addr)
	require.False(t, ok, "contaminated L2 read must not populate L1")
}

func TestGetSupplierState_CleanEntry_ServedNormally(t *testing.T) {
	cache, _, mr := newTestSupplierCache(t)
	ctx := context.Background()

	const addr = "pokt1clean"
	writeSupplierToRedis(t, mr, &SupplierState{
		OperatorAddress: addr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svc1", "svc2"},
	})

	state, err := cache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, addr, state.OperatorAddress)
	require.True(t, state.Staked)
	require.Equal(t, SupplierStatusActive, state.Status)
	require.Equal(t, []string{"svc1", "svc2"}, state.Services)

	// L1 populated for the next call.
	cached, ok := cache.localCache.Load(addr)
	require.True(t, ok, "clean L2 read must populate L1")
	require.Equal(t, []string{"svc1", "svc2"}, cached.Services)
}

func TestGetSupplierState_LegitimateUnstaked_ServedNormally(t *testing.T) {
	cache, _, mr := newTestSupplierCache(t)
	ctx := context.Background()

	const addr = "pokt1unstaked"
	writeSupplierToRedis(t, mr, &SupplierState{
		OperatorAddress: addr,
		Status:          SupplierStatusNotStaked,
		Staked:          false,
		Services:        nil,
	})

	before := testutil.ToFloat64(supplierContaminated.WithLabelValues("l2_read"))

	state, err := cache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.NotNil(t, state, "unstaked entry is legitimate and must be returned")
	require.False(t, state.Staked)
	require.Equal(t, SupplierStatusNotStaked, state.Status)

	after := testutil.ToFloat64(supplierContaminated.WithLabelValues("l2_read"))
	require.Equal(t, before, after, "legitimate unstaked entry must not increment contamination counter")
}

func TestWarmupFromRedis_SkipsContaminatedKeepsClean(t *testing.T) {
	cache, _, mr := newTestSupplierCache(t)
	ctx := context.Background()

	const cleanAddr = "pokt1warmup_clean"
	const dirtyAddr = "pokt1warmup_dirty"
	const unstakedAddr = "pokt1warmup_unstaked"

	writeSupplierToRedis(t, mr, &SupplierState{
		OperatorAddress: cleanAddr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svc1"},
	})
	writeSupplierToRedis(t, mr, &SupplierState{
		OperatorAddress: dirtyAddr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{},
	})
	writeSupplierToRedis(t, mr, &SupplierState{
		OperatorAddress: unstakedAddr,
		Status:          SupplierStatusNotStaked,
		Staked:          false,
		Services:        nil,
	})

	before := testutil.ToFloat64(supplierContaminated.WithLabelValues("warmup_skip"))

	require.NoError(t, cache.WarmupFromRedis(ctx, nil))

	after := testutil.ToFloat64(supplierContaminated.WithLabelValues("warmup_skip"))
	require.Equal(t, before+1, after, "warmup_skip counter must be incremented once")

	_, okClean := cache.localCache.Load(cleanAddr)
	require.True(t, okClean, "clean entry must be loaded into L1")

	_, okDirty := cache.localCache.Load(dirtyAddr)
	require.False(t, okDirty, "contaminated entry must be skipped during warmup")

	_, okUnstaked := cache.localCache.Load(unstakedAddr)
	require.True(t, okUnstaked, "legitimate unstaked entry must be loaded into L1")
}
