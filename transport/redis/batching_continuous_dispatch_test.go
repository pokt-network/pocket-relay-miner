//go:build test

package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestASlowWriteDoesNotHoldBackTheOtherWorkers: two workers, one hangs on its
// EXEC. The other keeps taking chunks and writing them while the hung one is in
// flight, so the rest of the queue lands without waiting for it. A dispatch that
// waits for every write of a round before starting the next never sends the
// third pipeline.
func TestASlowWriteDoesNotHoldBackTheOtherWorkers(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	stuck := transport.SupplierStreamName(prefix, "pokt1slow")
	free := transport.SupplierStreamName(prefix, "pokt1free")
	hook := &holdPipelines{
		stuck:   stuck,
		entered: make(chan string, 8),
		release: make(chan struct{}),
		proceed: make(chan struct{}),
	}
	close(hook.proceed)
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })

	publishMined(t, p, "pokt1slow", 200, func(int) string { return "s1" })
	const freeChunks = 3
	publishMined(t, p, "pokt1free", freeChunks*maxChunkCommands, func(int) string { return "s1" })

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.dispatchAll(context.Background())
	}()
	release := onceCloser(hook.release)
	t.Cleanup(func() {
		release()
		<-done
	})

	entered := map[string]int{}
	for i := 0; i < 1+freeChunks; i++ {
		entered[waitFor(t, hook.entered, fmt.Sprintf("pipeline %d while the slow write is held", i+1))]++
	}
	require.Equal(t, 1, entered[stuck], "premise: the slow write is in flight")
	require.Equal(t, freeChunks, entered[free], "every chunk of the other stream was sent while the slow write was held")
	require.Eventually(t, func() bool {
		return client.XLen(context.Background(), free).Val() == freeChunks*maxChunkCommands
	}, 10*time.Second, time.Millisecond, "the other stream lands in full while the slow write is still in flight")
	require.Zero(t, client.XLen(context.Background(), stuck).Val(), "premise: the slow write is still held")

	release()
	waitFor(t, done, "the drain to end once the slow write is answered")
	require.Equal(t, int64(200), client.XLen(context.Background(), stuck).Val())
}

// holdStreams holds the pipeline writing to each stream in hold until that
// stream's channel closes, and every other pipeline until proceed closes. Each
// reports its stream on entered first.
type holdStreams struct {
	hold    map[string]chan struct{}
	entered chan string
	proceed chan struct{}
}

func (h *holdStreams) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (h *holdStreams) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (h *holdStreams) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		stream := firstXAddStream(cmds)
		h.entered <- stream
		if ch, ok := h.hold[stream]; ok {
			<-ch
		} else {
			<-h.proceed
		}
		return next(ctx, cmds)
	}
}

