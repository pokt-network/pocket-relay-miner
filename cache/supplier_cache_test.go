//go:build test

package cache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// newTestSupplierCache wires a SupplierCache against the shared real Redis,
// under this test's own namespace. Returns the cache and the client, both
// cleaned up by t.Cleanup.
func newTestSupplierCache(t *testing.T) (*SupplierCache, *redisutil.Client) {
	t.Helper()

	client := newTestRedis(t)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	cache := NewSupplierCache(logger, client, SupplierCacheConfig{
		FailOpen: false,
	})

	return cache, client
}

// writeSupplierToRedis marshals a SupplierState straight into Redis so tests
// can simulate entries written by another (possibly buggy) producer.
//
// The key comes from the SAME client's KeyBuilder the cache reads through. A
// second KeyBuilder of its own would write under the default namespace, which
// nothing here reads, and every assertion below would fail as a cache miss.
func writeSupplierToRedis(t *testing.T, client *redisutil.Client, state *SupplierState) {
	t.Helper()
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, client.Set(context.Background(),
		client.KB().SupplierStateKey(state.OperatorAddress), data, 0).Err())
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
			require.Equal(t, tc.want, tc.state.IsContaminated())
		})
	}
}

func TestGetSupplierState_ContaminatedInL1_EvictsAndReportsMiss(t *testing.T) {
	cache, _ := newTestSupplierCache(t)
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
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	const addr = "pokt1contaminatedL2"
	writeSupplierToRedis(t, client, &SupplierState{
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
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	const addr = "pokt1clean"
	writeSupplierToRedis(t, client, &SupplierState{
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
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	const addr = "pokt1unstaked"
	writeSupplierToRedis(t, client, &SupplierState{
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
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	const cleanAddr = "pokt1warmup_clean"
	const dirtyAddr = "pokt1warmup_dirty"
	const unstakedAddr = "pokt1warmup_unstaked"

	writeSupplierToRedis(t, client, &SupplierState{
		OperatorAddress: cleanAddr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svc1"},
	})
	writeSupplierToRedis(t, client, &SupplierState{
		OperatorAddress: dirtyAddr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{},
	})
	writeSupplierToRedis(t, client, &SupplierState{
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
	sc, _ := newTestSupplierCache(t)
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
// in-flight deactivation — so IsContaminated must return false for it.
func TestIsActive_IsContaminated_Boundary(t *testing.T) {
	unstakingEmptyServices := SupplierState{
		Staked:                  true,
		Status:                  SupplierStatusUnstaking,
		Services:                []string{},
		UnstakeSessionEndHeight: 150,
	}
	require.False(t, unstakingEmptyServices.IsContaminated(),
		"unstaking+empty-services is NOT contamination (IsContaminated checks active, not unstaking)")
	// IsActive is still true — per-service gate is IsActiveForService.
	require.True(t, unstakingEmptyServices.IsActive(),
		"unstaking supplier is IsActive even with no services (IsActiveForService gates per-service relay acceptance)")
}

func TestTransportDeclared(t *testing.T) {
	state := SupplierState{
		Staked: true,
		Status: SupplierStatusActive,
		StakedEndpoints: []StakedEndpoint{
			{ServiceID: "eth", RpcType: "jsonrpc"},
			{ServiceID: "eth", RpcType: "websocket"},
			{ServiceID: "poly", RpcType: "grpc"},
		},
	}
	// Declared pairs.
	require.True(t, state.TransportDeclared("eth", "jsonrpc"))
	require.True(t, state.TransportDeclared("eth", "websocket"))
	require.True(t, state.TransportDeclared("poly", "grpc"))
	// Same service, undeclared transport → false (the canonical warn case).
	require.False(t, state.TransportDeclared("eth", "grpc"))
	require.False(t, state.TransportDeclared("eth", "rest"))
	// Service not staked at all → false.
	require.False(t, state.TransportDeclared("cosmos", "jsonrpc"))
	// Declared transport but on a different service → false.
	require.False(t, state.TransportDeclared("poly", "jsonrpc"))
}

func TestTransportDeclared_FailOpenWhenEmpty(t *testing.T) {
	// Old miner / not-yet-published: empty StakedEndpoints means "unknown", so
	// every transport reads as declared (fail-open — never warn on missing data).
	state := SupplierState{Staked: true, Status: SupplierStatusActive, Services: []string{"eth"}}
	require.True(t, state.TransportDeclared("eth", "jsonrpc"))
	require.True(t, state.TransportDeclared("eth", "grpc"))
	require.True(t, state.TransportDeclared("anything", "rest"))
}

func TestStakedEndpoints_JSONRoundTripAndBackCompat(t *testing.T) {
	// New field round-trips.
	in := SupplierState{
		Staked:          true,
		Status:          SupplierStatusActive,
		Services:        []string{"eth"},
		StakedEndpoints: []StakedEndpoint{{ServiceID: "eth", RpcType: "grpc"}},
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out SupplierState
	require.NoError(t, json.Unmarshal(b, &out))
	require.Equal(t, in.StakedEndpoints, out.StakedEndpoints)

	// Back-compat: JSON written by an OLD miner (no staked_endpoints key) decodes
	// with a nil slice → fail-open.
	const oldJSON = `{"status":"active","staked":true,"services":["eth"],"operator_address":"pokt1x"}`
	var legacy SupplierState
	require.NoError(t, json.Unmarshal([]byte(oldJSON), &legacy))
	require.Nil(t, legacy.StakedEndpoints)
	require.True(t, legacy.TransportDeclared("eth", "grpc"), "empty endpoints must fail-open")
}

// TestSupplierCache_L1RefreshesAfterTTL is the regression test for the supplier
// cache-TTL gap. The SupplierCache L1 (in-process xsync map) had NO TTL, so once
// a relayer cached a supplier its stake status and service list were frozen for
// the process lifetime: pub/sub invalidation fires on the miner's Set/Delete but
// a relayer can miss it (restart, dropped event), stranding a stale stake/services
// view forever. The fix ages L1 entries out after supplierCacheL1TTL so
// GetSupplierState falls through to L2 (Redis) and follows the on-chain
// stake/services change WITHOUT a pod restart. This test drives that change
// against the REAL supplier cache on a real Redis.
//
// NOTE: unlike the service cache, SupplierCache is L1+L2 only — it has no L3
// query client (and thus no frozen-query-client stub to reuse). The downstream
// change is therefore driven at L2 (Redis), the cache's only authoritative
// source below L1, via the existing writeSupplierToRedis helper.
func TestSupplierCache_L1RefreshesAfterTTL(t *testing.T) {
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	// Use a large L1 TTL while we prove caching; restore the package default after.
	origTTL := supplierCacheL1TTL
	supplierCacheL1TTL = time.Hour
	t.Cleanup(func() { supplierCacheL1TTL = origTTL })

	const addr = "pokt1ttl"

	// Seed L2 with the old service set and load it into L1 via a real Get.
	writeSupplierToRedis(t, client, &SupplierState{
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
	writeSupplierToRedis(t, client, &SupplierState{
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

// TestSupplierCacheTTLFromParams pins the formula (HIGH-1, review
// 2026-08-20): TTL = 2 x num_blocks_per_session x block_time_seconds,
// grounded in live chain params rather than an arbitrary constant, with
// every missing-input path falling back to defaultSupplierCacheTTL instead
// of ever landing on a zero/no-TTL write.
//
// SupplierUnbondingPeriodSessions is deliberately NOT a formula input and
// deliberately NOT in these cases — an earlier version used it and produced a
// ~40-day TTL against mainnet's real value (1429 sessions, verified live via
// sauron-api.infra.pocket.network 2026-08-20), because that param is the
// stake-unlock window, not the service-deactivation one. See the doc comment
// on SupplierCacheTTLFromParams for the full correction.
func TestSupplierCacheTTLFromParams(t *testing.T) {
	cases := []struct {
		name             string
		params           *sharedtypes.Params
		blockTimeSeconds int64
		want             time.Duration
	}{
		{
			name: "mainnet params: 20 blocks/session, 60s blocks (verified live 2026-08-20)",
			params: &sharedtypes.Params{
				NumBlocksPerSession: 20,
				// A real mainnet value, included to document that it must NOT
				// affect the result — see the "unbonding period is ignored" case.
				SupplierUnbondingPeriodSessions: 1429,
			},
			blockTimeSeconds: 60,
			// 2 * 20 * 60s = 2400s = 40m
			want: 40 * time.Minute,
		},
		{
			name: "unbonding period is ignored: 1429 sessions changes nothing",
			params: &sharedtypes.Params{
				NumBlocksPerSession:             20,
				SupplierUnbondingPeriodSessions: 1429,
			},
			blockTimeSeconds: 60,
			want:             40 * time.Minute,
		},
		{
			name: "longer session length scales linearly",
			params: &sharedtypes.Params{
				NumBlocksPerSession: 60,
			},
			blockTimeSeconds: 60,
			// 2 * 60 * 60s = 7200s = 2h
			want: 2 * time.Hour,
		},
		{
			name:             "nil params falls back to the safety net",
			params:           nil,
			blockTimeSeconds: 60,
			want:             defaultSupplierCacheTTL,
		},
		{
			name: "zero block time falls back to the safety net",
			params: &sharedtypes.Params{
				NumBlocksPerSession: 20,
			},
			blockTimeSeconds: 0,
			want:             defaultSupplierCacheTTL,
		},
		{
			name: "zero blocks per session falls back to the safety net",
			params: &sharedtypes.Params{
				NumBlocksPerSession: 0,
			},
			blockTimeSeconds: 60,
			want:             defaultSupplierCacheTTL,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SupplierCacheTTLFromParams(tc.params, tc.blockTimeSeconds)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestSupplierCacheTTLFromParams_MarginAboveReconcileInterval pins the
// invariant r1's review flagged (point 3, 2026-08-20): the TTL must stay
// comfortably above the interval that refreshes it, or a stalled reconcile
// loop (leader change, a stuck query) expires a LIVE supplier's entry and
// flips relayer/proxy.go's decideSupplierServe into optimistic-serve for a
// supplier the miner has simply lost track of, not decommissioned — the
// original bug's shape, inverted. Written here (not just in a comment) so
// shrinking the multiplier or the session length someday fails loudly instead
// of silently narrowing this margin.
//
// miner.DefaultSupplierReconcileInterval (60s) is duplicated as a literal
// because importing package miner from cache would cycle (miner already
// imports cache); the two are pinned to the SAME value by
// TestDefaultSupplierReconcileIntervalMatchesTTLMarginAssumption in
// miner/supplier_worker_test.go.
func TestSupplierCacheTTLFromParams_MarginAboveReconcileInterval(t *testing.T) {
	const reconcileInterval = 60 * time.Second
	const mainnetBlockTimeSeconds = 60

	ttl := SupplierCacheTTLFromParams(&sharedtypes.Params{NumBlocksPerSession: 20}, mainnetBlockTimeSeconds)

	require.Greater(t, ttl, 10*reconcileInterval,
		"the TTL must survive many missed reconcile ticks, not just one, or a single stalled "+
			"pass on a live supplier reads as decommissioned")
}

// TestSetSupplierState_WritesABoundedTTL is the test-teeth for HIGH-1: it
// asserts the CONSEQUENCE (the key actually expires in Redis) rather than the
// mechanism, so it cannot be satisfied by a TTL value that is merely
// "present" but too long to matter, or by a mock that never touches real
// expiry. Before the fix, SetSupplierState wrote with TTL=0 (no expiry) and
// this test fails: the key survives with TTL == -1 (no expiry) forever.
func TestSetSupplierState_WritesABoundedTTL(t *testing.T) {
	ctx := context.Background()
	client := newTestRedis(t)
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	const configuredTTL = 3 * time.Second
	cache := NewSupplierCache(logger, client, SupplierCacheConfig{
		FailOpen: false,
		TTL:      configuredTTL,
	})

	const addr = "pokt1ttlbound"
	require.NoError(t, cache.SetSupplierState(ctx, &SupplierState{
		OperatorAddress: addr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svcA"},
	}))

	key := client.KB().SupplierStateKey(addr)
	ttl, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0),
		"SetSupplierState must write a positive TTL, never TTL=0 (no expiry) — "+
			"a supplier whose signing key leaves the keyring must eventually age out, not freeze forever")
	require.LessOrEqual(t, ttl, configuredTTL,
		"the key's remaining TTL must not exceed what was configured")

	// The actual expiry, not just the metadata. Rule #1 (CLAUDE.md) forbids
	// time.Sleep for synchronization, so the remaining TTL is driven to 0
	// directly (PExpire) rather than waited out — the key vanishing on a
	// TTL of 0 is the same Redis guarantee a real elapsed TTL relies on, with
	// no timing window to be flaky on.
	ok, err := client.PExpire(ctx, key, 0).Result()
	require.NoError(t, err)
	require.True(t, ok, "the key must still exist for its TTL to be driven to 0")

	exists, err := client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), exists, "the entry must actually expire once its TTL elapses")
}

// TestNewSupplierCache_DefaultsTTLWhenUnconfigured pins the fallback path: a
// caller that does not (or cannot, chain unreachable at startup) supply a TTL
// must still get a bounded one, never SetSupplierState's old TTL=0.
func TestNewSupplierCache_DefaultsTTLWhenUnconfigured(t *testing.T) {
	ctx := context.Background()
	client := newTestRedis(t)
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	cache := NewSupplierCache(logger, client, SupplierCacheConfig{FailOpen: false})
	require.Equal(t, defaultSupplierCacheTTL, time.Duration(cache.ttl.Load()))

	const addr = "pokt1ttldefault"
	require.NoError(t, cache.SetSupplierState(ctx, &SupplierState{
		OperatorAddress: addr,
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svcA"},
	}))

	ttl, err := client.TTL(ctx, client.KB().SupplierStateKey(addr)).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, defaultSupplierCacheTTL)
}

// TestWarmupFromRedis_DiscoversEveryKeyItScans is the L2 half of a check that
// was only ever done by hand against a live stack (2026-08-21): every supplier
// key present in Redis must come back from the discovery scan and land in L1.
//
// It exists because the failure is INVISIBLE to the live gate. Discovery
// extracts the operator address from each scanned key; if that extraction breaks
// -- a drifted trim prefix, a namespace change -- the scan returns keys and the
// loop discards every one of them. L1 stays empty, every relay falls through to
// L2, and the relays still get served and billed. The live gate measures relays
// billed, so it passes green while the cache layer it depends on does nothing.
//
// Asserting on L1 CONTENT rather than on the log line is the point: the count in
// "discovered_suppliers" can be right while the addresses are garbage.
func TestWarmupFromRedis_DiscoversEveryKeyItScans(t *testing.T) {
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	// A staked entry, an unstaked one, and an address whose text could tempt a
	// naive prefix trim (it repeats the prefix's last segment).
	addrs := []string{
		"pokt1warmup_discover_a",
		"pokt1warmup_discover_b",
		"pokt1supplier_looking_addr",
	}
	for i, addr := range addrs {
		state := &SupplierState{
			OperatorAddress: addr,
			Status:          SupplierStatusActive,
			Staked:          true,
			Services:        []string{"svc1"},
		}
		if i == 1 {
			state.Status = SupplierStatusNotStaked
			state.Staked = false
			state.Services = nil
		}
		writeSupplierToRedis(t, client, state)
	}

	require.NoError(t, cache.WarmupFromRedis(ctx, nil))

	for _, addr := range addrs {
		_, ok := cache.localCache.Load(addr)
		require.Truef(t, ok,
			"warmup scanned this key but %q never reached L1: the address extraction "+
				"dropped it, which leaves L1 empty and sends every relay to L2 -- "+
				"degraded silently, and invisible to the live gate", addr)
	}
}

// countInvalidationsUntilFence returns how many invalidation messages the
// subscription holds up to a sentinel the caller publishes itself.
//
// It is deterministic without a sleep: PublishInvalidation is synchronous (it
// waits for the server's reply before SetSupplierState returns), so by the time
// the test publishes the fence, any invalidation the code under test produced is
// already queued ahead of it on the same server.
func countInvalidationsUntilFence(
	t *testing.T,
	client *redisutil.Client,
	msgs <-chan *goredis.Message,
) int {
	t.Helper()

	const fence = `{"operator_address": "__fence__"}`
	channel := client.KB().EventChannel(supplierCacheType, "invalidate")
	require.NoError(t, client.Publish(context.Background(), channel, fence).Err())

	count := 0
	for {
		select {
		case msg := <-msgs:
			if msg.Payload == fence {
				return count
			}
			count++
		case <-time.After(10 * time.Second):
			t.Fatalf("fence never arrived on %s after %d messages", channel, count)
		}
	}
}

// TestSetSupplierStateDoesNotInvalidateWhenNothingChanged pins the difference
// between "the entry is current" and "the entry was just republished".
//
// Measured on a clean stack (2026-08-21): 34 invalidations/min per relayer at
// rest with 17 suppliers and 2 miners -- exactly 17 x 2 / 60s, i.e. every
// reconcile rewriting and republishing every supplier with nothing changed.
// Each one drops that supplier's L1 on every relayer. At 594 suppliers it is
// ~1200/min of pure churn.
//
// The TTL must still be refreshed on the no-change path: it is what keeps the
// entry alive, and an expired entry makes the relayer serve optimistically,
// which quietly disables the per-service gate for the most stable suppliers --
// the ones that by definition never trigger a rewrite.
//
// This counts messages on the invalidation channel, NOT cacheInvalidations:
// SetSupplierState increments no counter (the supplier counter is bumped by
// DeleteSupplierState and by the subscriber's handler), so a metric assertion
// here would read 0 both before and after the fix.
func TestSetSupplierStateDoesNotInvalidateWhenNothingChanged(t *testing.T) {
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	sub := client.Subscribe(ctx, client.KB().EventChannel(supplierCacheType, "invalidate"))
	t.Cleanup(func() { _ = sub.Close() })
	_, err := sub.Receive(ctx) // wait for the subscription to be established
	require.NoError(t, err)
	msgs := sub.Channel()

	state := &SupplierState{
		OperatorAddress: "pokt1nochange",
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svc1"},
	}
	require.NoError(t, cache.SetSupplierState(ctx, state))
	require.Equal(t, 1, countInvalidationsUntilFence(t, client, msgs),
		"premise: the first write of an entry must publish")

	key := client.KB().SupplierStateKey(state.OperatorAddress)
	ttlBefore, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttlBefore, time.Duration(0), "premise: the entry carries a TTL")

	// Same content, written again -- what every reconcile of every miner does.
	require.NoError(t, cache.SetSupplierState(ctx, state))
	require.Equal(t, 0, countInvalidationsUntilFence(t, client, msgs),
		"re-writing identical state must not publish an invalidation: every one of "+
			"these drops the entry's L1 on every relayer for no reason")

	ttlAfter, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttlAfter, time.Duration(0),
		"the TTL must still be refreshed: an expired entry makes the relayer serve "+
			"optimistically, which disables the per-service gate exactly for the "+
			"suppliers stable enough never to trigger a rewrite")

	// And a real change must still publish.
	changed := *state
	changed.Services = []string{"svc1", "svc2"}
	require.NoError(t, cache.SetSupplierState(ctx, &changed))
	require.Equal(t, 1, countInvalidationsUntilFence(t, client, msgs),
		"a genuine change must still invalidate, or relayers keep serving stale services")
}

// TestSetSupplierStateComparesContentNotStoredBytes is the guard for the way
// this suppression is easiest to get wrong, and to get wrong INVISIBLY.
//
// SetSupplierState stamps state.LastUpdated with the current second before
// marshalling, so comparing the stored bytes as-is can never find two writes
// equal in production, where reconcile passes are 60s apart -- the suppression
// would be dead code. A test that writes twice in a row would not notice: both
// writes land in the same second, the bytes match by accident, and it passes
// against an implementation that does nothing.
//
// So this drives the case the naive comparison gets wrong: an entry whose stored
// timestamp is older than the one being written, with identical content.
func TestSetSupplierStateComparesContentNotStoredBytes(t *testing.T) {
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	state := &SupplierState{
		OperatorAddress: "pokt1oldtimestamp",
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svc1"},
		LastUpdated:     time.Now().Add(-2 * time.Hour).Unix(),
	}
	writeSupplierToRedis(t, client, state)

	// SetSupplierState stamps state.LastUpdated in place, so the pre-call value
	// has to be captured here or the assertion below compares it to itself.
	storedBefore := state.LastUpdated

	sub := client.Subscribe(ctx, client.KB().EventChannel(supplierCacheType, "invalidate"))
	t.Cleanup(func() { _ = sub.Close() })
	_, err := sub.Receive(ctx)
	require.NoError(t, err)
	msgs := sub.Channel()

	// Same content, two hours later. Only the timestamp differs.
	require.NoError(t, cache.SetSupplierState(ctx, state))
	require.Equal(t, 0, countInvalidationsUntilFence(t, client, msgs),
		"a fresher timestamp on identical content is not a change: comparing raw "+
			"stored bytes would republish here, which is every reconcile in production")

	key := client.KB().SupplierStateKey(state.OperatorAddress)

	ttl, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0),
		"and the entry written without a TTL by an older version must gain one")

	// Suppressing the PUBLISH is the point; suppressing the write is not. Only
	// the write refreshes last_updated, which is what `redis supplier` reports
	// as "Last Updated" -- freeze it and a healthy supplier that has not changed
	// in hours reads as one nothing is tracking.
	raw, err := client.Get(ctx, key).Bytes()
	require.NoError(t, err)

	var stored SupplierState
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Greater(t, stored.LastUpdated, storedBefore,
		"the unchanged path must still refresh last_updated: it is the operator's "+
			"only signal that this entry is still being tracked")
}

// TestSetSupplierStateIgnoresTheWriterWhenComparing is the regression test for a
// suppression that was correct in a one-miner test and inert in the real fleet.
//
// Every write stamps UpdatedBy with the miner's own instance ID (it exists "for
// debugging", per the field's own comment). With two miners both reconciling all
// suppliers, that field ALTERNATES: miner A writes and sees B's value, miner B
// writes and sees A's. Comparing it makes every pass look like a change, so both
// miners republish every supplier every interval and the invalidation rate does
// not move at all.
//
// Measured live on a clean stack (2026-08-21) with the comparison including
// UpdatedBy: 37.3 invalidations/min per side against a 34/min baseline, i.e. the
// fix bought nothing, while the stored value provably differed only in
// last_updated and updated_by. No single-writer test can see this.
func TestSetSupplierStateIgnoresTheWriterWhenComparing(t *testing.T) {
	cache, client := newTestSupplierCache(t)
	ctx := context.Background()

	state := &SupplierState{
		OperatorAddress: "pokt1twowriters",
		Status:          SupplierStatusActive,
		Staked:          true,
		Services:        []string{"svc1"},
		UpdatedBy:       "miner-a",
	}
	require.NoError(t, cache.SetSupplierState(ctx, state))

	sub := client.Subscribe(ctx, client.KB().EventChannel(supplierCacheType, "invalidate"))
	t.Cleanup(func() { _ = sub.Close() })
	_, err := sub.Receive(ctx)
	require.NoError(t, err)
	msgs := sub.Channel()

	// The second miner, same supplier, same state, different writer.
	fromB := *state
	fromB.UpdatedBy = "miner-b"
	require.NoError(t, cache.SetSupplierState(ctx, &fromB))
	require.Equal(t, 0, countInvalidationsUntilFence(t, client, msgs),
		"a different miner writing the same supplier state is not a change: comparing "+
			"UpdatedBy makes every reconcile of every miner republish everything")

	// And back to the first miner, which is what actually alternates in a fleet.
	require.NoError(t, cache.SetSupplierState(ctx, state))
	require.Equal(t, 0, countInvalidationsUntilFence(t, client, msgs),
		"and alternating back is not a change either")

	// The field is still recorded: it is how an operator sees who wrote last.
	raw, err := client.Get(ctx, client.KB().SupplierStateKey(state.OperatorAddress)).Bytes()
	require.NoError(t, err)

	var stored SupplierState
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Equal(t, "miner-a", stored.UpdatedBy,
		"ignoring the writer for the comparison must not stop recording it")
}
