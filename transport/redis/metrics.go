package redis

import (
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "transport_redis"
)

var (
	// Publisher metrics

	publishedTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "published_total",
			Help:      "Total number of mined relays published to Redis Streams",
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

	// Consumer metrics

	consumedTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "consumed_total",
			Help:      "Total number of mined relays consumed from Redis Streams",
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
			Help:      "Total Redis reconnection attempts by component",
		},
		[]string{"component"},
	)

	redisReconnectionSuccess = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reconnection_success_total",
			Help:      "Successful Redis reconnections by component",
		},
		[]string{"component"},
	)

	// Reclaim / reaper metrics

	reclaimErrorsTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reclaim_errors_total",
			Help:      "Reclaim scan operations that failed, by Redis operation. A failure aborts the whole drain for that tick, not just one page",
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
