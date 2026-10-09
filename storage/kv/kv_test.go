//go:build test

package kv

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// The Store contract, run against Redis and the embedded store.

type harness struct {
	store Store
	// expire makes key's expiry pass now: Redis is told to expire it at once,
	// the embedded store's clock moves past it.
	expire func(key string)
}

func eachStore(t *testing.T, body func(t *testing.T, h harness)) {
	t.Run("redis", func(t *testing.T) {
		testredis.Client(t) // fail fast with testredis's own message
		client, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
			URL:       testredis.URL(),
			Namespace: config.RedisNamespaceConfig{BasePrefix: testredis.Prefix(t)},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		r := NewRedis(zerolog.Nop(), client)
		body(t, harness{store: r, expire: func(key string) {
			require.NoError(t, client.PExpire(context.Background(), key, time.Millisecond).Err())
			// Redis expires lazily on access; wait for the TTL to pass.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if n, _ := client.Exists(context.Background(), key).Result(); n == 0 {
					return
				}
			}
		}})
	})
	t.Run("pebble", func(t *testing.T) {
		store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })
		p := NewPebble(zerolog.Nop(), store, redisutil.NewKeyBuilder(config.RedisNamespaceConfig{}))
		t.Cleanup(func() { _ = p.Close() })
		now := time.Now()
		p.now = func() time.Time { return now }
		body(t, harness{store: p, expire: func(string) { now = now.Add(time.Hour * 24 * 365) }})
	})
}

func key(t *testing.T, h harness, name string) string {
	return h.store.KB().CacheKey("kvtest", t.Name()+":"+name)
}

func TestStore_GetSetDel(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		k := key(t, h, "a")
		_, err := h.store.Get(ctx, k)
		require.ErrorIs(t, err, ErrNotFound)

		require.NoError(t, h.store.Set(ctx, k, []byte("v1"), 0))
		got, err := h.store.Get(ctx, k)
		require.NoError(t, err)
		require.Equal(t, []byte("v1"), got)
		exists, err := h.store.Exists(ctx, k)
		require.NoError(t, err)
		require.True(t, exists)

		require.NoError(t, h.store.Del(ctx, k))
		_, err = h.store.Get(ctx, k)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestStore_MGetReturnsNilForMissingKeys(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		a, b := key(t, h, "a"), key(t, h, "b")
		require.NoError(t, h.store.Set(ctx, a, []byte("1"), 0))
		vals, err := h.store.MGet(ctx, a, b)
		require.NoError(t, err)
		require.Equal(t, [][]byte{[]byte("1"), nil}, vals)
	})
}

func TestStore_SetAllWritesEveryEntry(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		a, b := key(t, h, "a"), key(t, h, "b")
		require.NoError(t, h.store.SetAll(ctx, Entry{Key: a, Value: []byte("1")}, Entry{Key: b, Value: []byte("2"), TTL: time.Hour}))
		vals, err := h.store.MGet(ctx, a, b)
		require.NoError(t, err)
		require.Equal(t, [][]byte{[]byte("1"), []byte("2")}, vals)
	})
}

func TestStore_AnExpiredKeyIsGone(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		k := key(t, h, "ttl")
		require.NoError(t, h.store.Set(ctx, k, []byte("v"), time.Hour))
		h.expire(k)
		_, err := h.store.Get(ctx, k)
		require.ErrorIs(t, err, ErrNotFound)
		exists, err := h.store.Exists(ctx, k)
		require.NoError(t, err)
		require.False(t, exists)
	})
}

func TestStore_SetNXAndCompareAndDeleteAreALock(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		k := key(t, h, "lock")
		got, err := h.store.SetNX(ctx, k, []byte("me"), time.Hour)
		require.NoError(t, err)
		require.True(t, got)
		got, err = h.store.SetNX(ctx, k, []byte("you"), time.Hour)
		require.NoError(t, err)
		require.False(t, got, "held")

		released, err := h.store.CompareAndDelete(ctx, k, []byte("you"))
		require.NoError(t, err)
		require.False(t, released, "only the holder releases")
		released, err = h.store.CompareAndDelete(ctx, k, []byte("me"))
		require.NoError(t, err)
		require.True(t, released)

		got, err = h.store.SetNX(ctx, k, []byte("you"), time.Hour)
		require.NoError(t, err)
		require.True(t, got, "free again")
	})
}

