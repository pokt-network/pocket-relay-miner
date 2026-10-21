package relayer

import (
	"sync/atomic"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// publishWorkersPerService is how many publish tasks one service runs at once.
// A task costs 33-55 µs (BenchmarkPublishTaskCost, 1 KiB to 64 KiB responses),
// so two workers carry a service's share of 1000+ rps many times over. The
// number is not for throughput: a service whose difficulty lookup hangs pins
// its own two workers and no other service's.
const publishWorkersPerService = 2

// servicePublishQueue is one service's publish tasks: its own pool, not drawn
// from the master pool, so a hung service cannot take the workers of another
// service or of validation; and the bytes its waiting tasks retain, bounded so a
// hung service is refused at admission instead of growing until the process is
// killed.
type servicePublishQueue struct {
	pool       pond.Pool
	queued     atomic.Int64
	maxBytes   int64
	bytesGauge prometheus.Gauge
}

// hold adds a task's bytes to the queue and returns what gives them back.
func (q *servicePublishQueue) hold(n int64) (release func()) {
	q.queued.Add(n)
	if q.bytesGauge != nil {
		q.bytesGauge.Add(float64(n))
	}
	return func() {
		q.queued.Add(-n)
		if q.bytesGauge != nil {
			q.bytesGauge.Sub(float64(n))
		}
	}
}

// newPublishQueues builds one queue per configured service. Every service gets
// one, whatever its transport or mode: a relay of a service without a queue is
// refused, never published unbounded.
func newPublishQueues(config *Config) map[string]*servicePublishQueue {
	queues := make(map[string]*servicePublishQueue, len(config.Services))
	for serviceID := range config.Services {
		maxBytes := config.ValidationQueueMaxBytes(serviceID)
		queues[serviceID] = &servicePublishQueue{
			pool:       pond.NewPool(publishWorkersPerService),
			maxBytes:   maxBytes,
			bytesGauge: publishQueueBytes.WithLabelValues(serviceID),
		}
		publishQueueMaxBytes.WithLabelValues(serviceID).Set(float64(maxBytes))
	}
	return queues
}

// full reports that this service's waiting tasks already retain its whole bound.
// A nil queue is full: the relay is refused rather than admitted with no bound.
func (q *servicePublishQueue) full() bool {
	return q == nil || q.queued.Load() >= q.maxBytes
}

// waitingTasks is the tasks waiting in every service's queue, for the publish
// subpool's worker_queue_depth series.
func waitingTasks(queues map[string]*servicePublishQueue) uint64 {
	var n uint64
	for _, q := range queues {
		n += q.pool.WaitingTasks()
	}
	return n
}
