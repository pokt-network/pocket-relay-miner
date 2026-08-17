package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// RelayResult contains the outcome of a relay request.
type RelayResult struct {
	// Success indicates if the relay completed successfully
	Success bool

	// Request is the relay request that was sent
	Request *servicetypes.RelayRequest

	// Response is the relay response received (if any)
	Response *servicetypes.RelayResponse

	// Error is any error that occurred
	Error error

	// Timings for performance tracking
	BuildDuration   time.Duration
	NetworkDuration time.Duration
	VerifyDuration  time.Duration
	TotalDuration   time.Duration

	// Response metadata
	ResponseSize int
	StatusCode   int // HTTP status code (if applicable)
}

// maxErrorBodyBytes bounds how much of a backend error body is echoed into an
// error message, so a large upstream body cannot bloat CLI/log output.
const maxErrorBodyBytes = 200

// CheckRelayResponseError checks if a RelayResponse carries a backend or
// application error.
//
// A relay's Payload is NOT raw JSON: the relayer wraps the backend's HTTP
// response in a shannon-sdk POKTHTTPResponse (protobuf) before signing it
// (see relayer/signer.go, BuildErrorRelayResponse / SerializeHTTPResponse). The
// previous implementation ran json.Unmarshal directly on that protobuf blob,
// which always failed silently and left this check dead: a signed HTTP 500
// error response was reported as a success. We therefore deserialize the
// POKTHTTPResponse first and inspect the real backend status and body:
//
//  1. POKTHTTPResponse decodes (the common case):
//     a. StatusCode >= 400 -> backend/transport error (the signed error
//     response the relayer emits when the backend fails).
//     b. StatusCode < 400  -> apply the JSON-RPC error check to BodyBz (the
//     actual backend body), not to the protobuf envelope.
//     c. otherwise -> no error.
//  2. POKTHTTPResponse does NOT decode -> fall back to a best-effort JSON-RPC
//     check on the raw payload. This covers payloads that are not wrapped
//     POKTHTTPResponses (e.g. WebSocket subscription frames).
//
// Returns nil when no error is detectable.
func CheckRelayResponseError(response *servicetypes.RelayResponse) error {
	if response == nil {
		return fmt.Errorf("nil relay response")
	}

	// The relay payload wraps the backend's HTTP response in a protobuf
	// POKTHTTPResponse. Decode it so the real status and body are inspected,
	// not the protobuf envelope.
	if poktResp, err := sdktypes.DeserializeHTTPResponse(response.Payload); err == nil {
		if poktResp.StatusCode >= 400 {
			return fmt.Errorf("backend HTTP %d: %s", poktResp.StatusCode, truncateForError(poktResp.BodyBz))
		}
		return checkJSONRPCError(poktResp.BodyBz)
	}

	// Fallback: payload is not a wrapped POKTHTTPResponse (e.g. a WebSocket
	// subscription frame). Best-effort JSON-RPC check on the raw bytes.
	return checkJSONRPCError(response.Payload)
}

// checkJSONRPCError performs a best-effort JSON-RPC error inspection on a body.
// Not all bodies are JSON-RPC; a parse failure or an absent error field yields
// nil.
func checkJSONRPCError(bodyBz []byte) error {
	var jsonRPCResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	if err := json.Unmarshal(bodyBz, &jsonRPCResp); err == nil {
		if jsonRPCResp.Error != nil {
			return fmt.Errorf("JSON-RPC error %d: %s", jsonRPCResp.Error.Code, jsonRPCResp.Error.Message)
		}
	}

	return nil
}

// truncateForError renders a backend error body for inclusion in an error
// message, capped at maxErrorBodyBytes.
func truncateForError(bodyBz []byte) string {
	if len(bodyBz) > maxErrorBodyBytes {
		return string(bodyBz[:maxErrorBodyBytes])
	}
	return string(bodyBz)
}

// buildRelayRequest builds a relay request for the given service+supplier,
// routing through the simulated-relay path
// (relay_client.RelayClient.BuildSimulatedRelayRequest — locally ring-signed,
// NO chain query) when --simulate is set, or the normal chain-backed path
// (BuildRelayRequest) otherwise.
//
// Every relay-building call site (diagnostic AND load test, across all five
// transports) goes through this single branch, so --simulate is picked up
// everywhere without duplicating the check at each call site.
func buildRelayRequest(
	ctx context.Context,
	client *relay_client.RelayClient,
	serviceID string,
	supplierAddr string,
	payloadBz []byte,
) (*servicetypes.RelayRequest, []byte, error) {
	if RelaySimulate {
		return client.BuildSimulatedRelayRequest(
			RelaySimAppPubKey,
			RelaySimGatewayPubKeys,
			serviceID,
			supplierAddr,
			payloadBz,
			time.Now(),
		)
	}
	return client.BuildRelayRequest(ctx, serviceID, supplierAddr, payloadBz)
}