func TestStore_SetKeepTTLKeepsTheExpiry(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		k := key(t, h, "keep")
		require.NoError(t, h.store.Set(ctx, k, []byte("v1"), time.Hour))
		require.NoError(t, h.store.SetKeepTTL(ctx, k, []byte("v2")))
		got, err := h.store.Get(ctx, k)
		require.NoError(t, err)
		require.Equal(t, []byte("v2"), got)
		h.expire(k)
		_, err = h.store.Get(ctx, k)
		require.ErrorIs(t, err, ErrNotFound, "the expiry survived the rewrite")
	})
}

func TestStore_Sets(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		k := key(t, h, "set")
		require.NoError(t, h.store.SAdd(ctx, k, "a", "b", "c"))
		require.NoError(t, h.store.SAdd(ctx, k, "a"))
		require.NoError(t, h.store.SRem(ctx, k, "b"))

		members, err := h.store.SMembers(ctx, k)
		require.NoError(t, err)
		sort.Strings(members)
		require.Equal(t, []string{"a", "c"}, members)
		n, err := h.store.SCard(ctx, k)
		require.NoError(t, err)
		require.Equal(t, int64(2), n)
		in, err := h.store.SIsMember(ctx, k, "c")
		require.NoError(t, err)
		require.True(t, in)
		in, err = h.store.SIsMember(ctx, k, "b")
		require.NoError(t, err)
		require.False(t, in)

		ok, err := h.store.Expire(ctx, k, time.Hour)
		require.NoError(t, err)
		require.True(t, ok)
		h.expire(k)
		members, err = h.store.SMembers(ctx, k)
		require.NoError(t, err)
		require.Empty(t, members, "an expired set is empty")

		require.NoError(t, h.store.SAdd(ctx, k, "z"))
		members, err = h.store.SMembers(ctx, k)
		require.NoError(t, err)
		require.Equal(t, []string{"z"}, members, "a set added to after expiring starts empty")
		require.NoError(t, h.store.Del(ctx, k))
		n, err = h.store.SCard(ctx, k)
		require.NoError(t, err)
		require.Zero(t, n)
	})
}

func TestStore_ScanPrefixFindsStringsAndSets(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		prefix := key(t, h, "scan:")
		require.NoError(t, h.store.Set(ctx, prefix+"s1", []byte("x"), 0))
		require.NoError(t, h.store.SAdd(ctx, prefix+"set1", "m"))
		require.NoError(t, h.store.Set(ctx, key(t, h, "other"), []byte("x"), 0))

		keys, err := h.store.ScanPrefix(ctx, prefix)

		require.NoError(t, err)
		sort.Strings(keys)
		require.Equal(t, []string{prefix + "s1", prefix + "set1"}, keys)
	})
}

func TestStore_PublishReachesASubscriberOpenedBefore(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		ch := h.store.KB().EventChannel("kvtest", t.Name())
		sub, err := h.store.Subscribe(ctx, ch)
		require.NoError(t, err)

		require.NoError(t, h.store.Publish(ctx, ch, []byte("hello")))

		select {
		case msg := <-sub.Messages():
			require.Equal(t, Message{Channel: ch, Payload: "hello"}, msg)
		case <-time.After(5 * time.Second):
			t.Fatal("no message")
		}
		require.NoError(t, sub.Close())
		require.NoError(t, sub.Close(), "idempotent")
	})
}

