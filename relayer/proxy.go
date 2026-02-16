package relayer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alitto/pond/v2"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"github.com/puzpuzpuz/xsync/v4"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// httpStreamingTypes contains Content-Type values that indicate streaming responses.
// These are used to detect when a backend response should be streamed to the client
// rather than buffered entirely.
var httpStreamingTypes = []string{
	"text/event-stream",    // Server-Sent Events (SSE)
	"application/x-ndjson", // Newline-Delimited JSON (common for LLM APIs)
}

// Pocket context headers sent to backends.
// These headers provide the backend with information about the relay context.
const (
	// HeaderPocketSupplier is the supplier operator address processing the relay.
	HeaderPocketSupplier = "Pocket-Supplier"
	// HeaderPocketService is the service ID for the relay.
	HeaderPocketService = "Pocket-Service"
	// HeaderPocketApplication is the application address that signed the relay.
	HeaderPocketApplication = "Pocket-Application"

	// Metric label constants
	metricLabelUnknown = "unknown"

	// HTTP Server configuration constants
	// MaxConcurrentStreams is the maximum concurrent HTTP/2 streams per connection.
	MaxConcurrentStreams = 250
	// ReadTimeoutBuffer is added to the max service timeout for request parsing overhead.
	ReadTimeoutBuffer = 5 * time.Second
	// DefaultIdleTimeout is how long to keep idle keep-alive connections open.
	DefaultIdleTimeout = 120 * time.Second
	// GracefulShutdownTimeout is the timeout for graceful server shutdown.
	GracefulShutdownTimeout = 30 * time.Second
	// MaxStreamScanTokenSize is the maximum size of a single chunk when scanning
	// streaming responses (256KB to handle large LLM response chunks).
	MaxStreamScanTokenSize = 256 * 1024

	// Rejection reasons (for relaysRejected metric)
	rejectReasonReadBodyError               = "read_body_error"
	rejectReasonBodyTooLarge                = "body_too_large"
	rejectReasonInvalidRelayRequest         = "invalid_relay_request"
	rejectReasonMissingServiceID            = "missing_service_id"
	rejectReasonNilRelayRequest             = "nil_relay_request"
	rejectReasonResponseSignerNotConfigured = "response_signer_not_configured"
	rejectReasonSupplierCacheNotConfigured  = "supplier_cache_not_configured"
	rejectReasonUnknownService              = "unknown_service"
	rejectReasonMissingSupplierAddress      = "missing_supplier_address"
	rejectReasonSupplierCacheError          = "supplier_cache_error"
	rejectReasonSupplierNotFound            = "supplier_not_found"
	rejectReasonSupplierInactive            = "supplier_inactive"
	rejectReasonNoServices                  = "no_services"
	rejectReasonWrongService                = "wrong_service"
	rejectReasonBackendUnhealthy            = "backend_unhealthy"
	rejectReasonMeterError                  = "meter_error"
	rejectReasonStakeExhausted              = "stake_exhausted"
	rejectReasonValidationFailed            = "validation_failed"
	rejectReasonClientDisconnected          = "client_disconnected"
	rejectReasonBackendTimeout              = "backend_timeout"
	rejectReasonBackendNetworkError         = "backend_network_error"
	rejectReasonBackend5xx                  = "backend_5xx"
	rejectReasonSigningError                = "signing_error"

	// Drop reasons (for relaysDropped metric)
	dropReasonValidationFailed = "validation_failed"
	dropReasonMeterError       = "meter_error"
	dropReasonStakeExhausted   = "stake_exhausted"
)

// gzipMinCompressSize is the minimum response size worth compressing.
// Below this threshold, gzip overhead (header/trailer/dictionary) makes the
// output larger than the input. Typical signed relay responses for simple
// JSON-RPC calls (eth_blockNumber, etc.) are 500-800 bytes.
const gzipMinCompressSize = 1024

// gzipWriterPool is a pool of gzip.Writer instances to reduce allocations
// in the hot path when compressing relay responses.
var gzipWriterPool = sync.Pool{
	New: func() interface{} {
		return gzip.NewWriter(nil)
	},
}

// gzipBufPool is a pool of bytes.Buffer instances for gzip compression output.
var gzipBufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// publishTask holds the data needed for publishing a mined relay.
type publishTask struct {
	reqBody            []byte
	respBody           []byte
	arrivalBlockHeight int64
	serviceID          string
	supplierAddr       string
	sessionID          string
	applicationAddr    string
}

