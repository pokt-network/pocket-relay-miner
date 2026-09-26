//go:build test

package redis

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// The delivery channel was bounded in entries only (5000 per supplier): after a
// restart with a backlog of 1 MiB relays the reclaim filled it with 5 GiB. It is
// now also bounded in bytes -- "X bytes or N relays" -- by send.

const boundRelaySize = 1 << 20

type boundFixture struct {
	client *goredis.Client
	prefix string
	stream string
	group  string
	c      *StreamsConsumer
	parked chan struct{}
}

func newBoundFixture(t *testing.T, supplier string) *boundFixture {
	t.Helper()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	f := &boundFixture{
		client: client, prefix: prefix,
		stream: transport.SupplierStreamName(prefix, supplier), group: prefix + ":group",
	}
	require.NoError(t, client.XGroupCreateMkStream(context.Background(), f.stream, f.group, "0").Err())
	c, err := NewStreamsConsumer(zerolog.Nop(), client, transport.ConsumerConfig{
		StreamPrefix:            prefix,
		SupplierOperatorAddress: supplier,
		ConsumerGroup:           f.group,
		ConsumerName:            "reader",
		ClaimIdleTimeout:        60000,
		BatchSize:               1000,
		ChannelBufferSize:       5000,
	})
	require.NoError(t, err)
	f.parked = make(chan struct{}, 1)
	c.sendWaitHook = func() {
		select {
		case f.parked <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { _ = c.Close() })
	f.c = c
	return f
}

// add appends n relays of boundRelaySize with distinct, incompressible bodies
// (chained SHA-256) and returns their IDs.
func (f *boundFixture) add(t *testing.T, n int) []string {
	t.Helper()
	seed := sha256.Sum256([]byte(f.stream))
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		body := make([]byte, 0, boundRelaySize)
		for len(body) < boundRelaySize {
			seed = sha256.Sum256(seed[:])
			body = append(body, seed[:]...)
		}
		hash := sha256.Sum256(body)
		buf, err := (&transport.MinedRelayMessage{
			SessionId: "sess-bound", SupplierOperatorAddress: "s", RelayBytes: body, RelayHash: hash[:],
		}).Marshal()
		require.NoError(t, err)
		id, err := f.client.XAdd(context.Background(), &goredis.XAddArgs{Stream: f.stream, Values: map[string]any{"data": string(buf)}}).Result()
		require.NoError(t, err)
		ids = append(ids, id)
	}
	return ids
}

// readAs reads n new entries as consumer name and leaves them pending.
func (f *boundFixture) readAs(t *testing.T, name string, n int) {
	t.Helper()
	res, err := f.client.XReadGroup(context.Background(), &goredis.XReadGroupArgs{
		Group: f.group, Consumer: name, Streams: []string{f.stream, ">"}, Count: int64(n),
	}).Result()
	require.NoError(t, err)
	require.Len(t, res[0].Messages, n)
}

// waitParked waits until a producer is waiting in send, then asserts the
// channel holds no more than the budget plus the relay let in below it.
func (f *boundFixture) waitParked(t *testing.T) {
	t.Helper()
	select {
	case <-f.parked:
	case <-time.After(20 * time.Second):
		t.Fatalf("no producer ever waited: the channel took %d bytes in %d relays", f.c.channelBytes.Load(), len(f.c.msgCh))
	}
	held := f.c.channelBytes.Load()
	require.Positive(t, held, "premise: relays are waiting in the channel")
	require.LessOrEqual(t, held, int64(readBudgetBytes+2*boundRelaySize),
		"the channel holds at most the budget plus the relay let in below it")
	require.Less(t, len(f.c.msgCh), cap(f.c.msgCh), "premise: it was the bytes that stopped it, not the entries")
}

