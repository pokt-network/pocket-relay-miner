package relay

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip compressor for grpc-encoding: gzip
	"google.golang.org/grpc/metadata"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

const (
	// RelayServiceMethodPath is the gRPC method path for the relay service.
	relayServiceMethodPath = "/pocket.service.RelayService/SendRelay"
)

// runGRPCMode sends gRPC relay requests to the relayer.
func RunGRPCMode(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient) error {
	// Build payload (eth_blockNumber by default)
	payloadBz, err := buildJSONRPCPayload()
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	// Diagnostic mode: single request with detailed output
	if !RelayLoadTest {
		return runGRPCDiagnostic(ctx, logger, relayClient, payloadBz)
	}

	// Load test mode: concurrent requests with metrics
	return runGRPCLoadTest(ctx, logger, relayClient, payloadBz)
}

// runGRPCDiagnostic sends a single gRPC relay request with detailed output.
func runGRPCDiagnostic(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Parse gRPC address (remove http:// prefix if present)
	grpcAddr := RelayRelayerURL
	if strings.HasPrefix(grpcAddr, "http://") {
		grpcAddr = strings.TrimPrefix(grpcAddr, "http://")
	} else if strings.HasPrefix(grpcAddr, "https://") {
		grpcAddr = strings.TrimPrefix(grpcAddr, "https://")
	}

	// Create gRPC connection
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to create gRPC connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Create sendFunc that invokes gRPC relay
	sendFunc := func(ctx context.Context, relayRequestBz []byte) ([]byte, error) {
		// Unmarshal relay request (needed for gRPC invocation)
		relayRequest := &servicetypes.RelayRequest{}
		if err := relayRequest.Unmarshal(relayRequestBz); err != nil {
			return nil, fmt.Errorf("failed to unmarshal relay request: %w", err)
		}

		// Invoke gRPC relay
		return invokeGRPCRelay(ctx, conn, relayRequest)
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

// runGRPCLoadTest sends concurrent gRPC relay requests with performance metrics.
func runGRPCLoadTest(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Build relay request once (reuse across requests)
	logger.Info().Msg("building relay request template for load test")
	relayRequest, _, err := relayClient.BuildRelayRequest(ctx, RelayServiceID, RelaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("failed to build relay request template: %w", err)
	}

	// Create gRPC connection (reuse across workers)
	// Parse URL to extract host:port (gRPC doesn't accept http:// prefix)
	grpcAddr := RelayRelayerURL
	if strings.HasPrefix(grpcAddr, "http://") {
		grpcAddr = strings.TrimPrefix(grpcAddr, "http://")
	} else if strings.HasPrefix(grpcAddr, "https://") {
		grpcAddr = strings.TrimPrefix(grpcAddr, "https://")
	}
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to create gRPC connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

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
		Int("rps", RelayRPS).
		Msg("starting gRPC load test")

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

			// Send relay with timeout
			requestCtx, cancel := context.WithTimeout(ctx, time.Duration(RelayTimeout)*time.Second)
			defer cancel()

			start := time.Now()
			relayResponseBz, err := invokeGRPCRelay(requestCtx, conn, relayRequest)
			latencyMs := float64(time.Since(start).Microseconds()) / 1000.0

			if err != nil {
				metrics.RecordError(err)
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("gRPC relay request failed (network error)")
				return
			}

			// Verify relay response signature
			relayResponse, err := relayClient.VerifyRelayResponse(requestCtx, RelaySupplierAddr, relayResponseBz)
			if err != nil {
				metrics.RecordError(fmt.Errorf("signature verification failed: %w", err))
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("gRPC relay request failed (invalid signature)")
				return
			}

			// Check for JSON-RPC errors in the payload
			if err := CheckRelayResponseError(relayResponse); err != nil {
				metrics.RecordError(err)
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("gRPC relay request failed (JSON-RPC error)")
				return
			}

			// Success: valid signature + no JSON-RPC error
			metrics.RecordSuccess(latencyMs)
			logger.Debug().
				Int("request_num", reqNum).
				Float64("latency_ms", latencyMs).
				Msg("gRPC relay request succeeded")
		}(i)
	}

	// Wait for all workers to finish
	wg.Wait()
	metrics.End()

	// Display results
	fmt.Println(metrics.GetSummary())

	return nil
}

// invokeGRPCRelay invokes the gRPC relay service method and returns raw response bytes.
//
// BEST PRACTICE: This demonstrates proper gRPC relay invocation:
// 1. rpc-type metadata: 1 - Tells relayer which backend type (GRPC=1)
// 2. grpc-encoding: gzip - Automatically negotiated when gzip compressor is imported
// 3. Bidirectional streaming - Supports both request/response and streaming patterns
//
// Note: The gzip compressor is auto-registered via the blank import at the top of this file.
// gRPC will automatically compress/decompress when the server supports it.
func invokeGRPCRelay(ctx context.Context, conn *grpc.ClientConn, relayRequest *servicetypes.RelayRequest) ([]byte, error) {
	// === Required Metadata ===
	// rpc-type: Backend routing hint (Reference: poktroll/x/shared/types/service.pb.go)
	// Values: 1=GRPC, 2=WEBSOCKET, 3=JSON_RPC, 4=REST
	md := metadata.Pairs("rpc-type", "1") // GRPC = 1
	ctx = metadata.NewOutgoingContext(ctx, md)

	// === Compression (gRPC standard) ===
	// gRPC uses grpc-encoding header for compression negotiation
	// The gzip compressor is registered via blank import: _ "google.golang.org/grpc/encoding/gzip"
	// Server and client will automatically negotiate compression

	// Create a new stream for the relay RPC
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    "SendRelay",
		ServerStreams: true,
		ClientStreams: true,
	}, relayServiceMethodPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	// Send the relay request
	if err := stream.SendMsg(relayRequest); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Close the send side
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("failed to close send: %w", err)
	}

	// Receive the relay response
	relayResponse := &servicetypes.RelayResponse{}
	if err := stream.RecvMsg(relayResponse); err != nil {
		return nil, fmt.Errorf("failed to receive response: %w", err)
	}

	// Marshal to bytes for signature verification
	relayResponseBz, err := relayResponse.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return relayResponseBz, nil
}
