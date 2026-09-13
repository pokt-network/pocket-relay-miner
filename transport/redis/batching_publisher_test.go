//go:build test

package redis

import (
	"context"
	"errors"
	"fmt"
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
	queued := len(p.queue) - p.head
	p.mu.Unlock()
	require.Zero(t, queued, "nothing invalid may sit in the queue")
}

// TestBatchingPublisherRejectsWhatTheOldPublisherRejected carries the two cases
// only the one-relay-per-round-trip publisher's test exercised: a NEGATIVE end
// height (prepareXAdd refuses <= 0, and only 0 was driven here) and a publish
// after Close.
func TestBatchingPublisherRejectsWhatTheOldPublisherRejected(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour)

	negative := mined("pokt1a", "s1", 1)
	negative.SessionEndHeight = -1
	require.Error(t, p.Publish(context.Background(), negative), "a negative session end height must be refused")

	require.NoError(t, p.Close())
	require.Error(t, p.Publish(context.Background(), mined("pokt1a", "s1", 2)), "a closed publisher must refuse")
}

// TestBatchingPublisherSetsNoStreamTTL is the regression test for the defect that
// made a supplier's relay stream disappear mid-session: an EXPIRE armed on the
// stream key deletes un-consumed relays and the pending-entries list with it,
// silently. -1 is Redis' answer for a key with no expiry; -2 would be a key that
// does not exist, which is why the length is checked too.
func TestBatchingPublisherSetsNoStreamTTL(t *testing.T) {
	p, client, prefix := newBatcher(t, time.Hour)
	ctx := context.Background()
	const supplier = "pokt1supplier_ttl"
	stream := transport.SupplierStreamName(prefix, supplier)

	require.NoError(t, p.Publish(ctx, mined(supplier, "s1", 1)))
	p.dispatchAll(ctx)

	ttl, err := client.TTL(ctx, stream).Result()
	require.NoError(t, err)
	require.Equal(t, time.Duration(-1), ttl, "the relay stream must carry NO expiry")
	require.Equal(t, int64(1), client.XLen(ctx, stream).Val(), "the stream must actually hold the relay")
}

// TestBatchingPublisherDoesNotArmTTLAcrossManyDispatches pins the property over
// repeated writes: a later "refresh the TTL on every write" would still leave an
// idle stream to be deleted with its pending entries.
func TestBatchingPublisherDoesNotArmTTLAcrossManyDispatches(t *testing.T) {
	p, client, prefix := newBatcher(t, time.Hour)
	ctx := context.Background()
	const supplier = "pokt1supplier_ttl_many"
	stream := transport.SupplierStreamName(prefix, supplier)

	for i := 0; i < 5; i++ {
		require.NoError(t, p.Publish(ctx, mined(supplier, "s1", i)))
		p.dispatchAll(ctx)
		ttl, err := client.TTL(ctx, stream).Result()
		require.NoError(t, err)
		require.Equal(t, time.Duration(-1), ttl, "no expiry may be armed on dispatch %d", i+1)
	}
	require.Equal(t, int64(5), client.XLen(ctx, stream).Val())
}

// TestBatchingPublisherQueuedBytesCountsPayloadNotEntries pins what the admission
// bound reads: three large relays must register as megabytes, which an entry count
// (three) could never express, and a dispatch must give the bytes back.
func TestBatchingPublisherQueuedBytesCountsPayloadNotEntries(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour)
	ctx := context.Background()
	require.Zero(t, p.QueuedBytes())

	for i := 0; i < 3; i++ {
		msg := mined("pokt1big", "s1", i)
		msg.RelayBytes = make([]byte, 1<<20)
		require.NoError(t, p.Publish(ctx, msg))
	}
	require.GreaterOrEqual(t, p.QueuedBytes(), 3<<20, "three 1 MiB relays must count as at least 3 MiB")

	p.dispatchAll(ctx)
	require.Zero(t, p.QueuedBytes(), "a dispatched queue retains nothing")
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
	rest := p.queue[p.head:]
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

