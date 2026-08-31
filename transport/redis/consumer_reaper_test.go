//go:build test

package redis

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// reaperFixture builds a group with three consumers:
//
//	me        - this consumer, holding one unacked delivery
//	deadEmpty - a consumer that read and acked, so its PEL is empty
//	deadBusy  - a consumer that read and did NOT ack, so its PEL is not empty
//
// Idle time is controlled through ClaimIdleTimeout rather than by waiting:
// reapDeadConsumers reaps when idle >= reapIdleMultiplier*ClaimIdleTimeout, so a
// timeout of 0 makes every consumer old enough and a large timeout makes none of
// them old enough. Nothing sleeps, so the test is deterministic.
type reaperFixture struct {
	client        redis.UniversalClient
	stream, group string
}

const (
	reaperSelf      = "me"
	reaperDeadEmpty = "dead-empty"
	reaperDeadBusy  = "dead-busy"
)

func newReaperFixture(t *testing.T) *reaperFixture {
	t.Helper()
	client := testredis.Client(t)
	ctx := context.Background()
	prefix := testredis.Prefix(t)
	stream, group := prefix+":stream", prefix+":group"

	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	for i := 0; i < 3; i++ {
		require.NoError(t, client.XAdd(ctx, &redis.XAddArgs{
			Stream: stream, Values: map[string]any{"f": "v"},
		}).Err())
	}

	read := func(consumer string) string {
		res, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: group, Consumer: consumer, Streams: []string{stream, ">"}, Count: 1,
		}).Result()
		require.NoError(t, err)
		require.Len(t, res[0].Messages, 1)
		return res[0].Messages[0].ID
	}

	emptyID := read(reaperDeadEmpty)
	require.NoError(t, client.XAck(ctx, stream, group, emptyID).Err())
	read(reaperDeadBusy) // deliberately unacked
	read(reaperSelf)     // deliberately unacked

	return &reaperFixture{client: client, stream: stream, group: group}
}

func (f *reaperFixture) consumer(idleTimeoutMs int64) *StreamsConsumer {
	return &StreamsConsumer{
		logger:     zerolog.Nop(),
		client:     f.client,
		streamName: f.stream,
		config: transport.ConsumerConfig{
			ConsumerGroup:           f.group,
			ConsumerName:            reaperSelf,
			ClaimIdleTimeout:        idleTimeoutMs,
			SupplierOperatorAddress: "pokt1supplier_reaper",
		},
	}
}

func (f *reaperFixture) names(t *testing.T) map[string]int64 {
	t.Helper()
	consumers, err := f.client.XInfoConsumers(context.Background(), f.stream, f.group).Result()
	require.NoError(t, err)
	out := make(map[string]int64, len(consumers))
	for _, c := range consumers {
		out[c.Name] = c.Pending
	}
	return out
}

// TestReapRemovesOnlyTheEmptyDeadConsumer is the core guard.
//
// XGROUP DELCONSUMER DISCARDS the pending entries of the consumer it deletes, so
// reaping a consumer that still owns deliveries destroys relays outright — worse
// than the registry growth the reaper exists to stop. The reaper must therefore
// touch only a consumer it has just observed with an empty PEL, and never
// itself.
func TestReapRemovesOnlyTheEmptyDeadConsumer(t *testing.T) {
	f := newReaperFixture(t)

	before := f.names(t)
	require.Contains(t, before, reaperDeadEmpty)
	require.Contains(t, before, reaperDeadBusy)
	require.Contains(t, before, reaperSelf)
	require.Equal(t, int64(0), before[reaperDeadEmpty], "premise: the empty one holds nothing")
	require.Equal(t, int64(1), before[reaperDeadBusy], "premise: the busy one holds a delivery")

	// ClaimIdleTimeout 0 => reap threshold 0 => every consumer is old enough,
	// so the ONLY thing that can save deadBusy and me is the guard under test.
	f.consumer(0).reapDeadConsumers(context.Background())

	after := f.names(t)
	require.NotContains(t, after, reaperDeadEmpty,
		"a dead consumer with an empty pending list is exactly what the reaper exists to remove")
	require.Contains(t, after, reaperDeadBusy,
		"a consumer still holding a pending entry must NEVER be deleted: XGROUP DELCONSUMER "+
			"discards that entry, which is a relay lost")
	require.Contains(t, after, reaperSelf,
		"the reaper must never delete the consumer that is running it")
}

// TestReapKeepsPendingEntriesIntact checks the consequence rather than the
// action: whatever the reaper did, no pending entry may have disappeared.
//
// Asserting only on the consumer list would pass if a future change deleted
// deadBusy and its entry together, since the list would then simply be shorter.
func TestReapKeepsPendingEntriesIntact(t *testing.T) {
	f := newReaperFixture(t)
	ctx := context.Background()

	pendingBefore, err := f.client.XPending(ctx, f.stream, f.group).Result()
	require.NoError(t, err)
	require.Equal(t, int64(2), pendingBefore.Count, "premise: deadBusy and me each hold one")

	f.consumer(0).reapDeadConsumers(ctx)

	pendingAfter, err := f.client.XPending(ctx, f.stream, f.group).Result()
	require.NoError(t, err)
	require.Equal(t, pendingBefore.Count, pendingAfter.Count,
		"reaping must not change the number of pending entries; any drop is acknowledged relays "+
			"that no longer exist anywhere")
}

// TestReapSparesConsumersUnderIdleThreshold proves the idle condition is real.
//
// Without it the reaper would delete a consumer that is merely slow the instant
// its pending list happened to be empty — a live pod between two reads.
func TestReapSparesConsumersUnderIdleThreshold(t *testing.T) {
	f := newReaperFixture(t)

	// One hour of required idleness: nothing in this test is that old.
	f.consumer(3600_000).reapDeadConsumers(context.Background())

	after := f.names(t)
	require.Contains(t, after, reaperDeadEmpty,
		"an empty consumer that has NOT been silent long enough must be left alone: it may be a "+
			"live pod between two reads")
	require.Contains(t, after, reaperDeadBusy)
	require.Contains(t, after, reaperSelf)
}

// TestReapOnMissingStreamIsQuiet covers the path a supplier takes before its
// stream exists: the reaper runs on the same ticker as the reclaim, so it fires
// against streams that have never received a relay.
func TestReapOnMissingStreamIsQuiet(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)

	c := &StreamsConsumer{
		logger:     zerolog.Nop(),
		client:     client,
		streamName: prefix + ":absent",
		config: transport.ConsumerConfig{
			ConsumerGroup:           prefix + ":group",
			ConsumerName:            reaperSelf,
			ClaimIdleTimeout:        0,
			SupplierOperatorAddress: "pokt1supplier_absent",
		},
	}

	require.NotPanics(t, func() { c.reapDeadConsumers(context.Background()) })
}
