package relayer

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// WebSocket timeout constants for connection keep-alive.
// These are fixed values for ping/pong health checks and are NOT derived from timeout profiles.
// WebSocket connections are long-lived and session-based - they don't follow request/response timeout patterns.
const (
	// wsWriteWait is the time allowed to write a message to the peer.
	wsWriteWait = 10 * time.Second

	// wsPongWait is the time allowed to wait for the next pong message.
	wsPongWait = 30 * time.Second

	// wsPingPeriod is the send pings to peer with this period. Must be less than pongWait.
	wsPingPeriod = (wsPongWait * 9) / 10

	// defaultWSDialTimeout is the default timeout for establishing WebSocket connection.
	defaultWSDialTimeout = 10 * time.Second
)

// RFC 6455 WebSocket Close Codes
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.1
const (
	CloseNormalClosure           = 1000 // Normal closure; the connection successfully completed
	CloseGoingAway               = 1001 // Endpoint is going away (e.g., server shutdown, browser navigating away)
	CloseProtocolError           = 1002 // Protocol error
	CloseUnsupportedData         = 1003 // Received data type cannot be accepted
	CloseNoStatusReceived        = 1005 // No status code was provided (reserved, must not be sent)
	CloseAbnormalClosure         = 1006 // Connection closed abnormally (reserved, must not be sent)
	CloseInvalidPayload          = 1007 // Received data was inconsistent with message type
	ClosePolicyViolation         = 1008 // Received message violates policy
	CloseMessageTooBig           = 1009 // Message too big to process
	CloseMandatoryExtension      = 1010 // Expected extension was not negotiated
	CloseInternalError           = 1011 // Server encountered unexpected condition
	CloseServiceRestart          = 1012 // Server is restarting
	CloseTryAgainLater           = 1013 // Server is overloaded, try again later
	CloseBadGateway              = 1014 // Gateway received invalid response from upstream (unofficial)
	CloseTLSHandshakeFailed      = 1015 // TLS handshake failed (reserved, must not be sent)
	CloseSessionExpired          = 4000 // Custom: Pocket session expired
	CloseValidationFailed        = 4001 // Custom: Relay validation failed
	CloseStakeLimitExceeded      = 4002 // Custom: Application stake limit exceeded
	CloseBackendConnectionFailed = 4003 // Custom: Failed to connect to backend
)

// closeCodeName returns a human-readable name for a WebSocket close code.
func closeCodeName(code int) string {
	switch code {
	case CloseNormalClosure:
		return "NormalClosure"
	case CloseGoingAway:
		return "GoingAway"
	case CloseProtocolError:
		return "ProtocolError"
	case CloseUnsupportedData:
		return "UnsupportedData"
	case CloseNoStatusReceived:
		return "NoStatusReceived"
	case CloseAbnormalClosure:
		return "AbnormalClosure"
	case CloseInvalidPayload:
		return "InvalidPayload"
	case ClosePolicyViolation:
		return "PolicyViolation"
	case CloseMessageTooBig:
		return "MessageTooBig"
	case CloseMandatoryExtension:
		return "MandatoryExtension"
	case CloseInternalError:
		return "InternalError"
	case CloseServiceRestart:
		return "ServiceRestart"
	case CloseTryAgainLater:
		return "TryAgainLater"
	case CloseBadGateway:
		return "BadGateway"
	case CloseTLSHandshakeFailed:
		return "TLSHandshakeFailed"
	case CloseSessionExpired:
		return "SessionExpired"
	case CloseValidationFailed:
		return "ValidationFailed"
	case CloseStakeLimitExceeded:
		return "StakeLimitExceeded"
	case CloseBackendConnectionFailed:
		return "BackendConnectionFailed"
	default:
		return "Unknown"
	}
}

// wsCloseInitiator identifies who initiated a WebSocket close.
type wsCloseInitiator string

const (
	wsCloseInitiatorClient  wsCloseInitiator = "client"  // PATH gateway (upstream)
	wsCloseInitiatorBackend wsCloseInitiator = "backend" // Backend service (downstream)
	wsCloseInitiatorRelayer wsCloseInitiator = "relayer" // This relayer (bridge)
)

// getWSDialTimeout returns the dial timeout for WebSocket connections.
// Uses the dial_timeout_seconds from the timeout profile if available.
func getWSDialTimeout(profile *TimeoutProfile) time.Duration {
	if profile != nil && profile.DialTimeoutSeconds > 0 {
		return time.Duration(profile.DialTimeoutSeconds) * time.Second
	}
	return defaultWSDialTimeout
}

// wsMessageSource represents the source of a WebSocket message.
type wsMessageSource string

const (
	wsMessageSourceBackend wsMessageSource = "backend"
	wsMessageSourceGateway wsMessageSource = "gateway"
)

// wsMessage represents a message in the WebSocket bridge.
type wsMessage struct {
	data        []byte
	source      wsMessageSource
	messageType int
}

