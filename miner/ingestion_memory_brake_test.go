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

func TestIngestionMemoryBrake_ClosesAboveAMarginAndReopensOnlyBelowAMarginAndAHalf(t *testing.T) {
	heap := &heapModel{objects: 6000 * mib, live: 6000 * mib}
	admission := heap.admission(brakeLimit)
	held := admission.IngestionPause("pokt1held")
	closedBefore := testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeClosed))
	openBefore := testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeOpen))
	forcedBefore := testutil.ToFloat64(forcedGCs.WithLabelValues(gcReasonMemoryBrake))

	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "under the brake the consumers read")
	require.Zero(t, heap.gcCount(), "objects under the brake settle it without a GC")

	changed := held.PauseChanged()
	heap.load(400 * mib)
	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "LINK brake-close: a live heap above the limit less a margin holds the consumers")
	requireClosed(t, changed, "LINK brake-signal: closing the brake wakes the consumers waiting on the pause")
	require.Equal(t, 1, heap.gcCount(), "objects above the brake are asked again of the live heap")
	require.Equal(t, float64(1), testutil.ToFloat64(ingestionMemoryBrakeClosed))
	require.Equal(t, closedBefore+1, testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeClosed)))

	done := admission.claimFlushWaiting("pokt1flush")
	require.False(t, admission.IngestionPause("pokt1flush").Paused(),
		"LINK brake-flush: the supplier whose claim waits for its stream reads under the brake")
	require.True(t, held.Paused(), "every other supplier stays held")
	done()
	require.True(t, admission.IngestionPause("pokt1flush").Paused(), "and the hold returns when its claim stops waiting")

	heap.drop(400 * mib)
	admission.evaluateMemoryBrake()
	require.Equal(t, uint64(6000*mib), heap.readLive(), "premise: the live heap is between the two thresholds")
	require.True(t, held.Paused(), "LINK brake-hysteresis: the brake does not reopen above the limit less a margin and a half")

	changed = held.PauseChanged()
	heap.drop(300 * mib)
	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "LINK brake-reopen: a live heap below the limit less a margin and a half reopens the brake")
	requireClosed(t, changed, "LINK brake-signal: reopening the brake wakes the consumers waiting on the pause")
	require.Zero(t, testutil.ToFloat64(ingestionMemoryBrakeClosed))
	require.Equal(t, openBefore+1, testutil.ToFloat64(ingestionMemoryBrakeTransitions.WithLabelValues(memoryBrakeOpen)))

	heap.load(1000 * mib)
	heap.drop(1000 * mib)
	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "LINK brake-live: garbage alone does not close the brake")
	require.Equal(t, 4, heap.gcCount())
	require.Equal(t, forcedBefore+4, testutil.ToFloat64(forcedGCs.WithLabelValues(gcReasonMemoryBrake)))

	heap.load(1000 * mib)
	admission.evaluateMemoryBrake()
	require.True(t, held.Paused(), "premise: closed again")
	heap.drop(2000 * mib)
	heap.gc() // the runtime collects on its own
	admission.evaluateMemoryBrake()
	require.False(t, held.Paused(), "objects under the reopen threshold reopen the brake without forcing a GC")
	require.Equal(t, 6, heap.gcCount(), "one GC to close, and the runtime's")
}

func TestIngestionMemoryBrake_ItsGCRunsAtMostOncePerIntervalWithTheAdmission(t *testing.T) {
	heap := &heapModel{objects: 6400 * mib, live: 6400 * mib, step: time.Millisecond}
	admission := heap.admission(brakeLimit)

	admission.collect(gcReasonRebuildAdmission)
	require.Equal(t, 1, heap.gcCount(), "premise: the admission forced a GC")
	admission.evaluateMemoryBrake()
	require.Equal(t, 1, heap.gcCount(), "LINK brake-gc-shared: the brake does not force another GC within the interval")
	require.True(t, admission.IngestionPause("pokt1held").Paused(), "the live heap that GC measured decides")
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
