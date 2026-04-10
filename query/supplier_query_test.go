//go:build test

package query

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetSupplier_Success tests successful supplier retrieval
func TestGetSupplier_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testSupplier := generateTestSupplier("pokt1supplier123")
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		require.Equal(t, "pokt1supplier123", req.OperatorAddress)
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *testSupplier}, nil
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

	// Query supplier
	ctx := context.Background()
	supplier, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
	require.NoError(t, err)
	require.Equal(t, "pokt1supplier123", supplier.OperatorAddress)
	require.NotNil(t, supplier.Stake)
	require.NotEmpty(t, supplier.Services)
}

// TestGetSupplier_NotFound tests supplier not found error
func TestGetSupplier_NotFound(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return not found
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		return nil, status.Error(codes.NotFound, "supplier not found")
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

	// Query non-existent supplier
	ctx := context.Background()
	supplier, err := qc.Supplier().GetSupplier(ctx, "pokt1nonexistent")
	require.Error(t, err)
	require.Empty(t, supplier.OperatorAddress)
	require.Contains(t, err.Error(), "failed to query supplier")
}

// TestGetSupplier_InvalidAddress tests invalid address handling
func TestGetSupplier_InvalidAddress(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return invalid argument error
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
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
	supplier, err := qc.Supplier().GetSupplier(ctx, "invalid")
	require.Error(t, err)
	require.Empty(t, supplier.OperatorAddress)
}

// TestGetSupplier_NetworkError tests network error handling
func TestGetSupplier_NetworkError(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return network error
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
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
	supplier, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier")
	require.Error(t, err)
	require.Empty(t, supplier.OperatorAddress)
	require.Contains(t, err.Error(), "failed to query supplier")
}

// TestGetSupplier_EmptyResponse tests empty supplier response
func TestGetSupplier_EmptyResponse(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return empty supplier
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		return &suppliertypes.QueryGetSupplierResponse{}, nil
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

	// Query should succeed with empty supplier
	ctx := context.Background()
	supplier, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier")
	require.NoError(t, err)
	require.Empty(t, supplier.OperatorAddress)
}

// TestGetSupplier_Cache tests supplier caching behavior
func TestGetSupplier_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock with counter to track queries
	queryCount := 0
	testSupplier := generateTestSupplier("pokt1supplier123")
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		queryCount++
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *testSupplier}, nil
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
	supplier1, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
	require.NoError(t, err)
	require.Equal(t, "pokt1supplier123", supplier1.OperatorAddress)
	require.Equal(t, 1, queryCount)

	// Second query - should use cache
	supplier2, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
	require.NoError(t, err)
	require.Equal(t, "pokt1supplier123", supplier2.OperatorAddress)
	require.Equal(t, 1, queryCount) // Still 1, not 2

	// Same supplier data
	require.Equal(t, supplier1.OperatorAddress, supplier2.OperatorAddress)
}

// TestGetSupplier_InvalidateRefreshesCache reproduces the bug where
// a supplier stakes for a new service (e.g. "pocket") but the query
// cache keeps returning stale data with the old service list.
// InvalidateSupplier must force the next GetSupplier to re-query chain.
func TestGetSupplier_InvalidateRefreshesCache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	queryCount := 0
	// Phase 1: supplier has 1 service ("develop")
	supplierV1 := generateTestSupplier("pokt1supplier123")

	// Phase 2: supplier stakes for a second service ("pocket")
	supplierV2 := generateTestSupplier("pokt1supplier123")
	supplierV2.Services = append(supplierV2.Services, &sharedtypes.SupplierServiceConfig{
		ServiceId: "pocket",
	})

	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		queryCount++
		if queryCount == 1 {
			return &suppliertypes.QueryGetSupplierResponse{Supplier: *supplierV1}, nil
		}
		// Subsequent queries return updated supplier with new service
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *supplierV2}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	defer qc.Close()

	ctx := context.Background()

	// First query: caches supplier with 1 service
	s1, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
	require.NoError(t, err)
	require.Len(t, s1.Services, 1, "initial query should return 1 service")
	require.Equal(t, 1, queryCount)

	// Second query without invalidation: returns stale cached data
	s2, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
	require.NoError(t, err)
	require.Len(t, s2.Services, 1, "cached query should still return 1 service")
	require.Equal(t, 1, queryCount, "cache hit — no new chain query")

	// Invalidate the supplier cache entry
	qc.Supplier().InvalidateSupplier("pokt1supplier123")

	// Third query after invalidation: must hit chain and return updated data
	s3, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
	require.NoError(t, err)
	require.Equal(t, 2, queryCount, "invalidation should force a new chain query")
	require.Len(t, s3.Services, 2, "post-invalidation query must return updated services")
	require.Equal(t, "pocket", s3.Services[1].ServiceId)
}