// ProxyServer handles incoming relay requests and forwards them to backends.
type ProxyServer struct {
	logger         logging.Logger
	config         *Config
	healthChecker  *HealthChecker
	publisher      transport.MinedRelayPublisher
	validator      RelayValidator
	relayProcessor RelayProcessor
	responseSigner *ResponseSigner
	supplierCache  *cache.SupplierCache
	relayMeter     *RelayMeter

	// HTTP client pool for backend requests (one client per timeout profile)
	// Key: timeout profile name (e.g., "fast", "streaming")
	// Value: *http.Client configured with that profile's timeouts
	clientPool   map[string]*http.Client
	clientPoolMu sync.RWMutex

	// Buffer pool for reading backend responses without blowing up RAM
	// Reuses buffers across requests to minimize GC pressure
	bufferPool *BufferPool

	// HTTP server
	server *http.Server

	// Parsed backend URLs (old - deprecated, kept for compatibility)
	backendURLs map[string]*url.URL

	// Thread-safe cache of pre-parsed backend URLs
	// Key: "serviceID:rpcType" (e.g., "develop:jsonrpc")
	// Value: *url.URL
	// Updated on config hot reload
	parsedBackendURLs *xsync.Map[string, *url.URL]

	// Current block height (from block subscriber)
	currentBlockHeight atomic.Int64

	// Worker pool for async operations
	workerPool pond.Pool

	// Subpools for specific tasks
	validationSubpool pond.Pool
	publishSubpool    pond.Pool
	metricsSubpool    pond.Pool

	// Global session monitor (shared across all WebSocket connections)
	sessionMonitor *SessionMonitor

	// Async metric recorder (avoids histogram lock contention in hot path)
	metricRecorder *MetricRecorder

	// Unified relay processing pipeline (validation + metering + signing + publishing)
	relayPipeline *RelayPipeline

	// gRPC relay service (proper relay protocol over gRPC)
	grpcRelayService *RelayGRPCService
	grpcRelayServer  *grpc.Server // gRPC server for the relay service
	grpcWebWrapper   *GRPCWebWrapper

	// Protects gRPC handler fields (grpcWebWrapper, grpcRelayService, grpcRelayServer)
	grpcMu sync.RWMutex

	// Lifecycle
	mu       sync.Mutex
	started  bool
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewProxyServer creates a new HTTP proxy server.
func NewProxyServer(
	logger logging.Logger,
	config *Config,
	healthChecker *HealthChecker,
	publisher transport.MinedRelayPublisher,
	workerPool pond.Pool,
) (*ProxyServer, error) {
	// Parse backend URLs - use the first available backend for each service
	backendURLs := make(map[string]*url.URL)
	for id, svc := range config.Services {
		// Find the first available backend (prefer "rest" if available)
		var backendURL string
		if backend, ok := svc.Backends["rest"]; ok {
			backendURL = backend.URL
		} else {
			// Use the first backend found
			for _, backend := range svc.Backends {
				backendURL = backend.URL
				break
			}
		}
		if backendURL == "" {
			return nil, fmt.Errorf("no backend configured for service %s", id)
		}
		parsed, err := url.Parse(backendURL)
		if err != nil {
			return nil, fmt.Errorf("invalid backend URL for service %s: %w", id, err)
		}
		backendURLs[id] = parsed
	}

	// Build HTTP client pool (one client per timeout profile)
	clientPool := buildClientPool(config, &config.HTTPTransport)

	// Initialize thread-safe parsed URL cache
	parsedURLCache := xsync.NewMap[string, *url.URL]()

	// Create subpools with dynamic worker allocation based on master pool size
	// This scales with available hardware
	// Note: Master pool is NumCPU * 8 for high concurrency
	masterPoolSize := workerPool.MaxConcurrency()
	validationWorkers := int(float64(masterPoolSize) * 0.7) // 70% for CPU-intensive ring signatures
	publishWorkers := int(float64(masterPoolSize) * 0.2)    // 20% for I/O-bound Redis writes
	metricsWorkers := int(float64(masterPoolSize) * 0.1)    // 10% for low-priority observability

	// Ensure at least 1 worker per subpool
	if validationWorkers < 1 {
		validationWorkers = 1
	}
	if publishWorkers < 1 {
		publishWorkers = 1
	}
	if metricsWorkers < 1 {
		metricsWorkers = 1
	}

	validationSubpool := workerPool.NewSubpool(validationWorkers)
	publishSubpool := workerPool.NewSubpool(publishWorkers)
	metricsSubpool := workerPool.NewSubpool(metricsWorkers)

	logger.Info().
		Int("validation_workers", validationWorkers).
		Int("publish_workers", publishWorkers).
		Int("metrics_workers", metricsWorkers).
		Int("master_pool_size", masterPoolSize).
		Msg("created worker subpools (8x CPU: 70% validation, 20% publish, 10% metrics)")

	// Initialize async metric recorder (avoids histogram lock contention in hot path)
	metricRecorder := NewMetricRecorder(logger, metricsSubpool)
	metricRecorder.Start()

	// Initialize buffer pool for reading backend responses
	// Find the maximum body size across all services to ensure we can handle any response
	maxBodySize := config.DefaultMaxBodySizeBytes
	for _, svc := range config.Services {
		if svc.MaxBodySizeBytes > maxBodySize {
			maxBodySize = svc.MaxBodySizeBytes
		}
	}
	if maxBodySize <= 0 {
		maxBodySize = DefaultMaxResponseSize // Fallback to 200MB if not configured
	}
	bufferPool := NewBufferPool(maxBodySize)

	logger.Info().
		Int64("max_body_size_bytes", maxBodySize).
		Int64("max_body_size_mb", maxBodySize/(1024*1024)).
		Msg("initialized buffer pool for backend response reading")

	proxy := &ProxyServer{
		logger:            logging.ForComponent(logger, logging.ComponentProxyServer),
		config:            config,
		healthChecker:     healthChecker,
		publisher:         publisher,
		backendURLs:       backendURLs,
		parsedBackendURLs: parsedURLCache,
		clientPool:        clientPool,
		bufferPool:        bufferPool,
		workerPool:        workerPool,
		validationSubpool: validationSubpool,
		publishSubpool:    publishSubpool,
		metricsSubpool:    metricsSubpool,
		metricRecorder:    metricRecorder,
	}

	return proxy, nil
}

// buildHTTPClient creates an optimized HTTP client with configured transport settings.
// Applies sensible defaults if values are not configured (zero values).
func buildHTTPClient(cfg *HTTPTransportConfig) *http.Client {
	// Apply defaults for zero values (5x increase for 1000+ RPS with connection reuse)
	maxIdleConns := cfg.MaxIdleConns
	if maxIdleConns == 0 {
		maxIdleConns = 500 // Support multiple backends and services
	}

	maxIdleConnsPerHost := cfg.MaxIdleConnsPerHost
	if maxIdleConnsPerHost == 0 {
		maxIdleConnsPerHost = 100 // Keep connections warm after bursts
	}

	maxConnsPerHost := cfg.MaxConnsPerHost
	if maxConnsPerHost == 0 {
		maxConnsPerHost = 500 // Handle p99 latency spikes and slow backends
	}

	idleConnTimeout := time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
	if idleConnTimeout == 0 {
		idleConnTimeout = 90 * time.Second
	}

	dialTimeout := time.Duration(cfg.DialTimeoutSeconds) * time.Second
	if dialTimeout == 0 {
		dialTimeout = 5 * time.Second
	}

	tlsHandshakeTimeout := time.Duration(cfg.TLSHandshakeTimeoutSeconds) * time.Second
	if tlsHandshakeTimeout == 0 {
		tlsHandshakeTimeout = 10 * time.Second
	}

	// ResponseHeaderTimeout: Respect 0 as "no timeout" for streaming services.
	// Defaults are applied in DefaultConfig and http_transport config.
	// Timeout profiles can explicitly set 0 to disable header timeout for long-running streams.
	responseHeaderTimeout := time.Duration(cfg.ResponseHeaderTimeoutSeconds) * time.Second

	expectContinueTimeout := time.Duration(cfg.ExpectContinueTimeoutSeconds) * time.Second
	if expectContinueTimeout == 0 {
		expectContinueTimeout = 1 * time.Second
	}

	tcpKeepAlive := time.Duration(cfg.TCPKeepAliveSeconds) * time.Second
	if tcpKeepAlive == 0 {
		tcpKeepAlive = 30 * time.Second
	}

	// Build custom dialer with optimized settings
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: tcpKeepAlive,
	}

	// Build transport with all optimizations
	transport := &http.Transport{
		// Connection pooling
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		MaxConnsPerHost:     maxConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,

		// Timeouts
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: expectContinueTimeout,

		// Custom dialer with keepalive
		DialContext: dialer.DialContext,

		// Don't modify content encoding for relay protocol
		DisableCompression: cfg.DisableCompression,

		// Force HTTP/2 for better multiplexing when available
		ForceAttemptHTTP2: true,
	}

	return &http.Client{
		Transport: transport,
		// Don't follow redirects - pass them through to the client
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// buildClientPool creates HTTP clients for each timeout profile.
// Each client is optimized for a specific use case (fast RPCs vs long-running streaming).
func buildClientPool(
	config *Config,
	transportConfig *HTTPTransportConfig,
) map[string]*http.Client {
	pool := make(map[string]*http.Client)

	for profileName, profile := range config.TimeoutProfiles {
		// Clone transport config and apply profile-specific timeouts
		profileTransport := *transportConfig
		profileTransport.ResponseHeaderTimeoutSeconds = profile.ResponseHeaderTimeoutSeconds
		profileTransport.DialTimeoutSeconds = profile.DialTimeoutSeconds
		profileTransport.TLSHandshakeTimeoutSeconds = profile.TLSHandshakeTimeoutSeconds

		pool[profileName] = buildHTTPClient(&profileTransport)
	}

	return pool
}

// getClientForService returns the appropriate HTTP client for a service.
// Uses the service's timeout_profile, falling back to "fast" profile.
func (p *ProxyServer) getClientForService(serviceID string) *http.Client {
	p.clientPoolMu.RLock()
	defer p.clientPoolMu.RUnlock()

	// Check if service has a custom timeout profile
	if svc, ok := p.config.Services[serviceID]; ok && svc.TimeoutProfile != "" {
		if client, ok := p.clientPool[svc.TimeoutProfile]; ok {
			return client
		}
		p.logger.Warn().
			Str("service_id", serviceID).
			Str("profile", svc.TimeoutProfile).
			Msg("timeout profile not found, using default 'fast'")
	}

	// Default to "fast" profile
	return p.clientPool["fast"]
}

// Start starts the HTTP proxy server.
func (p *ProxyServer) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("proxy server is closed")
	}
	if p.started {
		p.mu.Unlock()
		return fmt.Errorf("proxy server already started")
	}

	p.started = true
	ctx, p.cancelFn = context.WithCancel(ctx)
	p.mu.Unlock()

	// Initialize and start global session monitor for WebSocket connections
	p.sessionMonitor = NewSessionMonitor(
		p.logger,
		func() int64 { return p.currentBlockHeight.Load() },
		0, // No extra grace period - use on-chain params only
	)
	p.sessionMonitor.Start()

	// Start async metric recorder workers (avoids histogram lock contention in hot path)
	p.metricRecorder.Start()

	// Note: All async workers (validation, publish, metrics) are managed by pond subpools
	// No need to spawn worker goroutines manually - pond handles all concurrency

	// Create HTTP server with h2c (HTTP/2 cleartext) support for native gRPC
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handleRelay)

	// Configure HTTP/2 server for h2c (HTTP/2 without TLS)
	// This is required for native gRPC clients connecting without TLS
	h2s := &http2.Server{
		MaxConcurrentStreams: MaxConcurrentStreams,
	}

	// Wrap the handler with h2c to support both HTTP/1.1 and HTTP/2 cleartext
	h2cHandler := h2c.NewHandler(mux, h2s)

	// Wrap with panic recovery middleware to prevent handler panics from crashing the server
	handler := PanicRecoveryMiddleware(p.logger, h2cHandler)

	// Server timeout configuration:
	// - ReadTimeout: max timeout for reading entire request (including body)
	// - WriteTimeout: set to 0 - we use ResponseController for per-request write deadlines
	// - IdleTimeout: how long to keep idle keep-alive connections open
	//
	// Per-request write deadlines are controlled via http.ResponseController in handleRelay(),
	// allowing different timeouts per service (e.g., 30s for fast services, 600s for streaming).
	maxServiceTimeout := p.config.getMaxServiceTimeout()

	p.server = &http.Server{
		Addr:    p.config.ListenAddr,
		Handler: handler,
		// ReadTimeout: max service timeout + buffer for request parsing
		ReadTimeout: maxServiceTimeout + ReadTimeoutBuffer,
		// WriteTimeout: 0 (disabled) - we use ResponseController for per-request deadlines
		// This allows streaming services to have 600s while fast services have 30s
		WriteTimeout: 0,
		IdleTimeout:  DefaultIdleTimeout,
	}

	// Start server in goroutine
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.logger.Info().Str(logging.FieldListenAddr, p.config.ListenAddr).Msg("starting HTTP proxy server")

		if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			p.logger.Error().Err(err).Msg("HTTP server error")
		}
	}()

	// Wait for shutdown signal
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), GracefulShutdownTimeout)
		defer cancel()
		if err := p.server.Shutdown(shutdownCtx); err != nil {
			p.logger.Error().Err(err).Msg("error during server shutdown")
		}
	}()

	return nil
}

