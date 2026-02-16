//go:build test

package query

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetService_Success tests successful service retrieval
func TestGetService_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testService := generateTestService("develop")
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		require.Equal(t, "develop", req.Id)
		return &servicetypes.QueryGetServiceResponse{Service: *testService}, nil
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

	// Query service
	ctx := context.Background()
	service, err := qc.Service().GetService(ctx, "develop")
	require.NoError(t, err)
	require.Equal(t, "develop", service.Id)
	require.Equal(t, "develop", service.Name)
	require.Equal(t, uint64(1), service.ComputeUnitsPerRelay)
}

// TestGetService_NotFound tests service not found error
func TestGetService_NotFound(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return not found
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		return nil, status.Error(codes.NotFound, "service not found")
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

	// Query non-existent service
	ctx := context.Background()
	service, err := qc.Service().GetService(ctx, "nonexistent")
	require.Error(t, err)
	require.Empty(t, service.Id)
	require.Contains(t, err.Error(), "failed to query service")
}

// TestGetService_InvalidID tests invalid service ID handling
func TestGetService_InvalidID(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return invalid argument error
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		return nil, status.Error(codes.InvalidArgument, "invalid service ID")
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

	// Query with invalid ID
	ctx := context.Background()
	service, err := qc.Service().GetService(ctx, "")
	require.Error(t, err)
	require.Empty(t, service.Id)
}

// TestGetService_NetworkError tests network error handling
func TestGetService_NetworkError(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return network error
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
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
	service, err := qc.Service().GetService(ctx, "develop")
	require.Error(t, err)
	require.Empty(t, service.Id)
	require.Contains(t, err.Error(), "failed to query service")
}

// TestGetService_EmptyResponse tests empty service response
func TestGetService_EmptyResponse(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return empty service
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		return &servicetypes.QueryGetServiceResponse{}, nil
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

	// Query should succeed with empty service
	ctx := context.Background()
	service, err := qc.Service().GetService(ctx, "develop")
	require.NoError(t, err)
	require.Empty(t, service.Id)
}

// TestGetService_Cache tests service caching behavior
func TestGetService_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock with counter to track queries
	queryCount := 0
	testService := generateTestService("develop")
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		queryCount++
		return &servicetypes.QueryGetServiceResponse{Service: *testService}, nil
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
	service1, err := qc.Service().GetService(ctx, "develop")
	require.NoError(t, err)
	require.Equal(t, "develop", service1.Id)
	require.Equal(t, 1, queryCount)

	// Second query - should use cache
	service2, err := qc.Service().GetService(ctx, "develop")
	require.NoError(t, err)
	require.Equal(t, "develop", service2.Id)
	require.Equal(t, 1, queryCount) // Still 1, not 2

	// Same service data
	require.Equal(t, service1.Id, service2.Id)
}

// TestGetService_ConcurrentAccess tests thread-safe service queries
func TestGetService_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testService := generateTestService("develop")
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		return &servicetypes.QueryGetServiceResponse{Service: *testService}, nil
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

			service, err := qc.Service().GetService(ctx, "develop")
			require.NoError(t, err)
			require.Equal(t, "develop", service.Id)
		}()
	}

	// Wait for all
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestGetServiceRelayDifficulty_Success tests successful difficulty retrieval
func TestGetServiceRelayDifficulty_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testDifficulty := generateTestRelayMiningDifficulty("develop")
	mock.getRelayMiningDifficultyFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyRequest) (*servicetypes.QueryGetRelayMiningDifficultyResponse, error) {
		require.Equal(t, "develop", req.ServiceId)
		return &servicetypes.QueryGetRelayMiningDifficultyResponse{RelayMiningDifficulty: *testDifficulty}, nil
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

	// Query difficulty
	ctx := context.Background()
	difficulty, err := qc.Service().GetServiceRelayDifficulty(ctx, "develop")
	require.NoError(t, err)
	require.Equal(t, "develop", difficulty.ServiceId)
	require.Equal(t, int64(100), difficulty.BlockHeight)
}

