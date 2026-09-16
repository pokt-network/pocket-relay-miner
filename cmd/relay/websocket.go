package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// runWebSocketMode sends WebSocket relay requests to the relayer.
func RunWebSocketMode(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient) error {
	// Build payload (raw JSON for WebSocket - no HTTP wrapping)
	payloadBz, err := buildWebSocketPayload()
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	// One adversarial case instead of a relay: everything a WebSocket does
	// besides the happy path, asserted rather than eyeballed.
	if RelayWSCase != "" {
		return RunWebSocketCase(ctx, logger, relayClient)
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
			_ = conn.WriteMessage(websocket.CloseMessage, closeMessage) //nolint:errcheck // best-effort courtesy frame on a connection that is going away; the close below does not depend on it
			_ = conn.Close()
		}()

		// Send relay request
		if err := conn.WriteMessage(websocket.BinaryMessage, relayRequestBz); err != nil {
			return nil, fmt.Errorf("failed to send relay request: %w", err)
		}

		// Receive relay response
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck // redundant: the very next line checks the outcome this error would have predicted
		// The frame type is whatever the relayer echoed back from the backend, so
		// it carries no information about the response and is deliberately ignored.
		_, responseData, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("failed to read relay response: %w", err)
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

// assignSuppliersToPool returns the supplier each pooled connection must be
// handshaked against. Unlike HTTP (where the supplier is chosen per request),
// WebSocket pins the supplier at the handshake, so round-robin has to happen
// per CONNECTION here, not per message.
//
// The pool is sized to max(concurrency, len(suppliers)) so every supplier gets
// at least one connection even when there are more suppliers than workers. The
// connPool is a FIFO channel that workers pop-then-push, so all connections
// (hence all suppliers) rotate through the active workers over the run even
// though only `concurrency` are ever in flight at once — a pool larger than
// concurrency still spreads relays across every supplier.
//
// Suppliers are assigned round-robin (suppliers[i%len(suppliers)]). Returns nil
// when suppliers is empty; the caller validates non-emptiness before use.
func assignSuppliersToPool(suppliers []string, concurrency int) []string {
	if len(suppliers) == 0 {
		return nil
	}

	poolSize := concurrency
	if len(suppliers) > poolSize {
		poolSize = len(suppliers)
	}

	assigned := make([]string, poolSize)
	for i := range assigned {
		assigned[i] = suppliers[i%len(suppliers)]
	}
	return assigned
}

// runWebSocketLoadTest sends concurrent WebSocket relay requests with performance metrics.
// Uses a connection pool to avoid overhead of creating new connections for each request.
//
// Each worker calls BuildRelayRequest itself so the ring signature is generated
// fresh per relay (ring sigs are randomized). This matches PATH's production
// behavior (one sign per incoming request) and guarantees distinct relay bytes
// per call, so the SMST stores one leaf per request instead of collapsing.
func runWebSocketLoadTest(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Supplier targeting: fixed (--supplier / localnet default) or, with
	// --all-suppliers, round-robin across every supplier in the current
	// session. A single fixed supplier exhausts ITS per-session claimable
	// budget quickly while the other session suppliers sit idle; spreading
	// matches how a gateway distributes traffic.
	suppliers := []string{RelaySupplierAddr}
	if RelayAllSuppliers {
		var supErr error
		suppliers, supErr = relayClient.SessionSupplierAddresses(ctx, RelayServiceID)
		if supErr != nil {
			return fmt.Errorf("failed to list session suppliers: %w", supErr)
		}
		logger.Info().Int("suppliers", len(suppliers)).Msg("round-robining across session suppliers")
	}

	deps := wsLoadDeps{
		build: func(ctx context.Context, supplier string) ([]byte, error) {
			_, relayRequestBz, err := buildRelayRequest(ctx, relayClient, RelayServiceID, supplier, payloadBz)
			return relayRequestBz, err
		},
		verify: relayClient.VerifyRelayResponse,
	}
	_, _, err := runWebSocketLoad(ctx, logger, deps, suppliers)
	return err
}

// wsLoadDeps is what the WebSocket load test needs from the chain: a signed
// relay for a supplier, and a check of the supplier's signature on its answer.
// A test replaces both to drive the pool against a local server.
type wsLoadDeps struct {
	build  func(ctx context.Context, supplier string) ([]byte, error)
	verify func(ctx context.Context, supplier string, responseBz []byte) (*servicetypes.RelayResponse, error)
	// backoff spaces redials out; nil uses a real one.
	backoff *loadBackoff
}

// wsSlot is one pooled connection and the supplier it was handshaked against.
// WebSocket pins the supplier at connection time via the Pocket-Supplier-Address
// header (PATH v2 protocol; the relayer reads it in websocket.go
// handleWebSocket), so a connection can only serve relays for that one supplier
// and the pairing travels with it: workers sign and verify against the right key.
// conn is nil when the connection is dead: whoever takes the slot next dials it
// again before sending, so a dead connection never goes back to the pool
// looking alive.
type wsSlot struct {
	supplier string
	conn     *websocket.Conn
	// diedOnRollover says why conn is nil, so the redial is counted under the
	// right cause.
	diedOnRollover bool
}

// kill closes the slot's connection and marks it dead.
func (s *wsSlot) kill(onRollover bool) {
	_ = s.conn.Close()
	s.conn = nil
	s.diedOnRollover = onRollover
}

// wsPoolStats is what the pool did besides serving relays. Every relay asked
// for ends in exactly one of Successful, Errors or lost, so --count equals
// their sum.
type wsPoolStats struct {
	size                 int
	lost                 atomic.Int64 // relays the relayer's session end took with it
	redialsAfterRollover atomic.Int64
	redialsAfterError    atomic.Int64
	dialFailures         atomic.Int64
	backoff              *loadBackoff
}

// summary is printed after the load test's own summary. Neither line may start
// with "Successful:" or "Errors:": live.sh and the load drivers read those.
func (s *wsPoolStats) summary() string {
	after, onError := s.redialsAfterRollover.Load(), s.redialsAfterError.Load()
	return fmt.Sprintf("Lost to session rollover: %d\nWebSocket pool: size=%d redials=%d (after rollover %d, after error %d, dial failures %d, backoff waits %d)\n",
		s.lost.Load(), s.size, after+onError, after, onError, s.dialFailures.Load(), s.backoff.Waits())
}

// isSessionExpired reports whether a signed response is the relayer ending the
// connection's session (relayer/websocket.go sendSessionExpirationMessage): a
// relayer-set 410, not a backend's. A backend answering 410 is an error.
func isSessionExpired(resp *servicetypes.RelayResponse) bool {
	return resp.RelayMinerError != nil && resp.RelayMinerError.Code == http.StatusGone
}

// isSessionClosed reports whether err is the relayer's close 4000 ending the
// connection's session. gorilla's IsCloseError does not unwrap, and the send
// path wraps what it returns.
func isSessionClosed(err error) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == relayer.CloseSessionExpired
}

