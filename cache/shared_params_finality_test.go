//go:build test

package cache

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// TestGetSharedParams_OnlyFinalAnswersReachL2: L2 is shared by every replica
// (Redis in high-availability mode, the embedded store in standalone mode). An
// answer the node gave from below the asked height can still change, so it must
// never be written there; a final one is.
func TestGetSharedParams_OnlyFinalAnswersReachL2(t *testing.T) {
	stores := map[string]func(t *testing.T) kv.Store{
		"redis": func(t *testing.T) kv.Store {
			return kv.NewRedis(zerolog.Nop(), newTestRedis(t))
		},
		"pebble": func(t *testing.T) kv.Store {
			db, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			s := kv.NewPebble(zerolog.Nop(), db, redisutil.NewKeyBuilder(config.RedisNamespaceConfig{}))
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
	for name, newStore := range stores {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				final   bool
				inStore bool
			}{
				{name: "not final", final: false, inStore: false},
				{name: "final", final: true, inStore: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					store := newStore(t)
					client := &recordingSharedQueryClient{
						liveParams: &sharedtypes.Params{NumBlocksPerSession: 10},
						notFinal:   !tc.final,
					}
					c := NewRedisSharedParamCache(logging.NewLoggerFromConfig(logging.DefaultConfig()),
						store, client, nil, CacheConfig{})
					t.Cleanup(func() { _ = c.Close() })

					const height = int64(500)
					_, err := c.GetSharedParams(context.Background(), height)
					require.NoError(t, err)

					_, err = store.Get(context.Background(), store.KB().ParamsSharedAtHeightKey(height))
					if tc.inStore {
						require.NoError(t, err, "a final answer must be shared through L2")
					} else {
						require.ErrorIs(t, err, kv.ErrNotFound, "an answer that is not final must never reach L2")
					}
				})
			}
		})
	}
}

// TestGetSharedParams_NotFinalAnswerIsReusedUntilTheNextBlock: an answer that is
// not final stays in this process's L1 for the rest of the block, so a lagging
// node costs one read per block, not one per relay; the next block asks again.
func TestGetSharedParams_NotFinalAnswerIsReusedUntilTheNextBlock(t *testing.T) {
	client := &recordingSharedQueryClient{
		liveParams: &sharedtypes.Params{NumBlocksPerSession: 10},
		notFinal:   true,
	}
	blocks := &stubBlockClient{height: 499}
	c := NewRedisSharedParamCache(logging.NewLoggerFromConfig(logging.DefaultConfig()),
		kv.NewRedis(zerolog.Nop(), newTestRedis(t)), client, blocks, CacheConfig{})
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	const height = int64(500)
	for i := 0; i < 10; i++ {
		_, err := c.GetSharedParams(ctx, height)
		require.NoError(t, err)
	}
	_, atHeight := client.snapshot()
	require.Len(t, atHeight, 1, "within one block the answer is reused")

	blocks.height = 500
	_, err := c.GetSharedParams(ctx, height)
	require.NoError(t, err)
	_, atHeight = client.snapshot()
	require.Len(t, atHeight, 2, "a new block asks the not-final answer again")
}

// TestGetSharedParams_NotFinalAnswerIsNotReusedPastItsBound: when no block
// arrives, an L1 answer that is not final must not become permanent.
func TestGetSharedParams_NotFinalAnswerIsNotReusedPastItsBound(t *testing.T) {
	client := &recordingSharedQueryClient{
		liveParams: &sharedtypes.Params{NumBlocksPerSession: 10},
		notFinal:   true,
	}
	c := NewRedisSharedParamCache(logging.NewLoggerFromConfig(logging.DefaultConfig()),
		kv.NewRedis(zerolog.Nop(), newTestRedis(t)), client, &stubBlockClient{height: 499}, CacheConfig{})
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	const height = int64(500)
	_, err := c.GetSharedParams(ctx, height)
	require.NoError(t, err)

	key := c.store.KB().ParamsSharedAtHeightKey(height)
	e, ok := c.localCache.Load(key)
	require.True(t, ok)
	e.cachedAt = time.Now().Add(-maxPendingLocalReuse)
	c.localCache.Store(key, e)

	_, err = c.GetSharedParams(ctx, height)
	require.NoError(t, err)
	_, atHeight := client.snapshot()
	require.Len(t, atHeight, 2, "a not-final answer past its bound is asked again with no new block")
}
