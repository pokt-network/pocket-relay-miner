package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"

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

// CheckRelayResponseError checks if a RelayResponse contains an error.
// This checks the payload for JSON-RPC errors (best-effort).
//
// Returns:
// - error if the response contains a JSON-RPC error
// - nil if the response is successful or payload is not JSON-RPC
func CheckRelayResponseError(response *servicetypes.RelayResponse) error {
	if response == nil {
		return fmt.Errorf("nil relay response")
	}

	// Try to parse payload as JSON-RPC to check for errors
	// This is best-effort - not all payloads are JSON-RPC
	var jsonRPCResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	if err := json.Unmarshal(response.Payload, &jsonRPCResp); err == nil {
		if jsonRPCResp.Error != nil {
			return fmt.Errorf("JSON-RPC error %d: %s", jsonRPCResp.Error.Code, jsonRPCResp.Error.Message)
		}
	}

	return nil
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
	relayRequest, relayRequestBz, err := client.BuildRelayRequest(ctx, relayServiceID, relaySupplierAddr, payloadBz)
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
	relayResponse, err := client.VerifyRelayResponse(ctx, relaySupplierAddr, relayResponseBz)
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
	fmt.Printf("Service ID: %s\n", relayServiceID)

	if result.Request != nil {
		fmt.Printf("Session ID: %s\n", result.Request.Meta.SessionHeader.SessionId)
	}
	fmt.Printf("Supplier: %s\n", relaySupplierAddr)

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

// LoadTestStats tracks statistics for load testing.
type LoadTestStats struct {
	TotalRequests    int64
	SuccessCount     int64
	FailureCount     int64
	ValidationFailed int64 // Failed signature verification
	ErrorResponses   int64 // RelayResponse contained error
	NetworkErrors    int64 // Network/transport errors

	TotalLatency   time.Duration
	MinLatency     time.Duration
	MaxLatency     time.Duration
	LatencyBuckets map[string]int64 // e.g., "0-10ms", "10-50ms", etc.
}

// NewLoadTestStats creates a new load test statistics tracker.
func NewLoadTestStats() *LoadTestStats {
	return &LoadTestStats{
		MinLatency: time.Hour, // Start with very high value
		LatencyBuckets: map[string]int64{
			"0-10ms":     0,
			"10-50ms":    0,
			"50-100ms":   0,
			"100-500ms":  0,
			"500-1000ms": 0,
			"1000ms+":    0,
		},
	}
}

// RecordResult records a relay result in the statistics.
func (s *LoadTestStats) RecordResult(result *RelayResult) {
	s.TotalRequests++

	if result.Success {
		s.SuccessCount++
	} else {
		s.FailureCount++

		// Categorize failure type
		if result.Error != nil {
			errMsg := result.Error.Error()
			if contains(errMsg, "signature verification") {
				s.ValidationFailed++
			} else if contains(errMsg, "relay response contains error") || contains(errMsg, "JSON-RPC error") {
				s.ErrorResponses++
			} else {
				s.NetworkErrors++
			}
		}
	}

	// Track latency
	latency := result.TotalDuration
	s.TotalLatency += latency

	if latency < s.MinLatency {
		s.MinLatency = latency
	}
	if latency > s.MaxLatency {
		s.MaxLatency = latency
	}

	// Update latency buckets
	ms := latency.Milliseconds()
	switch {
	case ms < 10:
		s.LatencyBuckets["0-10ms"]++
	case ms < 50:
		s.LatencyBuckets["10-50ms"]++
	case ms < 100:
		s.LatencyBuckets["50-100ms"]++
	case ms < 500:
		s.LatencyBuckets["100-500ms"]++
	case ms < 1000:
		s.LatencyBuckets["500-1000ms"]++
	default:
		s.LatencyBuckets["1000ms+"]++
	}
}

// DisplayLoadTestSummary prints a formatted summary of load test results.
func (s *LoadTestStats) DisplayLoadTestSummary(duration time.Duration) {
	fmt.Printf("\n=== Load Test Summary ===\n")
	fmt.Printf("Total Requests: %d\n", s.TotalRequests)
	fmt.Printf("Successful: %d (%.2f%%)\n", s.SuccessCount, float64(s.SuccessCount)/float64(s.TotalRequests)*100)
	fmt.Printf("Failed: %d (%.2f%%)\n", s.FailureCount, float64(s.FailureCount)/float64(s.TotalRequests)*100)

	if s.FailureCount > 0 {
		fmt.Printf("\n=== Failure Breakdown ===\n")
		fmt.Printf("Validation Failed: %d\n", s.ValidationFailed)
		fmt.Printf("Error Responses: %d\n", s.ErrorResponses)
		fmt.Printf("Network Errors: %d\n", s.NetworkErrors)
	}

	fmt.Printf("\n=== Latency ===\n")
	if s.TotalRequests > 0 {
		avgLatency := s.TotalLatency / time.Duration(s.TotalRequests)
		fmt.Printf("Min: %v\n", s.MinLatency)
		fmt.Printf("Avg: %v\n", avgLatency)
		fmt.Printf("Max: %v\n", s.MaxLatency)
	}

	fmt.Printf("\n=== Latency Distribution ===\n")
	for _, bucket := range []string{"0-10ms", "10-50ms", "50-100ms", "100-500ms", "500-1000ms", "1000ms+"} {
		count := s.LatencyBuckets[bucket]
		pct := float64(count) / float64(s.TotalRequests) * 100
		fmt.Printf("%12s: %6d (%.1f%%)\n", bucket, count, pct)
	}

	fmt.Printf("\n=== Performance ===\n")
	rps := float64(s.TotalRequests) / duration.Seconds()
	fmt.Printf("Duration: %v\n", duration)
	fmt.Printf("Throughput: %.2f RPS\n", rps)

	fmt.Printf("\n")
}

// contains is a helper function to check if a string contains a substring.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
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
