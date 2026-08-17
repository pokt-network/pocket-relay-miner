//go:build test

package relayer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// TestRelayMeter_KeysFollowConfiguredNamespace proves the relay meter writes
// where the KeyBuilder says, under a namespace that is NOT the default.
//
// The meter used to build its keys with fmt.Sprintf and a prefix of its own
// (relay_meter.redis_key_prefix, defaulting to "ha"), while every reader --
// the miner's cleanup subscriber and the `redis meter` CLI -- derived theirs
// from the shared namespace config. With default settings the two happened to
// agree on the channel, which hid the split; the keys never agreed at all,
// because the writer emits {session}:{supplier}:meta and the reader addressed
// a bare {session}. Change base_prefix or meter_prefix and even the channel
// diverges, so the relayer publishes cleanup signals to nobody.
//
// This test pins the invariant that made that possible: writer and readers
// must derive from one namespace. It fails if a component reintroduces a
// prefix of its own.
func TestRelayMeter_KeysFollowConfiguredNamespace(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()

	// Deliberately non-default on BOTH segments the meter keys are built from.
	ns := config.RedisNamespaceConfig{
		BasePrefix:  "prod",
		MeterPrefix: "metering",
	}

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL:       fmt.Sprintf("redis://%s", mr.Addr()),
		Namespace: ns,
	})
	require.NoError(t, err)
	defer func() { _ = redisClient.Close() }()

	app := &fakeAppClient{addr: "pokt1app_ns"}
	app.stakeUpokt.Store(int64(1000))

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
	defer func() { _ = meter.Close() }()

	const (
		sessionID = "sess-ns"
		supplier  = "pokt1supplier_ns"
		serviceID = "svc-ns"
	)

	allowed, err := meter.CheckAndConsumeRelay(
		ctx, sessionID, app.addr, serviceID, supplier,
		1,  // sessionStartHeight
		10, // sessionEndHeight
		5,  // currentHeight
	)
	require.NoError(t, err)
	require.True(t, allowed, "the relay must be served for the meter to write anything")

	kb := redisClient.KB()

	// 1. The writer's keys are exactly the KeyBuilder's, under this namespace.
	metaKey := kb.MeterMetaKey(sessionID, supplier)
	require.Equal(t, "prod:metering:"+sessionID+":"+supplier+":meta", metaKey,
		"the meta key must follow the configured namespace, not a component prefix")
	require.True(t, mr.Exists(metaKey), "the meter must WRITE the key the KeyBuilder names: %s", metaKey)

	consumedKey := kb.MeterConsumedKey(sessionID, supplier)
	require.Equal(t, "prod:metering:"+sessionID+":"+supplier+":consumed", consumedKey)
	require.True(t, mr.Exists(consumedKey), "consumed counter missing at %s", consumedKey)

	// 2. Nothing was written under the old, self-prefixed shape. A pass here
	//    with "ha:..." present would mean the component prefix survived.
	for _, stale := range []string{
		"ha:meter:" + sessionID,
		"ha:meter:" + sessionID + ":" + supplier + ":meta",
		"prod:metering:" + sessionID,
	} {
		require.False(t, mr.Exists(stale),
			"nothing may be written at the pre-KeyBuilder key %s", stale)
	}

	// 3. The CLI's discovery pattern finds what the writer wrote. This is the
	//    reader half of the contract: `redis meter --session` scans this exact
	//    pattern, so a match here is the command working.
	keys := mr.Keys()
	pattern := kb.MeterSessionMetaPattern(sessionID)
	require.Equal(t, "prod:metering:"+sessionID+":*:meta", pattern)

	var matched []string
	for _, k := range keys {
		if ok, _ := filepath.Match(pattern, k); ok {
			matched = append(matched, k)
		}
	}
	require.Equal(t, []string{metaKey}, matched,
		"the CLI scan pattern must find the supplier's meter and nothing else")
}
