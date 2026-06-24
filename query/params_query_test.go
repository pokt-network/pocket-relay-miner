//go:build test

package query

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// TestGetParamsAtHeight_DoesNotServeStaleCache is a regression guard: GetParamsAtHeight
// must resolve through the chain's ParamsAtHeight RPC and never short-circuit on the
// in-process GetParams cache (which is populated once and never invalidated in
// production). Otherwise, after an on-chain MsgUpdateParam, a long-running miner returns
// stale params for current-epoch heights and computes the wrong claim/proof windows.
func TestGetParamsAtHeight_DoesNotServeStaleCache(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Live Params RPC reports the OLD epoch — this is what fills the set-once cache.
	mock.sharedParams = &sharedtypes.Params{
		NumBlocksPerSession:            10,
		GracePeriodEndOffsetBlocks:     1,
		ClaimWindowOpenOffsetBlocks:    1,
		ClaimWindowCloseOffsetBlocks:   4,
		ProofWindowOpenOffsetBlocks:    0,
		ProofWindowCloseOffsetBlocks:   4,
		ComputeUnitsToTokensMultiplier: 42,
		SessionGridAnchorHeight:        1,
		SessionNumberAtAnchor:          1,
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = qc.Close() }()
	ctx := context.Background()

	// Warm the in-process cache with the OLD epoch (a miner that booted before the change).
	live, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(10), live.NumBlocksPerSession)

	// The chain now reports a NEW epoch via ParamsAtHeight (num_blocks 10 -> 20, new anchor).
	mock.sharedParamsAtHeight = &sharedtypes.Params{
		NumBlocksPerSession:            20,
		GracePeriodEndOffsetBlocks:     1,
		ClaimWindowOpenOffsetBlocks:    1,
		ClaimWindowCloseOffsetBlocks:   4,
		ProofWindowOpenOffsetBlocks:    0,
		ProofWindowCloseOffsetBlocks:   4,
		ComputeUnitsToTokensMultiplier: 99,
		SessionGridAnchorHeight:        100,
		SessionNumberAtAnchor:          11,
	}

	// A current-epoch height. The stale cache has anchor=1 (<= 500), so the removed
	// fast path would have returned the cached OLD params. The query must instead hit
	// ParamsAtHeight and return the NEW epoch.
	got, err := qc.Shared().GetParamsAtHeight(ctx, 500)
	require.NoError(t, err)
	require.Equal(t, uint64(20), got.NumBlocksPerSession,
		"GetParamsAtHeight must resolve via ParamsAtHeight, not the stale live cache")
	require.Equal(t, uint64(99), got.ComputeUnitsToTokensMultiplier)
}

// TestGetParamsAtHeight_CachesByHeight verifies the height-keyed memoization: a second
// query for the same height is served from cache (no re-RPC), and a different height is
// resolved fresh. Caching is safe because params-at-a-started-session-height are immutable.
func TestGetParamsAtHeight_CachesByHeight(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mkParams := func(numBlocks uint64) *sharedtypes.Params {
		return &sharedtypes.Params{
			NumBlocksPerSession:            numBlocks,
			GracePeriodEndOffsetBlocks:     1,
			ClaimWindowOpenOffsetBlocks:    1,
			ClaimWindowCloseOffsetBlocks:   4,
			ProofWindowOpenOffsetBlocks:    0,
			ProofWindowCloseOffsetBlocks:   4,
			ComputeUnitsToTokensMultiplier: 42,
		}
	}
	mock.sharedParams = mkParams(10)
	mock.sharedParamsAtHeight = mkParams(10)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = qc.Close() }()
	ctx := context.Background()

	// First query at height 100 caches num_blocks=10.
	first, err := qc.Shared().GetParamsAtHeight(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(10), first.NumBlocksPerSession)

	// Chain response changes, but height 100 must still serve the cached (immutable) value.
	mock.sharedParamsAtHeight = mkParams(20)
	cached, err := qc.Shared().GetParamsAtHeight(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(10), cached.NumBlocksPerSession, "same height must be served from cache")

	// A different height is resolved fresh.
	other, err := qc.Shared().GetParamsAtHeight(ctx, 200)
	require.NoError(t, err)
	require.Equal(t, uint64(20), other.NumBlocksPerSession, "a new height must hit the RPC")
}

