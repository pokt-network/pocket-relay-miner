package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/alitto/pond/v2"

	"github.com/pokt-network/pocket-relay-miner/logging"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// ClaimInclusionOutcome is the result of polling GetClaim after a successful
// claim tx broadcast. Values are stable strings — metrics labels and
// submission-tracker JSON depend on them.
type ClaimInclusionOutcome string

const (
	// ClaimInclusionFound means GetClaim returned the claim for this (supplier,
	// sessionID) before the claim window closed — the claim is on-chain.
	ClaimInclusionFound ClaimInclusionOutcome = "on_chain_found"

	// ClaimInclusionMissing means the claim window closed and GetClaim still
	// returned NotFound — the claim never landed on-chain. Covers both
	// "mempool timeout" and "DeliverTx rejected" failure modes; without the
	// tx indexer we cannot distinguish them, but the operator-relevant
	// signal (did it land?) is unambiguous.
	ClaimInclusionMissing ClaimInclusionOutcome = "on_chain_missing"

	// ClaimInclusionPollError means we could not determine the outcome due to
	// repeated RPC errors against the proof query client. Treat as advisory.
	ClaimInclusionPollError ClaimInclusionOutcome = "poll_error"
)

// InclusionTrackerConfig configures the post-broadcast claim inclusion poller.
type InclusionTrackerConfig struct {
	// Disabled turns the poller off entirely. When true, ScheduleClaimCheck
	// is a no-op and no submission-tracker updates or metrics fire.
	Disabled bool

	// PollInterval is how often to call GetClaim. Default 2s.
	PollInterval time.Duration

	// MaxConcurrent bounds the worker pool size. Default 64.
	MaxConcurrent int

	// MaxPollDuration is a safety cap — the poll stops after this even if the
	// claim window hasn't closed yet (e.g., if block height is stuck). Default
	// 10 minutes.
	MaxPollDuration time.Duration
}

// DefaultInclusionTrackerConfig returns sensible defaults.
func DefaultInclusionTrackerConfig() InclusionTrackerConfig {
	return InclusionTrackerConfig{
		PollInterval:    2 * time.Second,
		MaxConcurrent:   64,
		MaxPollDuration: 10 * time.Minute,
	}
}

// ClaimInclusionCheck is the payload passed to
// InclusionTracker.ScheduleClaimCheck. One check is scheduled per session per
// claim broadcast; a batched claim tx that covers N sessions produces N checks.
type ClaimInclusionCheck struct {
	Supplier         string
	ServiceID        string
	SessionID        string
	SessionEndHeight int64
	ClaimTxHash      string
}

// InclusionTracker polls the chain via GetClaim(supplier, sessionID) after a
// claim has been broadcast, records the real on-chain outcome to the
// submission tracker, and emits metrics. It intentionally does NOT use the
// Tendermint tx indexer (GetTx), so it works against nodes configured with
// tx_index=null or with pruned tx history. The signal it captures — "is the
// claim actually on-chain?" — is also the one operators care about in
// practice.
type InclusionTracker struct {
	logger            logging.Logger
	proofQueryClient  pocktclient.ProofQueryClient
	sharedClient      pocktclient.SharedQueryClient
	blockClient       pocktclient.BlockClient
	submissionTracker *SubmissionTracker
	cfg               InclusionTrackerConfig

	pool pond.Pool

	mu     sync.Mutex
	closed bool
}

// NewInclusionTracker creates an inclusion tracker. If cfg.Disabled is true,
// ScheduleClaimCheck returns false without work; callers may always invoke
// the tracker safely.
func NewInclusionTracker(
	logger logging.Logger,
	proofQueryClient pocktclient.ProofQueryClient,
	sharedClient pocktclient.SharedQueryClient,
	blockClient pocktclient.BlockClient,
	submissionTracker *SubmissionTracker,
	cfg InclusionTrackerConfig,
) *InclusionTracker {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 64
	}
	if cfg.MaxPollDuration <= 0 {
		cfg.MaxPollDuration = 10 * time.Minute
	}

	t := &InclusionTracker{
		logger:            logging.ForComponent(logger, "claim_inclusion_tracker"),
		proofQueryClient:  proofQueryClient,
		sharedClient:      sharedClient,
		blockClient:       blockClient,
		submissionTracker: submissionTracker,
		cfg:               cfg,
	}

	if !cfg.Disabled {
		t.pool = pond.NewPool(
			cfg.MaxConcurrent,
			pond.WithQueueSize(cfg.MaxConcurrent*8),
			pond.WithNonBlocking(true),
		)
	}

	return t
}

// ScheduleClaimCheck enqueues a poll task for one session's claim. Returns
// true if the task was accepted, false if the poller is disabled or the
// worker pool queue is saturated (non-blocking drop). Safe to call
// concurrently.
func (t *InclusionTracker) ScheduleClaimCheck(check ClaimInclusionCheck) bool {
	if t == nil || t.cfg.Disabled || t.pool == nil {
		return false
	}
	if t.proofQueryClient == nil || t.sharedClient == nil || t.blockClient == nil {
		return false
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	t.mu.Unlock()

	_, accepted := t.pool.TrySubmit(func() {
		t.runClaimPoll(check)
	})
	if !accepted {
		t.logger.Warn().
			Str("supplier", check.Supplier).
			Str(logging.FieldSessionID, check.SessionID).
			Msg("claim inclusion pool saturated; dropping task — no on-chain outcome will be recorded for this session")
		claimInclusionOutcomeTotal.WithLabelValues(check.Supplier, check.ServiceID, "poll_dropped").Inc()
	}
	return accepted
}

// Close drains the worker pool, allowing in-flight polls to finish. Idempotent.
func (t *InclusionTracker) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.pool != nil {
		t.pool.StopAndWait()
	}
	return nil
}

