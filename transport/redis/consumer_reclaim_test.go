//go:build test

package redis

import (
	"context"
	"fmt"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestConsume_ReclaimsDeadConsumerPending pins the recovery path for issue
// #25: a relay delivered to a consumer whose pod died before acking sat in
// that consumer's PEL forever, because the only reclaim call was gated
// behind XReadGroup returning redis.Nil -- which BLOCK 0 makes impossible on
// a real server. The reclaim now runs on its own ticker, so a stranded entry
// must reach the live consumer's channel within a few ClaimIdleTimeout
// periods regardless of what the blocking read is doing.
//
// The limitation that used to be written here is gone: this now runs against a
// real Redis, where the read genuinely blocks, so the ticker really is what
// delivers the stranded entry -- the old Nil-gated trigger could not fire at
// all. What it pins is the reclaim MECHANISM end to end: a dead consumer's
// pending entry, claimed by a different consumer name, parsed, delivered, and
// marked as a reclaim for dedup, which had zero coverage while the bug shipped.
func TestConsume_ReclaimsDeadConsumerPending(t *testing.T) {
	rdb := testredis.Client(t)

	// Shorten the idle block so Close() does not wait a full interval for the
	// read loop to come back and notice the cancel.
	restore := blockInterval
	blockInterval = 200 * time.Millisecond
	t.Cleanup(func() { blockInterval = restore })

	ctx := context.Background()

	const supplier = "pokt1reclaim_test"
	prefix := testredis.Prefix(t)
	group := prefix + ":ha-miners"
	stream := prefix + ":" + supplier

	// A relay published before this consumer existed.
	relay := &transport.MinedRelayMessage{
		RelayHash:               []byte{0xAA, 0xBB},
		RelayBytes:              []byte("payload"),
		ComputeUnitsPerRelay:    7,
		SessionId:               "sess-reclaim",
		SessionEndHeight:        200,
		SupplierOperatorAddress: supplier,
		ServiceId:               "develop-http",
		ApplicationAddress:      "pokt1app",
		ArrivalBlockHeight:      150,
		SessionStartHeight:      100,
	}
	buf, err := relay.Marshal()
	require.NoError(t, err)

	require.NoError(t, rdb.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	require.NoError(t, rdb.XAdd(ctx, &goredis.XAddArgs{
		Stream: stream, Values: map[string]any{"data": string(buf)},
	}).Err())

	// Deliver it to a consumer that then "dies": read without ever acking.
	res, err := rdb.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group: group, Consumer: "dead-pod-consumer",
		Streams: []string{stream, ">"}, Count: 1,
	}).Result()
	require.NoError(t, err)
	require.Len(t, res[0].Messages, 1, "the dead consumer must hold the entry in its PEL")

	// A fresh consumer (new pod, new name) starts afterwards.
	consumer, err := NewStreamsConsumer(
		logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "json"}),
		rdb,
		transport.ConsumerConfig{
			StreamPrefix:            prefix,
			SupplierOperatorAddress: supplier,
			ConsumerGroup:           group,
			ConsumerName:            "alive-pod-consumer",
			BatchSize:               10,
			ClaimIdleTimeout:        100, // ms; keeps the test fast
		},
	)
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ch := consumer.Consume(runCtx)
	// Close IS called now. It used to hang: the read parked on XREADGROUP with
	// BLOCK 0, which sets no read deadline at all, so cancelling the context
	// did not end it and wg.Wait sat there until a message happened to arrive.
	// The read blocks for a bounded interval since, so shutdown terminates.
	defer func() {
		done := make(chan error, 1)
		go func() { done <- consumer.Close() }()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Error("Close() did not return: the read loop cannot see a cancelled context")
		}
	}()

	select {
	case msg := <-ch:
		require.NotNil(t, msg.Message)
		require.Equal(t, "sess-reclaim", msg.Message.SessionId,
			"the reclaimed relay must be the stranded one")
		require.True(t, msg.IsReclaim,
			"a PEL recovery must be marked as a reclaim so downstream dedup runs")
	case <-runCtx.Done():
		t.Fatalf("stranded relay was never reclaimed: %v", fmt.Errorf("timeout"))
	}
}
