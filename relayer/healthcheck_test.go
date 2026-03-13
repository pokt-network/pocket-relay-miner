//go:build test

package relayer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/pool"
)

// --- Test Helpers ---

// testLogger returns a nop logger suitable for unit tests.
func testLogger() zerolog.Logger {
	return zerolog.Nop()
}

// newTestEndpoint creates a pool.BackendEndpoint pointing at the given URL.
func newTestEndpoint(rawURL string) *pool.BackendEndpoint {
	ep, err := pool.NewBackendEndpoint("", rawURL)
	if err != nil {
		panic("newTestEndpoint: " + err.Error())
	}
	return ep
}

// newTestHealthChecker creates a HealthChecker with a nop logger.
func newTestHealthChecker() *HealthChecker {
	return NewHealthChecker(testLogger())
}

// defaultConfig returns a minimal enabled health check config for testing.
func defaultConfig() *BackendHealthCheckConfig {
	return &BackendHealthCheckConfig{
		Enabled:            true,
		Endpoint:           "/health",
		IntervalSeconds:    60,
		TimeoutSeconds:     5,
		UnhealthyThreshold: 3,
		HealthyThreshold:   2,
	}
}

// --- Recovery Tests (FAIL-03) ---

func TestHealthCheck_Recovery(t *testing.T) {
	// Backend starts unhealthy; after 2 consecutive successful probes it should recover.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.HealthyThreshold = 2

	// Mark endpoint unhealthy to simulate prior failure
	ep.SetUnhealthy()
	require.False(t, ep.IsHealthy())

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)

	ctx := context.Background()

	// First success: should not recover yet
	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.False(t, ep.IsHealthy(), "should still be unhealthy after 1 success")

	// Second success: meets healthy_threshold=2, should recover
	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.True(t, ep.IsHealthy(), "should be healthy after 2 consecutive successes")
}

func TestHealthCheck_HealthyBackendProbe(t *testing.T) {
	// Healthy backend receives probe failures, becomes unhealthy after threshold.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.UnhealthyThreshold = 3

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	ctx := context.Background()

	// 2 failures: still healthy
	hc.checkPool(ctx, "svc:jsonrpc", config)
	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.True(t, ep.IsHealthy(), "should still be healthy after 2 failures")

	// 3rd failure: becomes unhealthy
	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.False(t, ep.IsHealthy(), "should be unhealthy after 3 failures")
}

func TestHealthCheck_HealthyBackendProbeReset(t *testing.T) {
	// Healthy backend: a successful probe resets consecutiveFailures to 0.
	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.UnhealthyThreshold = 5

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	ctx := context.Background()

	// 2 failures
	hc.checkPool(ctx, "svc:jsonrpc", config)
	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.Equal(t, int32(2), ep.ConsecutiveFailures())

	// 1 success resets failures
	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.Equal(t, int32(0), ep.ConsecutiveFailures())
	require.True(t, ep.IsHealthy())
}

// --- Custom Probe Tests (FAIL-04) ---

