package miner

import (
	"context"
	"fmt"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// SubmissionTimingConfig contains configuration for submission timing.
type SubmissionTimingConfig struct {
	// BlockTimeSeconds is the estimated block time.
	BlockTimeSeconds int64

	// WindowStartBufferBlocks is blocks after window open to wait before earliest submission.
	// This spreads out supplier submissions more evenly across the window.
	WindowStartBufferBlocks int64

	// SubmissionBufferBlocks is blocks before window close to ensure submission.
	SubmissionBufferBlocks int64
}

// DefaultSubmissionTimingConfig returns sensible defaults.
func DefaultSubmissionTimingConfig() SubmissionTimingConfig {
	return SubmissionTimingConfig{
		BlockTimeSeconds:        6,
		WindowStartBufferBlocks: 10,
		SubmissionBufferBlocks:  2,
	}
}

// SubmissionWindow represents a timing window for claim or proof submission.
type SubmissionWindow struct {
	// WindowOpen is when the submission window opens.
	WindowOpen int64

	// WindowClose is when the submission window closes.
	WindowClose int64

	// EarliestSubmit is the earliest block this supplier can submit.
	// This is determined by deterministic hash spreading.
	EarliestSubmit int64

	// SafeDeadline is the last safe block to submit (with buffer).
	SafeDeadline int64

	// WindowOpenBlockHash is the hash of the window open block.
	// Used for deterministic supplier ordering.
	WindowOpenBlockHash []byte
}

// IsWithinWindow returns true if the current height is within the submission window.
func (w SubmissionWindow) IsWithinWindow(currentHeight int64) bool {
	return currentHeight >= w.WindowOpen && currentHeight < w.WindowClose
}

// CanSubmit returns true if the supplier can submit at the current height.
func (w SubmissionWindow) CanSubmit(currentHeight int64) bool {
	return currentHeight >= w.EarliestSubmit && currentHeight < w.WindowClose
}

// BlocksUntilEarliestSubmit returns blocks until the supplier can submit.
func (w SubmissionWindow) BlocksUntilEarliestSubmit(currentHeight int64) int64 {
	if currentHeight >= w.EarliestSubmit {
		return 0
	}
	return w.EarliestSubmit - currentHeight
}

// BlocksUntilDeadline returns blocks until the safe deadline.
func (w SubmissionWindow) BlocksUntilDeadline(currentHeight int64) int64 {
	if currentHeight >= w.SafeDeadline {
		return 0
	}
	return w.SafeDeadline - currentHeight
}

// SubmissionTimingCalculator provides submission timing calculations.
type SubmissionTimingCalculator struct {
	logger       logging.Logger
	config       SubmissionTimingConfig
	sharedClient client.SharedQueryClient
	blockClient  client.BlockClient
}

// NewSubmissionTimingCalculator creates a new timing calculator.
func NewSubmissionTimingCalculator(
	logger logging.Logger,
	sharedClient client.SharedQueryClient,
	blockClient client.BlockClient,
	config SubmissionTimingConfig,
) *SubmissionTimingCalculator {
	if config.BlockTimeSeconds == 0 {
		config.BlockTimeSeconds = 30
	}
	if config.WindowStartBufferBlocks == 0 {
		config.WindowStartBufferBlocks = 10
	}
	if config.SubmissionBufferBlocks == 0 {
		config.SubmissionBufferBlocks = 2
	}

	return &SubmissionTimingCalculator{
		logger:       logging.ForComponent(logger, logging.ComponentSubmissionTiming),
		config:       config,
		sharedClient: sharedClient,
		blockClient:  blockClient,
	}
}

// CalculateClaimWindow calculates the claim submission window for a session.
func (c *SubmissionTimingCalculator) CalculateClaimWindow(
	ctx context.Context,
	sessionEndHeight int64,
	supplierOperatorAddr string,
	windowOpenBlockHash []byte,
) (*SubmissionWindow, error) {
	sharedParams, err := c.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	windowOpen := sharedtypes.GetClaimWindowOpenHeight(sharedParams, sessionEndHeight)
	windowClose := sharedtypes.GetClaimWindowCloseHeight(sharedParams, sessionEndHeight)

	// Calculate protocol-defined earliest submit height (deterministic spread)
	protocolEarliestSubmit := sharedtypes.GetEarliestSupplierClaimCommitHeight(
		sharedParams,
		sessionEndHeight,
		windowOpenBlockHash,
		supplierOperatorAddr,
	)

	// Add window start buffer to spread submissions further
	earliestSubmit := protocolEarliestSubmit + c.config.WindowStartBufferBlocks

	// Ensure earliest submit doesn't exceed safe deadline
	safeDeadline := windowClose - c.config.SubmissionBufferBlocks
	if earliestSubmit > safeDeadline {
		// If buffer pushes us past safe deadline, use protocol earliest
		earliestSubmit = protocolEarliestSubmit
	}
	if safeDeadline < earliestSubmit {
		safeDeadline = earliestSubmit
	}

	return &SubmissionWindow{
		WindowOpen:          windowOpen,
		WindowClose:         windowClose,
		EarliestSubmit:      earliestSubmit,
		SafeDeadline:        safeDeadline,
		WindowOpenBlockHash: windowOpenBlockHash,
	}, nil
}

// CalculateProofWindow calculates the proof submission window for a session.
func (c *SubmissionTimingCalculator) CalculateProofWindow(
	ctx context.Context,
	sessionEndHeight int64,
	supplierOperatorAddr string,
	windowOpenBlockHash []byte,
) (*SubmissionWindow, error) {
	sharedParams, err := c.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	windowOpen := sharedtypes.GetProofWindowOpenHeight(sharedParams, sessionEndHeight)
	windowClose := sharedtypes.GetProofWindowCloseHeight(sharedParams, sessionEndHeight)

	// Calculate protocol-defined earliest submit height (deterministic spread)
	protocolEarliestSubmit := sharedtypes.GetEarliestSupplierProofCommitHeight(
		sharedParams,
		sessionEndHeight,
		windowOpenBlockHash,
		supplierOperatorAddr,
	)

	// Add window start buffer to spread submissions further
	earliestSubmit := protocolEarliestSubmit + c.config.WindowStartBufferBlocks

	// Ensure earliest submit doesn't exceed safe deadline
	safeDeadline := windowClose - c.config.SubmissionBufferBlocks
	if earliestSubmit > safeDeadline {
		// If buffer pushes us past safe deadline, use protocol earliest
		earliestSubmit = protocolEarliestSubmit
	}
	if safeDeadline < earliestSubmit {
		safeDeadline = earliestSubmit
	}

	return &SubmissionWindow{
		WindowOpen:          windowOpen,
		WindowClose:         windowClose,
		EarliestSubmit:      earliestSubmit,
		SafeDeadline:        safeDeadline,
		WindowOpenBlockHash: windowOpenBlockHash,
	}, nil
}

// WaitForClaimWindow waits for the claim window to open and returns the window info.
func (c *SubmissionTimingCalculator) WaitForClaimWindow(
	ctx context.Context,
	sessionEndHeight int64,
	supplierOperatorAddr string,
) (*SubmissionWindow, error) {
	sharedParams, err := c.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	windowOpen := sharedtypes.GetClaimWindowOpenHeight(sharedParams, sessionEndHeight)

	// Wait for window open block
	blockHash, err := c.waitForBlockHeight(ctx, windowOpen)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for claim window: %w", err)
	}

	return c.CalculateClaimWindow(ctx, sessionEndHeight, supplierOperatorAddr, blockHash)
}