// drain takes every relay the producer delivers until want are in, and asserts
// each ID arrives once and the count returns to zero.
func (f *boundFixture) drain(t *testing.T, want []string) {
	t.Helper()
	seen := map[string]int{}
	for len(seen) < len(want) {
		select {
		case msg := <-f.c.msgCh:
			f.c.MarkDelivered(msg)
			seen[msg.ID]++
			transport.ReleaseMinedRelayMessage(msg.Message)
		case <-time.After(20 * time.Second):
			t.Fatalf("only %d of %d relays were delivered", len(seen), len(want))
		}
	}
	for _, id := range want {
		require.Equal(t, 1, seen[id], "relay %s delivered once", id)
	}
	require.Zero(t, f.c.channelBytes.Load(), "every relay left the count")
}

func TestChannelBytes_TheReadLoopWaitsAtTheBudget(t *testing.T) {
	f := newBoundFixture(t, "pokt1bound_read")
	ids := f.add(t, readBudgetBytes/boundRelaySize*3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = f.c.Consume(ctx)

	f.waitParked(t)
	f.drain(t, ids)
}

func TestChannelBytes_TheReclaimWaitsAtTheBudget(t *testing.T) {
	f := newBoundFixture(t, "pokt1bound_reclaim")
	ids := f.add(t, readBudgetBytes/boundRelaySize*3)
	f.readAs(t, "dead-consumer", len(ids))
	// Age the dead consumer's entries past ClaimIdleTimeout without waiting.
	args := []any{"XCLAIM", f.stream, f.group, "dead-consumer", 0}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, "IDLE", 600000)
	require.NoError(t, f.client.Do(context.Background(), args...).Err())
	f.c.noteLargestEntry(boundRelaySize + 200)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.c.claimPendingMessages(ctx); close(done) }()

	f.waitParked(t)
	f.drain(t, ids)
	<-done
}

func TestChannelBytes_OwnPendingWaitsAtTheBudget(t *testing.T) {
	f := newBoundFixture(t, "pokt1bound_own")
	ids := f.add(t, readBudgetBytes/boundRelaySize*3)
	f.readAs(t, "reader", len(ids))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.c.deliverOwnPending(ctx) }()

	f.waitParked(t)
	f.drain(t, ids)
	require.NoError(t, <-done)
}

func TestChannelBytes_TheReclaimPageIsSizedByBytes(t *testing.T) {
	c := &StreamsConsumer{}
	require.Equal(t, int64(pendingPageSize), c.pageSize(), "before any read, the page is the full size")
	c.noteLargestEntry(boundRelaySize)
	require.Equal(t, int64(readBudgetBytes/boundRelaySize), c.pageSize(), "a page of 1 MiB entries fits the budget")
	c.noteLargestEntry(4 * readBudgetBytes)
	require.Equal(t, int64(1), c.pageSize(), "never below one")
}

func TestChannelBytes_AnEmptyChannelAlwaysTakesTheRelay(t *testing.T) {
	c := &StreamsConsumer{msgCh: make(chan transport.StreamMessage, 1), space: make(chan struct{}, 1)}
	c.channelBytes.Store(2 * readBudgetBytes) // a drifted count, or one relay over the budget
	msg := transport.StreamMessage{Message: &transport.MinedRelayMessage{RelayBytes: make([]byte, 8)}}
	require.NoError(t, c.send(context.Background(), msg), "an empty channel never makes a producer wait")
	require.Len(t, c.msgCh, 1)
}

func TestChannelBytes_CancellingAWaitingProducerHandsNothingOver(t *testing.T) {
	c := &StreamsConsumer{msgCh: make(chan transport.StreamMessage, 2), space: make(chan struct{}, 1)}
	c.msgCh <- transport.StreamMessage{}
	c.channelBytes.Store(readBudgetBytes)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.send(ctx, transport.StreamMessage{Message: &transport.MinedRelayMessage{RelayBytes: make([]byte, 8)}})
	}()
	cancel()
	select {
	case err := <-done:
		require.True(t, errors.Is(err, context.Canceled))
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled producer kept waiting")
	}
	require.Len(t, c.msgCh, 1, "nothing was handed over")
	require.Equal(t, int64(readBudgetBytes), c.channelBytes.Load(), "and nothing was counted")
}