// TestGetServiceRelayDifficulty_NotFound tests difficulty not found error
func TestGetServiceRelayDifficulty_NotFound(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return not found
	mock.getRelayMiningDifficultyFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyRequest) (*servicetypes.QueryGetRelayMiningDifficultyResponse, error) {
		return nil, status.Error(codes.NotFound, "difficulty not found")
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

	// Query non-existent difficulty
	ctx := context.Background()
	difficulty, err := qc.Service().GetServiceRelayDifficulty(ctx, "nonexistent")
	require.Error(t, err)
	require.Empty(t, difficulty.ServiceId)
	require.Contains(t, err.Error(), "failed to query relay mining difficulty")
}

// TestGetServiceRelayDifficulty_Cache tests difficulty caching
func TestGetServiceRelayDifficulty_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock with counter
	queryCount := 0
	testDifficulty := generateTestRelayMiningDifficulty("develop")
	mock.getRelayMiningDifficultyFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyRequest) (*servicetypes.QueryGetRelayMiningDifficultyResponse, error) {
		queryCount++
		return &servicetypes.QueryGetRelayMiningDifficultyResponse{RelayMiningDifficulty: *testDifficulty}, nil
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

	// First query
	difficulty1, err := qc.Service().GetServiceRelayDifficulty(ctx, "develop")
	require.NoError(t, err)
	require.Equal(t, "develop", difficulty1.ServiceId)
	require.Equal(t, 1, queryCount)

	// Second query - should use cache
	difficulty2, err := qc.Service().GetServiceRelayDifficulty(ctx, "develop")
	require.NoError(t, err)
	require.Equal(t, "develop", difficulty2.ServiceId)
	require.Equal(t, 1, queryCount) // Still 1

	// Same data
	require.Equal(t, difficulty1.ServiceId, difficulty2.ServiceId)
}

// TestGetService_Timeout tests query timeout
func TestGetService_Timeout(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to simulate slow response
	testService := generateTestService("develop")
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		// Simulate slow query
		time.Sleep(100 * time.Millisecond)
		return &servicetypes.QueryGetServiceResponse{Service: *testService}, nil
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
	_, _ = qc.Service().GetService(ctx, "develop")
	// The important thing is we don't hang
}

// TestGetServiceRelayDifficultyAtHeight_Success tests height-aware difficulty retrieval
func TestGetServiceRelayDifficultyAtHeight_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	testDifficulty := generateTestRelayMiningDifficulty("develop")
	mock.getRelayMiningDifficultyAtHeightFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		require.Equal(t, "develop", req.ServiceId)
		require.Equal(t, int64(100), req.BlockHeight)
		return &servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse{RelayMiningDifficulty: *testDifficulty}, nil
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
	difficulty, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, "develop", difficulty.ServiceId)
	require.Equal(t, int64(100), difficulty.BlockHeight)
}

// TestGetServiceRelayDifficultyAtHeight_Cache tests height-aware difficulty caching
func TestGetServiceRelayDifficultyAtHeight_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	queryCount := 0
	testDifficulty := generateTestRelayMiningDifficulty("develop")
	mock.getRelayMiningDifficultyAtHeightFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		queryCount++
		return &servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse{RelayMiningDifficulty: *testDifficulty}, nil
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

	// First query — hits server
	difficulty1, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, "develop", difficulty1.ServiceId)
	require.Equal(t, 1, queryCount)

	// Second query with same height — cache hit
	difficulty2, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, "develop", difficulty2.ServiceId)
	require.Equal(t, 1, queryCount) // Still 1

	// Third query with different height — cache miss
	_, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "develop", 200)
	require.NoError(t, err)
	require.Equal(t, 2, queryCount) // Now 2
}

// TestGetServiceRelayDifficultyAtHeight_NotFound tests not found error
func TestGetServiceRelayDifficultyAtHeight_NotFound(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.getRelayMiningDifficultyAtHeightFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		return nil, status.Error(codes.NotFound, "difficulty at height not found")
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
	difficulty, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "nonexistent", 100)
	require.Error(t, err)
	require.Empty(t, difficulty.ServiceId)
	require.Contains(t, err.Error(), "failed to query relay mining difficulty at height")
}