// TestTheOldestWriteInFlightKeepsAdmissionClosedWhileNewerOnesComeAndGo: a write
// that started at t0 hangs while newer writes start an hour later: some are
// answered, and one more hangs next to the old one. Admission measures from the
// OLDEST write still in flight, so it stays closed: neither the newer answers,
// nor the newer write in flight, nor the end of that newer write may stand in for
// the old write's progress. Once the old one is answered, admission opens again.
func TestTheOldestWriteInFlightKeepsAdmissionClosedWhileNewerOnesComeAndGo(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	old := transport.SupplierStreamName(prefix, "pokt1old")
	free := transport.SupplierStreamName(prefix, "pokt1new")
	newer := transport.SupplierStreamName(prefix, "pokt1newheld")
	hook := &holdStreams{
		hold:    map[string]chan struct{}{old: make(chan struct{}), newer: make(chan struct{})},
		entered: make(chan string, 8),
		proceed: make(chan struct{}),
	}
	client.AddHook(hook)
	t0 := time.Now()
	clock := newFakeClock(t0)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2), withClock(clock.now))
	t.Cleanup(func() { _ = p.Close() })

	publishMined(t, p, "pokt1old", 200, func(int) string { return "s1" })
	const freeChunks = 3
	publishMined(t, p, "pokt1new", freeChunks*maxChunkCommands, func(int) string { return "s1" })
	publishMined(t, p, "pokt1newheld", 200, func(int) string { return "s1" })

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.dispatchAll(context.Background())
	}()
	proceed := onceCloser(hook.proceed)
	releaseOld, releaseNewer := onceCloser(hook.hold[old]), onceCloser(hook.hold[newer])
	t.Cleanup(func() {
		proceed()
		releaseNewer()
		releaseOld()
		<-done
	})
	waitFor(t, hook.entered, "the first write")
	waitFor(t, hook.entered, "the second write")
	require.True(t, hasMonotonic(p.oldestInFlight()),
		"the mark a real dispatch stamps must carry a monotonic reading")

	later := t0.Add(time.Hour)
	clock.set(later)
	proceed()
	var last string
	for i := 1; i < freeChunks+1; i++ {
		last = waitFor(t, hook.entered, "a newer write, started while the old one is held")
	}
	require.Equal(t, newer, last, "premise: the newer held write started after every free chunk")
	require.Eventually(t, func() bool {
		return client.XLen(context.Background(), free).Val() == freeChunks*maxChunkCommands
	}, 10*time.Second, time.Millisecond, "premise: the newer free writes were answered")
	require.True(t, lastMark(p).Equal(later), "premise: the newer answers moved the mark")
	// Both held writes are in flight by construction: each entered the hook and
	// neither channel is closed. Not asked of the marks, which are under test.

	alive, err := p.DispatcherHealthy()
	require.False(t, alive,
		"a write started at %s is in flight next to the one started at %s: admission must measure from the "+
			"OLDEST write in flight, not the newest", later, t0)
	require.ErrorIs(t, err, errDispatcherSilent)

	releaseNewer()
	// Fewer than two marks: the dispatcher has handled the newer write's end,
	// whatever that end did to the other mark.
	require.Eventually(t, func() bool { return p.writesInFlight() < 2 },
		10*time.Second, time.Millisecond, "premise: the newer held write ended")
	require.Equal(t, int64(200), client.XLen(context.Background(), newer).Val(), "premise: the newer held write landed")
	alive, err = p.DispatcherHealthy()
	require.False(t, alive, "the end of a newer write must not clear the old one's mark")
	require.ErrorIs(t, err, errDispatcherSilent)

	releaseOld()
	waitFor(t, done, "the drain to end once the old write is answered")
	alive, err = p.DispatcherHealthy()
	require.True(t, alive, "with nothing in flight admission measures from the last answer again (%v)", err)
	require.Equal(t, int64(200), client.XLen(context.Background(), old).Val())
	require.Equal(t, int64(200), client.XLen(context.Background(), newer).Val())
}

// firstService is the ServiceId of the first XADD in a pipeline, "" if none.
func firstService(cmds []goredis.Cmder) string {
	for _, c := range cmds {
		if !strings.EqualFold(c.Name(), "xadd") {
			continue
		}
		args := c.Args()
		for i := 0; i+1 < len(args); i++ {
			if args[i] != "data" {
				continue
			}
			var msg transport.MinedRelayMessage
			if b, ok := args[i+1].([]byte); ok && msg.Unmarshal(b) == nil {
				return msg.ServiceId
			}
		}
	}
	return ""
}

// failsLaterAfterEarlier fails every dispatch pipeline. The one whose first relay
// is later fails only once the dispatcher has handled the failure of the one
// whose first relay is earlier, so the failures come back in the order their
// chunks were taken and the later one is handled last.
type failsLaterAfterEarlier struct {
	t     *testing.T
	later string
	p     *BatchingPublisher
	fired atomic.Int64
}

func (h *failsLaterAfterEarlier) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (h *failsLaterAfterEarlier) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return next
}

func (h *failsLaterAfterEarlier) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		svc := firstService(cmds)
		if svc == "" {
			return next(ctx, cmds)
		}
		h.fired.Add(1)
		if svc == h.later {
			// Until only this write is left in flight: the dispatcher has taken
			// the earlier one's result and done with it whatever it does. assert
			// and not require: this runs on a worker, not the test goroutine.
			assert.Eventually(h.t, func() bool { return h.p.writesInFlight() <= 1 },
				10*time.Second, time.Millisecond, "the earlier failed write was never handled")
		}
		err := errors.New("connection reset by peer")
		for _, cmd := range cmds {
			cmd.SetErr(err)
		}
		return err
	}
}

