//go:build test

package miner

import (
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestRecordTransitionQueueDepth_ReflectsWedgedSubpool proves the backpressure
// metric does its job: session_transition_queue_depth must rise to the number of
// tasks stuck in the transition subpool's (unbounded) queue, so a window-open
// burst that outpaces the workers is visible BEFORE it becomes RAM pressure or a
// missed claim/proof window. The failure is injected deterministically — a
// single-worker pool wedged on a blocking task forces every subsequent submit to
// queue — rather than relying on reproducing production supplier scale.
func TestRecordTransitionQueueDepth_ReflectsWedgedSubpool(t *testing.T) {
	const sup = "pokt1qdepthtest000000000000000000000000000"

	// Real single-worker pool: with the one worker busy, any extra task queues.
	pool := pond.NewPool(1)
	t.Cleanup(func() { pool.Stop() })

	m := &SessionLifecycleManager{
		logger:            logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:            SessionLifecycleConfig{SupplierAddress: sup},
		transitionSubpool: pool,
	}

	// Baseline: empty queue → gauge 0.
	m.recordTransitionQueueDepth()
	require.Equal(t, float64(0), testutil.ToFloat64(sessionTransitionQueueDepth.WithLabelValues(sup)),
		"empty subpool must report queue depth 0")

	// Wedge the only worker so nothing else can start.
	release := make(chan struct{})
	started := make(chan struct{})
	pool.Submit(func() {
		close(started)
		<-release
	})
	<-started // the worker is now occupied

	// Submit more tasks than the pool can run; they pile up in the queue.
	const queued = 5
	for i := 0; i < queued; i++ {
		pool.Submit(func() { <-release })
	}

	// pond updates its counters asynchronously; wait until the queue reflects the
	// backlog (deterministic poll, no fixed sleep).
	require.Eventually(t, func() bool { return pool.WaitingTasks() == uint64(queued) },
		2*time.Second, time.Millisecond, "subpool queue must hold the %d wedged tasks", queued)

	// The metric must surface exactly that backlog.
	m.recordTransitionQueueDepth()
	require.Equal(t, float64(queued), testutil.ToFloat64(sessionTransitionQueueDepth.WithLabelValues(sup)),
		"gauge must reflect the transition subpool queue depth under backpressure")

	// Release the backlog and confirm the gauge drains back to 0.
	close(release)
	require.Eventually(t, func() bool { return pool.WaitingTasks() == 0 },
		2*time.Second, time.Millisecond, "queue must drain once tasks are released")
	m.recordTransitionQueueDepth()
	require.Equal(t, float64(0), testutil.ToFloat64(sessionTransitionQueueDepth.WithLabelValues(sup)),
		"gauge must return to 0 after the backlog clears")
}
