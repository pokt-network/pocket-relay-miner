package redis

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// seq is the order Publish accepted this relay in, counted from 1. A drain
	// sends a chunk short of the limits only for entries queued before it began
	// (see takeChunkBefore).
	seq uint64
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
	// enqueued is how many relays Publish has accepted; the last one's seq.
	enqueued uint64
	// workers is how many chunks a dispatch keeps in flight at once, and pool
	// runs them.
	workers int
	pool    pond.Pool
	// ledger holds the served cost written with the XADDs; nil writes none.
	ledger *ChargeLedger

	// lastSuccess is the last round trip Redis answered: a PING on the heartbeat
	// tick or an EXEC. It is nil until the first one is answered, so a dispatcher
	// that has never reached Redis refuses instead of coasting.
	//
	// It holds the instant itself and not its UnixNano on purpose. An instant
	// stored as an integer loses its monotonic reading, so comparing it against a
	// later time.Now() measures the WALL clock, and a clock jump alone closes
	// admission with Redis healthy and the dispatcher writing. Measured
	// 2026-09-20 on a cold start: a +3.34s jump refused 1203 relays.
	lastSuccess atomic.Pointer[time.Time]
	// inFlight holds, per write slot, when the write now in it started, and nil
	// while the slot is free. Writes start and end independently, so the oldest
	// write in flight is the minimum over the slots: no single mark can stand for
	// it, since a newer start would overwrite an older one still hung and a newer
	// end would clear it.
	inFlight []atomic.Pointer[time.Time]
	// silenceBudget is how long the dispatcher may go without reaching Redis
	// before admission closes; dispatcherSilenceBudget derives it at construction
	// from the client that actually runs.
	silenceBudget time.Duration
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

