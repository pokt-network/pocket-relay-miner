//go:build test

package redis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// sweepSeen signals every XPENDING the reclaim sweep issues on key, with its
// error, so a test can wait for a sweep without a clock.
type sweepSeen struct {
	key string
	ch  chan error
}

func (h *sweepSeen) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *sweepSeen) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *sweepSeen) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if args := cmd.Args(); cmd.Name() == "xpending" && len(args) >= 2 && args[1] == h.key {
			select {
			case h.ch <- err:
			default:
			}
		}
		return err
	}
}

// deliverRelay adds a relay the consumer can actually parse -- the fixture's own
// entry carries "x", which a sweep drops as undecodable -- and delivers it to
// the fixture's consumer, unacknowledged.
func (f *releaseFixture) deliverRelay(t *testing.T) transport.StreamMessage {
	t.Helper()
	ctx := context.Background()
	buf, err := (&transport.MinedRelayMessage{SessionId: "sess-sweep", SupplierOperatorAddress: "pokt1sweep"}).Marshal()
	require.NoError(t, err)
	require.NoError(t, f.client.XAdd(ctx, &redis.XAddArgs{Stream: f.stream, Values: map[string]any{"data": string(buf)}}).Err())
	read, err := f.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: f.group, Consumer: releaseConsumerName, Streams: []string{f.stream, ">"}, Count: 1,
	}).Result()
	require.NoError(t, err)
	require.Len(t, read[0].Messages, 1)
	return transport.StreamMessage{ID: read[0].Messages[0].ID, StreamName: f.stream}
}

// startSweeper runs a second consumer's reclaim loop, the one a freshly claimed
// supplier starts, with an idle timeout of an hour: its periodic sweep cannot be
// what delivers anything in this test. The loop is stopped and waited for when
// the test ends.
func startSweeper(t *testing.T, client redis.UniversalClient, stream, group, name string) *StreamsConsumer {
	t.Helper()
	b := &StreamsConsumer{
		logger:     zerolog.Nop(),
		client:     client,
		streamName: stream,
		config: transport.ConsumerConfig{
			SupplierOperatorAddress: "pokt1sweeper",
			ConsumerGroup:           group,
			ConsumerName:            name,
			ClaimIdleTimeout:        int64(time.Hour / time.Millisecond),
		},
		msgCh: make(chan transport.StreamMessage, 10),
	}
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		b.reclaimLoop(ctx)
	}()
	t.Cleanup(func() { cancel(); done.Wait() })
	return b
}

// TestReclaimLoopSweepsAsSoonAsItStarts: an entry released by the consumer that
// held it -- what a supplier's exit does with its relay batch -- reaches the next
// consumer's reclaim at once, not one idle timeout later. In the L3 of df5441c
// (2026-09-11) the supplier was re-taken every ~32 s against a 60 s timeout, so
// the first sweep never came and the released relays waited for another miner.
func TestReclaimLoopSweepsAsSoonAsItStarts(t *testing.T) {
	f := newReleaseFixture(t, nil)
	released := f.deliverRelay(t)
	require.NoError(t, f.consumer.ReleaseMessage(context.Background(), released))

	b := startSweeper(t, f.client, f.stream, f.group, "b")

	select {
	case got := <-b.msgCh:
		require.Equal(t, released.ID, got.ID)
		require.True(t, got.IsReclaim, "it arrives as a reclaim, so the worker runs its duplicate check")
	case <-time.After(10 * time.Second):
		t.Fatal("the released entry did not reach the new consumer's reclaim: its first sweep waits for the idle timeout")
	}
}

// TestReclaimLoopCreatesTheGroupBeforeItsFirstSweep: the read loop creates the
// group on its own goroutine and may not have yet. A sweep against a missing
// group fails as NOGROUP, which the sweep skips in silence -- the immediate
// sweep would then do nothing at all.
func TestReclaimLoopCreatesTheGroupBeforeItsFirstSweep(t *testing.T) {
	f := newReleaseFixture(t, nil)
	const group = "group-not-created-yet"
	seen := &sweepSeen{key: f.stream, ch: make(chan error, 4)}
	f.client.AddHook(seen)

	startSweeper(t, f.client, f.stream, group, "b")

	select {
	case err := <-seen.ch:
		require.NoError(t, err, "the first sweep must find the group: a NOGROUP here is skipped in silence")
	case <-time.After(10 * time.Second):
		t.Fatal("no sweep at start")
	}
}

// TestReclaimSweepDoesNotTakeAnEntryALiveConsumerIsProcessing is the control:
// sweeping sooner and more often must not take work from a consumer that is
// merely busy. Only entries idle past ClaimIdleTimeout are claimed. The sweep is
// run to completion on this goroutine, so nothing cancels it half-way.
func TestReclaimSweepDoesNotTakeAnEntryALiveConsumerIsProcessing(t *testing.T) {
	f := newReleaseFixture(t, nil)
	inflight := f.deliverRelay(t) // delivered to "me", not released: in flight

	b := &StreamsConsumer{
		logger:     zerolog.Nop(),
		client:     f.client,
		streamName: f.stream,
		config: transport.ConsumerConfig{
			SupplierOperatorAddress: "pokt1sweeper",
			ConsumerGroup:           f.group,
			ConsumerName:            "b",
			ClaimIdleTimeout:        int64(time.Hour / time.Millisecond),
		},
		msgCh: make(chan transport.StreamMessage, 10),
	}
	b.claimPendingMessages(context.Background())

	require.Equal(t, releaseConsumerName, f.ownerOf(t, inflight.ID),
		"an entry a live consumer holds for less than the idle timeout is not taken")
	require.Empty(t, b.msgCh)
}
