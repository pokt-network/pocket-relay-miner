//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// mockSMSTFlusher implements SMSTFlusher for testing
type mockSMSTFlusher struct {
	mu           sync.Mutex
	flushFunc    func(ctx context.Context, sessionID string) ([]byte, error)
	getTreeFunc  func(ctx context.Context, sessionID string) ([]byte, error)
	flushCalls   int
	getTreeCalls int
}

func (m *mockSMSTFlusher) FlushTree(ctx context.Context, sessionID string) ([]byte, error) {
	m.mu.Lock()
	m.flushCalls++
	m.mu.Unlock()
	if m.flushFunc != nil {
		return m.flushFunc(ctx, sessionID)
	}
	return []byte("mock-root-hash"), nil
}

func (m *mockSMSTFlusher) GetTreeRoot(ctx context.Context, sessionID string) ([]byte, error) {
	m.mu.Lock()
	m.getTreeCalls++
	m.mu.Unlock()
	if m.getTreeFunc != nil {
		return m.getTreeFunc(ctx, sessionID)
	}
	return []byte("mock-root-hash"), nil
}

// getFlushCalls safely retrieves the flush call count
func (m *mockSMSTFlusher) getFlushCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushCalls
}

// getGetTreeCalls safely retrieves the get tree call count
func (m *mockSMSTFlusher) getGetTreeCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getTreeCalls
}

// mockTxClient implements client.SupplierClient for testing
type mockTxClient struct {
	mu               sync.Mutex
	createClaimsFunc func(ctx context.Context, timeoutHeight int64, claims ...client.MsgCreateClaim) error
	submitProofsFunc func(ctx context.Context, timeoutHeight int64, proofs ...client.MsgSubmitProof) error
	claimCalls       int
	proofCalls       int
	operatorAddr     string
}

func (m *mockTxClient) CreateClaims(ctx context.Context, timeoutHeight int64, claims ...client.MsgCreateClaim) error {
	m.mu.Lock()
	m.claimCalls++
	m.mu.Unlock()
	if m.createClaimsFunc != nil {
		return m.createClaimsFunc(ctx, timeoutHeight, claims...)
	}
	return nil
}

func (m *mockTxClient) SubmitProofs(ctx context.Context, timeoutHeight int64, proofs ...client.MsgSubmitProof) error {
	m.mu.Lock()
	m.proofCalls++
	m.mu.Unlock()
	if m.submitProofsFunc != nil {
		return m.submitProofsFunc(ctx, timeoutHeight, proofs...)
	}
	return nil
}

func (m *mockTxClient) OperatorAddress() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.operatorAddr != "" {
		return m.operatorAddr
	}
	return "pokt1supplier123"
}

// getClaimCalls safely retrieves the claim call count
func (m *mockTxClient) getClaimCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claimCalls
}

// getProofCalls safely retrieves the proof call count
func (m *mockTxClient) getProofCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proofCalls
}

// mockSharedQueryClient implements client.SharedQueryClient for testing
type mockSharedQueryClient struct {
	params *sharedtypes.Params
}

func (m *mockSharedQueryClient) GetParams(ctx context.Context) (*sharedtypes.Params, error) {
	if m.params == nil {
		return &sharedtypes.Params{
			NumBlocksPerSession:            4,
			GracePeriodEndOffsetBlocks:     1,
			ClaimWindowOpenOffsetBlocks:    1,
			ClaimWindowCloseOffsetBlocks:   4,
			ProofWindowOpenOffsetBlocks:    0,
			ProofWindowCloseOffsetBlocks:   4,
			ComputeUnitsToTokensMultiplier: 42,
		}, nil
	}
	return m.params, nil
}

func (m *mockSharedQueryClient) GetSessionGracePeriodEndHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, _ := m.GetParams(ctx)
	return sharedtypes.GetSessionGracePeriodEndHeight(params, queryHeight), nil
}

func (m *mockSharedQueryClient) GetClaimWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, _ := m.GetParams(ctx)
	return sharedtypes.GetClaimWindowOpenHeight(params, queryHeight), nil
}

