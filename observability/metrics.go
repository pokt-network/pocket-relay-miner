package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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
	// InstructionTimeSeconds tracks the duration of individual instructions.
	InstructionTimeSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "instruction_duration_seconds",
			Help:      "Duration of individual instructions in the relay/mining pipeline",
			Buckets:   []float64{0.00001, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
		},
		[]string{"component", "instruction"},
	)

	// OperationDurationSeconds tracks the duration of high-level operations.
	OperationDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "operation_duration_seconds",
			Help:      "Duration of high-level operations (claim, proof, relay processing)",
			Buckets:   []float64{0.001, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60},
		},
		[]string{"component", "operation", "status"},
	)

	// RedisOperationDurationSeconds tracks Redis operation latencies.
	RedisOperationDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "redis_operation_duration_seconds",
			Help:      "Duration of Redis operations",
			Buckets:   []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1},
		},
		[]string{"operation", "status"},
	)

	// RedisOperationsTotal counts Redis operations.
	RedisOperationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "redis_operations_total",
			Help:      "Total number of Redis operations",
		},
		[]string{"operation", "status"},
	)

	// OnchainQueryDurationSeconds tracks on-chain query latencies.
	OnchainQueryDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "onchain_query_duration_seconds",
			Help:      "Duration of on-chain queries",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
		[]string{"query_type", "status"},
	)

	// OnchainQueriesTotal counts on-chain queries.
	OnchainQueriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "onchain_queries_total",
			Help:      "Total number of on-chain queries",
		},
		[]string{"query_type", "status"},
	)

	// TxSubmissionDurationSeconds tracks transaction submission latencies.
	TxSubmissionDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "tx_submission_duration_seconds",
			Help:      "Duration of transaction submissions (claim/proof)",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 20, 30, 60},
		},
		[]string{"tx_type", "status"},
	)

	// TxSubmissionsTotal counts transaction submissions.
	TxSubmissionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "tx_submissions_total",
			Help:      "Total number of transaction submissions",
		},
		[]string{"tx_type", "status"},
	)

	// SigningDurationSeconds tracks signing operation latencies.
	SigningDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "signing_duration_seconds",
			Help:      "Duration of signing operations",
			Buckets:   []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1},
		},
		[]string{"operation"},
	)

	// CacheHitRatio tracks cache hit/miss ratios.
	CacheOperationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "cache_operations_total",
			Help:      "Total cache operations (hits/misses)",
		},
		[]string{"cache_name", "result"},
	)

	// MemoryUsageBytes tracks memory usage of various components.
	MemoryUsageBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "memory_usage_bytes",
			Help:      "Memory usage in bytes",
		},
		[]string{"component"},
	)

	// GoroutineCount tracks the number of goroutines per component.
	GoroutineCount = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "goroutine_count",
			Help:      "Number of active goroutines per component",
		},
		[]string{"component"},
	)

	// QueueDepth tracks the depth of various internal queues.
	QueueDepth = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "queue_depth",
			Help:      "Current depth of internal queues",
		},
		[]string{"queue_name"},
	)

	// QueueCapacity tracks the capacity of various internal queues.
	QueueCapacity = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "queue_capacity",
			Help:      "Capacity of internal queues",
		},
		[]string{"queue_name"},
	)

	// ErrorsTotal counts errors by type and component.
	ErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "errors_total",
			Help:      "Total number of errors",
		},
		[]string{"component", "error_type"},
	)

	// ProcessInfo provides static information about the process.
	ProcessInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "process_info",
			Help:      "Information about the running process",
		},
		[]string{"version", "component"},
	)

	// StartupDurationSeconds tracks startup time of components.
	StartupDurationSeconds = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "startup_duration_seconds",
			Help:      "Time taken to start components",
		},
		[]string{"component"},
	)

	// SMSTRedisOperations tracks Redis operations for SMST storage.
	SMSTRedisOperations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "redis_operations_total",
			Help:      "Total number of Redis operations for SMST storage",
		},
		[]string{"operation", "result"},
	)

	// SMSTRedisOperationDuration tracks latency of Redis operations for SMST.
	SMSTRedisOperationDuration = promauto.NewHistogramVec(
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
	SMSTRedisErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: "smst",
			Name:      "redis_errors_total",
			Help:      "Total number of Redis errors for SMST storage",
		},
		[]string{"operation", "error_type"},
	)
)
