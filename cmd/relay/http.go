package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	sdktypes "github.com/pokt-network/shannon-sdk/types"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// Shared HTTP client with connection pooling for load tests.
// Reuses connections to avoid TCP handshake overhead.
// IMPORTANT: DisableCompression is false (Go default) to mimic a gateway's behavior.
// This makes Go automatically add "Accept-Encoding: gzip" and decompress responses.
var sharedHTTPClient = &http.Client{
	Timeout: 30 * time.Second, // Default timeout, overridden per-request if needed
	Transport: &http.Transport{
		MaxIdleConns:        200,              // Total pool size
		MaxIdleConnsPerHost: 200,              // Per-host pool (all requests go to same relayer)
		IdleConnTimeout:     90 * time.Second, // Keep connections alive
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false, // Mimic a gateway: auto-add Accept-Encoding, auto-decompress
	},
}

// applyRelayTimeout propagates the --timeout flag onto the shared HTTP client
// used by the jsonrpc and cometbft modes. The client is constructed with a 30s
// default; http.Client.Timeout bounds the ENTIRE request and wins over any
// longer per-request context deadline, so without this the flag (default 120s)
// was silently ignored and every relay was capped at 30s. A non-positive flag is
// ignored so an unset value can never disable the client timeout entirely.
//
// Call this once at mode entry, before any load-test worker goroutine starts:
// the shared client is written here and read concurrently afterward.
func applyRelayTimeout() {
	if RelayTimeout > 0 {
		sharedHTTPClient.Timeout = time.Duration(RelayTimeout) * time.Second
	}
}

// runHTTPMode sends HTTP/JSONRPC relay requests to the relayer.
func RunHTTPMode(ctx context.Context, logger logging.Logger, client *relay_client.RelayClient) error {
	applyRelayTimeout()

	// Build payload (eth_blockNumber by default)
	payloadBz, err := buildJSONRPCPayload()
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	// Diagnostic mode: single request with detailed output
	if !RelayLoadTest {
		return runHTTPDiagnostic(ctx, logger, client, payloadBz)
	}

	// Load test mode: concurrent requests with metrics
	return runHTTPLoadTest(ctx, logger, client, payloadBz)
}

// runHTTPDiagnostic sends a single HTTP relay request with detailed output.
func runHTTPDiagnostic(ctx context.Context, logger logging.Logger, client *relay_client.RelayClient, payloadBz []byte) error {
	// Use shared build/send/verify logic
	result := BuildAndSendRelay(ctx, logger, client, payloadBz, sendHTTPRelay)

	// Display results using shared formatter
	DisplayDiagnosticResult(client, result)

	// Return error if relay failed
	if !result.Success {
		return result.Error
	}

	return nil
}

// runHTTPLoadTest sends concurrent HTTP relay requests with performance metrics.
//
// Each worker calls BuildRelayRequest itself so the ring signature is generated
// fresh per relay (ring sigs are randomized — NewRandomScalar). This matches
// what a gateway does in production (one sign per incoming request) and guarantees
// distinct relay bytes even when the payload is identical, so the SMST stores
// N unique leaves for N concurrent requests instead of collapsing to one.
// BuildRelayRequest reads the session at the chain's current height, so a run
// longer than a session moves to the next one on its own.
func runHTTPLoadTest(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Create metrics collector
	metrics := NewRelayMetrics()

	// Supplier targeting: fixed (--supplier / localnet default) or, with
	// --all-suppliers, round-robin across every supplier in the current
	// session. A single fixed supplier exhausts ITS per-session claimable
	// budget quickly while the other session suppliers sit idle; spreading
	// matches how a gateway distributes traffic. The list is captured once:
	// on localnet the staked set is stable across sessions, but a mid-run
	// session rollover on a real network could rotate suppliers out.
	supplierAddrs := []string{RelaySupplierAddr}
	if RelayAllSuppliers {
		var supErr error
		supplierAddrs, supErr = relayClient.SessionSupplierAddresses(ctx, RelayServiceID)
		if supErr != nil {
			return fmt.Errorf("failed to list session suppliers: %w", supErr)
		}
		logger.Info().Int("suppliers", len(supplierAddrs)).Msg("round-robining across session suppliers")
	}
	var supplierIdx atomic.Uint64

	runLoadTest(RelayCount, RelayConcurrency, RelayRPS, metrics,
		func() {
			logger.Info().
				Int("count", RelayCount).
				Int("concurrency", RelayConcurrency).
				Int("rps", RelayRPS).
				Msg("starting load test")
		},
		func(reqNum int) {
			// Send relay with timeout
			requestCtx, cancel := context.WithTimeout(ctx, time.Duration(RelayTimeout)*time.Second)
			defer cancel()

			// Build a FRESH relay request for this worker. Ring signatures use
			// randomness, so each call produces different bytes even for an
			// identical payload, as a gateway signs once per request.
			supplier := supplierAddrs[supplierIdx.Add(1)%uint64(len(supplierAddrs))]
			_, relayRequestBz, err := buildRelayRequest(requestCtx, relayClient, RelayServiceID, supplier, payloadBz)
			if err != nil {
				metrics.RecordError(fmt.Errorf("build relay request: %w", err))
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("relay request build failed")
				return
			}

			start := time.Now()
			relayResponseBz, err := sendHTTPRelay(requestCtx, relayRequestBz)
			latencyMs := float64(time.Since(start).Microseconds()) / 1000.0

			if err != nil {
				metrics.RecordError(err)
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("relay request failed (HTTP error)")
				return
			}

			// Verify relay response signature against the supplier this
			// request actually targeted (round-robin aware).
			relayResponse, err := relayClient.VerifyRelayResponse(requestCtx, supplier, relayResponseBz)
			if err != nil {
				metrics.RecordError(fmt.Errorf("signature verification failed: %w", err))
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("relay request failed (invalid signature)")
				return
			}

			// Inspect the signed response for a backend or JSON-RPC error. The
			// payload is a protobuf POKTHTTPResponse, NOT raw JSON, so the check
			// must go through CheckRelayResponseError (which deserializes the
			// envelope and inspects the real backend status + body). A bare
			// json.Unmarshal on the protobuf envelope always fails and silently
			// counts backend 4xx-with-body and 200-with-JSON-RPC-error responses
			// as load-test successes. Mirrors the diagnostic, grpc, and websocket
			// paths, which all route through this helper.
			if respErr := CheckRelayResponseError(relayResponse); respErr != nil {
				metrics.RecordError(respErr)
				logger.Debug().
					Err(respErr).
					Int("request_num", reqNum).
					Msg("relay request failed (backend/JSON-RPC error)")
				return
			}

			// Success: HTTP 200 + valid signature + no JSON-RPC error
			metrics.RecordSuccess(latencyMs)
			logger.Debug().
				Int("request_num", reqNum).
				Float64("latency_ms", latencyMs).
				Msg("relay request succeeded")
		},
	)

	return nil
}

