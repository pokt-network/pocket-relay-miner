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

// strandInPEL publishes n relays to the stream and delivers them to a
// consumer name that never acks, leaving them stranded in that consumer's
// PEL — the state a crashed pod leaves behind.
func strandInPEL(t *testing.T, rdb *goredis.Client, stream, group, deadConsumer string, n int) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, rdb.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	for i := 0; i < n; i++ {
		relay := &transport.MinedRelayMessage{
			RelayHash:               []byte(fmt.Sprintf("hash-%04d", i)),
			RelayBytes:              []byte("payload"),
			ComputeUnitsPerRelay:    1,
			SessionId:               "sess-drain",
			SessionEndHeight:        200,
			SupplierOperatorAddress: "pokt1drain_test",
			ServiceId:               "develop-http",
			ApplicationAddress:      "pokt1app",
			SessionStartHeight:      100,
		}
		buf, err := relay.Marshal()
		require.NoError(t, err)
		require.NoError(t, rdb.XAdd(ctx, &goredis.XAddArgs{
			Stream: stream, Values: map[string]any{"data": string(buf)},
		}).Err())
	}

	res, err := rdb.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group: group, Consumer: deadConsumer,
		Streams: []string{stream, ">"}, Count: int64(n),
	}).Result()
	require.NoError(t, err)
	require.Len(t, res[0].Messages, n, "the dead consumer must hold all entries in its PEL")

	ageOutPEL(t, rdb, stream, group, deadConsumer, res[0].Messages)
}

// ageOutPEL makes the stranded entries look old, deterministically.
//
// A reclaim only hands over entries idle past its threshold, so a test about
// WHICH entries move has to control their age. Sleeping past a 1ms threshold
// is how this used to be done, and that is a race dressed as a setup step. A
// real server takes XCLAIM's IDLE option, which sets the idle time outright --
// the entries stay with the dead consumer (JUSTID, same owner) and simply
// become an hour old. miniredis has no equivalent; this is one of the reasons
// the fake could not carry these tests.
func ageOutPEL(t *testing.T, rdb *goredis.Client, stream, group, owner string, msgs []goredis.XMessage) {
	t.Helper()
	args := []any{"XCLAIM", stream, group, owner, 0}
	for _, m := range msgs {
		args = append(args, m.ID)
	}
	args = append(args, "IDLE", 3_600_000, "JUSTID")
	require.NoError(t, rdb.Do(context.Background(), args...).Err())
}

// TestClaimPendingMessages_DrainsWholePEL pins that ONE reclaim invocation
// recovers the ENTIRE eligible PEL, not just the first pending page
// (XPENDING Count=50). A dead consumer can strand thousands of deliveries (a full
// read batch plus the channel buffer); recovering them at one page per tick
// would drain slower than the claim window closes, silently losing the tail
// — the exact issue-#25 loss the reclaim exists to prevent.
func TestClaimPendingMessages_DrainsWholePEL(t *testing.T) {
	rdb := testredis.Client(t)

	const (
		supplier = "pokt1drain_test"
		stranded = 120 // needs 3 pending pages at XPENDING Count=50
	)
	prefix := testredis.Prefix(t)
	group := prefix + ":ha-miners"
	stream := prefix + ":" + supplier

	strandInPEL(t, rdb, stream, group, "dead-pod-consumer", stranded)

	consumer, err := NewStreamsConsumer(
		logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "json"}),
		rdb,
		transport.ConsumerConfig{
			StreamPrefix:            prefix,
			SupplierOperatorAddress: supplier,
			ConsumerGroup:           group,
			ConsumerName:            "alive-pod-consumer",
			BatchSize:               10,
			// The stranded entries were aged to an hour by strandInPEL, so any
			// sane threshold lets all of them through. It used to be 1ms with
			// a sleep to outlast it: a race dressed as a setup step.
			ClaimIdleTimeout: 1000,
		},
	)
	require.NoError(t, err)

	// One reclaim invocation, called directly: no timing, no ticker.
	consumer.claimPendingMessages(context.Background())

	got := 0
	for {
		select {
		case msg := <-consumer.msgCh:
			require.True(t, msg.IsReclaim, "recovered entries must be marked as reclaims")
			got++
		default:
			require.Equal(t, stranded, got,
				"one reclaim pass must drain the WHOLE PEL, not one 50-entry page")
			return
		}
	}
}

