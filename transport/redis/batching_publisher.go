package redis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alitto/pond/v2"
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

	mu sync.Mutex
	// queue[head:] waits to be written. The slots before head were taken by a
	// chunk and are zeroed; compactLocked reclaims them.
	queue  []queued
	head   int
	bytes  int
	closed bool
	// workers is how many chunks one round of a dispatch writes at once, and pool
	// runs them.
	workers int
	pool    pond.Pool
	// ledger holds the served cost written with the XADDs; nil writes none.
	ledger *ChargeLedger

	// lastSuccess is the UnixNano of the last round trip Redis answered: a PING
	// on the heartbeat tick or an EXEC. Admission reads it to stop when the
	// dispatcher can no longer write.
	lastSuccess atomic.Int64
	// inFlightSince is the UnixNano at which the round of writes now in flight
	// started, and 0 while none is. Every chunk of a round starts together, so it
	// is also when the oldest write in flight started.
	inFlightSince atomic.Int64
	now           func() time.Time

	// health, when set, pauses dispatch while Redis cannot take writes.
	health *StoreHealth

	// stop ends the dispatch loop; done reports that it has ended.
	stop context.CancelFunc
	done chan struct{}
}

// BatchingPublisherOption configures a BatchingPublisher at construction.
type BatchingPublisherOption func(*BatchingPublisher)

// WithStoreHealth makes the dispatcher hold everything queued while health says
// Redis cannot take writes: nothing is written, dropped or charged an attempt
// until it reopens.
func WithStoreHealth(health *StoreHealth) BatchingPublisherOption {
	return func(p *BatchingPublisher) {
		p.health = health
	}
}

