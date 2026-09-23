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
	// dispatchRoundDuration is how long a round of the batch dispatcher took, from
	// its start until the slowest EXEC in it came back, by result (ok, error,
	// oom). While a round is in flight admission measures the dispatcher's age
	// from its start, so a round longer than the heartbeat limit keeps admission
	// closed for what it lasts past that limit.
	dispatchRoundDuration = observability.SharedFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dispatch_round_duration_seconds",
			Help:      "Duration of a batch dispatcher round, from its start until its slowest EXEC came back, by result",
			Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 5, 7, 10, 20, 30},
		},
		[]string{"result"},
	)

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

	// The four below say what the consumer asks Redis for and what it holds, in
	// BYTES as well as entries: a count bound admits a GiB per read once relays
	// are a MiB each, and the miner's heap under that load was 75% relays read
	// and not yet processed, with no series saying where they sat.
	consumerReadRequestedCount = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "consumer_read_requested_count",
			Help:      "COUNT the last XREADGROUP of this supplier's stream asked for",
		},
		[]string{"supplier_addr"},
	)

	consumerReadReplyBytes = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "consumer_read_reply_bytes",
			Help:      "Payload bytes of the XREADGROUP reply being parsed now; 0 once the reply is handed over",
		},
		[]string{"supplier_addr"},
	)

	consumerReadBytesTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "consumer_read_bytes_total",
			Help:      "Payload bytes read from this supplier's stream, reclaims included",
		},
		[]string{"supplier_addr"},
	)

	consumerChannelBytes = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "consumer_channel_bytes",
			Help:      "Relay bytes parsed and waiting in the consumer's delivery channel for the miner to take",
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

	// relayCompressionTotal counts, per service, what the relayer decided for each
	// mined relay's bytes before writing it to the WAL: compressed, or why not
	// (below_threshold, over_max, probe_incompressible, not_smaller).
	relayCompressionTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_compression_total",
			Help:      "Mined relays by what the relayer decided about compressing their bytes before the WAL write, by service and outcome",
		},
		[]string{"service_id", "outcome"},
	)

	// relayCompressionBytes counts the relay bytes the relayer COMPRESSED, before
	// (stage=in) and after (stage=out). A relay that stayed raw counts in neither:
	// in/out is the ratio of what compression was applied to, and
	// relay_compression_total says how many relays that was.
	relayCompressionBytes = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_compression_bytes_total",
			Help:      "Bytes of the relays the relayer compressed before the WAL write, before (stage=in) and after (stage=out), by service",
		},
		[]string{"service_id", "stage"},
	)

	// Note: Stream discovery metrics removed with single-stream-per-supplier architecture.
	// Discovery is no longer needed - we consume from a single known stream per supplier.
)