// WebSocketBridge handles bidirectional WebSocket communication between
// a gateway client and a backend service.
type WebSocketBridge struct {
	logger         logging.Logger
	gatewayConn    *websocket.Conn
	backendConn    *websocket.Conn
	relayProcessor RelayProcessor
	publisher      transport.MinedRelayPublisher
	responseSigner *ResponseSigner
	relayPipeline  *RelayPipeline // Unified relay processing pipeline

	// Message channel for bridge communication
	msgChan chan wsMessage

	// Track latest request/response for pairing
	latestRequest  *servicetypes.RelayRequest
	latestResponse *servicetypes.RelayResponse
	latestMu       sync.RWMutex

	// Service and supplier info
	serviceID       string
	supplierAddress string
	arrivalHeight   int64
	computeUnits    uint64 // Compute units for this service

	// Relay counting for billing
	relayCount atomic.Uint64

	// Session expiration monitoring (global, shared across all connections)
	sessionMonitor   *SessionMonitor // Global session monitor
	sessionEndHeight int64           // Session end block height

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	closed   atomic.Bool
	wg       sync.WaitGroup
}

// WebSocketUpgrader upgrades HTTP connections to WebSocket.
var WebSocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Accept connections from any origin for cross-origin support
	CheckOrigin: func(r *http.Request) bool { return true },
	// Enable per-message compression (RFC 7692 - permessage-deflate)
	EnableCompression: true,
}

// NewWebSocketBridge creates a new WebSocket bridge.
// The dialTimeout is derived from the service's timeout profile.
func NewWebSocketBridge(
	logger logging.Logger,
	gatewayConn *websocket.Conn,
	backendURL string,
	serviceID string,
	supplierAddress string,
	arrivalHeight int64,
	relayProcessor RelayProcessor,
	publisher transport.MinedRelayPublisher,
	responseSigner *ResponseSigner,
	headers http.Header,
	sessionMonitor *SessionMonitor,
	relayPipeline *RelayPipeline,
	computeUnits uint64,
	dialTimeout time.Duration,
) (*WebSocketBridge, error) {
	ctx, cancelFn := context.WithCancel(context.Background())

	// Connect to backend WebSocket using dial timeout from profile
	backendConn, err := connectWebSocketBackend(backendURL, headers, dialTimeout)
	if err != nil {
		cancelFn()
		return nil, err
	}

	bridge := &WebSocketBridge{
		logger:           logger.With().Str(logging.FieldComponent, logging.ComponentWebsocketBridge).Str(logging.FieldServiceID, serviceID).Logger(),
		gatewayConn:      gatewayConn,
		backendConn:      backendConn,
		relayProcessor:   relayProcessor,
		publisher:        publisher,
		responseSigner:   responseSigner,
		relayPipeline:    relayPipeline,
		msgChan:          make(chan wsMessage, 2000),
		serviceID:        serviceID,
		supplierAddress:  supplierAddress,
		arrivalHeight:    arrivalHeight,
		computeUnits:     computeUnits,
		sessionMonitor:   sessionMonitor,
		sessionEndHeight: 0, // Will be set from first relay request
		ctx:              ctx,
		cancelFn:         cancelFn,
	}

	// Track connection
	wsConnectionsActive.WithLabelValues(serviceID).Inc()
	wsConnectionsTotal.WithLabelValues(serviceID).Inc()

	return bridge, nil
}

// connectWebSocketBackend establishes a WebSocket connection to the backend.
// The dialTimeout is derived from the service's timeout profile.
func connectWebSocketBackend(backendURL string, headers http.Header, dialTimeout time.Duration) (*websocket.Conn, error) {
	parsedURL, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}

	// Create dialer with compression and timeout from profile
	dialer := &websocket.Dialer{
		EnableCompression: true, // Enable per-message compression for backend connections
		HandshakeTimeout:  dialTimeout,
	}

	// Use TLS for wss:// scheme
	if parsedURL.Scheme == "wss" {
		dialer.TLSClientConfig = &tls.Config{}
	}

	conn, _, err := dialer.Dial(backendURL, headers)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// Run starts the WebSocket bridge message loop.
// This is a blocking call that runs until the bridge is closed.
func (b *WebSocketBridge) Run() {
	// Start connection read loops
	b.wg.Add(2)
	go logging.RecoverGoRoutine(b.logger, "websocket_read_gateway", func(ctx context.Context) {
		b.readLoop(b.gatewayConn, wsMessageSourceGateway)
	})(b.ctx)
	go logging.RecoverGoRoutine(b.logger, "websocket_read_backend", func(ctx context.Context) {
		b.readLoop(b.backendConn, wsMessageSourceBackend)
	})(b.ctx)

	// Start ping loops for keep-alive
	b.wg.Add(2)
	go logging.RecoverGoRoutine(b.logger, "websocket_ping_gateway", func(ctx context.Context) {
		b.pingLoop(b.gatewayConn, "gateway")
	})(b.ctx)
	go logging.RecoverGoRoutine(b.logger, "websocket_ping_backend", func(ctx context.Context) {
		b.pingLoop(b.backendConn, "backend")
	})(b.ctx)

	// Note: Session expiration monitoring happens via global SessionMonitor.
	// This bridge registers itself when session parameters are known.

	// Main message processing loop
	b.messageLoop()

	// Wait for all goroutines to finish
	b.wg.Wait()

	b.logger.Info().Msg("websocket bridge stopped")
}

