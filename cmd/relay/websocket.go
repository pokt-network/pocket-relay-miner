package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// runWebSocketMode sends WebSocket relay requests to the relayer.
func RunWebSocketMode(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient) error {
	// Build payload (raw JSON for WebSocket - no HTTP wrapping)
	payloadBz, err := buildWebSocketPayload()
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	// Diagnostic mode: single request with detailed output
	if !RelayLoadTest {
		return runWebSocketDiagnostic(ctx, logger, relayClient, payloadBz)
	}

	// Load test mode: concurrent requests with metrics
	return runWebSocketLoadTest(ctx, logger, relayClient, payloadBz)
}

// buildWebSocketPayload creates a raw JSON-RPC payload for WebSocket relays.
// Unlike HTTP relays, WebSocket payloads are NOT wrapped in POKTHTTPRequest -
// they are sent as raw JSON to match WebSocket protocol expectations.
func buildWebSocketPayload() ([]byte, error) {
	var jsonPayload []byte
	var err error

	if RelayPayloadJSON != "" {
		// Use custom payload
		jsonPayload = []byte(RelayPayloadJSON)
	} else {
		// Default: eth_blockNumber request
		payload := map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_blockNumber",
			"params":  []interface{}{},
			"id":      1,
		}

		// Serialize to JSON
		jsonPayload, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal JSON payload: %w", err)
		}
	}

	return jsonPayload, nil
}

// runWebSocketDiagnostic sends a single WebSocket relay request with detailed output.
func runWebSocketDiagnostic(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Create sendFunc that connects and sends via WebSocket
	sendFunc := func(ctx context.Context, relayRequestBz []byte) ([]byte, error) {
		// Connect to WebSocket
		conn, err := connectWebSocket(RelayRelayerURL, RelayServiceID, RelaySupplierAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to WebSocket: %w", err)
		}
		defer func() {
			// Send close message for graceful shutdown
			closeMessage := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			_ = conn.WriteMessage(websocket.CloseMessage, closeMessage)
			_ = conn.Close()
		}()

		// Send relay request
		if err := conn.WriteMessage(websocket.BinaryMessage, relayRequestBz); err != nil {
			return nil, fmt.Errorf("failed to send relay request: %w", err)
		}

		// Receive relay response
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		messageType, responseData, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("failed to read relay response: %w", err)
		}

		if messageType != websocket.BinaryMessage {
			return nil, fmt.Errorf("unexpected message type: %d (expected binary)", messageType)
		}

		return responseData, nil
	}

	// Use shared build/send/verify logic
	result := BuildAndSendRelay(ctx, logger, relayClient, payloadBz, sendFunc)

	// Display results using shared formatter
	DisplayDiagnosticResult(relayClient, result)

	// Return error if relay failed
	if !result.Success {
		return result.Error
	}

	return nil
}

// runWebSocketLoadTest sends concurrent WebSocket relay requests with performance metrics.
// Uses a connection pool to avoid overhead of creating new connections for each request.
func runWebSocketLoadTest(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Build relay request once (reuse across requests)
	logger.Info().Msg("building relay request template for load test")
	_, relayRequestBz, err := relayClient.BuildRelayRequest(ctx, RelayServiceID, RelaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("failed to build relay request template: %w", err)
	}

	// Create connection pool as a buffered channel (thread-safe queue)
	// Workers will pop a connection, use it exclusively, then push it back
	connPool := make(chan *websocket.Conn, RelayConcurrency)
	for i := 0; i < RelayConcurrency; i++ {
		conn, err := connectWebSocket(RelayRelayerURL, RelayServiceID, RelaySupplierAddr)
		if err != nil {
			// Close any connections we already opened
			close(connPool)
			for c := range connPool {
				_ = c.Close()
			}
			return fmt.Errorf("failed to create connection pool: %w", err)
		}
		// Set ping/pong handlers to keep connections alive
		conn.SetPongHandler(func(string) error { return nil })
		connPool <- conn // Push to queue
	}
	defer func() {
		close(connPool)
		for conn := range connPool {
			_ = conn.Close()
		}
	}()

	// Create metrics collector
	metrics := NewRelayMetrics()

	// Worker pool pattern with semaphore
	semaphore := make(chan struct{}, RelayConcurrency)
	var wg sync.WaitGroup

	// Create rate limiter if RPS targeting is enabled
	rateLimiter := NewRateLimiter(RelayRPS)
	if rateLimiter != nil {
		defer rateLimiter.Stop()
	}

	logger.Info().
		Int("count", RelayCount).
		Int("concurrency", RelayConcurrency).
		Int("connection_pool_size", RelayConcurrency).
		Int("rps", RelayRPS).
		Msg("starting WebSocket load test with connection pool")

	metrics.Start()

	// Spawn workers
	for i := 0; i < RelayCount; i++ {
		// Wait for rate limiter if enabled (pace request launches)
		WaitForRateLimit(rateLimiter)

		wg.Add(1)
		semaphore <- struct{}{} // Acquire slot

		go func(reqNum int) {
			defer wg.Done()
			defer func() { <-semaphore }() // Release slot

			// Pop a connection from the pool (blocking until one is available)
			conn := <-connPool
			defer func() { connPool <- conn }() // Push back when done

			// Send relay with timeout
			requestCtx, cancel := context.WithTimeout(ctx, time.Duration(RelayTimeout)*time.Second)
			defer cancel()

			start := time.Now()
			relayResponseBz, err := sendWebSocketRelayOnConnection(requestCtx, conn, relayRequestBz)
			latencyMs := float64(time.Since(start).Microseconds()) / 1000.0

			if err != nil {
				metrics.RecordError(err)
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("WebSocket relay request failed (network error)")
				return
			}

			// Verify relay response signature
			relayResponse, err := relayClient.VerifyRelayResponse(requestCtx, RelaySupplierAddr, relayResponseBz)
			if err != nil {
				metrics.RecordError(fmt.Errorf("signature verification failed: %w", err))
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("WebSocket relay request failed (invalid signature)")
				return
			}

			// Check for JSON-RPC errors in the payload
			if err := CheckRelayResponseError(relayResponse); err != nil {
				metrics.RecordError(err)
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("WebSocket relay request failed (JSON-RPC error)")
				return
			}

			// Success: valid signature + no JSON-RPC error
			metrics.RecordSuccess(latencyMs)
			logger.Debug().
				Int("request_num", reqNum).
				Float64("latency_ms", latencyMs).
				Msg("WebSocket relay request succeeded")
		}(i)
	}

	// Wait for all workers to finish
	wg.Wait()
	metrics.End()

	// Display results
	fmt.Println(metrics.GetSummary())

	return nil
}

