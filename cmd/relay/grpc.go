package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip compressor for grpc-encoding: gzip
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	// RelayServiceMethodPath is the gRPC method path for the relay service.
	relayServiceMethodPath = "/pocket.service.RelayService/SendRelay"

	// demoGRPCMethodPath is the localnet demo backend's unary gRPC method
	// (demo.DemoService/GetBlockHeight). It is the default target of `relay grpc`
	// so the native gRPC forwarding path (CLI -> relayer h2c -> gRPC backend) is
	// exercised end to end.
	demoGRPCMethodPath = "/demo.DemoService/GetBlockHeight"

	// grpcFramePrefixLen is the length of a gRPC length-prefixed message frame
	// header: 1 compression-flag byte + 4 big-endian message-length bytes.
	grpcFramePrefixLen = 5
)

// runGRPCMode sends gRPC relay requests to the relayer.
func RunGRPCMode(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient) error {
	// Default gRPC mode drives a REAL gRPC inner request (the demo backend's
	// unary GetBlockHeight) so the relayer's native gRPC forwarding is exercised
	// end to end. A custom --payload instead sends a JSON-RPC inner request,
	// which deliberately drives the relayer's REST fallback for gRPC relays.
	buildPayload := buildNativeGRPCPayload
	if RelayPayloadJSON != "" {
		buildPayload = buildJSONRPCPayload
	}
	payloadBz, err := buildPayload()
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	// Decided ONCE, here, and passed down. The demo method's reply always
	// carries fields, so an empty one is a failure; a method the operator chose
	// may legitimately reply empty. Re-deriving this deeper down by comparing
	// the method against a constant would mean that renaming the demo backend
	// silently relaxes the check instead of breaking loudly.
	requireNonEmpty := strings.TrimSpace(RelayGRPCMethod) == ""

	// Diagnostic mode: single request with detailed output
	if !RelayLoadTest {
		return runGRPCDiagnostic(ctx, logger, relayClient, payloadBz, requireNonEmpty)
	}

	// Load test mode: concurrent requests with metrics
	return runGRPCLoadTest(ctx, logger, relayClient, payloadBz, requireNonEmpty)
}

// resolveGRPCMethodPath returns the gRPC method to invoke: --grpc-method when
// set, the localnet demo method otherwise.
//
// The shape check is not decoration. Measured on Go 1.26: http.NewRequest
// accepts "/demo.DemoService" (no method), "/a/b/c" and "" without complaint,
// and the malformed path travels as URL.Path all the way to the backend -- so a
// typo would be signed, metered against the application's stake and mined,
// returning an opaque UNIMPLEMENTED. Rejecting the shape here fails before any
// of that happens.
func resolveGRPCMethodPath() (string, error) {
	method := strings.TrimSpace(RelayGRPCMethod)
	if method == "" {
		return demoGRPCMethodPath, nil
	}
	parts := strings.Split(method, "/")
	if len(parts) != 3 || parts[0] != "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf(
			"--grpc-method %q is not of the form /package.Service/Method", method)
	}
	return method, nil
}

// buildGRPCRequestFrame wraps --grpc-request-hex in a gRPC length-prefixed
// frame. An empty value yields the zero-length frame, which is the correct body
// for any request message with no fields -- demo.Empty and cosmos'
// GetLatestBlockRequest among them, so the common case needs no hex at all.
func buildGRPCRequestFrame() ([]byte, error) {
	msg := []byte{}
	if h := strings.TrimSpace(RelayGRPCRequestHex); h != "" {
		decoded, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("--grpc-request-hex is not valid hex: %w", err)
		}
		msg = decoded
	}
	frame := make([]byte, grpcFramePrefixLen+len(msg))
	binary.BigEndian.PutUint32(frame[1:grpcFramePrefixLen], uint32(len(msg)))
	copy(frame[grpcFramePrefixLen:], msg)
	return frame, nil
}

// buildNativeGRPCPayload creates a serialized POKTHTTPRequest carrying a real
// unary gRPC request for the demo backend's GetBlockHeight method. This lets
// `relay grpc` exercise the relayer's native gRPC (h2c) forwarding path end to
// end, instead of sending JSON the gRPC backend would reject with HTTP 415.
//
// GetBlockHeight takes demo.Empty, which serializes to zero bytes, so the gRPC
// length-prefixed frame is just its 5-byte header:
// [compression flag=0][big-endian uint32 length=0].
func buildNativeGRPCPayload() ([]byte, error) {
	methodPath, err := resolveGRPCMethodPath()
	if err != nil {
		return nil, err
	}
	frame, err := buildGRPCRequestFrame()
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", methodPath, bytes.NewReader(frame))
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC HTTP request: %w", err)
	}

	// gRPC-over-HTTP/2 conventions: the content type marks the body as a gRPC
	// frame, and TE: trailers asks the backend to return grpc-status in HTTP
	// trailers (which the relayer folds back into the response headers).
	httpReq.Header.Set("Content-Type", "application/grpc")
	httpReq.Header.Set("TE", "trailers")

	_, poktHTTPRequestBz, err := sdktypes.SerializeHTTPRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize POKTHTTPRequest: %w", err)
	}

	return poktHTTPRequestBz, nil
}