// TestConsume_CloseDoesNotPanicWithReclaimInFlight pins the msgCh close
// ownership: the channel may be closed only after BOTH producers (the read
// loop and the reclaim ticker) have returned. Before this ordering existed,
// consumeLoop owned `defer close(msgCh)` while the reclaim ticker was
// mid-send on the same channel, and a shutdown or supplier rebalance racing
// a reclaim panicked with send-on-closed-channel — taking down the whole
// miner. The setup parks the reclaim goroutine on a send into a full
// channel, then cancels; the old code panicked here ~50% of the time per
// run, so the loop makes a regression effectively certain to be caught.
func TestConsume_CloseDoesNotPanicWithReclaimInFlight(t *testing.T) {
	rdb := testredis.Client(t)
	basePrefix := testredis.Prefix(t)

	// Shorten the idle block so a run does not spend an interval per iteration
	// waiting for the read loop to come back.
	restoreBlock := blockInterval
	blockInterval = 100 * time.Millisecond
	t.Cleanup(func() { blockInterval = restoreBlock })

	for i := 0; i < 10; i++ {
		const supplier = "pokt1close_test"
		// A prefix of its own per iteration: the entries of one round must not
		// be visible to the next.
		prefix := fmt.Sprintf("%s:r%d", basePrefix, i)
		group := prefix + ":ha-miners"
		stream := prefix + ":" + supplier

		// Stranded entries for the reclaim ticker to recover and send.
		strandInPEL(t, rdb, stream, group, "dead-pod-consumer", 3)

		// Fresh (undelivered) messages so the read loop parks on a channel
		// send rather than inside XREADGROUP: a send blocked on a full channel
		// is exactly the state the close must not race.
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			relay := &transport.MinedRelayMessage{
				RelayHash:               []byte(fmt.Sprintf("fresh-%d", i)),
				RelayBytes:              []byte("payload"),
				ComputeUnitsPerRelay:    1,
				SessionId:               "sess-close",
				SupplierOperatorAddress: supplier,
				ServiceId:               "develop-http",
			}
			buf, merr := relay.Marshal()
			require.NoError(t, merr)
			require.NoError(t, rdb.XAdd(ctx, &goredis.XAddArgs{
				Stream: stream, Values: map[string]any{"data": string(buf)},
			}).Err())
		}

		consumer, err := NewStreamsConsumer(
			logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "json"}),
			rdb,
			transport.ConsumerConfig{
				StreamPrefix:            prefix,
				SupplierOperatorAddress: supplier,
				ConsumerGroup:           group,
				ConsumerName:            "alive-pod-consumer",
				BatchSize:               10,
				ClaimIdleTimeout:        20, // ms; ticks fast so a reclaim is in flight
				ChannelBufferSize:       1,  // parks producers on their sends immediately
			},
		)
		require.NoError(t, err)

		runCtx, cancel := context.WithCancel(context.Background())
		ch := consumer.Consume(runCtx)

		// Give the ticker time to claim and park on the full channel, then
		// shut down while that send is in flight.
		time.Sleep(60 * time.Millisecond)
		cancel()

		// The channel must close (producers drained, coordinator closed it)
		// without a send-on-closed-channel panic. Drain until closed.
		deadline := time.After(5 * time.Second)
		for open := true; open; {
			select {
			case _, ok := <-ch:
				open = ok
			case <-deadline:
				t.Fatal("msgCh never closed after cancel: producers did not shut down")
			}
		}

	}
}