// WaitForProofWindow waits for the proof window to open and returns the window info.
func (c *SubmissionTimingCalculator) WaitForProofWindow(
	ctx context.Context,
	sessionEndHeight int64,
	supplierOperatorAddr string,
) (*SubmissionWindow, error) {
	sharedParams, err := c.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	windowOpen := sharedtypes.GetProofWindowOpenHeight(sharedParams, sessionEndHeight)

	// Wait for window open block
	blockHash, err := c.waitForBlockHeight(ctx, windowOpen)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for proof window: %w", err)
	}

	return c.CalculateProofWindow(ctx, sessionEndHeight, supplierOperatorAddr, blockHash)
}

// WaitForEarliestSubmit waits until the supplier can submit.
func (c *SubmissionTimingCalculator) WaitForEarliestSubmit(
	ctx context.Context,
	window *SubmissionWindow,
) error {
	_, err := c.waitForBlockHeight(ctx, window.EarliestSubmit)
	return err
}

// waitForBlockHeight waits for a specific block height and returns its hash.
func (c *SubmissionTimingCalculator) waitForBlockHeight(
	ctx context.Context,
	targetHeight int64,
) ([]byte, error) {
	ticker := time.NewTicker(time.Duration(c.config.BlockTimeSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-ticker.C:
			block := c.blockClient.LastBlock(ctx)
			if block.Height() >= targetHeight {
				return block.Hash(), nil
			}
		}
	}
}

