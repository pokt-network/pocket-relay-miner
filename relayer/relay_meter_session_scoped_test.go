//go:build test

package relayer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// stubComputeUnitsProvider records the height it was asked for and returns a
// per-height CUPR, so a test can prove the meter prices a relay with the SAME
// compute units the relay is mined with.
type stubComputeUnitsProvider struct {
	byHeight   map[int64]uint64
	gotHeights []int64
}

func (s *stubComputeUnitsProvider) GetServiceComputeUnits(_ context.Context, _ string, sessionStartHeight int64) uint64 {
	s.gotHeights = append(s.gotHeights, sessionStartHeight)
	if cupr, ok := s.byHeight[sessionStartHeight]; ok {
		return cupr
	}
	return 1
}

// newScopedTestMeter builds a RelayMeter over a real Redis with an epoch-aware
// shared params cache, so "which params epoch priced this?" is observable.
func newScopedTestMeter(
	t *testing.T,
	ctx context.Context,
	paramCache *fakeSharedParamCache,
	cuProvider ServiceComputeUnitsProvider,
) *RelayMeter {
	t.Helper()

	redisClient, _ := newTestRedis(t)

	app := &fakeAppClient{addr: "pokt1app_scoped"}
	app.stakeUpokt.Store(1_000_000)

	meter := NewRelayMeter(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		app,
		nil,
		&fakeSessionClient{numSuppliers: 1},
		nil,
		paramCache,
		nil,
		nil, // no service factor: exercise the baseLimit formula
		RelayMeterConfig{},
	)
	if cuProvider != nil {
		meter.SetServiceComputeUnitsProvider(cuProvider)
	}
	require.NoError(t, meter.Start(ctx))
	t.Cleanup(func() { _ = meter.Close() })

	return meter
}

// TestGetRelayCost_PricesAtSessionStartHeight is the P1.1 regression test for the
// pricing inputs.
//
// After governance DECREASES compute_units_to_tokens_multiplier, an already-open
// session priced with live params under-charges every remaining relay, so the
// supplier serves past what the application's stake covers at the settlement rate
// — the excess is delivered unpaid, silently.
func TestGetRelayCost_PricesAtSessionStartHeight(t *testing.T) {
	const (
		sessionStart = int64(91)
		oldCUTTM     = uint64(100)
		newCUTTM     = uint64(10)
	)

	paramCache := &fakeSharedParamCache{
		// "latest" = the post-change epoch.
		params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: newCUTTM,
			ComputeUnitCostGranularity:     1,
		},
		byHeight: map[int64]*sharedtypes.Params{
			sessionStart: {
				NumBlocksPerSession:            10,
				ComputeUnitsToTokensMultiplier: oldCUTTM,
				ComputeUnitCostGranularity:     1,
			},
		},
	}
	cu := &stubComputeUnitsProvider{byHeight: map[int64]uint64{sessionStart: 7}}

	meter := newScopedTestMeter(t, context.Background(), paramCache, cu)

	cost, err := meter.getRelayCost(context.Background(), "seda", sessionStart)
	require.NoError(t, err)
	require.Equal(t, int64(7*oldCUTTM), cost,
		"relay cost must use the session-start params epoch, not the live one")

	require.Contains(t, paramCache.heightsQueried(), sessionStart,
		"shared params must be resolved at the session start height")
	require.Equal(t, []int64{sessionStart}, cu.gotHeights,
		"CUPR must be resolved at the session start height")
}

// TestGetRelayCost_LiveWhenNoSessionHeight covers the CheckRelayHealth probe: it
// has no session, so height 0 must resolve the live params instead of querying
// at a meaningless height.
func TestGetRelayCost_LiveWhenNoSessionHeight(t *testing.T) {
	paramCache := &fakeSharedParamCache{
		params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 10,
			ComputeUnitCostGranularity:     1,
		},
	}
	cu := &stubComputeUnitsProvider{byHeight: map[int64]uint64{91: 7}}

	meter := newScopedTestMeter(t, context.Background(), paramCache, cu)

	cost, err := meter.getRelayCost(context.Background(), "seda", 0)
	require.NoError(t, err)
	require.Equal(t, int64(10), cost, "live params x default 1 CU")

	require.Empty(t, paramCache.heightsQueried(), "must not query params at a meaningless height")
	require.Empty(t, cu.gotHeights, "must not query CUPR at a meaningless height")
}

