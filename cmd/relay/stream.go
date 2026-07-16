package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
)

const (
	// streamDelimiter is the delimiter used by the relayer to separate batches in streaming responses.
	streamDelimiter = "||POKT_STREAM||"
)

// runStreamMode sends streaming relay requests to the relayer.
func RunStreamMode(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient) error {
	// Build payload (eth_blockNumber by default). When --batches is set, the demo
	// backend is asked to emit that many SSE batches and then close the stream;
	// unset means "receive everything until the server closes" (mainnet-style).
	payloadBz, err := buildStreamPayload(RelayBatches)
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	// Diagnostic mode: single request with detailed output
	if !RelayLoadTest {
		return runStreamDiagnostic(ctx, logger, relayClient, payloadBz)
	}

	// Load test mode not supported for streaming (streams are long-lived)
	return fmt.Errorf("load test mode is not supported for streaming relays")
}

// runStreamDiagnostic sends a single streaming relay request with detailed output.
func runStreamDiagnostic(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Build and sign relay request
	buildStart := time.Now()
	relayRequest, relayRequestBz, err := buildRelayRequest(ctx, relayClient, RelayServiceID, RelaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("failed to build relay request: %w", err)
	}
	buildDuration := time.Since(buildStart)

	logger.Info().
		Dur("build_time", buildDuration).
		Int("request_size", len(relayRequestBz)).
		Msg("relay request built and signed")

	// Send HTTP request
	networkStart := time.Now()
	batchesRaw, err := sendStreamingRelay(ctx, relayRequestBz)
	if err != nil {
		return fmt.Errorf("failed to send streaming relay: %w", err)
	}
	networkDuration := time.Since(networkStart)

	// Verify signatures for all batches and check for JSON-RPC errors
	verifyStart := time.Now()
	batches := make([]*servicetypes.RelayResponse, 0, len(batchesRaw))
	for i, batchBz := range batchesRaw {
		batch, err := relayClient.VerifyRelayResponse(ctx, RelaySupplierAddr, batchBz)
		if err != nil {
			return fmt.Errorf("signature verification failed for batch %d: %w", i+1, err)
		}

		// Check for JSON-RPC errors in batch payload
		if err := CheckRelayResponseError(batch); err != nil {
			return fmt.Errorf("batch %d contains error: %w", i+1, err)
		}

		batches = append(batches, batch)
	}
	verifyDuration := time.Since(verifyStart)

	// Display results
	fmt.Printf("\n=== Streaming Relay Diagnostic ===\n")
	fmt.Printf("App Address: %s\n", relayClient.GetAppAddress())
	fmt.Printf("Service ID: %s\n", RelayServiceID)
	fmt.Printf("Session ID: %s\n", relayRequest.Meta.SessionHeader.SessionId)
	fmt.Printf("Supplier: %s\n", RelaySupplierAddr)
	fmt.Printf("\n=== Timings ===\n")
	fmt.Printf("Build Time: %v\n", buildDuration)
	fmt.Printf("Stream Time: %v\n", networkDuration)
	fmt.Printf("Verify Time: %v\n", verifyDuration)
	fmt.Printf("Total Time: %v\n", buildDuration+networkDuration+verifyDuration)
	fmt.Printf("\n=== Stream Results ===\n")
	fmt.Printf("Batches Received: %d\n", len(batches))
	fmt.Printf("Signatures: ✅ ALL VALID\n")

	// Combine and display all payloads
	var combinedPayload []byte
	for i, batch := range batches {
		fmt.Printf("Batch %d: %d bytes\n", i+1, len(batch.Payload))
		combinedPayload = append(combinedPayload, batch.Payload...)
	}

	fmt.Printf("\n=== Combined Payload ===\n")
	if RelayOutputJSON {
		fmt.Printf("Payload (raw): %s\n", string(combinedPayload))
	} else {
		// Try to pretty-print JSON
		var payloadData interface{}
		if err := json.Unmarshal(combinedPayload, &payloadData); err == nil {
			prettyJSON, _ := json.MarshalIndent(payloadData, "", "  ")
			fmt.Printf("Payload:\n%s\n", string(prettyJSON))
		} else {
			fmt.Printf("Payload: %s\n", string(combinedPayload))
		}
	}

	return nil
}

// sendStreamingRelay sends a streaming relay request via HTTP and returns raw batch bytes.
func sendStreamingRelay(ctx context.Context, relayRequestBz []byte) ([][]byte, error) {
	// Build URL: {relayerURL}/{serviceID}
	url := fmt.Sprintf("%s/%s", RelayRelayerURL, RelayServiceID)

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(relayRequestBz))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/octet-stream")
	// Set Rpc-Type header to REST (4) to test SSE streaming endpoint
	// Reference: poktroll/x/shared/types/service.pb.go RPCType enum
	// Backend maps REST to http://backend:8545/stream/sse (tilt_config.yaml:156)
	req.Header.Set("Rpc-Type", "4") // REST = 4

	// Pocket-Simulation-Key-Id: tells the relayer this is a simulated relay.
	// Absent unless --simulate is set.
	if key, val, ok := simulationHTTPHeader(); ok {
		req.Header.Set(key, val)
	}

	// Send request
	client := &http.Client{
		Timeout: time.Duration(RelayTimeout) * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Read the streaming response until the server closes it (EOF) or the client
	// --timeout fires. On mainnet/beta the client cannot know how many batches a
	// service emits, so it collects everything the server sends. On localnet, the
	// demo backend honors the --batches count in the request body and closes after
	// emitting that many, so this returns promptly with exactly that many batches.
	return readStreamingBatches(resp.Body)
}

