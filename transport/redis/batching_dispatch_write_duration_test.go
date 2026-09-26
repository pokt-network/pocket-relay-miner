//go:build test

package redis

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// writeDurations reads the count and sum of the write duration histogram for result.
func writeDurations(t *testing.T, result string) (uint64, float64) {
	t.Helper()
	var m dto.Metric
	require.NoError(t, dispatchWriteDuration.WithLabelValues(result).(interface{ Write(*dto.Metric) error }).Write(&m))
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

// TestDispatchWriteDurationObservesEachWriteWhenItComesBack: two workers, one
// write answered 100 ms after it started and the other 3.2 s after. Each is
// observed on its own, as its own duration, when it comes back: the fast one does
// not wait for the slow one.
func TestDispatchWriteDurationObservesEachWriteWhenItComesBack(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	slow := transport.SupplierStreamName(prefix, "pokt1slowwrite")
	hook := &holdPipelines{
		stuck:   slow,
		entered: make(chan string, 2),
		release: make(chan struct{}),
		proceed: make(chan struct{}),
	}
	client.AddHook(hook)
	t0 := time.Now()
	clock := newFakeClock(t0)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2), withClock(clock.now))
	t.Cleanup(func() { _ = p.Close() })
	for _, s := range []string{"pokt1slowwrite", "pokt1fastwrite"} {
		publishMined(t, p, s, 200, func(int) string { return "s1" })
	}
	okCount, okSum := writeDurations(t, "ok")

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.dispatchAll(context.Background())
	}()
	proceed, release := onceCloser(hook.proceed), onceCloser(hook.release)
	t.Cleanup(func() {
		proceed()
		release()
		<-done
	})
	waitFor(t, hook.entered, "the first write")
	waitFor(t, hook.entered, "the second write")

	clock.set(t0.Add(100 * time.Millisecond))
	proceed()
	require.Eventually(t, func() bool {
		count, _ := writeDurations(t, "ok")
		return count == okCount+1
	}, 10*time.Second, time.Millisecond, "LINK write-each: the fast write is observed while the slow one is in flight")
	_, sum := writeDurations(t, "ok")
	require.InDelta(t, 0.1, sum-okSum, 1e-9, "LINK write-own: the fast write is observed as its own duration")

	clock.set(t0.Add(3200 * time.Millisecond))
	release()
	waitFor(t, done, "the drain to end")

	count, sum := writeDurations(t, "ok")
	require.Equal(t, okCount+2, count, "LINK write-each: one write is one observation")
	require.InDelta(t, 3.3, sum-okSum, 1e-9, "LINK write-own: the slow write lasts until its own EXEC came back")
}

// TestDispatchWriteDurationLabelsAWriteRefusedForMemory: a write Redis refuses
// for memory is observed as oom, not among the writes that landed.
func TestDispatchWriteDurationLabelsAWriteRefusedForMemory(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	client.AddHook(&redisRefusesForMemory{})
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	t.Cleanup(func() { _ = p.Close() })
	publishMined(t, p, "pokt1oomwrite", 3, func(int) string { return "s1" })
	okCount, _ := writeDurations(t, "ok")
	oomCount, _ := writeDurations(t, "oom")

	p.dispatchAll(context.Background())

	count, _ := writeDurations(t, "oom")
	require.Equal(t, oomCount+1, count, "LINK write-oom: a write refused for memory is observed as oom")
	count, _ = writeDurations(t, "ok")
	require.Equal(t, okCount, count, "and not as a write that landed")
}