// TestFailedWritesHandledOneAtATimeGoBackInArrivalOrder: two chunks of one stream
// are in flight and both fail, the earlier one handled before the later one has
// even failed. What goes back to the queue keeps arrival order, and no new chunk
// is taken once a write has failed. Putting each failure back at the head as it
// is handled would put the later chunk in front of the earlier one.
func TestFailedWritesHandledOneAtATimeGoBackInArrivalOrder(t *testing.T) {
	client := testredis.Client(t)
	p := NewBatchingPublisher(zerolog.Nop(), client, testredis.Prefix(t), time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })
	hook := &failsLaterAfterEarlier{t: t, later: fmt.Sprintf("svc-%04d", maxChunkCommands), p: p}
	client.AddHook(hook)

	fillOneStream(t, p, 3*maxChunkCommands)
	before := p.QueuedBytes()

	p.dispatchAll(context.Background())

	require.Equal(t, int64(2), hook.fired.Load(), "no chunk may be taken once a write has failed")
	require.Equal(t, before, p.QueuedBytes(), "every failed chunk went back")
	var got []string
	for {
		chunk := p.takeChunk()
		if len(chunk) == 0 {
			break
		}
		got = append(got, servicesOf(chunk)...)
	}
	require.Len(t, got, 3*maxChunkCommands)
	for i, s := range got {
		require.Equal(t, fmt.Sprintf("svc-%04d", i), s, "relay %d went back out of order", i)
	}
}

// refillingCharger keeps the queue full while a drain runs: every dispatch
// pipeline, up to refills of them, publishes another full chunk before it is
// sent, and moves the clock one step. At pipeline number chargeAt it adds a
// charge to the ledger, which is therefore NEW to the drain already running,
// and it records the first pipeline that carried that charge's INCRBY.
type refillingCharger struct {
	p        *BatchingPublisher
	ledger   *ChargeLedger
	clock    *fakeClock
	step     time.Duration
	supplier string
	key      string
	chargeAt int64
	refills  int64

	n          atomic.Int64
	firstWrite atomic.Int64
	mu         sync.Mutex
	errs       []error
}

func (h *refillingCharger) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (h *refillingCharger) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (h *refillingCharger) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if firstXAddStream(cmds) == "" && !hasIncr(cmds, h.key) {
			return next(ctx, cmds)
		}
		n := h.n.Add(1)
		h.clock.set(h.clock.now().Add(h.step))
		if hasIncr(cmds, h.key) {
			h.firstWrite.CompareAndSwap(0, n)
		}
		if n == h.chargeAt {
			h.ledger.Add(h.key, h.supplier, 7, time.Hour)
		}
		if n <= h.refills {
			for i := 0; i < maxChunkCommands; i++ {
				msg := mined(h.supplier, "s1", i)
				msg.RelayBytes = []byte(fmt.Sprintf("refill-%d-%d", n, i))
				if err := h.p.Publish(ctx, msg); err != nil {
					h.mu.Lock()
					h.errs = append(h.errs, err)
					h.mu.Unlock()
				}
			}
		}
		return next(ctx, cmds)
	}
}

func hasIncr(cmds []goredis.Cmder, key string) bool {
	for _, c := range cmds {
		if strings.EqualFold(c.Name(), "incrby") && fmt.Sprint(c.Args()[1]) == key {
			return true
		}
	}
	return false
}

