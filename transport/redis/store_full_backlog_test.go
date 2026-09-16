//go:build test

package redis

import (
	"context"
	"os"
	"strconv"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// TestStoreHealth_RealMaxmemoryWhatFreesAFullStore answers, against a REAL
// Redis at maxmemory, what reopens a store the miner stops reading while closed.
// A store filled by relay backlog stays closed: nothing the paused miner does
// frees a stream. A store filled by trees reopens once they are deleted, which
// is what a proof does. So a full store made of backlog reopens only when a
// proof frees enough tree memory, however far that proof is.
//
// It changes maxmemory: PRM_REAL_OOM=1 REDIS_TEST_URL=<disposable server>.
func TestStoreHealth_RealMaxmemoryWhatFreesAFullStore(t *testing.T) {
	if os.Getenv("PRM_REAL_OOM") != "1" {
		t.Skip("set PRM_REAL_OOM=1 and REDIS_TEST_URL to a disposable server: this changes maxmemory")
	}
	ctx := context.Background()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	require.NoError(t, client.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err())
	t.Cleanup(func() { _ = client.ConfigSet(context.Background(), "maxmemory", "0").Err() })

	usedMemory := func() uint64 {
		info, err := client.InfoMap(ctx, "memory").Result()
		require.NoError(t, err)
		n, err := strconv.ParseUint(info["Memory"]["used_memory"], 10, 64)
		require.NoError(t, err)
		return n
	}
	chunk := make([]byte, 256<<10)
	health := NewStoreHealth(zerolog.Nop(), client, "test_full_store")
	client.AddHook(health.Hook())

	// 32 MiB of stream backlog and 32 MiB of "tree", then a limit leaving
	// less free than the reserve (a tenth of maxmemory).
	for i := 0; i < 128; i++ {
		require.NoError(t, client.XAdd(ctx, &goredis.XAddArgs{Stream: prefix + ":relays:backlog", Values: map[string]any{"data": chunk}}).Err())
		require.NoError(t, client.HSet(ctx, prefix+":smst:tree:nodes", strconv.Itoa(i), chunk).Err())
	}
	used := usedMemory()
	maxmemory := used + used/20
	require.NoError(t, client.ConfigSet(ctx, "maxmemory", strconv.FormatUint(maxmemory, 10)).Err())
	health.poll(ctx)
	require.False(t, health.Operable(), "control: with less free than the reserve the store is closed")

	// The miner is paused: nothing touches the backlog. Sampling again changes nothing.
	health.poll(ctx)
	require.False(t, health.Operable(), "LINK backlog: a store full of backlog stays closed while nothing is consumed")

	// A proof deletes its tree (DEL is accepted when Redis refuses writes).
	require.NoError(t, client.Del(ctx, prefix+":smst:tree:nodes").Err())
	health.poll(ctx)
	require.True(t, health.Operable(), "LINK proof-frees: deleting the trees frees enough to reopen")
	require.Positive(t, client.XLen(ctx, prefix+":relays:backlog").Val(), "and the backlog is still there, to be consumed")
}
