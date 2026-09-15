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

	// SMSTStoreOperations tracks store operations for SMST storage.
	SMSTStoreOperations = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "store_operations_total",
			Help:      "Total number of store operations for SMST storage",
		},
		[]string{"operation", "result"},
	)

	// SMSTStoreOperationDuration tracks latency of store operations for SMST.
	SMSTStoreOperationDuration = MinerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "store_operation_duration_seconds",
			Help:      "Duration of store operations for SMST storage",
			Buckets:   MicroLatencyBuckets,
		},
		[]string{"operation"},
	)

	// SMSTStoreErrors tracks store error counts for SMST storage.
	SMSTStoreErrors = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "store_errors_total",
			Help:      "Total number of store errors for SMST storage",
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
			Help:      "Sessions whose stored SMST state was purged after repeated corruption evictions (escalation past persistentCorruptionThreshold)",
		},
		[]string{"supplier", "reason"},
	)

	// SMSTLeavesCompacted counts persisted SMST leaves whose in-memory
	// value has been dropped by CompactPersistedLeaves. A non-zero rate is
	// the only way to confirm
	// compaction is actually running: it is deliberately wired to be
	// mandatory (see updateTree), so its absence is a build-time or
	// startup-log signal, not a metric.
	SMSTLeavesCompacted = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "leaves_compacted_total",
			Help:      "Persisted SMST leaves whose in-memory value was dropped by CompactPersistedLeaves",
		},
		[]string{"supplier"},
	)

	// SMSTCompactionFailures counts a CompactPersistedLeaves call that
	// returned an error or panicked. The relay that
	// triggered it is never lost -- Update, Commit and FlushPipeline all
	// already succeeded by the time compaction runs -- so this tracks a
	// missed memory-reclaim opportunity, not a correctness failure. Alert
	// on a sustained non-zero rate: a transient one is expected under the
	// same corruption shapes SMSTPanicsRecovered already covers.
	SMSTCompactionFailures = MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "compaction_failures_total",
			Help:      "CompactPersistedLeaves calls that failed or panicked, without losing the relay that triggered them",
		},
		[]string{"supplier"},
	)
)
