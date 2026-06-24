//go:build test

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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
	require.NoError(t, mr.Set(fmt.Sprintf("%s:%s", DefaultSupplierKeyPrefix, state.OperatorAddress), string(data)))
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
	cache.localCache.Store(addr, supplierCacheL1Entry{supplier: contaminated, cachedAt: time.Now()})

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
	require.Equal(t, []string{"svc1", "svc2"}, cached.supplier.Services)
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

// TestIsActive covers the IsActive semantics: active→true, unstaking-with-services→true
// (the key case — an unstaking supplier still serves relays until its service configs
// deactivate), not_staked→false.
func TestIsActive(t *testing.T) {
	cases := []struct {
		name  string
		state SupplierState
		want  bool
	}{
		{
			name: "active supplier is active",
			state: SupplierState{
				Staked:                  true,
				Status:                  SupplierStatusActive,
				Services:                []string{"eth-mainnet"},
				UnstakeSessionEndHeight: 0,
			},
			want: true,
		},
		{
			name: "unstaking supplier with services is active (key case)",
			state: SupplierState{
				Staked:                  true,
				Status:                  SupplierStatusUnstaking,
				Services:                []string{"eth-mainnet"},
				UnstakeSessionEndHeight: 150,
			},
			want: true,
		},
		{
			name: "unstaking supplier with empty services is still active (IsActiveForService gates per-service)",
			state: SupplierState{
				Staked:                  true,
				Status:                  SupplierStatusUnstaking,
				Services:                []string{},
				UnstakeSessionEndHeight: 150,
			},
			want: true,
		},
		{
			name: "not_staked is not active",
			state: SupplierState{
				Staked:   false,
				Status:   SupplierStatusNotStaked,
				Services: nil,
			},
			want: false,
		},
		{
			name: "staked=false with active status is not active (Staked gates first)",
			state: SupplierState{
				Staked: false,
				Status: SupplierStatusActive,
			},
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.state.IsActive()
			require.Equal(t, tc.want, got,
				"IsActive() mismatch for state Status=%q Staked=%v UnstakeSessionEndHeight=%d",
				tc.state.Status, tc.state.Staked, tc.state.UnstakeSessionEndHeight)
		})
	}
}

// TestIsActiveForService verifies that per-service activity is gated by the
// Services list, not just the status field. An unstaking supplier with the
// service still in its list is active for that service; one whose service list
// has been cleared by poktroll's deactivation boundary is not.
func TestIsActiveForService(t *testing.T) {
	cases := []struct {
		name      string
		state     SupplierState
		serviceID string
		want      bool
	}{
		{
			name: "unstaking + service in Services → active for service",
			state: SupplierState{
				Staked:                  true,
				Status:                  SupplierStatusUnstaking,
				Services:                []string{"eth-mainnet", "polygon"},
				UnstakeSessionEndHeight: 200,
			},
			serviceID: "eth-mainnet",
			want:      true,
		},
		{
			name: "unstaking + service NOT in Services → not active for service",
			state: SupplierState{
				Staked:                  true,
				Status:                  SupplierStatusUnstaking,
				Services:                []string{"polygon"},
				UnstakeSessionEndHeight: 200,
			},
			serviceID: "eth-mainnet",
			want:      false,
		},
		{
			name: "unstaking + empty Services → not active for any service",
			state: SupplierState{
				Staked:                  true,
				Status:                  SupplierStatusUnstaking,
				Services:                []string{},
				UnstakeSessionEndHeight: 200,
			},
			serviceID: "eth-mainnet",
			want:      false,
		},
		{
			name: "active + service in Services → active for service",
			state: SupplierState{
				Staked:   true,
				Status:   SupplierStatusActive,
				Services: []string{"eth-mainnet"},
			},
			serviceID: "eth-mainnet",
			want:      true,
		},
		{
			name: "not_staked → not active for any service",
			state: SupplierState{
				Staked:   false,
				Status:   SupplierStatusNotStaked,
				Services: []string{"eth-mainnet"},
			},
			serviceID: "eth-mainnet",
			want:      false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.state.IsActiveForService(tc.serviceID)
			require.Equal(t, tc.want, got,
				"IsActiveForService(%q) mismatch for Status=%q Staked=%v Services=%v",
				tc.serviceID, tc.state.Status, tc.state.Staked, tc.state.Services)
		})
	}
}

