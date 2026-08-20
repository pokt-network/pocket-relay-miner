//go:build test

package cache

import (
	"context"
	"testing"

	"github.com/cosmos/gogoproto/proto"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// These tests pin the contended-loser behavior of the two param singletons
// that keep a hand-rolled copy of the query-lock pattern (the shared helper
// in keyed_query_lock.go already releases conditionally): an instance that
// LOSES the SetNX race must NOT delete the winner's still-held lock on exit.
// An unconditional deferred Del re-opened the thundering herd the lock
// exists to prevent: loser exits, deletes the winner's lock, and a third
// instance acquires immediately and fires a duplicate chain query.

func TestSharedParamsSingleton_ContendedLoserKeepsWinnersLock(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	// The winner holds the lock.
	lockKey := client.KB().ParamsSharedLockKey()
	require.NoError(t, client.Set(ctx, lockKey, "winner", 0).Err())

	// L2 already has params, so the loser's retry path returns without a
	// chain query (queryClient stays nil on purpose: reaching it would panic,
	// which doubles as an assertion that the loser never queries the chain).
	params := sharedtypes.DefaultParams()
	data, err := proto.Marshal(&params)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, client.KB().ParamsSharedCacheKey(), data, 0).Err())

	c := &sharedParamsCache{
		logger:      testLogger(),
		redisClient: client,
	}

	got, err := c.queryChainWithLock(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)

	exists, err := client.Exists(ctx, lockKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists,
		"the contended loser must NOT delete the winner's still-held lock")
}

func TestProofParamsSingleton_ContendedLoserKeepsWinnersLock(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	lockKey := client.KB().ParamsProofLockKey()
	require.NoError(t, client.Set(ctx, lockKey, "winner", 0).Err())

	params := prooftypes.DefaultParams()
	data, err := proto.Marshal(&params)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, client.KB().ParamsProofKey(), data, 0).Err())

	c := &proofParamsCache{
		logger:      testLogger(),
		redisClient: client,
	}

	got, err := c.queryChainWithLock(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)

	exists, err := client.Exists(ctx, lockKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists,
		"the contended loser must NOT delete the winner's still-held lock")
}
