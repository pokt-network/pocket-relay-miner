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

const (
	// wsWriteWait is the time allowed to write a message to the peer.
	wsWriteWait = 10 * time.Second

	// wsPongWait is the time allowed to wait for the next pong message.
	wsPongWait = 30 * time.Second

	// wsPingPeriod is the send pings to peer with this period. Must be less than pongWait.
	wsPingPeriod = (wsPongWait * 9) / 10
)

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
}

// NewWebSocketBridge creates a new WebSocket bridge.
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
) (*WebSocketBridge, error) {
	ctx, cancelFn := context.WithCancel(context.Background())

	// Connect to backend WebSocket
	backendConn, err := connectWebSocketBackend(backendURL, headers)
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
		msgChan:          make(chan wsMessage, 100),
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
func connectWebSocketBackend(backendURL string, headers http.Header) (*websocket.Conn, error) {
	parsedURL, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}

	// Use TLS for wss:// scheme
	var dialer *websocket.Dialer
	if parsedURL.Scheme == "wss" {
		dialer = &websocket.Dialer{TLSClientConfig: &tls.Config{}}
	} else {
		dialer = websocket.DefaultDialer
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
				// Real error - log it
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					b.logger.Warn().
						Err(err).
						Str(logging.FieldSource, string(source)).
						Msg("websocket read error")
				}
			}
			b.logger.Debug().
				Str(logging.FieldSource, string(source)).
				Err(err).
				Msg("readLoop calling Close() due to read error")
			_ = b.Close()
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
					Msg("failed to send ping")
				_ = b.Close()
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
		Msg("SESSION EXPIRED - closing WebSocket connection")

	// Send session expiration message to client
	if err := b.sendSessionExpirationMessage(); err != nil {
		b.logger.Warn().Err(err).Msg("failed to send session expiration message")
	} else {
		b.logger.Info().Msg("session expiration message sent to client")
	}

	// Close the connection
	_ = b.Close()
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
			_ = b.Close()
			return
		}

		// Meter relay (check stake before serving)
		allowed, overServiced, meterErr := b.relayPipeline.MeterRelay(b.ctx, relayCtx)
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
				Bool("over_serviced", overServiced).
				Msg("relay rejected - stake limit exceeded")
			_ = b.Close()
			return
		}

		if overServiced {
			b.logger.Warn().
				Str("session_id", relayCtx.SessionID).
				Msg("relay over-serviced (will be served but not mined)")
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
		b.logger.Debug().Msg("handleGatewayMessage calling Close() - failed to forward to backend")
		_ = b.Close()
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
			b.logger.Warn().Err(err).Msg("failed to forward to gateway")
			b.logger.Debug().Msg("handleBackendMessage calling Close() - failed forward (no request)")
			_ = b.Close()
		}
		return
	}

	b.logger.Debug().Msg("latestRequest found - building signed response")

	// Build and sign RelayResponse
	var respBytes []byte
	var relayResp *servicetypes.RelayResponse

	if b.responseSigner != nil {
		// Build signed WebSocket response (raw payload, no HTTP wrapping)
		var signErr error
		relayResp, respBytes, signErr = b.responseSigner.BuildAndSignWebSocketRelayResponse(
			latestReq,
			msg.data, // Raw WebSocket payload (e.g., JSON-RPC response)
		)
		if signErr != nil {
			b.logger.Warn().Err(signErr).Msg("failed to sign websocket response")
			// Fall back to unsigned
			relayResp = &servicetypes.RelayResponse{
				Meta: servicetypes.RelayResponseMetadata{
					SessionHeader: latestReq.Meta.SessionHeader,
				},
				Payload: msg.data,
			}
			respBytes, _ = relayResp.Marshal()
		}
	} else {
		// No signer - create unsigned response
		relayResp = &servicetypes.RelayResponse{
			Meta: servicetypes.RelayResponseMetadata{
				SessionHeader: latestReq.Meta.SessionHeader,
			},
			Payload: msg.data,
		}
		var marshalErr error
		respBytes, marshalErr = relayResp.Marshal()
		if marshalErr != nil {
			b.logger.Warn().Err(marshalErr).Msg("failed to marshal response")
			respBytes = msg.data
		}
	}

	// Store latest response for relay emission
	b.setLatestResponse(relayResp)

	// Forward signed RelayResponse to gateway as BinaryMessage (protobuf format)
	// Always use BinaryMessage for RelayResponse (protobuf is binary data)
	if err := b.gatewayConn.WriteMessage(websocket.BinaryMessage, respBytes); err != nil {
		b.logger.Warn().Err(err).Msg("failed to forward to gateway")
		b.logger.Debug().Msg("handleBackendMessage calling Close() - failed forward (after signing)")
		_ = b.Close()
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
		_ = b.Close()
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

// Close shuts down the WebSocket bridge.
func (b *WebSocketBridge) Close() error {
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
		Uint64("relays_emitted", relayCount).
		Msg("Close() called")
	if relayCount > 0 {
		b.logger.Info().
			Uint64("relays_emitted", relayCount).
			Msg("websocket bridge closing with relays emitted")
	}

	b.cancelFn()

	// Send close messages (best-effort, errors are logged but not propagated)
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bridge closing")
	deadline := time.Now().Add(wsWriteWait)

	if err := b.gatewayConn.WriteControl(websocket.CloseMessage, closeMsg, deadline); err != nil {
		b.logger.Debug().Err(err).Msg("failed to send close to gateway")
	}
	if err := b.backendConn.WriteControl(websocket.CloseMessage, closeMsg, deadline); err != nil {
		b.logger.Debug().Err(err).Msg("failed to send close to backend")
	}

	// Give connections time to receive close message
	time.Sleep(100 * time.Millisecond)

	_ = b.gatewayConn.Close()
	_ = b.backendConn.Close()

	b.logger.Debug().Msg("websocket bridge closed")
	return nil
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

		arrivalHeight := p.currentBlockHeight.Load()

		// TODO: Get actual compute units from service config or relay processor
		// For now, use default value of 1 (will be refined in future PR)
		computeUnits := uint64(1)

		// Create and run bridge
		// Session end height will be set when the first relay request arrives
		bridge, err := NewWebSocketBridge(
			p.logger,
			gatewayConn,
			backendURL,
			serviceID,
			p.supplierAddress,
			arrivalHeight,
			p.relayProcessor,
			p.publisher,
			p.responseSigner,
			headers,
			p.sessionMonitor, // Global session monitor (shared across all connections)
			p.relayPipeline,  // Unified relay pipeline for validation/metering/signing
			computeUnits,
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
