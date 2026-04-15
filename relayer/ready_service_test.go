//go:build test

package relayer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReadyTestProxy builds a minimal ProxyServer wired with enough state
// for the /ready/{service} handler to run: config with services, pool
// profiles validated, and a built client pool so p.clientPool is non-nil.
// The backend pool registry is populated via BuildPools so IsHealthy()
// works on real endpoints.
func newReadyTestProxy(t *testing.T, services map[string]ServiceConfig) *ProxyServer {
	t.Helper()
	c := &Config{
		ListenAddr:            "0.0.0.0:8080",
		Redis:                 RedisConfig{URL: "redis://localhost:6379"},
		PocketNode:            PocketNodeConfig{QueryNodeRPCUrl: "http://x", QueryNodeGRPCUrl: "x:9090"},
		DefaultValidationMode: ValidationModeOptimistic,
		HTTPTransport: HTTPTransportConfig{
			MaxConnsPerHost:        500,
			MaxIdleConnsPerHost:    100,
			IdleConnTimeoutSeconds: 90,
		},
		TimeoutProfiles: map[string]TimeoutProfile{
			"fast": {
				Name: "fast", RequestTimeoutSeconds: 30,
				ResponseHeaderTimeoutSeconds: 30,
				DialTimeoutSeconds:           5,
				TLSHandshakeTimeoutSeconds:   10,
			},
			"streaming": {
				Name: "streaming", RequestTimeoutSeconds: 600,
				DialTimeoutSeconds:         10,
				TLSHandshakeTimeoutSeconds: 15,
			},
		},
		Services: services,
	}
	require.NoError(t, c.Validate(), "config validate")
	require.NoError(t, c.BuildPools(), "build pools")

	clients, fallback := buildClientPool(c, &c.HTTPTransport)
	return &ProxyServer{
		logger:             testLogger(),
		config:             c,
		clientPool:         clients,
		clientPoolFallback: fallback,
	}
}

// callReadyService does the HTTP plumbing for the tests: fires a request
// at /ready/<serviceID>, decodes the JSON body, and returns both the
// parsed struct and the HTTP status code for assertions.
func callReadyService(t *testing.T, p *ProxyServer, serviceID string) (readyServiceResponse, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ready/"+serviceID, nil)
	w := httptest.NewRecorder()
	p.handleRelay(w, req) // routing through the real entry point

	var resp readyServiceResponse
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode body")
	}
	return resp, w.Code
}

func TestReadyService_KnownServiceReturnsPoolAndBackends(t *testing.T) {
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"op": {
			TimeoutProfile: "fast",
			PoolProfile:    "high",
			DefaultBackend: "jsonrpc",
			Backends: map[string]BackendConfig{
				"jsonrpc": {
					URLs: []BackendEndpointConfig{
						{URL: "http://op-primary:8545"},
						{Name: "backup", URL: "http://op-backup:8545"},
					},
				},
			},
		},
	})
	// Simulate 3 in-flight requests so the gauge has a non-zero reading.
	httpPoolInFlight.WithLabelValues("op").Add(3)
	defer httpPoolInFlight.WithLabelValues("op").Sub(3)

	resp, code := callReadyService(t, p, "op")
	require.Equal(t, http.StatusOK, code)

	// Shape and identifying fields.
	assert.Equal(t, "op", resp.ServiceID)
	assert.True(t, resp.Ready, "ready with healthy backends")
	assert.Empty(t, resp.Error)

	// Pool block — every field verified, not just the first one, so
	// partial bugs (e.g. forgetting to copy idle_timeout) get caught.
	assert.Equal(t, "high", resp.Pool.Profile)
	assert.Equal(t, 250, resp.Pool.MaxConnsPerHost)
	assert.Equal(t, 50, resp.Pool.MaxIdleConnsPerHost)
	assert.Equal(t, int64(90), resp.Pool.IdleTimeoutSeconds)
	assert.Equal(t, int64(3), resp.Pool.InFlight)
	assert.InDelta(t, 1.2, resp.Pool.SaturationPct, 0.001,
		"3/250 = 1.2% saturation")

	// Backend block — both endpoints discovered and reported healthy.
	assert.Equal(t, 2, resp.Backends.Total)
	assert.Equal(t, 2, resp.Backends.Healthy)
	require.Len(t, resp.Backends.Endpoint, 2)
	urls := []string{resp.Backends.Endpoint[0].URL, resp.Backends.Endpoint[1].URL}
	assert.Contains(t, urls, "http://op-primary:8545")
	assert.Contains(t, urls, "http://op-backup:8545")
}

func TestReadyService_UnknownServiceReturns404(t *testing.T) {
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"op": {
			DefaultBackend: "jsonrpc",
			Backends: map[string]BackendConfig{
				"jsonrpc": {URL: "http://op:8545"},
			},
		},
	})

	resp, code := callReadyService(t, p, "never-configured")
	assert.Equal(t, http.StatusNotFound, code)
	assert.False(t, resp.Ready)
	assert.Equal(t, "never-configured", resp.ServiceID)
	assert.Contains(t, resp.Error, "unknown service")
}

