//go:build test

package redis

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestReclaimSkipsOwnPendingEntries proves the reclaim does not steal back this
// consumer's OWN in-flight deliveries. XAUTOCLAIM filters on idle time alone —
// never on whether the owner is alive — so the previous implementation
// re-delivered anything the live consumer had held longer than
// ClaimIdleTimeout, turning a backlog into a duplicate storm.
func TestReclaimSkipsOwnPendingEntries(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	const stream, group, me, dead = "s", "g", "me", "dead-pod"

	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	for i := 0; i < 2; i++ {
		require.NoError(t, client.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]any{"f": "v"}}).Err())
	}

	// One delivery to a dead pod, one to us — neither acked.
	_, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: dead, Streams: []string{stream, ">"}, Count: 1,
	}).Result()
	require.NoError(t, err)
	mine, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: me, Streams: []string{stream, ">"}, Count: 1,
	}).Result()
	require.NoError(t, err)
	require.Len(t, mine[0].Messages, 1)
	myID := mine[0].Messages[0].ID

	// miniredis does not age PEL entries (FastForward leaves Idle at 0), so the
	// threshold is 0 here: this test pins the CONSUMER filter, which is the
	// defect. The time filter is Redis's own and was verified against a live
	// server — XAUTOCLAIM there reclaims the caller's own entries.

	c := &StreamsConsumer{
		client:     client,
		streamName: stream,
		config: transport.ConsumerConfig{
			ConsumerGroup:    group,
			ConsumerName:     me,
			ClaimIdleTimeout: 0,
		},
	}

	msgs, _, err := c.claimIdleFromOtherConsumers(ctx, "0-0")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "must reclaim exactly the dead pod's entry")
	require.NotEqual(t, myID, msgs[0].ID, "must NOT reclaim our own in-flight delivery")
}