// handleRelay handles incoming relay requests.
func (p *ProxyServer) handleRelay(w http.ResponseWriter, r *http.Request) {
	// Health check endpoint - bypasses relay validation for load balancers
	if r.URL.Path == "/health" || r.URL.Path == "/healthz" || r.URL.Path == "/ready" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"healthy","block_height":%d}`, p.currentBlockHeight.Load())
		return
	}

	startTime := time.Now()
	activeConnections.Inc()
	defer activeConnections.Dec()

	// Check for WebSocket upgrade request
	if IsWebSocketUpgrade(r) {
		p.WebSocketHandler()(w, r)
		return
	}

	// Check for native gRPC request (HTTP/2 with application/grpc content type)
	// isGRPCRequest checks Content-Type for "application/grpc" prefix
	isGRPC := strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")

	// Get gRPC handlers (protected by mutex for safe concurrent access)
	p.grpcMu.RLock()
	grpcWebWrapper := p.grpcWebWrapper
	grpcRelayServer := p.grpcRelayServer
	p.grpcMu.RUnlock()

	// Check for gRPC-Web requests (HTTP/1.1 browser clients)
	if grpcWebWrapper != nil && grpcWebWrapper.IsGRPCWebRequest(r) {
		grpcWebWrapper.ServeHTTP(w, r)
		return
	}

	// Check for native gRPC requests (HTTP/2 with application/grpc content type)
	// Uses the new relay service that properly handles RelayRequest/RelayResponse protocol
	if isGRPC && grpcRelayServer != nil {
		grpcRelayServer.ServeHTTP(w, r)
		return
	}

	// Read request body first (we need it to extract service ID from relay request)
	maxBodySize := p.config.DefaultMaxBodySizeBytes

	// Read request body
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		p.sendError(w, http.StatusBadRequest, "failed to read request body")
		relaysReceived.WithLabelValues(metricLabelUnknown, metricLabelUnknown).Inc()
		relaysRejected.WithLabelValues(metricLabelUnknown, metricLabelUnknown, rejectReasonReadBodyError).Inc()
		return
	}

	if int64(len(body)) > maxBodySize {
		p.sendError(w, http.StatusRequestEntityTooLarge, "request body too large")
		relaysReceived.WithLabelValues(metricLabelUnknown, metricLabelUnknown).Inc()
		relaysRejected.WithLabelValues(metricLabelUnknown, metricLabelUnknown, rejectReasonBodyTooLarge).Inc()
		return
	}

	// Parse the relay request protobuf to extract service ID and payload
	// SECURITY: Only valid RelayRequest protobufs are accepted - raw HTTP requests are rejected
	relayRequest, serviceID, poktHTTPRequest, parseErr := p.parseRelayRequest(body)
	if parseErr != nil {
		// SECURITY FIX: Reject all non-relay traffic with proper error
		// This prevents unsigned/raw HTTP requests from being proxied
		p.logger.Debug().
			Err(parseErr).
			Msg("rejected request: not a valid RelayRequest protobuf")
		p.sendError(w, http.StatusBadRequest, "invalid relay request: body must be a valid RelayRequest protobuf")
		relaysReceived.WithLabelValues(metricLabelUnknown, metricLabelUnknown).Inc()
		relaysRejected.WithLabelValues(metricLabelUnknown, metricLabelUnknown, rejectReasonInvalidRelayRequest).Inc()
		return
	}
	if serviceID == "" {
		p.sendError(w, http.StatusBadRequest, "missing service ID in relay request")
		relaysReceived.WithLabelValues(metricLabelUnknown, metricLabelUnknown).Inc()
		relaysRejected.WithLabelValues(metricLabelUnknown, metricLabelUnknown, rejectReasonMissingServiceID).Inc()
		return
	}
	if relayRequest == nil {
		p.sendError(w, http.StatusBadRequest, "invalid relay request")
		relaysReceived.WithLabelValues(metricLabelUnknown, metricLabelUnknown).Inc()
		relaysRejected.WithLabelValues(metricLabelUnknown, metricLabelUnknown, rejectReasonNilRelayRequest).Inc()
		return
	}

	// Extract session context early for consistent logging throughout the request
	sessionCtx := logging.SessionContextFromRelayRequest(relayRequest)

	// Validate critical dependencies are configured - fail fast before any processing
	if p.responseSigner == nil {
		logging.WithSessionContext(p.logger.Error(), sessionCtx).
			Msg("response signer not configured")
		p.sendError(w, http.StatusInternalServerError, "relayer not properly configured")
		relaysReceived.WithLabelValues(serviceID, "unknown").Inc()
		relaysRejected.WithLabelValues(serviceID, metricLabelUnknown, rejectReasonResponseSignerNotConfigured).Inc()
		return
	}

	if p.supplierCache == nil {
		logging.WithSessionContext(p.logger.Error(), sessionCtx).
			Msg("supplier cache not configured")
		p.sendError(w, http.StatusInternalServerError, "relayer not properly configured")
		relaysReceived.WithLabelValues(serviceID, "unknown").Inc()
		relaysRejected.WithLabelValues(serviceID, metricLabelUnknown, rejectReasonSupplierCacheNotConfigured).Inc()
		return
	}

	// Check if service exists
	svcConfig, ok := p.config.Services[serviceID]
	if !ok {
		p.sendError(w, http.StatusNotFound, fmt.Sprintf("unknown service: %s", serviceID))
		relaysReceived.WithLabelValues(serviceID, "unknown").Inc()
		relaysRejected.WithLabelValues(serviceID, metricLabelUnknown, rejectReasonUnknownService).Inc()
		return
	}

	// Determine RPC type from header, with fallback to service default
	rpcType := r.Header.Get("Rpc-Type")
	if rpcType == "" {
		if svcConfig.DefaultBackend != "" {
			rpcType = svcConfig.DefaultBackend
		} else {
			rpcType = DefaultBackendType
		}
	}
	rpcType = RPCTypeToBackendType(rpcType)
	relaysReceived.WithLabelValues(serviceID, rpcType).Inc()

	// Set per-request write deadline using ResponseController.
	// This allows different timeouts per service (e.g., 30s fast vs 600s streaming).
	// The deadline is set based on the service's timeout profile.
	serviceTimeout := p.config.GetServiceTimeout(serviceID)
	rc := http.NewResponseController(w)
	// Add 30s buffer for response signing and network write
	if err = rc.SetWriteDeadline(time.Now().Add(serviceTimeout + 30*time.Second)); err != nil {
		logging.WithSessionContext(p.logger.Debug(), sessionCtx).
			Err(err).
			Dur("timeout", serviceTimeout).
			Msg("failed to set write deadline (non-fatal)")
		// Non-fatal: continue without per-request deadline
	}

	// Validate supplier operator address - REQUIRED in every valid RelayRequest
	supplierOperatorAddr := relayRequest.Meta.SupplierOperatorAddress
	if supplierOperatorAddr == "" {
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Msg("missing supplier operator address in relay request")
		p.sendError(w, http.StatusBadRequest, "missing supplier operator address in relay request")
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonMissingSupplierAddress).Inc()
		return
	}

	// Check supplier state against our registry
	supplierState, cacheErr := p.supplierCache.GetSupplierState(r.Context(), supplierOperatorAddr)
	if cacheErr != nil {
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Err(cacheErr).
			Msg("failed to check supplier state in cache")
		p.sendError(w, http.StatusServiceUnavailable, "failed to verify supplier state")
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonSupplierCacheError).Inc()
		return
	}
	if supplierState == nil {
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Msg("supplier not found in cache")
		p.sendError(w, http.StatusServiceUnavailable, fmt.Sprintf("supplier %s not registered with any miner", supplierOperatorAddr))
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonSupplierNotFound).Inc()
		return
	}
	if !supplierState.IsActive() {
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Str("status", supplierState.Status).
			Msg("supplier not active")
		p.sendError(w, http.StatusServiceUnavailable, fmt.Sprintf("supplier %s is %s", supplierOperatorAddr, supplierState.Status))
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonSupplierInactive).Inc()
		return
	}
	if len(supplierState.Services) == 0 {
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Msg("supplier has no services registered")
		p.sendError(w, http.StatusServiceUnavailable, fmt.Sprintf("supplier %s has no services registered", supplierOperatorAddr))
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonNoServices).Inc()
		return
	}
	if !supplierState.IsActiveForService(serviceID) {
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Int("num_services", len(supplierState.Services)).
			Msg("supplier not staked for service")
		p.sendError(w, http.StatusServiceUnavailable, fmt.Sprintf("supplier %s not staked for service %s", supplierOperatorAddr, serviceID))
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonWrongService).Inc()
		return
	}
	logging.WithSessionContext(p.logger.Debug(), sessionCtx).
		Msg("supplier is active for service")

	// Check backend health
	if !p.healthChecker.IsHealthy(serviceID) {
		// NOTE: this will return true always until is properly implemented.
		p.sendError(w, http.StatusServiceUnavailable, "backend unhealthy")
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonBackendUnhealthy).Inc()
		return
	}

	// Check service-specific body size limit
	serviceMaxBodySize := p.config.GetServiceMaxBodySize(serviceID)
	if int64(len(body)) > serviceMaxBodySize {
		p.sendError(w, http.StatusRequestEntityTooLarge, "request body too large for service")
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonBodyTooLarge).Inc()
		return
	}

	requestBodySize.WithLabelValues(serviceID, rpcType).Observe(float64(len(body)))

	// Pin block height at arrival time (for grace period calculation)
	arrivalBlockHeight := p.currentBlockHeight.Load()

	// Get validation mode
	validationMode := p.config.GetServiceValidationMode(serviceID)

	logging.WithSessionContext(p.logger.Debug(), sessionCtx).
		Str("validation_mode", string(validationMode)).
		Msg("relay received")

	// For eager validation, validate before forwarding
	if validationMode == ValidationModeEager {
		// EAGER MODE: Check meter BEFORE backend call (synchronous, blocks the hot path)
		if p.relayMeter != nil && relayRequest.Meta.SessionHeader != nil {
			sessionHeader := relayRequest.Meta.SessionHeader
			sessionID := sessionHeader.SessionId
			appAddress := sessionHeader.ApplicationAddress
			supplierAddress := relayRequest.Meta.SupplierOperatorAddress
			sessionEndHeight := sessionHeader.SessionEndBlockHeight

			meterStart := time.Now()
			allowed, meterErr := p.relayMeter.CheckAndConsumeRelay(
				r.Context(),
				sessionID,
				appAddress,
				serviceID,
				supplierAddress,
				sessionEndHeight,
			)
			meterDuration := time.Since(meterStart)

			// Record relay meter latency asynchronously
			p.metricRecorder.RecordDuration(relayMeterLatency, []string{serviceID, "eager"}, meterDuration)

			if meterErr != nil {
				logging.WithSessionContext(p.logger.Warn(), sessionCtx).
					Err(meterErr).
					Msg("relay meter error (eager mode)")
				if !allowed {
					p.sendError(w, http.StatusServiceUnavailable, "relay metering unavailable")
					relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonMeterError).Inc()
					return
				}
			} else if !allowed {
				logging.WithSessionContext(p.logger.Debug(), sessionCtx).
					Msg("relay rejected: app stake exhausted (eager mode)")
				p.sendError(w, http.StatusPaymentRequired, "application stake exhausted for session")
				relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonStakeExhausted).Inc()
				return
			}
		}

		eagerStart := time.Now()
		if validationErr := p.validateRelayRequest(r.Context(), r, body, arrivalBlockHeight); validationErr != nil {
			p.sendError(w, http.StatusForbidden, validationErr.Error())
			relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonValidationFailed).Inc()
			validationFailures.WithLabelValues(serviceID, "signature").Inc()
			return
		}
		eagerDuration := time.Since(eagerStart)

		// Record eager validation latency asynchronously
		p.metricRecorder.RecordDuration(validationLatency, []string{serviceID, "eager"}, eagerDuration)

		logging.WithSessionContext(p.logger.Debug(), sessionCtx).
			Dur("validation_duration", eagerDuration).
			Str("validation_mode", "eager").
			Msg("eager validation passed (before backend)")
	}

	// Forward request to backend (handles both streaming and non-streaming)
	// Use the parsed POKTHTTPRequest if available, otherwise fall back to raw body
	backendStart := time.Now()
	respBody, respHeaders, respStatus, isStreaming, err := p.forwardToBackendWithStreaming(r.Context(), r, body, serviceID, &svcConfig, rpcType, poktHTTPRequest, w, relayRequest)
	backendDuration := time.Since(backendStart)

	// Record backend latency asynchronously (no blocking on histogram locks)
	p.metricRecorder.RecordDuration(backendLatency, []string{serviceID}, backendDuration)

	if err != nil {
		// Only send error response if we haven't started streaming yet
		if !isStreaming {
			p.sendError(w, http.StatusBadGateway, "backend error")
		}

		// Determine specific rejection reason from error
		var rejectionReason string
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, rejectReasonClientDisconnected):
			rejectionReason = rejectReasonClientDisconnected
		case strings.Contains(errMsg, rejectReasonBackendTimeout):
			rejectionReason = rejectReasonBackendTimeout
		default:
			rejectionReason = rejectReasonBackendNetworkError
		}

		relaysRejected.WithLabelValues(serviceID, rpcType, rejectionReason).Inc()
		return
	}

	// Check for 5xx backend errors - these should NOT be wrapped, signed, or mined
	// 2xx-4xx are valid relays (client/backend logic errors that should be paid)
	// 5xx are infrastructure/backend failures (supplier should not be compensated)
	if respStatus >= http.StatusInternalServerError {
		// Return raw 5xx status to client (no wrapping in RelayResponse)
		p.sendError(w, respStatus, "backend service error")
		relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonBackend5xx).Inc()
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Int("status_code", respStatus).
			Msg("backend returned 5xx error - relay not mined")
		return
	}

	// For non-streaming responses, build and return signed RelayResponse
	if !isStreaming {
		responseBodySize.WithLabelValues(serviceID, rpcType).Observe(float64(len(respBody)))

		// Build and sign the RelayResponse
		// responseSigner is guaranteed to be non-nil (validated early in handleRelay)
		_, signedResponseBz, signErr := p.responseSigner.BuildAndSignRelayResponseFromBody(
			relayRequest,
			respBody,
			respHeaders,
			respStatus,
		)
		if signErr != nil {
			logging.WithSessionContext(p.logger.Error(), sessionCtx).
				Err(signErr).
				Msg("failed to sign relay response")
			p.sendError(w, http.StatusInternalServerError, "failed to sign response")
			relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonSigningError).Inc()
			return
		}

		// Send the signed RelayResponse protobuf
		// Respect Accept header for content type negotiation (RFC 7231)
		responseContentType := r.Header.Get("Accept")
		if responseContentType == "" || responseContentType == "*/*" {
			responseContentType = "application/json" // Default to what gateways typically expect
		}
		w.Header().Set("Content-Type", responseContentType)

		// RFC compliance: Compress response if client accepts gzip and payload is large enough.
		// Skip compression for small payloads where gzip overhead makes the output larger.
		responseData := signedResponseBz
		if clientAcceptsGzip(r) && len(signedResponseBz) >= gzipMinCompressSize {
			compressed, compressErr := compressGzip(signedResponseBz)
			if compressErr != nil {
				logging.WithSessionContext(p.logger.Warn(), sessionCtx).
					Err(compressErr).
					Msg("failed to gzip compress response, sending uncompressed")
			} else {
				responseData = compressed
				w.Header().Set("Content-Encoding", "gzip")
				logging.WithSessionContext(p.logger.Debug(), sessionCtx).
					Int("original_size", len(signedResponseBz)).
					Int("compressed_size", len(compressed)).
					Float64("compression_ratio", float64(len(compressed))/float64(len(signedResponseBz))).
					Msg("gzip compressed response for client")
			}
		}

		w.WriteHeader(http.StatusOK)

		// Measure response write time
		if _, err := w.Write(responseData); err != nil {
			p.logger.Debug().Err(err).Msg("failed to write signed response body")
		}

		logging.WithSessionContext(p.logger.Debug(), sessionCtx).
			Int("response_size", len(responseData)).
			Bool("compressed", len(responseData) != len(signedResponseBz)).
			Msg("sent signed relay response")
	}

	// ALWAYS increment relaysServed when we send a response to the client
	// Use actual backend status code (200, 400, etc.) for visibility into backend behavior
	// In optimistic mode, some served relays may later be dropped (not mined)
	// Drop rate = relaysDropped / relaysServed
	relaysServed.WithLabelValues(serviceID, rpcType, fmt.Sprintf("%d", respStatus)).Inc()
	totalRelayDuration := time.Since(startTime)

	// Record total relay latency asynchronously (no blocking on histogram locks)
	p.metricRecorder.RecordDuration(relayLatency, []string{serviceID, rpcType}, totalRelayDuration)

	logging.WithSessionContext(p.logger.Debug(), sessionCtx).
		Dur("total_relay_duration", totalRelayDuration).
		Str("validation_mode", string(validationMode)).
		Bool("is_streaming", isStreaming).
		Msg("relay served")

	// Track streaming metrics
	if isStreaming {
		streamingRelaysServed.WithLabelValues(serviceID).Inc()
	}

	// For optimistic validation, validate after serving (in background using pond subpool)
	if validationMode == ValidationModeOptimistic {
		// Capture variables for closure (avoid race conditions)
		capturedRequest := relayRequest
		capturedHTTPReq := r
		capturedReqBody := make([]byte, len(body))
		copy(capturedReqBody, body)
		capturedRespBody := make([]byte, len(respBody))
		copy(capturedRespBody, respBody)
		capturedBlockHeight := arrivalBlockHeight
		capturedServiceID := serviceID
		capturedSessionCtx := sessionCtx

		// Submit to validation subpool (non-blocking, unbounded queue)
		p.validationSubpool.Submit(func() {
			logging.WithSessionContext(p.logger.Debug(), capturedSessionCtx).
				Str("validation_mode", "optimistic").
				Msg("starting optimistic validation (background)")

			// Extract app address for metrics (need it before validation check)
			var appAddress string
			if capturedRequest != nil && capturedRequest.Meta.SessionHeader != nil {
				appAddress = capturedRequest.Meta.SessionHeader.ApplicationAddress
			}
			if appAddress == "" {
				appAddress = metricLabelUnknown
			}

			// ONLY measure validation time, NOT meter or miner submit
			optimisticStart := time.Now()
			if err := p.validateRelayRequest(context.Background(), capturedHTTPReq, capturedReqBody, capturedBlockHeight); err != nil {
				validationFailures.WithLabelValues(capturedServiceID, "signature").Inc()
				relaysDropped.WithLabelValues(capturedServiceID, appAddress, dropReasonValidationFailed).Inc()
				logging.WithSessionContext(p.logger.Debug(), capturedSessionCtx).
					Err(err).
					Str("validation_mode", "optimistic").
					Msg("optimistic validation failed - relay dropped")
				return
			}
			optimisticDuration := time.Since(optimisticStart)

			// Record optimistic validation latency asynchronously (ONLY validation, not meter/miner)
			p.metricRecorder.RecordDuration(validationLatency, []string{capturedServiceID, "optimistic"}, optimisticDuration)

			logging.WithSessionContext(p.logger.Debug(), capturedSessionCtx).
				Dur("validation_duration", optimisticDuration).
				Str("validation_mode", "optimistic").
				Msg("optimistic validation passed (after serving)")

			// OPTIMISTIC MODE: Check meter AFTER serving (asynchronous, no hot path blocking)
			// User requirement: "if not valid or exhausted, metric and discard it; otherwise delivery to miner"
			if p.relayMeter != nil && capturedRequest != nil && capturedRequest.Meta.SessionHeader != nil {
				sessionHeader := capturedRequest.Meta.SessionHeader
				sessionID := sessionHeader.SessionId
				supplierAddress := capturedRequest.Meta.SupplierOperatorAddress
				sessionEndHeight := sessionHeader.SessionEndBlockHeight

				meterStart := time.Now()
				allowed, meterErr := p.relayMeter.CheckAndConsumeRelay(
					context.Background(),
					sessionID,
					appAddress,
					capturedServiceID,
					supplierAddress,
					sessionEndHeight,
				)
				meterDuration := time.Since(meterStart)

				// Record relay meter latency asynchronously
				p.metricRecorder.RecordDuration(relayMeterLatency, []string{capturedServiceID, "optimistic"}, meterDuration)

				if meterErr != nil {
					relaysDropped.WithLabelValues(capturedServiceID, appAddress, dropReasonMeterError).Inc()
					logging.WithSessionContext(p.logger.Warn(), capturedSessionCtx).
						Err(meterErr).
						Str("validation_mode", "optimistic").
						Msg("relay meter error (optimistic mode) - relay dropped")
					// Meter error in optimistic mode - discard, don't submit to miner
					if !allowed {
						return
					}
				} else if !allowed {
					relaysDropped.WithLabelValues(capturedServiceID, appAddress, dropReasonStakeExhausted).Inc()
					logging.WithSessionContext(p.logger.Debug(), capturedSessionCtx).
						Str("validation_mode", "optimistic").
						Msg("relay rejected: app stake exhausted (optimistic mode) - relay dropped")
					// Stake exhausted - discard, don't submit to miner
					// Note: We already served the response, but we won't mine it
					return
				}
			}

			// Submit publish task to worker pool (after successful validation AND metering)
			// Only publish relays that are within stake limits
			// Note: relaysServed already incremented when we sent response
			p.submitPublishTask(capturedRequest, capturedHTTPReq, capturedReqBody, capturedRespBody, capturedBlockHeight, capturedServiceID)
		})
	} else {
		// For eager validation, submit publish task to worker pool
		// If we reached here, the relay was allowed by the meter (stake not exhausted)
		p.submitPublishTask(relayRequest, r, body, respBody, arrivalBlockHeight, serviceID)
	}
}

// submitPublishTask submits a relay for publication via the worker pool.
// This is non-blocking and uses the server context, not the request context.
func (p *ProxyServer) submitPublishTask(
	relayRequest *servicetypes.RelayRequest,
	r *http.Request,
	reqBody, respBody []byte,
	arrivalBlockHeight int64,
	serviceID string,
) {
	// Get supplier address from relay request if available
	var supplierAddr string
	var sessionID string
	var applicationAddr string

	if relayRequest != nil {
		supplierAddr = relayRequest.Meta.SupplierOperatorAddress
		if relayRequest.Meta.SessionHeader != nil {
			sessionID = relayRequest.Meta.SessionHeader.SessionId
			applicationAddr = relayRequest.Meta.SessionHeader.ApplicationAddress
		}
	}

	// SECURITY: All values MUST come from the signed RelayRequest.
	// Never read these from HTTP headers as they can be spoofed.
	// The RelayRequest is cryptographically signed by the application/gateway.

	if supplierAddr == "" {
		// Create minimal session context from what we have
		sessionCtx := logging.SessionContextPartial("", serviceID, "", "", 0)
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Msg("no supplier address available, skipping relay publication")
		return
	}

	// If we don't have session metadata from the RelayRequest, skip publishing
	// This is a security requirement - we cannot trust header values
	if sessionID == "" || applicationAddr == "" {
		// Create minimal session context from what we have
		sessionCtx := logging.SessionContextPartial(sessionID, serviceID, supplierAddr, applicationAddr, 0)
		logging.WithSessionContext(p.logger.Debug(), sessionCtx).
			Bool("has_session_id", sessionID != "").
			Bool("has_app_addr", applicationAddr != "").
			Msg("missing session metadata from RelayRequest, skipping relay publication")
		return
	}

	// Submit publish task to pond worker pool (non-blocking, unbounded queue)
	// Uses context.Background() since publish should complete even if request context is cancelled
	p.publishSubpool.Submit(func() {
		task := publishTask{
			reqBody:            reqBody,
			respBody:           respBody,
			arrivalBlockHeight: arrivalBlockHeight,
			serviceID:          serviceID,
			supplierAddr:       supplierAddr,
			sessionID:          sessionID,
			applicationAddr:    applicationAddr,
		}
		p.executePublish(context.Background(), task)
	})
}

// parseRelayRequest parses the relay request protobuf body and extracts the service ID
// and the POKTHTTPRequest payload. Returns nil values if the body is not a valid relay request.
func (p *ProxyServer) parseRelayRequest(body []byte) (*servicetypes.RelayRequest, string, *sdktypes.POKTHTTPRequest, error) {
	if len(body) == 0 {
		return nil, "", nil, fmt.Errorf("empty body")
	}

	// Try to unmarshal as a RelayRequest protobuf
	relayRequest := &servicetypes.RelayRequest{}
	if err := relayRequest.Unmarshal(body); err != nil {
		// Not a valid relay request - this is expected for non-relay traffic
		p.logger.Debug().
			Err(err).
			Msg("request body is not a valid RelayRequest protobuf")
		return nil, "", nil, err
	}

	// Extract service ID from the session header
	var serviceID string
	if relayRequest.Meta.SessionHeader != nil {
		serviceID = relayRequest.Meta.SessionHeader.ServiceId
	}

	if serviceID == "" {
		return relayRequest, "", nil, fmt.Errorf("missing service ID in relay request")
	}

	// Create session context for logging (partial since we just parsed the request)
	sessionCtx := logging.SessionContextFromRelayRequest(relayRequest)
	logging.WithSessionContext(p.logger.Debug(), sessionCtx).
		Msg("extracted service ID from relay request")

	// Deserialize the POKTHTTPRequest from the payload
	poktHTTPRequest, err := sdktypes.DeserializeHTTPRequest(relayRequest.Payload)
	if err != nil {
		logging.WithSessionContext(p.logger.Debug(), sessionCtx).
			Err(err).
			Msg("failed to deserialize POKTHTTPRequest from payload")
		return relayRequest, serviceID, nil, err
	}

	logging.WithSessionContext(p.logger.Debug(), sessionCtx).
		Str("method", poktHTTPRequest.Method).
		Str("url", poktHTTPRequest.Url).
		Msg("deserialized POKTHTTPRequest from relay payload")

	return relayRequest, serviceID, poktHTTPRequest, nil
}

// extractServiceID extracts the service ID from request headers or path.
// This is a fallback method for non-relay traffic or when the body cannot be parsed.
func (p *ProxyServer) extractServiceID(r *http.Request) string {
	// Try Target-Service-Id header (PATH gateway uses this)
	if serviceID := r.Header.Get("Target-Service-Id"); serviceID != "" {
		return serviceID
	}

	// Try Pocket-Service-Id header (legacy/alternative)
	if serviceID := r.Header.Get("Pocket-Service-Id"); serviceID != "" {
		return serviceID
	}

	// Try X-Forwarded-Host header (for path-based routing)
	if host := r.Header.Get("X-Forwarded-Host"); host != "" {
		// Could parse host to extract service ID
		return host
	}

	// Try path-based extraction (e.g., /v1/ethereum/...)
	// This is a simplified version - real implementation would be more robust
	if len(r.URL.Path) > 1 {
		// Extract first path segment
		path := r.URL.Path[1:] // Remove leading /
		for i, c := range path {
			if c == '/' {
				return path[:i]
			}
		}
		return path
	}

	return ""
}

// forwardToBackendWithStreaming forwards the request to the backend service,
// handling both streaming and non-streaming responses.
// Returns the response body, headers, status, whether it was streaming, and any error.
// For streaming responses, the body is written directly to the ResponseWriter with proper signing.
// If poktHTTPRequest is provided (valid relay request), it uses the deserialized request data.
// Otherwise, it falls back to forwarding the raw body (for non-relay traffic).
//
// Streaming Support (SSE/NDJSON for LLM APIs):
// When the backend returns a streaming response (text/event-stream or application/x-ndjson),
// the response is handled with batch-based signing:
// - Chunks are accumulated into batches based on time (100ms), size (100KB), or count (100) thresholds
// - Each batch is signed as a RelayResponse and sent with the ||POKT_STREAM|| delimiter
// - This enables proper relay protocol compliance while maintaining low-latency streaming
func (p *ProxyServer) forwardToBackendWithStreaming(
	ctx context.Context,
	originalReq *http.Request,
	body []byte,
	serviceID string,
	svcConfig *ServiceConfig,
	rpcType string,
	poktHTTPRequest *sdktypes.POKTHTTPRequest,
	w http.ResponseWriter,
	relayRequest *servicetypes.RelayRequest,
) ([]byte, http.Header, int, bool, error) {
	// Create session context once for all logging in this function
	var sessionCtx *logging.SessionContext
	if relayRequest != nil {
		sessionCtx = logging.SessionContextFromRelayRequest(relayRequest)
	} else {
		sessionCtx = &logging.SessionContext{ServiceID: serviceID}
	}

	// Find the backend configuration
	var backendURL string
	var configHeaders map[string]string
	var auth *AuthenticationConfig

	if backend, ok := svcConfig.Backends[rpcType]; ok {
		backendURL = backend.URL
		configHeaders = backend.Headers
		auth = backend.Authentication
	} else {
		// Fallback chain if requested type not found
		// Try: configured default → DefaultBackendType → rest → any available
		fallbackTypes := make([]string, 0)

		if svcConfig.DefaultBackend != "" && svcConfig.DefaultBackend != rpcType {
			fallbackTypes = append(fallbackTypes, svcConfig.DefaultBackend)
		}
		if rpcType != DefaultBackendType {
			fallbackTypes = append(fallbackTypes, DefaultBackendType)
		}
		if rpcType != BackendTypeREST {
			fallbackTypes = append(fallbackTypes, BackendTypeREST)
		}

		// Try fallback types
		for _, tryType := range fallbackTypes {
			if backend, ok := svcConfig.Backends[tryType]; ok {
				backendURL = backend.URL
				configHeaders = backend.Headers
				auth = backend.Authentication
				logging.WithSessionContext(p.logger.Debug(), sessionCtx).
					Str("requested_type", rpcType).
					Str("fallback_type", tryType).
					Msg("using fallback backend type")
				break
			}
		}
	}

	if backendURL == "" {
		return nil, nil, 0, false, fmt.Errorf("no backend configured for service %s and RPC type %s", serviceID, rpcType)
	}

	// Create backend request
	timeout := p.config.GetServiceTimeout(serviceID)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Get parsed backend URL from cache or parse and cache it
	// Cache key: "serviceID:rpcType"
	cacheKey := serviceID + ":" + rpcType
	parsedBackendURL, _ := p.parsedBackendURLs.LoadOrCompute(cacheKey, func() (*url.URL, bool) {
		parsed, parseErr := url.Parse(backendURL)
		if parseErr != nil {
			// Return nil on error - will be caught below
			return nil, false
		}
		return parsed, false
	})

	if parsedBackendURL == nil {
		return nil, nil, 0, false, fmt.Errorf("failed to parse backend URL: %s", backendURL)
	}

	var req *http.Request
	var err error

	// If we have a valid POKTHTTPRequest from the relay payload, use it to build the backend request
	if poktHTTPRequest != nil {
		// Start with the backend URL (which is absolute) and copy it
		requestURL := *parsedBackendURL

		// Parse the request URL from POKTHTTPRequest to extract path and query
		var poktURL *url.URL
		poktURL, err = url.Parse(poktHTTPRequest.Url)
		if err != nil {
			return nil, nil, 0, false, fmt.Errorf("failed to parse request URL: %w", err)
		}

		// Update the path by merging backend path with POKT request path
		if poktURL.Path != "" {
			if parsedBackendURL.Path != "" && parsedBackendURL.Path != "/" {
				requestURL.Path = strings.TrimSuffix(parsedBackendURL.Path, "/") + poktURL.Path
			} else {
				requestURL.Path = poktURL.Path
			}
		}

		// Merge query parameters from both backend URL and POKT request
		query := requestURL.Query()
		for key, values := range poktURL.Query() {
			for _, value := range values {
				query.Add(key, value)
			}
		}
		requestURL.RawQuery = query.Encode()

		// Create the HTTP request with the payload body
		req, err = http.NewRequestWithContext(ctx, poktHTTPRequest.Method, requestURL.String(), bytes.NewReader(poktHTTPRequest.BodyBz))
		if err != nil {
			return nil, nil, 0, false, fmt.Errorf("failed to create request: %w", err)
		}

		// Copy headers from POKTHTTPRequest
		poktHTTPRequest.CopyToHTTPHeader(req.Header)

		// Also copy headers from wrapper request (e.g., Pocket-* headers)
		p.copyHeaders(req, originalReq)

		logging.WithSessionContext(p.logger.Debug(), sessionCtx).
			Str("method", poktHTTPRequest.Method).
			Str("url", requestURL.String()).
			Int("body_size", len(poktHTTPRequest.BodyBz)).
			Msg("built backend request from POKTHTTPRequest")
	} else {
		// Fallback: forward the raw body for non-relay traffic
		fullBackendURL := backendURL
		if originalReq.URL.Path != "" && originalReq.URL.Path != "/" {
			if parsedBackendURL.Path == "" || parsedBackendURL.Path == "/" {
				parsedBackendURL.Path = originalReq.URL.Path
			} else {
				parsedBackendURL.Path = strings.TrimSuffix(parsedBackendURL.Path, "/") + originalReq.URL.Path
			}
			fullBackendURL = parsedBackendURL.String()
		}

		req, err = http.NewRequestWithContext(ctx, originalReq.Method, fullBackendURL, bytes.NewReader(body))
		if err != nil {
			return nil, nil, 0, false, fmt.Errorf("failed to create request: %w", err)
		}

		// Copy relevant headers from original request
		p.copyHeaders(req, originalReq)
	}

	// Apply service-specific configuration headers (override any matching headers)
	for key, value := range configHeaders {
		req.Header.Set(key, value)
	}

	// Apply authentication
	if auth != nil {
		if auth.Username != "" && auth.Password != "" {
			req.SetBasicAuth(auth.Username, auth.Password)
		} else if auth.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+auth.BearerToken)
		} else if auth.PlainToken != "" {
			req.Header.Set("Authorization", auth.PlainToken)
		}
	}

	// Explicitly prevent compression from backend
	// We'll compress the final RelayResponse ourselves if the client supports it
	// Using "identity" tells the backend: send uncompressed data
	req.Header.Set("Accept-Encoding", "identity")

	// Set Pocket context headers for backend visibility
	// Use supplier address from relay request if available, fall back to proxy's configured address
	var supplierAddress string
	var applicationAddress string
	if relayRequest != nil {
		meta := relayRequest.GetMeta()
		supplierAddress = meta.GetSupplierOperatorAddress()
		if sessionHeader := meta.GetSessionHeader(); sessionHeader != nil {
			applicationAddress = sessionHeader.GetApplicationAddress()
		}
	}
	req.Header.Set(HeaderPocketSupplier, supplierAddress)
	req.Header.Set(HeaderPocketService, serviceID)
	if applicationAddress != "" {
		req.Header.Set(HeaderPocketApplication, applicationAddress)
	}

	// Execute backend request using service-specific HTTP client
	client := p.getClientForService(serviceID)
	resp, err := client.Do(req)

	if err != nil {
		// Distinguish between client disconnection vs internal timeout vs other errors
		// for proper metrics and logging
		if originalReq.Context().Err() != nil {
			// Client disconnected - their context was cancelled
			return nil, nil, 0, false, fmt.Errorf("%s: %w", rejectReasonClientDisconnected, originalReq.Context().Err())
		}
		if ctx.Err() != nil {
			// Our internal timeout fired
			return nil, nil, 0, false, fmt.Errorf("%s (service=%s, timeout=%v): %w", rejectReasonBackendTimeout, serviceID, timeout, ctx.Err())
		}
		// Other network/backend error
		return nil, nil, 0, false, fmt.Errorf("backend request failed: %w", err)
	}

	isStreaming := isStreamingResponse(resp)

	// Non-streaming: read entire response
	defer func() {
		if isStreaming {
			_ = resp.Body.Close()
		}
	}()

	// Check if this is a streaming response
	if isStreaming {
		// Use the new streaming handler with proper batch-based signing when we have a relay request
		if relayRequest != nil && p.responseSigner != nil {
			logging.WithSessionContext(p.logger.Debug(), sessionCtx).
				Msg("handling streaming response with batch-based signing (SSE/NDJSON)")
			respBody, streamErr := p.handleStreamingResponseWithSigning(ctx, resp, w, relayRequest, serviceID, rpcType)
			return respBody, resp.Header, resp.StatusCode, true, streamErr
		}

		// Fallback: forward raw stream without signing (backward compatibility / testing)
		logging.WithSessionContext(p.logger.Debug(), sessionCtx).
			Msg("handling streaming response without signing (no relay request or signer)")
		respBody, streamErr := p.handleStreamingResponse(resp, w)
		return respBody, resp.Header, resp.StatusCode, true, streamErr
	}

	// Read response body using buffer pool to avoid RAM exhaustion
	// Handles responses from 10KB to 200MB+ without allocating unbounded memory
	respBody, err := p.bufferPool.ReadWithBuffer(resp.Body)
	if err != nil {
		return nil, nil, 0, false, fmt.Errorf("failed to read response: %w", err)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		p.logger.Warn().Err(closeErr).Msg("failed to close response body")
	}

	return respBody, resp.Header, resp.StatusCode, false, nil
}

// isStreamingResponse checks if the HTTP response should be handled as a stream.
// Detects SSE (text/event-stream) and NDJSON (application/x-ndjson) content types.
func isStreamingResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		return false
	}

	// Parse media type to strip parameters (e.g., "; charset=utf-8")
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}

	return slices.Contains(httpStreamingTypes, strings.ToLower(mediaType))
}

// handleStreamingResponse handles streaming responses (SSE, NDJSON).
// It forwards chunks in real-time to the client while collecting the full body
// for relay publishing.
func (p *ProxyServer) handleStreamingResponse(
	resp *http.Response,
	w http.ResponseWriter,
) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()

	// Copy headers to response
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	// Set connection close to prevent client reuse issues with streaming
	w.Header().Set("Connection", "close")
	w.WriteHeader(resp.StatusCode)

	// Check if writer supports flushing (optional but recommended for streaming)
	flusher, canFlush := w.(http.Flusher)

	// Buffer to collect full response for relay publishing
	var fullResponse bytes.Buffer

	// Stream chunks to client
	scanner := bufio.NewScanner(resp.Body)

	// Increase buffer size for large chunks (LLM responses can be large)
	buf := make([]byte, MaxStreamScanTokenSize)
	scanner.Buffer(buf, MaxStreamScanTokenSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		lineWithNewline := append(line, '\n')

		// Collect for full response
		fullResponse.Write(lineWithNewline)

		// Forward to client
		if _, err := w.Write(lineWithNewline); err != nil {
			return fullResponse.Bytes(), fmt.Errorf("failed to write stream chunk: %w", err)
		}

		// Flush immediately for low latency if supported
		if canFlush {
			flusher.Flush()
		}

		// Track streaming metrics
		streamingChunksForwarded.Inc()
	}

	if err := scanner.Err(); err != nil {
		return fullResponse.Bytes(), fmt.Errorf("stream scanning error: %w", err)
	}

	streamingBytesForwarded.Add(float64(fullResponse.Len()))
	return fullResponse.Bytes(), nil
}

// copyHeaders copies relevant headers from original request to backend request.
func (p *ProxyServer) copyHeaders(dst, src *http.Request) {
	// Headers to copy
	headersToCopy := []string{
		"Content-Type",
		"Accept",
		"Accept-Encoding",
		"User-Agent",
	}

	for _, header := range headersToCopy {
		if value := src.Header.Get(header); value != "" {
			dst.Header.Set(header, value)
		}
	}

	// Copy Pocket-* headers if forward_pocket_headers is enabled
	// (This would be checked per-service in real implementation)
	// HTTP headers in Go are canonicalized, but we use case-insensitive matching
	// to handle any edge cases with header casing from different clients.
	for key := range src.Header {
		if strings.HasPrefix(strings.ToLower(key), "pocket-") {
			dst.Header.Set(key, src.Header.Get(key))
		}
	}
}

// SetValidator sets the relay validator for the proxy server.
// This is optional - if not set, validation is skipped (useful for testing).
func (p *ProxyServer) SetValidator(validator RelayValidator) {
	p.validator = validator
}

// SetRelayProcessor sets the relay processor for proper relay mining.
// This is required for proper relay handling - without it, mined relays will be skipped.
func (p *ProxyServer) SetRelayProcessor(processor RelayProcessor) {
	p.relayProcessor = processor
}

// TODO: this should use a sync map with a lock, since if we need to update on keys hot reload this will panic

// SetResponseSigner sets the response signer for signing relay responses.
// This is REQUIRED for proper relay handling - clients expect signed RelayResponse protobufs.
func (p *ProxyServer) SetResponseSigner(signer *ResponseSigner) {
	p.responseSigner = signer
}

// SetSupplierCache sets the supplier cache for checking supplier state.
// This allows the relayer to check if suppliers are active before processing relays.
func (p *ProxyServer) SetSupplierCache(cache *cache.SupplierCache) {
	p.supplierCache = cache
}

// SetRelayMeter sets the relay meter for rate limiting based on app stakes.
func (p *ProxyServer) SetRelayMeter(meter *RelayMeter) {
	p.relayMeter = meter
}

// InitializeRelayPipeline initializes the unified relay processing pipeline.
// This should be called AFTER all dependencies are set (validator, relayMeter, responseSigner, relayProcessor).
// The pipeline consolidates validation, metering, signing, and publishing logic for all relay protocols.
func (p *ProxyServer) InitializeRelayPipeline() {
	if p.validator == nil || p.relayMeter == nil || p.responseSigner == nil || p.relayProcessor == nil {
		p.logger.Warn().
			Bool("has_validator", p.validator != nil).
			Bool("has_meter", p.relayMeter != nil).
			Bool("has_signer", p.responseSigner != nil).
			Bool("has_processor", p.relayProcessor != nil).
			Msg("cannot initialize relay pipeline - missing dependencies")
		return
	}

	p.relayPipeline = NewRelayPipeline(
		p.validator,
		p.relayMeter,
		p.responseSigner,
		p.relayProcessor,
		p.logger,
		p.metricRecorder,
		p.config,
	)

	p.logger.Info().Msg("relay pipeline initialized successfully")
}

// InitGRPCHandler initializes the gRPC proxy handler for handling gRPC and gRPC-Web requests.
// This should be called after SetRelayProcessor and SetResponseSigner.
func (p *ProxyServer) InitGRPCHandler() {
	p.grpcMu.Lock()
	defer p.grpcMu.Unlock()

	// Initialize the new relay service (proper relay protocol over gRPC)
	p.grpcRelayService = NewRelayGRPCService(
		p.logger,
		RelayGRPCServiceConfig{
			ServiceConfigs:     p.config.Services,
			ResponseSigner:     p.responseSigner,
			Publisher:          p.publisher,
			RelayProcessor:     p.relayProcessor,
			RelayPipeline:      p.relayPipeline, // Unified relay processing pipeline
			CurrentBlockHeight: &p.currentBlockHeight,
			MaxBodySize:        p.config.DefaultMaxBodySizeBytes,
			BufferPool:         p.bufferPool, // Share buffer pool for efficient memory usage
			GetHTTPClient:      p.getClientForService,
			GetServiceTimeout:  p.config.GetServiceTimeout, // Timeout from profile
		},
	)

	// Create gRPC server for the relay service
	// This properly handles RelayRequest/RelayResponse protocol
	p.grpcRelayServer = NewGRPCServerForRelayService(p.grpcRelayService)
	p.logger.Info().Msg("gRPC relay service server initialized")

	// Initialize gRPC-Web wrapper using the relay server
	// gRPC-Web clients should send proper RelayRequest messages
	p.grpcWebWrapper = NewGRPCWebWrapper(
		p.logger,
		p.grpcRelayServer,
	)

	p.logger.Info().Msg("gRPC relay service and handlers initialized")
}

// validateRelayRequest validates the relay request.
// If no validator is configured, validation is skipped (but body must still be valid RelayRequest).
func (p *ProxyServer) validateRelayRequest(
	ctx context.Context,
	r *http.Request,
	body []byte,
	arrivalBlockHeight int64,
) error {
	// Deserialize RelayRequest from body
	// SECURITY: This should always succeed since we already validated in handleRelay
	relayRequest := &servicetypes.RelayRequest{}
	if err := relayRequest.Unmarshal(body); err != nil {
		// SECURITY FIX: Reject non-relay traffic - don't allow unsigned requests
		return fmt.Errorf("invalid relay request: %w", err)
	}

	// If no validator is configured, skip signature/session validation
	// (The request is still a valid RelayRequest protobuf, just not cryptographically verified)
	if p.validator == nil {
		p.logger.Debug().Msg("no validator configured, skipping signature validation")
		return nil
	}

	// Set the block height for the validator
	p.validator.SetCurrentBlockHeight(arrivalBlockHeight)

	// Validate the relay request
	if err := p.validator.ValidateRelayRequest(ctx, relayRequest); err != nil {
		return fmt.Errorf("relay validation failed: %w", err)
	}

	// Check reward eligibility (for eager validation, we do this now)
	if err := p.validator.CheckRewardEligibility(ctx, relayRequest); err != nil {
		p.logger.Warn().
			Err(err).
			Msg("relay not eligible for rewards (continuing to serve)")
		// Don't return error - we still serve the relay, just won't get rewards
	}

	return nil
}

// executePublish processes a publish task and publishes the relay to Redis.
// This is called by worker goroutines with the server context.
func (p *ProxyServer) executePublish(ctx context.Context, task publishTask) {
	// Create session context from task metadata
	sessionCtx := logging.SessionContextPartial(
		task.sessionID,
		task.serviceID,
		task.supplierAddr,
		task.applicationAddr,
		0, // sessionEndHeight not available in task
	)

	if p.publisher == nil {
		logging.WithSessionContext(p.logger.Debug(), sessionCtx).
			Msg("no publisher configured, skipping relay publication")
		return
	}

	// Use RelayProcessor if available for proper relay construction
	if p.relayProcessor != nil {
		msg, err := p.relayProcessor.ProcessRelay(
			ctx,
			task.reqBody,
			task.respBody,
			task.supplierAddr,
			task.serviceID,
			task.arrivalBlockHeight,
		)
		if err != nil {
			logging.WithSessionContext(p.logger.Warn(), sessionCtx).
				Err(err).
				Msg("failed to process relay")
			return
		}

		// msg is nil if relay doesn't meet mining difficulty
		if msg == nil {
			logging.WithSessionContext(p.logger.Debug(), sessionCtx).
				Msg("relay skipped (not mined)")
			return
		}

		// Publish the mined relay
		if err := p.publisher.Publish(ctx, msg); err != nil {
			logging.WithSessionContext(p.logger.Warn(), sessionCtx).
				Err(err).
				Msg("failed to publish mined relay")
			return
		}

		relaysPublished.WithLabelValues(task.serviceID, task.supplierAddr).Inc()
		relaysMinedSuccessfully.WithLabelValues(task.serviceID).Inc()
		return
	}

	// Fallback: create a basic message without proper relay construction
	// This path should only be used in testing or when RelayProcessor is not configured
	logging.WithSessionContext(p.logger.Warn(), sessionCtx).
		Msg("no relay processor configured, using fallback message construction")

	msg := &transport.MinedRelayMessage{
		RelayHash:               nil, // Not calculated - fallback mode
		RelayBytes:              task.reqBody,
		ComputeUnitsPerRelay:    1,
		SessionId:               task.sessionID,
		SessionEndHeight:        0,
		SupplierOperatorAddress: task.supplierAddr,
		ServiceId:               task.serviceID,
		ApplicationAddress:      task.applicationAddr,
		ArrivalBlockHeight:      task.arrivalBlockHeight,
	}
	msg.SetPublishedAt()

	if err := p.publisher.Publish(ctx, msg); err != nil {
		logging.WithSessionContext(p.logger.Warn(), sessionCtx).
			Err(err).
			Msg("failed to publish mined relay")
		return
	}

	relaysPublished.WithLabelValues(task.serviceID, task.supplierAddr).Inc()
}

// sendError sends an error response.
func (p *ProxyServer) sendError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":"%s"}`, message)
}

