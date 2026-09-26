//go:build test

package redis

import (
	"context"
	"fmt"
	"sync"
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

// barrierWait bounds how long a pipeline waits for a second one to be in flight.
// It is the failure bound of the barrier, not a synchronization: with parallel
// workers the second pipeline arrives at once and the wait ends on the channel.
const barrierWait = 5 * time.Second

// pipelineBarrier holds every pipeline until k of them are in flight at the same
// time. Once that happened it lets everything through. A dispatch that writes one
// chunk at a time never gets there: its first pipeline waits out barrierWait,
// the barrier records the miss and stops holding.
type pipelineBarrier struct {
	k        int
	mu       sync.Mutex
	inFlight int
	reached  chan struct{}
	once     sync.Once
	missed   atomic.Bool
}

func newPipelineBarrier(k int) *pipelineBarrier {
	return &pipelineBarrier{k: k, reached: make(chan struct{})}
}

func (b *pipelineBarrier) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (b *pipelineBarrier) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (b *pipelineBarrier) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		b.mu.Lock()
		b.inFlight++
		if b.inFlight >= b.k {
			b.once.Do(func() { close(b.reached) })
		}
		b.mu.Unlock()
		defer func() {
			b.mu.Lock()
			b.inFlight--
			b.mu.Unlock()
		}()
		if !b.missed.Load() {
			select {
			case <-b.reached:
			case <-time.After(barrierWait):
				b.missed.Store(true)
			}
		}
		return next(ctx, cmds)
	}
}

func (b *pipelineBarrier) requireReached(t *testing.T) {
	t.Helper()
	select {
	case <-b.reached:
	default:
		t.Fatalf("never saw %d pipelines in flight at once: the dispatch writes its chunks one at a time", b.k)
	}
	require.False(t, b.missed.Load(),
		"a pipeline waited %s for another one to be in flight: the dispatch writes its chunks one at a time", barrierWait)
}

// publishInterleaved enqueues perSupplier relays for each supplier, alternating
// suppliers, each relay with a payload unique across the whole test.
func publishInterleaved(t *testing.T, p *BatchingPublisher, suppliers []string, perSupplier int) {
	t.Helper()
	for i := 0; i < perSupplier; i++ {
		for _, s := range suppliers {
			msg := mined(s, "s1", i)
			msg.RelayBytes = []byte(fmt.Sprintf("%s-relay-%d", s, i))
			require.NoError(t, p.Publish(context.Background(), msg))
		}
	}
}

// requireEachRelayOnce asserts the stream holds exactly want entries and no two
// of them carry the same payload: nothing lost and nothing written twice.
func requireEachRelayOnce(t *testing.T, client goredis.UniversalClient, stream string, want int) {
	t.Helper()
	entries, err := client.XRange(context.Background(), stream, "-", "+").Result()
	require.NoError(t, err)
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		data := fmt.Sprint(e.Values["data"])
		_, dup := seen[data]
		require.False(t, dup, "%s holds a relay written twice", stream)
		seen[data] = struct{}{}
	}
	require.Len(t, entries, want, "%s must hold every relay published to it, once", stream)
}

func suppliersNamed(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// TestTheDispatchWritesChunksInParallel is the gate of the parallel dispatch: with
// two workers, two chunks are in flight at once. A dispatch that writes one chunk
// after the other never has two pipelines in flight and fails here.
func TestTheDispatchWritesChunksInParallel(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	barrier := newPipelineBarrier(2)
	client.AddHook(barrier)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })

	suppliers := suppliersNamed("pokt1par", 2)
	publishInterleaved(t, p, suppliers, 2*maxChunkCommands)

	p.dispatchAll(context.Background())

	barrier.requireReached(t)
	for _, s := range suppliers {
		requireEachRelayOnce(t, client, transport.SupplierStreamName(prefix, s), 2*maxChunkCommands)
	}
	require.Zero(t, p.QueuedBytes())
}

// TestAParallelDrainWritesEveryRelayOnce: several streams interleaved, drained by
// two workers while one of the pipelines fails at transport level. What that
// pipeline did not write goes back and is written by the next dispatch, and what
// the other workers wrote is not written again.
func TestAParallelDrainWritesEveryRelayOnce(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	hook := &exhaustedPoolTimeout{}
	hook.failNext.Store(1)
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })

	suppliers := suppliersNamed("pokt1drain", 5)
	const perSupplier = 300
	publishInterleaved(t, p, suppliers, perSupplier)
	ctx := context.Background()

	p.dispatchAll(ctx)
	require.Equal(t, int64(1), hook.fired.Load(), "premise: one pipeline of the drain failed")
	require.Positive(t, p.QueuedBytes(), "premise: the failed chunk went back to the queue")
	for p.QueuedBytes() > 0 {
		p.dispatchAll(ctx)
	}

	for _, s := range suppliers {
		requireEachRelayOnce(t, client, transport.SupplierStreamName(prefix, s), perSupplier)
	}
}