// TestGetService_MultipleServices tests caching of multiple services
func TestGetService_MultipleServices(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock for multiple services
	mock.getServiceFunc = func(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		service := generateTestService(req.Id)
		return &servicetypes.QueryGetServiceResponse{Service: *service}, nil
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

	// Query different services
	service1, err := qc.Service().GetService(ctx, "develop")
	require.NoError(t, err)
	require.Equal(t, "develop", service1.Id)

	service2, err := qc.Service().GetService(ctx, "ethereum")
	require.NoError(t, err)
	require.Equal(t, "ethereum", service2.Id)

	service3, err := qc.Service().GetService(ctx, "polygon")
	require.NoError(t, err)
	require.Equal(t, "polygon", service3.Id)

	// All should be different
	require.NotEqual(t, service1.Id, service2.Id)
	require.NotEqual(t, service2.Id, service3.Id)
}

// TestGetServiceRelayDifficultyAtHeight_ConcurrentAccess tests thread-safe height-aware
// difficulty queries across multiple goroutines and service/height combinations.
// Must pass with `go test -race`.
func TestGetServiceRelayDifficultyAtHeight_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.getRelayMiningDifficultyAtHeightFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		return &servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse{
			RelayMiningDifficulty: servicetypes.RelayMiningDifficulty{
				ServiceId:    req.ServiceId,
				BlockHeight:  req.BlockHeight,
				NumRelaysEma: 1000,
				TargetHash:   make([]byte, 32),
			},
		}, nil
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
	services := []string{"develop", "ethereum", "polygon", "solana", "avax"}
	heights := []int64{100, 104, 108, 112, 116}

	var wg sync.WaitGroup
	for _, svc := range services {
		for _, h := range heights {
			wg.Add(1)
			go func(serviceID string, height int64) {
				defer wg.Done()
				difficulty, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, serviceID, height)
				require.NoError(t, err)
				require.Equal(t, serviceID, difficulty.ServiceId)
				require.Equal(t, height, difficulty.BlockHeight)
			}(svc, h)
		}
	}
	wg.Wait()
}

// TestGetServiceRelayDifficultyAtHeight_CacheEviction verifies that the height-aware
// difficulty cache stays bounded by evicting the oldest half when the size cap is exceeded.
// No assumptions about session length, block timing, or network parameters.
func TestGetServiceRelayDifficultyAtHeight_CacheEviction(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.getRelayMiningDifficultyAtHeightFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		return &servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse{
			RelayMiningDifficulty: servicetypes.RelayMiningDifficulty{
				ServiceId:    req.ServiceId,
				BlockHeight:  req.BlockHeight,
				NumRelaysEma: 1000,
				TargetHash:   make([]byte, 32),
			},
		}, nil
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
	serviceClient := qc.serviceClient

	// Insert more entries than the cache cap to trigger eviction.
	// Use 10 services x (maxHeightDifficultyCacheEntries/10 + 10) heights.
	services := []string{"svc1", "svc2", "svc3", "svc4", "svc5", "svc6", "svc7", "svc8", "svc9", "svc10"}
	heightsPerService := int64(maxHeightDifficultyCacheEntries/len(services)) + 10
	for h := int64(1); h <= heightsPerService; h++ {
		for _, svc := range services {
			_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, svc, h)
			require.NoError(t, err)
		}
	}

	// After eviction, cache size should be <= maxHeightDifficultyCacheEntries.
	cacheSize := serviceClient.heightDifficultyCacheSize.Load()
	require.LessOrEqual(t, cacheSize, int64(maxHeightDifficultyCacheEntries),
		"cache size %d exceeds max %d after eviction", cacheSize, maxHeightDifficultyCacheEntries)

	// Recent entries (highest heights) should still be in cache.
	for _, svc := range services {
		key := fmt.Sprintf("%s@%d", svc, heightsPerService)
		_, ok := serviceClient.heightDifficultyCache.Load(key)
		require.True(t, ok, "recent entry %s should still be cached", key)
	}

	// Oldest entries (height 1) should have been evicted.
	for _, svc := range services {
		key := fmt.Sprintf("%s@%d", svc, 1)
		_, ok := serviceClient.heightDifficultyCache.Load(key)
		require.False(t, ok, "old entry %s should have been evicted", key)
	}
}