// readLoop reads messages from a WebSocket connection.
func (b *WebSocketBridge) readLoop(conn *websocket.Conn, source wsMessageSource) {
	defer b.wg.Done()

	for {
		if b.closed.Load() {
			return
		}

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			// If we're shutting down, don't log expected errors from closing connections
			select {
			case <-b.ctx.Done():
				// Context cancelled - this is expected during bridge shutdown
				b.logger.Debug().
					Str(logging.FieldSource, string(source)).
					Msg("readLoop exiting due to shutdown")
				return
			default:
				// Parse and log close error details
				b.logCloseError(err, source)
			}
			// Extract close code from peer and propagate it to the other side
			// This enables proper session rollover signaling (e.g., 4000 SessionExpired from PATH → backend)
			closeCode, closeText := extractCloseInfo(err)
			if closeCode == 0 {
				// Not a close frame - use GoingAway as default
				closeCode = CloseGoingAway
				closeText = "peer disconnected"
			}
			_ = b.closeWithReason(closeCode, closeText, wsCloseInitiator(source))
			return
		}

		// Reset read deadline on each message (not just pongs)
		// This is critical for long-running subscriptions where the backend
		// sends frequent data messages but may not respond to pings
		if err := conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
			b.logger.Warn().Err(err).Str(logging.FieldSource, string(source)).Msg("failed to reset read deadline")
		}

		b.logger.Debug().
			Str(logging.FieldSource, string(source)).
			Int("message_size", len(data)).
			Msg("readLoop received message")

		select {
		case <-b.ctx.Done():
			return
		case b.msgChan <- wsMessage{data: data, source: source, messageType: messageType}:
		}
	}
}

// pingLoop sends periodic ping messages to keep the connection alive.
func (b *WebSocketBridge) pingLoop(conn *websocket.Conn, name string) {
	defer b.wg.Done()

	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()

	// Set initial read deadline
	if err := conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
		b.logger.Debug().Err(err).Str("connection", name).Msg("failed to set initial read deadline")
	}

	// Reset deadline on pong
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
				b.logger.Debug().
					Err(err).
					Str("connection", name).
					Msg("ping failed - connection may be dead")
				// Determine initiator based on which connection failed
				var initiator wsCloseInitiator
				switch name {
				case "gateway":
					initiator = wsCloseInitiatorClient
				case "backend":
					initiator = wsCloseInitiatorBackend
				default:
					initiator = wsCloseInitiatorRelayer
				}
				_ = b.closeWithReason(CloseGoingAway, "ping timeout", initiator)
				return
			}
		}
	}
}

// handleSessionExpiration is called by the global SessionMonitor when the session expires.
// This is invoked as a callback, not in a dedicated goroutine per bridge.
func (b *WebSocketBridge) handleSessionExpiration(sessionEndHeight, graceEndHeight, currentHeight int64) {
	// Verify this expiration applies to our session
	if b.sessionEndHeight != sessionEndHeight {
		b.logger.Debug().
			Int64("our_session_end", b.sessionEndHeight).
			Int64("expired_session_end", sessionEndHeight).
			Msg("session expiration for different session - ignoring")
		return
	}

	b.logger.Warn().
		Int64("current_height", currentHeight).
		Int64("session_end_height", sessionEndHeight).
		Int64("grace_end_height", graceEndHeight).
		Int64("blocks_over", currentHeight-graceEndHeight).
		Msg("session expired - closing websocket connection")

	// Send session expiration message to client
	if err := b.sendSessionExpirationMessage(); err != nil {
		b.logger.Warn().Err(err).Msg("failed to send session expiration message")
	}

	// Close the connection with session expired code
	_ = b.closeWithReason(CloseSessionExpired, "session expired", wsCloseInitiatorRelayer)
}

// messageLoop processes messages from both connections.
func (b *WebSocketBridge) messageLoop() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case msg := <-b.msgChan:
			switch msg.source {
			case wsMessageSourceGateway:
				b.handleGatewayMessage(msg)
			case wsMessageSourceBackend:
				b.handleBackendMessage(msg)
			}
		}
	}
}

