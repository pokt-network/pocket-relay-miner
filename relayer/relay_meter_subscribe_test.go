//go:build test

package relayer

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// A cleanup subscription that fails when the meter starts (the store
// unreachable) is made again: a cleanup signal published later still clears
// the session's meter.
func TestRelayMeter_SubscribesAgainAfterAFailedSubscribe(t *testing.T) {
	db, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	inner := kv.NewPebble(zerolog.Nop(), db, redisutil.NewKeyBuilder(config.RedisNamespaceConfig{}))
	t.Cleanup(func() { _ = inner.Close() })
	store := kv.NewFailingSubscribes(inner, 1)

	meter := NewRelayMeter(
		zerolog.Nop(), store, &fakeAppClient{addr: "pokt1app"}, nil, &fakeSessionClient{numSuppliers: 1}, nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{NumBlocksPerSession: 10, ComputeUnitsToTokensMultiplier: 1, ComputeUnitCostGranularity: 1}},
		nil, staticServiceFactor{f: 1}, RelayMeterConfig{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, meter.Start(ctx))
	t.Cleanup(func() { _ = meter.Close() })

	select {
	case <-store.Subscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("never subscribed again after the failed subscribe")
	}
	consumed := meter.consumedKey("s1", "pokt1sup")
	require.NoError(t, store.Set(ctx, consumed, []byte("5"), time.Hour))
	require.NoError(t, store.Publish(ctx, store.KB().MeterCleanupChannel(), []byte("s1|pokt1sup")))

	require.Eventually(t, func() bool {
		_, err := store.Get(ctx, consumed)
		return err == kv.ErrNotFound
	}, 5*time.Second, 5*time.Millisecond)
}