// WebSocket dialer with compression enabled (RFC 7692 - permessage-deflate)
// BEST PRACTICE: Enable compression for WebSocket connections to reduce bandwidth
var wsDialer = &websocket.Dialer{
	EnableCompression: true, // RFC 7692 permessage-deflate
	HandshakeTimeout:  10 * time.Second,
}

// connectWebSocket establishes a WebSocket connection to the relayer.
//
// BEST PRACTICE: This demonstrates proper WebSocket relay connection:
// 1. Pocket-Service-Id: Required - identifies the service being consumed
// 2. Pocket-Supplier-Address: Optional - specifies preferred supplier
// 3. Rpc-Type: 2 - Tells relayer this is a WebSocket connection
// 4. EnableCompression: true - Negotiate permessage-deflate (RFC 7692)
func connectWebSocket(relayerURL, serviceID, supplierAddr string) (*websocket.Conn, error) {
	// Parse URL and convert to WebSocket scheme
	parsedURL, err := url.Parse(relayerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid relayer URL: %w", err)
	}

	// Convert http:// to ws:// and https:// to wss://
	switch parsedURL.Scheme {
	case "http":
		parsedURL.Scheme = "ws"
	case "https":
		parsedURL.Scheme = "wss"
	case "ws", "wss":
		// Already WebSocket scheme
	default:
		return nil, fmt.Errorf("unsupported URL scheme: %s", parsedURL.Scheme)
	}

	// === Required Headers ===
	headers := http.Header{}

	// Pocket-Service-Id: Identifies which service to consume
	headers.Set("Pocket-Service-Id", serviceID)

	// Pocket-Supplier-Address: Optional supplier preference
	if supplierAddr != "" {
		headers.Set("Pocket-Supplier-Address", supplierAddr)
	}

	// Rpc-Type: Backend routing hint (Reference: poktroll/x/shared/types/service.pb.go)
	// Values: 1=GRPC, 2=WEBSOCKET, 3=JSON_RPC, 4=REST
	headers.Set("Rpc-Type", "2") // WEBSOCKET = 2

	// === Compression (RFC 7692 compliance) ===
	// EnableCompression in wsDialer negotiates permessage-deflate
	// Both client and server must support it for compression to be active

	// Dial WebSocket connection with compression-enabled dialer
	conn, _, err := wsDialer.Dial(parsedURL.String(), headers)
	if err != nil {
		return nil, fmt.Errorf("WebSocket dial failed: %w", err)
	}

	return conn, nil
}

// sendWebSocketRelay sends a relay request via WebSocket and returns the response.

// sendWebSocketRelayOnConnection sends a relay request via an existing WebSocket connection.
// This is used by the load test to reuse connections from the connection pool.
// Returns raw response bytes for signature verification.
func sendWebSocketRelayOnConnection(ctx context.Context, conn *websocket.Conn, relayRequestBz []byte) ([]byte, error) {
	// Send the relay request
	if err := conn.WriteMessage(websocket.BinaryMessage, relayRequestBz); err != nil {
		return nil, fmt.Errorf("failed to send relay request: %w", err)
	}

	// Read the relay response (return raw bytes)
	_, responseBz, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("failed to read relay response: %w", err)
	}

	return responseBz, nil
}
