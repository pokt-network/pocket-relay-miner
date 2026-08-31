//go:build test

package relay

import (
	"encoding/binary"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	sdktypes "github.com/pokt-network/shannon-sdk/types"
)

// resetGRPCTargetFlags restores the package globals the tests drive, so a
// failing case cannot leak its flag into the next one.
func resetGRPCTargetFlags(t *testing.T) {
	t.Helper()
	prevMethod, prevHex := RelayGRPCMethod, RelayGRPCRequestHex
	t.Cleanup(func() { RelayGRPCMethod, RelayGRPCRequestHex = prevMethod, prevHex })
	RelayGRPCMethod, RelayGRPCRequestHex = "", ""
}

// TestResolveGRPCMethodPath pins the shapes accepted and rejected. The rejected
// ones matter more than the accepted ones: measured on Go 1.26, http.NewRequest
// takes "/demo.DemoService", "/a/b/c" and "" without error and carries them
// through as URL.Path, so without this check a typo is signed, metered against
// the application's stake and mined before anything notices.
func TestResolveGRPCMethodPath(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		want    string
		wantErr bool
	}{
		{name: "unset falls back to the demo method", flag: "", want: demoGRPCMethodPath},
		{name: "demo GetBlock", flag: "/demo.DemoService/GetBlock", want: "/demo.DemoService/GetBlock"},
		{name: "demo HealthCheck", flag: "/demo.DemoService/HealthCheck", want: "/demo.DemoService/HealthCheck"},
		{name: "cosmos service", flag: "/cosmos.base.tendermint.v1beta1.Service/GetLatestBlock", want: "/cosmos.base.tendermint.v1beta1.Service/GetLatestBlock"},
		{name: "whitespace is trimmed", flag: "  /demo.DemoService/GetBlock  ", want: "/demo.DemoService/GetBlock"},
		{name: "no leading slash rejected", flag: "demo.DemoService/GetBlock", wantErr: true},
		{name: "no method rejected", flag: "/demo.DemoService", wantErr: true},
		{name: "empty method rejected", flag: "/demo.DemoService/", wantErr: true},
		{name: "empty service rejected", flag: "//GetBlock", wantErr: true},
		{name: "extra segment rejected", flag: "/demo.DemoService/Get/Extra", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGRPCTargetFlags(t)
			RelayGRPCMethod = tt.flag

			got, err := resolveGRPCMethodPath()
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "--grpc-method",
					"the error must name the flag the operator typed")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestBuildGRPCRequestFrame_WrapsTheMessage checks the frame header, which is
// the part an operator cannot see. 082a is a real demo.BlockRequest{number:42},
// so this is the exact body `--grpc-method /demo.DemoService/GetBlock
// --grpc-request-hex 082a` sends against the Tilt localnet.
func TestBuildGRPCRequestFrame_WrapsTheMessage(t *testing.T) {
	tests := []struct {
		name    string
		hex     string
		wantMsg []byte
		wantErr string
	}{
		{name: "empty yields the zero-length frame", hex: "", wantMsg: []byte{}},
		{name: "demo BlockRequest number=42", hex: "082a", wantMsg: []byte{0x08, 0x2a}},
		{name: "whitespace tolerated", hex: "  082a  ", wantMsg: []byte{0x08, 0x2a}},
		{name: "odd length rejected", hex: "082", wantErr: "--grpc-request-hex"},
		{name: "non-hex rejected", hex: "zzzz", wantErr: "--grpc-request-hex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGRPCTargetFlags(t)
			RelayGRPCRequestHex = tt.hex

			frame, err := buildGRPCRequestFrame()
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr,
					"the error must name the flag, not just say 'invalid hex'")
				return
			}
			require.NoError(t, err)
			require.Len(t, frame, grpcFramePrefixLen+len(tt.wantMsg))
			require.Equal(t, byte(0x00), frame[0], "compression flag must be 0")
			require.Equal(t, uint32(len(tt.wantMsg)), binary.BigEndian.Uint32(frame[1:grpcFramePrefixLen]),
				"the length prefix must match the message, or the backend rejects the frame")
			require.Equal(t, tt.wantMsg, frame[grpcFramePrefixLen:])
		})
	}
}

// TestBuildNativeGRPCPayload_CarriesTheChosenTarget goes through the real
// serializer and reads the method and body back off the wire, so it goes red if
// the method is hardcoded again or the message stops being framed.
func TestBuildNativeGRPCPayload_CarriesTheChosenTarget(t *testing.T) {
	resetGRPCTargetFlags(t)
	// HealthCheck, not GetBlock: the default is GetBlockHeight, which CONTAINS
	// "GetBlock", so a Contains assert on GetBlock passes even when the flag is
	// discarded. Measured -- the first version of this test did exactly that and
	// stayed green with the defect injected.
	RelayGRPCMethod = "/demo.DemoService/HealthCheck"
	RelayGRPCRequestHex = "082a"

	payloadBz, err := buildNativeGRPCPayload()
	require.NoError(t, err)

	req, err := sdktypes.DeserializeHTTPRequest(payloadBz)
	require.NoError(t, err)
	parsed, err := url.Parse(req.Url)
	require.NoError(t, err)
	require.Equal(t, "/demo.DemoService/HealthCheck", parsed.Path,
		"the operator's method must reach the wire, and nothing else may")
	require.NotEqual(t, demoGRPCMethodPath, parsed.Path,
		"the default must not survive an explicit --grpc-method")
	require.Equal(t, []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x08, 0x2a}, req.BodyBz,
		"the request must be framed: 5-byte header then the protobuf message")
}

// TestValidateGRPCFrame_EmptyMessageIsCallerDecided pins the split that replaced
// the isDemoGRPCMethod predicate. The demo method's reply always carries fields,
// so empty is a failure there; an operator-chosen method may reply empty. The
// caller decides once, which is why a stale demo path can no longer relax this
// check without anyone noticing.
func TestValidateGRPCFrame_EmptyMessageIsCallerDecided(t *testing.T) {
	emptyFrame := []byte{0x00, 0x00, 0x00, 0x00, 0x00}

	require.Error(t, validateGRPCFrame(emptyFrame, true),
		"an empty reply to the demo method is a failure")
	require.NoError(t, validateGRPCFrame(emptyFrame, false),
		"an empty reply to an operator-chosen method is legitimate")

	// Malformedness is not caller-decided: both modes must reject these.
	for _, mode := range []bool{true, false} {
		require.Error(t, validateGRPCFrame([]byte{0x00, 0x00}, mode), "short frame")
		require.Error(t, validateGRPCFrame([]byte{0x01, 0x00, 0x00, 0x00, 0x00}, mode), "compressed frame")
		require.Error(t, validateGRPCFrame([]byte{0x00, 0x00, 0x00, 0x00, 0x09, 0x01}, mode), "length mismatch")
	}
}
