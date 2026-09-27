//go:build test

package redis

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestConsume_ReclaimsDeadConsumerPending pins the recovery path for issue
// #25: a relay delivered to a consumer whose pod died before acking sat in
// that consumer's PEL forever, because the only XAUTOCLAIM call was gated
// behind XReadGroup returning redis.Nil -- which BLOCK 0 makes impossible on
// a real server. The reclaim now runs on its own ticker, so a stranded entry
// must reach the live consumer's channel within a few ClaimIdleTimeout
// periods regardless of what the blocking read is doing.
//
// Honest limitation: miniredis does not block on XREADGROUP (it returns Nil
// immediately), so this test cannot distinguish the ticker trigger from the
// old Nil-gated trigger. What it does pin is the reclaim MECHANISM end to
// end -- dead consumer's pending entry, claimed by a different consumer name,
// parsed, delivered, and marked as a reclaim for dedup -- which had zero
// coverage while the bug shipped. The trigger topology is pinned by comment
// and review on the ticker itself; the live gate exercises the real blocking
// path.
func TestConsume_ReclaimsDeadConsumerPending(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	const (
		supplier = "pokt1reclaim_test"
		group    = "ha-miners"
		stream   = "ha:relays:" + supplier
	)

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
			StreamPrefix:            "ha:relays",
			SupplierOperatorAddress: supplier,
			ConsumerGroup:           group,
			ConsumerName:            "alive-pod-consumer",
			BatchSize:               10,
			ClaimIdleTimeout:        100, // ms; keeps the test fast
		},
		0,
	)
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ch := consumer.Consume(runCtx)
	// Deliberately NOT calling Close: it wg.Waits for the read goroutine, and a
	// read blocked in XREADGROUP BLOCK 0 is not interrupted by context
	// cancellation, so Close hangs until a message arrives. Pre-existing
	// shutdown behavior (pods are killed, so production never waits); noted for
	// PR A2. The cancel above bounds every goroutine this test starts except
	// that blocked read, which dies with the test process.

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
