//go:build test

package redis

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// A consumer's name is per process, so a supplier this process releases and
// takes again gets a consumer with the same name -- and whatever the previous
// one left pending under it. ">" returns only new entries and the reclaim skips
// its own consumer's entries, so nothing read them until the process restarted.

type ownPendingFixture struct {
	client   *redis.Client
	prefix   string
	supplier string
	stream   string
	group    string
}

func newOwnPendingFixture(t *testing.T, supplier string) *ownPendingFixture {
	t.Helper()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	f := &ownPendingFixture{
		client: client, prefix: prefix, supplier: supplier,
		stream: transport.SupplierStreamName(prefix, supplier), group: prefix + ":group",
	}
	require.NoError(t, client.XGroupCreateMkStream(context.Background(), f.stream, f.group, "0").Err())
	return f
}

// consumer builds a consumer named name whose reclaim takes nothing in a test:
// its idle timeout is an hour.
func (f *ownPendingFixture) consumer(t *testing.T, name string) *StreamsConsumer {
	t.Helper()
	c, err := NewStreamsConsumer(zerolog.Nop(), f.client, transport.ConsumerConfig{
		StreamPrefix:            f.prefix,
		SupplierOperatorAddress: f.supplier,
		ConsumerGroup:           f.group,
		ConsumerName:            name,
		ClaimIdleTimeout:        int64(time.Hour / time.Millisecond),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// add appends a relay the consumer can parse and returns its ID.
func (f *ownPendingFixture) add(t *testing.T) string {
	t.Helper()
	buf, err := (&transport.MinedRelayMessage{SessionId: "sess-own", SupplierOperatorAddress: f.supplier}).Marshal()
	require.NoError(t, err)
	id, err := f.client.XAdd(context.Background(), &redis.XAddArgs{Stream: f.stream, Values: map[string]any{"data": string(buf)}}).Result()
	require.NoError(t, err)
	return id
}

// deliverTo reads n new entries as consumer name and leaves them unacknowledged.
func (f *ownPendingFixture) deliverTo(t *testing.T, name string, n int) {
	t.Helper()
	read, err := f.client.XReadGroup(context.Background(), &redis.XReadGroupArgs{
		Group: f.group, Consumer: name, Streams: []string{f.stream, ">"}, Count: int64(n),
	}).Result()
	require.NoError(t, err)
	require.Len(t, read[0].Messages, n)
}

func (f *ownPendingFixture) pendingOf(t *testing.T, name string) int {
	t.Helper()
	pending, err := f.client.XPendingExt(context.Background(), &redis.XPendingExtArgs{
		Stream: f.stream, Group: f.group, Start: "-", End: "+", Count: 1000, Consumer: name,
	}).Result()
	require.NoError(t, err)
	return len(pending)
}

func receive(t *testing.T, ch <-chan transport.StreamMessage, what string) transport.StreamMessage {
	t.Helper()
	select {
	case msg, ok := <-ch:
		require.True(t, ok, "%s: the channel closed instead", what)
		return msg
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: nothing arrived", what)
		return transport.StreamMessage{}
	}
}

// TestConsumer_TheNextConsumerWithTheSameNameGetsWhatTheLastLeftPending: the
// first consumer reads E and ends without settling it.
func TestConsumer_TheNextConsumerWithTheSameNameGetsWhatTheLastLeftPending(t *testing.T) {
	f := newOwnPendingFixture(t, "pokt1ownpending_next")
	e := f.add(t)

	first := f.consumer(t, "n")
	got := receive(t, first.Consume(context.Background()), "the first consumer reads it")
	require.Equal(t, e, got.ID)
	transport.ReleaseMinedRelayMessage(got.Message)
	require.NoError(t, first.Close())
	require.Equal(t, 1, f.pendingOf(t, "n"), "premise: left pending under the name")

	next := f.consumer(t, "n")
	got = receive(t, next.Consume(context.Background()),
		"the next consumer with the same name must get what the last one left pending")
	require.Equal(t, e, got.ID)
	require.True(t, got.IsReclaim, "as a reclaim, so the worker runs its duplicate check")
	transport.ReleaseMinedRelayMessage(got.Message)
}

// failNewRead fails the n-th XREADGROUP for new entries (">") on stream, which
// sends the read loop through a reconnection.
type failNewRead struct {
	stream string
	n      int32
	seen   atomic.Int32
}

func (h *failNewRead) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failNewRead) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failNewRead) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "xreadgroup" && len(args) >= 2 && args[len(args)-1] == ">" &&
			args[len(args)-2] == h.stream && h.seen.Add(1) == h.n {
			err := errors.New("injected read failure")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// TestConsumer_AReconnectionDoesNotHandOverItsOwnPendingAgain: past the first
// pass, what is pending under the name is this consumer's own delivery -- here
// F, read as new and still being processed. A reconnection that passed over the
// pending list again would hand F over twice.
func TestConsumer_AReconnectionDoesNotHandOverItsOwnPendingAgain(t *testing.T) {
	f := newOwnPendingFixture(t, "pokt1ownpending_reconnect")
	e := f.add(t)
	f.deliverTo(t, "n", 1) // left pending under n by an earlier consumer
	fail := &failNewRead{stream: f.stream, n: 2}
	f.client.AddHook(fail)

	c := f.consumer(t, "n")
	ch := c.Consume(context.Background())
	got := receive(t, ch, "the pending entry")
	require.Equal(t, e, got.ID)
	transport.ReleaseMinedRelayMessage(got.Message)

	fresh := f.add(t)
	got = receive(t, ch, "the new entry")
	require.Equal(t, fresh, got.ID)
	transport.ReleaseMinedRelayMessage(got.Message)

	later := f.add(t)
	got = receive(t, ch, "the entry after the reconnection")
	require.GreaterOrEqual(t, fail.seen.Load(), int32(2), "premise: the read loop went through a reconnection")
	require.Equal(t, later, got.ID, "the reconnection handed over again what this consumer had already delivered")
	transport.ReleaseMinedRelayMessage(got.Message)
}

// failOwnRead fails the n-th read of this stream's pending list (an ID, not ">").
type failOwnRead struct {
	stream string
	n      int32
	seen   atomic.Int32
}

func (h *failOwnRead) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failOwnRead) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failOwnRead) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "xreadgroup" && len(args) >= 2 && args[len(args)-2] == h.stream &&
			args[len(args)-1] != ">" && h.seen.Add(1) == h.n {
			err := errors.New("injected read failure")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// TestConsumer_APassCutShortResumesWhereItStopped: the pass over the pending
// list fails on its second page; the reconnection resumes after the first page
// instead of handing it over again.
func TestConsumer_APassCutShortResumesWhereItStopped(t *testing.T) {
	f := newOwnPendingFixture(t, "pokt1ownpending_resume")
	var want []string
	for i := 0; i < pendingPageSize+1; i++ {
		want = append(want, f.add(t))
	}
	f.deliverTo(t, "n", len(want))
	fail := &failOwnRead{stream: f.stream, n: 2}
	f.client.AddHook(fail)

	c := f.consumer(t, "n")
	ch := c.Consume(context.Background())
	var seen []string
	for range want {
		got := receive(t, ch, "a pending entry")
		seen = append(seen, got.ID)
		transport.ReleaseMinedRelayMessage(got.Message)
	}
	require.GreaterOrEqual(t, fail.seen.Load(), int32(2), "premise: the second read failed")
	require.Equal(t, want, seen, "each pending entry once, in order, across the failed read")

	later := f.add(t)
	got := receive(t, ch, "the next entry")
	require.Equal(t, later, got.ID)
	transport.ReleaseMinedRelayMessage(got.Message)
}

// TestEachOwnPending_VisitsEveryEntryOnceAcrossPages: more than two pages.
func TestEachOwnPending_VisitsEveryEntryOnceAcrossPages(t *testing.T) {
	f := newOwnPendingFixture(t, "pokt1ownpending_pages")
	ctx := context.Background()
	var want []string
	for i := 0; i < 2*pendingPageSize+3; i++ {
		want = append(want, f.add(t))
	}
	f.deliverTo(t, "n", len(want))

	c := f.consumer(t, "n") // never started
	var seen []string
	require.NoError(t, c.EachOwnPending(ctx, func(msg transport.StreamMessage) {
		require.True(t, msg.IsReclaim)
		seen = append(seen, msg.ID)
		transport.ReleaseMinedRelayMessage(msg.Message)
	}))

	require.Equal(t, want, seen, "every entry, once, oldest first")
	require.Equal(t, len(want), f.pendingOf(t, "n"), "fn settled nothing, so nothing left the pending list")
}

// TestEachOwnPending_DropsAnEntryTheTrimDeleted: TrimStream deletes an entry
// from the stream and leaves it pending. It has no data to hand over, and it is
// not a producer's defect -- counted as a deserialization error, it would say so.
func TestEachOwnPending_DropsAnEntryTheTrimDeleted(t *testing.T) {
	const supplier = "pokt1ownpending_trim"
	f := newOwnPendingFixture(t, supplier)
	ctx := context.Background()
	f.add(t)
	kept := f.add(t)
	f.deliverTo(t, "n", 2)
	require.NoError(t, f.client.XTrimMinID(ctx, f.stream, kept).Err())
	require.Equal(t, 2, f.pendingOf(t, "n"), "premise: the trimmed entry is still pending")
	badPayloads := deserializationErrors.WithLabelValues(supplier)
	before := testutil.ToFloat64(badPayloads)

	c := f.consumer(t, "n")
	var seen []string
	require.NoError(t, c.EachOwnPending(ctx, func(msg transport.StreamMessage) {
		seen = append(seen, msg.ID)
		transport.ReleaseMinedRelayMessage(msg.Message)
	}))

	require.Equal(t, []string{kept}, seen, "the trimmed entry is not handed over")
	require.Equal(t, 1, f.pendingOf(t, "n"), "it leaves the pending list; the kept one stays for fn to settle")
	require.Equal(t, before, testutil.ToFloat64(badPayloads), "a trimmed entry is not a malformed one")
}

// TestStop_LeavesTheConsumerAbleToHandBackWhatItHolds: a teardown stops the
// producers and only then hands back what is left; Close must still refuse.
func TestStop_LeavesTheConsumerAbleToHandBackWhatItHolds(t *testing.T) {
	f := newOwnPendingFixture(t, "pokt1ownpending_stop")
	ctx := context.Background()
	f.add(t)

	c := f.consumer(t, "n")
	ch := c.Consume(ctx)
	got := receive(t, ch, "the entry")
	c.Stop()
	_, open := <-ch
	require.False(t, open, "stopped: both producers are done and the channel is closed")
	c.Stop() // idempotent

	require.NoError(t, c.ReleaseMessage(ctx, got), "a stopped consumer can still hand back what it holds")
	require.Zero(t, f.pendingOf(t, "n"))

	require.NoError(t, c.Close())
	require.Error(t, c.ReleaseMessage(ctx, got), "a closed one cannot")
	transport.ReleaseMinedRelayMessage(got.Message)
}
