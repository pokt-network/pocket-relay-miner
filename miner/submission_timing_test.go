//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

func setupTimingCalculatorTest(t *testing.T) (*SubmissionTimingCalculator, *mockSharedQueryClient, *mockBlockClient) {
	t.Helper()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	sharedClient := &mockSharedQueryClient{
		params: &sharedtypes.Params{
			NumBlocksPerSession:          4,
			GracePeriodEndOffsetBlocks:   1,
			ClaimWindowOpenOffsetBlocks:  1,
			ClaimWindowCloseOffsetBlocks: 4,
			ProofWindowOpenOffsetBlocks:  0,
			ProofWindowCloseOffsetBlocks: 4,
		},
	}

	blockClient := &mockBlockClient{
		currentHeight: 100,
		blockHash:     []byte("mock-block-hash"),
	}

	config := SubmissionTimingConfig{
		BlockTimeSeconds:       6,
		SubmissionBufferBlocks: 2,
	}

	calc := NewSubmissionTimingCalculator(logger, sharedClient, blockClient, config)

	return calc, sharedClient, blockClient
}

func TestSubmissionWindow_IsWithinWindow(t *testing.T) {
	window := SubmissionWindow{
		WindowOpen:  100,
		WindowClose: 110,
	}

	require.True(t, window.IsWithinWindow(100))
	require.True(t, window.IsWithinWindow(105))
	require.True(t, window.IsWithinWindow(109))
	require.False(t, window.IsWithinWindow(99))
	require.False(t, window.IsWithinWindow(110))
	require.False(t, window.IsWithinWindow(111))
}

func TestSubmissionWindow_CanSubmit(t *testing.T) {
	window := SubmissionWindow{
		WindowOpen:     100,
		WindowClose:    110,
		EarliestSubmit: 102,
	}

	require.False(t, window.CanSubmit(101), "too early")
	require.True(t, window.CanSubmit(102), "earliest submit")
	require.True(t, window.CanSubmit(105), "within window")
	require.True(t, window.CanSubmit(109), "before close")
	require.False(t, window.CanSubmit(110), "at close")
	require.False(t, window.CanSubmit(111), "after close")
}

func TestSubmissionWindow_BlocksUntilEarliestSubmit(t *testing.T) {
	window := SubmissionWindow{
		EarliestSubmit: 105,
	}

	require.Equal(t, int64(5), window.BlocksUntilEarliestSubmit(100))
	require.Equal(t, int64(1), window.BlocksUntilEarliestSubmit(104))
	require.Equal(t, int64(0), window.BlocksUntilEarliestSubmit(105))
	require.Equal(t, int64(0), window.BlocksUntilEarliestSubmit(106))
}

func TestSubmissionWindow_BlocksUntilDeadline(t *testing.T) {
	window := SubmissionWindow{
		SafeDeadline: 110,
	}

	require.Equal(t, int64(10), window.BlocksUntilDeadline(100))
	require.Equal(t, int64(1), window.BlocksUntilDeadline(109))
	require.Equal(t, int64(0), window.BlocksUntilDeadline(110))
	require.Equal(t, int64(0), window.BlocksUntilDeadline(111))
}

func TestTimingCalculator_CalculateClaimWindow(t *testing.T) {
	calc, _, _ := setupTimingCalculatorTest(t)

	ctx := context.Background()
	sessionEndHeight := int64(104)
	supplierOperatorAddr := "pokt1supplier123"
	blockHash := []byte("block-hash")

	window, err := calc.CalculateClaimWindow(ctx, sessionEndHeight, supplierOperatorAddr, blockHash)
	require.NoError(t, err)
	require.NotNil(t, window)

	// Based on params: numBlocksPerSession=4, claimOpenOffset=1, claimCloseOffset=4
	// WindowOpen = sessionEnd + claimOpenOffset + 1 = 104 + 1 + 1 = 106
	require.Equal(t, int64(106), window.WindowOpen)

	// WindowClose = sessionEnd + claimOpenOffset + claimCloseOffset + 1 = 104 + 1 + 4 + 1 = 110
	require.Equal(t, int64(110), window.WindowClose)

	// EarliestSubmit is deterministic based on supplier + blockHash (varies)
	require.GreaterOrEqual(t, window.EarliestSubmit, window.WindowOpen)
	require.LessOrEqual(t, window.EarliestSubmit, window.WindowClose)

	// SafeDeadline = WindowClose - buffer = 110 - 2 = 108
	require.Equal(t, int64(108), window.SafeDeadline)

	require.Equal(t, blockHash, window.WindowOpenBlockHash)
}