// BuildAndSendRelay is a helper function that builds, sends, and verifies a relay.
// This consolidates the common pattern across all relay protocols.
//
// Parameters:
// - ctx: Context for cancellation
// - logger: Logger for debug output
// - client: RelayClient for building and verifying
// - payloadBz: The relay payload (e.g., JSON-RPC request)
// - sendFunc: Function that sends the relay and returns raw response bytes
//
// Returns:
// - RelayResult with all timing and response data
func BuildAndSendRelay(
	ctx context.Context,
	logger logging.Logger,
	client *relay_client.RelayClient,
	payloadBz []byte,
	sendFunc func(ctx context.Context, relayRequestBz []byte) ([]byte, error),
) *RelayResult {
	result := &RelayResult{
		Success: false,
	}

	totalStart := time.Now()

	// Step 1: Build and sign relay request
	buildStart := time.Now()
	relayRequest, relayRequestBz, err := buildRelayRequest(ctx, client, RelayServiceID, RelaySupplierAddr, payloadBz)
	if err != nil {
		result.Error = fmt.Errorf("failed to build relay request: %w", err)
		result.TotalDuration = time.Since(totalStart)
		return result
	}
	result.BuildDuration = time.Since(buildStart)
	result.Request = relayRequest

	logger.Debug().
		Dur("build_time", result.BuildDuration).
		Int("request_size", len(relayRequestBz)).
		Msg("relay request built and signed")

	// Step 2: Send relay request
	networkStart := time.Now()
	relayResponseBz, err := sendFunc(ctx, relayRequestBz)
	if err != nil {
		result.Error = fmt.Errorf("failed to send relay: %w", err)
		result.NetworkDuration = time.Since(networkStart)
		result.TotalDuration = time.Since(totalStart)
		return result
	}
	result.NetworkDuration = time.Since(networkStart)
	result.ResponseSize = len(relayResponseBz)

	logger.Debug().
		Dur("network_time", result.NetworkDuration).
		Int("response_size", result.ResponseSize).
		Msg("relay response received")

	// Step 3: Verify supplier signature
	verifyStart := time.Now()
	relayResponse, err := client.VerifyRelayResponse(ctx, RelaySupplierAddr, relayResponseBz)
	if err != nil {
		result.Error = fmt.Errorf("signature verification failed: %w", err)
		result.VerifyDuration = time.Since(verifyStart)
		result.TotalDuration = time.Since(totalStart)
		return result
	}
	result.VerifyDuration = time.Since(verifyStart)
	result.Response = relayResponse

	logger.Debug().
		Dur("verify_time", result.VerifyDuration).
		Msg("relay response signature verified")

	// Step 4: Check for errors in RelayResponse
	if err := CheckRelayResponseError(relayResponse); err != nil {
		result.Error = fmt.Errorf("relay response contains error: %w", err)
		result.TotalDuration = time.Since(totalStart)
		return result
	}

	// Success!
	result.Success = true
	result.TotalDuration = time.Since(totalStart)

	logger.Debug().
		Dur("total_time", result.TotalDuration).
		Msg("relay completed successfully")

	return result
}

// DisplayDiagnosticResult prints a formatted diagnostic result for a single relay.
func DisplayDiagnosticResult(client *relay_client.RelayClient, result *RelayResult) {
	fmt.Printf("\n=== Relay Request Diagnostic ===\n")
	fmt.Printf("App Address: %s\n", client.GetAppAddress())
	fmt.Printf("Service ID: %s\n", RelayServiceID)

	if result.Request != nil {
		fmt.Printf("Session ID: %s\n", result.Request.Meta.SessionHeader.SessionId)
	}
	fmt.Printf("Supplier: %s\n", RelaySupplierAddr)

	fmt.Printf("\n=== Timings ===\n")
	fmt.Printf("Build Time: %v\n", result.BuildDuration)
	fmt.Printf("Network Time: %v\n", result.NetworkDuration)
	fmt.Printf("Verify Time: %v\n", result.VerifyDuration)
	fmt.Printf("Total Time: %v\n", result.TotalDuration)

	fmt.Printf("\n=== Response ===\n")
	if result.Success {
		fmt.Printf("Status: ✅ SUCCESS\n")
		fmt.Printf("Signature: ✅ VALID\n")
		fmt.Printf("Error Check: ✅ NO ERRORS\n")
		fmt.Printf("Response Size: %d bytes\n", result.ResponseSize)

		if result.Response != nil && len(result.Response.Payload) > 0 {
			fmt.Printf("\n=== Response Payload ===\n")
			// Try to pretty-print JSON
			var jsonData interface{}
			if err := json.Unmarshal(result.Response.Payload, &jsonData); err == nil {
				prettyJSON, _ := json.MarshalIndent(jsonData, "", "  ")
				fmt.Printf("%s\n", prettyJSON)
			} else {
				// Not JSON or parse error - print raw
				fmt.Printf("%s\n", result.Response.Payload)
			}
		}
	} else {
		fmt.Printf("Status: ❌ FAILED\n")
		if result.Error != nil {
			fmt.Printf("Error: %v\n", result.Error)
		}
	}

	fmt.Printf("\n")
}

// NewRateLimiter creates a rate limiter for RPS targeting.
// Returns nil if rps == 0 (unlimited).
func NewRateLimiter(rps int) *time.Ticker {
	if rps <= 0 {
		return nil
	}

	// Calculate delay between requests
	// delay = 1 second / rps
	delay := time.Second / time.Duration(rps)
	return time.NewTicker(delay)
}

// WaitForRateLimit waits for the rate limiter tick if enabled.
// Does nothing if rateLimiter is nil (unlimited rate).
func WaitForRateLimit(rateLimiter *time.Ticker) {
	if rateLimiter != nil {
		<-rateLimiter.C
	}
}
