package redis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// Chunk limits. A chunk is one MULTI/EXEC, which is one round trip and, for each
// stream it touches, ONE wake-up of the miner's blocked XREADGROUP -- measured
// against Redis 8.10.1: with MULTI a blocked reader receives all the entries at
// once, with loose XADDs or a pipeline without MULTI it receives one.
//
// They are caps, not targets. A single EXEC of 2000 XADDs blocks Redis for about
// 26ms, and Redis executes commands on one thread, so every other client waits
// out that whole batch. Bounding the chunk bounds that stall.
const (
	maxChunkCommands = 256
	maxChunkBytes    = 1 << 20 // 1 MiB
)

// queued is one relay waiting to be written.
type queued struct {
	stream string
	args   *redis.XAddArgs
	// supplier and service label the counters at DISPATCH time. They are kept
	// here rather than re-derived because the message itself is not retained:
	// args already carries the marshalled bytes.
	supplier string
	service  string
	bytes    int
	// attempts counts the DISPATCHES whose write of this entry failed. It is the
	// net under the permanent-error classifier: an unknown error that always
	// fails would otherwise keep this entry at the head of the queue forever.
	attempts int
	// discardReason is set by writeChunk when it gives up on this entry, so the
	// caller records the reason writeChunk actually decided on rather than
	// classifying the error a second time and possibly differently.
	discardReason string
	// enqueuedAt is when Publish accepted this relay. It answers "how long was
	// this waiting" for a discard nobody classified, and it is the only way to
	// tell a queue that is slow from one that is stuck.
	enqueuedAt time.Time
}

// BatchingPublisher writes mined relays in batches instead of one round trip
// each.
//
// Publish VALIDATES, marshals and enqueues, then returns. A dispatcher writes
// what has accumulated with TxPipelined -- MULTI/EXEC, one round trip -- so the
// miner's blocked reader wakes once per chunk rather than once per relay.
//
// It implements transport.MinedRelayPublisher, so the four publish sites (HTTP
// through the relay processor, the HTTP fallback, WebSocket and gRPC) are
// unchanged: they already call the same interface and none of them does anything
// after Publish that depends on the write having landed.
type BatchingPublisher struct {
	logger       logging.Logger
	client       redis.UniversalClient
	streamPrefix string
	interval     time.Duration

	mu     sync.Mutex
	queue  []queued
	bytes  int
	closed bool

	// stop ends the dispatch loop; done reports that it has ended.
	stop context.CancelFunc
	done chan struct{}
}

// NewBatchingPublisher starts the dispatcher and returns a publisher that
// enqueues.
//
// ctx governs the DISPATCH LOOP and nothing else. The context a caller hands to
// Publish is used only to enqueue and is deliberately not kept: every transport
// attaches its own deadline to that context (websocket.go and
// relay_grpc_service.go both build one with context.WithTimeout over
// WithoutCancel), and carrying it into the dispatch would let the deadline of the
// OLDEST relay in a chunk cancel the EXEC for all of them -- up to
// maxChunkCommands relays, already served and signed, lost to a timeout that no
// longer means what it meant when it was set.
func NewBatchingPublisher(
	logger logging.Logger,
	client redis.UniversalClient,
	streamPrefix string,
	interval time.Duration,
) *BatchingPublisher {
	loopCtx, stop := context.WithCancel(context.Background())
	p := &BatchingPublisher{
		logger:       logging.ForComponent(logger, "batching_publisher"),
		client:       client,
		streamPrefix: streamPrefix,
		interval:     interval,
		stop:         stop,
		done:         make(chan struct{}),
	}
	go logging.RecoverGoRoutine(p.logger, "batching_publisher_dispatch", func(c context.Context) {
		defer close(p.done)
		p.run(c)
	})(loopCtx)
	return p
}

// Publish validates and enqueues. It does NOT write.
//
// The validation runs here on purpose: an invalid message never reaches a chunk,
// so it cannot make one permanently undispatchable, and the caller learns of the
// rejection exactly where it learns today.
func (p *BatchingPublisher) Publish(_ context.Context, msg *transport.MinedRelayMessage) error {
	stream, args, reason, err := prepareXAdd(p.streamPrefix, msg)
	if err != nil {
		recordPublishReject(p.logger, reason, serviceOf(msg), err.Error())
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		recordPublishReject(p.logger, rejectReasonPublisherShut, serviceOf(msg), "publisher already closed")
		return fmt.Errorf("publisher is closed")
	}
	n := approxBytes(args)
	p.queue = append(p.queue, queued{
		stream:     stream,
		args:       args,
		supplier:   msg.SupplierOperatorAddress,
		service:    msg.ServiceId,
		bytes:      n,
		enqueuedAt: time.Now(),
	})
	p.bytes += n
	return nil
}

