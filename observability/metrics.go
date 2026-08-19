package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "observability"
)

var (
	// FineGrainedLatencyBuckets provides sub-millisecond to multi-second measurement.
	// Use for: relay latency, query latency, cache operations, signing, validation, etc.
	// Buckets: 1ms, 2ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s, 30s
	FineGrainedLatencyBuckets = []float64{0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

	// MicroLatencyBuckets provides ultra-fine-grained measurement for sub-millisecond operations.
	// Use for: SMST operations, in-memory cache hits, hash computations, marshaling, etc.
	// Buckets: 10µs, 50µs, 100µs, 500µs, 1ms, 5ms, 10ms, 50ms, 100ms
	MicroLatencyBuckets = []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1}
)

var (

	// SMSTRedisOperations tracks Redis operations for SMST storage.
	SMSTRedisOperations = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "redis_operations_total",
			Help:      "Total number of Redis operations for SMST storage",
		},
		[]string{"operation", "result"},
	)

	// SMSTRedisOperationDuration tracks latency of Redis operations for SMST.
	SMSTRedisOperationDuration = MinerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "redis_operation_duration_seconds",
			Help:      "Duration of Redis operations for SMST storage",
			Buckets:   MicroLatencyBuckets,
		},
		[]string{"operation"},
	)

	// SMSTRedisErrors tracks Redis error counts for SMST storage.
	SMSTRedisErrors = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "redis_errors_total",
			Help:      "Total number of Redis errors for SMST storage",
		},
		[]string{"operation", "error_type"},
	)

	// SMSTPanicsRecovered counts panics caught at the miner -> smt
	// library boundary (UpdateTree, Commit, ProveClosest, Import, etc.).
	// Any non-zero value is a data-corruption signal (missing node,
	// malformed payload, unexpected library assertion) that the
	// defensive wrapper converted into an error instead of tumbling the
	// relay-consumer goroutine. Alert on rate > 0.
	SMSTPanicsRecovered = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "panics_recovered_total",
			Help:      "SMT library panics caught by miner defer/recover at the trie operation boundary",
		},
		[]string{"supplier", "operation"},
	)

	// SMSTCorruptionEvictions counts sessions whose in-memory tree was
	// evicted after corruption (panic or ErrSMSTNodeMissing) so the
	// next relay starts from a consistent Redis state. Relays already
	// committed to the nodes hash survive; only the session's cached
	// in-memory pointer is dropped.
	SMSTCorruptionEvictions = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "corruption_evictions_total",
			Help:      "Sessions whose in-memory SMST was evicted after detected corruption",
		},
		[]string{"supplier", "reason"},
	)

	// SMSTCorruptionPurged counts eviction escalations: sessions that
	// hit the consecutive-corruption threshold (persistent corruption
	// in Redis, not transient memory state) and had their backing keys
	// purged to break the evict→resume→fail loop. A non-zero rate
	// means operators on this instance have legacy / diverged state
	// that the defensive in-memory eviction alone cannot self-heal.
	SMSTCorruptionPurged = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "corruption_purged_total",
			Help:      "Sessions whose Redis-backed SMST state was purged after repeated corruption evictions (escalation past persistentCorruptionThreshold)",
		},
		[]string{"supplier", "reason"},
	)
)
