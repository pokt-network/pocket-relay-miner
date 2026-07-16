//go:build test

package relayer

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// newHealthTestMeter builds a RelayMeter backed by a fresh miniredis for the
// CheckRelayHealth probe tests. Returns the meter and the miniredis handle so
// tests can snapshot/inspect keys and simulate an outage.
func newHealthTestMeter(t *testing.T, ctx context.Context) (*RelayMeter, *miniredis.Miniredis, *redisutil.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)

	app := &fakeAppClient{addr: "pokt1app_health"}
	app.stakeUpokt.Store(1000)

	meter := NewRelayMeter(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		app,
		nil,
		&fakeSessionClient{numSuppliers: 1},
		nil,
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
	return meter, mr, redisClient
}

// TestCheckRelayHealth_NonMutating proves the probe resolves the service cost
// and confirms Redis reachability WITHOUT writing any key: the simulated-relay
// path must never leave meter state behind for its synthetic session.
func TestCheckRelayHealth_NonMutating(t *testing.T) {
	ctx := context.Background()
	meter, mr, redisClient := newHealthTestMeter(t, ctx)
	defer func() { _ = meter.Close() }()
	defer func() { _ = redisClient.Close() }()
	defer mr.Close()

	before := append([]string(nil), mr.Keys()...)

	require.NoError(t, meter.CheckRelayHealth(ctx, "svc-health"))

	after := mr.Keys()
	require.ElementsMatch(t, before, after,
		"CheckRelayHealth must not create/mutate any Redis key (before=%v after=%v)", before, after)
}

// TestCheckRelayHealth_RedisUnreachable proves the probe reports degradation
// when Redis is down, so a health check can surface "meter degraded".
func TestCheckRelayHealth_RedisUnreachable(t *testing.T) {
	ctx := context.Background()
	meter, mr, redisClient := newHealthTestMeter(t, ctx)
	defer func() { _ = meter.Close() }()
	defer func() { _ = redisClient.Close() }()

	// Prime the cost path once while Redis is up, then take Redis down so the
	// probe fails specifically on the Ping reachability check.
	require.NoError(t, meter.CheckRelayHealth(ctx, "svc-health"))
	mr.Close()

	err := meter.CheckRelayHealth(ctx, "svc-health")
	require.Error(t, err, "CheckRelayHealth must fail when Redis is unreachable")
}

// TestCheckRelayHealth_Closed proves the probe fails fast on a closed meter.
func TestCheckRelayHealth_Closed(t *testing.T) {
	ctx := context.Background()
	meter, mr, redisClient := newHealthTestMeter(t, ctx)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	require.NoError(t, meter.Close())
	require.Error(t, meter.CheckRelayHealth(ctx, "svc-health"))
}
