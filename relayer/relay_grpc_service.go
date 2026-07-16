package relayer

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip compressor for grpc-encoding: gzip
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/pool"
	"github.com/pokt-network/pocket-relay-miner/transport"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// RelayServiceMethodPath is the gRPC method path for the relay service.
// Clients (e.g., PATH gateway) call this method with a RelayRequest message.
const RelayServiceMethodPath = "/pocket.service.RelayService/SendRelay"

// grpcPublishTimeout bounds the detached mining/WAL-publish work for a gRPC relay.
// It is independent of the request/backend timeout: the response is already sent to
// the client before publishing runs, so publication must survive request
// cancellation but still cannot hang forever.
const grpcPublishTimeout = 30 * time.Second

// errBackendMisconfigured marks failures that happen before any network call:
// missing backend, an unparseable/undialable backend URL, or a scheme that
// cannot be dialed for the resolved RPC type. These are operator/config errors,
// not backend health signals, so they MUST NOT feed the circuit breaker — a
// handful of misrouted relays used to trip the shared endpoint's breaker and
// fast-fail the whole service. Callers use errors.Is to skip RecordResult.
var errBackendMisconfigured = errors.New("backend misconfigured for relay type")

// RelayGRPCService implements a gRPC service that properly handles the relay protocol.
// It receives RelayRequest messages, extracts metadata, forwards to backends, and
// returns signed RelayResponse messages.
type RelayGRPCService struct {
	logger         logging.Logger
	serviceConfigs map[string]ServiceConfig
	responseSigner *ResponseSigner
	publisher      transport.MinedRelayPublisher
	relayProcessor RelayProcessor
	relayPipeline  *RelayPipeline // Unified relay processing pipeline

	// simVerifier owns the simulated-relay Admission zone (optional). relayMeter
	// is used only for the simulated path's non-mutating meter health probe.
	simVerifier *SimulationVerifier
	relayMeter  *RelayMeter

	// Function to get HTTP client for a service (supports per-service timeout profiles)
	getHTTPClient func(serviceID string) *http.Client

	// grpcHTTPClient dials gRPC backends over HTTP/2 cleartext (h2c) with prior
	// knowledge -- the wire form of a native gRPC unary/server-streaming call.
	// The per-service HTTP/1.1 client cannot speak it, so gRPC forwarding uses
	// this dedicated client. Per-service timeouts still apply through the
	// request context (set in handleSendRelay), same as the HTTP path.
	grpcHTTPClient *http.Client

	// Function to get service timeout (from timeout profile)
	getServiceTimeout func(serviceID string) time.Duration

	// Function to get pool for circuit breaker integration
	getPool func(serviceID, rpcType string) *pool.Pool

	// Function to get backend config for circuit breaker threshold
	getBackendConfig func(serviceID, rpcType string) *BackendConfig

	// Backend gRPC connections for passthrough mode
	grpcBackends sync.Map // map[string]*grpc.ClientConn

	// Block height tracking
	currentBlockHeight *atomic.Int64

	// Max response body size
	maxBodySize int64

	// Buffer pool for reading backend responses efficiently
	bufferPool *BufferPool
}

// RelayGRPCServiceConfig contains configuration for the relay gRPC service.
type RelayGRPCServiceConfig struct {
	ServiceConfigs     map[string]ServiceConfig
	ResponseSigner     *ResponseSigner
	Publisher          transport.MinedRelayPublisher
	RelayProcessor     RelayProcessor
	RelayPipeline      *RelayPipeline // Unified relay processing pipeline
	SimVerifier        *SimulationVerifier
	RelayMeter         *RelayMeter
	CurrentBlockHeight *atomic.Int64
	MaxBodySize        int64
	BufferPool         *BufferPool
	// GetHTTPClient returns the HTTP client for a service (supports per-service timeout profiles)
	GetHTTPClient func(serviceID string) *http.Client
	// GetServiceTimeout returns the request timeout for a service (from timeout profile)
	GetServiceTimeout func(serviceID string) time.Duration
	// GetPool returns the pool for a service and RPC type (for circuit breaker integration)
	GetPool func(serviceID, rpcType string) *pool.Pool
	// GetBackendConfig returns the backend config for a service and RPC type (for circuit breaker threshold)
	GetBackendConfig func(serviceID, rpcType string) *BackendConfig
}

