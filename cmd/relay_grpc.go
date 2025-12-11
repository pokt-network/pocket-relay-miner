package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
func runGRPCMode(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient) error {
	// Build payload (eth_blockNumber by default)
	payloadBz, err := buildJSONRPCPayload()
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	// Diagnostic mode: single request with detailed output
	if !relayLoadTest {
		return runGRPCDiagnostic(ctx, logger, relayClient, payloadBz)
	}

	// Load test mode: concurrent requests with metrics
	return runGRPCLoadTest(ctx, logger, relayClient, payloadBz)
}

// runGRPCDiagnostic sends a single gRPC relay request with detailed output.
func runGRPCDiagnostic(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Build and sign relay request
	buildStart := time.Now()
	relayRequest, _, err := relayClient.BuildRelayRequest(ctx, relayServiceID, relaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("failed to build relay request: %w", err)
	}
	buildDuration := time.Since(buildStart)

	logger.Info().
		Dur("build_time", buildDuration).
		Msg("relay request built and signed")

	// Connect to gRPC server
	connectStart := time.Now()
	// Parse URL to extract host:port (gRPC doesn't accept http:// prefix)
	grpcAddr := relayRelayerURL
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
	connectDuration := time.Since(connectStart)

	// Send relay request
	sendStart := time.Now()
	relayResponseBz, err := invokeGRPCRelay(ctx, conn, relayRequest)
	if err != nil {
		return fmt.Errorf("failed to invoke gRPC relay: %w", err)
	}
	sendDuration := time.Since(sendStart)

	// Verify supplier signature
	verifyStart := time.Now()
	relayResponse, err := relayClient.VerifyRelayResponse(ctx, relaySupplierAddr, relayResponseBz)
	if err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}
	verifyDuration := time.Since(verifyStart)

	// Display results
	fmt.Printf("\n=== gRPC Relay Diagnostic ===\n")
	fmt.Printf("App Address: %s\n", relayClient.GetAppAddress())
	fmt.Printf("Service ID: %s\n", relayServiceID)
	fmt.Printf("Session ID: %s\n", relayRequest.Meta.SessionHeader.SessionId)
	fmt.Printf("Supplier: %s\n", relaySupplierAddr)
	fmt.Printf("\n=== Timings ===\n")
	fmt.Printf("Build Time: %v\n", buildDuration)
	fmt.Printf("Connect Time: %v\n", connectDuration)
	fmt.Printf("RPC Time: %v\n", sendDuration)
	fmt.Printf("Verify Time: %v\n", verifyDuration)
	fmt.Printf("Total Time: %v\n", buildDuration+connectDuration+sendDuration+verifyDuration)
	fmt.Printf("\n=== Response ===\n")
	fmt.Printf("Signature: ✅ VALID\n")
	fmt.Printf("Size: %d bytes\n", len(relayResponse.Payload))

	// Parse and display response payload
	if relayOutputJSON {
		fmt.Printf("Payload (raw): %s\n", string(relayResponse.Payload))
	} else {
		// Try to pretty-print JSON
		var payloadData interface{}
		if err := json.Unmarshal(relayResponse.Payload, &payloadData); err == nil {
			prettyJSON, _ := json.MarshalIndent(payloadData, "", "  ")
			fmt.Printf("Payload:\n%s\n", string(prettyJSON))
		} else {
			fmt.Printf("Payload: %s\n", string(relayResponse.Payload))
		}
	}

	return nil
}

// runGRPCLoadTest sends concurrent gRPC relay requests with performance metrics.
func runGRPCLoadTest(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte) error {
	// Build relay request once (reuse across requests)
	logger.Info().Msg("building relay request template for load test")
	relayRequest, _, err := relayClient.BuildRelayRequest(ctx, relayServiceID, relaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("failed to build relay request template: %w", err)
	}

	// Create gRPC connection (reuse across workers)
	// Parse URL to extract host:port (gRPC doesn't accept http:// prefix)
	grpcAddr := relayRelayerURL
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
	semaphore := make(chan struct{}, relayConcurrency)
	var wg sync.WaitGroup

	logger.Info().
		Int("count", relayCount).
		Int("concurrency", relayConcurrency).
		Msg("starting gRPC load test")

	metrics.Start()

	// Spawn workers
	for i := 0; i < relayCount; i++ {
		wg.Add(1)
		semaphore <- struct{}{} // Acquire slot

		go func(reqNum int) {
			defer wg.Done()
			defer func() { <-semaphore }() // Release slot

			// Send relay with timeout
			requestCtx, cancel := context.WithTimeout(ctx, time.Duration(relayTimeout)*time.Second)
			defer cancel()

			start := time.Now()
			_, err := invokeGRPCRelay(requestCtx, conn, relayRequest)
			latencyMs := float64(time.Since(start).Microseconds()) / 1000.0

			if err != nil {
				metrics.RecordError(err)
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("gRPC relay request failed")
			} else {
				metrics.RecordSuccess(latencyMs)
				logger.Debug().
					Int("request_num", reqNum).
					Float64("latency_ms", latencyMs).
					Msg("gRPC relay request succeeded")
			}
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
func invokeGRPCRelay(ctx context.Context, conn *grpc.ClientConn, relayRequest *servicetypes.RelayRequest) ([]byte, error) {
	// Add Rpc-Type metadata to indicate backend type for proper routing
	// Reference: poktroll/x/shared/types/service.pb.go RPCType enum
	md := metadata.Pairs("rpc-type", "1") // GRPC = 1
	ctx = metadata.NewOutgoingContext(ctx, md)

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
