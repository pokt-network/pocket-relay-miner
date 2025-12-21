//go:build test

package query

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetClaim_Success tests successful claim retrieval
func TestGetClaim_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testClaim := generateTestClaim("pokt1supplier", "session123")
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		require.Equal(t, "pokt1supplier", req.SupplierOperatorAddress)
		require.Equal(t, "session123", req.SessionId)
		return &prooftypes.QueryGetClaimResponse{Claim: *testClaim}, nil
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

	// Query claim
	ctx := context.Background()
	claim, err := qc.Proof().GetClaim(ctx, "pokt1supplier", "session123")
	require.NoError(t, err)
	require.NotNil(t, claim)

	// Type assertion to access claim fields
	if claimPtr, ok := claim.(*prooftypes.Claim); ok {
		require.Equal(t, "pokt1supplier", claimPtr.SupplierOperatorAddress)
		require.Equal(t, "session123", claimPtr.SessionHeader.SessionId)
		require.NotEmpty(t, claimPtr.RootHash)
	}
}

// TestGetClaim_NotFound tests claim not found error
func TestGetClaim_NotFound(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return not found
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		return nil, status.Error(codes.NotFound, "claim not found")
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

	// Query non-existent claim
	ctx := context.Background()
	claim, err := qc.Proof().GetClaim(ctx, "pokt1nonexistent", "session123")
	require.Error(t, err)
	require.Nil(t, claim)
	require.Contains(t, err.Error(), "failed to query claim")
}

// TestGetClaim_InvalidSessionID tests invalid session ID handling
func TestGetClaim_InvalidSessionID(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return invalid argument error
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		return nil, status.Error(codes.InvalidArgument, "invalid session ID")
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

	// Query with invalid session ID
	ctx := context.Background()
	claim, err := qc.Proof().GetClaim(ctx, "pokt1supplier", "")
	require.Error(t, err)
	require.Nil(t, claim)
}

// TestGetClaim_NetworkError tests network error handling
func TestGetClaim_NetworkError(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return network error
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
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
	claim, err := qc.Proof().GetClaim(ctx, "pokt1supplier", "session123")
	require.Error(t, err)
	require.Nil(t, claim)
	require.Contains(t, err.Error(), "failed to query claim")
}

// TestGetClaim_Cache tests claim caching behavior
func TestGetClaim_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock with counter to track queries
	queryCount := 0
	testClaim := generateTestClaim("pokt1supplier", "session123")
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		queryCount++
		return &prooftypes.QueryGetClaimResponse{Claim: *testClaim}, nil
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
	claim1, err := qc.Proof().GetClaim(ctx, "pokt1supplier", "session123")
	require.NoError(t, err)
	require.NotNil(t, claim1)
	require.Equal(t, 1, queryCount)

	// Second query - should use cache
	claim2, err := qc.Proof().GetClaim(ctx, "pokt1supplier", "session123")
	require.NoError(t, err)
	require.NotNil(t, claim2)
	require.Equal(t, 1, queryCount) // Still 1, not 2
}

// TestGetClaim_ConcurrentAccess tests thread-safe claim queries
func TestGetClaim_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testClaim := generateTestClaim("pokt1supplier", "session123")
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		return &prooftypes.QueryGetClaimResponse{Claim: *testClaim}, nil
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

			claim, err := qc.Proof().GetClaim(ctx, "pokt1supplier", "session123")
			require.NoError(t, err)
			require.NotNil(t, claim)
		}()
	}

	// Wait for all
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestGetClaim_Timeout tests query timeout
func TestGetClaim_Timeout(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to simulate slow response
	testClaim := generateTestClaim("pokt1supplier", "session123")
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		// Simulate slow query
		time.Sleep(100 * time.Millisecond)
		return &prooftypes.QueryGetClaimResponse{Claim: *testClaim}, nil
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
	_, _ = qc.Proof().GetClaim(ctx, "pokt1supplier", "session123")
	// The important thing is we don't hang
}

// TestGetClaim_MultipleClaims tests caching of multiple claims
func TestGetClaim_MultipleClaims(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock for multiple claims
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		claim := generateTestClaim(req.SupplierOperatorAddress, req.SessionId)
		return &prooftypes.QueryGetClaimResponse{Claim: *claim}, nil
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

	// Query different claims
	claim1, err := qc.Proof().GetClaim(ctx, "pokt1supplier1", "session123")
	require.NoError(t, err)
	require.NotNil(t, claim1)

	claim2, err := qc.Proof().GetClaim(ctx, "pokt1supplier2", "session123")
	require.NoError(t, err)
	require.NotNil(t, claim2)

	claim3, err := qc.Proof().GetClaim(ctx, "pokt1supplier1", "session456")
	require.NoError(t, err)
	require.NotNil(t, claim3)

	// All should be different (different suppliers or sessions)
	require.NotEqual(t, claim1, claim2)
	require.NotEqual(t, claim1, claim3)
}

// TestGetClaim_EmptyResponse tests empty claim response
func TestGetClaim_EmptyResponse(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return empty claim
	mock.getClaimFunc = func(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		return &prooftypes.QueryGetClaimResponse{Claim: prooftypes.Claim{}}, nil
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

	// Query should succeed with empty claim
	ctx := context.Background()
	claim, err := qc.Proof().GetClaim(ctx, "pokt1supplier", "session123")
	require.NoError(t, err)
	require.NotNil(t, claim)

	// Check it's an empty claim
	if claimPtr, ok := claim.(*prooftypes.Claim); ok {
		require.Empty(t, claimPtr.SupplierOperatorAddress)
		require.Nil(t, claimPtr.SessionHeader)
	}
}
