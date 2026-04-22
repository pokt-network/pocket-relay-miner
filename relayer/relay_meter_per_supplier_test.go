//go:build test

package relayer

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// staticServiceFactor returns the same factor for every (service, app)
// tuple. Used to pin the cap arithmetic for deterministic tests.
type staticServiceFactor struct{ f float64 }

func (s staticServiceFactor) GetServiceFactor(_ context.Context, _ string) (float64, bool) {
	return s.f, true
}

// TestCheckAndConsumeRelay_PerSupplierIsolation proves that two suppliers
// serving the SAME session get INDEPENDENT meter state. The bug this
// test guards against: a prior schema keyed the consumed counter and the
// SessionMeterMeta only on sessionID, so once supplier A exhausted its
// cap, supplier B on the same session was immediately rejected — not
// because B had consumed anything, but because B read A's exhausted
// counter.
//
// With the per-(session, supplier) key layout, capping supplier A must
// leave supplier B's counter untouched, and B must be allowed to serve
// up to its OWN cap before getting rejected. This mirrors the canonical
// poktroll model: each supplier claims its portion of the app's stake
// independently.
func TestCheckAndConsumeRelay_PerSupplierIsolation(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer redisClient.Close()

	// Deterministic cap. With appStake=1000 uPOKT, serviceFactor=0.5 →
	// effectiveLimit = 500 uPOKT per supplier. Relay cost = 1 uPOKT
	// (fallback compute_units=1, multiplier=1, granularity=1). Per-
	// supplier cap therefore = 500 relays.
	const (
		appStakeUpokt  = int64(1000)
		perSupplierCap = int64(500)
	)

	appAddr := "pokt1app_shared"
	app := &fakeAppClient{addr: appAddr}
	app.stakeUpokt.Store(appStakeUpokt)

	// Pre-seed session params so getSessionParams hits Redis only.
	sessionParams := CachedSessionParams{NumSuppliersPerSession: 2, UpdatedAt: 1}
	spBytes, err := json.Marshal(sessionParams)
	require.NoError(t, err)
	require.NoError(t, redisClient.Set(ctx, "ha:params:session", spBytes, 0).Err())

	sharedParams := &sharedtypes.Params{
		NumBlocksPerSession:            10,
		ComputeUnitsToTokensMultiplier: 1,
		ComputeUnitCostGranularity:     1,
	}

	meter := NewRelayMeter(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		app,
		nil, // sharedClient (unused; fakeSharedParamCache serves)
		nil, // sessionClient (unused; Redis pre-seeded)
		nil, // blockClient
		&fakeSharedParamCache{params: sharedParams},
		nil, // serviceCache (unused — fallback compute_units=1)
		staticServiceFactor{f: 0.5},
		RelayMeterConfig{RedisKeyPrefix: "ha"},
	)
	require.NoError(t, meter.Start(ctx))
	defer func() { _ = meter.Close() }()

	sessionID := "sess-shared"
	supplierA := "pokt1supplier_a"
	supplierB := "pokt1supplier_b"
	serviceID := "svc-x"
	sessionEndHeight := int64(100)

	// Supplier A consumes up to its cap. Every call within the cap must
	// return allowed=true.
	for i := int64(0); i < perSupplierCap; i++ {
		allowed, err := meter.CheckAndConsumeRelay(ctx, sessionID, appAddr, serviceID, supplierA, sessionEndHeight)
		require.NoError(t, err, "unexpected error at supplier A relay #%d", i+1)
		require.True(t, allowed, "supplier A relay #%d must be within cap (cap=%d)", i+1, perSupplierCap)
	}

	// One more relay on A MUST be rejected — cap exhausted.
	allowed, err := meter.CheckAndConsumeRelay(ctx, sessionID, appAddr, serviceID, supplierA, sessionEndHeight)
	require.NoError(t, err)
	require.False(t, allowed, "supplier A must be rejected after %d relays (at cap)", perSupplierCap)

	// Inspect per-(session, supplier) state in Redis directly. A must be
	// at its cap, B must be zero (or absent).
	consumedA, err := redisClient.Get(ctx, meter.consumedKey(sessionID, supplierA)).Int64()
	require.NoError(t, err)
	require.Equal(t, perSupplierCap, consumedA,
		"supplier A's consumed counter must equal its cap after exhaustion")

	// Supplier B's counter must NOT exist yet — A's exhaustion must not
	// pollute B's bucket.
	bExists := mr.Exists(meter.consumedKey(sessionID, supplierB))
	require.False(t, bExists,
		"supplier B must not have a consumed counter before its first relay — "+
			"the bug this test guards against is a shared counter keyed only by sessionID")

	// CRITICAL assertion: supplier B's FIRST relay on the same session
	// must be allowed. With the pre-fix shared-counter schema, this call
	// would see consumed=perSupplierCap and return false. With the
	// per-(session, supplier) schema, B starts at zero.
	allowed, err = meter.CheckAndConsumeRelay(ctx, sessionID, appAddr, serviceID, supplierB, sessionEndHeight)
	require.NoError(t, err)
	require.True(t, allowed,
		"supplier B's first relay on the shared session must be allowed — "+
			"its cap is independent of supplier A's exhausted counter")

	// B must be allowed up to ITS OWN cap (perSupplierCap - 1 more after
	// the first call above).
	for i := int64(1); i < perSupplierCap; i++ {
		allowed, err := meter.CheckAndConsumeRelay(ctx, sessionID, appAddr, serviceID, supplierB, sessionEndHeight)
		require.NoError(t, err)
		require.True(t, allowed, "supplier B relay #%d must be within its own cap", i+1)
	}

	// B's (cap+1)th call must be rejected — B hit its own cap, NOT A's.
	allowed, err = meter.CheckAndConsumeRelay(ctx, sessionID, appAddr, serviceID, supplierB, sessionEndHeight)
	require.NoError(t, err)
	require.False(t, allowed, "supplier B must be rejected after consuming its own per-supplier cap")

	// Final sanity: A and B counters live in distinct keys, both at cap.
	consumedA, err = redisClient.Get(ctx, meter.consumedKey(sessionID, supplierA)).Int64()
	require.NoError(t, err)
	consumedB, err := redisClient.Get(ctx, meter.consumedKey(sessionID, supplierB)).Int64()
	require.NoError(t, err)
	require.Equal(t, perSupplierCap, consumedA, "A's counter must still be at its cap")
	require.Equal(t, perSupplierCap, consumedB, "B's counter must independently reach its cap")
}