// TestAChargeServedWhileTheQueueStaysFullIsWrittenDuringTheDrain: while relays
// keep arriving faster than they drain, one dispatch never finds the queue empty.
// A charge added to the ledger after that dispatch started must still be written
// while it runs, once intervals pass, and exactly once. Every chunk here is full
// at maxChunkCommands, so the charge cannot ride with its supplier's relays: it
// is written as a leftover, in a chunk of charges alone.
func TestAChargeServedWhileTheQueueStaysFullIsWrittenDuringTheDrain(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	rec := &execRecorder{}
	client.AddHook(rec)
	clock := newFakeClock(time.Now())
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2), withClock(clock.now))
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)
	const supplier = "pokt1full"
	hook := &refillingCharger{
		p: p, ledger: ledger, clock: clock, step: time.Hour, supplier: supplier,
		key:      prefix + ":consumed:late",
		chargeAt: 2,
		refills:  20,
	}
	client.AddHook(hook)
	ctx := context.Background()

	publishMined(t, p, supplier, maxChunkCommands, func(int) string { return "s1" })
	p.dispatchAll(ctx)

	hook.mu.Lock()
	require.Empty(t, hook.errs)
	hook.mu.Unlock()
	require.Greater(t, hook.n.Load(), hook.refills,
		"premise: one dispatch kept draining through every refill, so the queue never emptied under it")
	first := hook.firstWrite.Load()
	require.NotZero(t, first, "the charge added during the drain was not written by it at all")
	require.LessOrEqual(t, first, hook.refills,
		"the charge was written at pipeline %d, only once the queue had emptied: it waited out the whole saturation", first)
	requireChargesNeverSplit(t, rec.shapes())
	got, err := client.Get(ctx, hook.key).Int64()
	require.NoError(t, err)
	require.Equal(t, int64(7), got, "the charge is written exactly once")
	require.Zero(t, ledger.Pending(hook.key))
	require.Equal(t, (hook.refills+1)*maxChunkCommands,
		client.XLen(ctx, transport.SupplierStreamName(prefix, supplier)).Val())
}

// publishesOneBigRelayOnce publishes one relay larger than a chunk from inside
// the first dispatch pipeline, so it is queued AFTER the drain began.
type publishesOneBigRelayOnce struct {
	p     *BatchingPublisher
	once  sync.Once
	err   atomic.Pointer[error]
	extra []byte
}

func (h *publishesOneBigRelayOnce) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (h *publishesOneBigRelayOnce) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return next
}

func (h *publishesOneBigRelayOnce) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if firstXAddStream(cmds) != "" {
			h.once.Do(func() {
				msg := mined("pokt1big", "s1", 1)
				msg.RelayBytes = h.extra
				if err := h.p.Publish(ctx, msg); err != nil {
					h.err.Store(&err)
				}
			})
		}
		return next(ctx, cmds)
	}
}

// TestARelayLargerThanAChunkQueuedDuringTheDrainLeavesInIt: a relay queued after
// a drain began waits for the next tick only while it is part of a chunk short
// of the limits. One relay larger than a chunk is a full chunk on its own, which
// is every chunk of 1 MiB relays, so it leaves in the drain already running.
func TestARelayLargerThanAChunkQueuedDuringTheDrainLeavesInIt(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })
	// Incompressible: a compressible filler would be queued compressed and
	// stop being larger than a chunk.
	hook := &publishesOneBigRelayOnce{p: p, extra: transport.ChainedHashBytes("big", maxChunkBytes)}
	client.AddHook(hook)
	ctx := context.Background()

	publishMined(t, p, "pokt1big", 1, func(int) string { return "s1" })
	p.dispatchAll(ctx)

	require.Nil(t, hook.err.Load())
	require.Equal(t, int64(2), client.XLen(ctx, transport.SupplierStreamName(prefix, "pokt1big")).Val(),
		"the relay larger than a chunk, queued during the drain, must leave in it")
	require.Zero(t, p.QueuedBytes())
}

// publishesFewOnce publishes n small relays from inside the first dispatch
// pipeline, so they are queued AFTER the drain began.
type publishesFewOnce struct {
	p    *BatchingPublisher
	n    int
	once sync.Once
	err  atomic.Pointer[error]
}

func (h *publishesFewOnce) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (h *publishesFewOnce) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (h *publishesFewOnce) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if firstXAddStream(cmds) != "" {
			h.once.Do(func() {
				for i := 0; i < h.n; i++ {
					msg := mined("pokt1few", "s1", i)
					msg.RelayBytes = []byte(fmt.Sprintf("late-%d", i))
					if err := h.p.Publish(ctx, msg); err != nil {
						h.err.Store(&err)
					}
				}
			})
		}
		return next(ctx, cmds)
	}
}

