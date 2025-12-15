package cmd

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
)

const (
	// streamDelimiter is the delimiter used by the relayer to separate batches in streaming responses.
	streamDelimiter = "||POKT_STREAM||"
)

// runStreamMode sends streaming relay requests to the relayer.
func runStreamMode(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient) error {
	// Build payload (eth_blockNumber by default)
	payloadBz, err := buildJSONRPCPayload()
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	// Diagnostic mode: single request with detailed output
	if !relayLoadTest {
		return runStreamDiagnostic(ctx, logger, relayClient, payloadBz)
	}

	// Load test mode not supported for streaming (streams are long-lived)
	return fmt.Errorf("load test mode is not supported for streaming relays")
}

// runStreamDiagnostic sends a single streaming relay request with detailed output.
func runStreamDiagnostic(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Build and sign relay request
	buildStart := time.Now()
	relayRequest, relayRequestBz, err := relayClient.BuildRelayRequest(ctx, relayServiceID, relaySupplierAddr, payloadBz)
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
		batch, err := relayClient.VerifyRelayResponse(ctx, relaySupplierAddr, batchBz)
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
	fmt.Printf("Service ID: %s\n", relayServiceID)
	fmt.Printf("Session ID: %s\n", relayRequest.Meta.SessionHeader.SessionId)
	fmt.Printf("Supplier: %s\n", relaySupplierAddr)
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
	if relayOutputJSON {
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
	url := fmt.Sprintf("%s/%s", relayRelayerURL, relayServiceID)

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

	// Send request
	client := &http.Client{
		Timeout: time.Duration(relayTimeout) * time.Second,
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

	// Read streaming response
	return readStreamingBatches(resp.Body)
}

// readStreamingBatches reads streaming batches from the response body and returns raw bytes.
func readStreamingBatches(body io.Reader) ([][]byte, error) {
	var batches [][]byte
	scanner := bufio.NewScanner(body)

	// Increase buffer size for large chunks (LLM responses can be large)
	const maxScanTokenSize = 256 * 1024 // 256KB
	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

	// Custom split function to split on the delimiter
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}

		// Look for delimiter
		if i := bytes.Index(data, []byte(streamDelimiter)); i >= 0 {
			// Return the data up to (but not including) the delimiter
			return i + len(streamDelimiter), data[0:i], nil
		}

		// If we're at EOF, return remaining data as last token
		if atEOF {
			return len(data), data, nil
		}

		// Request more data
		return 0, nil, nil
	})

	// Scan batches
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
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading stream: %w", err)
	}

	return batches, nil
}

// Reuse buildJSONRPCPayload from relay_http.go
// (it's already defined in that file, so we can call it)
