//go:build test

package redis

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

func mined(supplier, session string, n int) *transport.MinedRelayMessage {
	return &transport.MinedRelayMessage{
		SessionId:               session,
		SessionEndHeight:        10,
		SupplierOperatorAddress: supplier,
		ServiceId:               "svc",
		RelayBytes:              []byte{byte(n)},
	}
}

func newBatcher(t *testing.T, interval time.Duration) (*BatchingPublisher, goredis.UniversalClient, string) {
	t.Helper()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, interval)
	t.Cleanup(func() { _ = p.Close() })
	return p, client, prefix
}

// TestBatchingPublisherValidatesAtEnqueue is the choice that decides how big the
// poison-message problem is: an invalid relay must be refused where the caller
// can see it, never carried into a chunk where the EXEC would reject it and leave
// the chunk permanently undispatchable.
func TestBatchingPublisherValidatesAtEnqueue(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour) // never dispatches on its own

	require.Error(t, p.Publish(context.Background(), nil), "a nil message must be refused at enqueue")

	noSession := mined("pokt1a", "", 1)
	require.Error(t, p.Publish(context.Background(), noSession), "an empty session must be refused at enqueue")

	badHeight := mined("pokt1a", "s1", 1)
	badHeight.SessionEndHeight = 0
	require.Error(t, p.Publish(context.Background(), badHeight),
		"SessionEndHeight <= 0 must be refused at enqueue: this is the check that rejected "+
			"1412 served relays, and carrying it into a chunk would block the queue's head")

	p.mu.Lock()
	queued := len(p.queue)
	p.mu.Unlock()
	require.Zero(t, queued, "nothing invalid may sit in the queue")
}

// TestBatchingPublisherCloseFlushesEverything is the shutdown guarantee: Close
// must land what is queued, on a context detached from the one that ended.
func TestBatchingPublisherCloseFlushesEverything(t *testing.T) {
	// An interval long enough that the dispatcher never ticks: the only thing
	// that can write these relays is the final flush.
	p, client, prefix := newBatcher(t, time.Hour)

	const n = 25
	for i := 0; i < n; i++ {
		require.NoError(t, p.Publish(context.Background(), mined("pokt1close", "s1", i)))
	}
	stream := transport.SupplierStreamName(prefix, "pokt1close")
	require.Equal(t, int64(0), client.XLen(context.Background(), stream).Val(),
		"nothing may have been written before the flush, or this test proves nothing about it")

	require.NoError(t, p.Close())

	require.Equal(t, int64(n), client.XLen(context.Background(), stream).Val(),
		"Close must flush every queued relay: they were served, signed and answered")
}

// TestBatchingPublisherDispatchesOnTheInterval covers the ordinary path, so the
// Close test above cannot be the only thing keeping the publisher honest.
func TestBatchingPublisherDispatchesOnTheInterval(t *testing.T) {
	p, client, prefix := newBatcher(t, 50*time.Millisecond)
	stream := transport.SupplierStreamName(prefix, "pokt1tick")

	require.NoError(t, p.Publish(context.Background(), mined("pokt1tick", "s1", 1)))

	require.Eventually(t, func() bool {
		return client.XLen(context.Background(), stream).Val() == 1
	}, 5*time.Second, 10*time.Millisecond, "the dispatcher must write without anyone closing it")
}

// TestBatchingPublisherKeepsAStreamWhole pins the chunking rule: a stream is not
// split across chunks, because the miner's blocked reader wakes once per EXEC
// that touches its stream and a split gives back what the batch bought.
func TestBatchingPublisherKeepsAStreamWhole(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour)

	// Two suppliers, interleaved, more entries than one chunk holds.
	for i := 0; i < maxChunkCommands+10; i++ {
		supplier := "pokt1even"
		if i%2 == 1 {
			supplier = "pokt1odd"
		}
		require.NoError(t, p.Publish(context.Background(), mined(supplier, "s1", i)))
	}

	chunk := p.takeChunk()
	require.NotEmpty(t, chunk)
	require.LessOrEqual(t, len(chunk), maxChunkCommands)

	// The chunk must end on a stream boundary: the entry after the cut must
	// start a different stream than the chunk's last entry, or the stream was
	// split.
	p.mu.Lock()
	rest := p.queue
	p.mu.Unlock()
	if len(rest) > 0 {
		require.NotEqual(t, chunk[len(chunk)-1].stream, rest[0].stream,
			"a stream was split across two chunks: the reader for that supplier now wakes twice")
	}
}

