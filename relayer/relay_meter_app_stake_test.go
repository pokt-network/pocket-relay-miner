//go:build test

package relayer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// fakeAppClient lets the test swap the on-chain stake an app reports
// between two calls, simulating a MsgStakeApplication landing mid-session.
type fakeAppClient struct {
	addr       string
	stakeUpokt atomic.Int64
}

func (f *fakeAppClient) GetApplication(_ context.Context, address string) (apptypes.Application, error) {
	if address != f.addr {
		return apptypes.Application{}, fmt.Errorf("unexpected app: %s", address)
	}
	return apptypes.Application{
		Address: f.addr,
		Stake: &cosmostypes.Coin{
			Denom:  "upokt",
			Amount: sdkmath.NewInt(f.stakeUpokt.Load()),
		},
	}, nil
}

func (f *fakeAppClient) GetAllApplications(_ context.Context) ([]apptypes.Application, error) {
	return nil, nil
}

func (f *fakeAppClient) GetParams(_ context.Context) (*apptypes.Params, error) {
	return nil, nil
}

type fakeSharedParamCache struct{ params *sharedtypes.Params }

func (f *fakeSharedParamCache) GetLatestSharedParams(_ context.Context) (*sharedtypes.Params, error) {
	return f.params, nil
}

// TestGetOrCreateSessionMeter_RecomputesMaxStake_WhenAppStakeChanges is the
// behavioural proof that the fix for the app-stake staleness bug works:
// when an operator runs MsgStakeApplication mid-session, the next relay on
// that session must recompute MaxStakeUpokt from the fresh stake instead of
// serving the snapshot taken at session creation.
//
// Before the fix, SessionMeterMeta only invalidated on serviceFactor change
// and the RelayMeter read app stake from a sidecar Redis cache with 2h TTL
// and zero invalidation. This test drives the meter through a stake change
// and asserts the recomputed meta reflects the new value.
func TestGetOrCreateSessionMeter_RecomputesMaxStake_WhenAppStakeChanges(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer redisClient.Close()

	appAddr := "pokt1app_under_test"
	app := &fakeAppClient{addr: appAddr}
	app.stakeUpokt.Store(10_000_000_000) // 10k POKT

	// Pre-seed session params in Redis so the meter doesn't call the
	// session client on chain. Same contract the miner's refresher uses.
	sessionParams := CachedSessionParams{NumSuppliersPerSession: 1, UpdatedAt: 1}
	spBytes, err := json.Marshal(sessionParams)
	require.NoError(t, err)
	require.NoError(t, redisClient.Set(ctx, "ha:params:session", spBytes, 0).Err())

	// Fake shared params cache: minimum fields calculateMaxStake reads.
	sharedParams := &sharedtypes.Params{
		NumBlocksPerSession:            10,
		ComputeUnitsToTokensMultiplier: 1,
		ComputeUnitCostGranularity:     1,
		// Keep windows at 0 so the pendingSessions math is a plain 1.
	}

	meter := NewRelayMeter(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		app,            // appClient
		nil,            // sharedClient (unused — cache hits)
		nil,            // sessionClient (unused — Redis pre-seeded)
		nil,            // blockClient
		&fakeSharedParamCache{params: sharedParams},
		nil, // serviceCache (unused in getOrCreateSessionMeter path)
		nil, // serviceFactorProvider → baseLimit path
		RelayMeterConfig{RedisKeyPrefix: "ha"},
	)
	require.NoError(t, meter.Start(ctx))
	defer func() { _ = meter.Close() }()

	sessionID := "sess-stake-change"

	// First call: creates meter with stake = 10k POKT.
	meta1, maxStake1, err := meter.getOrCreateSessionMeter(ctx, sessionID, appAddr, "svc-a", "pokt1sup", 100)
	require.NoError(t, err)
	require.NotNil(t, meta1)
	require.Greater(t, maxStake1, int64(0), "initial max stake must be non-zero")
	require.Equal(t, int64(10_000_000_000), meta1.CreatedWithAppStake,
		"meter must snapshot the app stake that produced MaxStakeUpokt")

	// Operator broadcasts MsgStakeApplication: stake now 50k POKT.
	// The application cache's pub/sub invalidation would reach the reader,
	// but for this unit test we directly swap what the cached app client
	// returns — the RelayMeter must still observe and react.
	app.stakeUpokt.Store(50_000_000_000)

	// Second call on the SAME session: must recompute, not serve the stale
	// snapshot. This is the invariant the pre-fix code violated.
	meta2, maxStake2, err := meter.getOrCreateSessionMeter(ctx, sessionID, appAddr, "svc-a", "pokt1sup", 100)
	require.NoError(t, err)
	require.NotNil(t, meta2)
	require.Equal(t, int64(50_000_000_000), meta2.CreatedWithAppStake,
		"meter must recompute CreatedWithAppStake after app top-up observed by getAppStake")
	require.Greater(t, maxStake2, maxStake1,
		"MaxStakeUpokt must grow proportionally with the new app stake (baseLimit formula is linear in stake)")
}
