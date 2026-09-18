//go:build test

package miner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const (
	mib        = 1 << 20
	brakeLimit = 7 << 30 // closes above 6272 MiB, reopens below 5824 MiB
)

func requireClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatal(msg)
	}
}

func TestIngestionMemoryBrake_ClosesWhenTheRuntimeIsOverItsLimitAndReopensOnceItIsBackWithin(t *testing.T) {
	heap := &heapModel{objects: 1500 * mib, live: 1500 * mib, mapped: brakeLimit + mib}
	admission := heap.admission(brakeLimit)
	held := admission.IngestionPause("pokt1held")
	forcedBefore := testutil.ToFloat64(forcedGCs.WithLabelValues(gcReasonMemoryBrake))

	changed := held.PauseChanged()
	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "LINK brake-overage-one-tick: one evaluation over the limit is the runtime's overshoot, not a close")
	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "LINK brake-overage: the runtime over its limit closes the brake, whatever the live heap")
	requireClosed(t, changed, "LINK brake-signal: closing the brake wakes the consumers waiting on the pause")
	require.Zero(t, heap.gcCount(), "LINK brake-overage-no-gc: closing on the runtime's memory forces no GC")

	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "LINK brake-reopen-mapped: a low live heap does not reopen while the runtime is over its limit")

	// The scavenger returns nothing while the runtime is within its limit, so
	// the memory the limit bounds may stay just under it.
	heap.setMapped(brakeLimit * 97 / 100)
	changed = held.PauseChanged()
	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "LINK brake-reopen-sustained: the brake does not reopen the moment the runtime is back within its limit")
	heap.advance(brakeReopenAfter)
	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "LINK brake-reopen-within: back within its limit with a low live heap, the brake reopens")
	requireClosed(t, changed, "LINK brake-signal: reopening the brake wakes the consumers waiting on the pause")
	require.Equal(t, forcedBefore, testutil.ToFloat64(forcedGCs.WithLabelValues(gcReasonMemoryBrake)))
}

func TestIngestionMemoryBrake_ClosesOnTheLiveHeapTheRuntimeMeasuredAndReopensOnlyBelowAMarginAndAHalf(t *testing.T) {
	// A clock of milliseconds: every GC here is the runtime's, and recent.
	heap := &heapModel{objects: 6000 * mib, live: 6000 * mib, mapped: 6900 * mib, step: time.Millisecond}
	admission := heap.admission(brakeLimit)
	held := admission.IngestionPause("pokt1held")
	closedBefore := testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeClosed))
	openBefore := testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeOpen))

	heap.load(1000 * mib)
	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "objects over the threshold do not close the brake: they include garbage")
	require.Zero(t, heap.gcCount(), "LINK brake-open-no-gc: an open brake forces no GC, it reads the live heap the runtime measured")

	heap.drop(600 * mib)
	heap.gc() // the runtime collects: live 6400
	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "LINK brake-close: a live heap above the limit less a margin holds the consumers")
	require.Equal(t, float64(1), testutil.ToFloat64(ingestionMemoryBrakeClosed))
	require.Equal(t, closedBefore+1, testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeClosed)))

	done := admission.claimFlushWaiting("pokt1flush")
	require.False(t, admission.IngestionPause("pokt1flush").Paused(),
		"LINK brake-flush: the supplier whose claim waits for its stream reads under the brake")
	require.True(t, held.Paused(), "every other supplier stays held")
	done()
	require.True(t, admission.IngestionPause("pokt1flush").Paused(), "and the hold returns when its claim stops waiting")

	heap.drop(400 * mib)
	heap.gc() // live 6000
	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "LINK brake-hysteresis: the brake does not reopen above the limit less a margin and a half")

	heap.drop(300 * mib)
	heap.gc() // live 5700
	changed := held.PauseChanged()
	admission.evaluateMemoryBrake()
	heap.advance(brakeReopenAfter)
	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "LINK brake-reopen: a live heap below the limit less a margin and a half reopens the brake")
	requireClosed(t, changed, "LINK brake-signal: reopening the brake wakes the consumers waiting on the pause")
	require.Zero(t, testutil.ToFloat64(ingestionMemoryBrakeClosed))
	require.Equal(t, openBefore+1, testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeOpen)))
	require.Equal(t, 3, heap.gcCount(), "every GC was the runtime's")
}

func TestIngestionMemoryBrake_ClosedItForcesAGCOnlyAfterAMinuteWithoutOne(t *testing.T) {
	heap := &heapModel{objects: 6400 * mib, live: 6400 * mib, mapped: 6900 * mib, step: time.Millisecond}
	heap.gc() // the runtime's last GC measured 6400
	admission := heap.admission(brakeLimit)
	held := admission.IngestionPause("pokt1held")
	forcedBefore := testutil.ToFloat64(forcedGCs.WithLabelValues(gcReasonMemoryBrake))

	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "premise: closed on the live heap")

	heap.drop(1000 * mib) // a claim lets its trees go; nothing sees it until a GC
	for range 59 {
		heap.advance(time.Second)
		admission.evaluateMemoryBrake()
	}
	require.Equal(t, 1, heap.gcCount(), "LINK brake-collect-after: within a minute of the last GC the closed brake forces none")
	require.True(t, held.Paused(), "and the live heap it reads has not fallen")

	heap.advance(time.Second)
	admission.evaluateMemoryBrake()
	require.Equal(t, 2, heap.gcCount(), "LINK brake-collect: a minute without a GC, the closed brake forces one")
	heap.advance(brakeReopenAfter)
	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "and the live heap that GC measured reopens it, once it has held")
	require.Equal(t, forcedBefore+1, testutil.ToFloat64(forcedGCs.WithLabelValues(gcReasonMemoryBrake)))
}