// Rpc-Type header values (Reference: poktroll/x/shared/types/service.pb.go RPCType).
const (
	rpcTypeJSONRPC  = "3" // JSON_RPC
	rpcTypeCometBFT = "5" // COMET_BFT (CometBFT RPC, itself JSON-RPC over HTTP)
)

// sendHTTPRelay sends a JSON-RPC (Rpc-Type 3) relay request via HTTP.
func sendHTTPRelay(ctx context.Context, relayRequestBz []byte) ([]byte, error) {
	return sendRelayOverHTTP(ctx, relayRequestBz, rpcTypeJSONRPC)
}

// sendRelayOverHTTP POSTs a protobuf RelayRequest to the relayer, tagging it with
// the given Rpc-Type routing hint, and returns the raw response bytes. It uses the
// shared HTTP client with connection pooling to avoid TCP handshake overhead.
//
// BEST PRACTICE: This demonstrates proper HTTP relay consumption:
//  1. Content-Type: application/x-protobuf - the body is a protobuf RelayRequest
//  2. Rpc-Type - tells the relayer which backend type to route to (3=JSON_RPC,
//     4=REST, 5=CometBFT, ...)
//  3. Accept-Encoding: gzip - request compressed responses (RFC 7231)
//
// Note: Go's http.Client automatically handles Accept-Encoding and decompression
// when DisableCompression is false (the default).
func sendRelayOverHTTP(ctx context.Context, relayRequestBz []byte, rpcType string) ([]byte, error) {
	// Build URL: {relayerURL}/{serviceID}
	url := fmt.Sprintf("%s/%s", RelayRelayerURL, RelayServiceID)

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(relayRequestBz))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// === Required Headers ===
	// Content-Type: The body is protobuf-encoded RelayRequest
	req.Header.Set("Content-Type", "application/x-protobuf")

	// Rpc-Type: Backend routing hint (Reference: poktroll/x/shared/types/service.pb.go)
	req.Header.Set("Rpc-Type", rpcType)

	// Pocket-Simulation-Key-Id: tells the relayer this is a simulated relay
	// (relayer.SimulationVerifier path — served but never charged) instead of
	// a normal chain-backed one. Absent unless --simulate is set.
	if key, val, ok := simulationHTTPHeader(); ok {
		req.Header.Set(key, val)
	}

	// === Compression (RFC 7231 compliance) ===
	// Accept-Encoding is automatically added by Go's http.Client when DisableCompression=false
	// The relayer will compress responses when this header is present
	// Go's client automatically decompresses gzip responses

	// Send request using shared client with connection pooling
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // the body only enriches an error that is returned either way; on failure it is empty and the status still names the failure
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Read response body
	// Note: Go's http.Client automatically decompresses gzip when Content-Encoding: gzip
	// is present in the response (because DisableCompression=false in our Transport)
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return respBody, nil
}

// buildJSONRPCPayload creates a serialized POKTHTTPRequest with JSON-RPC payload.
// Uses custom payload if provided, otherwise defaults to eth_blockNumber.
func buildJSONRPCPayload() ([]byte, error) {
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

	// Create HTTP POST request with JSON-RPC body
	httpReq, err := http.NewRequest("POST", "/", bytes.NewReader(jsonPayload))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set JSON-RPC headers
	httpReq.Header.Set("Content-Type", "application/json")

	// Serialize to POKTHTTPRequest protobuf
	_, poktHTTPRequestBz, err := sdktypes.SerializeHTTPRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize POKTHTTPRequest: %w", err)
	}

	return poktHTTPRequestBz, nil
}
