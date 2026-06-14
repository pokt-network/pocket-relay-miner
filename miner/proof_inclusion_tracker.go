package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/alitto/pond/v2"

	"github.com/pokt-network/pocket-relay-miner/logging"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// ProofInclusionQueryClient is the subset of the proof query client the
// ProofInclusionTracker needs: a fresh (uncached) GetProof. The concrete
// *query.proofQueryClient satisfies it. Kept narrow so the tracker is trivial
// to unit test.
type ProofInclusionQueryClient interface {
	GetProof(ctx context.Context, supplierOperatorAddress, sessionID string) (*prooftypes.Proof, error)
}

// ProofInclusionOutcome is the result of polling GetProof after a successful
// proof tx broadcast. Values are stable strings — metric labels and
// submission-tracker JSON depend on them. Mirrors ClaimInclusionOutcome.
type ProofInclusionOutcome string

const (
	// ProofInclusionFound means GetProof returned the proof for this (supplier,
	// sessionID) before the proof window closed — the proof is on-chain
	// (possibly after one or more in-window rebroadcasts).
	ProofInclusionFound ProofInclusionOutcome = "on_chain_found"

	// ProofInclusionMissing means the proof window closed and GetProof still
	// returned NotFound — the proof never landed. This is the previously-silent
	// PROOF_MISSING forfeit, now observed.
	ProofInclusionMissing ProofInclusionOutcome = "on_chain_missing"

	// ProofInclusionPollError means the outcome could not be determined due to
	// repeated RPC errors against the proof query client.
	ProofInclusionPollError ProofInclusionOutcome = "poll_error"
)

// ProofInclusionTrackerConfig configures the post-broadcast proof inclusion
// poller + rebroadcaster.
type ProofInclusionTrackerConfig struct {
	// Disabled turns the poller off entirely.
	Disabled bool

	// PollInterval is how often to call GetProof. Default 3s.
	PollInterval time.Duration

	// MaxConcurrent bounds the worker pool size. Default 64.
	MaxConcurrent int

	// MaxPollDuration is a wall-clock safety cap. Default 12 minutes (one mainnet
	// proof window of 10 blocks at ~64s plus headroom).
	MaxPollDuration time.Duration

	// MaxRebroadcasts caps how many times a still-missing proof is re-submitted
	// while the window is open. 0 disables rebroadcast (observe-only). Default 2.
	MaxRebroadcasts int

	// RebroadcastSafetyBlocks stops rebroadcasting once the chain is within this
	// many blocks of proof-window-close, so a re-submit can't land after the
	// window. Default 1.
	RebroadcastSafetyBlocks int64
}

// DefaultProofInclusionTrackerConfig returns sensible defaults.
func DefaultProofInclusionTrackerConfig() ProofInclusionTrackerConfig {
	return ProofInclusionTrackerConfig{
		PollInterval:            3 * time.Second,
		MaxConcurrent:           64,
		MaxPollDuration:         12 * time.Minute,
		MaxRebroadcasts:         2,
		RebroadcastSafetyBlocks: 1,
	}
}

// ProofInclusionCheck is the payload passed to ScheduleProofCheck. One check is
// scheduled per session per proof broadcast.
type ProofInclusionCheck struct {
	Supplier         string
	ServiceID        string
	SessionID        string
	SessionEndHeight int64
	ProofTxHash      string

	// SubmitHeight is the block height at which the original proof was broadcast.
	// Rebroadcasts are gated on the chain advancing past it (then past each
	// prior attempt), so retries land in LATER, less-congested window blocks
	// instead of being burned in the same block as the original. Zero disables
	// the grace/spacing gate (legacy: rebroadcast as soon as missing).
	SubmitHeight int64

	// Resubmit re-broadcasts THIS session's proof and returns the new tx hash.
	// It is invoked only while the proof window is open and the proof is still
	// missing on-chain. If nil, the tracker is observe-only for this check.
	Resubmit func(ctx context.Context) (string, error)
}

// ProofInclusionTracker polls GetProof(supplier, sessionID) after a proof has
// been broadcast. While the proof is missing and the proof window is still
// open, it re-broadcasts (bounded by MaxRebroadcasts) — the fix for silent
// PROOF_MISSING forfeits, where a CheckTx-accepted proof missed its single
// burst block and was never retried despite empty capacity later in the window.
// It records the real on-chain outcome to the submission tracker and emits
// metrics. Like InclusionTracker it uses module state (GetProof), not the
// Tendermint tx indexer, so it works on nodes with tx_index=null.
type ProofInclusionTracker struct {
	logger            logging.Logger
	proofClient       ProofInclusionQueryClient
	sharedClient      pocktclient.SharedQueryClient
	blockClient       pocktclient.BlockClient
	submissionTracker *SubmissionTracker
	cfg               ProofInclusionTrackerConfig

	pool pond.Pool

	mu     sync.Mutex
	closed bool
}