// runGRPCDiagnostic sends a single gRPC relay request with detailed output.
func runGRPCDiagnostic(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte, requireNonEmpty bool) error {
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

	// Native gRPC e2e check: on top of the shared signature/error verification,
	// confirm the reply is a well-formed gRPC frame with grpc-status 0. Skipped
	// when a custom --payload drove the REST fallback instead of a gRPC frame.
	if result.Success && RelayPayloadJSON == "" {
		if err := verifyGRPCRelayPayload(result.Response, requireNonEmpty); err != nil {
			result.Success = false
			result.Error = fmt.Errorf("gRPC relay verification failed: %w", err)
		}
	}

	// Display results using shared formatter
	DisplayDiagnosticResult(relayClient, result)

	// Return error if relay failed
	if !result.Success {
		return result.Error
	}

	// Surface the demo backend's block height decoded from the gRPC response
	// frame, proving the native gRPC round trip actually reached the backend.
	if RelayPayloadJSON == "" && RelayGRPCMethod == "" {
		if height, err := decodeGRPCBlockHeight(result.Response); err == nil {
			fmt.Printf("Block Height: %d\n", height)
		}
	}

	return nil
}

// runGRPCLoadTest sends concurrent gRPC relay requests with performance metrics.
//
// Each worker calls BuildRelayRequest itself so the ring signature is generated
// fresh per relay (ring sigs are randomized). This matches PATH's production
// behavior (one sign per incoming request) and guarantees distinct relay bytes
// per call, so the SMST stores one leaf per request instead of collapsing.
func runGRPCLoadTest(ctx context.Context, logger logging.Logger, relayClient *relay_client.RelayClient, payloadBz []byte, requireNonEmpty bool) error {
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

	// Supplier targeting mirrors the HTTP load test (http.go): fixed supplier
	// by default, or per-request round-robin across the session's suppliers
	// with --all-suppliers. Unlike WebSocket, gRPC does not pin the supplier at
	// the transport level — each relay carries it in Meta — so rotating per
	// request over one connection is valid.
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
				Msg("starting gRPC load test")
		},
		func(reqNum int) {
			// Send relay with timeout
			requestCtx, cancel := context.WithTimeout(ctx, time.Duration(RelayTimeout)*time.Second)
			defer cancel()

			// Build a FRESH relay request for this worker. Ring signatures use
			// randomness, so each call yields distinct bytes even for an
			// identical payload — matches PATH's per-request sign behaviour.
			supplier := supplierAddrs[supplierIdx.Add(1)%uint64(len(supplierAddrs))]
			relayRequest, _, err := buildRelayRequest(requestCtx, relayClient, RelayServiceID, supplier, payloadBz)
			if err != nil {
				metrics.RecordError(fmt.Errorf("build relay request: %w", err))
				logger.Debug().
					Err(err).
					Int("request_num", reqNum).
					Msg("gRPC relay request build failed")
				return
			}

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

			// Verify relay response signature against the supplier this relay
			// was addressed to (round-robin aware).
			relayResponse, err := relayClient.VerifyRelayResponse(requestCtx, supplier, relayResponseBz)
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

			// Native gRPC e2e check: reply must be a well-formed gRPC frame with
			// grpc-status 0. Skipped when a custom --payload drove the REST
			// fallback instead of a native gRPC frame.
			if RelayPayloadJSON == "" {
				if err := verifyGRPCRelayPayload(relayResponse, requireNonEmpty); err != nil {
					metrics.RecordError(err)
					logger.Debug().
						Err(err).
						Int("request_num", reqNum).
						Msg("gRPC relay request failed (gRPC payload verification)")
					return
				}
			}

			// Success: valid signature + no JSON-RPC error
			metrics.RecordSuccess(latencyMs)
			logger.Debug().
				Int("request_num", reqNum).
				Float64("latency_ms", latencyMs).
				Msg("gRPC relay request succeeded")
		},
	)

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
	pairs := []string{"rpc-type", "1"} // GRPC = 1
	// pocket-simulation-key-id: tells the relayer this is a simulated relay.
	// Absent unless --simulate is set.
	if key, val, ok := simulationGRPCMetadataPair(); ok {
		pairs = append(pairs, key, val)
	}
	md := metadata.Pairs(pairs...)
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

