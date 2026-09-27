//go:build test

package relayer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// newHealthTestMeter builds a RelayMeter on its own namespace of the real
// Redis for the CheckRelayHealth probe tests. Returns the meter, that
// namespace's prefix (so a test can list what the probe wrote, and only that),
// and a switch that breaks every command to simulate an outage.
func newHealthTestMeter(t *testing.T, ctx context.Context) (*RelayMeter, string, *testredis.FailSwitch, *redisutil.Client) {
	t.Helper()
	redisClient, prefix := newTestRedis(t)
	failRedis := testredis.NewFailSwitch(redisClient)

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
		RelayMeterConfig{},
	)
	require.NoError(t, meter.Start(ctx))
	return meter, prefix, failRedis, redisClient
}

// TestCheckRelayHealth_NonMutating proves the probe resolves the service cost
// and confirms Redis reachability WITHOUT writing any key: the simulated-relay
// path must never leave meter state behind for its synthetic session.
func TestCheckRelayHealth_NonMutating(t *testing.T) {
	ctx := context.Background()
	meter, prefix, _, redisClient := newHealthTestMeter(t, ctx)
	defer func() { _ = meter.Close() }()

	// The service id carries a token unique to this run, so a key the probe
	// writes ANYWHERE on the server can be attributed to this test.
	serviceID := "svc-health-" + strings.ReplaceAll(prefix, ":", "_")

	before := testredis.Keys(t, redisClient, prefix)

	require.NoError(t, meter.CheckRelayHealth(ctx, serviceID))

	after := testredis.Keys(t, redisClient, prefix)
	require.ElementsMatch(t, before, after,
		"CheckRelayHealth must not create/mutate any key in its namespace (before=%v after=%v)", before, after)

	// The namespaced check above cannot see a write that ignores the
	// configured namespace -- a component prefixing "ha:" itself, which this
	// repository has already shipped twice. Look for the token across the
	// whole keyspace to catch that shape.
	//
	// What this still cannot see, stated rather than papered over: a stray
	// write whose key is entirely constant. On a shared server that is
	// indistinguishable from another package's traffic, and it is the static
	// key-literal check in internal/conventions that has to catch it.
	require.Empty(t, testredis.KeysMatching(t, redisClient, "*"+serviceID+"*"),
		"CheckRelayHealth must not write outside its configured namespace either")
}

// TestCheckRelayHealth_RedisUnreachable proves the probe reports degradation
// when Redis is down, so a health check can surface "meter degraded".
func TestCheckRelayHealth_RedisUnreachable(t *testing.T) {
	ctx := context.Background()
	meter, _, failRedis, _ := newHealthTestMeter(t, ctx)
	defer func() { _ = meter.Close() }()

	// Prime the cost path once while Redis is up, then break it so the probe
	// fails specifically on the Ping reachability check.
	require.NoError(t, meter.CheckRelayHealth(ctx, "svc-health"))
	// Break the commands, never the server: taking the server away frees its
	// port, another package's test binary can bind it mid-probe, and the Ping
	// then succeeds against a foreign Redis so this test passes for the wrong
	// reason. Doubly so now that the server is shared by every package.
	failRedis.Fail("LOADING Redis is loading the dataset in memory")

	err := meter.CheckRelayHealth(ctx, "svc-health")
	require.Error(t, err, "CheckRelayHealth must fail when Redis is unreachable")
}

// TestCheckRelayHealth_Closed proves the probe fails fast on a closed meter.
func TestCheckRelayHealth_Closed(t *testing.T) {
	ctx := context.Background()
	meter, _, _, _ := newHealthTestMeter(t, ctx)

	require.NoError(t, meter.Close())
	require.Error(t, meter.CheckRelayHealth(ctx, "svc-health"))
}