// TestClearSessionMeter_PerSupplierIsolation proves that cleaning up one
// supplier's meter does NOT touch a co-supplier on the same session.
// The cleanup publisher now carries (sessionID, supplierAddress) so the
// subscriber can scope the deletion correctly.
func TestClearSessionMeter_PerSupplierIsolation(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer redisClient.Close()

	app := &fakeAppClient{addr: "pokt1app_shared"}
	app.stakeUpokt.Store(1000)

	sessionParams := CachedSessionParams{NumSuppliersPerSession: 2, UpdatedAt: 1}
	spBytes, err := json.Marshal(sessionParams)
	require.NoError(t, err)
	require.NoError(t, redisClient.Set(ctx, "ha:params:session", spBytes, 0).Err())

	meter := NewRelayMeter(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		app,
		nil, nil, nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 1,
			ComputeUnitCostGranularity:     1,
		}},
		nil,
		staticServiceFactor{f: 0.5},
		RelayMeterConfig{RedisKeyPrefix: "ha"},
	)
	require.NoError(t, meter.Start(ctx))
	defer func() { _ = meter.Close() }()

	sessionID := "sess-cleanup"
	supplierA, supplierB := "pokt1sa", "pokt1sb"
	serviceID := "svc"

	// Prime both meters with a relay each so both keys exist in Redis.
	for _, sup := range []string{supplierA, supplierB} {
		allowed, err := meter.CheckAndConsumeRelay(ctx, sessionID, "pokt1app_shared", serviceID, sup, 100)
		require.NoError(t, err)
		require.True(t, allowed)
	}

	// Clear ONLY A's meter.
	require.NoError(t, meter.ClearSessionMeter(ctx, sessionID, supplierA))

	// A's keys must be gone.
	require.False(t, mr.Exists(meter.consumedKey(sessionID, supplierA)),
		"A's consumed key must be deleted")
	require.False(t, mr.Exists(meter.metaKey(sessionID, supplierA)),
		"A's meta key must be deleted")

	// B's keys must be untouched.
	require.True(t, mr.Exists(meter.consumedKey(sessionID, supplierB)),
		"B's consumed key must survive A's cleanup — the bug to guard against "+
			"is shared-sessionID cleanup clobbering a co-supplier's live meter")
	require.True(t, mr.Exists(meter.metaKey(sessionID, supplierB)),
		"B's meta key must survive A's cleanup")
}
