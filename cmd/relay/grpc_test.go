package relay

import (
	"encoding/binary"
	"testing"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// makeGRPCRelayResponse builds a RelayResponse whose Payload is a serialized
// POKTHTTPResponse for a native gRPC reply: an HTTP status, the trailer-folded
// grpc-status header, and a gRPC message frame body. An empty grpcStatus omits
// the header entirely, so the "missing header" case can be exercised.
func makeGRPCRelayResponse(t *testing.T, status uint32, grpcStatus string, body []byte) *servicetypes.RelayResponse {
	t.Helper()

	poktResp := &sdktypes.POKTHTTPResponse{
		StatusCode: status,
		Header:     make(map[string]*sdktypes.Header),
		BodyBz:     body,
	}
	if grpcStatus != "" {
		poktResp.Header["Grpc-Status"] = &sdktypes.Header{
			Key:    "Grpc-Status",
			Values: []string{grpcStatus},
		}
	}

	payloadBz, err := proto.MarshalOptions{Deterministic: true}.Marshal(poktResp)
	require.NoError(t, err, "marshal POKTHTTPResponse")

	return &servicetypes.RelayResponse{Payload: payloadBz}
}

// makeGRPCFrame wraps a protobuf message in an uncompressed gRPC
// length-prefixed frame: [0x00][big-endian uint32 length][message].
func makeGRPCFrame(t *testing.T, msg []byte) []byte {
	t.Helper()

	frame := make([]byte, grpcFramePrefixLen+len(msg))
	frame[0] = 0x00
	binary.BigEndian.PutUint32(frame[1:grpcFramePrefixLen], uint32(len(msg)))
	copy(frame[grpcFramePrefixLen:], msg)
	return frame
}

// TestBuildNativeGRPCPayload verifies the default gRPC mode payload is a real
// unary gRPC request for the demo backend: POST to the demo method path,
// application/grpc content type, and an empty 5-byte gRPC frame body.
func TestBuildNativeGRPCPayload(t *testing.T) {
	payloadBz, err := buildNativeGRPCPayload()
	require.NoError(t, err)

	poktReq, err := sdktypes.DeserializeHTTPRequest(payloadBz)
	require.NoError(t, err)

	require.Equal(t, "POST", poktReq.Method)
	require.Equal(t, demoGRPCMethodPath, poktReq.Url)

	require.Contains(t, poktReq.Header, "Content-Type")
	require.Equal(t, []string{"application/grpc"}, poktReq.Header["Content-Type"].Values)

	// http.Header canonicalizes "TE" to "Te"; assert the trailers signal survives.
	require.Contains(t, poktReq.Header, "Te")
	require.Equal(t, []string{"trailers"}, poktReq.Header["Te"].Values)

	// demo.Empty serializes to zero bytes, so the frame is just its 5-byte header.
	require.Equal(t, []byte{0x00, 0x00, 0x00, 0x00, 0x00}, poktReq.BodyBz)
}

// TestVerifyGRPCRelayPayload_OK verifies a 200 response with grpc-status 0 and a
// well-formed, non-empty gRPC frame passes verification.
func TestVerifyGRPCRelayPayload_OK(t *testing.T) {
	// {field 1 (height): varint 42}
	resp := makeGRPCRelayResponse(t, 200, "0", makeGRPCFrame(t, []byte{0x08, 0x2a}))

	require.NoError(t, verifyGRPCRelayPayload(resp))
}

// TestVerifyGRPCRelayPayload_GrpcStatusNonZero verifies a non-zero grpc-status
// is surfaced as a failure even when HTTP is 200.
func TestVerifyGRPCRelayPayload_GrpcStatusNonZero(t *testing.T) {
	resp := makeGRPCRelayResponse(t, 200, "12", makeGRPCFrame(t, []byte{0x08, 0x2a}))

	err := verifyGRPCRelayPayload(resp)

	require.Error(t, err)
	require.ErrorContains(t, err, "grpc-status")
}

// TestVerifyGRPCRelayPayload_MissingGrpcStatus verifies an absent grpc-status
// header (trailers not folded in) is treated as a failure.
func TestVerifyGRPCRelayPayload_MissingGrpcStatus(t *testing.T) {
	resp := makeGRPCRelayResponse(t, 200, "", makeGRPCFrame(t, []byte{0x08, 0x2a}))

	err := verifyGRPCRelayPayload(resp)

	require.Error(t, err)
	require.ErrorContains(t, err, "grpc-status")
}

// TestVerifyGRPCRelayPayload_CaseInsensitiveGrpcStatus verifies the grpc-status
// lookup ignores wire casing, so a lowercase key still satisfies the check.
func TestVerifyGRPCRelayPayload_CaseInsensitiveGrpcStatus(t *testing.T) {
	poktResp := &sdktypes.POKTHTTPResponse{
		StatusCode: 200,
		Header: map[string]*sdktypes.Header{
			"grpc-status": {Key: "grpc-status", Values: []string{"0"}},
		},
		BodyBz: makeGRPCFrame(t, []byte{0x08, 0x2a}),
	}
	payloadBz, err := proto.MarshalOptions{Deterministic: true}.Marshal(poktResp)
	require.NoError(t, err)
	resp := &servicetypes.RelayResponse{Payload: payloadBz}

	require.NoError(t, verifyGRPCRelayPayload(resp))
}

// TestVerifyGRPCRelayPayload_BadFrame verifies a body too short to be a gRPC
// frame is rejected.
func TestVerifyGRPCRelayPayload_BadFrame(t *testing.T) {
	resp := makeGRPCRelayResponse(t, 200, "0", []byte{0x00, 0x00, 0x00})

	err := verifyGRPCRelayPayload(resp)

	require.Error(t, err)
	require.ErrorContains(t, err, "frame")
}

// TestVerifyGRPCRelayPayload_HTTPError verifies a non-200 HTTP status (e.g. the
// 415 a gRPC backend returns to a JSON body) fails verification.
func TestVerifyGRPCRelayPayload_HTTPError(t *testing.T) {
	resp := makeGRPCRelayResponse(t, 415, "0", makeGRPCFrame(t, []byte{0x08, 0x2a}))

	err := verifyGRPCRelayPayload(resp)

	require.Error(t, err)
	require.ErrorContains(t, err, "415")
}

// TestVerifyGRPCRelayPayload_NilResponse verifies the nil guard.
func TestVerifyGRPCRelayPayload_NilResponse(t *testing.T) {
	err := verifyGRPCRelayPayload(nil)

	require.Error(t, err)
	require.ErrorContains(t, err, "nil relay response")
}

// TestDecodeGRPCBlockHeight verifies the height is decoded from the frame's
// protobuf body, including a multi-byte varint (proves it is decoded, not read
// as a single byte).
func TestDecodeGRPCBlockHeight(t *testing.T) {
	// {field 1 (height): varint 300} -> 0xac 0x02 (multi-byte varint).
	resp := makeGRPCRelayResponse(t, 200, "0", makeGRPCFrame(t, []byte{0x08, 0xac, 0x02}))

	height, err := decodeGRPCBlockHeight(resp)

	require.NoError(t, err)
	require.Equal(t, uint64(300), height)
}

// TestDecodeGRPCBlockHeight_BadFrame verifies a malformed frame surfaces an
// error instead of a bogus height.
func TestDecodeGRPCBlockHeight_BadFrame(t *testing.T) {
	resp := makeGRPCRelayResponse(t, 200, "0", []byte{0x00, 0x01})

	_, err := decodeGRPCBlockHeight(resp)

	require.Error(t, err)
	require.ErrorContains(t, err, "frame")
}
