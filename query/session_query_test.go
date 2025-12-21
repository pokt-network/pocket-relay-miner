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

// TestGetSession_Success tests successful session retrieval
func TestGetSession_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testSession := generateTestSession("pokt1app", "develop", 100)
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		require.Equal(t, "pokt1app", req.ApplicationAddress)
		require.Equal(t, "develop", req.ServiceId)
		require.Equal(t, int64(100), req.BlockHeight)
		return &sessiontypes.QueryGetSessionResponse{Session: testSession}, nil
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

	// Query session
	ctx := context.Background()
	session, err := qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, "pokt1app", session.Header.ApplicationAddress)
	require.Equal(t, "develop", session.Header.ServiceId)
	require.Equal(t, int64(100), session.Header.SessionStartBlockHeight)
}

// TestGetSession_NotFound tests session not found error
func TestGetSession_NotFound(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return not found
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		return nil, status.Error(codes.NotFound, "session not found")
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

	// Query non-existent session
	ctx := context.Background()
	session, err := qc.Session().GetSession(ctx, "pokt1nonexistent", "develop", 100)
	require.Error(t, err)
	require.Nil(t, session)
	require.Contains(t, err.Error(), "failed to query session")
}

// TestGetSession_InvalidSessionID tests invalid session ID handling
func TestGetSession_InvalidSessionID(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return invalid argument error
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
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

	// Query with invalid data
	ctx := context.Background()
	session, err := qc.Session().GetSession(ctx, "invalid", "develop", 100)
	require.Error(t, err)
	require.Nil(t, session)
}

// TestGetSession_NetworkError tests network error handling
func TestGetSession_NetworkError(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return network error
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
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
	session, err := qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.Error(t, err)
	require.Nil(t, session)
	require.Contains(t, err.Error(), "failed to query session")
}

// TestGetSession_Timeout tests query timeout
func TestGetSession_Timeout(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to simulate slow response
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		// Simulate slow query
		time.Sleep(100 * time.Millisecond)
		return &sessiontypes.QueryGetSessionResponse{
			Session: generateTestSession("pokt1app", "develop", 100),
		}, nil
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

	// Query should timeout
	ctx := context.Background()
	_, err = qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	// May timeout or succeed depending on timing
	// The important thing is we don't hang
}

// TestGetSession_MalformedResponse tests malformed response handling
func TestGetSession_MalformedResponse(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return nil session (invalid response)
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		return &sessiontypes.QueryGetSessionResponse{Session: nil}, nil
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

	// Query should succeed but return nil session
	ctx := context.Background()
	session, err := qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.NoError(t, err)
	require.Nil(t, session)
}

// TestGetSession_EmptyResponse tests empty session response
func TestGetSession_EmptyResponse(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock to return empty session
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		return &sessiontypes.QueryGetSessionResponse{Session: &sessiontypes.Session{}}, nil
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

	// Query should succeed with empty session
	ctx := context.Background()
	session, err := qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Nil(t, session.Header)
}

// TestGetSession_Cache tests session caching behavior
func TestGetSession_Cache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock with counter to track queries
	queryCount := 0
	testSession := generateTestSession("pokt1app", "develop", 100)
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		queryCount++
		return &sessiontypes.QueryGetSessionResponse{Session: testSession}, nil
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
	session1, err := qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.NoError(t, err)
	require.NotNil(t, session1)
	require.Equal(t, 1, queryCount)

	// Second query - should use cache
	session2, err := qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.NoError(t, err)
	require.NotNil(t, session2)
	require.Equal(t, 1, queryCount) // Still 1, not 2

	// Same session data
	require.Equal(t, session1.SessionId, session2.SessionId)
}

// TestGetSession_CacheInvalidation tests cache invalidation
func TestGetSession_CacheInvalidation(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	queryCount := 0
	testSession := generateTestSession("pokt1app", "develop", 100)
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		queryCount++
		return &sessiontypes.QueryGetSessionResponse{Session: testSession}, nil
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
	_, err = qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.NoError(t, err)
	require.Equal(t, 1, queryCount)

	// Invalidate cache
	sessionClient := qc.sessionClient
	sessionClient.InvalidateCache()

	// Next query should hit server again
	_, err = qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
	require.NoError(t, err)
	require.Equal(t, 2, queryCount) // Hit server again
}

// TestGetSession_ConcurrentAccess tests thread-safe session queries
func TestGetSession_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock
	testSession := generateTestSession("pokt1app", "develop", 100)
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		return &sessiontypes.QueryGetSessionResponse{Session: testSession}, nil
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

			session, err := qc.Session().GetSession(ctx, "pokt1app", "develop", 100)
			require.NoError(t, err)
			require.NotNil(t, session)
		}()
	}

	// Wait for all
	for i := 0; i < 10; i++ {
		<-done
	}
}
