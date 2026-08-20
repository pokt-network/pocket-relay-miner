//go:build test

package testredis_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// These cover the two helpers that replace miniredis capabilities the real
// server does not offer. They are test infrastructure, so a silent failure
// here would make the suites that depend on them pass for the wrong reason --
// the exact fault the migration off miniredis exists to remove.

func TestKeys_ReturnsOnlyThePrefixSubtree(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	other := testredis.Prefix(t) // stands in for another package's namespace
	ctx := context.Background()

	require.NoError(t, client.Set(ctx, prefix+":a", "1", 0).Err())
	require.NoError(t, client.Set(ctx, prefix+":b", "1", 0).Err())
	require.NoError(t, client.Set(ctx, other+":a", "1", 0).Err())

	require.Equal(t, []string{prefix + ":a", prefix + ":b"},
		testredis.Keys(t, client, prefix),
		"Keys must not report a key outside the prefix it was given")
}

func TestKeys_EmptyPrefixSubtreeIsEmpty(t *testing.T) {
	client := testredis.Client(t)
	require.Empty(t, testredis.Keys(t, client, testredis.Prefix(t)))
}

func TestFailSwitch_FailsEveryCommandUntilCleared(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	ctx := context.Background()

	fs := testredis.NewFailSwitch(client)

	// Open: commands reach the server.
	require.NoError(t, client.Ping(ctx).Err())
	require.NoError(t, client.Set(ctx, prefix+":k", "v", 0).Err())

	fs.Fail("LOADING Redis is loading the dataset in memory")

	// The error must arrive on the COMMAND, not only as Process's return
	// value: go-redis discards that one and callers read cmd.Err().
	require.ErrorContains(t, client.Ping(ctx).Err(), "LOADING")
	require.ErrorContains(t, client.Set(ctx, prefix+":k2", "v", 0).Err(), "LOADING")
	require.ErrorContains(t, client.Get(ctx, prefix+":k").Err(), "LOADING")

	// Pipelines too, or a batched write would sail through an "outage" -- and
	// the error must reach EACH command, since Exec is the only thing go-redis
	// hands the hook chain's error to.
	pipe := client.Pipeline()
	set := pipe.Set(ctx, prefix+":k3", "v", 0)
	get := pipe.Get(ctx, prefix+":k")
	_, err := pipe.Exec(ctx)
	require.ErrorContains(t, err, "LOADING")
	require.ErrorContains(t, set.Err(), "LOADING",
		"a queued command must report the outage, not a nil error")
	require.ErrorContains(t, get.Err(), "LOADING")

	fs.Clear()

	require.NoError(t, client.Ping(ctx).Err())
	got, err := client.Get(ctx, prefix+":k").Result()
	require.NoError(t, err)
	require.Equal(t, "v", got, "the connection survives the outage; only commands failed")

	// Nothing written while the switch was closed may have landed.
	require.Equal(t, []string{prefix + ":k"}, testredis.Keys(t, client, prefix))
}