// NewProofInclusionTracker creates a proof inclusion tracker. If cfg.Disabled is
// true, ScheduleProofCheck returns false without work.
func NewProofInclusionTracker(
	logger logging.Logger,
	proofClient ProofInclusionQueryClient,
	sharedClient pocktclient.SharedQueryClient,
	blockClient pocktclient.BlockClient,
	submissionTracker *SubmissionTracker,
	cfg ProofInclusionTrackerConfig,
) *ProofInclusionTracker {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 64
	}
	if cfg.MaxPollDuration <= 0 {
		cfg.MaxPollDuration = 12 * time.Minute
	}
	if cfg.MaxRebroadcasts < 0 {
		cfg.MaxRebroadcasts = 0
	}
	if cfg.RebroadcastSafetyBlocks < 0 {
		cfg.RebroadcastSafetyBlocks = 0
	}

	t := &ProofInclusionTracker{
		logger:            logging.ForComponent(logger, "proof_inclusion_tracker"),
		proofClient:       proofClient,
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

// ScheduleProofCheck enqueues a poll+rebroadcast task for one session's proof.
// Returns true if accepted, false if disabled or the queue is saturated
// (non-blocking drop). Safe to call concurrently.
func (t *ProofInclusionTracker) ScheduleProofCheck(check ProofInclusionCheck) bool {
	if t == nil || t.cfg.Disabled || t.pool == nil {
		return false
	}
	if t.proofClient == nil || t.sharedClient == nil || t.blockClient == nil {
		return false
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	t.mu.Unlock()

	_, accepted := t.pool.TrySubmit(func() {
		t.runProofPoll(check)
	})
	if !accepted {
		t.logger.Warn().
			Str("supplier", check.Supplier).
			Str(logging.FieldSessionID, check.SessionID).
			Msg("proof inclusion pool saturated; dropping task — no on-chain outcome will be recorded for this session")
		proofInclusionOutcomeTotal.WithLabelValues(check.Supplier, check.ServiceID, "poll_dropped").Inc()
	}
	return accepted
}

// Close drains the worker pool, allowing in-flight polls to finish. Idempotent.
func (t *ProofInclusionTracker) Close() error {
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

// runProofPoll polls GetProof until the proof appears, the proof window closes,
// or the safety cap elapses. While the proof is missing and the window is open
// it rebroadcasts (bounded). On exit it records the outcome + metrics.
func (t *ProofInclusionTracker) runProofPoll(check ProofInclusionCheck) {
	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.MaxPollDuration)
	defer cancel()

	proofWindowClose, err := t.computeProofWindowCloseHeight(ctx, check.SessionEndHeight)
	if err != nil {
		t.logger.Warn().
			Err(err).
			Str("supplier", check.Supplier).
			Str(logging.FieldSessionID, check.SessionID).
			Msg("proof inclusion: failed to compute proof window close height; skipping poll")
		t.recordOutcome(ctx, check, ProofInclusionPollError, 0, 0, "")
		return
	}

	start := time.Now()
	ticker := time.NewTicker(t.cfg.PollInterval)
	defer ticker.Stop()

	rebroadcasts := 0
	lastTxHash := ""
	// lastAttemptHeight seeds from the original broadcast height: the first
	// rebroadcast only fires once the chain advances past it (1-block grace for
	// the original to land), and each subsequent rebroadcast only once height
	// advances again (≥1 block apart). This is what spreads retries into the
	// empty later blocks of the proof window instead of burning them in the
	// burst block alongside the original.
	lastAttemptHeight := check.SubmitHeight

	outcome, height, exit := t.tickOnce(ctx, &check, proofWindowClose, &rebroadcasts, &lastTxHash, &lastAttemptHeight)
	for !exit {
		select {
		case <-ctx.Done():
			if outcome == "" {
				outcome = ProofInclusionPollError
			}
			exit = true
		case <-ticker.C:
			outcome, height, exit = t.tickOnce(ctx, &check, proofWindowClose, &rebroadcasts, &lastTxHash, &lastAttemptHeight)
		}
	}

	t.logger.Debug().
		Str("supplier", check.Supplier).
		Str(logging.FieldSessionID, check.SessionID).
		Str("outcome", string(outcome)).
		Int64("inclusion_height", height).
		Int("rebroadcasts", rebroadcasts).
		Dur("poll_duration", time.Since(start)).
		Msg("proof inclusion poll resolved")

	t.recordOutcome(ctx, check, outcome, height, rebroadcasts, lastTxHash)
}

// tickOnce performs one GetProof check and, if warranted, one rebroadcast.
// Returns (outcome, inclusionHeight, exit); when exit=false keep polling.
func (t *ProofInclusionTracker) tickOnce(
	ctx context.Context,
	check *ProofInclusionCheck,
	proofWindowClose int64,
	rebroadcasts *int,
	lastTxHash *string,
	lastAttemptHeight *int64,
) (ProofInclusionOutcome, int64, bool) {
	// Read chain height first so a Found outcome can report the height at which
	// the proof was observed on-chain (poll-granularity, not the exact landing
	// block).
	currentHeight := int64(0)
	if block := t.blockClient.LastBlock(ctx); block != nil {
		currentHeight = block.Height()
	}

	proof, err := t.proofClient.GetProof(ctx, check.Supplier, check.SessionID)
	if err == nil && proof != nil {
		return ProofInclusionFound, currentHeight, true
	}
	if err != nil && !isClaimNotFoundError(err) {
		// Transient RPC error — keep polling; if never resolved before window
		// close the outcome collapses to on_chain_missing.
		t.logger.Debug().
			Err(err).
			Str("supplier", check.Supplier).
			Str(logging.FieldSessionID, check.SessionID).
			Msg("proof inclusion poll: transient GetProof error, will retry")
	}

	if currentHeight > proofWindowClose {
		return ProofInclusionMissing, 0, true
	}

	// Proof missing, window still open → rebroadcast if allowed.
	// Gates:
	//   - budget not exhausted (MaxRebroadcasts)
	//   - far enough from window close that a resend can still land (safety)
	//   - chain advanced past the last attempt (grace for the original on the
	//     first rebroadcast, ≥1-block spacing thereafter) — so retries go into
	//     the empty later blocks of the window rather than the burst block. A
	//     zero SubmitHeight disables this gate (legacy immediate behavior).
	if check.Resubmit != nil &&
		*rebroadcasts < t.cfg.MaxRebroadcasts &&
		currentHeight <= proofWindowClose-t.cfg.RebroadcastSafetyBlocks &&
		currentHeight > *lastAttemptHeight {
		newHash, resubErr := check.Resubmit(ctx)
		*rebroadcasts++
		*lastAttemptHeight = currentHeight
		if resubErr != nil {
			proofRebroadcastsTotal.WithLabelValues(check.Supplier, check.ServiceID, "error").Inc()
			t.logger.Warn().
				Err(resubErr).
				Str("supplier", check.Supplier).
				Str(logging.FieldSessionID, check.SessionID).
				Int("attempt", *rebroadcasts).
				Msg("proof rebroadcast failed")
		} else {
			proofRebroadcastsTotal.WithLabelValues(check.Supplier, check.ServiceID, "success").Inc()
			if newHash != "" {
				*lastTxHash = newHash
			}
			t.logger.Info().
				Str("supplier", check.Supplier).
				Str(logging.FieldSessionID, check.SessionID).
				Int("attempt", *rebroadcasts).
				Int64("current_height", currentHeight).
				Int64("proof_window_close", proofWindowClose).
				Str("new_proof_tx_hash", newHash).
				Msg("rebroadcast proof (CheckTx-accepted but not yet on-chain)")
		}
	}

	return "", 0, false
}

// computeProofWindowCloseHeight returns the height at which the proof window
// closes for the given session, using params effective at the session's height
// (poktroll #543 anchored grid).
func (t *ProofInclusionTracker) computeProofWindowCloseHeight(ctx context.Context, sessionEndHeight int64) (int64, error) {
	params, err := t.sharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
	if err != nil {
		return 0, fmt.Errorf("failed to get shared params: %w", err)
	}
	return sharedtypes.GetProofWindowCloseHeight(params, sessionEndHeight), nil
}

// recordOutcome applies the final outcome to the submission tracker and emits
// the outcome metric.
func (t *ProofInclusionTracker) recordOutcome(
	ctx context.Context,
	check ProofInclusionCheck,
	outcome ProofInclusionOutcome,
	inclusionHeight int64,
	rebroadcasts int,
	newTxHash string,
) {
	proofInclusionOutcomeTotal.WithLabelValues(check.Supplier, check.ServiceID, string(outcome)).Inc()

	if t.submissionTracker == nil {
		return
	}

	// Detach from the poll context: when the outcome is reached via the
	// MaxPollDuration safety cap, ctx is already canceled, and a canceled ctx
	// would make the Redis write silently no-op — losing the outcome we just
	// computed. Persist on a fresh, bounded context instead.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := t.submissionTracker.UpdateProofOnChainOutcome(writeCtx, ProofOnChainUpdate{
		Supplier:        check.Supplier,
		SessionEnd:      check.SessionEndHeight,
		SessionID:       check.SessionID,
		Outcome:         string(outcome),
		InclusionHeight: inclusionHeight,
		NewProofTxHash:  newTxHash,
		Rebroadcasts:    rebroadcasts,
	}); err != nil {
		t.logger.Warn().
			Err(err).
			Str("supplier", check.Supplier).
			Str(logging.FieldSessionID, check.SessionID).
			Msg("proof inclusion: failed to persist outcome to submission tracker")
	}
}
