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
	// Use the shared params that were effective at the session's height, not the
	// live params. After a session-length change (poktroll #543 anchored grid),
	// computing an old-epoch session's window with new-epoch num_blocks_per_session
	// would resolve the wrong session grid and submit at the wrong window.
	sharedParams, err := c.sharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
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
	// Use the shared params that were effective at the session's height, not the
	// live params. After a session-length change (poktroll #543 anchored grid),
	// computing an old-epoch session's window with new-epoch num_blocks_per_session
	// would resolve the wrong session grid and submit at the wrong window.
	sharedParams, err := c.sharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
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
	// Use the shared params that were effective at the session's height, not the
	// live params. After a session-length change (poktroll #543 anchored grid),
	// computing an old-epoch session's window with new-epoch num_blocks_per_session
	// would resolve the wrong session grid and submit at the wrong window.
	sharedParams, err := c.sharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	windowOpen := sharedtypes.GetClaimWindowOpenHeight(sharedParams, sessionEndHeight)

	// Wait for the window to open. No block hash is captured — see
	// waitForBlockHeight for why passing nil is both correct and required here.
	if err := c.waitForBlockHeight(ctx, windowOpen); err != nil {
		return nil, fmt.Errorf("failed to wait for claim window: %w", err)
	}

	return c.CalculateClaimWindow(ctx, sessionEndHeight, supplierOperatorAddr, nil)
}

// WaitForProofWindow waits for the proof window to open and returns the window info.
func (c *SubmissionTimingCalculator) WaitForProofWindow(
	ctx context.Context,
	sessionEndHeight int64,
	supplierOperatorAddr string,
) (*SubmissionWindow, error) {
	// Use the shared params that were effective at the session's height, not the
	// live params. After a session-length change (poktroll #543 anchored grid),
	// computing an old-epoch session's window with new-epoch num_blocks_per_session
	// would resolve the wrong session grid and submit at the wrong window.
	sharedParams, err := c.sharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	windowOpen := sharedtypes.GetProofWindowOpenHeight(sharedParams, sessionEndHeight)

	// Wait for the window to open. No block hash is captured — see
	// waitForBlockHeight for why passing nil is both correct and required here.
	if err := c.waitForBlockHeight(ctx, windowOpen); err != nil {
		return nil, fmt.Errorf("failed to wait for proof window: %w", err)
	}

	return c.CalculateProofWindow(ctx, sessionEndHeight, supplierOperatorAddr, nil)
}

// WaitForEarliestSubmit waits until the supplier can submit.
func (c *SubmissionTimingCalculator) WaitForEarliestSubmit(
	ctx context.Context,
	window *SubmissionWindow,
) error {
	return c.waitForBlockHeight(ctx, window.EarliestSubmit)
}

// waitForBlockHeight blocks until the chain has reached targetHeight.
//
// It deliberately returns NO block hash. The obvious-looking
// `if block.Height() >= targetHeight { return block.Hash() }` returns whatever
// block happens to be current, not the block AT targetHeight — the two differ
// whenever the poll lands late or several blocks arrive between ticks. That hash
// is the seed for the claim/proof distribution offset, so returning the wrong one
// silently places submissions in the wrong window and gets suppliers slashed with
// no diagnostic. blockClient here exposes only LastBlock, so the correct hash is
// not obtainable at all on this path.
//
// Callers therefore pass a nil window-open hash, which is what the chain expects:
// both GetEarliestSupplierClaimCommitHeight and GetEarliestSupplierProofCommitHeight
// ignore the hash and return the plain window-open height — the hash seeding is
// commented out in x/shared/types/session.go (verified identical in poktroll v0.1.34
// and v0.1.35, so this is not a v0.1.35 change). query/query.go:487,516 already pass
// nil for the same reason.
func (c *SubmissionTimingCalculator) waitForBlockHeight(
	ctx context.Context,
	targetHeight int64,
) error {
	ticker := time.NewTicker(time.Duration(c.config.BlockTimeSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
			if c.blockClient.LastBlock(ctx).Height() >= targetHeight {
				return nil
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