// TestBatchingPublisherWritesTheGoodHalfOnceAndDiscardsThePoison covers the
// PARTIAL failure, which is the case neither of the core's two teeth touched:
// both of them failed the whole EXEC, where no XADD succeeds and nothing can be
// counted or written twice.
//
// An EXEC reports per-command errors and does NOT roll back the commands that
// succeeded beside the failing one, so a chunk can be half written. Requeueing
// all of it -- what this used to do -- wrote the successful half a SECOND time
// on the next tick and counted it a second time, so published climbed above
// served: the goal's invariant 1 breaking in the direction nobody was watching.
//
// The poison is a string sitting where a stream should be, which is the real
// shape of the failure: XADD on it returns WRONGTYPE, permanently.
func TestBatchingPublisherWritesTheGoodHalfOnceAndDiscardsThePoison(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	ctx := context.Background()

	const good, poisoned = "pokt1partialgood", "pokt1partialbad"
	goodStream := transport.SupplierStreamName(prefix, good)
	require.NoError(t,
		client.Set(ctx, transport.SupplierStreamName(prefix, poisoned), "not a stream", 0).Err(),
		"the poisoned stream must hold a wrong-typed value before anything is published")

	publishedBefore := testutil.ToFloat64(publishedTotal.WithLabelValues(good, "svc"))
	poisonPublishedBefore := testutil.ToFloat64(publishedTotal.WithLabelValues(poisoned, "svc"))
	discardedBefore := testutil.ToFloat64(
		publishDiscardedTotal.WithLabelValues(poisoned, "svc", discardReasonWrongType))

	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, 50*time.Millisecond)
	t.Cleanup(func() { _ = p.Close() })

	// One chunk: well under maxChunkCommands, so takeChunk does not cut.
	const n = 6
	for i := 0; i < n-1; i++ {
		require.NoError(t, p.Publish(ctx, mined(good, "s1", i)))
	}
	require.NoError(t, p.Publish(ctx, mined(poisoned, "s1", 99)))

	// Waited on the COUNTER and not on len(p.queue): takeChunk removes a chunk
	// before writing it, so an empty queue also means "a write is in flight", and
	// polling for it catches that window and proves nothing.
	//
	// Before the discard decision the poisoned entry went back to the head
	// forever, and because dispatchAll stops at the first failed chunk that
	// blocked every other supplier's relays too.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(
			publishDiscardedTotal.WithLabelValues(poisoned, "svc", discardReasonWrongType))-discardedBefore == 1
	}, 5*time.Second, 10*time.Millisecond,
		"the unwritable relay must be given up on and counted: it was served and answered, "+
			"and a stream nobody can write must not hold every other supplier hostage")

	// A sentinel proves the dispatcher kept ticking AFTER the drain, so a
	// re-write of the good half would have had somewhere to show up.
	require.NoError(t, p.Publish(ctx, mined(good, "s1", 1000)))
	require.Eventually(t, func() bool {
		return client.XLen(ctx, goodStream).Val() == int64(n)
	}, 5*time.Second, 10*time.Millisecond, "the dispatcher must still be writing after the discard")

	require.Equal(t, int64(n), client.XLen(ctx, goodStream).Val(),
		"the relays that already reached the stream must not be written again: a second copy "+
			"is a relay the miner counts twice and a leaf that does not exist")

	published := testutil.ToFloat64(publishedTotal.WithLabelValues(good, "svc")) - publishedBefore
	require.Equal(t, float64(n), published,
		"published must equal what is in the stream: counting a re-write puts published above served")

	require.Equal(t, float64(0),
		testutil.ToFloat64(publishedTotal.WithLabelValues(poisoned, "svc"))-poisonPublishedBefore,
		"nothing reached the poisoned stream, so nothing may be counted as published for it: "+
			"published is the ONLY counter in this repository that means 'reached the stream', "+
			"and an increment here is the invariant lying about a relay that is lost")
}

// alwaysFails fails every pipeline with an error the classifier does not know,
// which is the case the attempt cap exists for.
type alwaysFails struct {
	err   error
	fired atomic.Int64
}

func (h *alwaysFails) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h *alwaysFails) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }

func (h *alwaysFails) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		h.fired.Add(1)
		for _, cmd := range cmds {
			cmd.SetErr(h.err)
		}
		return h.err
	}
}

// TestBatchingPublisherDoesNotDiscardATransientFailureOnItsFirstAttempt is the
// half of the discard decision that protects served work.
//
// A pool timeout that outlived go-redis's own retries is the ORDINARY failure
// and it passes. e0667eb on this branch exists because dropping a relay there
// lost work that had already been served, so a discard policy that fires on the
// first failure would undo it.
func TestBatchingPublisherDoesNotDiscardATransientFailureOnItsFirstAttempt(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	ctx := context.Background()

	hook := &exhaustedPoolTimeout{}
	hook.failNext.Store(1) // exactly one dispatch fails; the next must succeed
	client.AddHook(hook)

	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, 50*time.Millisecond)
	t.Cleanup(func() { _ = p.Close() })

	const supplier = "pokt1transient"
	discardedBefore := testutil.ToFloat64(
		publishDiscardedTotal.WithLabelValues(supplier, "svc", discardReasonAttemptsExhausted))

	const n = 4
	for i := 0; i < n; i++ {
		require.NoError(t, p.Publish(ctx, mined(supplier, "s1", i)))
	}
	stream := transport.SupplierStreamName(prefix, supplier)

	require.Eventually(t, func() bool {
		return client.XLen(ctx, stream).Val() == int64(n)
	}, 5*time.Second, 20*time.Millisecond,
		"a transient failure must be retried, not given up on: every one of these was served")

	require.Equal(t, float64(0),
		testutil.ToFloat64(publishDiscardedTotal.WithLabelValues(supplier, "svc", discardReasonAttemptsExhausted))-discardedBefore,
		"nothing may be discarded on a first failure")
	require.Equal(t, int64(1), hook.fired.Load(),
		"the injected failure must have hit a real dispatch, or this proves nothing")
}