// WithDispatchWorkers sets how many chunks one dispatch round writes at once.
// Values below two keep a single writer.
func WithDispatchWorkers(n int) BatchingPublisherOption {
	return func(p *BatchingPublisher) {
		if n > 1 {
			p.workers = n
		}
	}
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
	opts ...BatchingPublisherOption,
) *BatchingPublisher {
	loopCtx, stop := context.WithCancel(context.Background())
	p := &BatchingPublisher{
		logger:       logging.ForComponent(logger, "batching_publisher"),
		client:       client,
		streamPrefix: streamPrefix,
		interval:     interval,
		stop:         stop,
		done:         make(chan struct{}),
		now:          time.Now,
		workers:      1,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.pool = pond.NewPool(p.workers)
	p.markSuccess()
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

// approxBytes is the heap a queued entry retains: the marshalled relay, the
// allocator rounding it up to its size class (up to an eighth more), and a fixed
// overhead per entry (the XADD arguments, their map and the queue slot).
// Counting the relay alone undercounted what the queue held 5.5x for a 64 B
// relay and 1.13x for a 64 KiB one, measured with runtime.MemStats over 4,000
// entries (TestApproxBytesTracksTheRetainedHeap); a gate on that count admitted
// past its limit.
func approxBytes(args *redis.XAddArgs) int {
	b, _ := args.Values.(map[string]interface{})["data"].([]byte)
	return len(b) + len(b)/8 + queuedEntryOverheadBytes
}

// queuedEntryOverheadBytes is the measured heap a queued entry retains besides its
// relay bytes: 730-746 B at every relay size measured.
const queuedEntryOverheadBytes = 730

// heartbeatInterval is how often the dispatcher proves Redis answers it. Fixed
// and independent of the batch interval: admission closes after a few of these
// without an answer, and tying it to a 10 s batch interval would keep serving
// blind for 30 s.
const heartbeatInterval = time.Second

// SetChargeLedger makes every dispatch write the ledger's charges alongside the
// XADDs.
func (p *BatchingPublisher) SetChargeLedger(ledger *ChargeLedger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ledger = ledger
}

// LastSuccess is the time admission measures the dispatcher's progress from:
// when Redis last answered it, or, while writes are in flight, when the oldest of
// them started if that is earlier.
//
// An answer alone is not progress with several workers: one worker can hang on
// its EXEC while another is answered, and the charges riding in the hung write
// stay unwritten, invisible to every other replica. So a write in flight for
// longer than admission tolerates closes admission even if Redis answers the
// rest.
func (p *BatchingPublisher) LastSuccess() time.Time {
	last := p.lastSuccess.Load()
	if since := p.inFlightSince.Load(); since != 0 && since < last {
		last = since
	}
	return time.Unix(0, last)
}

func (p *BatchingPublisher) markSuccess() {
	p.lastSuccess.Store(p.now().UnixNano())
}

// heartbeat marks success when Redis answers a PING. It runs on the dispatcher's
// goroutine, so a dispatch stuck on a slow Redis also stops the marks.
//
// It PINGs only while no write is in flight: then the writes are what shows
// whether the dispatcher progresses, and a PING answered next to a hung write
// would only prove that Redis answers.
func (p *BatchingPublisher) heartbeat(ctx context.Context) {
	if p.inFlightSince.Load() != 0 {
		return
	}
	if err := p.client.Ping(ctx).Err(); err == nil {
		p.markSuccess()
	}
}

// run dispatches on a fixed interval until ctx ends, then flushes what is left.
func (p *BatchingPublisher) run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	beat := time.NewTicker(heartbeatInterval)
	defer beat.Stop()

	for {
		select {
		case <-ctx.Done():
			p.finalFlush()
			return
		case <-beat.C:
			p.heartbeat(ctx)
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

// dispatchAll writes every queued relay and every pending charge, one chunk per
// round trip.
//
// A charge rides in the first chunk that carries XADDs of its supplier and has
// room for its two commands; what does not fit, or whose supplier mined nothing
// this tick, goes in chunks of charges alone at the end. A charge's INCRBY and
// EXPIRE NX are never split across two EXECs.
func (p *BatchingPublisher) dispatchAll(ctx context.Context) {
	// A write Redis refuses for memory is refused for every entry, and each
	// refusal would spend one of an entry's attempts: the queue waits instead.
	if !p.health.Operable() {
		return
	}
	p.mu.Lock()
	ledger := p.ledger
	p.mu.Unlock()
	var charges chargesBySupplier
	if ledger != nil {
		charges = groupCharges(ledger.takeAll())
	}
	// Charges taken and never sent go back when the tick stops early.
	defer func() {
		for _, c := range charges.rest() {
			ledger.untake(c)
		}
	}()

	for {
		// A round is at most one chunk per worker, each with the charges of its
		// suppliers attached. Taking chunks and attaching charges stay on this
		// goroutine, so no two writes can carry the same charge.
		round := make([]*dispatchJob, 0, p.workers)
		for len(round) < p.workers {
			chunk := p.takeChunk()
			if len(chunk) == 0 {
				break
			}
			round = append(round, &dispatchJob{
				chunk:   chunk,
				charges: charges.attach(chunk, maxChunkCommands-len(chunk)),
			})
		}
		if len(round) == 0 {
			break
		}
		p.writeRound(ctx, round, ledger)

		failed := false
		// Back to front: requeueFront puts what it gets at the head, so the last
		// job goes back first and the queue keeps its arrival order.
		for i := len(round) - 1; i >= 0; i-- {
			job := round[i]
			chunk, retry, discard, err := job.chunk, job.retry, job.discard, job.err
			for _, q := range discard {
				p.recordDiscard(ctx, q, err)
			}
			if err == nil {
				continue
			}
			failed = true
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
		}
		if failed {
			return
		}
	}

	for {
		part := charges.take(maxChunkCommands / 2)
		if len(part) == 0 {
			return
		}
		if _, _, err := p.writeChunk(ctx, nil, part, ledger); err != nil {
			p.logger.Warn().Err(err).Int("charges", len(part)).
				Msg("charge dispatch failed; the charges not sent stay pending")
			return
		}
	}
}

// dispatchJob is one chunk of a round, the charges that ride in its EXEC, and
// what writing it left to retry or to discard.
type dispatchJob struct {
	chunk   []queued
	charges []Charge
	retry   []queued
	discard []queued
	err     error
	// started is set by the worker before it writes, so a job the pool never ran
	// can be told from one whose worker panicked.
	started bool
}

// errDispatchWorkerStopped is the outcome of a job whose worker stopped before
// reporting its write.
var errDispatchWorkerStopped = errors.New("dispatch worker stopped before reporting the write")

// writeRound writes the jobs of a round at once, one worker each, and records
// each outcome on its job. A job starts out as unwritten, so one whose worker
// panics goes back to the queue instead of being taken as written.
func (p *BatchingPublisher) writeRound(ctx context.Context, round []*dispatchJob, ledger *ChargeLedger) {
	p.inFlightSince.Store(p.now().UnixNano())
	defer p.inFlightSince.Store(0)
	if len(round) == 1 {
		job := round[0]
		job.retry, job.discard, job.err = p.writeChunk(ctx, job.chunk, job.charges, ledger)
		return
	}
	group := p.pool.NewGroup()
	for _, job := range round {
		job.retry, job.err = job.chunk, errDispatchWorkerStopped
		group.Submit(func() {
			job.started = true
			job.retry, job.discard, job.err = p.writeChunk(ctx, job.chunk, job.charges, ledger)
		})
	}
	// The tasks return nothing; Wait reports a panic in one of them, and that
	// job keeps the unwritten outcome it started with. Wait also waits for every
	// task still running, so the jobs are read below only once no worker writes
	// them.
	if err := group.Wait(); err != nil {
		p.logger.Error().Err(err).Msg("a batch dispatch worker stopped before reporting its write")
	}
	for _, job := range round {
		if !errors.Is(job.err, errDispatchWorkerStopped) {
			continue
		}
		// Nothing reported the charges of this job either way. A job that never
		// started sent nothing, so its charges go back. One whose worker panicked
		// may have sent its EXEC, so, like an EXEC whose reply never came back,
		// its charges are not written again.
		for _, c := range job.charges {
			if job.started {
				chargeWriteFailures.WithLabelValues("exec_unknown").Inc()
				ledger.forget(c)
			} else {
				ledger.untake(c)
			}
		}
	}
}

// chargesBySupplier keeps charges grouped by the supplier whose stream they
// prefer, in the order suppliers were first seen.
type chargesBySupplier struct {
	order []string
	by    map[string][]Charge
}

func groupCharges(all []Charge) chargesBySupplier {
	g := chargesBySupplier{by: make(map[string][]Charge)}
	for _, c := range all {
		if _, seen := g.by[c.Supplier]; !seen {
			g.order = append(g.order, c.Supplier)
		}
		g.by[c.Supplier] = append(g.by[c.Supplier], c)
	}
	return g
}

// attach removes and returns the charges of the chunk's suppliers that fit in
// room commands, two per charge.
func (g *chargesBySupplier) attach(chunk []queued, room int) []Charge {
	var out []Charge
	for _, q := range chunk {
		for room >= 2 && len(g.by[q.supplier]) > 0 {
			out = append(out, g.by[q.supplier][0])
			g.by[q.supplier] = g.by[q.supplier][1:]
			room -= 2
		}
		if room < 2 {
			break
		}
	}
	return out
}

// take removes and returns up to n charges, whatever their supplier.
func (g *chargesBySupplier) take(n int) []Charge {
	var out []Charge
	for _, s := range g.order {
		for len(out) < n && len(g.by[s]) > 0 {
			out = append(out, g.by[s][0])
			g.by[s] = g.by[s][1:]
		}
	}
	return out
}

func (g *chargesBySupplier) rest() []Charge {
	return g.take(int(^uint(0) >> 1))
}

// transportFailure reports an error that did not come back from Redis, so the
// command's outcome is unknown. go-redis stamps a pipeline's transport error on
// every command in it; an error Redis returned for one command is a redis.Error.
func transportFailure(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) {
		return false
	}
	var redisErr redis.Error
	return !errors.As(err, &redisErr)
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
	live := p.queue[p.head:]
	if len(live) == 0 {
		return nil
	}

	cut := len(live)
	cmds, bytes := 0, 0
	for i, q := range live {
		if (cmds+1 > maxChunkCommands || bytes+q.bytes > maxChunkBytes) && cmds > 0 {
			// Cut back to where the trailing stream begins, so no stream is
			// split. If that would empty the chunk, this one stream is larger
			// than a chunk on its own and has to be split: the cap on how long a
			// single EXEC blocks Redis wins over the wake-up count.
			cut = streamStart(live[:i])
			if cut == 0 {
				cut = i
			}
			break
		}
		cmds++
		bytes += q.bytes
	}

	// The chunk is COPIED out: requeueFront writes a failed chunk back into the
	// slots in front of the head, and a chunk still sharing those slots would be
	// overwritten by it. The rest of the queue is NOT copied: the head moves past
	// the chunk and the slots it leaves are zeroed, so they stop holding the
	// relays' bytes, which keeps a take O(chunk) under the lock Publish waits on.
	chunk := make([]queued, cut)
	copy(chunk, live[:cut])
	clear(live[:cut])
	p.head += cut
	p.compactLocked()
	p.bytes -= bytes0(chunk)
	return chunk
}

// compactLocked keeps the zeroed slots before the head from growing without
// bound. An empty queue starts over at the front of its array; otherwise, once
// the taken slots are more than half the array, the live entries move to the
// front. An entry moves at most once per halving, so takes stay O(chunk)
// amortized.
func (p *BatchingPublisher) compactLocked() {
	if p.head == len(p.queue) {
		p.queue = p.queue[:0]
		p.head = 0
		return
	}
	if p.head <= len(p.queue)/2 {
		return
	}
	n := copy(p.queue, p.queue[p.head:])
	clear(p.queue[n:])
	p.queue = p.queue[:n]
	p.head = 0
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
// It fills the free slots in front of the head when they are enough, and
// otherwise builds a new array with the chunk first.
func (p *BatchingPublisher) requeueFront(chunk []queued) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.head >= len(chunk) {
		p.head -= len(chunk)
		copy(p.queue[p.head:], chunk)
	} else {
		live := p.queue[p.head:]
		requeued := make([]queued, 0, len(chunk)+len(live))
		requeued = append(requeued, chunk...)
		p.queue = append(requeued, live...)
		p.head = 0
	}
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
func (p *BatchingPublisher) writeChunk(ctx context.Context, chunk []queued, charges []Charge, ledger *ChargeLedger) (retry, discard []queued, err error) {
	cmds := make([]*redis.StringCmd, len(chunk))
	incrs := make([]*redis.IntCmd, len(charges))
	expires := make([]*redis.BoolCmd, len(charges))
	// The pipeline's own error is deliberately discarded; the paragraph below
	// says why it cannot be used to decide anything here.
	_, _ = p.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, q := range chunk {
			cmds[i] = pipe.XAdd(ctx, q.args)
		}
		for i, c := range charges {
			incrs[i] = pipe.IncrBy(ctx, c.Key, c.Amount)
			expires[i] = pipe.ExpireNX(ctx, c.Key, c.TTL)
		}
		return nil
	})

	// Redis refuses a write for memory when the command is QUEUED, and a MULTI
	// with a refused command is discarded whole on EXEC (EXECABORT): measured
	// against Redis 8.10.0, go-redis then reports OOM on every refused command and
	// EXECABORT on the others, and nothing in the transaction was written. So one
	// OOM means the chunk did not happen. It is not Redis answering the dispatcher
	// (no markSuccess), no relay reached the stream, and no entry or charge spends
	// an attempt: the store is full, not the entry wrong, and spending attempts is
	// how served relays were discarded as attempts_exhausted.
	if chunkRefusedForMemory(cmds, incrs) {
		for _, c := range charges {
			ledger.untake(c)
		}
		for _, q := range chunk {
			publishErrorsTotal.WithLabelValues(q.supplier, q.service).Inc()
		}
		return chunk, nil, fmt.Errorf("redis refused the chunk for memory: %w", errStoreOutOfMemory)
	}

	answered := false
	for _, cmd := range cmds {
		answered = answered || !transportFailure(cmd.Err())
	}
	for i, c := range charges {
		incrErr := incrs[i].Err()
		switch {
		case incrErr == nil:
			answered = true
			if expErr := expires[i].Err(); expErr != nil {
				chargeWriteFailures.WithLabelValues("expire_failed").Inc()
			}
			ledger.commit(c, incrs[i].Val())
		case transportFailure(incrErr):
			chargeWriteFailures.WithLabelValues("exec_unknown").Inc()
			ledger.forget(c)
			if err == nil {
				err = fmt.Errorf("INCRBY %s: %w", c.Key, incrErr)
			}
		default:
			answered = true
			if !ledger.retry(c) {
				chargeWriteFailures.WithLabelValues("attempts_exhausted").Inc()
				p.logger.Warn().Err(incrErr).Str("key", c.Key).
					Msg("consumed counter refused every INCRBY; its charge is dropped")
			}
		}
	}
	if answered {
		p.markSuccess()
	}

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

	firstErr := err
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

		// Only THIS entry is in question. An error Redis raises while EXECUTING a
		// command (WRONGTYPE) leaves its siblings written: EXEC does not roll back,
		// which is why each cmd.Err() is read. An error raised while QUEUING one
		// (OOM) discards the whole transaction instead, and never gets here: see
		// chunkRefusedForMemory above.
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

// errStoreOutOfMemory marks a chunk Redis refused for memory.
var errStoreOutOfMemory = errors.New("store out of memory")

// chunkRefusedForMemory reports whether Redis refused any command of the chunk
// for memory, which discards the whole MULTI.
func chunkRefusedForMemory(xadds []*redis.StringCmd, incrs []*redis.IntCmd) bool {
	for _, cmd := range xadds {
		if redis.IsOOMError(cmd.Err()) {
			return true
		}
	}
	for _, cmd := range incrs {
		if redis.IsOOMError(cmd.Err()) {
			return true
		}
	}
	return false
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
	// After the dispatch loop: its final flush still writes through the pool.
	p.pool.StopAndWait()

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
	left := p.queue[p.head:]
	p.queue = nil
	p.head = 0
	p.bytes = 0
	return left
}

var _ transport.MinedRelayPublisher = (*BatchingPublisher)(nil)
