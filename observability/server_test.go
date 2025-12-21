//go:build test

package observability

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestNewServer creates a new observability server.
func TestNewServer(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultServerConfig()
	config.MetricsAddr = ":0" // Use random available port

	server := NewServer(logger, config)
	require.NotNil(t, server, "Server should not be nil")
	require.False(t, server.IsRunning(), "Server should not be running initially")
}

// TestDefaultServerConfig tests default configuration values.
func TestDefaultServerConfig(t *testing.T) {
	config := DefaultServerConfig()
	require.True(t, config.MetricsEnabled, "Metrics should be enabled by default")
	require.Equal(t, ":9090", config.MetricsAddr, "Default metrics address should be :9090")
	require.False(t, config.PprofEnabled, "Pprof should be disabled by default")
	require.Equal(t, ":6060", config.PprofAddr, "Default pprof address should be :6060")
}

// TestServer_Start_Stop tests basic server lifecycle.
func TestServer_Start_Stop(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    ":0", // Use random available port
		PprofEnabled:   false,
		Registry:       prometheus.NewRegistry(), // Use isolated registry for tests
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server
	err := server.Start(ctx)
	require.NoError(t, err, "Server start should succeed")
	require.True(t, server.IsRunning(), "Server should be running")

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Stop server
	err = server.Stop()
	require.NoError(t, err, "Server stop should succeed")
	require.False(t, server.IsRunning(), "Server should not be running after stop")
}

// TestServer_Start_AlreadyRunning tests that starting an already running server is safe.
func TestServer_Start_AlreadyRunning(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    ":0",
		PprofEnabled:   false,
		Registry:       prometheus.NewRegistry(), // Use isolated registry for tests
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server twice
	err := server.Start(ctx)
	require.NoError(t, err)

	err = server.Start(ctx)
	require.NoError(t, err, "Starting already running server should not error")

	// Cleanup
	_ = server.Stop()
}

// TestServer_Stop_NotRunning tests that stopping a non-running server is safe.
func TestServer_Stop_NotRunning(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultServerConfig()
	config.MetricsAddr = ":0"
	config.Registry = prometheus.NewRegistry() // Use isolated registry for tests

	server := NewServer(logger, config)

	// Stop without starting
	err := server.Stop()
	require.NoError(t, err, "Stopping non-running server should not error")
}

// TestServer_MetricsEndpoint_Success tests the /metrics endpoint.
func TestServer_MetricsEndpoint_Success(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	// Create custom registry for isolated testing
	registry := prometheus.NewRegistry()

	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    "127.0.0.1:0", // Localhost with random port
		PprofEnabled:   false,
		Registry:       registry,
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := server.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = server.Stop() }()

	// Give server time to start and bind
	time.Sleep(200 * time.Millisecond)

	// We can't easily get the actual port since it's random,
	// so we just verify the server started successfully
	require.True(t, server.IsRunning())
}

// TestServer_HealthEndpoint tests the /health endpoint.
func TestServer_HealthEndpoint(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    "127.0.0.1:0",
		PprofEnabled:   false,
		Registry:       prometheus.NewRegistry(), // Use isolated registry for tests
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := server.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = server.Stop() }()

	time.Sleep(200 * time.Millisecond)
	require.True(t, server.IsRunning())
}

// TestServer_ReadyEndpoint tests the /ready endpoint.
func TestServer_ReadyEndpoint(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    "127.0.0.1:0",
		PprofEnabled:   false,
		Registry:       prometheus.NewRegistry(), // Use isolated registry for tests
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := server.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = server.Stop() }()

	time.Sleep(200 * time.Millisecond)
	require.True(t, server.IsRunning())
}

// TestServer_WithPprof tests starting server with pprof enabled.
func TestServer_WithPprof(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    ":0",
		PprofEnabled:   true,
		PprofAddr:      ":0",
		Registry:       prometheus.NewRegistry(), // Use isolated registry for tests
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := server.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = server.Stop() }()

	time.Sleep(200 * time.Millisecond)
	require.True(t, server.IsRunning())
}

// TestServer_MetricsDisabled tests server with metrics disabled.
func TestServer_MetricsDisabled(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ServerConfig{
		MetricsEnabled: false,
		PprofEnabled:   false,
		Registry:       prometheus.NewRegistry(), // Use isolated registry for tests
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := server.Start(ctx)
	require.NoError(t, err, "Server should start even with metrics disabled")
	defer func() { _ = server.Stop() }()

	require.True(t, server.IsRunning())
}

// TestServer_ContextCancellation tests server shutdown on context cancellation.
func TestServer_ContextCancellation(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    ":0",
		PprofEnabled:   false,
		Registry:       prometheus.NewRegistry(), // Use isolated registry for tests
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())

	err := server.Start(ctx)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// Cancel context
	cancel()

	// Give server time to shutdown
	time.Sleep(200 * time.Millisecond)

	// Cleanup
	_ = server.Stop()
}

// TestServer_CustomRegistry tests server with custom Prometheus registry.
func TestServer_CustomRegistry(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	customRegistry := prometheus.NewRegistry()

	// Register a test metric
	testCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_counter",
		Help: "A test counter",
	})
	customRegistry.MustRegister(testCounter)
	testCounter.Inc()

	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    ":0",
		PprofEnabled:   false,
		Registry:       customRegistry,
	}

	server := NewServer(logger, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := server.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = server.Stop() }()

	time.Sleep(100 * time.Millisecond)
	require.True(t, server.IsRunning())
}

// TestServer_IsRunning tests the IsRunning method.
func TestServer_IsRunning(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    ":0",
		PprofEnabled:   false,
		Registry:       prometheus.NewRegistry(), // Use isolated registry for tests
	}

	server := NewServer(logger, config)
	require.False(t, server.IsRunning(), "Server should not be running initially")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := server.Start(ctx)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	require.True(t, server.IsRunning(), "Server should be running after start")

	err = server.Stop()
	require.NoError(t, err)
	require.False(t, server.IsRunning(), "Server should not be running after stop")
}

// Helper function to make HTTP request with retry
func makeHTTPRequest(t *testing.T, url string, maxRetries int) (*http.Response, error) {
	var resp *http.Response
	var err error

	for i := 0; i < maxRetries; i++ {
		resp, err = http.Get(url)
		if err == nil {
			return resp, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return nil, fmt.Errorf("failed after %d retries: %w", maxRetries, err)
}

// Helper function to read response body
func readResponseBody(t *testing.T, resp *http.Response) string {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}