// QueuedBytes is the payload the queue retains right now: what Publish has
// accepted and no chunk has taken yet. It carries no policy -- the relayer compares
// it with redis.batch_max_queued_mib to stop admitting, and nothing here refuses or
// drops a relay because of it.
func (p *BatchingPublisher) QueuedBytes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes
}

// approxBytes is the payload a queued entry retains. Only the marshalled relay
// is counted: it is the term that varies by orders of magnitude between a tiny
// JSON-RPC reply and a large one, and the fixed overhead per entry is noise
// beside it.
func approxBytes(args *redis.XAddArgs) int {
	if b, ok := args.Values.(map[string]interface{})["data"].([]byte); ok {
		return len(b)
	}
	return 0
}

// run dispatches on a fixed interval until ctx ends, then flushes what is left.
func (p *BatchingPublisher) run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.finalFlush()
			return
		case <-ticker.C:
			p.dispatchAll(ctx)
		}
	}
}

// finalFlush writes everything left, on a context DETACHED from the one that
// just ended.
//
// Inheriting the shutdown cancellation would kill the flush at the only moment
// the queue holds a full backlog: proxy.Close stops the publish subpool with
// StopAndWait, which drains every queued task into this publisher just before
// this runs. A flush that dies there loses all of it.
func (p *BatchingPublisher) finalFlush() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), finalFlushTimeout)
	defer cancel()
	p.dispatchAll(ctx)
}

// finalFlushTimeout bounds the shutdown flush. It is generous on purpose: the
// backlog it drains is whatever the publish subpool held, not one interval's
// worth, and the cost of being too short is losing served relays while the cost
// of being too long is a slower shutdown.
const finalFlushTimeout = 30 * time.Second

// dispatchAll writes every queued relay, one chunk per round trip.
func (p *BatchingPublisher) dispatchAll(ctx context.Context) {
	for {
		chunk := p.takeChunk()
		if len(chunk) == 0 {
			return
		}
		retry, discard, err := p.writeChunk(ctx, chunk)
		for _, q := range discard {
			p.recordDiscard(ctx, q, err)
		}
		if err != nil {
			// Only what did NOT reach the stream goes back, at the front, to be
			// retried next tick. It is not dropped and it is not counted as
			// published: every relay in it was served, signed and answered to a
			// client, and the write simply did not happen.
			//
			// Requeueing the WHOLE chunk is what this used to do, and it was
			// wrong whenever the EXEC succeeded with one XADD failing inside it
			// (see writeChunk): the relays that had already landed were written
			// a second time and counted a second time, so published climbed
			// above served. The comment that stood here asserted "it is not
			// counted as published", which was false in exactly that case.
			if len(retry) > 0 {
				p.requeueFront(retry)
			}
			p.logger.Warn().
				Err(err).
				Int("relays", len(chunk)).
				Int("requeued", len(retry)).
				Int("discarded", len(discard)).
				Msg("batch dispatch failed; the unwritten relays stay queued and will be retried")
			return
		}
	}
}

// takeChunk removes the next chunk from the queue, WITHOUT splitting a stream
// across chunks.
//
// Keeping a stream whole is the point of batching: the miner's reader for that
// supplier wakes once per EXEC that touches its stream, so a stream split over
// two chunks wakes it twice and gives back what the batch bought. A single
// stream larger than the limits on its own is split anyway -- the cap on how
// long one EXEC blocks Redis wins over the wake-up count.
func (p *BatchingPublisher) takeChunk() []queued {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return nil
	}

	cut := len(p.queue)
	cmds, bytes := 0, 0
	for i, q := range p.queue {
		if (cmds+1 > maxChunkCommands || bytes+q.bytes > maxChunkBytes) && cmds > 0 {
			// Cut back to where the trailing stream begins, so no stream is
			// split. If that would empty the chunk, this one stream is larger
			// than a chunk on its own and has to be split: the cap on how long a
			// single EXEC blocks Redis wins over the wake-up count.
			cut = streamStart(p.queue[:i])
			if cut == 0 {
				cut = i
			}
			break
		}
		cmds++
		bytes += q.bytes
	}

	// COPIED out, not resliced: requeueFront prepends a failed chunk to the
	// queue, and a chunk that still shared the queue's backing array would be
	// overwritten by that append.
	chunk := make([]queued, cut)
	copy(chunk, p.queue[:cut])
	p.queue = append([]queued(nil), p.queue[cut:]...)
	p.bytes -= bytes0(chunk)
	return chunk
}

// streamStart returns the index where the LAST stream in prefix begins, which is
// the largest cut point leaving every stream in the chunk complete.
func streamStart(prefix []queued) int {
	if len(prefix) == 0 {
		return 0
	}
	last := prefix[len(prefix)-1].stream
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i].stream != last {
			return i + 1
		}
	}
	return 0
}

func bytes0(chunk []queued) int {
	n := 0
	for _, q := range chunk {
		n += q.bytes
	}
	return n
}