// exhaustedPoolTimeout fails the first N TxPipeline executions with
// redis.ErrPoolTimeout.
//
// Injected through a HOOK, which is the only faithful way: go-redis retries a
// pool timeout by itself (shouldRetry treats ErrPoolTimeout as retryable), and a
// hook sits OUTSIDE that retry loop, so an error returned here is what the caller
// sees when the retries are already exhausted. Making Redis itself time out would
// be absorbed and never reach the dispatcher, and the test would pass while
// proving nothing.
type exhaustedPoolTimeout struct{ failNext, fired atomic.Int64 }

func (h *exhaustedPoolTimeout) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h *exhaustedPoolTimeout) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return next
}

func (h *exhaustedPoolTimeout) ProcessPipelineHook(
	next goredis.ProcessPipelineHook,
) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if h.failNext.Add(-1) >= 0 {
			h.fired.Add(1)
			for _, cmd := range cmds {
				cmd.SetErr(goredis.ErrPoolTimeout)
			}
			return goredis.ErrPoolTimeout
		}
		return next(ctx, cmds)
	}
}

// TestBatchingPublisherRetriesAChunkAfterAnExhaustedPoolTimeout is condition (A),
// and it is the most expensive failure this item can have.
//
// A pool timeout that outlives go-redis's own retries means the write NEVER
// LEFT. Under the old one-relay-per-call publisher that lost one served relay;
// under a batch it arrives for a whole chunk at once, so the same defect costs up
// to maxChunkCommands relays -- every one of them served, signed, and answered to
// a client.
//
// The assertion is the stream length, not a counter of ours: XLEN cannot be
// satisfied by bookkeeping.
func TestBatchingPublisherRetriesAChunkAfterAnExhaustedPoolTimeout(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)

	hook := &exhaustedPoolTimeout{}
	hook.failNext.Store(1) // the first dispatch fails, the next must succeed
	client.AddHook(hook)

	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, 50*time.Millisecond)
	t.Cleanup(func() { _ = p.Close() })

	const n = 5
	for i := 0; i < n; i++ {
		require.NoError(t, p.Publish(context.Background(), mined("pokt1pool", "s1", i)))
	}
	stream := transport.SupplierStreamName(prefix, "pokt1pool")

	require.Eventually(t, func() bool {
		return client.XLen(context.Background(), stream).Val() == int64(n)
	}, 5*time.Second, 20*time.Millisecond,
		"every relay must reach the stream: a pool timeout means the write never left, "+
			"so the chunk has to be retried, not dropped")

	require.Equal(t, int64(1), hook.fired.Load(),
		"the injected timeout must have hit a real dispatch: if the hook matched nothing, "+
			"this test proves nothing")
}