// SetBlockHeight updates the current block height.
func (p *ProxyServer) SetBlockHeight(height int64) {
	p.currentBlockHeight.Store(height)
	currentBlockHeight.Set(float64(height))
}

// Close gracefully shuts down the proxy server.
func (p *ProxyServer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}

	p.closed = true

	if p.cancelFn != nil {
		p.cancelFn()
	}

	// Stop global session monitor
	if p.sessionMonitor != nil {
		_ = p.sessionMonitor.Close()
	}

	// Stop async metric recorder
	if p.metricRecorder != nil {
		_ = p.metricRecorder.Close()
	}

	// Stop pond subpools gracefully (drains queued tasks)
	if p.validationSubpool != nil {
		p.validationSubpool.StopAndWait()
	}
	if p.publishSubpool != nil {
		p.publishSubpool.StopAndWait()
	}
	if p.metricsSubpool != nil {
		p.metricsSubpool.StopAndWait()
	}

	p.wg.Wait()

	p.logger.Info().Msg("proxy server closed")
	return nil
}

// compressGzip compresses data using gzip compression.
// Returns the compressed data or an error if compression fails.
// Uses sync.Pool for both gzip.Writer and bytes.Buffer to reduce allocations.
func compressGzip(data []byte) ([]byte, error) {
	buf := gzipBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer gzipBufPool.Put(buf)

	writer := gzipWriterPool.Get().(*gzip.Writer)
	writer.Reset(buf)
	defer gzipWriterPool.Put(writer)

	if _, err := writer.Write(data); err != nil {
		return nil, fmt.Errorf("failed to write gzip data: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}

	// Copy to a new slice — the pooled buffer will be reused.
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

// clientAcceptsGzip checks if the client accepts gzip encoding
func clientAcceptsGzip(r *http.Request) bool {
	acceptEncoding := r.Header.Get("Accept-Encoding")
	return strings.Contains(strings.ToLower(acceptEncoding), "gzip")
}
