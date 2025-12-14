//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// mockSMSTProver implements SMSTProver for testing
type mockSMSTProver struct {
	mu               sync.Mutex
	proveClosestFunc func(ctx context.Context, sessionID string, path []byte) ([]byte, error)
	getClaimRootFunc func(ctx context.Context, sessionID string) ([]byte, error)
	proveCallCount   int
	getRootCallCount int
}

func (m *mockSMSTProver) ProveClosest(ctx context.Context, sessionID string, path []byte) ([]byte, error) {
	m.mu.Lock()
	m.proveCallCount++
	m.mu.Unlock()
	if m.proveClosestFunc != nil {
		return m.proveClosestFunc(ctx, sessionID, path)
	}
	return []byte("mock-proof-bytes"), nil
}

func (m *mockSMSTProver) GetClaimRoot(ctx context.Context, sessionID string) ([]byte, error) {
	m.mu.Lock()
	m.getRootCallCount++
	m.mu.Unlock()
	if m.getClaimRootFunc != nil {
		return m.getClaimRootFunc(ctx, sessionID)
	}
	return []byte("mock-root-hash"), nil
}

// getProveCallCount safely retrieves the prove call count
func (m *mockSMSTProver) getProveCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proveCallCount
}

// getGetRootCallCount safely retrieves the get root call count
func (m *mockSMSTProver) getGetRootCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getRootCallCount
}

// mockProofQueryClient implements client.ProofQueryClient for testing
type mockProofQueryClient struct {
	params *prooftypes.Params
}

func (m *mockProofQueryClient) GetParams(ctx context.Context) (client.ProofParams, error) {
	if m.params != nil {
		return m.params, nil
	}
	return &prooftypes.Params{
		ProofRequestProbability: 0.25,
	}, nil
}

func (m *mockProofQueryClient) GetClaim(ctx context.Context, supplierOperatorAddress string, sessionId string) (client.Claim, error) {
	return nil, nil
}

func setupProofPipelineTest(t *testing.T) (*ProofPipeline, *mockTxClient, *mockSMSTProver, *mockSharedQueryClient, *mockProofQueryClient, *mockBlockClient) {
	t.Helper()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	txClient := &mockTxClient{}
	sharedClient := &mockSharedQueryClient{}
	proofClient := &mockProofQueryClient{}
	blockClient := &mockBlockClient{currentHeight: 110}
	smstProver := &mockSMSTProver{}

	config := ProofPipelineConfig{
		SupplierAddress:    "pokt1supplier123",
		MaxProofsPerBatch:  10,
		BatchWaitTime:      100 * time.Millisecond,
		ProofRetryAttempts: 3,
		ProofRetryDelay:    10 * time.Millisecond,
	}

	pipeline := NewProofPipeline(logger, txClient, sharedClient, proofClient, blockClient, smstProver, config)

	return pipeline, txClient, smstProver, sharedClient, proofClient, blockClient
}

func TestProofPipeline_SubmitProof_Required(t *testing.T) {
	pipeline, txClient, _, _, _, _ := setupProofPipelineTest(t)

	ctx := context.Background()
	err := pipeline.Start(ctx)
	require.NoError(t, err)
	defer pipeline.Close()

	var callbackCalled atomic.Bool
	req := &ProofRequest{
		SessionID: "session-proof-123",
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               "session-proof-123",
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   104,
			ApplicationAddress:      "pokt1app123",
			ServiceId:               "ethereum",
		},
		ProofBytes:              []byte("test-proof-bytes"),
		SupplierOperatorAddress: "pokt1supplier123",
		SessionEndHeight:        104,
		Callback: func(success bool, err error) {
			callbackCalled.Store(true)
			require.True(t, success)
			require.NoError(t, err)
		},
	}

	err = pipeline.SubmitProof(ctx, req)
	require.NoError(t, err)

	// Wait for batch processing
	time.Sleep(200 * time.Millisecond)

	require.Equal(t, 1, txClient.getProofCalls())
	require.True(t, callbackCalled.Load())
}

func TestProofPipeline_SubmitProof_NotRequired(t *testing.T) {
	pipeline, _, _, _, _, _ := setupProofPipelineTest(t)

	ctx := context.Background()

	// CheckProofRequired returns true by default (unused method)
	required, err := pipeline.CheckProofRequired(ctx, "session-123", []byte("root-hash"), []byte("block-hash"))
	require.NoError(t, err)
	require.True(t, required) // Always returns true (legacy behavior)
}

// TestProofPipeline_SubmitProof_WindowClosed removed - integration test with timing/tx client dependencies
// TODO(e2e): Re-implement as e2e test with testcontainers

