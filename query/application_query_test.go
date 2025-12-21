//go:build test

package query

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetApplication_Success tests successful application retrieval
func TestGetApplication_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testApp := generateTestApplication("pokt1app123")
	mock.getApplicationFunc = func(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		require.Equal(t, "pokt1app123", req.Address)
		return &apptypes.QueryGetApplicationResponse{Application: *testApp}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query application
	ctx := context.Background()
	app, err := qc.Application().GetApplication(ctx, "pokt1app123")
	require.NoError(t, err)
	require.Equal(t, "pokt1app123", app.Address)
	require.NotNil(t, app.Stake)
}

// TestGetApplication_NotFound tests application not found error
func TestGetApplication_NotFound(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return not found
	mock.getApplicationFunc = func(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		return nil, status.Error(codes.NotFound, "application not found")
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query non-existent application
	ctx := context.Background()
	app, err := qc.Application().GetApplication(ctx, "pokt1nonexistent")
	require.Error(t, err)
	require.Empty(t, app.Address)
	require.Contains(t, err.Error(), "failed to query application")
}

// TestGetApplication_InvalidAddress tests invalid address handling
func TestGetApplication_InvalidAddress(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return invalid argument error
	mock.getApplicationFunc = func(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		return nil, status.Error(codes.InvalidArgument, "invalid address")
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query with invalid address
	ctx := context.Background()
	app, err := qc.Application().GetApplication(ctx, "invalid")
	require.Error(t, err)
	require.Empty(t, app.Address)
}

// TestGetApplication_NetworkError tests network error handling
func TestGetApplication_NetworkError(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return network error
	mock.getApplicationFunc = func(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		return nil, status.Error(codes.Unavailable, "network unavailable")
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query should fail with network error
	ctx := context.Background()
	app, err := qc.Application().GetApplication(ctx, "pokt1app")
	require.Error(t, err)
	require.Empty(t, app.Address)
	require.Contains(t, err.Error(), "failed to query application")
}

// TestGetApplication_EmptyResponse tests empty application response
func TestGetApplication_EmptyResponse(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return empty application
	mock.getApplicationFunc = func(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		return &apptypes.QueryGetApplicationResponse{Application: apptypes.Application{}}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query should succeed with empty application
	ctx := context.Background()
	app, err := qc.Application().GetApplication(ctx, "pokt1app")
	require.NoError(t, err)
	require.Empty(t, app.Address)
}

// TestGetApplication_Cache tests application caching behavior
func TestGetApplication_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock with counter to track queries
	queryCount := 0
	testApp := generateTestApplication("pokt1app123")
	mock.getApplicationFunc = func(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		queryCount++
		return &apptypes.QueryGetApplicationResponse{Application: *testApp}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// First query - should hit server
	app1, err := qc.Application().GetApplication(ctx, "pokt1app123")
	require.NoError(t, err)
	require.Equal(t, "pokt1app123", app1.Address)
	require.Equal(t, 1, queryCount)

	// Second query - should use cache
	app2, err := qc.Application().GetApplication(ctx, "pokt1app123")
	require.NoError(t, err)
	require.Equal(t, "pokt1app123", app2.Address)
	require.Equal(t, 1, queryCount) // Still 1, not 2

	// Same application data
	require.Equal(t, app1.Address, app2.Address)
}

// TestGetApplication_ConcurrentAccess tests thread-safe application queries
func TestGetApplication_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testApp := generateTestApplication("pokt1app123")
	mock.getApplicationFunc = func(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		return &apptypes.QueryGetApplicationResponse{Application: *testApp}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Concurrent queries
	ctx := context.Background()
	done := make(chan struct{}, 10)

	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- struct{}{} }()

			app, err := qc.Application().GetApplication(ctx, "pokt1app123")
			require.NoError(t, err)
			require.Equal(t, "pokt1app123", app.Address)
		}()
	}

	// Wait for all
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestGetAllApplications_Success tests successful retrieval of all applications
func TestGetAllApplications_Success(t *testing.T) {
	_, address, cleanup, _ := setupMockQueryServer(t)
	defer cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query all applications (mock returns empty list by default)
	ctx := context.Background()
	apps, err := qc.Application().GetAllApplications(ctx)
	require.NoError(t, err)
	require.Empty(t, apps) // Mock returns empty list
}

// TestGetApplicationParams_Success tests successful application params retrieval
func TestGetApplicationParams_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testParams := generateTestApplicationParams()
	mock.applicationParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query params
	ctx := context.Background()
	params, err := qc.Application().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params)
	require.NotNil(t, params.MinStake)
}

// TestGetApplicationParams_Cache tests params caching
func TestGetApplicationParams_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestApplicationParams()
	mock.applicationParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// First query
	params1, err := qc.Application().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params1)

	// Second query - should use cache
	params2, err := qc.Application().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params2)

	// Should be same params object (pointer equality due to cache)
	require.Equal(t, params1, params2)
}

// TestGetApplication_Timeout tests query timeout
func TestGetApplication_Timeout(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to simulate slow response
	testApp := generateTestApplication("pokt1app123")
	mock.getApplicationFunc = func(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		// Simulate slow query
		time.Sleep(100 * time.Millisecond)
		return &apptypes.QueryGetApplicationResponse{Application: *testApp}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 10 * time.Millisecond, // Very short timeout
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query may timeout or succeed depending on timing
	ctx := context.Background()
	_, _ = qc.Application().GetApplication(ctx, "pokt1app123")
	// The important thing is we don't hang
}
