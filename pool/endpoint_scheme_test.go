//go:build test

package pool

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewBackendEndpoint_GRPCSchemesAreDialable is the test that a per-path fix
// cannot pass. Every consumer of an endpoint -- the gRPC relay path, the HTTP
// proxy path, the WebSocket path, the health checker, the breaker logs -- reads
// RawURL or URL. Asserting here that BOTH carry a dialable scheme covers all of
// them at once, which is why the rewrite lives in the constructor.
//
// Measured 2026-08-30: relayer/proxy.go and relayer/healthcheck.go contained no
// gRPC normalization, so a fix applied only to relay_grpc_service.go left a
// grpc:// backend undialable on two of three paths while `relayer validate`
// certified it as valid.
func TestNewBackendEndpoint_GRPCSchemesAreDialable(t *testing.T) {
	tests := []struct {
		name       string
		rawURL     string
		wantRaw    string
		wantScheme string
	}{
		{name: "grpc:// becomes h2c cleartext", rawURL: "grpc://node:9090", wantRaw: "http://node:9090", wantScheme: "http"},
		{name: "grpcs:// becomes TLS", rawURL: "grpcs://node:443", wantRaw: "https://node:443", wantScheme: "https"},
		{name: "grpc:// keeps its path", rawURL: "grpc://node:9090/prefix", wantRaw: "http://node:9090/prefix", wantScheme: "http"},
		{name: "bare host:port unchanged behaviour", rawURL: "node:9090", wantRaw: "node:9090", wantScheme: "http"},
		{name: "http:// untouched", rawURL: "http://node:9090", wantRaw: "http://node:9090", wantScheme: "http"},
		{name: "https:// untouched", rawURL: "https://node:443", wantRaw: "https://node:443", wantScheme: "https"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, err := NewBackendEndpoint("", tt.rawURL)
			require.NoError(t, err)
			require.Equal(t, tt.wantRaw, ep.RawURL,
				"RawURL is what the dialers, the health checker and the breaker logs read")
			require.Equal(t, tt.wantScheme, ep.URL.Scheme,
				"URL.Scheme is what proxy.go reads")
		})
	}
}

// TestNewBackendEndpoint_WebSocketSchemesSurvive guards the rewrite against
// eating the schemes the WebSocket dialer needs. gorilla requires ws:// or
// wss://; rewriting either would break every websocket backend.
func TestNewBackendEndpoint_WebSocketSchemesSurvive(t *testing.T) {
	for _, raw := range []string{"ws://node:8547", "wss://node:8547"} {
		ep, err := NewBackendEndpoint("", raw)
		require.NoError(t, err)
		require.Equal(t, raw, ep.RawURL, "websocket schemes must reach gorilla intact")
	}
}

// TestNormalizeGRPCScheme_LeavesEverythingElseAlone pins that the rewrite is
// narrow. It is called for every backend type, so anything it touches beyond
// the two gRPC conventions is a regression in another transport.
func TestNormalizeGRPCScheme_LeavesEverythingElseAlone(t *testing.T) {
	for _, raw := range []string{
		"http://a", "https://a", "ws://a", "wss://a", "ftp://a", "node:9090", "",
	} {
		require.Equal(t, raw, NormalizeGRPCScheme(raw), "must not rewrite %q", raw)
	}
}