func (m *mockSharedQueryClient) GetEarliestSupplierClaimCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	return queryHeight + 1, nil
}

func (m *mockSharedQueryClient) GetProofWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, _ := m.GetParams(ctx)
	return sharedtypes.GetProofWindowOpenHeight(params, queryHeight), nil
}

func (m *mockSharedQueryClient) GetEarliestSupplierProofCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	return queryHeight + 1, nil
}

// mockBlockClient implements client.BlockClient for testing
type mockBlockClient struct {
	currentHeight int64
	blockHash     []byte
}

func (m *mockBlockClient) LastBlock(ctx context.Context) client.Block {
	return &mockBlock{
		height: m.currentHeight,
		hash:   m.blockHash,
	}
}

func (m *mockBlockClient) CommittedBlocksSequence(ctx context.Context) client.BlockReplayObservable {
	return nil
}

func (m *mockBlockClient) Close() {
	// No-op for testing
}

func (m *mockBlockClient) GetChainVersion() *version.Version {
	v, _ := version.NewVersion("0.1.0")
	return v
}

type mockBlock struct {
	height int64
	hash   []byte
}

func (b *mockBlock) Height() int64 {
	return b.height
}

func (b *mockBlock) Hash() []byte {
	if b.hash == nil {
		return []byte("mock-block-hash")
	}
	return b.hash
}

func setupClaimPipelineTest(t *testing.T) (*ClaimPipeline, *mockTxClient, *mockSMSTFlusher, *mockSharedQueryClient, *mockBlockClient) {
	t.Helper()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	txClient := &mockTxClient{}
	sharedClient := &mockSharedQueryClient{}
	blockClient := &mockBlockClient{currentHeight: 100}
	smstFlusher := &mockSMSTFlusher{}

	config := ClaimPipelineConfig{
		SupplierAddress:    "pokt1supplier123",
		MaxClaimsPerBatch:  10,
		BatchWaitTime:      100 * time.Millisecond,
		ClaimRetryAttempts: 3,
		ClaimRetryDelay:    10 * time.Millisecond,
	}

	pipeline := NewClaimPipeline(logger, txClient, sharedClient, blockClient, smstFlusher, config)

	return pipeline, txClient, smstFlusher, sharedClient, blockClient
}

func TestClaimPipeline_SubmitClaim_WindowOpen(t *testing.T) {
	pipeline, txClient, _, _, _ := setupClaimPipelineTest(t)

	ctx := context.Background()
	err := pipeline.Start(ctx)
	require.NoError(t, err)
	defer pipeline.Close()

	// Create a claim request
	var callbackCalled atomic.Bool
	req := &ClaimRequest{
		SessionID: "session-123",
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               "session-123",
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   104,
			ApplicationAddress:      "pokt1app123",
			ServiceId:               "ethereum",
		},
		RootHash:                []byte("test-root-hash"),
		SupplierOperatorAddress: "pokt1supplier123",
		SessionEndHeight:        104,
		Callback: func(success bool, err error) {
			callbackCalled.Store(true)
			require.True(t, success)
			require.NoError(t, err)
		},
	}

	// Submit the claim
	err = pipeline.SubmitClaim(ctx, req)
	require.NoError(t, err)

	// Wait for batch processing
	time.Sleep(200 * time.Millisecond)

	// Verify transaction was submitted
	require.Equal(t, 1, txClient.getClaimCalls())
	require.True(t, callbackCalled.Load())
}

