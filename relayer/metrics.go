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

	// backendLatency is the total latency of backend requests (upstream
	// service call). The "outcome" label distinguishes successful calls
	// from failures so dashboards can split p99 success vs p99 error —
	// without it, a small number of slow timeouts skew the success p99.
	//
	// outcome values:
	//   - success             : 2xx/3xx/4xx response received and read
	//   - backend_5xx         : 5xx response (not mined)
	//   - backend_timeout     : our internal context deadline fired
	//   - client_disconnected : PATH cancelled the request mid-flight
	//   - backend_network_error: dial/read/reset/other transport error
	backendLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "backend_latency_seconds",
			Help:      "Total latency of backend requests (upstream service call)",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id", "outcome"},
	)

	// backendRequests is a per-outcome counter so dashboards can compute
	// error rate as a ratio (non-success / total) without needing to derive
	// it from histogram counts. status_code is the HTTP status string for
	// completed requests, or "none" for errors with no response.
	backendRequests = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "backend_requests_total",
			Help:      "Total backend requests by service, outcome, and HTTP status code",
		},
		[]string{"service_id", "outcome", "status_code"},
	)

	// httpPoolInFlight is the current number of in-flight backend requests
	// per service. Together with httpPoolMaxConns this gives pool
	// saturation: in_flight / max_conns. If saturation stays near 1.0, the
	// service is pool-bound and needs a bigger profile (or a faster
	// backend).
	httpPoolInFlight = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "http_pool_in_flight",
			Help:      "Current number of in-flight backend HTTP requests per service",
		},
		[]string{"service_id"},
	)

	// httpPoolWaitSeconds is the time each request waited for a free pool
	// slot (GetConn -> GotConn). Non-zero values mean the pool is
	// saturated and requests are queuing.
	httpPoolWaitSeconds = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "http_pool_wait_seconds",
			Help:      "Time spent waiting for a free HTTP pool connection, per service",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id"},
	)

	// httpPoolMaxConns is the configured max_conns_per_host for each
	// service, exported as a gauge so dashboards can compute saturation
	// percentage without needing to read the config file.
	httpPoolMaxConns = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "http_pool_max_conns",
			Help:      "Configured max_conns_per_host for the service (from pool_profile)",
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

	// relayPreBackendLatency is the time spent by the relayer BEFORE the
	// upstream backend call starts — i.e. ring signature verification,
	// session lookup, relay-meter check, request body parsing and the
	// first HTTP pool acquisition. Everything in this metric is 100%
	// relayer CPU/IO; none of it is upstream latency. It lets us tell
	// "are we slow?" from "is the backend slow?" without subtracting
	// p99s across independent distributions (which is arithmetically
	// meaningless).
	relayPreBackendLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_pre_backend_seconds",
			Help:      "Per-request relayer time BEFORE backend call (validate + meter + request build + pool wait). Excludes backend.",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id", "rpc_type"},
	)

	// relayPostBackendLatency is the time spent by the relayer AFTER
	// the upstream backend response is received — response signing
	// (ECDSA), optional gzip compression, client write and WAL publish
	// to Redis Streams. Same rationale as relayPreBackendLatency.
	relayPostBackendLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_post_backend_seconds",
			Help:      "Per-request relayer time AFTER backend call (sign + compress + write + publish WAL). Excludes backend.",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id", "rpc_type"},
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

	// Health check metrics (per-endpoint visibility via "endpoint" label)
	healthCheckSuccesses = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "health_check_successes_total",
			Help:      "Total number of successful health checks",
		},
		[]string{"service_id", "endpoint"},
	)

	healthCheckFailures = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "health_check_failures_total",
			Help:      "Total number of failed health checks",
		},
		[]string{"service_id", "endpoint"},
	)

	backendHealthStatus = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "backend_health_status",
			Help:      "Current health status of backend (1=healthy, 0=unhealthy)",
		},
		[]string{"service_id", "endpoint"},
	)

	// Fast-fail metrics (separate from relaysRejected per user decision)
	fastFailsTotal = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "fast_fails_total",
			Help:      "Total number of fast-fail responses when all backends are unhealthy",
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