// runWebSocketLoad runs the WebSocket load test over a pool of connections, one
// per assigned supplier slot.
//
// When the relayer ends a connection's session it sends a signed 410 and then
// closes with 4000. The relay that meets either is lost to the rollover, not
// retried (the relayer may already have billed it) and not counted as an
// error; the connection is dead from that moment, because nothing read after
// the 410 belongs to a relay. Any other send or read failure is an error and
// kills the connection too. Either way the next worker to take the slot dials
// it again. A bad signature or a JSON-RPC error leaves the connection alive:
// the request/response pairing on it is intact.
func runWebSocketLoad(ctx context.Context, logger logging.Logger, deps wsLoadDeps, suppliers []string) (*RelayMetrics, *wsPoolStats, error) {
	// WebSocket pins the supplier at the handshake, so round-robin is per
	// connection: one pooled connection per assigned supplier slot.
	poolSuppliers := assignSuppliersToPool(suppliers, RelayConcurrency)
	backoff := deps.backoff
	if backoff == nil {
		backoff = newLoadBackoff()
	}
	stats := &wsPoolStats{size: len(poolSuppliers), backoff: backoff}

	// Create connection pool as a buffered channel (thread-safe queue).
	// Workers will pop a slot, use it exclusively, then push it back.
	pool := make(chan *wsSlot, len(poolSuppliers))
	closePool := func() {
		close(pool)
		for slot := range pool {
			if slot.conn != nil {
				_ = slot.conn.Close()
			}
		}
	}
	for i := range poolSuppliers {
		conn, err := connectWebSocket(RelayRelayerURL, RelayServiceID, poolSuppliers[i])
		if err != nil {
			closePool()
			return nil, nil, fmt.Errorf("failed to create connection pool: %w", err)
		}
		// Set ping/pong handlers to keep connections alive
		conn.SetPongHandler(func(string) error { return nil })
		pool <- &wsSlot{supplier: poolSuppliers[i], conn: conn}
	}
	defer closePool()

	// Create metrics collector
	metrics := NewRelayMetrics()

	runLoadTest(RelayCount, RelayConcurrency, RelayRPS, metrics,
		func() {
			logger.Info().
				Int("count", RelayCount).
				Int("concurrency", RelayConcurrency).
				Int("connection_pool_size", len(poolSuppliers)).
				Int("rps", RelayRPS).
				Msg("starting WebSocket load test with connection pool")
		},
		func(reqNum int) {
			// Pop a slot from the pool (blocking until one is available). Its
			// connection is pinned to a supplier at its handshake, so this
			// worker signs and verifies against that same supplier.
			slot := <-pool
			defer func() { pool <- slot }() // Push back when done, dead or alive

			if slot.conn == nil {
				conn, err := connectWebSocket(RelayRelayerURL, RelayServiceID, slot.supplier)
				if err != nil {
					stats.dialFailures.Add(1)
					metrics.RecordError(fmt.Errorf("redial: %w", err))
					backoff.refused()
					return
				}
				backoff.succeeded()
				conn.SetPongHandler(func(string) error { return nil })
				if slot.diedOnRollover {
					stats.redialsAfterRollover.Add(1)
				} else {
					stats.redialsAfterError.Add(1)
				}
				slot.conn, slot.diedOnRollover = conn, false
			}

			// Send relay with timeout
			requestCtx, cancel := context.WithTimeout(ctx, time.Duration(RelayTimeout)*time.Second)
			defer cancel()

			// Build a FRESH relay request for this worker. Ring signatures use
			// randomness, so each call yields distinct bytes even for an
			// identical payload — matches PATH's per-request sign behaviour.
			relayRequestBz, err := deps.build(requestCtx, slot.supplier)
			if err != nil {
				metrics.RecordError(fmt.Errorf("build relay request: %w", err))
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("WebSocket relay request build failed")
				return
			}

			start := time.Now()
			relayResponseBz, err := sendWebSocketRelayOnConnection(requestCtx, slot.conn, relayRequestBz)
			latencyMs := float64(time.Since(start).Microseconds()) / 1000.0

			if err != nil {
				if isSessionClosed(err) {
					stats.lost.Add(1)
					slot.kill(true)
					logger.Debug().Int("request_num", reqNum).Msg("WebSocket relay lost: the relayer closed the session")
					return
				}
				metrics.RecordError(err)
				slot.kill(false)
				if isTryAgainLater(err) {
					backoff.refused()
				}
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("WebSocket relay request failed (network error)")
				return
			}

			// Verify relay response signature against the supplier this
			// connection was handshaked with (round-robin aware).
			relayResponse, err := deps.verify(requestCtx, slot.supplier, relayResponseBz)
			if err != nil {
				metrics.RecordError(fmt.Errorf("signature verification failed: %w", err))
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("WebSocket relay request failed (invalid signature)")
				return
			}

			if isSessionExpired(relayResponse) {
				stats.lost.Add(1)
				slot.kill(true)
				logger.Debug().Int("request_num", reqNum).Msg("WebSocket relay lost: the relayer ended the session")
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
		},
	)
	fmt.Print(stats.summary())

	return metrics, stats, nil
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

	// Pocket-Supplier-Address: names the supplier in the handshake. That is
	// PATH's shape, and it is what the relayer's 403 gate checks against its
	// live key set.
	//
	// --ws-handshake=v1 omits it, which is sage's shape: the supplier then
	// arrives inside the first RelayRequest and the bridge adopts it as its
	// owner. Those are two different code paths in the relayer, and before this
	// flag the CLI could only ever produce the first one -- so the metering
	// path that the owner rule was written to fix had never been exercised
	// end to end.
	if supplierAddr != "" && !strings.EqualFold(RelayWSHandshake, "v1") {
		headers.Set("Pocket-Supplier-Address", supplierAddr)
	}

	// Rpc-Type: Backend routing hint (Reference: poktroll/x/shared/types/service.pb.go)
	// Values: 1=GRPC, 2=WEBSOCKET, 3=JSON_RPC, 4=REST
	headers.Set("Rpc-Type", "2") // WEBSOCKET = 2

	// Pocket-Simulation-Key-Id: tells the relayer this is a simulated relay.
	// Absent unless --simulate is set.
	if key, val, ok := simulationHTTPHeader(); ok {
		headers.Set(key, val)
	}

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

// sendWebSocketRelayOnConnection sends a relay request via an existing WebSocket connection.
// This is used by the load test to reuse connections from the connection pool.
// Returns raw response bytes for signature verification.
func sendWebSocketRelayOnConnection(ctx context.Context, conn *websocket.Conn, relayRequestBz []byte) ([]byte, error) {
	// Send the relay request
	if err := conn.WriteMessage(websocket.BinaryMessage, relayRequestBz); err != nil {
		return nil, fmt.Errorf("failed to send relay request: %w", err)
	}

	// Bound the read by the request's --timeout: a relayer that neither answers
	// nor closes would otherwise hold this worker, and its slot, forever.
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("failed to set read deadline: %w", err)
		}
	}

	// Read the relay response (return raw bytes)
	_, responseBz, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("failed to read relay response: %w", err)
	}

	return responseBz, nil
}