// runClaimPoll polls GetClaim until the claim appears, the claim window
// closes, or the safety cap elapses. On exit, records the outcome to the
// submission tracker and emits metrics.
//
// The polling horizon is bounded by TWO conditions, whichever fires first:
//  1. Current block height exceeds claim_window_close + 1 — the chain would
//     refuse any further MsgCreateClaim for this session, so the claim is
//     definitively "not landing" at this point.
//  2. cfg.MaxPollDuration wall-clock has elapsed — safety cap for the case
//     where the local block height feed is stuck.
func (t *InclusionTracker) runClaimPoll(check ClaimInclusionCheck) {
	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.MaxPollDuration)
	defer cancel()

	claimWindowClose, err := t.computeClaimWindowCloseHeight(ctx, check.SessionEndHeight)
	if err != nil {
		t.logger.Warn().
			Err(err).
			Str("supplier", check.Supplier).
			Str(logging.FieldSessionID, check.SessionID).
			Msg("claim inclusion: failed to compute claim window close height; skipping poll")
		t.recordOutcome(ctx, check, ClaimInclusionPollError, 0)
		return
	}

	start := time.Now()
	ticker := time.NewTicker(t.cfg.PollInterval)
	defer ticker.Stop()

	outcome, height, exit := t.tickOnce(ctx, check, claimWindowClose)
	for !exit {
		select {
		case <-ctx.Done():
			// Safety cap fired. Prefer the most-recent evidence.
			if outcome == "" {
				outcome = ClaimInclusionPollError
			}
			exit = true
		case <-ticker.C:
			outcome, height, exit = t.tickOnce(ctx, check, claimWindowClose)
		}
	}

	t.logger.Debug().
		Str("supplier", check.Supplier).
		Str(logging.FieldSessionID, check.SessionID).
		Str("outcome", string(outcome)).
		Int64("inclusion_height", height).
		Dur("poll_duration", time.Since(start)).
		Msg("claim inclusion poll resolved")

	t.recordOutcome(ctx, check, outcome, height)
}

// tickOnce performs one GetClaim + window-close check. Returns (outcome,
// inclusionHeight, exit) — when exit=false the caller should keep polling.
func (t *InclusionTracker) tickOnce(
	ctx context.Context,
	check ClaimInclusionCheck,
	claimWindowClose int64,
) (ClaimInclusionOutcome, int64, bool) {
	claim, err := t.proofQueryClient.GetClaim(ctx, check.Supplier, check.SessionID)
	if err == nil && claim != nil {
		// Claim exists on-chain. The Claim object doesn't carry its own
		// inclusion height, so report 0 — the submission tracker's
		// ClaimSubmitHeight is still available for broadcast-time reference.
		return ClaimInclusionFound, 0, true
	}
	if err != nil && !isClaimNotFoundError(err) {
		// Transient RPC error — log quietly and keep polling. If we never
		// recover before claim window close, the final outcome collapses to
		// on_chain_missing (window closed, no successful lookup) which is
		// the operator-useful signal.
		t.logger.Debug().
			Err(err).
			Str("supplier", check.Supplier).
			Str(logging.FieldSessionID, check.SessionID).
			Msg("claim inclusion poll: transient GetClaim error, will retry")
	}

	currentHeight := int64(0)
	if block := t.blockClient.LastBlock(ctx); block != nil {
		currentHeight = block.Height()
	}
	if currentHeight > claimWindowClose {
		return ClaimInclusionMissing, 0, true
	}
	return "", 0, false
}

// computeClaimWindowCloseHeight returns the block height at which the claim
// window closes for the given session.
func (t *InclusionTracker) computeClaimWindowCloseHeight(ctx context.Context, sessionEndHeight int64) (int64, error) {
	params, err := t.sharedClient.GetParams(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get shared params: %w", err)
	}
	return sharedtypes.GetClaimWindowCloseHeight(params, sessionEndHeight), nil
}

// recordOutcome applies the final outcome to the submission tracker and emits
// metrics. Safe to call with inclusionHeight=0 when we don't know the height.
func (t *InclusionTracker) recordOutcome(
	ctx context.Context,
	check ClaimInclusionCheck,
	outcome ClaimInclusionOutcome,
	inclusionHeight int64,
) {
	claimInclusionOutcomeTotal.WithLabelValues(check.Supplier, check.ServiceID, string(outcome)).Inc()

	if t.submissionTracker == nil || check.ClaimTxHash == "" {
		return
	}
	if err := t.submissionTracker.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier:        check.Supplier,
		TxHash:          check.ClaimTxHash,
		Outcome:         string(outcome),
		InclusionHeight: inclusionHeight,
	}); err != nil {
		t.logger.Warn().
			Err(err).
			Str("supplier", check.Supplier).
			Str(logging.FieldSessionID, check.SessionID).
			Msg("claim inclusion: failed to persist outcome to submission tracker")
	}
}
