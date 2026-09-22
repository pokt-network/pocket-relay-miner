package relayer

import (
	"errors"
	"fmt"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "relayer"
)

// statusCodeNoHTTP is the status_code value for transports that HAVE no HTTP
// status: gRPC and WebSocket. It is deliberately NOT a gRPC code -- this repo
// already decided not to mix two numbering systems in one field (see
// tx/tx_rejection.go, which keeps ABCICode and GRPCCode in two differently typed
// fields and says why), and a numeric value here would be read as an HTTP status
// when grouping. It is not "200" either: a padded HTTP code would be a false
// value in a label operators group by.
//
// It asserts success, and that is true BY CONSTRUCTION at both sites that use
// it: the gRPC increment sits after a successful SendMsg on the success branch,
// and the WebSocket one at the top of emitRelay, which is reached only after the
// response was written to the gateway. A future non-success site on those
// transports needs its own value, not this one.
const statusCodeNoHTTP = "ok"

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

	// relaysServedOverBudget counts relays that were served and charged and left
	// their session at or over its budget. A WebSocket backend message is charged
	// after it is served, so the one that reaches the budget is served and then
	// closes the connection: at most one per connection. It is apart from
	// relays_rejected_total because the relay WAS served, and apart from
	// websocket_closes_total because that series does not say whether anything
	// was served past the budget. reason is a constant.
	relaysServedOverBudget = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_served_over_budget_total",
			Help:      "Relays served and charged that left their session at or over its budget",
		},
		[]string{"service_id", "rpc_type", "reason"},
	)

	// backendMissing counts relays that resolved no backend pool for their
	// requested transport type on a service that exists. Under the strict
	// backend contract (no cross-transport fallback) this is the signal an
	// operator needs: e.g. a `websocket` relay arriving at a service that has
	// only a jsonrpc backend configured. rpc_type is the resolved backend-type
	// name, bounded to the five known transports (plus any explicit override),
	// so cardinality stays low.
	backendMissing = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "backend_missing_total",
			Help:      "Relays with no backend configured for their transport type (service exists but lacks that backend)",
		},
		[]string{"service_id", "rpc_type"},
	)

	// liveConnectionsCut counts live WebSocket bridges and in-flight gRPC relays
	// cut because Redis stopped taking writes, by transport (websocket, grpc).
	liveConnectionsCut = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "live_connections_cut_total",
			Help:      "Live WebSocket bridges and in-flight gRPC relays cut because Redis stopped taking writes",
		},
		[]string{"transport"},
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

	// undeclaredTransportServed counts relays served for a (service, transport)
	// the supplier did NOT declare on-chain (it staked the service but not that
	// rpc_type endpoint). The relay is still served and is claimable — the chain
	// keys claims by (supplier, session) and never sees the transport — so this
	// is a visibility signal, not a rejection: declare the endpoint on-chain so
	// PATH routes it deliberately. It also fires for every relay of a supplier
	// whose miner is too old to publish the per-transport stake view, because an
	// empty view now declares nothing; the deduped warn names which of the two
	// cases it is.
	// Cardinality is service_id × rpc_type (bounded); the supplier is in the
	// deduped warn log, not a label.
	undeclaredTransportServed = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "undeclared_transport_served_total",
			Help:      "Relays served for a (service, transport) the supplier did not declare on-chain (staked the service but not that rpc_type)",
		},
		[]string{"service_id", "rpc_type"},
	)

	// relaysServedOptimistically counts relays served during the boot window
	// for a supplier that is absent from the registry but whose operator key
	// this relayer holds (so it is ours). See handleRelay: the miner is the
	// final arbiter and won't claim a non-staked supplier, so this is safe.
	// A persistently high rate means the registry is not being populated.
	relaysServedOptimistically = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_served_optimistically_total",
			Help:      "Relays served for an owned supplier not yet in the registry (boot-window optimistic path)",
		},
		[]string{"service_id", "rpc_type"},
	)

	relaysPublished = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_published_total",
			Help:      "Mined relays ACCEPTED by the publisher, over any transport. Accepted is not written: the relayer always batches, so a relay is counted here the moment it is queued. ha_transport_published_total is the one that means it reached the stream, and this counter minus that one is what is queued and not yet dispatched. rpc_type is the transport that published it",
		},
		[]string{"service_id", "supplier", "rpc_type"},
	)

	relaysDropped = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_dropped_total",
			Help:      "Total number of relays served but not mined (optimistic mode: validation failed, meter error, stake exhausted). Because the relayer always batches, reason=publish_failed is only a REJECTION at enqueue -- a malformed message, or a closed publisher -- because a failing XADD no longer reaches the caller; those are counted in ha_transport_publish_errors_total",
		},
		// application excluded: on-chain bech32 address is unbounded on a
		// Counter → TSDB OOM. Per-app detail is in the drop logs.
		[]string{"service_id", "rpc_type", "reason"},
	)

	// simulatedRelaysTotal counts SIMULATED relays only. It is deliberately
	// separate from every real-relay counter above: a simulated relay never
	// increments relaysReceived/relaysServed/relaysRejected/etc. `key_id` is
	// intentionally NOT a label (operator-chosen, potentially unbounded).
	// result ∈ {success, rate_limited, verify_failed, replay_rejected,
	// identity_mismatch, service_unknown, supplier_not_loaded, sign_failed,
	// backend_error, meter_degraded}.
	simulatedRelaysTotal = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "simulated_relays_total",
			Help:      "Total simulated relays (health-check/probe traffic), isolated from real-relay counters",
		},
		[]string{"transport", "service", "supplier", "result"},
	)

	// simulatedRelayDuration is the end-to-end latency of simulated relays,
	// kept separate from relayLatency so probe traffic never skews real p99s.
	simulatedRelayDuration = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "simulated_relay_duration_seconds",
			Help:      "End-to-end latency of simulated relays",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"transport", "service"},
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
	// workerPoolMaxWorkers is each subpool's capacity, so the depth above can be
	// read against something. It changes only with the CPU limit.
	workerPoolMaxWorkers = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_max_workers",
			Help:      "Concurrent workers a subpool is allowed",
		},
		[]string{"subpool"},
	)

	// publishQueueBytes is the request AND response bodies the publish queue is
	// holding. Task COUNT is not a proxy for it: a hundred tasks carrying 100 KB
	// each are 10 MB of retained heap and a hundred carrying ten bytes are
	// nothing, and it is the bytes that end a process, not the count.
	// Per SERVICE, because the bound is per service: with a global bound the
	// service that PAID the rejection and the one that was OCCUPYING could be
	// different, which is the defect this replaced. The max below is its twin:
	// a depth is only readable against the ceiling it is approaching, the same
	// pairing as http_pool_in_flight with http_pool_max_conns.
	validationQueueBytes = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "validation_queue_bytes",
			Help:      "Request and response bodies held by this service's optimistic relays, served and not yet validated",
		},
		[]string{"service_id"},
	)

	validationQueueMaxBytes = observability.RelayerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "validation_queue_max_bytes",
			Help:      "Most this service's optimistic relays may hold before it is refused (effective bound, floor applied)",
		},
		[]string{"service_id"},
	)

	publishQueueBytes = observability.RelayerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "publish_queue_bytes",
			Help:      "Request and response bodies currently retained by queued publish tasks",
		},
	)

	// BatchQueueBytes is the batcher's own retained-bytes count -- the exact
	// number the admission gate compares against redis.batch_max_queued_mib
	// (cmd/cmd_relayer.go). It stayed unexported until 2026-09-22: a pulse test
	// found a 1 GiB relayer RSS spike with zero publish_queue_full rejections,
	// and there was no metric to say whether the gate saw it or not -- the
	// number that decides admission was invisible to Prometheus.
	BatchQueueBytes = observability.RelayerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "batch_queue_bytes",
			Help:      "Bytes the batching publisher retains right now -- the exact value the admission gate compares against redis.batch_max_queued_mib",
		},
	)

	// signingKeysLoaded is how many supplier signing keys this relayer holds
	// right now. It moves on every hot reload.
	//
	// It is an OBSERVABLE and deliberately not a guard. The Redis pool is sized
	// from the bounded workers and carries no supplier term, because what a
	// supplier costs depends on how many applications relay through it -- demand
	// we neither choose nor can read at startup. So there is no "expected"
	// number to compare this against, and a guard would need a threshold
	// somebody invented. What this gives an operator is the correlation instead:
	// the moment latency changed is the moment the set went from 52 to 300.
	signingKeysLoaded = observability.RelayerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "signing_keys_loaded",
			Help:      "Supplier signing keys the relayer currently holds (changes on key hot reload)",
		},
	)

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

	// relayMeterUnbilled counts relays that were SERVED and submitted for mining
	// without their stake being metered, because the meter could not answer.
	//
	// It exists because that outcome has no other signal. In optimistic mode the
	// meter runs after the response is out, so refusing is not an option -- the
	// relay is gone -- and dropping it, which is what happened until
	// 2026-08-31, threw away work whose backend call was already paid for. What
	// is left is over-servicing, bounded by the application's stake and settled
	// by the chain, and this is the series that says how much of it happened.
	relayMeterUnbilled = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_meter_unbilled_total",
			Help:      "Relays served and submitted for mining without being metered (the meter could not answer)",
		},
		[]string{"service_id"},
	)

	relayMeterLatency = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_meter_latency_seconds",
			Help:      "Latency of relay meter check and consume operations (store calls)",
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

	// backendConnReused answers "is upstream keepalive working?" per service.
	// Incremented in httptrace.GotConn: reused="true" means the pool served an
	// idle conn (keepalive is working); reused="false" means a fresh TCP dial
	// was required (upstream closed the conn, or we ran out of idles). Track
	// the ratio per-service: if a specific backend shows low reuse, THAT
	// upstream is the one sabotaging keepalive — our own config is fine.
	backendConnReused = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "backend_conn_reused_total",
			Help:      "Backend conn acquisitions labelled by reuse status (true=idle pool hit, false=new TCP dial required). Per-service reuse ratio diagnoses upstream keepalive hygiene.",
		},
		[]string{"service_id", "reused"},
	)

	// backendDialSeconds captures pure TCP dial time (ConnectStart ->
	// ConnectDone). Only fires when a fresh conn is created; reused conns
	// don't trigger this hook. Combined with http_pool_wait_seconds this
	// decomposes the "waiting for a conn" latency into dial-vs-queue.
	backendDialSeconds = observability.RelayerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "backend_dial_seconds",
			Help:      "Time spent establishing a fresh TCP connection to the upstream backend (from httptrace ConnectStart to ConnectDone). Reused connections are not observed here.",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"service_id"},
	)

	// inboundRequestsByProto tracks the wire protocol every inbound request
	// arrives on. Today PATH ships over HTTP/1.1 exclusively, so http1 will
	// be ~100% and h2c will be 0. The metric is here to surface any shift
	// early — if h2c starts ticking, we know the MaxConcurrentStreams=250
	// ceiling per-conn becomes relevant and inflight analysis has to
	// account for multi-stream connections.
	inboundRequestsByProto = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inbound_requests_by_proto_total",
			Help:      "Inbound relay requests by wire protocol (http1|h2c). Tracks any migration to HTTP/2 cleartext so per-conn stream caps can be reasoned about.",
		},
		[]string{"proto"},
	)

	// Streaming metrics
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

	// difficultyQueryFailures counts failures to resolve a service's mining
	// difficulty. The path FAILS OPEN (the relay is treated as applicable), so
	// without this counter a broken difficulty query silently mines every
	// relay against the wrong target with no visible signal.
	difficultyQueryFailures = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "difficulty_query_failures_total",
			Help:      "Failures to query a service's relay mining difficulty (fails open: relay treated as applicable)",
		},
		[]string{"service_id"},
	)

	// Mining difficulty metrics
	relaysSkippedDifficulty = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_skipped_difficulty_total",
			Help:      "Total number of relays skipped due to not meeting mining difficulty",
		},
		[]string{"service_id", "rpc_type"},
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

	// wsClosesTotal is what separates "clients we refused" from "the backend is
	// down" during an incident. Without it the only WebSocket counters are
	// active/total/forwarded, so a flood of refused connections and a
	// dead backend produce the same shape: connections_total climbing and
	// connections_active flat.
	//
	// Both labels are bounded: close_code goes through closeCodeName, which has
	// an Unknown default so a peer-supplied code cannot invent a series, and
	// initiated_by goes through closeInitiatorForSource, which maps onto the
	// three declared wsCloseInitiator constants.
	wsClosesTotal = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "websocket_closes_total",
			Help:      "Total number of WebSocket bridge closures by close code and initiator",
		},
		[]string{"service_id", "close_code", "initiated_by"},
	)

	// gRPC-Web metrics. gRPC relays themselves count in the same series as every
	// other transport, under rpc_type="grpc".
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

	relayMeterErrors = observability.RelayerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_meter_errors_total",
			Help:      "Total relay meter errors, whether the meter's own store or a chain query it depends on",
		},
		[]string{"operation"},
	)
)