// NewRelayGRPCService creates a new gRPC relay service.
func NewRelayGRPCService(logger logging.Logger, config RelayGRPCServiceConfig) *RelayGRPCService {
	getHTTPClient := config.GetHTTPClient
	if getHTTPClient == nil {
		// Default client factory (fallback for tests or legacy usage)
		defaultClient := &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
			},
		}
		getHTTPClient = func(_ string) *http.Client {
			return defaultClient
		}
	}

	getServiceTimeout := config.GetServiceTimeout
	if getServiceTimeout == nil {
		// Default timeout (fallback for tests or legacy usage)
		getServiceTimeout = func(_ string) time.Duration {
			return 30 * time.Second
		}
	}

	maxBodySize := config.MaxBodySize
	if maxBodySize == 0 {
		maxBodySize = 10 * 1024 * 1024 // 10MB default
	}

	bufferPool := config.BufferPool
	if bufferPool == nil {
		// Create a default buffer pool if not provided
		bufferPool = NewBufferPool(maxBodySize)
	}

	// Dedicated client for forwarding to gRPC backends. h2c (HTTP/2 cleartext,
	// prior knowledge) with HTTP/1.1 disabled is exactly how a native gRPC
	// client opens a non-TLS connection. No client-level Timeout: the per-request
	// context deadline (service timeout) governs, matching the HTTP path.
	grpcTransport := &http.Transport{}
	grpcTransport.Protocols = new(http.Protocols)
	grpcTransport.Protocols.SetUnencryptedHTTP2(true)
	grpcTransport.Protocols.SetHTTP1(false)
	grpcHTTPClient := &http.Client{
		Transport: grpcTransport,
		// Never follow redirects -- pass them through, as the HTTP client does.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &RelayGRPCService{
		logger:             logger.With().Str(logging.FieldComponent, "grpc_relay_service").Logger(),
		serviceConfigs:     config.ServiceConfigs,
		responseSigner:     config.ResponseSigner,
		publisher:          config.Publisher,
		relayProcessor:     config.RelayProcessor,
		relayPipeline:      config.RelayPipeline,
		simVerifier:        config.SimVerifier,
		relayMeter:         config.RelayMeter,
		currentBlockHeight: config.CurrentBlockHeight,
		maxBodySize:        maxBodySize,
		bufferPool:         bufferPool,
		getHTTPClient:      getHTTPClient,
		grpcHTTPClient:     grpcHTTPClient,
		getServiceTimeout:  getServiceTimeout,
		getPool:            config.GetPool,
		getBackendConfig:   config.GetBackendConfig,
	}
}

// RegisterWithServer registers the relay service handler with a gRPC server.
// This uses the UnknownServiceHandler pattern to intercept calls to our method path.
func (s *RelayGRPCService) RegisterWithServer(server *grpc.Server) {
	// Note: We use UnknownServiceHandler in the server options instead of registering here.
	// This is because we're handling a dynamically defined service.
	s.logger.Info().Msg("relay gRPC service registered")
}

// HandleUnknownService is a gRPC stream handler that processes relay requests.
// It should be registered as the UnknownServiceHandler on the gRPC server.
func (s *RelayGRPCService) HandleUnknownService(srv interface{}, stream grpc.ServerStream) error {
	// Get the full method name from the stream
	fullMethod, ok := grpc.Method(stream.Context())
	if !ok {
		return status.Error(codes.Internal, "failed to get method name")
	}

	// Check if this is a relay request
	if fullMethod != RelayServiceMethodPath {
		return status.Errorf(codes.Unimplemented, "unknown method: %s", fullMethod)
	}

	return s.handleSendRelay(stream)
}