// handleGatewayMessage handles messages from the gateway.
func (b *WebSocketBridge) handleGatewayMessage(msg wsMessage) {
	wsMessagesForwarded.WithLabelValues(b.serviceID, "gateway_to_backend").Inc()

	// Try to parse as RelayRequest
	relayReq := &servicetypes.RelayRequest{}
	if err := relayReq.Unmarshal(msg.data); err != nil {
		// Not a valid RelayRequest - forward raw data to backend
		b.forwardToBackend(msg)
		return
	}

	// Extract session parameters from first request and register with global monitor
	if b.sessionEndHeight == 0 && relayReq.Meta.SessionHeader != nil {
		b.sessionEndHeight = relayReq.Meta.SessionHeader.SessionEndBlockHeight
		b.logger.Info().
			Int64("session_end_height", b.sessionEndHeight).
			Msg("session parameters initialized from first relay request")

		// Register with global session monitor
		if b.sessionMonitor != nil {
			b.sessionMonitor.RegisterBridge(b, b.sessionEndHeight)
		}
	}

	// Validate and meter the relay if pipeline is available
	if b.relayPipeline != nil {
		// Build relay context for validation/metering
		relayCtx := &RelayContext{
			Request:            relayReq,
			ServiceID:          b.serviceID,
			SupplierAddress:    b.supplierAddress,
			SessionID:          relayReq.Meta.SessionHeader.SessionId,
			ComputeUnits:       b.computeUnits,
			ArrivalBlockHeight: b.arrivalHeight,
		}

		// Validate relay request (ring signature + session)
		if err := b.relayPipeline.ValidateRelay(b.ctx, relayCtx); err != nil {
			b.logger.Warn().
				Err(err).
				Str("session_id", relayCtx.SessionID).
				Msg("relay validation failed - closing connection")
			_ = b.closeWithReason(CloseValidationFailed, "relay validation failed", wsCloseInitiatorRelayer)
			return
		}

		// Meter relay (check stake before serving)
		allowed, meterErr := b.relayPipeline.MeterRelay(b.ctx, relayCtx)
		if meterErr != nil {
			// Fail-open: log warning but allow relay
			b.logger.Warn().
				Err(meterErr).
				Str("session_id", relayCtx.SessionID).
				Msg("relay metering error (fail-open: allowing relay)")
		} else if !allowed {
			// Stake limit exceeded - reject relay
			b.logger.Warn().
				Str("session_id", relayCtx.SessionID).
				Msg("relay rejected - stake limit exceeded")
			_ = b.closeWithReason(CloseStakeLimitExceeded, "stake limit exceeded", wsCloseInitiatorRelayer)
			return
		}
	}

	// Clear any previous request when new one arrives
	// This ensures each incoming request becomes the new latestRequest
	b.clearLatestRequest()

	// Store latest request for pairing with subsequent backend responses
	b.setLatestRequest(relayReq)

	b.logger.Debug().
		Int("payload_size", len(relayReq.Payload)).
		Str("payload_preview", string(relayReq.Payload[:min(50, len(relayReq.Payload))])).
		Msg("forwarding payload to backend")

	// Forward payload to backend as BinaryMessage (transport-agnostic opaque bytes)
	// The relayer doesn't inspect or care about the payload format (JSON, protobuf, etc.)
	if err := b.backendConn.WriteMessage(websocket.BinaryMessage, relayReq.Payload); err != nil {
		b.logger.Warn().Err(err).Msg("failed to forward to backend")
		_ = b.closeWithReason(CloseInternalError, "backend write failed", wsCloseInitiatorRelayer)
		return
	}

	b.logger.Debug().
		Int("bytes_sent", len(relayReq.Payload)).
		Str("sent_to_backend", string(relayReq.Payload[:min(50, len(relayReq.Payload))])).
		Msg("successfully forwarded payload to backend")
}

// handleBackendMessage handles messages from the backend.
// Each backend message is billed as part of a relay.
func (b *WebSocketBridge) handleBackendMessage(msg wsMessage) {
	wsMessagesForwarded.WithLabelValues(b.serviceID, "backend_to_gateway").Inc()

	b.logger.Debug().
		Int("message_size", len(msg.data)).
		Str("backend_response", string(msg.data[:min(50, len(msg.data))])).
		Msg("handleBackendMessage called")

	latestReq := b.getLatestRequest()
	if latestReq == nil {
		b.logger.Debug().Msg("no latestRequest - forwarding raw to gateway")
		// No request yet - just forward raw data
		if err := b.gatewayConn.WriteMessage(msg.messageType, msg.data); err != nil {
			b.logger.Warn().Err(err).Msg("failed to forward to client (PATH)")
			_ = b.closeWithReason(CloseInternalError, "client write failed", wsCloseInitiatorRelayer)
		}
		return
	}

	b.logger.Debug().Msg("latestRequest found - building signed response")

	// Build and sign RelayResponse
	// responseSigner is guaranteed to be non-nil (validated early in WebSocketHandler)
	var respBytes []byte
	var relayResp *servicetypes.RelayResponse

	// Build signed WebSocket response (raw payload, no HTTP wrapping)
	var signErr error
	relayResp, respBytes, signErr = b.responseSigner.BuildAndSignWebSocketRelayResponse(
		latestReq,
		msg.data, // Raw WebSocket payload (e.g., JSON-RPC response)
	)
	if signErr != nil {
		b.logger.Warn().Err(signErr).Msg("failed to sign websocket response")
		// Fall back to unsigned on signing error only (not nil signer)
		relayResp = &servicetypes.RelayResponse{
			Meta: servicetypes.RelayResponseMetadata{
				SessionHeader: latestReq.Meta.SessionHeader,
			},
			Payload: msg.data,
		}
		respBytes, _ = relayResp.Marshal()
	}

	// Store latest response for relay emission
	b.setLatestResponse(relayResp)

	// Forward signed RelayResponse to gateway as BinaryMessage (protobuf format)
	// Always use BinaryMessage for RelayResponse (protobuf is binary data)
	if err := b.gatewayConn.WriteMessage(websocket.BinaryMessage, respBytes); err != nil {
		b.logger.Warn().Err(err).Msg("failed to forward signed response to client (PATH)")
		_ = b.closeWithReason(CloseInternalError, "client write failed", wsCloseInitiatorRelayer)
		return
	}

	b.logger.Debug().Msg("forwarded signed response to gateway successfully")

	// Emit relay for this request/response pair
	b.emitRelay(latestReq, relayResp, msg.data)

	b.logger.Debug().Msg("handleBackendMessage completed - latestRequest preserved for next message")

	// NOTE: latestRequest is NOT cleared here - it's reused for subsequent backend messages
	// This allows subscription-based APIs (eth_subscribe) to bill for each update
	// The request will be cleared when the client sends a new request (in handleGatewayMessage)
}

