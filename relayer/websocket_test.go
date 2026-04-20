//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/pool"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// noopRelayProcessor is a minimal stub satisfying RelayProcessor for tests
// that never reach relay processing (e.g., fast-fail tests).
type noopRelayProcessor struct{}

func (n *noopRelayProcessor) ProcessRelay(
	_ context.Context,
	_, _ []byte,
	_, _ string,
	_ int64,
) (*transport.MinedRelayMessage, error) {
	return nil, nil
}

func (n *noopRelayProcessor) GetServiceDifficulty(
	_ context.Context,
	_ string,
	_ int64,
) ([]byte, error) {
	return nil, nil
}

func (n *noopRelayProcessor) SetDifficultyProvider(_ DifficultyProvider) {}

// noopPublisher is a minimal stub satisfying transport.MinedRelayPublisher
// for tests that never reach relay publishing (e.g., fast-fail tests).
type noopPublisher struct{}

func (n *noopPublisher) Publish(_ context.Context, _ *transport.MinedRelayMessage) error {
	return nil
}

func (n *noopPublisher) PublishBatch(_ context.Context, _ []*transport.MinedRelayMessage) error {
	return nil
}

func (n *noopPublisher) Close() error {
	return nil
}

// TestWebSocketHandler_FastFail_AllUnhealthy verifies that the WebSocket handler
// returns HTTP 503 (not 101 Switching Protocols) when all pool endpoints are unhealthy.
func TestWebSocketHandler_FastFail_AllUnhealthy(t *testing.T) {
	ep1, err := pool.NewBackendEndpoint("ws1", "ws://node1:8545")
	require.NoError(t, err)
	ep2, err := pool.NewBackendEndpoint("ws2", "ws://node2:8545")
	require.NoError(t, err)
	ep1.SetUnhealthy()
	ep2.SetUnhealthy()

	wsPool := pool.NewPool(
		"develop-websocket:websocket",
		[]*pool.BackendEndpoint{ep1, ep2},
		&pool.FirstHealthySelector{},
		"first_healthy(test)",
	)

	cfg := &Config{
		Services: map[string]ServiceConfig{
			"develop-websocket": {},
		},
		pools: map[string]*pool.Pool{
			"develop-websocket:websocket": wsPool,
		},
	}

	proxy := &ProxyServer{
		logger:         testLogger(),
		config:         cfg,
		responseSigner: &ResponseSigner{},
		relayProcessor: &noopRelayProcessor{},
		publisher:      &noopPublisher{},
	}

	handler := proxy.WebSocketHandler()

	req := httptest.NewRequest("GET", "/v1", nil)
	req.Header.Set("Target-Service-Id", "develop-websocket")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotEqual(t, http.StatusSwitchingProtocols, w.Code)
	require.Contains(t, w.Body.String(), "service temporarily unavailable")
}

// TestWebSocketHandler_FastFail_NilPool verifies that the WebSocket handler
// returns HTTP 503 when no pool is configured for the requested service.
func TestWebSocketHandler_FastFail_NilPool(t *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"develop-websocket": {},
		},
		pools: map[string]*pool.Pool{}, // no pool for the service
	}

	proxy := &ProxyServer{
		logger:         testLogger(),
		config:         cfg,
		responseSigner: &ResponseSigner{},
		relayProcessor: &noopRelayProcessor{},
		publisher:      &noopPublisher{},
	}

	handler := proxy.WebSocketHandler()

	req := httptest.NewRequest("GET", "/v1", nil)
	req.Header.Set("Target-Service-Id", "develop-websocket")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotEqual(t, http.StatusSwitchingProtocols, w.Code)
	require.Contains(t, w.Body.String(), "service temporarily unavailable")
}

// TestNewWebSocketBridge_RequiresRelayProcessor locks in the fix for the
// silent-data-loss fallback that used to kick in when relayProcessor was nil:
// MinedRelayMessage{RelayHash: nil, CU: 1} collapsed every event to the same
// empty-key SMST leaf and underbilled CU. The constructor now refuses nil so
// the error surfaces at wiring time instead of at revenue-reconciliation time.
func TestNewWebSocketBridge_RequiresRelayProcessor(t *testing.T) {
	bridge, err := NewWebSocketBridge(
		zerolog.Nop(),
		nil, // gatewayConn - not reached because the nil check fires first
		"ws://backend:8545",
		"svc-test",
		"pokt1supplier",
		0,
		nil, // relayProcessor - intentionally nil
		&noopPublisher{},
		nil,
		http.Header{},
		nil,
		nil,
		1,
		time.Second,
	)
	require.Error(t, err, "nil relayProcessor must fail fast — the old fallback silently collapsed events")
	assert.Nil(t, bridge)
	assert.Contains(t, err.Error(), "RelayProcessor", "error must point at the missing dependency")
}
