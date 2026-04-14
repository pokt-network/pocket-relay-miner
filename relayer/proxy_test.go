//go:build test

package relayer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/pool"
)

// --- sendServiceUnavailable Tests ---

func TestSendServiceUnavailable(t *testing.T) {
	p := &ProxyServer{
		logger: testLogger(),
	}

	w := httptest.NewRecorder()
	p.sendServiceUnavailable(w, "develop-http")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	assert.Contains(t, w.Body.String(), "service temporarily unavailable")
	assert.Contains(t, w.Body.String(), "develop-http")
}

// --- HTTP Fast-Fail Pre-Check Tests ---

func TestHTTPFastFail_AllUnhealthy(t *testing.T) {
	// When all backends for a service are unhealthy, HasHealthy() returns false.
	// The pre-check should cause a 503 fast-fail.
	ep1, _ := pool.NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := pool.NewBackendEndpoint("b", "http://node2:8545")
	ep1.SetUnhealthy()
	ep2.SetUnhealthy()

	p := pool.NewPool("svc:jsonrpc", []*pool.BackendEndpoint{ep1, ep2}, &pool.FirstHealthySelector{}, "first_healthy(test)")

	// Simulate the pre-check logic
	require.False(t, p.HasHealthy(), "pool with all unhealthy endpoints should return false")

	// Verify the fast-fail response
	proxy := &ProxyServer{logger: testLogger()}
	w := httptest.NewRecorder()
	proxy.sendServiceUnavailable(w, "develop-http")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "service temporarily unavailable")
	assert.Contains(t, w.Body.String(), "develop-http")
}

func TestHTTPFastFail_SomeHealthy(t *testing.T) {
	// When at least one backend is healthy, HasHealthy() returns true.
	// The pre-check should NOT trigger fast-fail.
	ep1, _ := pool.NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := pool.NewBackendEndpoint("b", "http://node2:8545")
	ep1.SetUnhealthy()
	// ep2 stays healthy

	p := pool.NewPool("svc:jsonrpc", []*pool.BackendEndpoint{ep1, ep2}, &pool.FirstHealthySelector{}, "first_healthy(test)")

	require.True(t, p.HasHealthy(), "pool with at least one healthy endpoint should return true")
}