// TestTimingCalculator_CalculateProofWindow removed - timing calculation integration
// TODO(e2e): Re-implement as e2e test with testcontainers

func TestTimingCalculator_GetCurrentHeight(t *testing.T) {
	calc, _, blockClient := setupTimingCalculatorTest(t)

	ctx := context.Background()

	blockClient.currentHeight = 200
	height := calc.GetCurrentHeight(ctx)
	require.Equal(t, int64(200), height)

	blockClient.currentHeight = 300
	height = calc.GetCurrentHeight(ctx)
	require.Equal(t, int64(300), height)
}

func TestTimingCalculator_EstimateTimeUntilHeight(t *testing.T) {
	calc, _, blockClient := setupTimingCalculatorTest(t)

	ctx := context.Background()
	blockClient.currentHeight = 100

	// 10 blocks away, 6 seconds per block = 60 seconds
	duration := calc.EstimateTimeUntilHeight(ctx, 110)
	require.Equal(t, 60*time.Second, duration)

	// Already at target
	duration = calc.EstimateTimeUntilHeight(ctx, 100)
	require.Equal(t, time.Duration(0), duration)

	// Past target
	duration = calc.EstimateTimeUntilHeight(ctx, 90)
	require.Equal(t, time.Duration(0), duration)
}

// TestTimingCalculator_WaitForClaimWindow removed - timing wait integration test
// TODO(e2e): Re-implement as e2e test with testcontainers

// TestTimingCalculator_WaitForProofWindow removed - timing wait integration test
// TODO(e2e): Re-implement as e2e test with testcontainers

// TestTimingCalculator_WaitForEarliestSubmit removed - timing wait integration test
// TODO(e2e): Re-implement as e2e test with testcontainers