// TestBatchingPublisherGivesUpOnAnUnknownErrorAfterTheCap is the net under the
// classifier.
//
// Recognising a permanent failure by the text of its error is fragile by
// construction: an error nobody anticipated, that also never passes, would sit
// at the head of the queue forever and stop every supplier from draining -- the
// exact defect the discard decision closes. The cap bounds it without needing to
// know what the error was.
func TestBatchingPublisherGivesUpOnAnUnknownErrorAfterTheCap(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	ctx := context.Background()

	hook := &alwaysFails{err: errors.New("ERR something no classifier has ever seen")}
	client.AddHook(hook)

	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, 10*time.Millisecond)
	t.Cleanup(func() { _ = p.Close() })

	const supplier = "pokt1unknown"
	discardedBefore := testutil.ToFloat64(
		publishDiscardedTotal.WithLabelValues(supplier, "svc", discardReasonAttemptsExhausted))

	const n = 3
	for i := 0; i < n; i++ {
		require.NoError(t, p.Publish(ctx, mined(supplier, "s1", i)))
	}

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(
			publishDiscardedTotal.WithLabelValues(supplier, "svc", discardReasonAttemptsExhausted))-discardedBefore == float64(n)
	}, 10*time.Second, 10*time.Millisecond,
		"the cap must drain a queue whose error nobody classified, or it is not a net -- "+
			"and every relay given up on must be counted, whatever made it unwritable")

	require.GreaterOrEqual(t, hook.fired.Load(), int64(maxPublishAttempts),
		"giving up must take maxPublishAttempts dispatches: discarding sooner would drop "+
			"work a transient failure would have delivered")
}

// TestBatchingPublisherCountsWhatTheFinalFlushAbandoned is the other half of the
// same defect: the dispatch could not tell written from unwritten, and the
// shutdown could not tell drained from abandoned.
//
// When the final flush fails, dispatchAll puts the chunk back on a queue that
// nothing will ever read again. Until this counter existed, Close logged
// "batching publisher closed" and returned nil -- the same line, byte for byte,
// that it logs after draining everything.
func TestBatchingPublisherCountsWhatTheFinalFlushAbandoned(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	ctx := context.Background()

	hook := &exhaustedPoolTimeout{}
	hook.failNext.Store(1 << 20) // every dispatch fails, the final flush included
	client.AddHook(hook)

	// An interval long enough that nothing dispatches on its own: the only write
	// attempted is the final flush, and it is the one that fails.
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)

	const supplier = "pokt1abandoned"
	abandonedBefore := testutil.ToFloat64(shutdownAbandonedRelays.WithLabelValues(supplier, "svc"))

	const n = 7
	for i := 0; i < n; i++ {
		require.NoError(t, p.Publish(ctx, mined(supplier, "s1", i)))
	}

	err := p.Close()
	require.Error(t, err,
		"Close must report that it did not write what it was holding: returning nil "+
			"is what made this silent")

	abandoned := testutil.ToFloat64(shutdownAbandonedRelays.WithLabelValues(supplier, "svc")) - abandonedBefore
	require.Equal(t, float64(n), abandoned,
		"every relay the flush could not write must be counted: they were served, "+
			"signed and answered, and nothing downstream will ever see them")

	require.Positive(t, hook.fired.Load(),
		"the injected failure must have hit a real dispatch, or this proves nothing")
}

// TestIsWrongTypeErrorReadsTheRawServerError pins the classifier, including the
// way it can be defeated.
//
// writeChunk wraps the failing command's error as "XADD to <stream>: %w" before
// returning it, and HasPrefix does not see through a wrapper -- so classifying
// the WRAPPED error would never match and every poisoned stream would be
// retried forever, which is the defect the discard decision closes. The
// classifier is fed cmd.Err() straight from go-redis, and this says so.
func TestIsWrongTypeErrorReadsTheRawServerError(t *testing.T) {
	raw := errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	require.True(t, IsWrongTypeError(raw), "the server's own reply must classify")

	require.False(t, IsWrongTypeError(fmt.Errorf("XADD to s: %w", raw)),
		"a wrapped error must NOT match: this is why writeChunk classifies cmd.Err() and "+
			"not the error it builds from it")

	require.False(t, IsWrongTypeError(nil))
	require.False(t, IsWrongTypeError(errors.New("wrongtype operation against a key")),
		"Redis sends the code upper-case; matching lower-case would be matching something else")
	require.False(t, IsWrongTypeError(errors.New("ERR the value is WRONGTYPE somewhere inside")),
		"anchored at the start on purpose, for the reason IsOOMError matches \"OOM command\"")
}