// handleSendRelay processes a SendRelay gRPC call.
func (s *RelayGRPCService) handleSendRelay(stream grpc.ServerStream) error {
	ctx := stream.Context()
	arrivalTime := time.Now()
	arrivalHeight := int64(0)
	if s.currentBlockHeight != nil {
		arrivalHeight = s.currentBlockHeight.Load()
	}

	// Receive the RelayRequest message (typed proto message)
	relayRequest := &servicetypes.RelayRequest{}
	if err := stream.RecvMsg(relayRequest); err != nil {
		grpcRelayErrors.WithLabelValues("unknown", "recv_error").Inc()
		return status.Errorf(codes.InvalidArgument, "failed to receive request: %v", err)
	}

	// Extract metadata from RelayRequest.Meta
	if relayRequest.Meta.SessionHeader == nil {
		grpcRelayErrors.WithLabelValues("unknown", "missing_session_header").Inc()
		return status.Error(codes.InvalidArgument, "missing session header in RelayRequest")
	}

	serviceID := relayRequest.Meta.SessionHeader.ServiceId
	supplierOperatorAddr := relayRequest.Meta.SupplierOperatorAddress
	applicationAddr := relayRequest.Meta.SessionHeader.ApplicationAddress
	sessionID := relayRequest.Meta.SessionHeader.SessionId
	sessionEndHeight := relayRequest.Meta.SessionHeader.SessionEndBlockHeight

	// Create session context for consistent logging
	sessionCtx := logging.SessionContextPartial(sessionID, serviceID, supplierOperatorAddr, applicationAddr, sessionEndHeight)

	if serviceID == "" {
		grpcRelayErrors.WithLabelValues("unknown", "missing_service_id").Inc()
		return status.Error(codes.InvalidArgument, "missing service ID in session header")
	}

	if supplierOperatorAddr == "" {
		grpcRelayErrors.WithLabelValues(serviceID, "missing_supplier_address").Inc()
		return status.Error(codes.InvalidArgument, "missing supplier operator address in RelayRequest")
	}

	logging.WithSessionContext(s.logger.Debug(), sessionCtx).
		Msg("received gRPC relay request")

	// Verify we have a signer for this supplier
	if s.responseSigner == nil || !s.responseSigner.HasSigner(supplierOperatorAddr) {
		grpcRelayErrors.WithLabelValues(serviceID, "no_signer").Inc()
		return status.Errorf(codes.FailedPrecondition, "no signer for supplier %s", supplierOperatorAddr)
	}

	// Get service configuration
	svcConfig, ok := s.serviceConfigs[serviceID]
	if !ok {
		grpcRelayErrors.WithLabelValues(serviceID, "unknown_service").Inc()
		return status.Errorf(codes.NotFound, "unknown service: %s", serviceID)
	}

	// Apply service timeout to context (from timeout profile)
	serviceTimeout := s.getServiceTimeout(serviceID)
	ctx, cancel := context.WithTimeout(ctx, serviceTimeout)
	defer cancel()

	// SIMULATION SEAM — the only simulation-aware line on the gRPC normal path.
	// Admitted eagerly here, before the pipeline validate/meter and before any
	// publish. Reuses the shared data-path primitives (forwardToBackend +
	// response signer) inside serveSimulatedGRPC. Header ignored when disabled.
	md, _ := metadata.FromIncomingContext(ctx)
	if directive := SimDirectiveFromGRPC(md); directive.KeyID != "" && s.simVerifier != nil && s.simVerifier.Enabled() {
		return s.serveSimulatedGRPC(stream, ctx, relayRequest, serviceID, svcConfig, supplierOperatorAddr, directive.KeyID, md)
	}

	// Validate and meter the relay if pipeline is available
	if s.relayPipeline != nil {
		// TODO: Get actual compute units from service config
		// For now, use default value of 1 (will be refined in future PR)
		computeUnits := uint64(1)

		// Build relay context for validation/metering
		relayCtx := &RelayContext{
			Request:            relayRequest,
			ServiceID:          serviceID,
			SupplierAddress:    supplierOperatorAddr,
			SessionID:          sessionID,
			ComputeUnits:       computeUnits,
			ArrivalBlockHeight: arrivalHeight,
		}

		// Validate relay request (ring signature + session)
		if err := s.relayPipeline.ValidateRelay(ctx, relayCtx); err != nil {
			grpcRelayErrors.WithLabelValues(serviceID, "validation_failed").Inc()
			logging.WithSessionContext(s.logger.Warn(), sessionCtx).
				Err(err).
				Msg("relay validation failed")
			return status.Errorf(codes.PermissionDenied, "relay validation failed: %v", err)
		}

		// Meter relay (check stake before serving)
		allowed, meterErr := s.relayPipeline.MeterRelay(ctx, relayCtx)
		if meterErr != nil {
			// Fail-open: log warning but allow relay
			logging.WithSessionContext(s.logger.Warn(), sessionCtx).
				Err(meterErr).
				Msg("relay metering error (fail-open: allowing relay)")
		} else if !allowed {
			// Stake limit exceeded - reject relay
			grpcRelayErrors.WithLabelValues(serviceID, "meter_rejected").Inc()
			logging.WithSessionContext(s.logger.Warn(), sessionCtx).
				Msg("relay rejected - stake limit exceeded")
			return status.Error(codes.ResourceExhausted, "relay rejected - stake limit exceeded")
		}
	}

	// Deserialize the POKTHTTPRequest from the relay payload
	poktHTTPRequest, err := sdktypes.DeserializeHTTPRequest(relayRequest.Payload)
	if err != nil {
		grpcRelayErrors.WithLabelValues(serviceID, "payload_deserialize_error").Inc()
		return status.Errorf(codes.InvalidArgument, "failed to deserialize POKTHTTPRequest: %v", err)
	}

	logging.WithSessionContext(s.logger.Debug(), sessionCtx).
		Str("method", poktHTTPRequest.Method).
		Str("url", poktHTTPRequest.Url).
		Int("body_size", len(poktHTTPRequest.BodyBz)).
		Msg("deserialized POKTHTTPRequest from relay payload")

	// Resolve backend RPC type for pool lookup and forwarding. Precedence mirrors
	// the HTTP path (proxy.go:719-728): the client's declared "rpc-type" gRPC
	// metadata wins, then the inner Content-Type heuristic, then the service
	// default. Honouring the metadata is what fixes native gRPC relays -- the CLI
	// declares metadata.Pairs("rpc-type","1"); the inner request is still typed
	// application/json, so the old Content-Type-only logic mislabeled every gRPC
	// relay as "rest" and misrouted it.
	grpcRPCType := resolveGRPCRelayRPCType(md, poktHTTPRequest, &svcConfig)

	var grpcEndpoint *pool.BackendEndpoint
	var grpcPool *pool.Pool
	if s.getPool != nil {
		grpcPool = s.getPool(serviceID, grpcRPCType)
	}

	// Fast-fail pre-check: return codes.Unavailable when all backends are unhealthy.
	// Skips expensive forwarding, signing, and publishing.
	if grpcPool == nil || !grpcPool.HasHealthy() {
		fastFailsTotal.WithLabelValues(serviceID).Inc()
		s.logger.Debug().Str("service_id", serviceID).Msg("fast-fail: all gRPC backends unhealthy")
		return status.Errorf(codes.Unavailable, "all backends unhealthy for service %s", serviceID)
	}

	grpcEndpoint = grpcPool.Next()

	// Forward request to backend and get response
	respBody, respHeaders, respStatus, err := s.forwardToBackend(ctx, serviceID, &svcConfig, poktHTTPRequest, grpcEndpoint, grpcRPCType)

	// Record result for circuit breaker (covers both success and network-error
	// paths). Config/misroute errors (errBackendMisconfigured) are excluded: the
	// breaker measures backend health, and a misrouted relay that never touched
	// the network says nothing about it -- counting it would fast-fail the whole
	// endpoint on a handful of mistyped relays.
	if grpcEndpoint != nil && grpcPool != nil && !errors.Is(err, errBackendMisconfigured) {
		threshold := s.getCircuitBreakerThreshold(serviceID, grpcRPCType)
		transition := grpcPool.RecordResult(grpcEndpoint, respStatus, err, threshold)
		if transition != nil {
			s.logCircuitBreakerTransition(transition, serviceID, grpcRPCType)
		}
	}

	if err != nil {
		grpcRelayErrors.WithLabelValues(serviceID, "backend_error").Inc()
		logging.WithSessionContext(s.logger.Error(), sessionCtx).
			Err(err).
			Msg("failed to forward request to backend")

		// Build error response
		relayResponse, _, buildErr := s.responseSigner.BuildErrorRelayResponse(
			relayRequest.Meta.SessionHeader,
			supplierOperatorAddr,
			500,
			fmt.Sprintf("backend error: %v", err),
		)
		if buildErr != nil {
			return status.Errorf(codes.Internal, "failed to build error response: %v", buildErr)
		}

		// Send error response (typed proto message) so the client learns the
		// relay failed. This is a transport/config failure: forwardToBackend
		// returned err != nil, meaning the backend produced NO real response
		// (connection refused, timeout, misroute/errBackendMisconfigured). Such a
		// relay is NOT publishable — mirror the HTTP path (proxy.go), which on a
		// forward error returns an error to the client and publishes nothing.
		// Publishing here would mine a supplier-fabricated 500 for a relay the
		// backend never answered, inflating the WAL with unclaimable garbage and
		// diverging from HTTP. A backend that DID answer (4xx/5xx with a body)
		// sets err == nil and is handled by the success/5xx branches below, so
		// legitimately claimable error relays are unaffected.
		if sendErr := stream.SendMsg(relayResponse); sendErr != nil {
			return status.Errorf(codes.Internal, "failed to send error response: %v", sendErr)
		}

		return nil
	}

	// Check for 5xx backend errors - these should NOT be wrapped, signed, or mined
	// 2xx-4xx are valid relays (client/backend logic errors that should be paid)
	// 5xx are infrastructure/backend failures (supplier should not be compensated)
	if respStatus >= http.StatusInternalServerError {
		grpcRelayErrors.WithLabelValues(serviceID, "backend_5xx").Inc()
		logging.WithSessionContext(s.logger.Warn(), sessionCtx).
			Int("status_code", respStatus).
			Msg("backend returned 5xx error - relay not mined")
		// Return gRPC error without wrapping/signing/mining
		return status.Errorf(codes.Unavailable, "backend service error: HTTP %d", respStatus)
	}

	// Build and sign the RelayResponse
	relayResponse, relayResponseBz, err := s.responseSigner.BuildAndSignRelayResponseFromBody(
		relayRequest,
		respBody,
		respHeaders,
		respStatus,
	)
	if err != nil {
		grpcRelayErrors.WithLabelValues(serviceID, "sign_error").Inc()
		return status.Errorf(codes.Internal, "failed to build/sign response: %v", err)
	}

	// Send the response (typed proto message)
	if err := stream.SendMsg(relayResponse); err != nil {
		grpcRelayErrors.WithLabelValues(serviceID, "send_error").Inc()
		return status.Errorf(codes.Internal, "failed to send response: %v", err)
	}

	// Use RelayProcessor for consistent relay processing (mining difficulty, deduplication, publishing)
	if s.relayProcessor != nil {
		// Marshal request and response for ProcessRelay
		reqBz, err := relayRequest.Marshal()
		if err != nil {
			logging.WithSessionContext(s.logger.Warn(), sessionCtx).
				Err(err).
				Msg("failed to marshal relay request for processing")
		} else {
			// publishCtx detaches the mining + WAL publish from the request
			// lifetime. It is created HERE — after the backend forward and after
			// the signed response was already sent to the client — so its full
			// grpcPublishTimeout budget starts at publish time, NOT at handler
			// entry. Creating it earlier would let a slow-but-successful backend
			// consume the budget: an already-served relay would then fail to
			// publish (context deadline exceeded) and never reach the WAL — no
			// session/claim/proof, lost reward. context.WithoutCancel drops the
			// request/backend deadline (already spent) while keeping publication
			// alive past client cancellation. Mirrors the HTTP path, which
			// publishes on a fresh detached context created at publish time.
			publishCtx, publishCancel := context.WithTimeout(context.WithoutCancel(ctx), grpcPublishTimeout)
			defer publishCancel()

			// Mine the RAW backend response body, exactly like the HTTP path
			// (proxy.go executePublish passes task.respBody). ProcessRelay builds
			// and signs its OWN RelayResponse over this body and derives
			// PayloadHash = sha256(respBody). Handing it the already-marshaled
			// relayResponseBz instead would make PayloadHash cover a
			// RelayResponse-wrapped-in-a-RelayResponse — the wrong bytes — and
			// diverge from what the HTTP path mines.
			//
			// ProcessRelay only BUILDS the message; without the Publish call below
			// the relay never reaches the miner's WAL and no session/claim/proof is
			// ever created for it.
			msg, err := s.relayProcessor.ProcessRelay(
				publishCtx,
				reqBz,
				respBody,
				supplierOperatorAddr,
				serviceID,
				arrivalHeight,
			)
			if err != nil {
				logging.WithSessionContext(s.logger.Warn(), sessionCtx).
					Err(err).
					Msg("failed to process relay")
			} else if msg == nil {
				// Relay didn't meet mining difficulty
				logging.WithSessionContext(s.logger.Debug(), sessionCtx).
					Msg("gRPC relay skipped (did not meet mining difficulty)")
			} else if s.publisher == nil {
				logging.WithSessionContext(s.logger.Warn(), sessionCtx).
					Msg("no publisher configured, skipping relay publication")
			} else if pubErr := s.publisher.Publish(publishCtx, msg); pubErr != nil {
				logging.WithSessionContext(s.logger.Warn(), sessionCtx).
					Err(pubErr).
					Msg("failed to publish mined relay")
			} else {
				// Relay was successfully processed and published
				logging.WithSessionContext(s.logger.Debug(), sessionCtx).
					Msg("gRPC relay processed and published")
				grpcRelaysPublished.WithLabelValues(serviceID).Inc()
			}
		}
	}

	// Update metrics
	grpcRelaysTotal.WithLabelValues(serviceID).Inc()
	grpcRelayLatency.WithLabelValues(serviceID).Observe(time.Since(arrivalTime).Seconds())

	logging.WithSessionContext(s.logger.Debug(), sessionCtx).
		Int("response_size", len(relayResponseBz)).
		Dur("latency", time.Since(arrivalTime)).
		Msg("gRPC relay completed successfully")

	return nil
}

