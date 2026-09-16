package redis

import (
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricsNamespace = "ha"
	// The subsystem is the CONCEPT, not the implementation: these metric names
	// (published_total, consumed_total, reconnection_attempts_total, ...) describe
	// a relay stream, and a different store behind the same stream would emit the
	// same series rather than a parallel set nobody graphs.
	metricsSubsystem = "transport"
)

var (
	// storeOperable is 1 while StoreHealth admits work for the component and 0
	// while it does not.
	storeOperable = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "store_operable",
			Help:      "1 while Redis is taken as able to accept writes and work is admitted, 0 while it is not",
		},
		[]string{"component", "gate"},
	)

	// storeTransitions counts StoreHealth changing state. state is open or closed;
	// reason is why the store closed (memory_reserve, oom_reply, sample_stale), on
	// both the closing and the reopening transition.
	storeTransitions = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "store_transitions_total",
			Help:      "Times Redis was taken as not operable (state=closed) or operable again (state=open), by why it closed",
		},
		[]string{"component", "gate", "state", "reason"},
	)

	// storeClosedSeconds adds, when the store reopens, how long it was closed, by
	// why it closed. A store still closed has not added its current closure yet.
	storeClosedSeconds = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "store_closed_seconds_total",
			Help:      "Seconds Redis was taken as not operable, added when it reopens, by why it closed",
		},
		[]string{"component", "gate", "reason"},
	)

	// storeFreeBytes is maxmemory minus used_memory at the last sample, or -1 when
	// Redis has no maxmemory.
	storeFreeBytes = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "store_free_bytes",
			Help:      "Redis maxmemory minus used_memory at the last sample; -1 when maxmemory is not set",
		},
		[]string{"component"},
	)

	// Publisher metrics

	publishedTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "published_total",
			Help:      "Total number of mined relays published to the relay stream",
		},
		[]string{"supplier_addr", "service_id"},
	)

	publishErrorsTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "publish_errors_total",
			Help:      "Total number of publish errors",
		},
		[]string{"supplier_addr", "service_id"},
	)

	// shutdownAbandonedRelays MUST stay at zero. The final flush runs on a
	// context detached from the shutdown, with its own 30s budget, so anything
	// counted here is a relay that was served, signed and answered to a client
	// and that the process then exited without writing. Until this existed the
	// shutdown logged "batching publisher closed" and returned nil in exactly
	// that case, which is the same line it logs when it drained everything.
	shutdownAbandonedRelays = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "shutdown_abandoned_relays_total",
			Help:      "Mined relays still queued when the publisher's final flush gave up. Any non-zero value is served work that was never written to the stream",
		},
		[]string{"supplier_addr", "service_id"},
	)

	// chargeWriteFailures counts consumed-counter writes that did not land as
	// intended. reason is bounded: exec_unknown (the EXEC's outcome never came back;
	// the charge is dropped rather than risk billing twice), attempts_exhausted
	// (Redis kept refusing the INCRBY; the charge is dropped), expire_failed (the
	// INCRBY landed and its EXPIRE NX did not).
	chargeWriteFailures = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "charge_write_failures_total",
			Help:      "Consumed-counter writes that did not land as intended, by bounded reason",
		},
		[]string{"reason"},
	)

	// Consumer metrics

	consumedTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "consumed_total",
			Help:      "Total number of mined relays consumed from the relay stream",
		},
		[]string{"supplier_addr", "service_id"},
	)

	consumeErrorsTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "consume_errors_total",
			Help:      "Total number of consume errors",
		},
		[]string{"supplier_addr", "error_type"},
	)

	ackedTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "acked_total",
			Help:      "Total number of messages acknowledged",
		},
		[]string{"supplier_addr"},
	)

	claimedMessages = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claimed_total",
			Help:      "Total number of messages claimed from idle consumers",
		},
		[]string{"supplier_addr"},
	)

	deserializationErrors = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "deserialization_errors_total",
			Help:      "Total number of message deserialization errors",
		},
		[]string{"supplier_addr"},
	)

	// End-to-end latency from publish to consume
	endToEndLatency = observability.SharedFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "end_to_end_latency_seconds",
			Help:      "End-to-end latency from publish to consume",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"supplier_addr", "service_id"},
	)

	// Reconnection metrics
	// Track reconnection attempts and successes for Redis operations

	redisReconnectionAttempts = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reconnection_attempts_total",
			Help:      "Total store reconnection attempts by component",
		},
		[]string{"component"},
	)

	redisReconnectionSuccess = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reconnection_success_total",
			Help:      "Successful store reconnections by component",
		},
		[]string{"component"},
	)

	// Reclaim / reaper metrics

	reclaimErrorsTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reclaim_errors_total",
			Help:      "Reclaim scan operations that failed, by store operation. A failure aborts the whole drain for that tick, not just one page",
		},
		[]string{"supplier_addr", "op"},
	)

	reapedConsumersTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reaped_consumers_total",
			Help:      "Dead consumer records removed from a stream group after being seen with an empty PEL and idle past the reap threshold",
		},
		[]string{"supplier_addr"},
	)

	// reapDestroyedPendingTotal MUST stay at zero. XGROUP DELCONSUMER returns how many
	// pending entries it destroyed, and the reaper only deletes consumers it has just
	// observed with an empty PEL -- so a non-zero value here is a relay that was
	// acknowledged into oblivion by the race between that observation and the delete.
	// It is measured rather than assumed: Redis offers no conditional delete, so the
	// return value is the only evidence that the guard held.
	reapDestroyedPendingTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reap_destroyed_pending_total",
			Help:      "Pending entries destroyed by reaping a consumer that was observed empty. Any non-zero value is lost relays and a bug in the reaper guard",
		},
		[]string{"supplier_addr"},
	)

	// Note: Stream discovery metrics removed with single-stream-per-supplier architecture.
	// Discovery is no longer needed - we consume from a single known stream per supplier.
)
