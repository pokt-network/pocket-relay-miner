//go:build test

package redis

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestReclaimSkipsOwnPendingEntries proves the reclaim does not steal back this
// consumer's OWN in-flight deliveries. XAUTOCLAIM filters on idle time alone —
// never on whether the owner is alive — so the previous implementation
// re-delivered anything the live consumer had held longer than
// ClaimIdleTimeout, turning a backlog into a duplicate storm.
func TestReclaimSkipsOwnPendingEntries(t *testing.T) {
	client := testredis.Client(t)

	ctx := context.Background()
	prefix := testredis.Prefix(t)
	stream, group := prefix+":stream", prefix+":group"
	const me, dead = "me", "dead-pod"

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

	newConsumer := func(idleTimeoutMs int64) *StreamsConsumer {
		return &StreamsConsumer{
			client:     client,
			streamName: stream,
			config: transport.ConsumerConfig{
				ConsumerGroup:    group,
				ConsumerName:     me,
				ClaimIdleTimeout: idleTimeoutMs,
			},
		}
	}

	// The CONSUMER filter, which is the defect: with no age threshold both
	// entries are old enough, so only the owner can tell them apart.
	msgs, _, err := newConsumer(0).claimIdleFromOtherConsumers(ctx, "0-0")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "must reclaim exactly the dead pod's entry")
	require.NotEqual(t, myID, msgs[0].ID, "must NOT reclaim our own in-flight delivery")

	// The AGE filter, which only a real server can show: miniredis leaves PEL
	// entries at Idle 0 whatever FastForward does, so this half used to be
	// asserted by hand against a live Redis and written down in a comment.
	// Both deliveries were made moments ago, so a minute-long threshold must
	// hand over nothing at all.
	fresh, _, err := newConsumer(60_000).claimIdleFromOtherConsumers(ctx, "0-0")
	require.NoError(t, err)
	require.Empty(t, fresh, "entries younger than ClaimIdleTimeout must not be reclaimed")
}