// simGRPCCode maps a simulation admission error to a gRPC status code. The
// metric result label comes from SimResultForError (transport-agnostic).
func simGRPCCode(err error) codes.Code {
	switch {
	case errors.Is(err, ErrSimReplay):
		return codes.AlreadyExists
	case errors.Is(err, ErrSimServiceUnknown):
		return codes.NotFound
	case errors.Is(err, ErrSimSupplierMissing), errors.Is(err, ErrSimBadSessionID):
		return codes.InvalidArgument
	case errors.Is(err, ErrSimDedupUnavailable):
		return codes.Unavailable
	default:
		return codes.PermissionDenied
	}
}

// serveSimulatedGRPC serves a simulated relay over native gRPC. It runs the
// simulation Admission zone (global slot -> pinned-ring/binding/freshness
// Verify -> per-key rate), then the SHARED data path (the SAME forwardToBackend
// and response signer the real gRPC path uses), skipping Accounting entirely
// (no meter consume, no publish). Always eager-admission.
func (s *RelayGRPCService) serveSimulatedGRPC(
	stream grpc.ServerStream,
	ctx context.Context,
	relayRequest *servicetypes.RelayRequest,
	serviceID string,
	svcConfig ServiceConfig,
	supplier, keyID string,
	md metadata.MD,
) error {
	const transportLabel = "grpc"
	record := func(result string) {
		simulatedRelaysTotal.WithLabelValues(transportLabel, serviceID, supplier, result).Inc()
	}

	release, ok := s.simVerifier.AcquireGlobal()
	if !ok {
		record(SimResultRateLimited)
		return status.Error(codes.ResourceExhausted, "simulation concurrency limit reached")
	}
	defer release()

	if err := s.simVerifier.Verify(ctx, keyID, relayRequest); err != nil {
		record(SimResultForError(err))
		return status.Errorf(simGRPCCode(err), "simulation rejected: %v", err)
	}
	if !s.simVerifier.AllowKey(keyID) {
		record(SimResultRateLimited)
		return status.Error(codes.ResourceExhausted, "simulation rate limit reached")
	}

	// SHARED DATA PATH — deserialize, resolve backend type, forward.
	poktHTTPRequest, err := sdktypes.DeserializeHTTPRequest(relayRequest.Payload)
	if err != nil {
		record(SimResultVerifyFailed)
		return status.Errorf(codes.InvalidArgument, "failed to deserialize payload: %v", err)
	}
	grpcRPCType := resolveGRPCRelayRPCType(md, poktHTTPRequest, &svcConfig)
	var grpcEndpoint *pool.BackendEndpoint
	if s.getPool != nil {
		grpcPool := s.getPool(serviceID, grpcRPCType)
		if grpcPool == nil || !grpcPool.HasHealthy() {
			record(SimResultBackendError)
			return status.Error(codes.Unavailable, "backend unavailable")
		}
		grpcEndpoint = grpcPool.Next()
	}
	respBody, respHeaders, respStatus, err := s.forwardToBackend(ctx, serviceID, &svcConfig, poktHTTPRequest, grpcEndpoint, grpcRPCType)
	if err != nil {
		record(SimResultBackendError)
		return status.Errorf(codes.Unavailable, "backend error: %v", err)
	}
	if respStatus >= 500 {
		record(SimResultBackendError)
		return status.Errorf(codes.Unavailable, "backend service error: HTTP %d", respStatus)
	}

	// SHARED DATA PATH — same response signer as the real path.
	relayResponse, _, err := s.responseSigner.BuildAndSignRelayResponseFromBody(relayRequest, respBody, respHeaders, respStatus)
	if err != nil {
		record(SimResultSignFailed)
		return status.Errorf(codes.Internal, "failed to sign response: %v", err)
	}
	if err := stream.SendMsg(relayResponse); err != nil {
		record(SimResultBackendError)
		return status.Errorf(codes.Internal, "failed to send response: %v", err)
	}

	// ACCOUNTING gated off: no publish. Dry meter probe feeds the result label.
	result := SimResultSuccess
	if s.relayMeter != nil {
		if healthErr := s.relayMeter.CheckRelayHealth(ctx, serviceID); healthErr != nil {
			result = SimResultMeterDegraded
		}
	}
	record(result)
	return nil
}