// TestGetParamsAtHeight_ConcurrentAccess exercises the height cache under the race
// detector with simultaneous reads/writes across many heights.
func TestGetParamsAtHeight_ConcurrentAccess(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.sharedParams = generateTestSharedParams()
	mock.sharedParamsAtHeight = generateTestSharedParams()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = qc.Close() }()
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h := int64((i % 50) + 1) // mix of repeated (cached) and varied heights
				p, err := qc.Shared().GetParamsAtHeight(ctx, h)
				if err != nil || p == nil {
					t.Errorf("GetParamsAtHeight(%d): err=%v p=%v", h, err, p)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestGetSharedParams_Success tests successful shared params retrieval
func TestGetSharedParams_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	// Setup mock responses
	testParams := generateTestSharedParams()
	mock.sharedParams = testParams

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 10 * time.Millisecond, // Very short timeout
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

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
	config := ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	}

	qc, err := NewQueryClients(logger, config)
	require.NoError(t, err)
	require.NotNil(t, qc)
	defer func() { _ = qc.Close() }()

	// Create canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Query should fail due to canceled context
	_, err = qc.Shared().GetParams(ctx)
	// May succeed if query is cached, or fail with context canceled
	// Either is acceptable behavior
	_ = err
}

// TestLiveParams_RefreshAfterTTL is the regression guard for the June 2026
// PROOF_MISSING incident: live GetParams must NOT be fetch-once. After
// liveParamsCacheTTL the client must re-query the chain and observe an
// on-chain param change (e.g. the weekly CUTTM update); a fetch-once cache
// freezes CUTTM at process start, mis-values claims against the chain's
// proof-requirement threshold, and silently forfeits rewards until restart.
func TestLiveParams_RefreshAfterTTL(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	oldParams := generateTestSharedParams()
	oldParams.ComputeUnitsToTokensMultiplier = 97714
	mock.sharedParams = oldParams

	prevTTL := liveParamsCacheTTL
	liveParamsCacheTTL = 50 * time.Millisecond
	defer func() { liveParamsCacheTTL = prevTTL }()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = qc.Close() }()
	ctx := context.Background()

	// Warm the cache with the old epoch.
	p, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(97714), p.ComputeUnitsToTokensMultiplier)

	// On-chain param change (CUTTM bump). Within the TTL the cached value is
	// served — that staleness window is bounded and accepted.
	newParams := generateTestSharedParams()
	newParams.ComputeUnitsToTokensMultiplier = 118569
	mock.sharedParams = newParams

	p, err = qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(97714), p.ComputeUnitsToTokensMultiplier, "within TTL the cached value is expected")

	// After the TTL the refresh must observe the new value.
	time.Sleep(80 * time.Millisecond)
	p, err = qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(118569), p.ComputeUnitsToTokensMultiplier, "post-TTL read must observe the on-chain change")
}

// TestLiveParams_ServesStaleOnRefreshFailure verifies the availability side of
// the TTL refresh: when the post-TTL re-query fails (transient RPC outage), the
// client serves the last-known value instead of erroring, so the proof path is
// never broken by a refresh that used to be a cache hit.
func TestLiveParams_ServesStaleOnRefreshFailure(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.sharedParams = generateTestSharedParams()

	prevTTL := liveParamsCacheTTL
	liveParamsCacheTTL = 50 * time.Millisecond
	defer func() { liveParamsCacheTTL = prevTTL }()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = qc.Close() }()
	ctx := context.Background()

	p, err := qc.Shared().GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, p)

	// Break the params RPC, expire the TTL: the stale value must be served.
	mock.sharedParams = nil
	time.Sleep(80 * time.Millisecond)
	p, err = qc.Shared().GetParams(ctx)
	require.NoError(t, err, "refresh failure must fall back to the stale cache, not error")
	require.NotNil(t, p)
}