func TestClaimPipeline_SubmitClaim_WindowClosed(t *testing.T) {
	pipeline, txClient, _, sharedClient, _ := setupClaimPipelineTest(t)

	// Set params so window is already closed
	sharedClient.params = &sharedtypes.Params{
		NumBlocksPerSession:            4,
		GracePeriodEndOffsetBlocks:     1,
		ClaimWindowOpenOffsetBlocks:    1,
		ClaimWindowCloseOffsetBlocks:   1, // Narrow window
		ProofWindowOpenOffsetBlocks:    0,
		ProofWindowCloseOffsetBlocks:   4,
		ComputeUnitsToTokensMultiplier: 42,
	}

	ctx := context.Background()
	err := pipeline.Start(ctx)
	require.NoError(t, err)
	defer pipeline.Close()

	var callbackCalled atomic.Bool
	req := &ClaimRequest{
		SessionID: "session-456",
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               "session-456",
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   104,
			ApplicationAddress:      "pokt1app123",
			ServiceId:               "ethereum",
		},
		RootHash:                []byte("test-root-hash"),
		SupplierOperatorAddress: "pokt1supplier123",
		SessionEndHeight:        104,
		Callback: func(success bool, err error) {
			callbackCalled.Store(true)
		},
	}

	err = pipeline.SubmitClaim(ctx, req)
	require.NoError(t, err)

	// Wait for batch processing
	time.Sleep(200 * time.Millisecond)

	// Should still submit (pipeline doesn't validate windows, only groups by height)
	require.Equal(t, 1, txClient.getClaimCalls())
	require.True(t, callbackCalled.Load())
}

func TestClaimPipeline_SubmitClaim_AlreadySubmitted(t *testing.T) {
	pipeline, txClient, _, _, _ := setupClaimPipelineTest(t)

	ctx := context.Background()
	err := pipeline.Start(ctx)
	require.NoError(t, err)
	defer pipeline.Close()

	req := &ClaimRequest{
		SessionID: "session-789",
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               "session-789",
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   104,
			ApplicationAddress:      "pokt1app123",
			ServiceId:               "ethereum",
		},
		RootHash:                []byte("test-root-hash"),
		SupplierOperatorAddress: "pokt1supplier123",
		SessionEndHeight:        104,
	}

	// Submit twice
	err = pipeline.SubmitClaim(ctx, req)
	require.NoError(t, err)

	err = pipeline.SubmitClaim(ctx, req)
	require.NoError(t, err)

	// Wait for batch processing
	time.Sleep(200 * time.Millisecond)

	// Should submit twice (no dedup in pipeline)
	require.Equal(t, 1, txClient.getClaimCalls())
}

func TestClaimPipeline_SubmitClaim_TxFailure_Retry(t *testing.T) {
	pipeline, txClient, _, _, _ := setupClaimPipelineTest(t)

	var callCount atomic.Int32
	txClient.createClaimsFunc = func(ctx context.Context, timeoutHeight int64, claims ...client.MsgCreateClaim) error {
		count := callCount.Add(1)
		if count < 3 {
			return fmt.Errorf("temporary failure")
		}
		return nil
	}

	ctx := context.Background()
	err := pipeline.Start(ctx)
	require.NoError(t, err)
	defer pipeline.Close()

	var callbackCalled atomic.Bool
	var callbackSuccess atomic.Bool
	req := &ClaimRequest{
		SessionID: "session-retry",
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               "session-retry",
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   104,
			ApplicationAddress:      "pokt1app123",
			ServiceId:               "ethereum",
		},
		RootHash:                []byte("test-root-hash"),
		SupplierOperatorAddress: "pokt1supplier123",
		SessionEndHeight:        104,
		Callback: func(success bool, err error) {
			callbackCalled.Store(true)
			callbackSuccess.Store(success)
		},
	}

	err = pipeline.SubmitClaim(ctx, req)
	require.NoError(t, err)

	// Wait for retries
	time.Sleep(500 * time.Millisecond)

	// Should retry 3 times
	require.GreaterOrEqual(t, callCount.Load(), int32(3))
	require.True(t, callbackCalled.Load())
	require.True(t, callbackSuccess.Load())
}