// forwardToBackend forwards a raw message to the backend.
func (b *WebSocketBridge) forwardToBackend(msg wsMessage) {
	if err := b.backendConn.WriteMessage(msg.messageType, msg.data); err != nil {
		b.logger.Warn().Err(err).Msg("failed to forward raw message to backend")
		_ = b.closeWithReason(CloseInternalError, "backend write failed", wsCloseInitiatorRelayer)
	}
}

// emitRelay creates and publishes a mined relay for a request/response pair.
// This is the billing mechanism - each req/resp pair becomes a relay.
func (b *WebSocketBridge) emitRelay(req *servicetypes.RelayRequest, resp *servicetypes.RelayResponse, respPayload []byte) {
	if b.publisher == nil {
		return
	}

	// Increment relay count for this connection
	count := b.relayCount.Add(1)

	// Get supplier address from request metadata or fallback to bridge config
	supplierAddr := b.supplierAddress
	if req.Meta.SupplierOperatorAddress != "" {
		supplierAddr = req.Meta.SupplierOperatorAddress
	}

	// Extract session context for logging
	sessionCtx := logging.SessionContextFromRelayRequest(req)
	if sessionCtx.Supplier == "" {
		sessionCtx.Supplier = supplierAddr
	}
	if sessionCtx.ServiceID == "" {
		sessionCtx.ServiceID = b.serviceID
	}

	if supplierAddr == "" {
		logging.WithSessionContext(b.logger.Warn(), sessionCtx).
			Msg("no supplier address available for websocket relay")
		return
	}

	// Marshal the original request body for relay processing
	reqBytes, err := req.Marshal()
	if err != nil {
		logging.WithSessionContext(b.logger.Warn(), sessionCtx).
			Err(err).
			Msg("failed to marshal relay request")
		return
	}

	// Use RelayProcessor if available for proper relay construction
	if b.relayProcessor != nil {
		msg, procErr := b.relayProcessor.ProcessRelay(
			b.ctx,
			reqBytes,
			respPayload,
			supplierAddr,
			b.serviceID,
			b.arrivalHeight,
		)
		if procErr != nil {
			logging.WithSessionContext(b.logger.Warn(), sessionCtx).
				Err(procErr).
				Msg("failed to process websocket relay")
			return
		}

		if msg != nil {
			if pubErr := b.publisher.Publish(b.ctx, msg); pubErr != nil {
				logging.WithSessionContext(b.logger.Warn(), sessionCtx).
					Err(pubErr).
					Msg("failed to publish websocket relay")
				return
			}
			wsRelaysEmitted.WithLabelValues(b.serviceID).Inc()
			logging.WithSessionContext(b.logger.Debug(), sessionCtx).
				Uint64("relay_count", count).
				Msg("websocket relay published")
		}
		return
	}

	// Fallback: Create basic relay message without full processing
	relay := &servicetypes.Relay{
		Req: req,
		Res: resp,
	}
	relayBytes, err := relay.Marshal()
	if err != nil {
		logging.WithSessionContext(b.logger.Warn(), sessionCtx).
			Err(err).
			Msg("failed to marshal relay")
		return
	}

	msg := &transport.MinedRelayMessage{
		RelayHash:               nil, // Not calculated in fallback mode
		RelayBytes:              relayBytes,
		ComputeUnitsPerRelay:    1,
		SessionId:               "",
		SessionEndHeight:        0,
		SupplierOperatorAddress: supplierAddr,
		ServiceId:               b.serviceID,
		ApplicationAddress:      "",
		ArrivalBlockHeight:      b.arrivalHeight,
	}

	if req.Meta.SessionHeader != nil {
		msg.SessionId = req.Meta.SessionHeader.SessionId
		msg.SessionEndHeight = req.Meta.SessionHeader.SessionEndBlockHeight
		msg.ApplicationAddress = req.Meta.SessionHeader.ApplicationAddress
	}

	msg.SetPublishedAt()

	if pubErr := b.publisher.Publish(b.ctx, msg); pubErr != nil {
		logging.WithSessionContext(b.logger.Warn(), sessionCtx).
			Err(pubErr).
			Msg("failed to publish websocket relay")
		return
	}

	wsRelaysEmitted.WithLabelValues(b.serviceID).Inc()
	logging.WithSessionContext(b.logger.Debug(), sessionCtx).
		Uint64("relay_count", count).
		Msg("websocket relay published (fallback)")
}