// TestAChunkShortOfTheLimitsQueuedDuringTheDrainWaitsForTheNextTick: a drain
// empties what was queued when it began. A few relays queued after that are a
// chunk short of the limits, and they wait for the next tick instead of leaving
// as soon as a write slot frees: a slot frees every few milliseconds, and sending
// what arrived by then would turn one EXEC into many small ones.
func TestAChunkShortOfTheLimitsQueuedDuringTheDrainWaitsForTheNextTick(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })
	hook := &publishesFewOnce{p: p, n: 3}
	client.AddHook(hook)
	ctx := context.Background()
	stream := transport.SupplierStreamName(prefix, "pokt1few")

	publishMined(t, p, "pokt1few", 10, func(int) string { return "s1" })
	p.dispatchAll(ctx)

	require.Nil(t, hook.err.Load())
	require.Equal(t, int64(10), client.XLen(ctx, stream).Val(), "the drain wrote what was queued when it began")
	require.Positive(t, p.QueuedBytes(), "the few relays queued during the drain wait for the next tick")

	p.dispatchAll(ctx)
	require.Equal(t, int64(13), client.XLen(ctx, stream).Val(), "the next tick writes them")
	require.Zero(t, p.QueuedBytes())
}

// advancesClockOnce moves the clock by step the first time a dispatch pipeline
// passes, so every chunk taken after that one sees an interval gone by.
type advancesClockOnce struct {
	clock *fakeClock
	step  time.Duration
	once  sync.Once
}

func (h *advancesClockOnce) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (h *advancesClockOnce) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (h *advancesClockOnce) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if firstXAddStream(cmds) != "" {
			h.once.Do(func() { h.clock.set(h.clock.now().Add(h.step)) })
		}
		return next(ctx, cmds)
	}
}

// TestChargesThatDidNotRideAreWrittenOnceWhenTheLedgerIsTakenAgain: 40 charges
// of one supplier, whose one chunk has room for 28 of them. An interval passes,
// and the next chunk (another supplier's) takes the ledger again: the 12 that
// did not ride leave as leftovers. Each of the 40 is written exactly once, in
// one EXEC, never split from its EXPIRE NX.
func TestChargesThatDidNotRideAreWrittenOnceWhenTheLedgerIsTakenAgain(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	rec := &execRecorder{}
	client.AddHook(rec)
	clock := newFakeClock(time.Now())
	client.AddHook(&advancesClockOnce{clock: clock, step: 2 * time.Hour})
	// One worker, so the second chunk is taken after the first pipeline passed.
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, withClock(clock.now))
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)
	ctx := context.Background()

	publishMined(t, p, "pokt1ride", 200, func(int) string { return "s1" })
	publishMined(t, p, "pokt1other", 200, func(int) string { return "s1" })
	amounts := map[string]int64{}
	for j := 0; j < 40; j++ {
		key := fmt.Sprintf("%s:consumed:ride:%d", prefix, j)
		amounts[key] = int64(j + 1)
		ledger.Add(key, "pokt1ride", amounts[key], time.Hour)
	}

	p.dispatchAll(ctx)

	shapes := rec.shapes()
	requireChargesNeverSplit(t, shapes)
	require.Equal(t, 28, shapes[0].incrs, "premise: 28 charges rode with their supplier's 200 relays")
	execsOf := map[string]int{}
	for _, s := range shapes {
		for _, k := range s.incrKeys {
			execsOf[k]++
		}
	}
	for key, amount := range amounts {
		require.Equal(t, 1, execsOf[key], "%s must be charged in exactly one EXEC", key)
		got, err := client.Get(ctx, key).Int64()
		require.NoError(t, err, key)
		require.Equal(t, amount, got, "%s: written exactly once", key)
		require.Zero(t, ledger.Pending(key), key)
	}
}