// registerWorkerQueueDepth publishes each subpool's waiting-task count, read at
// scrape time so it cannot go stale.
//
// GaugeFunc and not a value written from the submit path: a queue drains when
// tasks COMPLETE, and nothing on the submit path runs then, so a gauge updated
// only on submit would report the depth at the last submission rather than now
// -- worst exactly when submissions stop because everything is stuck.
//
// AlreadyRegisteredError is tolerated because a process may build more than one
// proxy (tests do). The first registration wins and keeps reading a live pool;
// failing here would turn an observability detail into a startup error.
func registerWorkerQueueDepth(subpools map[string]pond.Pool) error {
	for name, sp := range subpools {
		sp := sp
		g := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace:   metricsNamespace,
			Subsystem:   metricsSubsystem,
			Name:        "worker_queue_depth",
			Help:        "Tasks waiting in a worker subpool right now (the queues are unbounded)",
			ConstLabels: prometheus.Labels{"subpool": name},
		}, func() float64 {
			return float64(sp.WaitingTasks())
		})
		if err := observability.RelayerRegistry.Register(g); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				return fmt.Errorf("registering worker_queue_depth for subpool %q: %w", name, err)
			}
		}
	}
	return nil
}

// SetSigningKeysLoaded publishes how many supplier signing keys are loaded.
//
// Exported because the key manager is wired in package cmd, which is also the
// only place that learns of a reload.
func SetSigningKeysLoaded(n int) {
	signingKeysLoaded.Set(float64(n))
}