// sendSessionExpirationMessage sends a signed error response to the client
// informing them that the session has expired.
func (b *WebSocketBridge) sendSessionExpirationMessage() error {
	// Get the latest request to extract session header
	b.latestMu.RLock()
	latestReq := b.latestRequest
	b.latestMu.RUnlock()

	if latestReq == nil || latestReq.Meta.SessionHeader == nil {
		return nil // No session to expire
	}

	supplierAddr := b.supplierAddress
	if latestReq.Meta.SupplierOperatorAddress != "" {
		supplierAddr = latestReq.Meta.SupplierOperatorAddress
	}

	// Build error response
	_, respBytes, err := b.responseSigner.BuildErrorRelayResponse(
		latestReq.Meta.SessionHeader,
		supplierAddr,
		410, // HTTP 410 Gone
		"session expired",
	)
	if err != nil {
		return err
	}

	// Send to gateway as BinaryMessage (protobuf format)
	if err := b.gatewayConn.WriteMessage(websocket.BinaryMessage, respBytes); err != nil {
		return err
	}

	b.logger.Info().
		Str("session_id", latestReq.Meta.SessionHeader.SessionId).
		Msg("sent session expiration message to client")

	return nil
}

// Unused - deprecated method, use emitRelay directly
// tryEmitRelay is deprecated - use emitRelay directly.
// Kept for compatibility but does nothing.
// func (b *WebSocketBridge) tryEmitRelay() {
// 	// No-op: relay emission is now done in handleBackendMessage
// }

// setLatestRequest stores the latest request.
func (b *WebSocketBridge) setLatestRequest(req *servicetypes.RelayRequest) {
	b.latestMu.Lock()
	defer b.latestMu.Unlock()
	b.latestRequest = req
}

// getLatestRequest retrieves the latest request.
func (b *WebSocketBridge) getLatestRequest() *servicetypes.RelayRequest {
	b.latestMu.RLock()
	defer b.latestMu.RUnlock()
	return b.latestRequest
}

// setLatestResponse stores the latest response.
func (b *WebSocketBridge) setLatestResponse(resp *servicetypes.RelayResponse) {
	b.latestMu.Lock()
	defer b.latestMu.Unlock()
	b.latestResponse = resp
}

// Unused - reserved for future use
// getLatestResponse retrieves the latest response.
// func (b *WebSocketBridge) getLatestResponse() *servicetypes.RelayResponse {
// 	b.latestMu.RLock()
// 	defer b.latestMu.RUnlock()
// 	return b.latestResponse
// }

// clearLatestRequest clears the latest request after emitting a relay.
// This ensures each backend response is paired with a unique gateway request.
func (b *WebSocketBridge) clearLatestRequest() {
	b.latestMu.Lock()
	defer b.latestMu.Unlock()
	b.latestRequest = nil
}

// logCloseError parses and logs WebSocket close errors with proper RFC 6455 codes.
func (b *WebSocketBridge) logCloseError(err error, source wsMessageSource) {
	// Try to extract close code from error
	closeErr, ok := err.(*websocket.CloseError)
	if ok {
		// Log with structured close code info
		b.logger.Debug().
			Int("close_code", closeErr.Code).
			Str("close_code_name", closeCodeName(closeErr.Code)).
			Str("close_text", closeErr.Text).
			Str("initiated_by", string(source)).
			Msg("websocket connection closed by peer")
		return
	}

	// Not a close error - log as unexpected error
	b.logger.Warn().
		Err(err).
		Str("initiated_by", string(source)).
		Msg("websocket read error (not a close frame)")
}

// extractCloseInfo extracts close code and text from a WebSocket close error.
// Returns 0 and empty string if the error is not a close error.
// This is used to propagate close codes bidirectionally through the bridge.
func extractCloseInfo(err error) (int, string) {
	closeErr, ok := err.(*websocket.CloseError)
	if ok {
		return closeErr.Code, closeErr.Text
	}
	return 0, ""
}

// mapToRFCCloseCode converts custom Pocket close codes to standard RFC 6455 codes.
// This is used when closing the backend connection - backends don't understand Pocket codes.
// Pocket codes (4000-4999) are mapped to appropriate RFC codes for clean disconnection.
func mapToRFCCloseCode(code int) (int, string) {
	// Standard RFC codes (1000-1015) pass through unchanged
	if code >= 1000 && code <= 1015 {
		return code, ""
	}

	// Map custom Pocket codes to RFC equivalents for backend
	switch code {
	case CloseSessionExpired: // 4000 - session ended, clean shutdown
		return CloseGoingAway, "session ended"
	case CloseValidationFailed: // 4001 - protocol/validation error
		return ClosePolicyViolation, "request rejected"
	case CloseStakeLimitExceeded: // 4002 - rate limiting
		return CloseTryAgainLater, "rate limited"
	case CloseBackendConnectionFailed: // 4003 - already a backend issue
		return CloseInternalError, "connection failed"
	default:
		// Unknown custom code - use generic going away
		return CloseGoingAway, "connection closing"
	}
}

