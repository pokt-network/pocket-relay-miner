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