// forwardToBackend forwards the request to the appropriate backend service.
// If endpoint is non-nil, its URL is used for backend selection (pool-based).
// Otherwise, falls back to config-based URL lookup (legacy path).
//
// rpcType is resolved by the caller (see resolveGRPCRelayRPCType) so backend
// selection, URL normalization, and transport choice all follow the client's
// declared transport rather than re-deriving it from the inner Content-Type.
func (s *RelayGRPCService) forwardToBackend(
	ctx context.Context,
	serviceID string,
	svcConfig *ServiceConfig,
	poktHTTPRequest *sdktypes.POKTHTTPRequest,
	endpoint *pool.BackendEndpoint,
	rpcType string,
) ([]byte, http.Header, int, error) {
	// Find the backend configuration
	var backendURL string
	var configHeaders map[string]string
	var auth *AuthenticationConfig

	// Use pool endpoint URL if available (circuit breaker integration)
	if endpoint != nil {
		backendURL = endpoint.RawURL
	}

	// Get headers and auth from backend config (always needed regardless of pool usage)
	if backend, ok := svcConfig.Backends[rpcType]; ok {
		if backendURL == "" {
			backendURL = backend.URL
		}
		configHeaders = backend.Headers
		auth = backend.Authentication
	} else if backend, ok := svcConfig.Backends["rest"]; ok {
		if backendURL == "" {
			backendURL = backend.URL
		}
		configHeaders = backend.Headers
		auth = backend.Authentication
	} else {
		// Use any available backend
		for _, backend := range svcConfig.Backends {
			if backendURL == "" {
				backendURL = backend.URL
			}
			configHeaders = backend.Headers
			auth = backend.Authentication
			break
		}
	}

	if backendURL == "" {
		return nil, nil, 0, fmt.Errorf("%w: no backend configured for service %s", errBackendMisconfigured, serviceID)
	}

	// Normalize the backend URL for the resolved RPC type. gRPC backends are
	// conventionally configured as bare host:port (e.g. "backend:50051"), which
	// url.Parse reads as scheme "backend" and http.NewRequest then rejects.
	backendURL, err := normalizeBackendURLForRPCType(backendURL, rpcType)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("%w: %v", errBackendMisconfigured, err)
	}

	// Build the request URL
	requestURL, err := url.Parse(poktHTTPRequest.Url)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to parse request URL: %w", err)
	}

	backendParsed, err := url.Parse(backendURL)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("%w: failed to parse backend URL: %v", errBackendMisconfigured, err)
	}

	// After normalization the backend must be dialable over HTTP(S); anything
	// else (e.g. a scheme-less REST backend, or ws://) would fail deep inside
	// http.NewRequest as a raw error and poison the breaker. Treat it as config.
	if backendParsed.Scheme != "http" && backendParsed.Scheme != "https" {
		return nil, nil, 0, fmt.Errorf("%w: backend scheme %q is not dialable for rpc type %q", errBackendMisconfigured, backendParsed.Scheme, rpcType)
	}

	// Merge URLs
	requestURL.Scheme = backendParsed.Scheme
	requestURL.Host = backendParsed.Host
	if backendParsed.Path != "" && backendParsed.Path != "/" {
		requestURL.Path = backendParsed.Path + requestURL.Path
	}
	// Normalize multi-slash artifacts (e.g. "//") before dispatch — raw
	// backends without a normalizing proxy return 404 for `POST //`. See #8.
	requestURL.Path = normalizeBackendPath(requestURL.Path)

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, poktHTTPRequest.Method, requestURL.String(), bytes.NewReader(poktHTTPRequest.BodyBz))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to create request: %w", err)
	}

	// Copy headers from POKTHTTPRequest
	if poktHTTPRequest.Header != nil {
		for key, header := range poktHTTPRequest.Header {
			for _, value := range header.Values {
				req.Header.Add(key, value)
			}
		}
	}

	// Add config headers (override any matching keys)
	for key, value := range configHeaders {
		req.Header.Set(key, value)
	}

	// Add authentication if configured
	if auth != nil {
		if auth.Username != "" && auth.Password != "" {
			req.SetBasicAuth(auth.Username, auth.Password)
		} else if auth.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+auth.BearerToken)
		} else if auth.PlainToken != "" {
			req.Header.Set("Authorization", auth.PlainToken)
		}
	}

	// Set host header
	req.Host = backendParsed.Host

	// Execute request. gRPC backends need the dedicated h2c client; everything
	// else uses the per-service HTTP/1.1 client with its pool/timeout profile.
	client := s.getHTTPClient(serviceID)
	if rpcType == BackendTypeGRPC {
		client = s.grpcHTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response body using buffer pool to avoid RAM exhaustion
	// The buffer pool enforces the size limit and reuses buffers
	respBody, err := s.bufferPool.ReadWithBuffer(resp.Body)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to read response: %w", err)
	}

	// gRPC carries its status (grpc-status/grpc-message) in HTTP trailers, sent
	// after the body -- so they are only readable now that the body reached EOF.
	// gRPC clients need grpc-status to interpret the reply, so fold trailers into
	// the returned headers. This covers unary and buffered server-streaming; it
	// does not model full-duplex streaming, which this proxy does not forward.
	return respBody, mergeTrailersIntoHeader(resp.Header, resp.Trailer), resp.StatusCode, nil
}

