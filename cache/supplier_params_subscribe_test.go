//go:build test

package cache

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
)

// A subscription that fails when the cache starts (the store unreachable) is
// made again: an invalidation published later still clears the local copy.
func TestSupplierParamCache_SubscribesAgainAfterAFailedSubscribe(t *testing.T) {
	db, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	inner := kv.NewPebble(zerolog.Nop(), db, redisutil.NewKeyBuilder(config.RedisNamespaceConfig{}))
	t.Cleanup(func() { _ = inner.Close() })
	store := kv.NewFailingSubscribes(inner, 1)

	c := NewRedisSupplierParamCache(testLogger(), store, nil, CacheConfig{})
	c.localCacheMu.Lock()
	c.localCacheSet = true
	c.localCacheMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() { _ = c.Close() })

	select {
	case <-store.Subscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("never subscribed again after the failed subscribe")
	}
	require.NoError(t, store.Publish(ctx, store.KB().SupplierParamsInvalidateChannel(), []byte("x")))

	require.Eventually(t, func() bool {
		c.localCacheMu.Lock()
		defer c.localCacheMu.Unlock()
		return !c.localCacheSet
	}, 5*time.Second, 5*time.Millisecond)
}
