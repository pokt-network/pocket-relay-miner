//go:build test

package relayer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
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
	testredis.Client(t) // fail fast with the "start one with ..." message
	ctx := context.Background()

	// The base is deliberately non-default. meter_prefix is set too, and it must
	// have NO effect: it is a retired knob, so the assertions below prove the
	// keys follow the base plus the fixed "meter" segment, and that nothing
	// landed where the retired value would have put it. (A config that sets it
	// is rejected at startup by RedisNamespaceConfig.Validate; this test builds
	// the client directly, which is what lets it observe the key layout.)
	// The base carries this test's isolation prefix as well: the server is
	// shared, and a bare "prod" would collide with any other test that picked
	// the same obvious placeholder.
	prefix := testredis.Prefix(t)
	base := prefix + ":prod"
	ns := config.RedisNamespaceConfig{
		BasePrefix:  base,
		MeterPrefix: "metering",
	}

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL:       testredis.URL(),
		Namespace: ns,
	})
	require.NoError(t, err)
	defer func() { _ = redisClient.Close() }()

	// The session id is unique to this run so that the "nothing was written
	// under the pre-KeyBuilder shape" check below stays valid on a shared
	// server: ha:meter:{sessionID} is a key only this test could have created.
	sessionID := "sess-ns-" + strings.ReplaceAll(prefix, ":", "_")

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
	require.Equal(t, base+":meter:"+sessionID+":"+supplier+":meta", metaKey,
		"the meta key must follow the configured BASE, and the fixed meter segment: "+
			"the per-family prefix is no longer configurable, so a config that sets it "+
			"cannot move these keys (it is rejected at startup instead)")
	requireKeyExists(t, redisClient, metaKey, "the meter must WRITE the key the KeyBuilder names: %s", metaKey)

	consumedKey := kb.MeterConsumedKey(sessionID, supplier)
	require.Equal(t, base+":meter:"+sessionID+":"+supplier+":consumed", consumedKey)
	requireKeyExists(t, redisClient, consumedKey, "consumed counter missing at %s", consumedKey)

	// The CLI contract: both keys are plain STRINGS -- the meta a JSON blob,
	// the consumed a counter. The first version of `redis meter` read the meta
	// with HGETALL and failed with WRONGTYPE against real data, which no
	// existence check catches. Pin the readable shape, not just the address.
	rawMeta, err := redisClient.Get(ctx, metaKey).Result()
	require.NoError(t, err, "the meta key must be readable as a string (GET)")
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(rawMeta), &meta),
		"the meta value must be the SessionMeterMeta JSON")
	require.Equal(t, supplier, meta["supplier_address"])
	rawConsumed, err := redisClient.Get(ctx, consumedKey).Result()
	require.NoError(t, err, "the consumed key must be readable as a string (GET)")
	require.Regexp(t, `^[0-9]+$`, rawConsumed, "consumed must be a plain integer counter")

	// 2. Nothing was written under the old, self-prefixed shape. A pass here
	//    with "ha:..." present would mean the component prefix survived.
	for _, stale := range []string{
		"ha:meter:" + sessionID,
		"ha:meter:" + sessionID + ":" + supplier + ":meta",
		base + ":metering:" + sessionID,
	} {
		n, err := redisClient.Exists(ctx, stale).Result()
		require.NoError(t, err)
		require.Zero(t, n, "nothing may be written at the pre-KeyBuilder key %s", stale)
	}

	// 3. The CLI's discovery pattern finds what the writer wrote. This is the
	//    reader half of the contract: `redis meter --session` scans this exact
	//    pattern, so a match here is the command working.
	keys := testredis.Keys(t, redisClient, prefix)
	pattern := kb.MeterSessionMetaPattern(sessionID)
	require.Equal(t, base+":meter:"+sessionID+":*:meta", pattern)

	var matched []string
	for _, k := range keys {
		if ok, _ := filepath.Match(pattern, k); ok {
			matched = append(matched, k)
		}
	}
	require.Equal(t, []string{metaKey}, matched,
		"the CLI scan pattern must find the supplier's meter and nothing else")
}

// requireKeyExists fails unless key is present, and says which key.
func requireKeyExists(t *testing.T, client *redisutil.Client, key, msg string, args ...any) {
	t.Helper()
	n, err := client.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	require.Equalf(t, int64(1), n, msg, args...)
}