// GetCurrentHeight returns the current block height.
func (c *SubmissionTimingCalculator) GetCurrentHeight(ctx context.Context) int64 {
	block := c.blockClient.LastBlock(ctx)
	return block.Height()
}

// EstimateTimeUntilHeight estimates the time until a target height.
func (c *SubmissionTimingCalculator) EstimateTimeUntilHeight(
	ctx context.Context,
	targetHeight int64,
) time.Duration {
	currentHeight := c.GetCurrentHeight(ctx)
	if currentHeight >= targetHeight {
		return 0
	}

	blocksRemaining := targetHeight - currentHeight
	return time.Duration(blocksRemaining*c.config.BlockTimeSeconds) * time.Second
}

// SubmissionScheduler helps schedule submissions across multiple sessions.
type SubmissionScheduler struct {
	logger           logging.Logger
	timingCalculator *SubmissionTimingCalculator
	supplierOperator string

	// Pending submissions
	pendingClaims map[string]*SubmissionWindow // sessionID -> window
	pendingProofs map[string]*SubmissionWindow
}

// NewSubmissionScheduler creates a new submission scheduler.
func NewSubmissionScheduler(
	logger logging.Logger,
	timingCalculator *SubmissionTimingCalculator,
	supplierOperator string,
) *SubmissionScheduler {
	return &SubmissionScheduler{
		logger:           logging.ForSupplierComponent(logger, logging.ComponentSubmissionSched, supplierOperator),
		timingCalculator: timingCalculator,
		supplierOperator: supplierOperator,
		pendingClaims:    make(map[string]*SubmissionWindow),
		pendingProofs:    make(map[string]*SubmissionWindow),
	}
}

// ScheduleClaim schedules a claim for submission.
func (s *SubmissionScheduler) ScheduleClaim(
	ctx context.Context,
	sessionID string,
	sessionEndHeight int64,
	windowOpenBlockHash []byte,
) (*SubmissionWindow, error) {
	window, err := s.timingCalculator.CalculateClaimWindow(
		ctx,
		sessionEndHeight,
		s.supplierOperator,
		windowOpenBlockHash,
	)
	if err != nil {
		return nil, err
	}

	s.pendingClaims[sessionID] = window
	return window, nil
}

// ScheduleProof schedules a proof for submission.
func (s *SubmissionScheduler) ScheduleProof(
	ctx context.Context,
	sessionID string,
	sessionEndHeight int64,
	windowOpenBlockHash []byte,
) (*SubmissionWindow, error) {
	window, err := s.timingCalculator.CalculateProofWindow(
		ctx,
		sessionEndHeight,
		s.supplierOperator,
		windowOpenBlockHash,
	)
	if err != nil {
		return nil, err
	}

	s.pendingProofs[sessionID] = window
	return window, nil
}

// GetReadyClaims returns sessions that are ready for claim submission.
func (s *SubmissionScheduler) GetReadyClaims(ctx context.Context) []string {
	currentHeight := s.timingCalculator.GetCurrentHeight(ctx)

	var ready []string
	for sessionID, window := range s.pendingClaims {
		if window.CanSubmit(currentHeight) {
			ready = append(ready, sessionID)
		}
	}
	return ready
}

// GetReadyProofs returns sessions that are ready for proof submission.
func (s *SubmissionScheduler) GetReadyProofs(ctx context.Context) []string {
	currentHeight := s.timingCalculator.GetCurrentHeight(ctx)

	var ready []string
	for sessionID, window := range s.pendingProofs {
		if window.CanSubmit(currentHeight) {
			ready = append(ready, sessionID)
		}
	}
	return ready
}

// RemoveClaim removes a claim from the schedule.
func (s *SubmissionScheduler) RemoveClaim(sessionID string) {
	delete(s.pendingClaims, sessionID)
}

// RemoveProof removes a proof from the schedule.
func (s *SubmissionScheduler) RemoveProof(sessionID string) {
	delete(s.pendingProofs, sessionID)
}