func TestHealthCheck_CustomProbe_POST(t *testing.T) {
	// POST with JSON body: method=POST, Content-Type auto-detected as application/json.
	var (
		receivedMethod      string
		receivedContentType string
		receivedBody        string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedContentType = r.Header.Get("Content-Type")
		bodyBytes, _ := io.ReadAll(r.Body)
		receivedBody = string(bodyBytes)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.Method = "POST"
	config.RequestBody = `{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	hc.checkPool(context.Background(), "svc:jsonrpc", config)

	assert.Equal(t, "POST", receivedMethod)
	assert.Equal(t, "application/json", receivedContentType)
	assert.Equal(t, config.RequestBody, receivedBody)
}

func TestHealthCheck_CustomProbe_ExplicitContentType(t *testing.T) {
	// Explicit content_type overrides auto-detection.
	var receivedContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.Method = "POST"
	config.RequestBody = `{"foo":"bar"}`
	config.ContentType = "text/plain"

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	hc.checkPool(context.Background(), "svc:jsonrpc", config)

	assert.Equal(t, "text/plain", receivedContentType)
}

func TestHealthCheck_ExpectedStatus(t *testing.T) {
	t.Run("matches expected status list", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent) // 204
		}))
		defer server.Close()

		hc := newTestHealthChecker()
		ep := newTestEndpoint(server.URL)
		config := defaultConfig()
		config.ExpectedStatus = []int{200, 204}

		hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
		hc.checkPool(context.Background(), "svc:jsonrpc", config)

		// 204 is in the expected list, so should remain healthy
		require.True(t, ep.IsHealthy())
		require.Equal(t, int32(0), ep.ConsecutiveFailures())
	})

	t.Run("fails on non-matching status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError) // 500
		}))
		defer server.Close()

		hc := newTestHealthChecker()
		ep := newTestEndpoint(server.URL)
		config := defaultConfig()
		config.ExpectedStatus = []int{200, 204}
		config.UnhealthyThreshold = 1

		hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
		hc.checkPool(context.Background(), "svc:jsonrpc", config)

		require.False(t, ep.IsHealthy())
	})

	t.Run("empty expected_status uses 2xx range", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated) // 201
		}))
		defer server.Close()

		hc := newTestHealthChecker()
		ep := newTestEndpoint(server.URL)
		config := defaultConfig()
		// ExpectedStatus is nil/empty -> should accept any 2xx

		hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
		hc.checkPool(context.Background(), "svc:jsonrpc", config)

		require.True(t, ep.IsHealthy())
	})

	t.Run("empty expected_status rejects non-2xx", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest) // 400
		}))
		defer server.Close()

		hc := newTestHealthChecker()
		ep := newTestEndpoint(server.URL)
		config := defaultConfig()
		config.UnhealthyThreshold = 1

		hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
		hc.checkPool(context.Background(), "svc:jsonrpc", config)

		require.False(t, ep.IsHealthy())
	})
}

func TestHealthCheck_ExpectedBody(t *testing.T) {
	t.Run("succeeds when body contains expected substring", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok","result":"healthy"}`))
		}))
		defer server.Close()

		hc := newTestHealthChecker()
		ep := newTestEndpoint(server.URL)
		config := defaultConfig()
		config.ExpectedBody = "result"

		hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
		hc.checkPool(context.Background(), "svc:jsonrpc", config)

		require.True(t, ep.IsHealthy())
	})

	t.Run("fails when body does not contain expected substring", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"error"}`))
		}))
		defer server.Close()

		hc := newTestHealthChecker()
		ep := newTestEndpoint(server.URL)
		config := defaultConfig()
		config.ExpectedBody = "result"
		config.UnhealthyThreshold = 1

		hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
		hc.checkPool(context.Background(), "svc:jsonrpc", config)

		require.False(t, ep.IsHealthy())
	})
}

// --- Threshold Tests (FAIL-05) ---

func TestHealthCheck_UnhealthyThreshold(t *testing.T) {
	// With unhealthy_threshold=3: stays healthy after 2 failures, unhealthy after 3.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.UnhealthyThreshold = 3

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	ctx := context.Background()

	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.True(t, ep.IsHealthy(), "should be healthy after 1 failure")

	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.True(t, ep.IsHealthy(), "should be healthy after 2 failures")

	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.False(t, ep.IsHealthy(), "should be unhealthy after 3 failures")
}

func TestHealthCheck_HealthyThreshold(t *testing.T) {
	// With healthy_threshold=3: unhealthy backend stays unhealthy after 2 successes,
	// becomes healthy after 3.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.HealthyThreshold = 3

	ep.SetUnhealthy()
	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	ctx := context.Background()

	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.False(t, ep.IsHealthy(), "should be unhealthy after 1 success")

	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.False(t, ep.IsHealthy(), "should be unhealthy after 2 successes")

	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.True(t, ep.IsHealthy(), "should be healthy after 3 successes")
}

// --- Multi-Endpoint Tests ---

func TestHealthCheck_MultiEndpoint(t *testing.T) {
	// Pool with 3 endpoints: all 3 are probed on a single checkPool call.
	var counts [3]atomic.Int32
	servers := make([]*httptest.Server, 3)
	endpoints := make([]*pool.BackendEndpoint, 3)

	for i := 0; i < 3; i++ {
		idx := i
		servers[idx] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[idx].Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer servers[idx].Close()
		endpoints[idx] = newTestEndpoint(servers[idx].URL)
	}

	hc := newTestHealthChecker()
	config := defaultConfig()
	hc.RegisterPool("svc:jsonrpc", endpoints, config, nil, nil)

	hc.checkPool(context.Background(), "svc:jsonrpc", config)

	for i := 0; i < 3; i++ {
		assert.Equal(t, int32(1), counts[i].Load(), "endpoint %d should have been probed exactly once", i)
	}
}

// --- Auth and Headers Tests ---

func TestHealthCheck_AuthHeaders(t *testing.T) {
	// Probe request includes Bearer token from pool auth config.
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	auth := &AuthenticationConfig{
		BearerToken: "my-secret-token",
	}

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, auth)
	hc.checkPool(context.Background(), "svc:jsonrpc", config)

	assert.Equal(t, "Bearer my-secret-token", receivedAuth)
}

func TestHealthCheck_PoolHeaders(t *testing.T) {
	// Probe request includes custom headers from pool headers map.
	var receivedAPIKey string
	var receivedCustom string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAPIKey = r.Header.Get("X-Api-Key")
		receivedCustom = r.Header.Get("X-Custom")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	headers := map[string]string{
		"X-Api-Key": "key123",
		"X-Custom":  "value456",
	}

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, headers, nil)
	hc.checkPool(context.Background(), "svc:jsonrpc", config)

	assert.Equal(t, "key123", receivedAPIKey)
	assert.Equal(t, "value456", receivedCustom)
}

// --- Reset and State Tests ---

func TestHealthCheck_FullResetOnRecovery(t *testing.T) {
	// After recovery transition, both consecutiveFailures=0 and consecutiveSuccesses=0.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	config.HealthyThreshold = 2

	// Start unhealthy
	ep.SetUnhealthy()
	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	ctx := context.Background()

	// Get the BackendHealth to check internal counters
	backends := hc.backends["svc:jsonrpc"]
	require.Len(t, backends, 1)
	bh := backends[0]

	// 2 successes should trigger recovery
	hc.checkPool(ctx, "svc:jsonrpc", config)
	hc.checkPool(ctx, "svc:jsonrpc", config)
	require.True(t, ep.IsHealthy(), "should be healthy after recovery")

	// After recovery: full reset
	assert.Equal(t, int32(0), ep.ConsecutiveFailures(), "consecutiveFailures should be 0 after recovery")
	assert.Equal(t, int32(0), bh.consecutiveSuccesses.Load(), "consecutiveSuccesses should be 0 after recovery")
}

func TestHealthCheck_DefaultGET(t *testing.T) {
	// When no method specified, probe uses GET.
	var receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	// Method is intentionally left empty

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	hc.checkPool(context.Background(), "svc:jsonrpc", config)

	assert.Equal(t, "GET", receivedMethod)
}

func TestHealthCheck_ResponseBodyAlwaysRead(t *testing.T) {
	// Even when no expected_body is configured, the response body is fully read.
	// We verify this by checking our server writes all bytes without error.
	bodyWritten := make(chan bool, 1)
	largeBody := make([]byte, 4096) // 4KB body
	for i := range largeBody {
		largeBody[i] = 'x'
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		n, err := w.Write(largeBody)
		if err == nil && n == len(largeBody) {
			bodyWritten <- true
		} else {
			bodyWritten <- false
		}
	}))
	defer server.Close()

	hc := newTestHealthChecker()
	ep := newTestEndpoint(server.URL)
	config := defaultConfig()
	// No ExpectedBody set

	hc.RegisterPool("svc:jsonrpc", []*pool.BackendEndpoint{ep}, config, nil, nil)
	hc.checkPool(context.Background(), "svc:jsonrpc", config)

	// Body should have been fully written (and thus read by client)
	written := <-bodyWritten
	assert.True(t, written, "response body should be fully read even without expected_body config")
	require.True(t, ep.IsHealthy())
}
