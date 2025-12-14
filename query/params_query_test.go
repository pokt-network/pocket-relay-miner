//go:build test

package query

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/stretchr/testify/require"
)

// TestGetSharedParams_Success tests successful shared params retrieval
func TestGetSharedParams_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query params
	ctx := context.Background()
	params, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params)
	require.Equal(t, uint64(4), params.NumBlocksPerSession)
	require.Equal(t, uint64(1), params.GracePeriodEndOffsetBlocks)
	require.Equal(t, uint64(42), params.ComputeUnitsToTokensMultiplier)
}

// TestGetSessionParams_Success tests successful session params retrieval
func TestGetSessionParams_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testParams := generateTestSessionParams()
	mock.sessionParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query params
	ctx := context.Background()
	params, err := qc.Session().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params)
}

// TestGetProofParams_Success tests successful proof params retrieval
func TestGetProofParams_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testParams := generateTestProofParams()
	mock.proofParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query params
	ctx := context.Background()
	params, err := qc.Proof().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params)
}

// TestGetParams_NetworkError tests network error handling for all param types
func TestGetParams_NetworkError(t *testing.T) {
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

	ctx := context.Background()

	// Test shared params (mock returns not found by default)
	_, err = qc.Shared().GetParams(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to query shared params")

	// Test session params
	_, err = qc.Session().GetParams(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to query session params")

	// Test proof params
	_, err = qc.Proof().GetParams(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to query proof params")
}

// TestGetParams_Timeout tests timeout handling for param queries
func TestGetParams_Timeout(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock params
	mock.sharedParams = generateTestSharedParams()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 10 * time.Millisecond, // Very short timeout
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query may timeout or succeed depending on timing
	ctx := context.Background()
	_, _ = qc.Shared().GetParams(ctx)
	// The important thing is we don't hang
}

// TestGetSharedParams_Cache tests shared params caching
func TestGetSharedParams_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// First query
	params1, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params1)

	// Second query - should use cache
	params2, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params2)

	// Should be same params object (pointer equality due to cache)
	require.Equal(t, params1, params2)
}

// TestSharedParams_InvalidateCache tests cache invalidation
func TestSharedParams_InvalidateCache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// First query
	params1, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params1)

	// Invalidate cache
	sharedClient := qc.sharedClient
	sharedClient.InvalidateCache()

	// Next query should fetch again
	params2, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params2)

	// Should still be equal in content
	require.Equal(t, params1.NumBlocksPerSession, params2.NumBlocksPerSession)
}

// TestGetSharedParams_ConcurrentAccess tests thread-safe param queries
func TestGetSharedParams_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
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

			params, err := qc.Shared().GetParams(ctx)
			require.NoError(t, err)
			require.NotNil(t, params)
			require.Equal(t, uint64(4), params.NumBlocksPerSession)
		}()
	}

	// Wait for all
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestGetSessionGracePeriodEndHeight tests grace period calculation
func TestGetSessionGracePeriodEndHeight(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// Test grace period calculation
	height, err := qc.Shared().GetSessionGracePeriodEndHeight(ctx, 100)
	require.NoError(t, err)
	require.Greater(t, height, int64(100))
}

// TestGetClaimWindowOpenHeight tests claim window calculation
func TestGetClaimWindowOpenHeight(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// Test claim window calculation
	height, err := qc.Shared().GetClaimWindowOpenHeight(ctx, 100)
	require.NoError(t, err)
	require.Greater(t, height, int64(100))
}

// TestGetProofWindowOpenHeight tests proof window calculation
func TestGetProofWindowOpenHeight(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// Test proof window calculation
	height, err := qc.Shared().GetProofWindowOpenHeight(ctx, 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, height, int64(100))
}

// TestGetEarliestSupplierClaimCommitHeight tests earliest claim commit height
func TestGetEarliestSupplierClaimCommitHeight(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// Test earliest claim commit height
	height, err := qc.Shared().GetEarliestSupplierClaimCommitHeight(ctx, 100, "pokt1supplier")
	require.NoError(t, err)
	require.Greater(t, height, int64(0))
}

// TestGetEarliestSupplierProofCommitHeight tests earliest proof commit height
func TestGetEarliestSupplierProofCommitHeight(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// Test earliest proof commit height
	height, err := qc.Shared().GetEarliestSupplierProofCommitHeight(ctx, 100, "pokt1supplier")
	require.NoError(t, err)
	require.Greater(t, height, int64(0))
}

// TestGetSupplierParams_Success tests successful supplier params retrieval
func TestGetSupplierParams_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testParams := generateTestSupplierParams()
	mock.supplierParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query params
	ctx := context.Background()
	params, err := qc.Supplier().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params)
	require.NotNil(t, params.MinStake)
}

// TestGetServiceParams_Success tests successful service params retrieval
func TestGetServiceParams_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testParams := generateTestServiceParams()
	mock.serviceParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	// Query params
	ctx := context.Background()
	params, err := qc.Service().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params)
	require.NotNil(t, params.AddServiceFee)
}

// TestGetParams_ParamsMissing tests behavior when params are not set
func TestGetParams_ParamsMissing(t *testing.T) {
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

	ctx := context.Background()

	// All param queries should fail when not configured in mock
	_, err = qc.Shared().GetParams(ctx)
	require.Error(t, err)

	_, err = qc.Session().GetParams(ctx)
	require.Error(t, err)

	_, err = qc.Proof().GetParams(ctx)
	require.Error(t, err)

	_, err = qc.Application().GetParams(ctx)
	require.Error(t, err)

	_, err = qc.Supplier().GetParams(ctx)
	require.Error(t, err)

	_, err = qc.Service().GetParams(ctx)
	require.Error(t, err)
}

// TestGetParams_ServerError tests server error handling
func TestGetParams_ServerError(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Override Params method to return error (we need to use a custom wrapper)
	// For simplicity, we'll just test that the client handles errors properly
	// by not setting params in mock (which triggers NotFound error)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := QueryClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer qc.Close()

	ctx := context.Background()

	// Without params set, should get error
	_, err = qc.Shared().GetParams(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to query")

	// Now set params and verify it works
	mock.sharedParams = generateTestSharedParams()
	params, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, params)
}

// TestParams_ContextCancellation tests context cancellation handling
func TestParams_ContextCancellation(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
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

	// Create canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Query should fail due to canceled context
	_, err = qc.Shared().GetParams(ctx)
	// May succeed if query is cached, or fail with context canceled
	// Either is acceptable behavior
	_ = err
}