// TestBatchingPublisherWakesTheReaderOncePerChunk is what the whole item buys,
// and the only assertion that can tell TxPipelined from Pipelined.
//
// MEASURED against Redis 8.10.1: a blocked XREADGROUP receives ALL of a MULTI's
// entries in a single wake-up, and receives ONE when the same XADDs arrive as a
// plain pipeline. So the count of entries in the FIRST read is the difference
// between batching and not batching -- and it is invisible to any test that only
// checks the stream's final length.
//
// It needs a real Redis and would pass for the wrong reason on miniredis, which
// answers XREADGROUP without blocking at all.
func TestBatchingPublisherWakesTheReaderOncePerChunk(t *testing.T) {
	// An interval long enough that nothing dispatches until Close: the reader
	// must be blocked BEFORE the single write it is meant to observe.
	p, client, prefix := newBatcher(t, time.Hour)
	ctx := context.Background()

	const n = 8
	stream := transport.SupplierStreamName(prefix, "pokt1wake")
	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, "g", "0").Err())

	for i := 0; i < n; i++ {
		require.NoError(t, p.Publish(ctx, mined("pokt1wake", "s1", i)))
	}

	first := make(chan int, 1)
	go func() {
		res, err := client.XReadGroup(ctx, &goredis.XReadGroupArgs{
			Group:    "g",
			Consumer: "c",
			Streams:  []string{stream, ">"},
			Count:    n,
			Block:    5 * time.Second,
		}).Result()
		if err != nil || len(res) == 0 {
			first <- 0
			return
		}
		first <- len(res[0].Messages)
	}()

	// Give the reader time to be BLOCKED rather than merely started: a read
	// issued after the write would find the entries already there and would
	// report n regardless of how they were written, which is the exact way this
	// test could pass for the wrong reason.
	require.Eventually(t, func() bool {
		return client.XInfoGroups(ctx, stream).Val()[0].Consumers > 0
	}, 5*time.Second, 10*time.Millisecond, "the consumer must be registered and blocked before the write")

	require.NoError(t, p.Close()) // one chunk, one EXEC

	got := <-first
	require.Equal(t, n, got,
		"a blocked reader must receive the whole chunk in ONE wake-up. Receiving 1 means the "+
			"batch went out as a plain pipeline instead of MULTI/EXEC, which is the entire "+
			"saving this item exists for")
}

// TestPublishRejectionsAreDistinguishable is the half of the 1412 lost relays
// that no code change can recover but every future one can: knowing WHICH check
// refused them. One generic drop reason is why nobody could say, for a whole
// load, which of the validations was firing.
func TestPublishRejectionsAreDistinguishable(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour)

	before := map[string]float64{}
	for _, r := range []string{rejectReasonNilMessage, rejectReasonNoSessionID, rejectReasonBadEndHeight} {
		before[r] = testutil.ToFloat64(publishRejectedTotal.WithLabelValues("svc", r))
	}
	beforeNil := testutil.ToFloat64(publishRejectedTotal.WithLabelValues("unknown", rejectReasonNilMessage))

	require.Error(t, p.Publish(context.Background(), nil))
	noSession := mined("pokt1a", "", 1)
	require.Error(t, p.Publish(context.Background(), noSession))
	badHeight := mined("pokt1a", "s1", 1)
	badHeight.SessionEndHeight = 0
	require.Error(t, p.Publish(context.Background(), badHeight))

	require.Equal(t, beforeNil+1,
		testutil.ToFloat64(publishRejectedTotal.WithLabelValues("unknown", rejectReasonNilMessage)),
		"a nil message has no service, and it must be counted as unknown rather than dropped from the count")
	require.Equal(t, before[rejectReasonNoSessionID]+1,
		testutil.ToFloat64(publishRejectedTotal.WithLabelValues("svc", rejectReasonNoSessionID)))
	require.Equal(t, before[rejectReasonBadEndHeight]+1,
		testutil.ToFloat64(publishRejectedTotal.WithLabelValues("svc", rejectReasonBadEndHeight)),
		"session_end_height is the one proxy.go can produce from a literal 0 (queue item 188): "+
			"when that is fixed, this series should fall to zero, which verifies the fix without a new test")
}

// TestRejectionLoggingIsRateLimited pins the other half: the counter is
// unconditional, the LOG is not. proxy.go has a path that builds every message
// with SessionEndHeight 0, so an unbounded Warn would be one line per relay at
// thousands per second -- the flood the logging policy exists to prevent.
func TestRejectionLoggingIsRateLimited(t *testing.T) {
	rejectLogLast.Delete("r\x00s")
	require.True(t, shouldLogReject("r", "s"), "the first occurrence must be logged")
	require.False(t, shouldLogReject("r", "s"), "an immediate repeat must not")
	require.True(t, shouldLogReject("r", "other-service"),
		"a different service is a different signal and must not be suppressed by the first")
}