// buildStreamPayload creates a serialized POKTHTTPRequest for stream mode.
//
// When batches <= 0 (the mainnet/real-backend path) it forwards the payload
// verbatim via buildJSONRPCPayload — the exact --payload bytes, or the default
// eth_blockNumber body — so nothing is re-encoded. This matters: parsing a
// request into a generic map and re-marshalling would reorder keys and coerce
// integers to float64 (corrupting e.g. a large JSON-RPC id), which a real service
// may reject.
//
// When batches > 0 (the localnet demo path) it must add a "batches" control field
// so the demo backend emits exactly that many SSE batches and then closes. That
// requires parsing the body into a JSON object; a non-object (array/scalar/null)
// payload cannot carry the field and is rejected up front rather than silently
// dropped. The map round-trip is confined to this demo-only path.
func buildStreamPayload(batches int) ([]byte, error) {
	if batches <= 0 {
		return buildJSONRPCPayload()
	}

	var body map[string]any
	if RelayPayloadJSON != "" {
		if err := json.Unmarshal([]byte(RelayPayloadJSON), &body); err != nil {
			return nil, fmt.Errorf("stream --payload must be a JSON object to carry --batches: %w", err)
		}
		// A JSON "null" unmarshals into a nil map without error; injecting into it
		// would panic. Reject it as a non-object, same as an array/scalar payload.
		if body == nil {
			return nil, fmt.Errorf("stream --payload must be a JSON object to carry --batches, got null")
		}
	} else {
		body = map[string]any{
			"jsonrpc": "2.0",
			"method":  "eth_blockNumber",
			"params":  []any{},
			"id":      1,
		}
	}

	body["batches"] = batches

	jsonPayload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal stream payload: %w", err)
	}

	httpReq, err := http.NewRequest("POST", "/", bytes.NewReader(jsonPayload))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	_, poktHTTPRequestBz, err := sdktypes.SerializeHTTPRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize POKTHTTPRequest: %w", err)
	}

	return poktHTTPRequestBz, nil
}

// readStreamingBatches reads every non-empty signed batch from the response body
// and returns their raw bytes. It scans until the server closes the stream (EOF)
// or the reader fails (e.g. the HTTP client --timeout fires). Batches collected
// before a mid-stream reader error are returned with a nil error — they are
// complete and independently signature-verified by the caller — so a timeout on a
// long-lived stream never discards the data already received. The reader error is
// surfaced only when nothing at all was collected.
//
// The relayer suffixes the "||POKT_STREAM||" delimiter after every COMPLETE batch,
// so a batch terminated by a delimiter is whole. A timeout/reset that lands
// mid-write leaves a trailing batch with no delimiter; that partial is a truncated
// protobuf that would fail the caller's signature check and abort the whole run,
// discarding the valid batches before it. To honor the "never discards received
// data" contract, a non-delimiter-terminated trailing batch is dropped when — and
// only when — the stream ended on a reader error. On a clean EOF the same trailing
// token is a complete final batch (the server finished writing, then closed) and
// is kept.
func readStreamingBatches(body io.Reader) ([][]byte, error) {
	var batches [][]byte
	scanner := bufio.NewScanner(body)

	// Increase buffer size for large chunks (LLM responses can be large)
	const maxScanTokenSize = 256 * 1024 // 256KB
	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

	// tokenWasLeftover records whether the token the scanner just returned came
	// from the atEOF "leftover" path (no trailing delimiter) rather than a
	// delimiter boundary. Written by the split func, read in the scan loop.
	tokenWasLeftover := false

	// Custom split function to split on the delimiter
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}

		// Look for delimiter
		if i := bytes.Index(data, []byte(streamDelimiter)); i >= 0 {
			// Return the data up to (but not including) the delimiter
			tokenWasLeftover = false
			return i + len(streamDelimiter), data[0:i], nil
		}

		// If we're at EOF, return remaining data as last token
		if atEOF {
			tokenWasLeftover = true
			return len(data), data, nil
		}

		// Request more data
		return 0, nil, nil
	})

	// Scan every batch until the server closes the stream (EOF) or the reader
	// fails (client --timeout). Termination is by server-close, so the client
	// collects everything the service chose to send.
	lastAppendedWasLeftover := false
	for scanner.Scan() {
		batchData := scanner.Bytes()
		if len(batchData) == 0 {
			continue
		}

		// Store raw batch bytes for signature verification
		// NOTE: Do NOT trim whitespace - this corrupts binary protobuf data
		batchCopy := make([]byte, len(batchData))
		copy(batchCopy, batchData)
		batches = append(batches, batchCopy)
		lastAppendedWasLeftover = tokenWasLeftover
	}

	// A scanner error after we already have batches is expected on a long-lived
	// stream: the HTTP client timeout fires once the server stops sending. Those
	// batches are complete and are independently signature-verified by the caller,
	// so hand them back and swallow the error. Only surface the error when we
	// collected nothing to return.
	if err := scanner.Err(); err != nil {
		// Ended on a reader error, not a clean EOF: a non-delimiter-terminated
		// trailing batch is truncated (server was mid-write). Drop it so it does
		// not fail verification and abort the run, discarding the complete batches
		// received before it.
		if lastAppendedWasLeftover && len(batches) > 0 {
			batches = batches[:len(batches)-1]
		}
		if len(batches) == 0 {
			return nil, fmt.Errorf("error reading stream: %w", err)
		}
	}

	return batches, nil
}