// resolveGRPCRelayRPCType determines the backend RPC type for a gRPC relay.
// Precedence mirrors the HTTP path (proxy.go:719-728): the client declares its
// intended transport through the "rpc-type" gRPC metadata, and that intent must
// win over the inner POKTHTTPRequest Content-Type, which is only a heuristic
// fallback. Order: (1) metadata "rpc-type", (2) inner Content-Type
// "application/grpc*", (3) service DefaultBackend, (4) global default.
func resolveGRPCRelayRPCType(md metadata.MD, poktReq *sdktypes.POKTHTTPRequest, svcConfig *ServiceConfig) string {
	// 1. Explicit client declaration via gRPC metadata (numeric code or name).
	for _, v := range md.Get("rpc-type") {
		if v != "" {
			return RPCTypeToBackendType(v)
		}
	}

	// 2. Heuristic: the inner request advertises application/grpc.
	if poktReq != nil && poktReq.Header != nil {
		if ct, ok := poktReq.Header["Content-Type"]; ok && len(ct.Values) > 0 {
			if strings.HasPrefix(ct.Values[0], "application/grpc") {
				return BackendTypeGRPC
			}
		}
	}

	// 3. Service-configured default backend (normalized the same as the header).
	if svcConfig != nil && svcConfig.DefaultBackend != "" {
		return RPCTypeToBackendType(svcConfig.DefaultBackend)
	}

	// 4. Global default.
	return DefaultBackendType
}