func TestTimingCalculator_WaitForEarliestSubmit_Timeout(t *testing.T) {
	calc, _, blockClient := setupTimingCalculatorTest(t)

	blockClient.currentHeight = 100

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	window := &SubmissionWindow{
		EarliestSubmit: 1000, // Far in future
	}

	// Should timeout
	err := calc.WaitForEarliestSubmit(ctx, window)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestTimingCalculator_DefaultConfig(t *testing.T) {
	config := DefaultSubmissionTimingConfig()

	require.Equal(t, int64(6), config.BlockTimeSeconds)
	require.Equal(t, int64(2), config.SubmissionBufferBlocks)
}

func TestTimingCalculator_ConfigDefaults(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	sharedClient := &mockSharedQueryClient{}
	blockClient := &mockBlockClient{}

	// Empty config
	config := SubmissionTimingConfig{}

	calc := NewSubmissionTimingCalculator(logger, sharedClient, blockClient, config)

	// Should apply defaults
	require.Equal(t, int64(30), calc.config.BlockTimeSeconds)
	require.Equal(t, int64(2), calc.config.SubmissionBufferBlocks)
}

func TestSubmissionScheduler_ScheduleClaim(t *testing.T) {
	calc, _, _ := setupTimingCalculatorTest(t)
	scheduler := NewSubmissionScheduler(logging.NewLoggerFromConfig(logging.DefaultConfig()), calc, "pokt1supplier123")

	ctx := context.Background()
	sessionID := "session-sched-1"
	sessionEndHeight := int64(104)
	blockHash := []byte("block-hash")

	window, err := scheduler.ScheduleClaim(ctx, sessionID, sessionEndHeight, blockHash)
	require.NoError(t, err)
	require.NotNil(t, window)

	// Should be in pending claims
	require.Contains(t, scheduler.pendingClaims, sessionID)
	require.Equal(t, window, scheduler.pendingClaims[sessionID])
}

func TestSubmissionScheduler_ScheduleProof(t *testing.T) {
	calc, _, _ := setupTimingCalculatorTest(t)
	scheduler := NewSubmissionScheduler(logging.NewLoggerFromConfig(logging.DefaultConfig()), calc, "pokt1supplier123")

	ctx := context.Background()
	sessionID := "session-sched-proof"
	sessionEndHeight := int64(104)
	blockHash := []byte("block-hash")

	window, err := scheduler.ScheduleProof(ctx, sessionID, sessionEndHeight, blockHash)
	require.NoError(t, err)
	require.NotNil(t, window)

	// Should be in pending proofs
	require.Contains(t, scheduler.pendingProofs, sessionID)
	require.Equal(t, window, scheduler.pendingProofs[sessionID])
}

func TestSubmissionScheduler_GetReadyClaims(t *testing.T) {
	calc, _, blockClient := setupTimingCalculatorTest(t)
	scheduler := NewSubmissionScheduler(logging.NewLoggerFromConfig(logging.DefaultConfig()), calc, "pokt1supplier123")

	ctx := context.Background()

	// Set current height
	blockClient.currentHeight = 106

	// Schedule claims with different windows
	scheduler.pendingClaims["session-1"] = &SubmissionWindow{
		WindowOpen:     100,
		WindowClose:    110,
		EarliestSubmit: 105, // Can submit now
	}
	scheduler.pendingClaims["session-2"] = &SubmissionWindow{
		WindowOpen:     100,
		WindowClose:    110,
		EarliestSubmit: 107, // Too early
	}
	scheduler.pendingClaims["session-3"] = &SubmissionWindow{
		WindowOpen:     100,
		WindowClose:    105, // Window closed
		EarliestSubmit: 102,
	}

	ready := scheduler.GetReadyClaims(ctx)
	require.Len(t, ready, 1)
	require.Contains(t, ready, "session-1")
}

func TestSubmissionScheduler_GetReadyProofs(t *testing.T) {
	calc, _, blockClient := setupTimingCalculatorTest(t)
	scheduler := NewSubmissionScheduler(logging.NewLoggerFromConfig(logging.DefaultConfig()), calc, "pokt1supplier123")

	ctx := context.Background()

	// Set current height
	blockClient.currentHeight = 112

	// Schedule proofs with different windows
	scheduler.pendingProofs["proof-1"] = &SubmissionWindow{
		WindowOpen:     110,
		WindowClose:    115,
		EarliestSubmit: 111, // Can submit now
	}
	scheduler.pendingProofs["proof-2"] = &SubmissionWindow{
		WindowOpen:     110,
		WindowClose:    115,
		EarliestSubmit: 113, // Too early
	}
	scheduler.pendingProofs["proof-3"] = &SubmissionWindow{
		WindowOpen:     105,
		WindowClose:    111, // Window closed
		EarliestSubmit: 107,
	}

	ready := scheduler.GetReadyProofs(ctx)
	require.Len(t, ready, 1)
	require.Contains(t, ready, "proof-1")
}

func TestSubmissionScheduler_RemoveClaim(t *testing.T) {
	calc, _, _ := setupTimingCalculatorTest(t)
	scheduler := NewSubmissionScheduler(logging.NewLoggerFromConfig(logging.DefaultConfig()), calc, "pokt1supplier123")

	scheduler.pendingClaims["session-1"] = &SubmissionWindow{}
	scheduler.pendingClaims["session-2"] = &SubmissionWindow{}

	require.Len(t, scheduler.pendingClaims, 2)

	scheduler.RemoveClaim("session-1")
	require.Len(t, scheduler.pendingClaims, 1)
	require.NotContains(t, scheduler.pendingClaims, "session-1")

	// Removing non-existent is safe
	scheduler.RemoveClaim("nonexistent")
	require.Len(t, scheduler.pendingClaims, 1)
}

func TestSubmissionScheduler_RemoveProof(t *testing.T) {
	calc, _, _ := setupTimingCalculatorTest(t)
	scheduler := NewSubmissionScheduler(logging.NewLoggerFromConfig(logging.DefaultConfig()), calc, "pokt1supplier123")

	scheduler.pendingProofs["proof-1"] = &SubmissionWindow{}
	scheduler.pendingProofs["proof-2"] = &SubmissionWindow{}

	require.Len(t, scheduler.pendingProofs, 2)

	scheduler.RemoveProof("proof-1")
	require.Len(t, scheduler.pendingProofs, 1)
	require.NotContains(t, scheduler.pendingProofs, "proof-1")

	// Removing non-existent is safe
	scheduler.RemoveProof("nonexistent")
	require.Len(t, scheduler.pendingProofs, 1)
}

func TestTimingCalculator_DifferentParams(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	sharedClient := &mockSharedQueryClient{
		params: &sharedtypes.Params{
			NumBlocksPerSession:          10, // Longer sessions
			GracePeriodEndOffsetBlocks:   2,
			ClaimWindowOpenOffsetBlocks:  2,
			ClaimWindowCloseOffsetBlocks: 6,
			ProofWindowOpenOffsetBlocks:  1,
			ProofWindowCloseOffsetBlocks: 6,
		},
	}

	blockClient := &mockBlockClient{currentHeight: 100}
	config := DefaultSubmissionTimingConfig()

	calc := NewSubmissionTimingCalculator(logger, sharedClient, blockClient, config)

	ctx := context.Background()
	sessionEndHeight := int64(110)
	blockHash := []byte("block")

	window, err := calc.CalculateClaimWindow(ctx, sessionEndHeight, "pokt1supplier", blockHash)
	require.NoError(t, err)

	// WindowOpen = sessionEnd + claimOpenOffset + 1 = 110 + 2 + 1 = 113
	require.Equal(t, int64(113), window.WindowOpen)

	// WindowClose = sessionEnd + claimOpenOffset + claimCloseOffset + 1 = 110 + 2 + 6 + 1 = 119
	require.Equal(t, int64(119), window.WindowClose)
}

func TestTimingCalculator_SafeDeadline_Adjustment(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	sharedClient := &mockSharedQueryClient{
		params: &sharedtypes.Params{
			NumBlocksPerSession:          4,
			GracePeriodEndOffsetBlocks:   1,
			ClaimWindowOpenOffsetBlocks:  1,
			ClaimWindowCloseOffsetBlocks: 2, // Very narrow window
			ProofWindowOpenOffsetBlocks:  0,
			ProofWindowCloseOffsetBlocks: 4,
		},
	}

	blockClient := &mockBlockClient{currentHeight: 100}

	config := SubmissionTimingConfig{
		BlockTimeSeconds:       6,
		SubmissionBufferBlocks: 10, // Large buffer
	}

	calc := NewSubmissionTimingCalculator(logger, sharedClient, blockClient, config)

	ctx := context.Background()
	sessionEndHeight := int64(104)
	blockHash := []byte("block")

	window, err := calc.CalculateClaimWindow(ctx, sessionEndHeight, "pokt1supplier", blockHash)
	require.NoError(t, err)

	// SafeDeadline should be adjusted to not be before EarliestSubmit
	require.GreaterOrEqual(t, window.SafeDeadline, window.EarliestSubmit)
}