func TestHTTPFastFail_NilPool(t *testing.T) {
	// When pool lookup returns nil (unknown service/rpcType), the pre-check
	// should cause a 503 fast-fail.
	cfg := &Config{
		pools: map[string]*pool.Pool{}, // empty pools
	}

	// Lookup for a service that does not exist
	result := cfg.GetPool("unknown-service", "jsonrpc")
	require.Nil(t, result, "GetPool for unknown service should return nil")

	// Verify the fast-fail response
	proxy := &ProxyServer{logger: testLogger()}
	w := httptest.NewRecorder()
	proxy.sendServiceUnavailable(w, "unknown-service")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// --- WebSocket Fast-Fail Pre-Check Tests ---

func TestWebSocketFastFail_AllUnhealthy(t *testing.T) {
	// When all WebSocket backends are unhealthy, the handler should return 503
	// BEFORE upgrading the connection (no 101 Switching Protocols).
	ep1, _ := pool.NewBackendEndpoint("ws1", "ws://node1:8545")
	ep2, _ := pool.NewBackendEndpoint("ws2", "ws://node2:8545")
	ep1.SetUnhealthy()
	ep2.SetUnhealthy()

	p := pool.NewPool("svc:websocket", []*pool.BackendEndpoint{ep1, ep2}, &pool.FirstHealthySelector{}, "first_healthy(test)")

	require.False(t, p.HasHealthy(), "pool with all unhealthy WebSocket endpoints should return false")

	// Simulate what should happen: 503 returned, no upgrade
	proxy := &ProxyServer{logger: testLogger()}
	w := httptest.NewRecorder()
	proxy.sendServiceUnavailable(w, "develop-websocket")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotEqual(t, http.StatusSwitchingProtocols, w.Code, "connection should NOT be upgraded")
	assert.Contains(t, w.Body.String(), "service temporarily unavailable")
}

func TestWebSocketFastFail_SomeHealthy(t *testing.T) {
	// When at least one WebSocket backend is healthy, the handler should proceed
	// past the pre-check (not return 503).
	ep1, _ := pool.NewBackendEndpoint("ws1", "ws://node1:8545")
	ep2, _ := pool.NewBackendEndpoint("ws2", "ws://node2:8545")
	ep1.SetUnhealthy()
	// ep2 stays healthy

	p := pool.NewPool("svc:websocket", []*pool.BackendEndpoint{ep1, ep2}, &pool.FirstHealthySelector{}, "first_healthy(test)")

	require.True(t, p.HasHealthy(), "pool with at least one healthy WebSocket endpoint should return true")
}

func TestMergeBackendPath(t *testing.T) {
	tests := []struct {
		name       string
		urlPath    string
		basePath   string
		clientPath string
		want       string
	}{
		// base_path precedence over urlPath.
		{"basePath only, empty client", "", "/ext/bc/C/rpc", "", "/ext/bc/C/rpc"},
		{"basePath only, slash client", "", "/ext/bc/C/rpc", "/", "/ext/bc/C/rpc"},
		{"basePath, client already prefixed", "", "/ext/bc/C/rpc", "/ext/bc/C/rpc", "/ext/bc/C/rpc"},
		{"basePath, client already prefixed with subpath", "", "/ext/bc/C/rpc", "/ext/bc/C/rpc/v1/users", "/ext/bc/C/rpc/v1/users"},
		{"basePath, client unrelated path gets prefixed", "", "/ext/bc/C/rpc", "/v1/users", "/ext/bc/C/rpc/v1/users"},
		{"basePath with trailing slash normalises", "", "/ext/bc/C/rpc/", "/", "/ext/bc/C/rpc"},
		{"basePath does not false-match substring", "", "/ext", "/extra/foo", "/ext/extra/foo"},

		// No basePath, legacy behaviour using urlPath.
		{"no basePath, urlPath fallback", "/ext/bc/C/rpc", "", "/", "/ext/bc/C/rpc"},
		{"no basePath, urlPath + subpath", "/ext/bc/C/rpc", "", "/v1/users", "/ext/bc/C/rpc/v1/users"},

		// Neither set.
		{"neither set, empty client", "", "", "", ""},
		{"neither set, slash client", "", "", "/", ""},
		{"neither set, real path", "", "", "/v1/users", "/v1/users"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeBackendPath(tt.urlPath, tt.basePath, tt.clientPath)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestShouldCompressResponse(t *testing.T) {
	tests := []struct {
		name        string
		cfg         ResponseCompressionConfig
		acceptsGzip bool
		payloadSize int
		want        bool
	}{
		{
			name:        "disabled in config — never compress",
			cfg:         ResponseCompressionConfig{Enabled: false, MinSizeBytes: 0},
			acceptsGzip: true,
			payloadSize: 10_000,
			want:        false,
		},
		{
			name:        "disabled + client does not accept gzip",
			cfg:         ResponseCompressionConfig{Enabled: false},
			acceptsGzip: false,
			payloadSize: 10_000,
			want:        false,
		},
		{
			name:        "enabled but client did not advertise gzip",
			cfg:         ResponseCompressionConfig{Enabled: true, MinSizeBytes: 1024},
			acceptsGzip: false,
			payloadSize: 10_000,
			want:        false,
		},
		{
			name:        "enabled + accepted + size above explicit threshold",
			cfg:         ResponseCompressionConfig{Enabled: true, MinSizeBytes: 1024},
			acceptsGzip: true,
			payloadSize: 2048,
			want:        true,
		},
		{
			name:        "enabled + accepted + size exactly at threshold",
			cfg:         ResponseCompressionConfig{Enabled: true, MinSizeBytes: 1024},
			acceptsGzip: true,
			payloadSize: 1024,
			want:        true,
		},
		{
			name:        "enabled + accepted + size below threshold",
			cfg:         ResponseCompressionConfig{Enabled: true, MinSizeBytes: 1024},
			acceptsGzip: true,
			payloadSize: 800,
			want:        false,
		},
		{
			name:        "enabled with MinSizeBytes=0 uses defaultGzipMinCompressSize (1024)",
			cfg:         ResponseCompressionConfig{Enabled: true, MinSizeBytes: 0},
			acceptsGzip: true,
			payloadSize: 1024,
			want:        true,
		},
		{
			name:        "enabled with MinSizeBytes=0 — payload below 1 KiB still skipped",
			cfg:         ResponseCompressionConfig{Enabled: true, MinSizeBytes: 0},
			acceptsGzip: true,
			payloadSize: 512,
			want:        false,
		},
		{
			name:        "enabled with negative MinSizeBytes falls back to default",
			cfg:         ResponseCompressionConfig{Enabled: true, MinSizeBytes: -1},
			acceptsGzip: true,
			payloadSize: 2048,
			want:        true,
		},
		{
			name:        "enabled with tiny MinSizeBytes — operator opted into small-payload mode",
			cfg:         ResponseCompressionConfig{Enabled: true, MinSizeBytes: 64},
			acceptsGzip: true,
			payloadSize: 100,
			want:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldCompressResponse(tt.cfg, tt.acceptsGzip, tt.payloadSize)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestDefaultConfig_ResponseCompressionDisabled pins the intentional default.
// Flipping this default would silently re-enable gzip on operators that never
// opted in and would erase the ~9 % CPU win documented in BREEZE-FIXES-SUMMARY.
// Change both the default and the operator communication if this test fails.
func TestDefaultConfig_ResponseCompressionDisabled(t *testing.T) {
	cfg := DefaultConfig()
	assert.False(t, cfg.ResponseCompression.Enabled, "default must be opt-in")
	assert.Equal(t, 1024, cfg.ResponseCompression.MinSizeBytes, "default min size should be 1 KiB")
}