// TestCancellingTheDispatchLetsWritesInFlightLandAndCharged: the dispatch's
// context is cancelled -- Close does this -- while two writes are in flight. Both
// still land with their charges, and nothing more is taken. A write that
// inherits the cancellation is refused by go-redis before anything is sent, and
// its charges are forgotten as if the EXEC might have run: served, never billed.
func TestCancellingTheDispatchLetsWritesInFlightLandAndCharged(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	s1 := transport.SupplierStreamName(prefix, "pokt1cancelA")
	s2 := transport.SupplierStreamName(prefix, "pokt1cancelB")
	s3 := transport.SupplierStreamName(prefix, "pokt1cancelC")
	hook := &holdStreams{
		hold:    map[string]chan struct{}{s1: make(chan struct{}), s2: make(chan struct{})},
		entered: make(chan string, 8),
		proceed: make(chan struct{}),
	}
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)

	keys := map[string]string{}
	for _, s := range []string{"pokt1cancelA", "pokt1cancelB", "pokt1cancelC"} {
		publishMined(t, p, s, 200, func(int) string { return "s1" })
		keys[s] = prefix + ":consumed:" + s
		ledger.Add(keys[s], s, 9, time.Hour)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.dispatchAll(ctx)
	}()
	proceed := onceCloser(hook.proceed)
	release1, release2 := onceCloser(hook.hold[s1]), onceCloser(hook.hold[s2])
	t.Cleanup(func() {
		proceed()
		release1()
		release2()
		<-done
	})
	waitFor(t, hook.entered, "the first write")
	waitFor(t, hook.entered, "the second write")

	cancel()
	release1()
	release2()
	proceed()
	waitFor(t, done, "the dispatch to return once cancelled")

	bg := context.Background()
	for _, s := range []string{"pokt1cancelA", "pokt1cancelB"} {
		require.Equal(t, int64(200), client.XLen(bg, transport.SupplierStreamName(prefix, s)).Val(),
			"%s: a write in flight when the dispatch was cancelled must still land", s)
		got, err := client.Get(bg, keys[s]).Int64()
		require.NoError(t, err, "%s: its charge must be written, not forgotten", s)
		require.Equal(t, int64(9), got, s)
		require.Zero(t, ledger.Pending(keys[s]), s)
	}
	require.Zero(t, client.XLen(bg, s3).Val(), "nothing is taken once the dispatch is cancelled")
	require.Positive(t, p.QueuedBytes(), "the untaken relays stay queued for the final flush")
	require.Equal(t, int64(9), ledger.Pending(keys["pokt1cancelC"]),
		"a charge that never rode stays pending for the final flush")
}

// hangsUntilContextEnds blocks every dispatch pipeline until the context it was
// handed ends -- a Redis that never answers, bounded only by the caller's
// context -- or until released, after which pipelines go through.
type hangsUntilContextEnds struct {
	entered  chan struct{}
	released chan struct{}
}

func (h *hangsUntilContextEnds) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (h *hangsUntilContextEnds) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return next
}

func (h *hangsUntilContextEnds) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if firstXAddStream(cmds) == "" {
			return next(ctx, cmds)
		}
		select {
		case h.entered <- struct{}{}:
		default:
		}
		select {
		case <-h.released:
			return next(ctx, cmds)
		case <-ctx.Done():
		}
		for _, cmd := range cmds {
			cmd.SetErr(ctx.Err())
		}
		return ctx.Err()
	}
}

// TestADeadlineOnTheDispatchBoundsTheWritesAlreadyStarted: the final flush runs
// on a context with a deadline, and a write it started against a Redis that
// never answers must end at that deadline. Detaching the writes from
// cancellation must not detach them from the deadline too, or the flush -- and
// the shutdown -- lasts as long as the hung write.
func TestADeadlineOnTheDispatchBoundsTheWritesAlreadyStarted(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	hook := &hangsUntilContextEnds{entered: make(chan struct{}, 1), released: make(chan struct{})}
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	publishMined(t, p, "pokt1deadline", 10, func(int) string { return "s1" })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.dispatchAll(ctx)
	}()
	waitFor(t, hook.entered, "the write to be in flight")
	waitFor(t, done, "the dispatch to end at its deadline, with its write hung")

	require.Positive(t, p.QueuedBytes(), "the write that ran out of time goes back to the queue")
	close(hook.released)
	require.NoError(t, p.Close(), "once Redis answers, the final flush writes what went back")
	require.Equal(t, int64(10), client.XLen(context.Background(), transport.SupplierStreamName(prefix, "pokt1deadline")).Val())
}