// verifyGRPCRelayPayload asserts that a relay response carries a successful
// native gRPC reply: HTTP 200, a grpc-status of 0 folded in from the backend's
// trailers, and a well-formed, non-empty gRPC message frame. It runs after the
// shared signature and error checks so `relay grpc` has a real end-to-end
// assertion that the relayer forwarded to the gRPC backend (not the REST
// fallback) and got a valid gRPC reply.
func verifyGRPCRelayPayload(relayResponse *servicetypes.RelayResponse, requireNonEmpty bool) error {
	if relayResponse == nil {
		return fmt.Errorf("nil relay response")
	}

	poktResp, err := sdktypes.DeserializeHTTPResponse(relayResponse.Payload)
	if err != nil {
		return fmt.Errorf("deserialize POKTHTTPResponse: %w", err)
	}

	if poktResp.StatusCode != http.StatusOK {
		return fmt.Errorf("expected HTTP 200, got %d", poktResp.StatusCode)
	}

	grpcStatus, ok := lookupGRPCHeader(poktResp.Header, "Grpc-Status")
	if !ok {
		return fmt.Errorf("missing grpc-status header (backend gRPC trailers not folded into response)")
	}
	if grpcStatus != "0" {
		// grpc-message carries the backend's own explanation. The relayer folds
		// the backend trailers into the response headers, so it is already here;
		// printing only the numeric status throws away the one line that says
		// which method or field was wrong.
		if msg, found := lookupGRPCHeader(poktResp.Header, "Grpc-Message"); found && msg != "" {
			return fmt.Errorf("grpc-status %q is non-zero (gRPC call failed): %s", grpcStatus, msg)
		}
		return fmt.Errorf("grpc-status %q is non-zero (gRPC call failed, backend sent no grpc-message)", grpcStatus)
	}

	if err := validateGRPCFrame(poktResp.BodyBz, requireNonEmpty); err != nil {
		return err
	}

	return nil
}

// validateGRPCFrame checks that bodyBz is a single, well-formed gRPC
// length-prefixed message frame carrying a non-empty message. GetBlockHeight
// always encodes height>0, so an empty message means the backend answered
// nothing.
// validateGRPCFrame checks the reply is a well-formed gRPC frame. requireNonEmpty
// says whether an empty message is a failure: it is for the demo method, whose
// response always carries fields, and it is not for an operator-chosen method,
// whose reply may legitimately be empty. The caller decides once and passes it
// down -- deriving it here by comparing the method string would turn a stale
// constant into a silent downgrade of this check.
func validateGRPCFrame(bodyBz []byte, requireNonEmpty bool) error {
	if len(bodyBz) < grpcFramePrefixLen {
		return fmt.Errorf("gRPC frame too short: %d bytes (need at least %d)", len(bodyBz), grpcFramePrefixLen)
	}
	if bodyBz[0] != 0x00 {
		return fmt.Errorf("gRPC frame compression flag %d unsupported (expected 0)", bodyBz[0])
	}
	msgLen := binary.BigEndian.Uint32(bodyBz[1:grpcFramePrefixLen])
	if int(msgLen) != len(bodyBz)-grpcFramePrefixLen {
		return fmt.Errorf("gRPC frame length %d does not match message size %d", msgLen, len(bodyBz)-grpcFramePrefixLen)
	}
	if msgLen == 0 && requireNonEmpty {
		return fmt.Errorf("gRPC frame carries an empty message")
	}
	return nil
}

// lookupGRPCHeader returns the first value of a header by case-insensitive key.
// The relayer folds gRPC trailers in under http.Header-canonical keys
// ("Grpc-Status"), but we compare case-insensitively so a different wire casing
// cannot silently drop the check.
func lookupGRPCHeader(headers map[string]*sdktypes.Header, key string) (string, bool) {
	for headerKey, header := range headers {
		if strings.EqualFold(headerKey, key) && header != nil && len(header.Values) > 0 {
			return header.Values[0], true
		}
	}
	return "", false
}

// decodeGRPCBlockHeight extracts the demo backend's block height from a gRPC
// response frame. The message after the 5-byte frame header encodes
// BlockHeightResponse{uint64 height=1}, so we scan its protobuf fields for
// field 1 (varint).
func decodeGRPCBlockHeight(relayResponse *servicetypes.RelayResponse) (uint64, error) {
	poktResp, err := sdktypes.DeserializeHTTPResponse(relayResponse.Payload)
	if err != nil {
		return 0, fmt.Errorf("deserialize POKTHTTPResponse: %w", err)
	}
	if err := validateGRPCFrame(poktResp.BodyBz, true); err != nil {
		return 0, err
	}

	msg := poktResp.BodyBz[grpcFramePrefixLen:]
	for len(msg) > 0 {
		fieldNum, wireType, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		msg = msg[n:]

		if fieldNum == 1 && wireType == protowire.VarintType {
			height, vn := protowire.ConsumeVarint(msg)
			if vn < 0 {
				return 0, protowire.ParseError(vn)
			}
			return height, nil
		}

		n = protowire.ConsumeFieldValue(fieldNum, wireType, msg)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		msg = msg[n:]
	}

	return 0, fmt.Errorf("block height field not found in gRPC response")
}