// WithDispatchWorkers sets how many chunks a dispatch keeps in flight at once.
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
	p.inFlight = make([]atomic.Pointer[time.Time], p.workers)
	p.silenceBudget = dispatcherSilenceBudget(p.logger, client)
	// Nothing is marked here. Marking success at construction handed admission a
	// mark nobody earned: with Redis unreachable from the start the relayer
	// admitted for the whole budget on a round trip that never happened.
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
	p.enqueued++
	p.queue = append(p.queue, queued{
		stream:     stream,
		args:       args,
		supplier:   msg.SupplierOperatorAddress,
		service:    msg.ServiceId,
		bytes:      n,
		enqueuedAt: time.Now(),
		seq:        p.enqueued,
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

// minDispatcherSilence is the floor of how long the dispatcher may go without
// reaching Redis before admission closes. Operator decision (Jorge, 2026-09-20:
// "10s MINIMO"): under load, ordinary scheduling contention keeps a healthy
// dispatcher from marking for longer than a small budget tolerates, and every
// relay refused there was one a healthy fleet would have served and charged.
const minDispatcherSilence = 10 * time.Second

// healthyWriteBudget is how long a HEALTHY write may stay in flight. While one
// is, admission measures from its start, so its duration is what the silence
// budget has to cover on top of the pool timeout.
//
// It is a declared allowance and NOT a measurement: a healthy write is one
// TxPipelined round trip of at most maxChunkCommands, milliseconds in practice.
// The number never decides the budget on its own -- with go-redis's 6s pool
// timeout the sum stays under the floor for any value up to 3s. What it does is
// keep the budget growing with an operator who raises the pool timeout.
const healthyWriteBudget = time.Second

// dispatcherSilenceBudget derives the budget from the client that actually runs
// rather than from config: a pool timeout left unset reaches the client as
// go-redis's own default, and config would have published the zero.
//
// The batch interval is deliberately NOT a term: the heartbeat has its own
// ticker, and what bounds the budget is how long a dispatch keeps the goroutine,
// not how often one starts.
//
// ok=false means this client type cannot be asked, and then the floor is the
// whole budget. It says so once, at startup: without the line the max() returns
// the right number for the wrong reason and nobody learns the term was missing.
func dispatcherSilenceBudget(logger logging.Logger, client redis.UniversalClient) time.Duration {
	pool, ok := EffectivePoolOf(client)
	if !ok {
		logger.Warn().Msg("redis client cannot report its pool timeout; the dispatcher silence budget falls back to its floor")
		return minDispatcherSilence
	}
	// One heartbeat interval and not two: a time.Ticker buffers one tick, so a
	// beat delayed by a round is not lost, it fires late.
	budget := healthyWriteBudget + pool.PoolTimeout + heartbeatInterval
	if budget < minDispatcherSilence {
		return minDispatcherSilence
	}
	return budget
}

// errDispatcherNeverReachedRedis refuses before the first answered round trip:
// nothing served could be charged yet.
var errDispatcherNeverReachedRedis = errors.New("batch dispatcher has not reached redis since startup")

// errDispatcherSilent refuses once the dispatcher stops reaching Redis.
var errDispatcherSilent = errors.New("batch dispatcher stopped reaching redis")

// SetChargeLedger makes every dispatch write the ledger's charges alongside the
// XADDs.
func (p *BatchingPublisher) SetChargeLedger(ledger *ChargeLedger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ledger = ledger
}

// DispatcherHealthy answers the one question admission asks before serving a
// relay: can what is served now still be charged? Yes while the dispatcher keeps
// reaching Redis, and no with the reason once it stops.
//
// The decision is made HERE, next to the instants, instead of handing an instant
// out. An instant that crosses this boundary has to be carried as something, and
// carrying it as an integer of UnixNano is what let a wall-clock jump close
// admission while Redis was answering.
//
// Progress is measured from when Redis last answered, or, while writes are in
// flight, from when the oldest of them started if that is earlier. An answer
// alone is not progress with several workers: one worker can hang on its EXEC
// while another is answered, and the charges riding in the hung write stay
// unwritten, invisible to every other replica. So a write in flight for longer
// than the budget closes admission even if Redis answers the rest.
func (p *BatchingPublisher) DispatcherHealthy() (bool, error) {
	last := p.lastSuccess.Load()
	if last == nil {
		return false, errDispatcherNeverReachedRedis
	}
	measuredFrom := *last
	if since := p.oldestInFlight(); !since.IsZero() && since.Before(measuredFrom) {
		measuredFrom = since
	}
	if age := p.now().Sub(measuredFrom); age > p.silenceBudget {
		return false, fmt.Errorf("%w: last answer %s ago, budget %s",
			errDispatcherSilent, age.Round(time.Millisecond), p.silenceBudget)
	}
	return true, nil
}

func (p *BatchingPublisher) markSuccess() {
	at := p.now()
	p.lastSuccess.Store(&at)
}

// oldestInFlight is when the oldest write now in flight started, and the zero
// time while none is.
func (p *BatchingPublisher) oldestInFlight() time.Time {
	var oldest time.Time
	for i := range p.inFlight {
		if at := p.inFlight[i].Load(); at != nil && (oldest.IsZero() || at.Before(oldest)) {
			oldest = *at
		}
	}
	return oldest
}

// writesInFlight is how many write slots are taken right now.
func (p *BatchingPublisher) writesInFlight() int {
	n := 0
	for i := range p.inFlight {
		if p.inFlight[i].Load() != nil {
			n++
		}
	}
	return n
}

// heartbeat marks success when Redis answers a PING. It runs on the dispatcher's
// goroutine, so a dispatch stuck on a slow Redis also stops the marks.
//
// It PINGs only while no write is in flight: then the writes are what shows
// whether the dispatcher progresses, and a PING answered next to a hung write
// would only prove that Redis answers.
func (p *BatchingPublisher) heartbeat(ctx context.Context) {
	if p.writesInFlight() > 0 {
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

	// The first beat is taken now rather than a tick from now. Construction
	// marks nothing, so until Redis answers once, admission is closed: waiting
	// out a full interval to ask would keep it closed for that interval with
	// Redis healthy.
	p.heartbeat(ctx)

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
// round trip, keeping up to p.workers writes in flight.
//
// It is ONE dispatcher, this goroutine, and N write slots. Taking chunks,
// attaching charges, deciding what goes back and recording what is discarded all
// stay here, so no two writes can carry the same charge and the queue is only
// ever touched from one place. The slots only write: each write is a task on
// p.pool and reports back on a channel, and as soon as one reports, its slot
// takes the next chunk. No write waits for another to finish, which is what a
// round did: with 1 MiB relays every chunk carries one relay, and a round of N
// such writes was as slow as its slowest EXEC.
//
// A charge rides in the first chunk that carries XADDs of its supplier and has
// room for its two commands; what does not fit, or whose supplier mined nothing,
// goes in chunks of charges alone once the queue is drained. A charge's INCRBY
// and EXPIRE NX are never split across two EXECs.
func (p *BatchingPublisher) dispatchAll(ctx context.Context) {
	// A write Redis refuses for memory is refused for every entry, and each
	// refusal would spend one of an entry's attempts: the queue waits instead.
	if !p.health.Operable() {
		return
	}
	p.mu.Lock()
	ledger := p.ledger
	// What was queued up to here may leave in a chunk short of the limits; what
	// arrives later waits for a full chunk or for the next tick.
	cutoff := p.enqueued
	p.mu.Unlock()
	d := drain{cutoff: cutoff, charges: groupCharges(nil)}
	if ledger != nil {
		d.charges.add(ledger.takeAll())
	}
	// Charges taken and never sent go back when the tick stops early.
	defer func() {
		for _, c := range d.charges.rest() {
			ledger.untake(c)
		}
	}()

	results := make(chan *dispatchJob, p.workers)
	free := make([]int, 0, p.workers)
	for slot := p.workers - 1; slot >= 0; slot-- {
		free = append(free, slot)
	}
	var failed []*dispatchJob
	for taken := 0; ; {
		// Once a write has failed nothing more is taken: what failed goes back to
		// the head only after every write in flight has reported, see below.
		for len(failed) == 0 && len(free) > 0 {
			job := p.nextJob(&d)
			if job == nil {
				break
			}
			job.slot, job.order = free[len(free)-1], taken
			free = free[:len(free)-1]
			p.startWrite(ctx, job, results, ledger)
			taken++
		}
		if len(free) == p.workers {
			break
		}
		job := <-results
		p.finishWrite(ctx, job, ledger)
		free = append(free, job.slot)
		if job.err != nil {
			failed = append(failed, job)
		}
	}

	if len(failed) > 0 {
		// Only what did NOT reach the stream goes back, at the front, to be
		// retried next tick. It is not dropped and it is not counted as
		// published: every relay in it was served, signed and answered to a
		// client, and the write simply did not happen.
		//
		// Requeueing the WHOLE chunk is what this used to do, and it was wrong
		// whenever the EXEC succeeded with one XADD failing inside it (see
		// writeChunk): the relays that had already landed were written a second
		// time and counted a second time, so published climbed above served.
		//
		// Back to front in the order the chunks were TAKEN, not the order their
		// writes failed: requeueFront puts what it gets at the head, so the chunk
		// taken last goes back first and the queue keeps its arrival order. The
		// writes fail in whatever order Redis answers them.
		slices.SortFunc(failed, func(a, b *dispatchJob) int { return b.order - a.order })
		for _, job := range failed {
			if len(job.retry) > 0 {
				p.requeueFront(job.retry)
			}
			p.logger.Warn().
				Err(job.err).
				Int("relays", len(job.chunk)).
				Int("requeued", len(job.retry)).
				Int("discarded", len(job.discard)).
				Msg("batch dispatch failed; the unwritten relays stay queued and will be retried")
		}
		return
	}

	for {
		part := d.charges.take(maxChunkCommands / 2)
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

// drain is what one dispatchAll carries from one job to the next.
type drain struct {
	// cutoff is the last relay queued when the drain began: see takeChunkBefore.
	cutoff uint64
	// charges were taken from the ledger when the drain began. Each rides in a
	// chunk that carries XADDs of its supplier and has room for it; what does
	// not goes in chunks of charges alone once the queue is drained.
	charges chargesBySupplier
}

// nextJob is the next write of a drain, or nil when there is none to start now:
// the next chunk with the charges that ride in it.
func (p *BatchingPublisher) nextJob(d *drain) *dispatchJob {
	chunk := p.takeChunkBefore(d.cutoff)
	if len(chunk) == 0 {
		return nil
	}
	return &dispatchJob{chunk: chunk, charges: d.charges.attach(chunk, maxChunkCommands-len(chunk))}
}

// dispatchJob is one chunk in flight, the charges that ride in its EXEC, and
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
	// slot is the write slot the job holds, and startedAt when it took it.
	slot      int
	startedAt time.Time
	// order is when the job was taken within its dispatch, counted from 0.
	order int
}

// errDispatchWorkerStopped is the outcome of a job whose worker stopped before
// reporting its write.
var errDispatchWorkerStopped = errors.New("dispatch worker stopped before reporting the write")

// startWrite marks the job's slot as in flight and hands the write to the pool.
// The job reports on results exactly once, whatever happens to its worker.
func (p *BatchingPublisher) startWrite(ctx context.Context, job *dispatchJob, results chan<- *dispatchJob, ledger *ChargeLedger) {
	at := p.now()
	job.startedAt = at
	p.inFlight[job.slot].Store(&at)
	// A job starts out as unwritten, so one whose worker panics, or that the pool
	// never ran, goes back to the queue instead of being taken as written.
	job.retry, job.err = job.chunk, errDispatchWorkerStopped
	err := p.pool.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				p.logger.Error().Interface("panic", r).Msg("a batch dispatch worker stopped before reporting its write")
			}
			results <- job
		}()
		job.started = true
		job.retry, job.discard, job.err = p.writeChunk(ctx, job.chunk, job.charges, ledger)
	})
	if err != nil {
		// The pool is stopped, so nothing ran: the job reports its unwritten
		// outcome from here. results has room for one job per slot.
		results <- job
	}
}

// finishWrite records the outcome of a job that reported, on the dispatcher's
// goroutine. Its slot is cleared LAST: until then admission still measures from
// this write, which errs on the side of closing.
func (p *BatchingPublisher) finishWrite(ctx context.Context, job *dispatchJob, ledger *ChargeLedger) {
	dispatchWriteDuration.WithLabelValues(writeResult(job.err)).Observe(p.now().Sub(job.startedAt).Seconds())
	for _, q := range job.discard {
		p.recordDiscard(ctx, q, job.err)
	}
	if errors.Is(job.err, errDispatchWorkerStopped) {
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
	p.inFlight[job.slot].Store(nil)
}

// writeResult labels a finished write: oom if Redis refused it for memory,
// error if it failed otherwise, ok if it did not.
func writeResult(err error) string {
	switch {
	case errors.Is(err, errStoreOutOfMemory):
		return "oom"
	case err != nil:
		return "error"
	}
	return "ok"
}

// chargesBySupplier keeps charges grouped by the supplier whose stream they
// prefer, in the order suppliers were first seen.
type chargesBySupplier struct {
	order []string
	by    map[string][]Charge
}

func groupCharges(all []Charge) chargesBySupplier {
	g := chargesBySupplier{by: make(map[string][]Charge)}
	g.add(all)
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

// add appends charges, keeping suppliers in the order first seen.
func (g *chargesBySupplier) add(more []Charge) {
	for _, c := range more {
		if _, seen := g.by[c.Supplier]; !seen {
			g.order = append(g.order, c.Supplier)
		}
		g.by[c.Supplier] = append(g.by[c.Supplier], c)
	}
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

// takeChunkBefore removes the next chunk from the queue, WITHOUT splitting a
// stream across chunks.
//
// Keeping a stream whole is the point of batching: the miner's reader for that
// supplier wakes once per EXEC that touches its stream, so a stream split over
// two chunks wakes it twice and gives back what the batch bought. A single
// stream larger than the limits on its own is split anyway -- the cap on how
// long one EXEC blocks Redis wins over the wake-up count.
//
// A chunk short of the limits is taken only when its first entry was queued at
// or before cutoff; otherwise it returns nothing and the entries wait. A write
// slot frees every few milliseconds, and taking whatever has arrived by then
// would send chunks of two or three relays: more EXECs on Redis' one thread and
// more wake-ups of the miner for the same relays. So a drain empties what was
// queued when it began, and after that only a FULL chunk leaves before the next
// tick. Under saturation every chunk is full and nothing waits.
func (p *BatchingPublisher) takeChunkBefore(cutoff uint64) []queued {
	p.mu.Lock()
	defer p.mu.Unlock()
	live := p.queue[p.head:]
	if len(live) == 0 {
		return nil
	}

	cut := len(live)
	full := false
	cmds, bytes := 0, 0
	for i, q := range live {
		if (cmds+1 > maxChunkCommands || bytes+q.bytes > maxChunkBytes) && cmds > 0 {
			full = true
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
	// A chunk that reached a limit exactly is full too, and so is one relay
	// larger than a chunk on its own: that is every chunk of 1 MiB relays.
	full = full || cmds >= maxChunkCommands || bytes >= maxChunkBytes
	if !full && live[0].seq > cutoff {
		return nil
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
