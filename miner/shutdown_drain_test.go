//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// drainFixture wires a SupplierManager against a REAL Redis stream whose group
// already holds unacknowledged deliveries for this consumer.
//
// Redis is not a convenience here: acknowledgement is the whole point of the
// drain, and the only honest way to check a relay was acknowledged is to ask the
// server whether it is still pending. A fake consumer would let the test assert
// that a method was called, which is not the same claim.
type drainFixture struct {
	mgr           *SupplierManager
	state         *SupplierState
	client        redis.UniversalClient
	stream, group string
	msgs          []transport.StreamMessage
	processed     *[]string
}

const drainSupplier = "pokt1supplier_drain"

func newDrainFixture(t *testing.T, buffered int) *drainFixture {
	t.Helper()
	ctx := context.Background()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	streamPrefix, group := prefix+":relays", prefix+":group"
	stream := transport.SupplierStreamName(streamPrefix, drainSupplier)

	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, group, "0").Err())

	consumer, err := redistransport.NewStreamsConsumer(zerolog.Nop(), client, transport.ConsumerConfig{
		StreamPrefix:            streamPrefix,
		SupplierOperatorAddress: drainSupplier,
		ConsumerGroup:           group,
		ConsumerName:            "me",
		ClaimIdleTimeout:        60000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = consumer.Close() })

	for i := 0; i < buffered; i++ {
		require.NoError(t, client.XAdd(ctx, &redis.XAddArgs{
			Stream: stream, Values: map[string]any{"data": []byte("x")},
		}).Err())
	}

	// Only read when there is something to read. go-redis sends BLOCK 0 when
	// XReadGroupArgs.Block is left at its zero value, and BLOCK 0 blocks
	// FOREVER on an empty stream -- it sets no read deadline at all, so even a
	// cancelled context cannot end it. Guarding here rather than passing
	// Block: -1 keeps the empty case obviously a no-op.
	var delivered []redis.XMessage
	if buffered > 0 {
		read, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: group, Consumer: "me", Streams: []string{stream, ">"}, Count: int64(buffered),
		}).Result()
		require.NoError(t, err)
		delivered = read[0].Messages
	}
	require.Len(t, delivered, buffered, "premise: every entry is delivered and unacked")

	msgs := make([]transport.StreamMessage, 0, buffered)
	for _, m := range delivered {
		msgs = append(msgs, transport.StreamMessage{
			ID:         m.ID,
			StreamName: stream,
			Message: &transport.MinedRelayMessage{
				SupplierOperatorAddress: drainSupplier,
				ServiceId:               "svc-a",
				SessionId:               "sess-1",
				SessionEndHeight:        100,
			},
		})
	}

	processed := make([]string, 0, buffered)
	mgr := &SupplierManager{
		logger:    logging.NewLoggerFromConfig(logging.DefaultConfig()),
		suppliers: xsync.NewMap[string, *SupplierState](),
		onRelay: func(_ context.Context, _ string, msg *transport.StreamMessage) error {
			processed = append(processed, msg.ID)
			return nil
		},
	}
	state := &SupplierState{OperatorAddr: drainSupplier, Consumer: consumer}
	state.StoreStatus(SupplierStatusActive)
	mgr.suppliers.Store(drainSupplier, state)

	return &drainFixture{
		mgr: mgr, state: state, client: client,
		stream: stream, group: group, msgs: msgs, processed: &processed,
	}
}

func (f *drainFixture) pendingCount(t *testing.T) int64 {
	t.Helper()
	res, err := f.client.XPending(context.Background(), f.stream, f.group).Result()
	require.NoError(t, err)
	return res.Count
}

// bufferedChan returns a closed channel already holding every message, which is
// what the delivery channel looks like the moment the producers stop.
func (f *drainFixture) bufferedChan() <-chan transport.StreamMessage {
	ch := make(chan transport.StreamMessage, len(f.msgs))
	for _, m := range f.msgs {
		ch <- m
	}
	close(ch)
	return ch
}