// requeueFront puts a failed chunk back at the head, preserving arrival order.
func (p *BatchingPublisher) requeueFront(chunk []queued) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(chunk, p.queue...)
	p.bytes += bytes0(chunk)
}

// writeChunk issues one MULTI/EXEC and returns the entries that still have to be
// written, which is NOT always the whole chunk.
//
// TxPipelined and not Pipelined: MEASURED against Redis 8.10.1, a blocked
// XREADGROUP receives all of a MULTI's entries in one wake-up, and receives ONE
// when the same XADDs arrive as a plain pipeline. The whole point of the batch is
// that second number.
//
// Every command's own error is checked. An EXEC can succeed while an individual
// XADD inside it failed -- a WRONGTYPE on one stream does not abort the rest --
// so trusting the EXEC's error alone would report a chunk as written while some
// of its relays never landed.
func (p *BatchingPublisher) writeChunk(ctx context.Context, chunk []queued) (retry, discard []queued, err error) {
	cmds := make([]*redis.StringCmd, len(chunk))
	// The pipeline's own error is deliberately discarded; the paragraph below
	// says why it cannot be used to decide anything here.
	_, _ = p.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, q := range chunk {
			cmds[i] = pipe.XAdd(ctx, q.args)
		}
		return nil
	})

	// Everything is decided per COMMAND, including a failure of the EXEC itself:
	// when the pipeline fails at transport level, go-redis stamps that error onto
	// every command in it (generalProcessPipeline -> setCmdsErr, redis.go, and
	// again on the retries-exhausted path), and when only some commands fail the
	// server reports them individually. So a command with no error of its own
	// reached the stream, whatever the pipeline returned.
	//
	// Reading the pipeline's returned error instead is what an earlier version of
	// this function did, and it is wrong: TxPipelined returns cmdsFirstErr(cmds),
	// so ONE failing XADD makes the whole chunk look unwritten.
	//
	// The corner this does NOT cover: an EXEC that Redis executed and whose reply
	// never came back is indistinguishable from one that never ran, so retrying
	// it writes twice. That is the at-least-once edge of writing over a network,
	// declared rather than solved.

	var firstErr error
	for i, cmd := range cmds {
		cmdErr := cmd.Err()
		if cmdErr == nil || errors.Is(cmdErr, redis.Nil) {
			// Counted HERE and not at enqueue: publishedTotal is the only counter
			// in this repository that means "reached the stream", and it is what
			// the relayer-side counters are measured against.
			publishedTotal.WithLabelValues(chunk[i].supplier, chunk[i].service).Inc()
			continue
		}

		publishErrorsTotal.WithLabelValues(chunk[i].supplier, chunk[i].service).Inc()
		if firstErr == nil {
			firstErr = fmt.Errorf("XADD to %s: %w", chunk[i].stream, cmdErr)
		}

		// Only THIS entry is in question. Its siblings in the same EXEC that
		// succeeded are already in the stream: an EXEC reports per-command errors
		// and does not roll back the commands that succeeded, which is the whole
		// reason each cmd.Err() is read.
		entry := chunk[i]
		entry.attempts++
		switch {
		case IsWrongTypeError(cmdErr):
			entry.discardReason = discardReasonWrongType
			discard = append(discard, entry)
		case entry.attempts >= maxPublishAttempts:
			entry.discardReason = discardReasonAttemptsExhausted
			discard = append(discard, entry)
		default:
			// Everything else goes back, including the ordinary case: a pool
			// timeout that outlived go-redis's own retries is transient, and
			// e0667eb exists because dropping a relay there lost served work to
			// a condition that passes.
			retry = append(retry, entry)
		}
	}
	return retry, discard, firstErr
}

// Close stops the dispatcher and waits for the final flush.
func (p *BatchingPublisher) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	p.stop()
	<-p.done

	// What the final flush could not write. dispatchAll puts a failed chunk
	// back on the queue and returns, and after the flush nothing will ever read
	// that queue again -- so this is the last place the loss can be observed at
	// all, and it has to be counted here or it is invisible.
	if abandoned := p.drainAbandoned(); len(abandoned) > 0 {
		total := 0
		for _, q := range abandoned {
			shutdownAbandonedRelays.WithLabelValues(q.supplier, q.service).Inc()
			total++
		}
		p.logger.Error().
			Int("relays", total).
			Msg("batching publisher closed with relays it never wrote; they were served and are lost")
		return fmt.Errorf("batching publisher abandoned %d unwritten relays", total)
	}

	p.logger.Info().Msg("batching publisher closed")
	return nil
}

// drainAbandoned empties the queue and returns what was in it. It empties on
// purpose: the entries are counted as lost exactly once, and a second Close
// must not count them again.
func (p *BatchingPublisher) drainAbandoned() []queued {
	p.mu.Lock()
	defer p.mu.Unlock()
	left := p.queue
	p.queue = nil
	p.bytes = 0
	return left
}

var _ transport.MinedRelayPublisher = (*BatchingPublisher)(nil)