func TestReadyService_EmptyServiceIDReturns400(t *testing.T) {
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"op": {
			DefaultBackend: "jsonrpc",
			Backends:       map[string]BackendConfig{"jsonrpc": {URL: "http://op:8545"}},
		},
	})
	// Hit /ready/ with a trailing slash and nothing after it.
	req := httptest.NewRequest(http.MethodGet, "/ready/", nil)
	w := httptest.NewRecorder()
	p.handleRelay(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var resp readyServiceResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp.Error, "required")
}

func TestReadyService_NoPoolProfileFallsBackToGlobals(t *testing.T) {
	// A service without pool_profile must still return sensible pool data:
	// the global HTTPTransport defaults. Previously a nil dereference
	// here would have crashed the handler.
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"avax": {
			TimeoutProfile: "fast",
			DefaultBackend: "jsonrpc",
			Backends: map[string]BackendConfig{
				"jsonrpc": {URL: "http://avax:9650"},
			},
		},
	})

	resp, code := callReadyService(t, p, "avax")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, resp.Pool.Profile, "no profile name when unset")
	assert.Equal(t, 500, resp.Pool.MaxConnsPerHost, "inherits HTTPTransport default")
	assert.Equal(t, 100, resp.Pool.MaxIdleConnsPerHost)
	assert.Equal(t, int64(90), resp.Pool.IdleTimeoutSeconds)
	assert.Equal(t, int64(0), resp.Pool.InFlight)
	assert.Equal(t, 0.0, resp.Pool.SaturationPct)
}

func TestReadyService_ReturnsServiceUnavailableWhenAllBackendsDown(t *testing.T) {
	// Create a service with one backend, then mark it unhealthy and
	// check that the endpoint flips to 503 + ready=false.
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"op": {
			PoolProfile:    "low",
			DefaultBackend: "jsonrpc",
			Backends: map[string]BackendConfig{
				"jsonrpc": {URL: "http://op-primary:8545"},
			},
		},
	})

	bp := p.config.GetPool("op", "jsonrpc")
	require.NotNil(t, bp)
	for _, ep := range bp.All() {
		ep.SetUnhealthy()
	}

	resp, code := callReadyService(t, p, "op")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.False(t, resp.Ready)
	assert.Equal(t, 1, resp.Backends.Total)
	assert.Equal(t, 0, resp.Backends.Healthy)
	require.Len(t, resp.Backends.Endpoint, 1)
	assert.False(t, resp.Backends.Endpoint[0].Healthy)
}

func TestReadyService_PartialBackendOutageStaysReady(t *testing.T) {
	// Two backends: one healthy, one dead. The service is still
	// servable, so ready must be true and 200.
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"op": {
			PoolProfile:    "low",
			DefaultBackend: "jsonrpc",
			Backends: map[string]BackendConfig{
				"jsonrpc": {
					URLs: []BackendEndpointConfig{
						{Name: "primary", URL: "http://op-primary:8545"},
						{Name: "backup", URL: "http://op-backup:8545"},
					},
				},
			},
		},
	})

	bp := p.config.GetPool("op", "jsonrpc")
	require.NotNil(t, bp)
	all := bp.All()
	require.Len(t, all, 2)
	// Trip only the first endpoint.
	all[0].SetUnhealthy()

	resp, code := callReadyService(t, p, "op")
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, resp.Ready)
	assert.Equal(t, 2, resp.Backends.Total)
	assert.Equal(t, 1, resp.Backends.Healthy)
}

func TestReadyService_SaturationPercentReflectsInFlight(t *testing.T) {
	// Cover several saturation levels against a low (max=10) pool:
	// 0, 5, 10 in-flight → 0, 50, 100%.
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"op": {
			PoolProfile:    "low",
			DefaultBackend: "jsonrpc",
			Backends: map[string]BackendConfig{
				"jsonrpc": {URL: "http://op:8545"},
			},
		},
	})

	cases := []struct {
		inFlight float64
		wantPct  float64
	}{
		{0, 0},
		{5, 50},
		{10, 100},
	}
	for _, tc := range cases {
		httpPoolInFlight.WithLabelValues("op").Set(tc.inFlight)
		resp, code := callReadyService(t, p, "op")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(tc.inFlight), resp.Pool.InFlight)
		assert.InDelta(t, tc.wantPct, resp.Pool.SaturationPct, 0.001,
			"saturation for inFlight=%v", tc.inFlight)
	}
	// Reset so later tests see a clean gauge.
	httpPoolInFlight.WithLabelValues("op").Set(0)
}

func TestReadyService_JSONContentType(t *testing.T) {
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"op": {
			PoolProfile:    "low",
			DefaultBackend: "jsonrpc",
			Backends:       map[string]BackendConfig{"jsonrpc": {URL: "http://op:8545"}},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/ready/op", nil)
	w := httptest.NewRecorder()
	p.handleRelay(w, req)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
}

func TestReadyService_RoutingPrecedenceAggregateFirst(t *testing.T) {
	// A bare /ready (no service) must still hit the aggregate
	// /ready handler and return {status, block_height}, not the
	// per-service one. This protects the routing order.
	p := newReadyTestProxy(t, map[string]ServiceConfig{
		"op": {
			DefaultBackend: "jsonrpc",
			Backends:       map[string]BackendConfig{"jsonrpc": {URL: "http://op:8545"}},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()
	p.handleRelay(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"status":"healthy"`,
		"bare /ready must hit the aggregate handler")
}
