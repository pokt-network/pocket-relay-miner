//go:build test

package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Every distributed cache lock in this package is taken on a REQUEST context
// and released in a defer. When the client disconnects or the request times
// out, that deferred release inherits an already-cancelled context, the DEL
// never reaches Redis, and the lock sits there until its TTL expires -- during
// which every other instance asking for the same key takes the contended path
// and can fire the duplicate chain query the lock exists to prevent.
//
// A cancelled request is the normal case under load, not the rare one.
//
// Against a real Redis, because the whole assertion is about whether a command
// with a cancelled context reaches the server.

// newLockTestClient returns a client whose whole keyspace is namespaced to this
// test: the server is shared with the packages running alongside, so a lock key
// must not be guessable by another one.
func newLockTestClient(t *testing.T) *redisutil.Client {
	t.Helper()
	testredis.Client(t) // fail fast, with the "start one with..." message
	client, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL:       testredis.URL(),
		Namespace: config.RedisNamespaceConfig{BasePrefix: testredis.Prefix(t)},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestReleaseCacheLock_ReleasesEvenWhenTheRequestWasCancelled(t *testing.T) {
	client := newLockTestClient(t)
	lockKey := client.KB().CacheLockKey("application", "pokt1cancelled")

	ctx, cancel := context.WithCancel(context.Background())
	token := newLockToken()
	ok, err := client.SetNX(ctx, lockKey, token, 5*time.Second).Result()
	require.NoError(t, err)
	require.True(t, ok, "precondition: this instance holds the lock")

	// The request dies before the deferred release runs, which is what a
	// disconnecting client or an expired deadline does.
	cancel()

	releaseCacheLock(ctx, client, lockKey, token)

	exists, err := client.Exists(context.Background(), lockKey).Result()
	require.NoError(t, err)
	require.Zero(t, exists,
		"the lock outlived its holder: every other instance now takes the contended path until the TTL expires")
}

func TestReleaseCacheLock_ReleasesOnALiveContext(t *testing.T) {
	client := newLockTestClient(t)
	lockKey := client.KB().CacheLockKey("service", "svc-live")

	ctx := context.Background()
	token := newLockToken()
	ok, err := client.SetNX(ctx, lockKey, token, 5*time.Second).Result()
	require.NoError(t, err)
	require.True(t, ok)

	releaseCacheLock(ctx, client, lockKey, token)

	exists, err := client.Exists(ctx, lockKey).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

// The release runs on a context detached from the request precisely so it
// survives cancellation -- which means a request whose chain query outran the
// lock TTL now DOES reach Redis on its way out, where before it failed and left
// the successor alone. Without an ownership check that turns one duplicate
// query into another: the straggler frees a lock a DIFFERENT instance is
// holding, and a third instance walks in.
func TestReleaseCacheLock_LeavesASuccessorsLockAlone(t *testing.T) {
	client := newLockTestClient(t)
	lockKey := client.KB().CacheLockKey("application", "pokt1succession")
	ctx := context.Background()

	// The straggler acquired, then its lock expired.
	stragglerToken := newLockToken()

	// A different instance acquired afterwards and is holding it now.
	successorToken := newLockToken()
	ok, err := client.SetNX(ctx, lockKey, successorToken, 5*time.Second).Result()
	require.NoError(t, err)
	require.True(t, ok)

	// The straggler's deferred release finally runs.
	releaseCacheLock(ctx, client, lockKey, stragglerToken)

	got, err := client.Get(ctx, lockKey).Result()
	require.NoError(t, err, "the successor's lock must still be there")
	require.Equal(t, successorToken, got,
		"the straggler freed a lock it no longer owned, so a third instance can now fire the duplicate query")
}