// TestGetRelayCost_ConsumeAndRevertPriceIdentically guards the meter's consumed
// counter. The refund is recomputed rather than remembered, so consume and revert
// must resolve the same cost for the same session — otherwise the counter drifts
// permanently.
func TestGetRelayCost_ConsumeAndRevertPriceIdentically(t *testing.T) {
	const sessionStart = int64(91)

	paramCache := &fakeSharedParamCache{
		params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 10, // live epoch differs...
			ComputeUnitCostGranularity:     1,
		},
		byHeight: map[int64]*sharedtypes.Params{
			sessionStart: { // ...from the session's own epoch
				NumBlocksPerSession:            10,
				ComputeUnitsToTokensMultiplier: 100,
				ComputeUnitCostGranularity:     1,
			},
		},
	}
	cu := &stubComputeUnitsProvider{byHeight: map[int64]uint64{sessionStart: 7}}

	ctx := context.Background()
	meter := newScopedTestMeter(t, ctx, paramCache, cu)

	consumeCost, err := meter.getRelayCost(ctx, "seda", sessionStart)
	require.NoError(t, err)

	revertCost, err := meter.getRelayCost(ctx, "seda", sessionStart)
	require.NoError(t, err)

	require.Equal(t, consumeCost, revertCost,
		"a revert priced differently from its consume desynchronises the session meter")
}

// TestCalculateMaxStake_UsesParamsAtSessionEnd is the P1.1 regression test for the
// budget divisor: num_pending_sessions is derived from the window offsets, which
// must resolve under the session's own params epoch (matching poktroll's
// ensureRequestSessionRelayMeter).
func TestCalculateMaxStake_UsesParamsAtSessionEnd(t *testing.T) {
	const (
		sessionEnd = int64(100)
		appStake   = int64(1_000_000)
	)

	// Old epoch: proof window close total = 10, session length 10 -> ceil(10/10)+1 = 2
	// New epoch: proof window close total = 40, session length 10 -> ceil(40/10)+1 = 5
	oldParams := &sharedtypes.Params{
		NumBlocksPerSession:            10,
		ComputeUnitsToTokensMultiplier: 1,
		ComputeUnitCostGranularity:     1,
		ClaimWindowOpenOffsetBlocks:    2,
		ClaimWindowCloseOffsetBlocks:   2,
		ProofWindowOpenOffsetBlocks:    3,
		ProofWindowCloseOffsetBlocks:   3,
	}
	newParams := &sharedtypes.Params{
		NumBlocksPerSession:            10,
		ComputeUnitsToTokensMultiplier: 1,
		ComputeUnitCostGranularity:     1,
		ClaimWindowOpenOffsetBlocks:    10,
		ClaimWindowCloseOffsetBlocks:   10,
		ProofWindowOpenOffsetBlocks:    10,
		ProofWindowCloseOffsetBlocks:   10,
	}

	paramCache := &fakeSharedParamCache{
		params:   newParams,
		byHeight: map[int64]*sharedtypes.Params{sessionEnd: oldParams},
	}

	ctx := context.Background()
	meter := newScopedTestMeter(t, ctx, paramCache, nil)

	// currentHeight past the session end -> ended session -> at-height resolution.
	const currentHeight = int64(110)
	maxStake, factor, stakeUsed, err := meter.calculateMaxStake(ctx, "pokt1app_scoped", "seda", sessionEnd, currentHeight)
	require.NoError(t, err)
	require.Equal(t, appStake, stakeUsed)
	require.Zero(t, factor, "no service factor configured")

	// numSuppliers = 1 -> appStakePerSupplier = appStake
	// old epoch pendingSessions = ceil(10/10) + 1 = 2
	require.Equal(t, appStake/2, maxStake,
		"budget must use the session's own params epoch (old=2 pending sessions), not the live epoch (5)")

	require.Contains(t, paramCache.heightsQueried(), sessionEnd,
		"window offsets must be resolved at the session END height")
}

// TestCalculateMaxStake_ActiveSessionUsesLiveParams is the F1 meter regression: for
// an ACTIVE session the end height is in the future, so the budget must resolve the
// live params directly rather than querying at the future end height (which pins
// today's value under a future cache key and leans on pocketd answering futures).
func TestCalculateMaxStake_ActiveSessionUsesLiveParams(t *testing.T) {
	const (
		sessionEnd    = int64(100)
		currentHeight = int64(95) // still inside the session -> end is in the future
		appStake      = int64(1_000_000)
	)

	liveParams := &sharedtypes.Params{
		NumBlocksPerSession:            10,
		ComputeUnitsToTokensMultiplier: 1,
		ComputeUnitCostGranularity:     1,
		ClaimWindowOpenOffsetBlocks:    2,
		ClaimWindowCloseOffsetBlocks:   2,
		ProofWindowOpenOffsetBlocks:    3,
		ProofWindowCloseOffsetBlocks:   3,
	}
	// byHeight is deliberately EMPTY: an at-height query would fall back to params
	// but would also record the future height in heightsQueried, which we forbid.
	paramCache := &fakeSharedParamCache{params: liveParams}

	ctx := context.Background()
	meter := newScopedTestMeter(t, ctx, paramCache, nil)

	_, _, stakeUsed, err := meter.calculateMaxStake(ctx, "pokt1app_scoped", "seda", sessionEnd, currentHeight)
	require.NoError(t, err)
	require.Equal(t, appStake, stakeUsed)

	require.NotContains(t, paramCache.heightsQueried(), sessionEnd,
		"an active session must NOT query params at the future end height")
}
