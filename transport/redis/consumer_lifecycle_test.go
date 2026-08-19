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
}

// TestClaimPendingMessages_DrainsWholePEL pins that ONE reclaim invocation
// recovers the ENTIRE eligible PEL, not just the first pending page
// (XPENDING Count=50). A dead consumer can strand thousands of deliveries (a full
// read batch plus the channel buffer); recovering them at one page per tick
// would drain slower than the claim window closes, silently losing the tail
// — the exact issue-#25 loss the reclaim exists to prevent.
func TestClaimPendingMessages_DrainsWholePEL(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	const (
		supplier = "pokt1drain_test"
		group    = "ha-miners"
		stream   = "ha:relays:" + supplier
		stranded = 120 // needs 3 pending pages at XPENDING Count=50
	)

	strandInPEL(t, rdb, stream, group, "dead-pod-consumer", stranded)

	consumer, err := NewStreamsConsumer(
		logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "json"}),
		rdb,
		transport.ConsumerConfig{
			StreamPrefix:            "ha:relays",
			SupplierOperatorAddress: supplier,
			ConsumerGroup:           group,
			ConsumerName:            "alive-pod-consumer",
			BatchSize:               10,
			ClaimIdleTimeout:        1, // ms; everything stranded is already eligible
		},
		0,
	)
	require.NoError(t, err)

	// Let the stranded entries exceed the (1ms) min-idle threshold.
	time.Sleep(5 * time.Millisecond)

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
	for i := 0; i < 10; i++ {
		mr, err := miniredis.Run()
		require.NoError(t, err)

		rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

		const (
			supplier = "pokt1close_test"
			group    = "ha-miners"
			stream   = "ha:relays:" + supplier
		)

		// Stranded entries for the reclaim ticker to recover and send.
		strandInPEL(t, rdb, stream, group, "dead-pod-consumer", 3)

		// Fresh (undelivered) messages so the read loop parks on a channel
		// send rather than inside XREADGROUP: miniredis does not interrupt a
		// blocked XREADGROUP on context cancellation, and a send parked on a
		// full channel is exactly the state the close must not race.
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
				StreamPrefix:            "ha:relays",
				SupplierOperatorAddress: supplier,
				ConsumerGroup:           group,
				ConsumerName:            "alive-pod-consumer",
				BatchSize:               10,
				ClaimIdleTimeout:        20, // ms; ticks fast so a reclaim is in flight
				ChannelBufferSize:       1,  // parks producers on their sends immediately
			},
			0,
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

		_ = rdb.Close()
		mr.Close()
	}
}