// A set whose last member is removed no longer exists, as Redis deletes an
// empty set: Exists and ScanPrefix do not find it.
func TestStore_ASetEmptiedBySRemIsGone(t *testing.T) {
	eachStore(t, func(t *testing.T, h harness) {
		ctx := context.Background()
		prefix := key(t, h, "empty:")
		k := prefix + "set"
		require.NoError(t, h.store.SAdd(ctx, k, "a", "b"))
		require.NoError(t, h.store.SRem(ctx, k, "a"))
		exists, err := h.store.Exists(ctx, k)
		require.NoError(t, err)
		require.True(t, exists, "control: a set with a member left exists")

		require.NoError(t, h.store.SRem(ctx, k, "b"))

		exists, err = h.store.Exists(ctx, k)
		require.NoError(t, err)
		require.False(t, exists)
		keys, err := h.store.ScanPrefix(ctx, prefix)
		require.NoError(t, err)
		require.Empty(t, keys)
	})
}

func newTestPebble(t *testing.T) *Pebble {
	t.Helper()
	store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	p := NewPebble(zerolog.Nop(), store, redisutil.NewKeyBuilder(config.RedisNamespaceConfig{}))
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// SRem decides whether the set is left empty from its first member, not by
// listing them all: it runs under the store's lock, and the relay meter's set
// of active sessions holds one member per session and supplier.
func TestPebble_SRemDoesNotListTheSet(t *testing.T) {
	p := newTestPebble(t)
	ctx := context.Background()
	members := make([]string, 20000)
	for i := range members {
		members[i] = fmt.Sprintf("m-%05d", i)
	}
	require.NoError(t, p.SAdd(ctx, "big", members...))

	allocs := testing.AllocsPerRun(10, func() {
		if err := p.SRem(ctx, "big", "m-00000"); err != nil {
			t.Fatal(err)
		}
		if err := p.SAdd(ctx, "big", "m-00000"); err != nil {
			t.Fatal(err)
		}
	})
	require.Less(t, allocs, float64(len(members)/10), "SRem allocated per member of the set")

	require.NoError(t, p.SRem(ctx, "big", members[1:]...))
	n, err := p.SCard(ctx, "big")
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "a set with a member left is kept")
	require.NoError(t, p.SRem(ctx, "big", "m-00000"))
	n, err = p.SCard(ctx, "big")
	require.NoError(t, err)
	require.Zero(t, n, "a set left empty is deleted")
}

// The janitor finds expired keys without the store's lock and deletes them
// under it, re-reading each first: a key written again in between is kept.
func TestPebble_TheJanitorKeepsAKeyWrittenAfterItsScan(t *testing.T) {
	p := newTestPebble(t)
	ctx := context.Background()
	now := time.Now()
	p.now = func() time.Time { return now }
	require.NoError(t, p.Set(ctx, "rewritten", []byte("old"), time.Second))
	require.NoError(t, p.Set(ctx, "expired", []byte("gone"), time.Second))
	require.NoError(t, p.SAdd(ctx, "set-rewritten", "a"))
	require.NoError(t, p.SAdd(ctx, "set-expired", "a"))
	ok, err := p.Expire(ctx, "set-rewritten", time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = p.Expire(ctx, "set-expired", time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	now = now.Add(time.Minute)

	p.beforeExpiredDelete = func() {
		require.NoError(t, p.Set(ctx, "rewritten", []byte("new"), 0))
		require.NoError(t, p.SAdd(ctx, "set-rewritten", "b"))
	}
	require.NoError(t, p.deleteExpired())

	got, err := p.Get(ctx, "rewritten")
	require.NoError(t, err)
	require.Equal(t, []byte("new"), got, "a key written after the scan was deleted")
	_, err = p.Get(ctx, "expired")
	require.ErrorIs(t, err, ErrNotFound)
	members, err := p.SMembers(ctx, "set-rewritten")
	require.NoError(t, err)
	require.Equal(t, []string{"b"}, members, "a set revived after the scan keeps its new member")
	n, err := p.SCard(ctx, "set-expired")
	require.NoError(t, err)
	require.Zero(t, n)
}