// TestShutdownDrainProcessesAndAcksBufferedRelays is the regression test for
// relays abandoned on a graceful shutdown.
//
// consumeForSupplier used to return straight out of its ctx.Done branch, so
// everything already sitting in the 5000-slot delivery channel was dropped:
// never processed, never acknowledged, and its pooled message never returned.
// Those entries stay in the pending list of a consumer name that embeds this
// pid, so after the restart NOTHING this process becomes can reclaim them —
// only another consumer's reclaim can, one full claim_idle_timeout later, and
// only if one is running.
//
// The assertion is on the server's pending count, not on a call count: an entry
// that is still pending is an entry that was not acknowledged, whatever the code
// believes it did.
func TestShutdownDrainProcessesAndAcksBufferedRelays(t *testing.T) {
	f := newDrainFixture(t, 3)
	require.Equal(t, int64(3), f.pendingCount(t), "premise: three deliveries are outstanding")

	// The supplier's context is already cancelled when the drain runs — that is
	// the situation being reproduced, and it is why the drain must detach from it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f.mgr.drainDeliveryBuffer(ctx, f.state, f.bufferedChan())

	require.Len(t, *f.processed, 3, "every buffered relay must be processed, not dropped")
	require.Equal(t, int64(0), f.pendingCount(t),
		"every drained relay must be acknowledged; a still-pending entry is work this process "+
			"can never reclaim once its pid changes")
}

// TestShutdownDrainLeavesLeftoversPendingWhenWindowCloses pins the deliberate
// half of the behaviour.
//
// The window is best-effort: what does not fit must NOT be acknowledged, so the
// reclaim on a surviving or restarted miner still recovers it. Acknowledging an
// unprocessed entry to make the shutdown look clean would delete the relay.
func TestShutdownDrainLeavesLeftoversPendingWhenWindowCloses(t *testing.T) {
	f := newDrainFixture(t, 3)

	original := shutdownDrainWindow
	shutdownDrainWindow = 0 // the window is over before the first message
	t.Cleanup(func() { shutdownDrainWindow = original })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f.mgr.drainDeliveryBuffer(ctx, f.state, f.bufferedChan())

	require.Empty(t, *f.processed, "with no window left, nothing may be processed")
	require.Equal(t, int64(3), f.pendingCount(t),
		"abandoned entries must stay PENDING so the reclaim can recover them; acknowledging them "+
			"to tidy up the shutdown would destroy the relays")
}

// TestShutdownDrainOnEmptyBufferIsANoOp covers the ordinary case: an idle
// supplier shutting down has nothing buffered, and the drain must return at once
// rather than wait out its window.
func TestShutdownDrainOnEmptyBufferIsANoOp(t *testing.T) {
	f := newDrainFixture(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		f.mgr.drainDeliveryBuffer(ctx, f.state, f.bufferedChan())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an empty delivery buffer must return immediately, not hold the shutdown open " +
			"for the whole drain window")
	}
	require.Empty(t, *f.processed)
}

// TestShutdownDrainWaitsOutItsWindowEvenWhenTheBufferStartsEmpty is the
// regression test for review 2026-08-21: drainDeliveryBuffer used to bail out
// through a bare `default:` the instant msgChan looked momentarily empty,
// instead of waiting out the drain window like the rest of the function
// claims to. The transport-layer producer shares the supplier's already
// cancelled context and can still be blocked inside a blocking XREADGROUP
// call when the drain starts -- transport/redis/consumer.go documents why a
// cancelled context is only observed once that block elapses, not when it is
// cancelled -- so a message it delivers moments later used to arrive at a
// channel nobody was reading from anymore.
//
// This does NOT race a send against the bug: an unbuffered send would
// sometimes land before the old `default:` fires and sometimes not,
// depending on goroutine scheduling -- exactly the kind of flake Rule #1
// forbids. Instead it asserts the one thing that distinguishes the two
// versions unconditionally: with the channel open and empty, the OLD code
// returns within microseconds every time, while the fix blocks until the
// window elapses or the channel closes. 200ms is a three-orders-of-magnitude
// margin against that near-instant old-code return, chosen to survive CI
// jitter without ever masking the bug.
func TestShutdownDrainWaitsOutItsWindowEvenWhenTheBufferStartsEmpty(t *testing.T) {
	f := newDrainFixture(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	msgChan := make(chan transport.StreamMessage) // open, empty, nothing ever sent
	done := make(chan struct{})
	go func() {
		f.mgr.drainDeliveryBuffer(ctx, f.state, msgChan)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("drainDeliveryBuffer returned immediately although its channel is still open and " +
			"nothing has arrived yet -- an empty buffer must not be treated as \"nothing more is " +
			"coming\" while the window has time left")
	case <-time.After(200 * time.Millisecond):
		// Still running, as required: the drain waited instead of bailing on
		// the first empty check.
	}

	close(msgChan)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainDeliveryBuffer did not return after its channel closed")
	}
	require.Empty(t, *f.processed, "nothing was ever sent, so nothing should have been processed")
}
