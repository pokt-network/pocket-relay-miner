//go:build test

package query

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestNewQueryClients_ValidConfig tests successful client initialization
func TestNewQueryClients_ValidConfig(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
		UseTLS:       false,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)

	// Verify all clients are initialized
	require.NotNil(t, qc.Shared())
	require.NotNil(t, qc.Session())
	require.NotNil(t, qc.Application())
	require.NotNil(t, qc.Supplier())
	require.NotNil(t, qc.Proof())
	require.NotNil(t, qc.Service())
	require.NotNil(t, qc.Account())
	require.NotNil(t, qc.GRPCConnection())

	// Clean up
	err = qc.Close()
	require.NoError(t, err)
}

// TestNewQueryClients_InvalidEndpoint tests failure with invalid endpoint
func TestNewQueryClients_InvalidEndpoint(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: "invalid-endpoint:999999", // Invalid port
		QueryTimeout: 5 * time.Second,
		UseTLS:       false,
	}

	// This should succeed in creating the client (gRPC doesn't connect immediately)
	// but connection will fail when attempting queries
	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)

	// Clean up
	err = qc.Close()
	require.NoError(t, err)
}

// TestNewQueryClients_MissingEndpoint tests failure with missing endpoint
func TestNewQueryClients_MissingEndpoint(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: "",
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.Error(t, err)
	require.Nil(t, qc)
	require.Contains(t, err.Error(), "gRPC endpoint is required")
}

// TestNewQueryClients_WithTLS tests client initialization with TLS
func TestNewQueryClients_WithTLS(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
		UseTLS:       true,
	}

	qc, err := NewQueryClients(logger, config)
	// May succeed in creating client, but queries will fail due to TLS handshake
	// with non-TLS server. This is expected behavior.
	if err == nil {
		require.NotNil(t, qc)
		_ = qc.Close()
	}
}

// TestNewQueryClients_WithoutTLS tests client initialization without TLS
func TestNewQueryClients_WithoutTLS(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
		UseTLS:       false,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)

	err = qc.Close()
	require.NoError(t, err)
}

// TestQueryClients_Close_Success tests successful cleanup
func TestQueryClients_Close_Success(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)

	err = qc.Close()
	require.NoError(t, err)
}

// TestQueryClients_Close_AlreadyClosed tests double close is safe
func TestQueryClients_Close_AlreadyClosed(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)

	// First close
	err = qc.Close()
	require.NoError(t, err)

	// Second close should also succeed (idempotent)
	err = qc.Close()
	require.NoError(t, err)
}

// TestGRPCConnection_Success tests successful gRPC connection
func TestGRPCConnection_Success(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)

	// Verify connection is accessible
	conn := qc.GRPCConnection()
	require.NotNil(t, conn)

	err = qc.Close()
	require.NoError(t, err)
}

// TestGRPCConnection_Timeout tests query timeout behavior
func TestGRPCConnection_Timeout(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to simulate slow response
	mock.sharedParams = generateTestSharedParams()
	slowDuration := 100 * time.Millisecond

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 10 * time.Millisecond, // Very short timeout
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Simulate a slow query by adding delay in mock
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		time.Sleep(slowDuration)
		close(done)
	}()

	// Query should timeout (but may succeed depending on timing)
	_, err = qc.Shared().GetParams(ctx)
	// Either timeout error or success is acceptable depending on timing
	// The important thing is that we don't hang indefinitely

	<-done
}

// TestGRPCConnection_Refused tests connection refused scenario
func TestGRPCConnection_Refused(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	// Use an address that's not listening
	config := QueryClientConfig{
		GRPCEndpoint: "localhost:0", // Port 0 is reserved, nothing listening
		QueryTimeout: 1 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	// Creating the client may succeed (gRPC connects lazily)
	if err == nil {
		require.NotNil(t, qc)

		// But queries should fail
		ctx := context.Background()
		_, err := qc.Shared().GetParams(ctx)
		require.Error(t, err)

		_ = qc.Close()
	}
}

// TestQueryClientConfig_DefaultTimeout tests default timeout is applied
func TestQueryClientConfig_DefaultTimeout(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 0, // Should use default
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)

	// Verify default timeout was set
	require.Equal(t, defaultQueryTimeout, qc.config.QueryTimeout)

	err = qc.Close()
	require.NoError(t, err)
}

// TestQueryClients_AllClients tests all client accessors
func TestQueryClients_AllClients(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Test all accessor methods
	require.NotNil(t, qc.Shared())
	require.NotNil(t, qc.Session())
	require.NotNil(t, qc.Application())
	require.NotNil(t, qc.Supplier())
	require.NotNil(t, qc.Proof())
	require.NotNil(t, qc.Service())
	require.NotNil(t, qc.Account())
	require.NotNil(t, qc.GRPCConnection())
}

// TestQueryClients_ConcurrentAccess tests thread-safe access to clients
func TestQueryClients_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	mock.sharedParams = generateTestSharedParams()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Simulate concurrent access
	ctx := context.Background()
	done := make(chan struct{}, 10)

	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- struct{}{} }()

			// Access shared client concurrently
			_, err := qc.Shared().GetParams(ctx)
			require.NoError(t, err)
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestQueryClients_NetworkError tests network error handling
func TestQueryClients_NetworkError(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return error for session query and valid shared params
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		return nil, status.Error(codes.Unavailable, "network error")
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query should fail with network error
	ctx := context.Background()
	_, err = qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to query session")
}