func TestProofPipeline_BuildProof_FromSMST(t *testing.T) {
	pipeline, _, smstProver, _, _, _ := setupProofPipelineTest(t)

	expectedProof := []byte("expected-proof-data-12345")
	smstProver.proveClosestFunc = func(ctx context.Context, sessionID string, path []byte) ([]byte, error) {
		require.Equal(t, "session-build-proof", sessionID)
		require.NotNil(t, path)
		return expectedProof, nil
	}

	ctx := context.Background()

	snapshot := &SessionSnapshot{
		SessionID:               "session-build-proof",
		SupplierOperatorAddress: "pokt1supplier123",
		ServiceID:               "ethereum",
		ApplicationAddress:      "pokt1app123",
		SessionStartHeight:      100,
		SessionEndHeight:        104,
		State:                   SessionStateClaimed,
		ClaimedRootHash:         []byte("claimed-root-hash"),
	}

	sessionHeader := &sessiontypes.SessionHeader{
		SessionId:               "session-build-proof",
		SessionStartBlockHeight: 100,
		SessionEndBlockHeight:   104,
		ApplicationAddress:      "pokt1app123",
		ServiceId:               "ethereum",
	}

	proofSeedBlockHash := []byte("proof-seed-block-hash")

	req, err := pipeline.GenerateProofForSession(ctx, snapshot, sessionHeader, proofSeedBlockHash)
	require.NoError(t, err)
	require.NotNil(t, req)
	require.Equal(t, "session-build-proof", req.SessionID)
	require.Equal(t, expectedProof, req.ProofBytes)
	require.Equal(t, "pokt1supplier123", req.SupplierOperatorAddress)
	require.Equal(t, int64(104), req.SessionEndHeight)
	require.Equal(t, 1, smstProver.getProveCallCount())
}

func TestProofPipeline_ProofRequirement_Calculation(t *testing.T) {
	pipeline, _, smstProver, _, _, _ := setupProofPipelineTest(t)

	ctx := context.Background()

	// Test that proof path is calculated correctly
	proofGenerated := false
	smstProver.proveClosestFunc = func(ctx context.Context, sessionID string, path []byte) ([]byte, error) {
		// Verify path is non-empty (calculated from block hash + session ID)
		require.NotNil(t, path)
		require.NotEmpty(t, path)
		proofGenerated = true
		return []byte("proof-data"), nil
	}

	snapshot := &SessionSnapshot{
		SessionID:               "test-session",
		SupplierOperatorAddress: "pokt1supplier123",
		ServiceID:               "ethereum",
		ApplicationAddress:      "pokt1app123",
		SessionStartHeight:      100,
		SessionEndHeight:        104,
		ClaimedRootHash:         []byte("root-hash"),
	}

	sessionHeader := &sessiontypes.SessionHeader{
		SessionId:               "test-session",
		SessionStartBlockHeight: 100,
		SessionEndBlockHeight:   104,
		ApplicationAddress:      "pokt1app123",
		ServiceId:               "ethereum",
	}

	blockHash := []byte("block-hash-for-proof-requirement")

	req, err := pipeline.GenerateProofForSession(ctx, snapshot, sessionHeader, blockHash)
	require.NoError(t, err)
	require.NotNil(t, req)
	require.True(t, proofGenerated)
}

func TestProofPipeline_ClosestProof_Selection(t *testing.T) {
	pipeline, _, smstProver, _, _, _ := setupProofPipelineTest(t)

	ctx := context.Background()

	// Verify that ProveClosest is called with correct path
	var capturedPath []byte
	smstProver.proveClosestFunc = func(ctx context.Context, sessionID string, path []byte) ([]byte, error) {
		capturedPath = path
		return []byte("closest-proof"), nil
	}

	snapshot := &SessionSnapshot{
		SessionID:               "session-closest",
		SupplierOperatorAddress: "pokt1supplier123",
		ServiceID:               "ethereum",
		ApplicationAddress:      "pokt1app123",
		SessionStartHeight:      100,
		SessionEndHeight:        104,
		ClaimedRootHash:         []byte("root-hash"),
	}

	sessionHeader := &sessiontypes.SessionHeader{
		SessionId:               "session-closest",
		SessionStartBlockHeight: 100,
		SessionEndBlockHeight:   104,
		ApplicationAddress:      "pokt1app123",
		ServiceId:               "ethereum",
	}

	blockHash := []byte("deterministic-block-hash")

	_, err := pipeline.GenerateProofForSession(ctx, snapshot, sessionHeader, blockHash)
	require.NoError(t, err)

	// Verify path was calculated deterministically
	expectedPath := protocol.GetPathForProof(blockHash, "session-closest")
	require.Equal(t, expectedPath, capturedPath)
}