// Measured under load, in a proof window: the runtime crossed its limit and came
// back on the next evaluation, again and again, with the live heap far below the
// thresholds. Closing on each crossing closed the brake nine times in 25 s.
func TestIngestionMemoryBrake_DoesNotFlapOnTheRuntimeOvershootingItsLimitOneEvaluationAtATime(t *testing.T) {
	heap := &heapModel{objects: 4000 * mib, live: 4000 * mib, step: time.Second}
	admission := heap.admission(brakeLimit)
	held := admission.IngestionPause("pokt1held")
	closedBefore := testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeClosed))

	for tick := range 25 {
		if tick%2 == 0 {
			heap.setMapped(brakeLimit + 200*mib)
		} else {
			heap.setMapped(brakeLimit - 10*mib)
		}
		admission.evaluateMemoryBrake()
		require.False(t, held.Paused(), "LINK brake-no-flap: tick %d", tick)
	}
	require.Equal(t, closedBefore, testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeClosed)))

	heap.setMapped(brakeLimit + 200*mib)
	admission.evaluateMemoryBrake()
	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "a runtime that stays over its limit closes the brake")
	for tick := range 20 {
		if tick%2 == 0 {
			heap.setMapped(brakeLimit - 10*mib)
		} else {
			heap.setMapped(brakeLimit + 200*mib)
		}
		admission.evaluateMemoryBrake()
		require.True(t, held.Paused(), "LINK brake-reopen-sustained: tick %d, the process has not held within its limit", tick)
	}
}

// armedPause records when a consumer found its pause held after taking the
// channel it waits on, which only its read path does.
type armedPause struct {
	IngestionPauseView
	mu     sync.Mutex
	armed  bool
	held   bool
	checks int
}

func (p *armedPause) PauseChanged() <-chan struct{} {
	ch := p.IngestionPauseView.PauseChanged()
	p.mu.Lock()
	p.armed = true
	p.mu.Unlock()
	return ch
}

func (p *armedPause) Paused() bool {
	paused := p.IngestionPauseView.Paused()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks++
	if p.armed && paused {
		p.held = true
	}
	return paused
}

// asleep reports whether the read path found the pause held and every check
// the consumer makes when it starts has run: the reclaim loop checks once, and
// then not before its sweep.
func (p *armedPause) asleep() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.held && p.checks >= 2
}

func TestIngestionMemoryBrake_TheStreamConsumerSleepsUnderTheBrakeAndReadsOnceItReopens(t *testing.T) {
	client, _ := newTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const supplier = "pokt1brake_reads"

	consumer, err := redisutil.NewStreamsConsumer(zerolog.Nop(), client, transport.ConsumerConfig{
		StreamPrefix: client.KB().StreamPrefix(), SupplierOperatorAddress: supplier,
		ConsumerGroup: client.KB().ConsumerGroup(), ConsumerName: "brake", BatchSize: 10, ClaimIdleTimeout: 60000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = consumer.Close() })
	require.NoError(t, client.XAdd(ctx, &goredis.XAddArgs{Stream: consumer.StreamName(), Values: map[string]any{"data": "relay"}}).Err())

	heap := &heapModel{objects: 6400 * mib, live: 6400 * mib}
	admission := heap.admission(brakeLimit)
	admission.evaluateMemoryBrake()
	require.True(t, admission.IngestionPause(supplier).Paused(), "premise: the brake is closed")

	pause := &armedPause{IngestionPauseView: admission.IngestionPause(supplier)}
	consumer.SetIngestionPause(pause)
	var read atomic.Pointer[streamMsgID]
	client.AddHook(readRecorder{stream: consumer.StreamName(), read: &read})
	messages := consumer.Consume(ctx)
	go func() {
		for range messages {
		}
	}()

	for !pause.asleep() {
		select {
		case <-ctx.Done():
			t.Fatal("the consumer never found the brake closed")
		case <-time.After(time.Millisecond):
		}
	}
	require.Nil(t, read.Load(), "LINK brake-hold: nothing is read while the brake is closed")

	heap.drop(1000 * mib)
	admission.evaluateMemoryBrake()
	admission.evaluateMemoryBrake() // the clock moves an hour: the reopen has held
	for read.Load() == nil {
		select {
		case <-ctx.Done():
			t.Fatal("LINK brake-wake: the consumer asleep on the closed brake reads once it reopens, with nothing else happening")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestSupplierManagerStart_RunsTheIngestionMemoryBrakeOnTheProcessAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	redisClient, _ := newTestRedis(t)
	pool := pond.NewPool(4)
	defer pool.StopAndWait()
	const supplier = "pokt1brake_wiring"
	qc := &toggleableSupplierQueryClient{addr: supplier}
	mgr := NewSupplierManager(zerolog.Nop(), &fakeKeyManager{addrs: []string{supplier}}, nil, SupplierManagerConfig{
		RedisClient: redisClient, MinerID: "test-miner-brake", SupplierQueryClient: qc, WorkerPool: pool,
	})
	heap := &heapModel{objects: 6400 * mib, live: 6400 * mib}
	mgr.rebuildAdmission.processMemory = heap.memory(brakeLimit)

	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Close() })
	for !mgr.rebuildAdmission.IngestionPause(supplier).Paused() {
		select {
		case <-ctx.Done():
			t.Fatal("LINK brake-wiring: a started supplier manager evaluates the ingestion memory brake")
		case <-time.After(time.Millisecond):
		}
	}
}
