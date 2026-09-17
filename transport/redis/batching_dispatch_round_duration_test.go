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

// roundDurations reads the count and sum of the round duration histogram for result.
func roundDurations(t *testing.T, result string) (uint64, float64) {
	t.Helper()
	var m dto.Metric
	require.NoError(t, dispatchRoundDuration.WithLabelValues(result).(interface{ Write(*dto.Metric) error }).Write(&m))
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

// TestDispatchRoundDurationIsTheSlowestWriteOfTheRound: two workers, one answered
// 100 ms into the round and the other 3.2 s into it. The round is observed once,
// as 3.2 s: what admission's heartbeat saw in flight.
func TestDispatchRoundDurationIsTheSlowestWriteOfTheRound(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	slow := transport.SupplierStreamName(prefix, "pokt1slowround")
	hook := &holdPipelines{
		stuck:   slow,
		entered: make(chan string, 2),
		release: make(chan struct{}),
		proceed: make(chan struct{}),
	}
	client.AddHook(hook)
	t0 := time.Unix(1_700_000_000, 0)
	clock := newFakeClock(t0)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2), withClock(clock.now))
	t.Cleanup(func() { _ = p.Close() })
	for _, s := range []string{"pokt1slowround", "pokt1fastround"} {
		publishMined(t, p, s, 200, func(int) string { return "s1" })
	}
	okCount, okSum := roundDurations(t, "ok")

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
	waitFor(t, hook.entered, "the first write of the round")
	waitFor(t, hook.entered, "the second write of the round")

	fastAt := t0.Add(100 * time.Millisecond)
	clock.set(fastAt)
	proceed()
	require.Eventually(t, func() bool { return p.lastSuccess.Load() == fastAt.UnixNano() },
		10*time.Second, time.Millisecond, "premise: the fast write was answered first")
	count, _ := roundDurations(t, "ok")
	require.Equal(t, okCount, count, "LINK round-once: nothing is observed while the round's slowest write is in flight")

	clock.set(t0.Add(3200 * time.Millisecond))
	release()
	waitFor(t, done, "the round to end")

	count, sum := roundDurations(t, "ok")
	require.Equal(t, okCount+1, count, "LINK round-once: one round is one observation")
	require.InDelta(t, 3.2, sum-okSum, 1e-9, "LINK round-slowest: the round lasts until its slowest write came back")
}

// TestDispatchRoundDurationLabelsARoundRefusedForMemory: a round Redis refuses
// for memory is observed as oom, not among the rounds that wrote.
func TestDispatchRoundDurationLabelsARoundRefusedForMemory(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	client.AddHook(&redisRefusesForMemory{})
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	t.Cleanup(func() { _ = p.Close() })
	publishMined(t, p, "pokt1oomround", 3, func(int) string { return "s1" })
	okCount, _ := roundDurations(t, "ok")
	oomCount, _ := roundDurations(t, "oom")

	p.dispatchAll(context.Background())

	count, _ := roundDurations(t, "oom")
	require.Equal(t, oomCount+1, count, "LINK round-oom: a round refused for memory is observed as oom")
	count, _ = roundDurations(t, "ok")
	require.Equal(t, okCount, count, "and not as a round that wrote")
}