// TestGetSupplier_InvalidateOnlyAffectsTargetAddress ensures invalidating
// one supplier does not evict other cached suppliers.
func TestGetSupplier_InvalidateOnlyAffectsTargetAddress(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	queryCount := map[string]int{}
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		queryCount[req.OperatorAddress]++
		s := generateTestSupplier(req.OperatorAddress)
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *s}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	defer qc.Close()

	ctx := context.Background()

	// Cache two suppliers
	_, err = qc.Supplier().GetSupplier(ctx, "pokt1a")
	require.NoError(t, err)
	_, err = qc.Supplier().GetSupplier(ctx, "pokt1b")
	require.NoError(t, err)
	require.Equal(t, 1, queryCount["pokt1a"])
	require.Equal(t, 1, queryCount["pokt1b"])

	// Invalidate only pokt1a
	qc.Supplier().InvalidateSupplier("pokt1a")

	// Query both again
	_, err = qc.Supplier().GetSupplier(ctx, "pokt1a")
	require.NoError(t, err)
	_, err = qc.Supplier().GetSupplier(ctx, "pokt1b")
	require.NoError(t, err)

	require.Equal(t, 2, queryCount["pokt1a"], "pokt1a should have been re-queried")
	require.Equal(t, 1, queryCount["pokt1b"], "pokt1b should still be cached")
}

// TestGetSupplier_ConcurrentAccess tests thread-safe supplier queries
func TestGetSupplier_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testSupplier := generateTestSupplier("pokt1supplier123")
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *testSupplier}, nil
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

			supplier, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
			require.NoError(t, err)
			require.Equal(t, "pokt1supplier123", supplier.OperatorAddress)
		}()
	}

	// Wait for all
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestGetSupplier_Timeout tests query timeout
func TestGetSupplier_Timeout(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to simulate slow response
	testSupplier := generateTestSupplier("pokt1supplier123")
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		// Simulate slow query
		time.Sleep(100 * time.Millisecond)
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *testSupplier}, nil
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
	_, _ = qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
	// The important thing is we don't hang
}

// TestGetSupplier_MultipleSuppliers tests caching of multiple suppliers
func TestGetSupplier_MultipleSuppliers(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock for multiple suppliers
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		supplier := generateTestSupplier(req.OperatorAddress)
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *supplier}, nil
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

	// Query different suppliers
	supplier1, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier1")
	require.NoError(t, err)
	require.Equal(t, "pokt1supplier1", supplier1.OperatorAddress)

	supplier2, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier2")
	require.NoError(t, err)
	require.Equal(t, "pokt1supplier2", supplier2.OperatorAddress)

	supplier3, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier3")
	require.NoError(t, err)
	require.Equal(t, "pokt1supplier3", supplier3.OperatorAddress)

	// All should be different
	require.NotEqual(t, supplier1.OperatorAddress, supplier2.OperatorAddress)
	require.NotEqual(t, supplier2.OperatorAddress, supplier3.OperatorAddress)
}

// TestGetSupplier_WithServices tests supplier with service configurations
func TestGetSupplier_WithServices(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock with supplier that has services
	testSupplier := generateTestSupplier("pokt1supplier123")
	mock.getSupplierFunc = func(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *testSupplier}, nil
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

	// Query supplier
	ctx := context.Background()
	supplier, err := qc.Supplier().GetSupplier(ctx, "pokt1supplier123")
	require.NoError(t, err)
	require.Equal(t, "pokt1supplier123", supplier.OperatorAddress)
	require.NotEmpty(t, supplier.Services)
	require.Equal(t, "develop", supplier.Services[0].ServiceId)
}
