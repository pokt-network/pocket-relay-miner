package relayer

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "relayer"
)

var (
	// Request metrics
	relaysReceived = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_received_total",
			Help:      "Total number of relay requests received",
		},
		[]string{"service_id", "rpc_type"},
	)

	relaysServed = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_served_total",
			Help:      "Total number of relay requests successfully served",
		},
		[]string{"service_id", "rpc_type", "status_code"},
	)

	relaysRejected = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_rejected_total",
			Help:      "Total number of relay requests rejected",
		},
		[]string{"service_id", "rpc_type", "reason"},
	)

	relaysPublished = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_published_total",
			Help:      "Total number of mined relays published to Redis",
		},
		[]string{"service_id", "supplier"},
	)

	relaysDropped = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_dropped_total",
			Help:      "Total number of relays served but not mined (optimistic mode: validation failed, meter error, stake exhausted)",
		},
		[]string{"service_id", "application", "reason"},
	)

	// === CRITICAL HISTOGRAMS (async recorded to avoid hot path blocking) ===
	// Only 4 histograms to minimize lock contention - recorded via MetricRecorder worker

	relayLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_latency_seconds",
			Help:      "End-to-end latency of relay requests (request received to response sent)",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id", "rpc_type"},
	)

	backendLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "backend_latency_seconds",
			Help:      "Total latency of backend requests (upstream service call)",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id"},
	)

	validationLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "validation_latency_seconds",
			Help:      "Latency of relay validation (signature, session, params)",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id", "mode"}, // mode: eager, optimistic
	)

	relayMeterLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_meter_latency_seconds",
			Help:      "Latency of relay meter check and consume operations (Redis calls)",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id", "mode"}, // mode: eager, optimistic
	)

	validationFailures = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "validation_failures_total",
			Help:      "Total number of relay validation failures",
		},
		[]string{"service_id", "reason"},
	)

	// Late relay metrics (reserved for future instrumentation)
	_ = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "late_relays_received_total",
			Help:      "Total number of relays received after session ended",
		},
		[]string{"service_id"},
	)

	_ = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "late_relays_within_grace_total",
			Help:      "Total number of late relays that were within grace period",
		},
		[]string{"service_id"},
	)

	_ = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "late_relays_rejected_total",
			Help:      "Total number of late relays rejected (past grace period)",
		},
		[]string{"service_id"},
	)

	// Health check metrics
	healthCheckSuccesses = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "health_check_successes_total",
			Help:      "Total number of successful health checks",
		},
		[]string{"service_id"},
	)

	healthCheckFailures = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "health_check_failures_total",
			Help:      "Total number of failed health checks",
		},
		[]string{"service_id"},
	)

	backendHealthStatus = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "backend_health_status",
			Help:      "Current health status of backend (1=healthy, 0=unhealthy)",
		},
		[]string{"service_id"},
	)

	// Request size metrics
	requestBodySize = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "request_body_size_bytes",
			Help:      "Size of request bodies in bytes",
			Buckets:   []float64{100, 1000, 10000, 100000, 1000000, 10000000},
		},
		[]string{"service_id", "rpc_type"},
	)

	responseBodySize = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "response_body_size_bytes",
			Help:      "Size of response bodies in bytes",
			Buckets:   []float64{100, 1000, 10000, 100000, 1000000, 10000000},
		},
		[]string{"service_id", "rpc_type"},
	)

	// Block height metric
	currentBlockHeight = observability.RelayerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "current_block_height",
			Help:      "Current block height as seen by the relayer",
		},
	)

	// Active connections
	activeConnections = observability.RelayerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "active_connections",
			Help:      "Number of active HTTP connections",
		},
	)

	// Streaming metrics
	streamingRelaysServed = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "streaming_relays_served_total",
			Help:      "Total number of streaming relay requests served (SSE/NDJSON)",
		},
		[]string{"service_id"},
	)

	streamingChunksForwarded = observability.RelayerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "streaming_chunks_forwarded_total",
			Help:      "Total number of streaming chunks forwarded to clients",
		},
	)

	streamingBytesForwarded = observability.RelayerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "streaming_bytes_forwarded_total",
			Help:      "Total bytes forwarded in streaming responses",
		},
	)

	streamingBatchesSigned = observability.RelayerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "streaming_batches_signed_total",
			Help:      "Total number of streaming batches signed (for SSE/NDJSON LLM responses)",
		},
	)

	// Mining difficulty metrics
	relaysSkippedDifficulty = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_skipped_difficulty_total",
			Help:      "Total number of relays skipped due to not meeting mining difficulty",
		},
		[]string{"service_id"},
	)

	relaysMinedSuccessfully = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_mined_total",
			Help:      "Total number of relays that met mining difficulty and were mined",
		},
		[]string{"service_id"},
	)

	// relaySigningLatency reserved for future instrumentation
	_ = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_signing_latency_seconds",
			Help:      "Latency of relay response signing",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"service_id"},
	)

	// difficultyLookupLatency reserved for future instrumentation
	_ = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "difficulty_lookup_latency_seconds",
			Help:      "Latency of difficulty target lookups",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"service_id"},
	)

	// WebSocket metrics
	wsConnectionsActive = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "websocket_connections_active",
			Help:      "Number of active WebSocket connections",
		},
		[]string{"service_id"},
	)

	wsConnectionsTotal = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "websocket_connections_total",
			Help:      "Total number of WebSocket connections established",
		},
		[]string{"service_id"},
	)

	wsMessagesForwarded = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "websocket_messages_forwarded_total",
			Help:      "Total number of WebSocket messages forwarded",
		},
		[]string{"service_id", "direction"}, // direction: gateway_to_backend, backend_to_gateway
	)

	wsRelaysEmitted = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "websocket_relays_emitted_total",
			Help:      "Total number of relays emitted for billing from WebSocket connections",
		},
		[]string{"service_id"},
	)

	// gRPC Relay Service metrics (for proper relay protocol over gRPC)
	grpcRelaysTotal = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "grpc_relays_total",
			Help:      "Total number of gRPC relay requests processed",
		},
		[]string{"service_id"},
	)

	grpcRelayErrors = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "grpc_relay_errors_total",
			Help:      "Total number of gRPC relay request errors",
		},
		[]string{"service_id", "reason"},
	)

	grpcRelayLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "grpc_relay_latency_seconds",
			Help:      "Latency of gRPC relay requests",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id"},
	)

	grpcRelaysPublished = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "grpc_relays_published_total",
			Help:      "Total number of gRPC relays published to Redis",
		},
		[]string{"service_id"},
	)

	grpcWebRequestsTotal = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "grpc_web_requests_total",
			Help:      "Total number of gRPC-Web requests received",
		},
		[]string{"service_id"},
	)

	// Relay meter metrics
	relayMeterConsumptions = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_meter_consumptions_total",
			Help:      "Total relay meter consumption checks",
		},
		[]string{"service_id", "result"}, // result: within_limit, over_limit
	)

	relayMeterSessionsActive = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_meter_sessions_active",
			Help:      "Number of active session meters",
		},
		[]string{"supplier", "service_id"},
	)

	relayMeterRedisErrors = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_meter_redis_errors_total",
			Help:      "Total relay meter Redis errors",
		},
		[]string{"operation"},
	)

	// Unused - reserved for future relay meter parameter refresh tracking
	// relayMeterParamsRefreshed = observability.RelayerFactory.NewCounterVec(
	// 	prometheus.CounterOpts{
	// 		Namespace: metricsNamespace,
	// 		Subsystem: metricsSubsystem,
	// 		Name:      "relay_meter_params_refreshed_total",
	// 		Help:      "Total relay meter parameter cache refreshes",
	// 	},
	// 	[]string{"param_type"}, // param_type: shared, session, app_stake, service
	// )
)