// normalizeBackendURLForRPCType makes a gRPC backend URL dialable over h2c.
// Only gRPC is rewritten: a bare host:port (the conventional gRPC backend form,
// e.g. "backend:50051") gets an "http://" scheme so url.Parse/http.NewRequest
// can dial it as cleartext HTTP/2. URLs that already carry http/https are left
// alone; any other explicit scheme (ws://, ftp://, ...) is a misconfiguration.
// Non-gRPC types are returned untouched -- a genuine problem there surfaces as
// an undialable scheme downstream, still classified as a config error.
func normalizeBackendURLForRPCType(backendURL, rpcType string) (string, error) {
	if rpcType != BackendTypeGRPC {
		return backendURL, nil
	}
	if strings.HasPrefix(backendURL, "http://") || strings.HasPrefix(backendURL, "https://") {
		return backendURL, nil
	}
	// No "://" means a bare host:port -- treat it as h2c cleartext.
	if scheme, _, found := strings.Cut(backendURL, "://"); found {
		return "", fmt.Errorf("gRPC backend URL %q has undialable scheme %q", backendURL, scheme)
	}
	return "http://" + backendURL, nil
}

// mergeTrailersIntoHeader returns header extended with every value from trailer.
// When trailer is empty the original header is returned unchanged (no copy).
// Keys are canonicalized by http.Header, so callers read grpc-status via
// Get("Grpc-Status") regardless of the wire casing.
func mergeTrailersIntoHeader(header, trailer http.Header) http.Header {
	if len(trailer) == 0 {
		return header
	}
	merged := header.Clone()
	if merged == nil {
		merged = make(http.Header, len(trailer))
	}
	for key, values := range trailer {
		for _, value := range values {
			merged.Add(key, value)
		}
	}
	return merged
}