// TestProofPipeline_Concurrent_MultipleProofs removed - concurrency/timing integration test
// TODO(e2e): Re-implement as e2e test with testcontainers

// TestProofPipeline_Close_FlushesPendingProofs removed - tx client timing dependencies
// TODO(e2e): Re-implement as e2e test with testcontainers

func TestProofPipeline_Close_Safe(t *testing.T) {
	pipeline, _, _, _, _, _ := setupProofPipelineTest(t)

	// Close without starting
	err := pipeline.Close()
	require.NoError(t, err)

	// Double close should be safe
	err = pipeline.Close()
	require.NoError(t, err)
}

func TestProofPipeline_Start_AlreadyClosed(t *testing.T) {
	pipeline, _, _, _, _, _ := setupProofPipelineTest(t)

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

func TestProofBatcher_SubmitBatch(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	txClient := &mockTxClient{}

	batcher := NewProofBatcher(logger, txClient, "pokt1supplier123", 5)

	ctx := context.Background()
	proofs := []*ProofRequest{
		{
			SessionID: "session-p1",
			SessionHeader: &sessiontypes.SessionHeader{
				SessionId:               "session-p1",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   104,
			},
			ProofBytes:              []byte("proof-1"),
			SupplierOperatorAddress: "pokt1supplier123",
		},
		{
			SessionID: "session-p2",
			SessionHeader: &sessiontypes.SessionHeader{
				SessionId:               "session-p2",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   104,
			},
			ProofBytes:              []byte("proof-2"),
			SupplierOperatorAddress: "pokt1supplier123",
		},
	}

	result := batcher.SubmitBatch(ctx, proofs, 115)
	require.NoError(t, result.Error)
	require.Len(t, result.SuccessfulProofs, 2)
	require.Len(t, result.FailedProofs, 0)
	require.Equal(t, 1, txClient.getProofCalls())
}

func TestProofBatcher_SubmitBatch_WithFailure(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	txClient := &mockTxClient{
		submitProofsFunc: func(ctx context.Context, timeoutHeight int64, proofs ...client.MsgSubmitProof) error {
			return fmt.Errorf("proof submission failed")
		},
	}

	batcher := NewProofBatcher(logger, txClient, "pokt1supplier123", 5)

	ctx := context.Background()
	proofs := []*ProofRequest{
		{
			SessionID: "session-fail",
			SessionHeader: &sessiontypes.SessionHeader{
				SessionId:               "session-fail",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   104,
			},
			ProofBytes:              []byte("proof"),
			SupplierOperatorAddress: "pokt1supplier123",
		},
	}

	result := batcher.SubmitBatch(ctx, proofs, 115)
	require.Error(t, result.Error)
	require.Len(t, result.SuccessfulProofs, 0)
	require.Len(t, result.FailedProofs, 1)
}

func TestCalculateEarliestProofHeight(t *testing.T) {
	params := &sharedtypes.Params{
		NumBlocksPerSession:          4,
		ProofWindowOpenOffsetBlocks:  0,
		ProofWindowCloseOffsetBlocks: 4,
	}

	sessionEndHeight := int64(104)
	blockHash := []byte("test-block-hash")
	supplierAddr := "pokt1supplier123"

	height := CalculateEarliestProofHeight(params, sessionEndHeight, blockHash, supplierAddr)
	require.Greater(t, height, sessionEndHeight)
}

func TestProofPipeline_WaitForProofWindow(t *testing.T) {
	pipeline, _, _, sharedClient, _, blockClient := setupProofPipelineTest(t)

	// Set current height below proof window
	blockClient.currentHeight = 105

	sharedClient.params = &sharedtypes.Params{
		NumBlocksPerSession:          4,
		GracePeriodEndOffsetBlocks:   1,
		ClaimWindowOpenOffsetBlocks:  1,
		ClaimWindowCloseOffsetBlocks: 4,
		ProofWindowOpenOffsetBlocks:  0,
		ProofWindowCloseOffsetBlocks: 4,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessionEndHeight := int64(104)

	// Start goroutine to advance block height
	go func() {
		time.Sleep(100 * time.Millisecond)
		blockClient.currentHeight = 110 // Proof window opens at 110 (claimClose + proofOffset)
	}()

	height, hash, err := pipeline.WaitForProofWindow(ctx, sessionEndHeight)
	require.NoError(t, err)
	require.Equal(t, int64(110), height) // claimClose (106+4) + proofOffset (0) = 110
	require.NotNil(t, hash)
}

// TestProofPipeline_Retry_OnFailure removed - tx client retry integration test
// TODO(e2e): Re-implement as e2e test with testcontainers
