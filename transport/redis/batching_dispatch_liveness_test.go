//go:build test

package redis

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// fakeClock is a clock the test moves by hand, safe to read from the workers.
//
// It stores the instant and not its UnixNano, for the same reason the publisher
// does: an instant that crosses an int64 comes back without its monotonic
// reading, and then every Sub over it measures the wall clock. Measured
// 2026-09-20 while writing item 388's positive control -- with the old
// atomic.Int64 here, a round's own mark carried no reading even after the test's
// base was moved from a literal to time.Now, so the test could not tell a
// monotonic implementation from the broken one.
type fakeClock struct{ at atomic.Pointer[time.Time] }

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.set(t)
	return c
}

func (c *fakeClock) set(t time.Time) { c.at.Store(&t) }
func (c *fakeClock) now() time.Time  { return *c.at.Load() }
func withClock(now func() time.Time) BatchingPublisherOption {
	return func(p *BatchingPublisher) { p.now = now }
}

// firstXAddStream is the stream of the first XADD in a pipeline, "" if none.
func firstXAddStream(cmds []goredis.Cmder) string {
	for _, c := range cmds {
		if strings.EqualFold(c.Name(), "xadd") {
			return fmt.Sprint(c.Args()[1])
		}
	}
	return ""
}

// holdPipelines stops every pipeline at the hook: the one writing to stuck until
// release closes, any other until proceed closes. Each reports on entered first.
type holdPipelines struct {
	stuck   string
	entered chan string
	release chan struct{}
	proceed chan struct{}
}

func (h *holdPipelines) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (h *holdPipelines) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (h *holdPipelines) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		stream := firstXAddStream(cmds)
		h.entered <- stream
		if stream == h.stuck {
			<-h.release
		} else {
			<-h.proceed
		}
		return next(ctx, cmds)
	}
}

// waitFor receives from ch or fails after a bound that only a broken dispatch
// reaches.
func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// onceCloser closes ch the first time it is called and does nothing after, so a
// test can release a held write both on its path and in its cleanup.
func onceCloser(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// TestAStuckWriteClosesAdmissionEvenIfAnotherWorkerAnswers: two workers, one
// hangs on its EXEC and the other is answered. The answer moves the mark, but what
// admission measures stays at the start of the hung write, so its age grows until
// admission closes. Once the hung write is answered, the mark is fresh again.
func TestAStuckWriteClosesAdmissionEvenIfAnotherWorkerAnswers(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	stuck := transport.SupplierStreamName(prefix, "pokt1stuck")
	hook := &holdPipelines{
		stuck:   stuck,
		entered: make(chan string, 2),
		release: make(chan struct{}),
		proceed: make(chan struct{}),
	}
	client.AddHook(hook)
	// time.Now and not a wall-clock literal: a literal carries no monotonic
	// reading, so Sub over it falls back to the wall clock and an implementation
	// that lost the monotonic reading would pass this test too.
	t0 := time.Now()
	clock := newFakeClock(t0)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2), withClock(clock.now))
	t.Cleanup(func() { _ = p.Close() })

	for _, s := range []string{"pokt1stuck", "pokt1answered"} {
		publishMined(t, p, s, 200, func(int) string { return "s1" })
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.dispatchAll(context.Background())
	}()
	proceed, release := onceCloser(hook.proceed), onceCloser(hook.release)
	// Registered after the publisher's Close, so it runs before it: a failed
	// assertion must not leave a write held at the hook, where Close would wait
	// for its worker forever.
	t.Cleanup(func() {
		proceed()
		release()
		<-done
	})
	waitFor(t, hook.entered, "the first write of the round")
	waitFor(t, hook.entered, "the second write of the round")

	// The round stamped its own mark through writeRound, not a test store: if
	// that stamp ever loses its monotonic reading, the pinch below measures the
	// wall clock again. See TestTheMarksAdmissionMeasuresFromKeepTheirMonotonicReading.
	require.True(t, hasMonotonic(*p.inFlightSince.Load()),
		"the mark a real dispatch round stamps must carry a monotonic reading")

	answeredAt := t0.Add(2500 * time.Millisecond)
	clock.set(answeredAt)
	proceed()
	require.Eventually(t, func() bool { return lastMark(p).Equal(answeredAt) },
		10*time.Second, time.Millisecond, "premise: the other worker's EXEC was answered and marked")

	clock.set(t0.Add(time.Hour))
	alive, err := p.DispatcherHealthy()
	require.False(t, alive,
		"a worker was answered at %s, but the oldest write in flight started at %s: admission must measure "+
			"from the hung write, not from any answer", answeredAt, t0)
	require.ErrorIs(t, err, errDispatcherSilent)

	recovered := t0.Add(2 * time.Hour)
	clock.set(recovered)
	release()
	waitFor(t, done, "the round to end once the hung write is answered")
	alive, err = p.DispatcherHealthy()
	require.True(t, alive,
		"once no write is in flight, admission measures from the last answer again (%v)", err)
	require.Equal(t, int64(200), client.XLen(context.Background(), stuck).Val())
}

// lastMark is the instant the publisher last marked, or the zero time while it
// has marked nothing. Tests read it through here because the field holds a
// pointer: nil is "never reached Redis", which is what a fresh publisher is.
func lastMark(p *BatchingPublisher) time.Time {
	if at := p.lastSuccess.Load(); at != nil {
		return *at
	}
	return time.Time{}
}

// pingCounter counts the PINGs sent through the client.
type pingCounter struct{ pings atomic.Int64 }

func (h *pingCounter) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (h *pingCounter) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if strings.EqualFold(cmd.Name(), "ping") {
			h.pings.Add(1)
		}
		return next(ctx, cmd)
	}
}

func (h *pingCounter) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

// TestTheHeartbeatPingsOnlyWhileNoWriteIsInFlight: while a write is in flight the
// heartbeat sends no PING, so a PING answered next to a hung write cannot stand in
// for the write's progress; once nothing is in flight the PING goes out and its
// answer marks.
func TestTheHeartbeatPingsOnlyWhileNoWriteIsInFlight(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	counter := &pingCounter{}
	hook := &holdPipelines{
		stuck:   transport.SupplierStreamName(prefix, "pokt1idle"),
		entered: make(chan string, 1),
		release: make(chan struct{}),
	}
	client.AddHook(counter)
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	publishMined(t, p, "pokt1idle", 3, func(int) string { return "s1" })
	chunk := p.takeChunk()
	require.Len(t, chunk, 3)
	// Closed first, with an empty queue: its own heartbeat ticker stops, so every
	// PING counted below is one this test asked for.
	require.NoError(t, p.Close())
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.writeRound(ctx, []*dispatchJob{{chunk: chunk}}, nil)
	}()
	release := onceCloser(hook.release)
	t.Cleanup(func() {
		release()
		<-done
	})
	waitFor(t, hook.entered, "the write to be in flight")

	before := counter.pings.Load()
	markBefore := lastMark(p)
	p.heartbeat(ctx)
	require.Equal(t, before, counter.pings.Load(), "the heartbeat sent a PING while a write was in flight")
	require.True(t, lastMark(p).Equal(markBefore))

	release()
	waitFor(t, done, "the write to end")
	p.heartbeat(ctx)
	require.Equal(t, before+1, counter.pings.Load(), "with nothing in flight the heartbeat must PING")
	require.Equal(t, int64(3), client.XLen(ctx, hook.stuck).Val())
}