// TestWriteAndReadSupplierStatusUnstaking verifies that a SupplierState with
// UnstakeSessionEndHeight>0 round-trips through the cache with Status=unstaking
// and the height field preserved. This is the serialisation contract that
// writeSupplierStatusToCache (miner) and GetSupplierState (relayer) depend on.
func TestWriteAndReadSupplierStatusUnstaking(t *testing.T) {
	sc, _, _ := newTestSupplierCache(t)
	ctx := context.Background()

	const addr = "pokt1unstaking_roundtrip"
	const wantHeight = uint64(300)

	state := &SupplierState{
		OperatorAddress:         addr,
		Status:                  SupplierStatusUnstaking,
		Staked:                  true,
		Services:                []string{"eth-mainnet"},
		UnstakeSessionEndHeight: wantHeight,
	}
	require.NoError(t, sc.SetSupplierState(ctx, state))

	// Expire L1 so the read exercises L2 serialisation.
	sc.localCache.Delete(addr)

	got, err := sc.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.NotNil(t, got, "unstaking state must be returned (not treated as miss)")

	require.Equal(t, SupplierStatusUnstaking, got.Status, "Status must round-trip as unstaking")
	require.True(t, got.Staked, "Staked must be true for an unstaking supplier")
	require.Equal(t, wantHeight, got.UnstakeSessionEndHeight, "UnstakeSessionEndHeight must round-trip correctly")
	require.Equal(t, []string{"eth-mainnet"}, got.Services, "Services must round-trip correctly")
	require.True(t, got.IsActive(), "unstaking supplier must be IsActive()==true")
	require.True(t, got.IsActiveForService("eth-mainnet"), "unstaking supplier must be active for its service")
}

// TestIsActive_IsContaminated_Boundary verifies that the contamination check
// (staked+active+empty services) is unaffected by the new IsActive semantics.
// An unstaking+empty-services entry is NOT contamination — it is a legitimate
// in-flight deactivation — so isContaminated must return false for it.
func TestIsActive_IsContaminated_Boundary(t *testing.T) {
	unstakingEmptyServices := SupplierState{
		Staked:                  true,
		Status:                  SupplierStatusUnstaking,
		Services:                []string{},
		UnstakeSessionEndHeight: 150,
	}
	require.False(t, unstakingEmptyServices.isContaminated(),
		"unstaking+empty-services is NOT contamination (isContaminated checks active, not unstaking)")
	// IsActive is still true — per-service gate is IsActiveForService.
	require.True(t, unstakingEmptyServices.IsActive(),
		"unstaking supplier is IsActive even with no services (IsActiveForService gates per-service relay acceptance)")
}

// TestSupplierCache_L1RefreshesAfterTTL is the regression test for the supplier
// cache-TTL gap. The SupplierCache L1 (in-process xsync map) had NO TTL, so once
// a relayer cached a supplier its stake status and service list were frozen for
// the process lifetime: pub/sub invalidation fires on the miner's Set/Delete but
// a relayer can miss it (restart, dropped event), stranding a stale stake/services
// view forever. The fix ages L1 entries out after supplierCacheL1TTL so
// GetSupplierState falls through to L2 (Redis) and follows the on-chain
// stake/services change WITHOUT a pod restart. This test drives that change
// against the REAL supplier cache with miniredis.
//
// NOTE: unlike the service cache, SupplierCache is L1+L2 only — it has no L3
// query client (and thus no frozen-query-client stub to reuse). The downstream
// change is therefore driven at L2 (Redis), the cache's only authoritative
// source below L1, via the existing writeSupplierToRedis helper.
func TestSupplierCache_L1RefreshesAfterTTL(t *testing.T) {
	cache, _, mr := newTestSupplierCache(t)
	ctx := context.Background()

	// Use a large L1 TTL while we prove caching; restore the package default after.
	origTTL := supplierCacheL1TTL
	supplierCacheL1TTL = time.Hour
	t.Cleanup(func() { supplierCacheL1TTL = origTTL })

	const addr = "pokt1ttl"

	// Seed L2 with the old service set and load it into L1 via a real Get.
	writeSupplierToRedis(t, mr, &SupplierState{
		OperatorAddress: addr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svcA"},
	})
	state, err := cache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, []string{"svcA"}, state.Services)

	// On-chain services change mid-session: the miner rewrites L2 with a new
	// service set. The relayer's L1 entry is the stale one.
	writeSupplierToRedis(t, mr, &SupplierState{
		OperatorAddress: addr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svcA", "svcB"},
	})

	// Within the (huge) L1 TTL: Get must still serve the cached service set, even
	// though L2 already changed. Proves L1 actually caches.
	state, err = cache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, []string{"svcA"}, state.Services,
		"L1 must keep serving the cached supplier while the entry is within supplierCacheL1TTL")

	// Expire L1: the next Get must treat L1 as a miss, re-read L2, and pick up the
	// new service set — the exact regression this test guards.
	supplierCacheL1TTL = 0
	state, err = cache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, []string{"svcA", "svcB"}, state.Services,
		"after supplierCacheL1TTL elapses, L1 must refresh and follow the L2 supplier state")
}