// TestEveryChargeIsWrittenOnceWithParallelWorkers: the ledger is taken once per
// tick and each charge rides in one EXEC, even with two workers writing at once.
// Every counter ends at its amount, appears in exactly one EXEC, and nothing stays
// pending.
func TestEveryChargeIsWrittenOnceWithParallelWorkers(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	barrier := newPipelineBarrier(2)
	rec := &execRecorder{}
	client.AddHook(barrier)
	client.AddHook(rec)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)
	ctx := context.Background()

	// Four suppliers of 200 relays each: one chunk per supplier, two rounds of
	// two, room for 28 charges in each chunk and the rest in chunks of charges
	// alone. The fifth supplier mined nothing and is charged alone.
	suppliers := suppliersNamed("pokt1charge", 4)
	const relays, sessions = 200, 40
	amounts := map[string]int64{}
	for si, s := range suppliers {
		publishMined(t, p, s, relays, func(int) string { return "s1" })
		for j := 0; j < sessions; j++ {
			key := fmt.Sprintf("%s:consumed:%s:%d", prefix, s, j)
			amounts[key] = int64(si*100 + j + 1)
			ledger.Add(key, s, amounts[key], time.Hour)
		}
	}
	for j := 0; j < 5; j++ {
		key := fmt.Sprintf("%s:consumed:idle:%d", prefix, j)
		amounts[key] = int64(1000 + j)
		ledger.Add(key, "pokt1idle", amounts[key], time.Hour)
	}

	p.dispatchAll(ctx)

	barrier.requireReached(t)
	shapes := rec.shapes()
	requireChargesNeverSplit(t, shapes)
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
		require.Equal(t, amount, got, "%s: the charge is written exactly once", key)
		require.Zero(t, ledger.Pending(key), "%s: nothing may stay pending", key)
	}
	for _, s := range suppliers {
		require.Equal(t, int64(relays), client.XLen(ctx, transport.SupplierStreamName(prefix, s)).Val())
	}
}

// panicsOnce panics inside the first pipeline it sees, which is inside a worker's
// write, and lets every later pipeline through.
type panicsOnce struct{ fired atomic.Int64 }

func (h *panicsOnce) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (h *panicsOnce) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (h *panicsOnce) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if h.fired.CompareAndSwap(0, 1) {
			panic("pipeline panicked by test")
		}
		return next(ctx, cmds)
	}
}

// TestAPanickedWorkerLeavesItsChunkQueuedAndUnwritten: when a worker panics
// mid-write, its chunk is neither counted as published nor taken as written: it
// goes back to the queue and the next dispatch writes it, once. Its charges do not
// stay in the ledger's writing set, where admission would count them forever and
// nothing would ever write them.
func TestAPanickedWorkerLeavesItsChunkQueuedAndUnwritten(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	hook := &panicsOnce{}
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)
	ctx := context.Background()

	suppliers := suppliersNamed("pokt1panic", 2)
	const relays = 200
	var keys []string
	publishedBefore := 0.0
	for _, s := range suppliers {
		publishMined(t, p, s, relays, func(int) string { return "s1" })
		publishedBefore += testutil.ToFloat64(publishedTotal.WithLabelValues(s, "svc"))
		for j := 0; j < 4; j++ {
			key := fmt.Sprintf("%s:consumed:%s:%d", prefix, s, j)
			keys = append(keys, key)
			ledger.Add(key, s, 5, time.Hour)
		}
	}
	unknown := chargeWriteFailures.WithLabelValues("exec_unknown")
	unknownBefore := testutil.ToFloat64(unknown)

	p.dispatchAll(ctx)
	require.Equal(t, int64(1), hook.fired.Load(), "premise: a worker panicked inside its write")

	written, published := int64(0), 0.0
	for _, s := range suppliers {
		written += client.XLen(ctx, transport.SupplierStreamName(prefix, s)).Val()
		published += testutil.ToFloat64(publishedTotal.WithLabelValues(s, "svc"))
	}
	require.Less(t, written, int64(2*relays), "premise: the panicked chunk did not reach the stream")
	require.Equal(t, float64(written), published-publishedBefore,
		"only what reached a stream is counted as published: the panicked chunk was counted as written")
	p.mu.Lock()
	queued := len(p.queue) - p.head
	p.mu.Unlock()
	require.Equal(t, int64(2*relays)-written, int64(queued), "the panicked chunk must go back to the queue")

	p.dispatchAll(ctx)

	for _, s := range suppliers {
		requireEachRelayOnce(t, client, transport.SupplierStreamName(prefix, s), relays)
	}
	forgotten := 0
	for _, key := range keys {
		require.Zero(t, ledger.Pending(key),
			"%s stays in the ledger's writing set: the panicked write never reported its charge", key)
		if client.Exists(ctx, key).Val() == 0 {
			forgotten++
			continue
		}
		got, err := client.Get(ctx, key).Int64()
		require.NoError(t, err, key)
		require.Equal(t, int64(5), got, "%s is charged once", key)
	}
	require.Equal(t, float64(forgotten), testutil.ToFloat64(unknown)-unknownBefore,
		"a charge the panicked write may have sent is counted as unknown, not lost silently")
}