// getCircuitBreakerThreshold returns the unhealthy threshold for circuit breaker evaluation.
func (s *RelayGRPCService) getCircuitBreakerThreshold(serviceID, rpcType string) int32 {
	if s.getBackendConfig != nil {
		if backendCfg := s.getBackendConfig(serviceID, rpcType); backendCfg != nil {
			if backendCfg.HealthCheck != nil && backendCfg.HealthCheck.UnhealthyThreshold > 0 {
				return int32(backendCfg.HealthCheck.UnhealthyThreshold)
			}
		}
	}
	return pool.DefaultUnhealthyThreshold
}

// logCircuitBreakerTransition logs a circuit breaker state transition.
func (s *RelayGRPCService) logCircuitBreakerTransition(transition *pool.TransitionEvent, serviceID, rpcType string) {
	if transition.OldHealthy && !transition.NewHealthy {
		event := s.logger.Warn().
			Str("backend", transition.Endpoint.Name).
			Str("url", transition.Endpoint.RawURL).
			Str(logging.FieldServiceID, serviceID).
			Str("rpc_type", rpcType).
			Int32("consecutive_failures", transition.Failures).
			Int32("threshold", pool.DefaultUnhealthyThreshold)

		if transition.StatusCode > 0 {
			event = event.Int("trigger_http_status", transition.StatusCode)
		}
		if transition.Error != nil {
			event = event.Str("trigger_error", transition.Error.Error())
		}

		event.Msg("BACKEND DOWN: circuit breaker tripped, traffic will failover to other backends")
	} else if !transition.OldHealthy && transition.NewHealthy {
		event := s.logger.Info().
			Str("backend", transition.Endpoint.Name).
			Str("url", transition.Endpoint.RawURL).
			Str(logging.FieldServiceID, serviceID).
			Str("rpc_type", rpcType)

		if transition.DowntimeDuration > 0 {
			event = event.Dur("downtime", transition.DowntimeDuration)
		}

		event.Msg("BACKEND UP: circuit breaker recovered, backend is healthy again")
	}
}

// Unused - reserved for future gRPC streaming support
// connectToGRPCBackend establishes a gRPC connection to a backend (for future streaming support).
// func (s *RelayGRPCService) connectToGRPCBackend(backendURL string) (*grpc.ClientConn, error) {
// 	// Check cache
// 	if conn, ok := s.grpcBackends.Load(backendURL); ok {
// 		return conn.(*grpc.ClientConn), nil
// 	}
//
// 	// Parse URL to determine TLS
// 	useTLS := strings.HasPrefix(backendURL, "grpcs://") || strings.HasPrefix(backendURL, "https://")
//
// 	// Strip scheme
// 	address := backendURL
// 	address = strings.TrimPrefix(address, "grpcs://")
// 	address = strings.TrimPrefix(address, "grpc://")
// 	address = strings.TrimPrefix(address, "https://")
// 	address = strings.TrimPrefix(address, "http://")
//
// 	var opts []grpc.DialOption
// 	if useTLS {
// 		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
// 			MinVersion: tls.VersionTLS12,
// 		})))
// 	} else {
// 		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
// 	}
//
// 	conn, err := grpc.NewClient(address, opts...)
// 	if err != nil {
// 		return nil, err
// 	}
//
// 	s.grpcBackends.Store(backendURL, conn)
// 	return conn, nil
// }

// Close closes all backend connections.
func (s *RelayGRPCService) Close() error {
	var lastErr error
	s.grpcBackends.Range(func(key, value interface{}) bool {
		if conn, ok := value.(*grpc.ClientConn); ok {
			if err := conn.Close(); err != nil {
				lastErr = err
			}
		}
		return true
	})
	return lastErr
}

// NewGRPCServerForRelayService creates a gRPC server configured for the relay service.
// It uses the standard proto codec and handles typed RelayRequest/RelayResponse messages.
// Includes panic recovery interceptors for both unary and stream RPCs.
func NewGRPCServerForRelayService(service *RelayGRPCService) *grpc.Server {
	server := grpc.NewServer(
		grpc.UnknownServiceHandler(service.HandleUnknownService),
		grpc.UnaryInterceptor(UnaryPanicRecoveryInterceptor(service.logger)),
		grpc.StreamInterceptor(StreamPanicRecoveryInterceptor(service.logger)),
	)
	return server
}