// closeWithReason closes the bridge with a specific close code and reason.
// Close codes are handled differently for each connection:
// - Gateway (PATH): Receives the original code (including custom Pocket codes like 4000)
// - Backend: Receives RFC 6455 standard codes only (custom codes are mapped)
func (b *WebSocketBridge) closeWithReason(code int, reason string, initiator wsCloseInitiator) error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil // Already closed
	}

	// Unregister from global session monitor
	if b.sessionMonitor != nil {
		b.sessionMonitor.UnregisterBridge(b)
	}

	// Decrement active connections metric
	wsConnectionsActive.WithLabelValues(b.serviceID).Dec()

	// Log final stats for this connection
	relayCount := b.relayCount.Load()
	b.logger.Debug().
		Int("close_code", code).
		Str("close_code_name", closeCodeName(code)).
		Str("close_reason", reason).
		Str("initiated_by", string(initiator)).
		Uint64("relays_emitted", relayCount).
		Msg("websocket bridge closing")

	b.cancelFn()

	deadline := time.Now().Add(wsWriteWait)

	// Send close to gateway (PATH) with original code - PATH understands Pocket codes
	gatewayCloseMsg := websocket.FormatCloseMessage(code, reason)
	if err := b.gatewayConn.WriteControl(websocket.CloseMessage, gatewayCloseMsg, deadline); err != nil {
		b.logger.Debug().Err(err).Msg("failed to send close to client (PATH)")
	}

	// Send close to backend with RFC-compliant code - backends don't understand Pocket codes
	backendCode, backendReason := mapToRFCCloseCode(code)
	if backendReason == "" {
		backendReason = reason // Use original reason if no mapping override
	}
	backendCloseMsg := websocket.FormatCloseMessage(backendCode, backendReason)
	if err := b.backendConn.WriteControl(websocket.CloseMessage, backendCloseMsg, deadline); err != nil {
		b.logger.Debug().Err(err).Msg("failed to send close to backend")
	}

	// Give connections time to receive close message
	time.Sleep(100 * time.Millisecond)

	_ = b.gatewayConn.Close()
	_ = b.backendConn.Close()

	b.logger.Debug().Msg("websocket bridge closed")
	return nil
}

// Close shuts down the WebSocket bridge with normal closure.
func (b *WebSocketBridge) Close() error {
	return b.closeWithReason(CloseNormalClosure, "bridge closing", wsCloseInitiatorRelayer)
}

// WebSocketHandler returns an HTTP handler for WebSocket upgrades.
// This should be used when detecting WebSocket upgrade requests.
func (p *ProxyServer) WebSocketHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serviceID := p.extractServiceID(r)
		if serviceID == "" {
			p.sendError(w, http.StatusBadRequest, "missing service ID")
			return
		}

		svcConfig, ok := p.config.Services[serviceID]
		if !ok {
			p.sendError(w, http.StatusNotFound, "unknown service")
			return
		}

		// Validate critical dependencies are configured - fail fast before upgrade
		if p.responseSigner == nil {
			p.logger.Error().Str(logging.FieldServiceID, serviceID).Msg("response signer not configured")
			p.sendError(w, http.StatusInternalServerError, "relayer not properly configured")
			return
		}

		if p.relayProcessor == nil {
			p.logger.Error().Str(logging.FieldServiceID, serviceID).Msg("relay processor not configured")
			p.sendError(w, http.StatusInternalServerError, "relayer not properly configured")
			return
		}

		if p.publisher == nil {
			p.logger.Error().Str(logging.FieldServiceID, serviceID).Msg("publisher not configured")
			p.sendError(w, http.StatusInternalServerError, "relayer not properly configured")
			return
		}

		// Validate and log WebSocket handshake (permissive - never rejects)
		// - PATH v2: Attempts signature verification, logs WARN if fails
		// - PATH v1: Logs INFO about legacy handshake
		p.validateAndLogWebSocketHandshake(r, serviceID)

		// Upgrade HTTP connection to WebSocket
		gatewayConn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			p.logger.Warn().Err(err).Msg("failed to upgrade to websocket")
			return
		}

		// Get WebSocket backend URL
		var backendURL string
		var configHeaders map[string]string
		if backend, ok := svcConfig.Backends["websocket"]; ok {
			backendURL = backend.URL
			configHeaders = backend.Headers
		} else {
			http.Error(w, "WebSocket backend not configured for this service", http.StatusServiceUnavailable)
			return
		}

		// Build headers
		headers := make(http.Header)
		for k, v := range configHeaders {
			headers.Set(k, v)
		}

		// Extract supplier address from handshake header (sent by PATH v2 protocol)
		// For PATH v1, this will be empty and should be extracted from first RelayRequest
		supplierAddress := r.Header.Get(HeaderPocketSupplierAddress)

		// Add Pocket context headers for backend visibility
		headers.Set(HeaderPocketSupplier, supplierAddress)
		headers.Set(HeaderPocketService, serviceID)
		// Note: Application address is not known until first relay request arrives on the WebSocket

		arrivalHeight := p.currentBlockHeight.Load()

		// TODO: Get actual compute units from service config or relay processor
		// For now, use default value of 1 (will be refined in future PR)
		computeUnits := uint64(1)

		// Get dial timeout from service's timeout profile
		dialTimeout := getWSDialTimeout(p.config.GetServiceTimeoutProfile(serviceID))

		// Create and run bridge
		// Session end height will be set when the first relay request arrives
		// Note: supplierAddress may be empty for PATH v1 - bridge will extract from first RelayRequest
		bridge, err := NewWebSocketBridge(
			p.logger,
			gatewayConn,
			backendURL,
			serviceID,
			supplierAddress,
			arrivalHeight,
			p.relayProcessor,
			p.publisher,
			p.responseSigner,
			headers,
			p.sessionMonitor, // Global session monitor (shared across all connections)
			p.relayPipeline,  // Unified relay pipeline for validation/metering/signing
			computeUnits,
			dialTimeout, // Dial timeout from service's timeout profile
		)
		if err != nil {
			p.logger.Warn().Err(err).Msg("failed to create websocket bridge")
			_ = gatewayConn.Close()
			return
		}

		// Run bridge (blocking)
		bridge.Run()
	}
}

// IsWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func IsWebSocketUpgrade(r *http.Request) bool {
	return websocket.IsWebSocketUpgrade(r)
}

// WebSocket handshake header constants (matching PATH's request/parser.go)
const (
	// Headers sent by PATH during WebSocket handshake for validation
	HeaderPocketSessionID          = "Pocket-Session-Id"
	HeaderPocketSessionStartHeight = "Pocket-Session-Start-Height"
	HeaderPocketSessionEndHeight   = "Pocket-Session-End-Height"
	HeaderPocketSupplierAddress    = "Pocket-Supplier-Address"
	HeaderPocketSignature          = "Pocket-Signature"
	HeaderPocketAppAddress         = "App-Address"
	HeaderTargetServiceID          = "Target-Service-Id"
	HeaderRpcType                  = "Rpc-Type"
)

// validateAndLogWebSocketHandshake validates and logs WebSocket handshake.
// This function is permissive - it never rejects connections, only logs validation results.
// - PATH v2: Attempts signature verification, logs WARN if fails (but continues)
// - PATH v1: Logs INFO about legacy handshake (no validation headers)
func (p *ProxyServer) validateAndLogWebSocketHandshake(r *http.Request, serviceID string) {
	// Extract handshake headers
	sessionID := r.Header.Get(HeaderPocketSessionID)
	sessionStartHeight := r.Header.Get(HeaderPocketSessionStartHeight)
	sessionEndHeight := r.Header.Get(HeaderPocketSessionEndHeight)
	supplierAddress := r.Header.Get(HeaderPocketSupplierAddress)
	appAddress := r.Header.Get(HeaderPocketAppAddress)
	signature := r.Header.Get(HeaderPocketSignature)
	rpcType := r.Header.Get(HeaderRpcType)

	// Check if we have PATH v2 validation headers
	hasV2Headers := sessionID != "" && supplierAddress != "" && signature != ""

	if !hasV2Headers {
		// PATH v1 - no validation headers present
		p.logger.Info().
			Str(logging.FieldServiceID, serviceID).
			Str("remote_addr", r.RemoteAddr).
			Str("app_address", appAddress).
			Str("rpc_type", rpcType).
			Msg("websocket handshake from PATH v1 (no validation headers)")
		return
	}

	// PATH v2 - validation headers present, attempt verification
	p.logger.Info().
		Str(logging.FieldServiceID, serviceID).
		Str("remote_addr", r.RemoteAddr).
		Str("session_id", sessionID).
		Str("session_start_height", sessionStartHeight).
		Str("session_end_height", sessionEndHeight).
		Str("supplier_address", supplierAddress).
		Str("app_address", appAddress).
		Str("rpc_type", rpcType).
		Bool("has_signature", true).
		Int("signature_length", len(signature)).
		Msg("websocket handshake from PATH v2 (with validation headers)")

	// TODO(PATH-v2): Implement full handshake signature verification
	// The signature should be verified against a reconstructed message containing:
	// - Session ID, session start/end heights
	// - Supplier address, application address
	// - Service ID, RPC type
	//
	// For now, we accept all handshakes permissively until PATH v2 protocol is finalized.
	// When ready, we should:
	// 1. Reconstruct the signed message structure (matching PATH's signing logic)
	// 2. Verify signature using application's public key (from session or blockchain)
	// 3. Log WARN if verification fails (but still allow connection for backward compatibility)

	if p.validator != nil {
		// Placeholder for future signature verification
		// When PATH v2 is stable, uncomment and implement:
		/*
			verifyErr := p.verifyWebSocketHandshakeSignature(sessionID, sessionStartHeight, sessionEndHeight,
				supplierAddress, appAddress, serviceID, rpcType, signature)
			if verifyErr != nil {
				p.logger.Warn().
					Err(verifyErr).
					Str(logging.FieldServiceID, serviceID).
					Str("session_id", sessionID).
					Str("supplier_address", supplierAddress).
					Msg("websocket handshake signature verification failed (accepting permissively)")
				return
			}
			p.logger.Debug().
				Str(logging.FieldServiceID, serviceID).
				Str("session_id", sessionID).
				Msg("websocket handshake signature verified successfully")
		*/

		// For now, just log that verification is not yet implemented
		p.logger.Debug().
			Str(logging.FieldServiceID, serviceID).
			Str("session_id", sessionID).
			Msg("websocket handshake signature verification not yet implemented (accepting permissively)")
	}
}