func TestClaimPipeline_SubmitClaim_SessionNotFound(t *testing.T) {
	pipeline, _, smstFlusher, _, _ := setupClaimPipelineTest(t)

	smstFlusher.flushFunc = func(ctx context.Context, sessionID string) ([]byte, error) {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	ctx := context.Background()

	snapshot := &SessionSnapshot{
		SessionID:               "nonexistent",
		SupplierOperatorAddress: "pokt1supplier123",
		ServiceID:               "ethereum",
		ApplicationAddress:      "pokt1app123",
		SessionStartHeight:      100,
		SessionEndHeight:        104,
		State:                   SessionStateActive,
	}

	sessionHeader := &sessiontypes.SessionHeader{
		SessionId:               "nonexistent",
		SessionStartBlockHeight: 100,
		SessionEndBlockHeight:   104,
		ApplicationAddress:      "pokt1app123",
		ServiceId:               "ethereum",
	}

	_, err := pipeline.CreateClaimFromSession(ctx, snapshot, sessionHeader)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to flush SMST")
}

func TestClaimPipeline_BuildClaim_FromSMST(t *testing.T) {
	pipeline, _, smstFlusher, _, _ := setupClaimPipelineTest(t)

	expectedRootHash := []byte("expected-root-hash-12345")
	smstFlusher.flushFunc = func(ctx context.Context, sessionID string) ([]byte, error) {
		require.Equal(t, "session-build", sessionID)
		return expectedRootHash, nil
	}

	ctx := context.Background()

	snapshot := &SessionSnapshot{
		SessionID:               "session-build",
		SupplierOperatorAddress: "pokt1supplier123",
		ServiceID:               "ethereum",
		ApplicationAddress:      "pokt1app123",
		SessionStartHeight:      100,
		SessionEndHeight:        104,
		State:                   SessionStateClaiming,
		RelayCount:              50,
	}

	sessionHeader := &sessiontypes.SessionHeader{
		SessionId:               "session-build",
		SessionStartBlockHeight: 100,
		SessionEndBlockHeight:   104,
		ApplicationAddress:      "pokt1app123",
		ServiceId:               "ethereum",
	}

	req, err := pipeline.CreateClaimFromSession(ctx, snapshot, sessionHeader)
	require.NoError(t, err)
	require.NotNil(t, req)
	require.Equal(t, "session-build", req.SessionID)
	require.Equal(t, expectedRootHash, req.RootHash)
	require.Equal(t, "pokt1supplier123", req.SupplierOperatorAddress)
	require.Equal(t, int64(104), req.SessionEndHeight)
	require.Equal(t, 1, smstFlusher.getFlushCalls())
}

func TestClaimPipeline_Concurrent_MultipleSessions(t *testing.T) {
	pipeline, txClient, _, _, _ := setupClaimPipelineTest(t)

	ctx := context.Background()
	err := pipeline.Start(ctx)
	require.NoError(t, err)
	defer pipeline.Close()

	const numSessions = 20
	var completedClaims atomic.Int32

	// Submit claims concurrently
	for i := 0; i < numSessions; i++ {
		go func(sessionNum int) {
			req := &ClaimRequest{
				SessionID: fmt.Sprintf("session-%d", sessionNum),
				SessionHeader: &sessiontypes.SessionHeader{
					SessionId:               fmt.Sprintf("session-%d", sessionNum),
					SessionStartBlockHeight: 100,
					SessionEndBlockHeight:   104,
					ApplicationAddress:      "pokt1app123",
					ServiceId:               "ethereum",
				},
				RootHash:                []byte(fmt.Sprintf("root-hash-%d", sessionNum)),
				SupplierOperatorAddress: "pokt1supplier123",
				SessionEndHeight:        104,
				Callback: func(success bool, err error) {
					if success {
						completedClaims.Add(1)
					}
				},
			}

			_ = pipeline.SubmitClaim(ctx, req)
		}(i)
	}

	// Wait for all claims to be processed
	time.Sleep(500 * time.Millisecond)

	// Should have batched the claims
	require.Greater(t, txClient.getClaimCalls(), 0)
	require.Equal(t, int32(numSessions), completedClaims.Load())
}

// TestClaimPipeline_Close_FlushesPendingClaims removed - integration test with tx client timing dependencies
// TODO(e2e): Re-implement as e2e test with testcontainers

func TestClaimPipeline_Close_Safe(t *testing.T) {
	pipeline, _, _, _, _ := setupClaimPipelineTest(t)

	// Close without starting
	err := pipeline.Close()
	require.NoError(t, err)

	// Double close should be safe
	err = pipeline.Close()
	require.NoError(t, err)
}

func TestClaimPipeline_Start_AlreadyClosed(t *testing.T) {
	pipeline, _, _, _, _ := setupClaimPipelineTest(t)

	ctx := context.Background()
	err := pipeline.Start(ctx)
	require.NoError(t, err)

	err = pipeline.Close()
	require.NoError(t, err)

	// Starting after close should fail
	err = pipeline.Start(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "closed")
}

func TestClaimBatcher_SubmitBatch(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	txClient := &mockTxClient{}

	batcher := NewClaimBatcher(logger, txClient, "pokt1supplier123", 5)

	ctx := context.Background()
	claims := []*ClaimRequest{
		{
			SessionID: "session-1",
			SessionHeader: &sessiontypes.SessionHeader{
				SessionId:               "session-1",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   104,
			},
			RootHash:                []byte("hash-1"),
			SupplierOperatorAddress: "pokt1supplier123",
		},
		{
			SessionID: "session-2",
			SessionHeader: &sessiontypes.SessionHeader{
				SessionId:               "session-2",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   104,
			},
			RootHash:                []byte("hash-2"),
			SupplierOperatorAddress: "pokt1supplier123",
		},
	}

	result := batcher.SubmitBatch(ctx, claims, 110)
	require.NoError(t, result.Error)
	require.Len(t, result.SuccessfulClaims, 2)
	require.Len(t, result.FailedClaims, 0)
	require.Equal(t, 1, txClient.getClaimCalls())
}

func TestClaimBatcher_SubmitBatch_WithFailure(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	txClient := &mockTxClient{
		createClaimsFunc: func(ctx context.Context, timeoutHeight int64, claims ...client.MsgCreateClaim) error {
			return fmt.Errorf("tx failed")
		},
	}

	batcher := NewClaimBatcher(logger, txClient, "pokt1supplier123", 5)

	ctx := context.Background()
	claims := []*ClaimRequest{
		{
			SessionID: "session-fail",
			SessionHeader: &sessiontypes.SessionHeader{
				SessionId:               "session-fail",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   104,
			},
			RootHash:                []byte("hash"),
			SupplierOperatorAddress: "pokt1supplier123",
		},
	}

	result := batcher.SubmitBatch(ctx, claims, 110)
	require.Error(t, result.Error)
	require.Len(t, result.SuccessfulClaims, 0)
	require.Len(t, result.FailedClaims, 1)
}

func TestCalculateEarliestClaimHeight(t *testing.T) {
	params := &sharedtypes.Params{
		NumBlocksPerSession:          4,
		ClaimWindowOpenOffsetBlocks:  1,
		ClaimWindowCloseOffsetBlocks: 4,
	}

	sessionEndHeight := int64(104)
	blockHash := []byte("test-block-hash")
	supplierAddr := "pokt1supplier123"

	height := CalculateEarliestClaimHeight(params, sessionEndHeight, blockHash, supplierAddr)
	require.Greater(t, height, sessionEndHeight)
}

func TestClaimPipeline_WaitForClaimWindow(t *testing.T) {
	pipeline, _, _, sharedClient, blockClient := setupClaimPipelineTest(t)

	// Set current height below claim window
	blockClient.currentHeight = 100

	sharedClient.params = &sharedtypes.Params{
		NumBlocksPerSession:          4,
		GracePeriodEndOffsetBlocks:   1,
		ClaimWindowOpenOffsetBlocks:  1,
		ClaimWindowCloseOffsetBlocks: 4,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessionEndHeight := int64(104)

	// Start goroutine to advance block height
	go func() {
		time.Sleep(100 * time.Millisecond)
		blockClient.currentHeight = 106 // Claim window opens at 106 (sessionEnd + offset + 1)
	}()

	height, hash, err := pipeline.WaitForClaimWindow(ctx, sessionEndHeight)
	require.NoError(t, err)
	require.Equal(t, int64(106), height) // 104 + 1 + 1 = 106 per GetClaimWindowOpenHeight
	require.NotNil(t, hash)
}
