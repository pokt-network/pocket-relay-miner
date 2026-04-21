package miner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alitto/pond/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/pokt-network/smt"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// TestConfig holds test mode flags read once at initialization.
// These environment variables are only for testing and should not be set in production.
type TestConfig struct {
	// ForceClaimTxError forces claim transaction submission to fail (for testing claim error path)
	ForceClaimTxError bool

	// ForceProofTxError forces proof transaction submission to fail (for testing proof error path)
	ForceProofTxError bool

	// ClaimDelaySeconds delays claim submission by N seconds (for testing claim window timeout)
	ClaimDelaySeconds int

	// ProofDelaySeconds delays proof submission by N seconds (for testing proof window timeout)
	ProofDelaySeconds int
}

var (
	cachedTestConfig     TestConfig
	cachedTestConfigOnce sync.Once
)

// getTestConfig returns the test configuration, reading environment variables once.
func getTestConfig() TestConfig {
	cachedTestConfigOnce.Do(func() {
		cachedTestConfig.ForceClaimTxError = os.Getenv("TEST_FORCE_CLAIM_TX_ERROR") == "true"
		cachedTestConfig.ForceProofTxError = os.Getenv("TEST_FORCE_PROOF_TX_ERROR") == "true"

		if delayStr := os.Getenv("TEST_CLAIM_DELAY_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				cachedTestConfig.ClaimDelaySeconds = delay
			}
		}

		// Support both TEST_DELAY_PROOF_SECONDS and TEST_PROOF_DELAY_SECONDS for backward compatibility
		if delayStr := os.Getenv("TEST_DELAY_PROOF_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				cachedTestConfig.ProofDelaySeconds = delay
			}
		}
		if delayStr := os.Getenv("TEST_PROOF_DELAY_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				cachedTestConfig.ProofDelaySeconds = delay
			}
		}
	})
	return cachedTestConfig
}

// LifecycleCallbackConfig contains configuration for the lifecycle callback.
type LifecycleCallbackConfig struct {
	// SupplierAddress is the supplier this callback is for.
	SupplierAddress string

	// ClaimRetryAttempts is the number of times to retry failed claims.
	ClaimRetryAttempts int

	// ClaimRetryDelay is the delay between retry attempts.
	ClaimRetryDelay time.Duration

	// ProofRetryAttempts is the number of times to retry failed proofs.
	ProofRetryAttempts int

	// ProofRetryDelay is the delay between retry attempts.
	ProofRetryDelay time.Duration

	// DisableClaimBatching disables batching of claim submissions.
	// When true, each session's claim is submitted in a separate transaction.
	DisableClaimBatching bool

	// DisableProofBatching disables batching of proof submissions.
	// When true, each session's proof is submitted in a separate transaction.
	DisableProofBatching bool

	// BlockTimeSeconds is the expected block time used to convert remaining window
	// blocks into a TX broadcast deadline. The TxClient enforces hard min/max bounds
	// regardless of this value. Default: 30.
	BlockTimeSeconds int64

	// DisablePreProofClaimVerification turns off the pre-proof GetClaim guard.
	// When the guard is on (the default — zero value is false), the miner
	// queries the chain for each session's claim before proof submission and
	// drops sessions whose claim is missing on-chain, transitioning them to
	// SessionStateClaimMissing. This prevents the "no claim found for session
	// ID ..." FailedPrecondition retry storm and the wasted gas that follows
	// when a claim tx was accepted to mempool but never included in a block.
	//
	// Leave this false (guard enabled) in production.
	DisablePreProofClaimVerification bool
}

// DefaultLifecycleCallbackConfig returns sensible defaults.
func DefaultLifecycleCallbackConfig() LifecycleCallbackConfig {
	return LifecycleCallbackConfig{
		ClaimRetryAttempts: 3,
		ClaimRetryDelay:    2 * time.Second,
		ProofRetryAttempts: 3,
		ProofRetryDelay:    2 * time.Second,
	}
}

// SMSTManager provides SMST operations for claim/proof generation.
// This interface combines what the lifecycle callback needs from both
// SMSTFlusher and SMSTProver.
type SMSTManager interface {
	// FlushTree flushes the SMST for a session and returns the root hash.
	FlushTree(ctx context.Context, sessionID string) (rootHash []byte, err error)

	// GetTreeRoot returns the root hash for an already-flushed session.
	GetTreeRoot(ctx context.Context, sessionID string) (rootHash []byte, err error)

	// ProveClosest generates a proof for the closest leaf to the given path.
	ProveClosest(ctx context.Context, sessionID string, path []byte) (proofBytes []byte, err error)

	// DeleteTree removes the SMST for a session (cleanup after settlement).
	DeleteTree(ctx context.Context, sessionID string) error
}

// SessionQueryClient queries session information from the blockchain.
type SessionQueryClient interface {
	GetSession(ctx context.Context, appAddr, serviceID string, blockHeight int64) (*sessiontypes.Session, error)
}

// ApplicationQueryClient queries application information from the blockchain.
type ApplicationQueryClient interface {
	GetApplication(ctx context.Context, appAddress string) (*apptypes.Application, error)
}

// ServiceFactorProvider provides service factor configuration.
// This allows the lifecycle callback to check claim amounts against configured ceilings.
type ServiceFactorProvider interface {
	// GetServiceFactor returns the service factor for a service.
	// Returns (factor, true) if configured, (0, false) if not.
	GetServiceFactor(serviceID string) (float64, bool)
}

// StreamDeleter deletes session streams after settlement.
// This stops late relays from being consumed and frees Redis memory.
type StreamDeleter interface {
	// DeleteStream deletes the stream for a session.
	// Safe to call even if the stream doesn't exist.
	DeleteStream(ctx context.Context, sessionID string) error
}

// LifecycleCallback implements SessionLifecycleCallback to handle claim and proof submission.
// It coordinates SMST operations with transaction submission and uses proper timing spread.
type LifecycleCallback struct {
	logger             logging.Logger
	config             LifecycleCallbackConfig
	supplierClient     pocktclient.SupplierClient
	sharedClient       pocktclient.SharedQueryClient
	blockClient        pocktclient.BlockClient
	sessionClient      SessionQueryClient
	smstManager        SMSTManager
	sessionCoordinator *SessionCoordinator

	// proofChecker determines if a proof is required for a claimed session.
	// If nil, proofs are always submitted (legacy behavior).
	proofChecker *ProofRequirementChecker

	// serviceFactorProvider provides service factor configuration for claim ceiling warnings.
	// If nil, no ceiling warnings are logged.
	serviceFactorProvider ServiceFactorProvider

	// appClient queries application data for claim ceiling calculations.
	// If nil, ceiling warnings are skipped.
	appClient ApplicationQueryClient

	// streamDeleter deletes session streams after settlement.
	// If nil, streams are not deleted (will rely on TTL expiration).
	streamDeleter StreamDeleter

	// submissionTracker tracks claim/proof submissions to Redis for debugging.
	// If nil, submissions are not tracked.
	submissionTracker *SubmissionTracker

	// deduplicator is used to clean up session deduplication entries after settlement.
	// If nil, local cache cleanup is skipped (Redis entries still expire via TTL).
	deduplicator Deduplicator

	// proofQueryClient is used by the pre-proof GetClaim guard to verify each
	// session's claim exists on-chain before proof submission. If nil, the
	// guard is skipped (legacy behavior).
	proofQueryClient pocktclient.ProofQueryClient

	// inclusionTracker schedules post-broadcast GetClaim polls that record
	// the real on-chain outcome of each claim tx. If nil, outcome tracking
	// is skipped — submissionTracker records still show broadcast-accepted
	// semantics via ClaimSuccess, but no ClaimOnChainOutcome field will land.
	inclusionTracker *InclusionTracker

	// Per-session locks to prevent concurrent claim/proof operations
	sessionLocks   map[string]*sync.Mutex
	sessionLocksMu sync.Mutex

	// buildPool is used for bounded parallel claim/proof building.
	// If nil, falls back to unbounded goroutines (legacy behavior).
	buildPool pond.Pool
}

// NewLifecycleCallback creates a new lifecycle callback.
// The proofChecker parameter is optional - if nil, proofs are always submitted (legacy behavior).
func NewLifecycleCallback(
	logger logging.Logger,
	supplierClient pocktclient.SupplierClient,
	sharedClient pocktclient.SharedQueryClient,
	blockClient pocktclient.BlockClient,
	sessionClient SessionQueryClient,
	smstManager SMSTManager,
	sessionCoordinator *SessionCoordinator,
	proofChecker *ProofRequirementChecker,
	config LifecycleCallbackConfig,
) *LifecycleCallback {
	if config.ClaimRetryAttempts <= 0 {
		config.ClaimRetryAttempts = 3
	}
	if config.ClaimRetryDelay <= 0 {
		config.ClaimRetryDelay = 2 * time.Second
	}
	if config.ProofRetryAttempts <= 0 {
		config.ProofRetryAttempts = 3
	}
	if config.ProofRetryDelay <= 0 {
		config.ProofRetryDelay = 2 * time.Second
	}

	return &LifecycleCallback{
		logger:             logging.ForSupplierComponent(logger, logging.ComponentLifecycleCallback, config.SupplierAddress),
		config:             config,
		supplierClient:     supplierClient,
		sharedClient:       sharedClient,
		blockClient:        blockClient,
		sessionClient:      sessionClient,
		smstManager:        smstManager,
		sessionCoordinator: sessionCoordinator,
		proofChecker:       proofChecker,
		sessionLocks:       make(map[string]*sync.Mutex),
	}
}

// SetServiceFactorProvider sets the service factor provider for claim ceiling warnings.
// This is optional - if not set, no ceiling warnings are logged.
func (lc *LifecycleCallback) SetServiceFactorProvider(provider ServiceFactorProvider) {
	lc.serviceFactorProvider = provider
}

// SetAppClient sets the application query client for claim ceiling calculations.
// This is optional - if not set, ceiling warnings are skipped.
func (lc *LifecycleCallback) SetAppClient(client ApplicationQueryClient) {
	lc.appClient = client
}

// SetStreamDeleter sets the stream deleter for cleanup after session settlement.
// This is optional - if not set, streams rely on TTL expiration.
func (lc *LifecycleCallback) SetStreamDeleter(deleter StreamDeleter) {
	lc.streamDeleter = deleter
}

// SetSubmissionTracker sets the submission tracker for debugging claim/proof submissions.
// This is optional - if not set, submissions are not tracked.
func (lc *LifecycleCallback) SetSubmissionTracker(tracker *SubmissionTracker) {
	lc.submissionTracker = tracker
}

// SetBuildPool sets the worker pool for bounded parallel claim/proof building.
// If not set, falls back to unbounded goroutines (legacy behavior).
func (lc *LifecycleCallback) SetBuildPool(pool pond.Pool) {
	lc.buildPool = pool
}

// SetDeduplicator sets the deduplicator for cleaning up session deduplication entries.
// If not set, local cache cleanup is skipped (Redis entries still expire via TTL).
func (lc *LifecycleCallback) SetDeduplicator(dedup Deduplicator) {
	lc.deduplicator = dedup
}

// SetProofQueryClient wires the on-chain proof query client used by the
// pre-proof GetClaim guard. If not set (or if the config flag
// EnablePreProofClaimVerification is false), the guard is skipped and the
// proof pipeline behaves as before the WS-A fix.
func (lc *LifecycleCallback) SetProofQueryClient(client pocktclient.ProofQueryClient) {
	lc.proofQueryClient = client
}

// SetInclusionTracker wires the post-broadcast GetClaim inclusion tracker.
// Optional — without it, no ClaimOnChainOutcome field is written and no
// miner_claim_inclusion_outcome_total metric fires.
func (lc *LifecycleCallback) SetInclusionTracker(tracker *InclusionTracker) {
	lc.inclusionTracker = tracker
}

// isClaimNotFoundError returns true when an error from the proof query client
// indicates that a claim does not exist on-chain. The query client wraps the
// gRPC error via fmt.Errorf(..., %w) (see query/query.go), so we both unwrap
// with errors.As (via status.FromError) and fall back to a substring check on
// the rendered message for defensive coverage of older wrappers.
func isClaimNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return true
	}
	return strings.Contains(err.Error(), "not found")
}

// removeSessionLock removes a per-session lock.
func (lc *LifecycleCallback) removeSessionLock(sessionID string) {
	lc.sessionLocksMu.Lock()
	defer lc.sessionLocksMu.Unlock()
	delete(lc.sessionLocks, sessionID)
}

// getClaimReward calculates the expected reward for a claim using the canonical
// poktroll formula (prooftypes.Claim.GetClaimeduPOKT). This uses the SMST root
// hash as the source of truth — not snapshot counters.
func (lc *LifecycleCallback) getClaimReward(
	ctx context.Context,
	claim *prooftypes.Claim,
	serviceID string,
) (sdk.Coin, error) {
	if lc.proofChecker == nil {
		return sdk.Coin{}, fmt.Errorf("proof checker not available")
	}
	difficultyClient := lc.proofChecker.ServiceDifficultyClient()
	if difficultyClient == nil {
		return sdk.Coin{}, fmt.Errorf("difficulty client not available")
	}

	difficulty, err := difficultyClient.GetServiceRelayDifficultyAtHeight(
		ctx, serviceID, claim.SessionHeader.GetSessionStartBlockHeight(),
	)
	if err != nil {
		return sdk.Coin{}, fmt.Errorf("failed to fetch relay mining difficulty: %w", err)
	}

	sharedParams, err := lc.sharedClient.GetParams(ctx)
	if err != nil {
		return sdk.Coin{}, fmt.Errorf("failed to get shared params: %w", err)
	}

	return claim.GetClaimeduPOKT(*sharedParams, difficulty)
}

// claimBuildResult carries the outcome of building a single session's claim
// inside OnSessionsNeedClaim's parallel fan-out. It is lifted out of the
// enclosing method so the collector logic can be unit tested independently
// of the full claim pipeline (block waits, supplier client, etc.).
type claimBuildResult struct {
	index      int
	snapshot   *SessionSnapshot
	claimMsg   *prooftypes.MsgCreateClaim
	rootHash   []byte
	err        error
	skipped    bool // true if session was skipped (empty tree, unprofitable)
	skipReason string
}

// claimBuildCollection is the partitioned result of draining numTasks
// claimBuildResult values from the worker channel. The caller uses `built`
// to populate the batched MsgCreateClaim, iterates `skipped` to run the
// per-reason finalise paths, and surfaces `failed` via warnings + metrics
// for operators (and for the next claim-window retry).
type claimBuildCollection struct {
	built   []claimBuildResult
	skipped []claimBuildResult
	failed  []claimBuildResult
}

// proofBuildResult is the output of a single proof-build goroutine
// inside OnSessionsNeedProof. Declared at package scope so that the
// collector (collectProofBuildResults) can consume a typed channel
// without redeclaring the struct inside the function.
type proofBuildResult struct {
	index    int
	snapshot *SessionSnapshot
	proofMsg *prooftypes.MsgSubmitProof
	err      error
}

// proofBuildCollection is the partitioned result of draining numTasks
// proofBuildResult values from the proof worker channel. Proofs have no
// skip path (unlike claims, which can bail early on "unprofitable" or
// "empty_tree"); every attempt either builds or fails.
type proofBuildCollection struct {
	built  []proofBuildResult
	failed []proofBuildResult
}

// collectProofBuildResults drains exactly numTasks values from resultsCh
// and partitions them into (built, failed). A prior implementation bailed
// out on the first result.err != nil, which had two serious consequences:
//
//  1. The remaining goroutines were still attempting to write into a
//     buffered channel of size numTasks that would never be read again,
//     leaking one goroutine per remaining task until process exit.
//  2. Proof-builds that had already completed successfully were silently
//     abandoned — their sessions stayed in SessionStateClaimed forever,
//     the proof window closed, and the supplier lost the claim even
//     though the proof was ready to submit.
//
// Like collectClaimBuildResults, this function never returns an error:
// it drains every task and lets the caller decide what to do with each
// bucket. resultsCh MUST deliver exactly numTasks values.
func collectProofBuildResults(numTasks int, resultsCh <-chan proofBuildResult) proofBuildCollection {
	coll := proofBuildCollection{}
	if numTasks <= 0 {
		return coll
	}
	coll.built = make([]proofBuildResult, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		r := <-resultsCh
		if r.err != nil {
			coll.failed = append(coll.failed, r)
			continue
		}
		coll.built = append(coll.built, r)
	}
	return coll
}

// collectClaimBuildResults drains exactly numTasks values from results and
// partitions them into (built, skipped, failed).
//
// A prior implementation bailed out on the first result.err != nil, which
// silently abandoned every session whose buildFunc had already completed
// successfully in the same batch — including sessions whose SMST had
// already been sealed (claimedRoot set) and so could not be re-built on a
// subsequent claim-window retry. That produced claims that never made it
// on-chain even though the supplier had fully mined the relays.
//
// This function never returns an error: it collects all results and lets
// the caller decide per-bucket what to do. Failures are surfaced to the
// caller verbatim (full err chain preserved) so the caller can log and
// meter them without losing context.
//
// The resultsCh MUST deliver exactly numTasks values; callers that
// cancel early must drain it themselves to avoid leaking goroutines.
func collectClaimBuildResults(numTasks int, resultsCh <-chan claimBuildResult) claimBuildCollection {
	coll := claimBuildCollection{}
	if numTasks <= 0 {
		return coll
	}
	coll.built = make([]claimBuildResult, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		r := <-resultsCh
		switch {
		case r.err != nil:
			coll.failed = append(coll.failed, r)
		case r.skipped:
			coll.skipped = append(coll.skipped, r)
		default:
			coll.built = append(coll.built, r)
		}
	}
	return coll
}

// OnSessionActive is called when a new session starts.
// For HA miner, sessions are created on-demand when relays arrive, so this is mostly informational.
func (lc *LifecycleCallback) OnSessionActive(_ context.Context, snapshot *SessionSnapshot) error {
	lc.logger.Debug().
		Str(logging.FieldSessionID, snapshot.SessionID).
		Int64(logging.FieldSessionEndHeight, snapshot.SessionEndHeight).
		Str(logging.FieldServiceID, snapshot.ServiceID).
		Msg("session active")

	return nil
}

// OnSessionsNeedClaim is called when sessions need claims submitted (batched).
// It waits for the proper timing spread, flushes SMSTs, and submits all claims in a single transaction.
func (lc *LifecycleCallback) OnSessionsNeedClaim(ctx context.Context, snapshots []*SessionSnapshot) (rootHashes [][]byte, err error) {
	if len(snapshots) == 0 {
		return nil, nil
	}

	// All sessions for a single supplier, so we can batch them
	firstSnapshot := snapshots[0]
	logger := lc.logger.With().
		Str(logging.FieldSupplier, firstSnapshot.SupplierOperatorAddress).
		Int("batch_size", len(snapshots)).
		Logger()

	logger.Debug().Msg("batched sessions need claims - starting claim process")

	// Get shared params
	sharedParams, err := lc.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	// Group sessions by session end height (they might have different claim windows)
	// WORKAROUND: If batching is disabled, create one group per session to avoid
	// cross-contamination where one invalid claim causes the entire batch to fail.
	sessionsByEndHeight := make(map[int64][]*SessionSnapshot)
	if lc.config.DisableClaimBatching {
		// No batching - each session in its own "group" using unique key
		logger.Info().
			Int("total_sessions", len(snapshots)).
			Bool("batching_disabled", true).
			Msg("CLAIM_BATCHING_DISABLED: submitting each session in separate transaction (workaround for difficulty validation)")
		for i, snapshot := range snapshots {
			// Use negative index as key to avoid conflicts with real end heights
			sessionsByEndHeight[int64(-i-1)] = []*SessionSnapshot{snapshot}
		}
	} else {
		// Normal batching - group by session end height
		logger.Info().
			Int("total_sessions", len(snapshots)).
			Bool("batching_enabled", true).
			Msg("claim batching enabled - grouping sessions by end height")
		for _, snapshot := range snapshots {
			sessionsByEndHeight[snapshot.SessionEndHeight] = append(sessionsByEndHeight[snapshot.SessionEndHeight], snapshot)
		}
	}

	logger.Info().
		Int("total_sessions", len(snapshots)).
		Int("num_batches", len(sessionsByEndHeight)).
		Bool("batching_disabled", lc.config.DisableClaimBatching).
		Msg("claim batching strategy applied")

	// Process each group (same claim window) separately
	allRootHashes := make([][]byte, len(snapshots))
	sessionIndex := 0

	for _, groupSnapshots := range sessionsByEndHeight {
		// Get the actual session end height from the first snapshot in the group
		// (all snapshots in a group have the same end height)
		sessionEndHeight := groupSnapshots[0].SessionEndHeight

		// Wait for claim window to open and get the block hash for timing spread
		claimWindowOpenHeight := sharedtypes.GetClaimWindowOpenHeight(sharedParams, sessionEndHeight)
		claimWindowCloseHeight := sharedtypes.GetClaimWindowCloseHeight(sharedParams, sessionEndHeight)
		currentHeight := lc.blockClient.LastBlock(ctx).Height()

		// Build session IDs list for logging (truncate if too many)
		sessionIDs := make([]string, 0, len(groupSnapshots))
		for _, s := range groupSnapshots {
			if len(sessionIDs) < 5 { // Show first 5 session IDs
				sessionIDs = append(sessionIDs, s.SessionID[:16]+"...")
			}
		}

		logger.Debug().
			Int64("claim_window_open_height", claimWindowOpenHeight).
			Int64("claim_window_close_height", claimWindowCloseHeight).
			Int64("session_end_height", sessionEndHeight).
			Int64("current_height", currentHeight).
			Int("group_size", len(groupSnapshots)).
			Str("first_service_id", groupSnapshots[0].ServiceID).
			Strs("session_ids", sessionIDs).
			Msg("waiting for claim window to open")

		if _, blockErr := lc.waitForBlock(ctx, claimWindowOpenHeight); blockErr != nil {
			return nil, fmt.Errorf("failed to wait for claim window open: %w", blockErr)
		}

		// NOTE: Timing spread DISABLED - submit claims immediately when window opens
		// The protocol's GetEarliestSupplierClaimCommitHeight() spreads suppliers across the window,
		// but this can cause claims to be submitted too close to window close, resulting in failures.
		// We now submit immediately when the claim window opens to maximize success rate.
		earliestClaimHeight := claimWindowOpenHeight // Use window open, not spread height

		logger.Info().
			Int64("claim_window_open", claimWindowOpenHeight).
			Int64("session_end_height", sessionEndHeight).
			Msg("claim window open - submitting immediately (timing spread disabled)")

		// CRITICAL: Verify claim window is still open AND we have enough time to build+submit
		// AGGRESSIVE MODE: Push claims until the very last block of the window
		claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(sharedParams, sessionEndHeight)
		currentBlock := lc.blockClient.LastBlock(ctx)
		blocksRemaining := claimWindowClose - currentBlock.Height()

		// AGGRESSIVE: No buffer - use every available block in the window
		const minBlocksRequired = 2

		if blocksRemaining <= minBlocksRequired {
			logger.Error().
				Int64("current_height", currentBlock.Height()).
				Int64("claim_window_close", claimWindowClose).
				Int64("blocks_remaining", blocksRemaining).
				Int64("min_blocks_required", minBlocksRequired).
				Int64("session_end_height", sessionEndHeight).
				Int("group_size", len(groupSnapshots)).
				Msg("insufficient time remaining to build and submit claims - aborting (sessions will be marked as failed)")

			// Mark all sessions in this group as failed (metrics + Redis state for HA)
			for _, snapshot := range groupSnapshots {
				RecordClaimWindowClosed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

				// CRITICAL: Update session state in Redis immediately for HA compatibility
				// If this miner crashes, the new leader must know this session already failed
				if lc.sessionCoordinator != nil {
					if err := lc.sessionCoordinator.OnClaimWindowClosed(ctx, snapshot.SessionID); err != nil {
						logger.Warn().
							Err(err).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
							Str(logging.FieldServiceID, snapshot.ServiceID).
							Msg("failed to mark session as claim_window_closed in Redis")
					}
				}
			}

			return nil, fmt.Errorf("insufficient time to build claims: %d blocks remaining, %d required (window closes at %d, current: %d)",
				blocksRemaining, minBlocksRequired, claimWindowClose, currentBlock.Height())
		}

		logger.Debug().
			Int("group_size", len(groupSnapshots)).
			Int64("claim_window_close", claimWindowClose).
			Int64("current_height", currentBlock.Height()).
			Int64("blocks_remaining", claimWindowClose-currentBlock.Height()).
			Msg("claim window timing reached - flushing SMSTs and submitting batched claims")

		// DEBUG/TEST: Intentionally delay claim submission to test claim window timeout tracking
		// Set environment variable TEST_CLAIM_DELAY_SECONDS to enable (e.g., "60" for 60 seconds)
		// This simulates scenarios where SMST flushing takes too long or network delays occur
		if testCfg := getTestConfig(); testCfg.ClaimDelaySeconds > 0 {
			delayDuration := time.Duration(testCfg.ClaimDelaySeconds) * time.Second
			logger.Warn().
				Int("delay_seconds", testCfg.ClaimDelaySeconds).
				Int64("claim_window_close", claimWindowClose).
				Int64("current_height", currentBlock.Height()).
				Msg("TEST MODE: Intentionally delaying claim submission to test window timeout tracking")
			time.Sleep(delayDuration)

			// Log current state after delay
			postDelayHeight := lc.blockClient.LastBlock(ctx).Height()
			logger.Warn().
				Int64("height_after_delay", postDelayHeight).
				Int64("claim_window_close", claimWindowClose).
				Bool("window_still_open", postDelayHeight < claimWindowClose).
				Msg("TEST MODE: Delay complete, continuing with claim submission")
		}

		// Pre-filter: skip already-claimed sessions (dedup).
		// This is the only check that uses snapshot data as a gate.
		var candidateSnapshots []*SessionSnapshot
		for _, snapshot := range groupSnapshots {
			if snapshot.ClaimTxHash != "" {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
					Str("existing_claim_tx_hash", snapshot.ClaimTxHash).
					Msg("skipping claim - already submitted for this session (deduplication)")
				continue
			}
			candidateSnapshots = append(candidateSnapshots, snapshot)
		}

		// PARALLEL CLAIM BUILDING: flush SMST first (source of truth), then
		// use the root hash for economic viability — matching poktroll's
		// canonical flow (flush → GetClaimeduPOKT → submit).
		//
		// Snapshot counters (relay_count, total_compute_units) are NOT used
		// for claim decisions because they can race with concurrent updates.
		// The SMST root hash encodes the real count and sum atomically.
		results := make(chan claimBuildResult, len(candidateSnapshots))
		numTasks := len(candidateSnapshots)

		// Resolve fee cost once for the entire batch (shared across all sessions)
		var claimAndProofCostUpokt uint64
		if haClient, ok := lc.supplierClient.(*tx.HASupplierClient); ok {
			claimAndProofCostUpokt = haClient.GetEstimatedFeeUpokt(ctx)
		}

		for i, snapshot := range candidateSnapshots {
			index := i
			snap := snapshot

			buildFunc := func() {
				result := claimBuildResult{
					index:    index,
					snapshot: snap,
				}

				// Panic guard: the collector downstream MUST receive exactly
				// numTasks values or it leaks a goroutine per missing slot.
				// If any step below panics (nil snapshot field, SMST boundary
				// miss, proto zero-value) the deferred recover converts the
				// panic into a failed result so the collector drains cleanly.
				// Covers both the pool path (pond also absorbs panics, but
				// would not write our channel) and the fallback go path.
				defer func() {
					if r := recover(); r != nil {
						logging.PanicRecoveriesTotal.WithLabelValues("claim_build").Inc()
						lc.logger.Error().
							Str(logging.FieldSessionID, snap.SessionID).
							Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
							Str("panic_value", fmt.Sprintf("%v", r)).
							Str("stack_trace", string(debug.Stack())).
							Msg("PANIC RECOVERED in claim build goroutine")
						results <- claimBuildResult{
							index:    index,
							snapshot: snap,
							err:      fmt.Errorf("claim build panic for session %s: %v", snap.SessionID, r),
						}
					}
				}()

				// Phase 1: Flush the SMST to get the root hash (source of truth).
				// The root hash encodes count (relays) and sum (compute units)
				// atomically — no race with concurrent relay processing.
				rootHash, flushErr := lc.smstManager.FlushTree(ctx, snap.SessionID)
				if flushErr != nil {
					result.err = fmt.Errorf("failed to flush SMST for session %s: %w", snap.SessionID, flushErr)
					results <- result
					return
				}
				// Belt-and-suspenders: smt.MerkleSumRoot.Count()/Sum() slice into
				// the trailing 16 bytes without bounds checking. Refuse to
				// build a claim from a malformed root rather than panicking.
				if len(rootHash) != SMSTRootLen {
					result.err = fmt.Errorf("session %s: flushed root has invalid length %d, expected %d", snap.SessionID, len(rootHash), SMSTRootLen)
					results <- result
					return
				}
				result.rootHash = rootHash

				// Phase 2: Extract count and sum from the SMST root hash.
				// These are the authoritative values — not the snapshot counters.
				smstRoot := smt.MerkleSumRoot(rootHash)
				smstCount, countErr := smstRoot.Count()
				if countErr != nil {
					result.err = fmt.Errorf("failed to read count from SMST root for session %s: %w", snap.SessionID, countErr)
					results <- result
					return
				}
				smstSum, sumErr := smstRoot.Sum()
				if sumErr != nil {
					result.err = fmt.Errorf("failed to read sum from SMST root for session %s: %w", snap.SessionID, sumErr)
					results <- result
					return
				}

				// Phase 3: Check if tree is empty (no relays mined).
				if smstCount == 0 || smstSum == 0 {
					logger.Warn().
						Str(logging.FieldSessionID, snap.SessionID).
						Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
						Uint64("smst_count", smstCount).
						Uint64("smst_sum", smstSum).
						Int64("snapshot_relay_count", snap.RelayCount).
						Msg("skipping claim - SMST tree is empty (0 mined relays)")
					result.skipped = true
					result.skipReason = "empty_tree"
					results <- result
					return
				}

				// Phase 4: Economic viability using the SMST root hash.
				// Build a temporary Claim (like poktroll does) to call GetClaimeduPOKT.
				if claimAndProofCostUpokt > 0 {
					sessionHeader, headerErr := lc.buildSessionHeader(ctx, snap)
					if headerErr != nil {
						result.err = fmt.Errorf("failed to build session header for %s: %w", snap.SessionID, headerErr)
						results <- result
						return
					}

					tempClaim := prooftypes.Claim{
						SupplierOperatorAddress: snap.SupplierOperatorAddress,
						SessionHeader:           sessionHeader,
						RootHash:                rootHash,
					}

					rewardCoin, rewardErr := lc.getClaimReward(ctx, &tempClaim, snap.ServiceID)
					if rewardErr != nil {
						// Fail open on reward calculation errors — don't skip a
						// potentially valid claim due to a transient query failure.
						logger.Debug().Err(rewardErr).
							Str(logging.FieldSessionID, snap.SessionID).
							Msg("economic viability: failed to calculate reward, allowing claim")
					} else if rewardCoin.IsNil() || uint64(rewardCoin.Amount.Int64()) <= claimAndProofCostUpokt {
						logger.Warn().
							Str(logging.FieldSessionID, snap.SessionID).
							Str(logging.FieldServiceID, snap.ServiceID).
							Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
							Uint64("claim_and_proof_cost_upokt", claimAndProofCostUpokt).
							Str("expected_reward", rewardCoin.String()).
							Uint64("smst_compute_units", smstSum).
							Uint64("smst_relay_count", smstCount).
							Msg("SKIP UNPROFITABLE: expected reward < claim+proof cost (calculated from SMST root hash)")

						result.skipped = true
						result.skipReason = "unprofitable"
						results <- result
						return
					}

					// Build claim message (session header already built above)
					result.claimMsg = &prooftypes.MsgCreateClaim{
						SupplierOperatorAddress: snap.SupplierOperatorAddress,
						SessionHeader:           sessionHeader,
						RootHash:                rootHash,
					}

					results <- result
					return
				}

				// No fee estimation available — build claim without viability check
				sessionHeader, headerErr := lc.buildSessionHeader(ctx, snap)
				if headerErr != nil {
					result.err = fmt.Errorf("failed to build session header for %s: %w", snap.SessionID, headerErr)
					results <- result
					return
				}

				result.claimMsg = &prooftypes.MsgCreateClaim{
					SupplierOperatorAddress: snap.SupplierOperatorAddress,
					SessionHeader:           sessionHeader,
					RootHash:                rootHash,
				}

				results <- result
			}

			if lc.buildPool != nil {
				lc.buildPool.Submit(buildFunc)
			} else {
				go buildFunc()
			}
		}

		// Collect all results (blocking until all tasks complete). We
		// deliberately collect every result even when some buildFuncs
		// fail: a single-session failure (e.g. a transient
		// buildSessionHeader query error) must not abandon the other
		// sessions in the batch, because their SMSTs have already been
		// sealed (claimedRoot set) and cannot be re-built on a future
		// retry — leaving them in-limbo silently drops claims that were
		// otherwise valid and properly mined.
		partitioned := collectClaimBuildResults(numTasks, results)

		// Per-reason finalise paths for skipped sessions.
		for _, r := range partitioned.skipped {
			snap := r.snapshot
			switch r.skipReason {
			case "unprofitable":
				RecordClaimSkipped(snap.SupplierOperatorAddress, snap.ServiceID, "unprofitable")
				if skipErr := lc.OnClaimSkipped(ctx, snap); skipErr != nil {
					logger.Warn().Err(skipErr).
						Str(logging.FieldSessionID, snap.SessionID).
						Msg("failed to finalise claim_skipped transition")
				}
			case "empty_tree":
				// Session had no mined relays — not a failure, just nothing to claim
			}
		}

		// Failed per-session builds are surfaced as warnings + metrics
		// for operator visibility and next-window retry inspection.
		// The batch continues with whatever successfully built.
		for _, r := range partitioned.failed {
			snap := r.snapshot
			sessionID := ""
			supplier := ""
			serviceID := ""
			if snap != nil {
				sessionID = snap.SessionID
				supplier = snap.SupplierOperatorAddress
				serviceID = snap.ServiceID
			}
			RecordClaimSkipped(supplier, serviceID, "build_failed")
			logger.Warn().
				Err(r.err).
				Str(logging.FieldSessionID, sessionID).
				Str(logging.FieldSupplier, supplier).
				Str(logging.FieldServiceID, serviceID).
				Msg("session claim build failed - dropping from batch, other sessions continue")
		}

		// Valid claims — collect for submission.
		claimMsgs := make([]*prooftypes.MsgCreateClaim, 0, len(partitioned.built))
		groupRootHashes := make([][]byte, 0, len(partitioned.built))
		validSnapshots := make([]*SessionSnapshot, 0, len(partitioned.built))
		for _, r := range partitioned.built {
			SetClaimScheduledHeight(
				r.snapshot.SupplierOperatorAddress,
				r.snapshot.ServiceID,
				r.snapshot.SessionID,
				float64(earliestClaimHeight),
			)
			claimMsgs = append(claimMsgs, r.claimMsg)
			groupRootHashes = append(groupRootHashes, r.rootHash)
			validSnapshots = append(validSnapshots, r.snapshot)
		}

		// Convert to interface types for variadic call
		interfaceClaimMsgs := make([]pocktclient.MsgCreateClaim, len(claimMsgs))
		for i, msg := range claimMsgs {
			interfaceClaimMsgs[i] = msg
		}

		// CRITICAL: Re-check window is still open RIGHT before submission
		// Building claims (SMST flush, headers) takes time - blocks may have advanced!
		currentBlock = lc.blockClient.LastBlock(ctx)
		if currentBlock.Height() >= claimWindowClose {
			logger.Error().
				Int64("current_height", currentBlock.Height()).
				Int64("claim_window_close", claimWindowClose).
				Int64("session_end_height", sessionEndHeight).
				Int("batch_size", len(claimMsgs)).
				Msg("claim window closed while building claims - cannot submit")

			// Mark all sessions in this batch as failed (metrics + Redis state for HA)
			for _, snapshot := range groupSnapshots {
				RecordClaimWindowClosed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

				// CRITICAL: Update session state in Redis immediately for HA compatibility
				if lc.sessionCoordinator != nil {
					if err := lc.sessionCoordinator.OnClaimWindowClosed(ctx, snapshot.SessionID); err != nil {
						logger.Warn().
							Err(err).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
							Str(logging.FieldServiceID, snapshot.ServiceID).
							Msg("failed to mark session as claim_window_closed in Redis")
					}
				}
			}

			return nil, fmt.Errorf("claim window closed while building claims at height %d (current: %d)", claimWindowClose, currentBlock.Height())
		}

		claimBlocksLeft := claimWindowClose - currentBlock.Height()
		claimBlockTimeSec := lc.config.BlockTimeSeconds
		if claimBlockTimeSec <= 0 {
			claimBlockTimeSec = 30
		}
		rawClaimTimeout := time.Duration(claimBlocksLeft) * time.Duration(claimBlockTimeSec) * time.Second
		claimCtx := tx.WithTxWindowTimeout(ctx, rawClaimTimeout)

		logger.Info().
			Int64("current_height", currentBlock.Height()).
			Int64("claim_window_close", claimWindowClose).
			Int64("blocks_remaining", claimBlocksLeft).
			Dur("tx_deadline", rawClaimTimeout).
			Int("batch_size", len(claimMsgs)).
			Msg("submitting claims")

		// Submit all claims in a single transaction with retries
		var lastErr error
		var claimTxHash string
		for attempt := 1; attempt <= lc.config.ClaimRetryAttempts; attempt++ {
			submitErr := lc.supplierClient.CreateClaims(claimCtx, claimWindowClose, interfaceClaimMsgs...)
			if submitErr != nil {
				lastErr = submitErr

				// Check if error is due to claim window being closed (permanent failure - don't retry)
				errorMsg := submitErr.Error()
				if strings.Contains(errorMsg, "claim window") || strings.Contains(errorMsg, "claim_window") {
					logger.Error().
						Err(submitErr).
						Int64("current_height", lc.blockClient.LastBlock(ctx).Height()).
						Int64("claim_window_close", claimWindowClose).
						Int("batch_size", len(claimMsgs)).
						Msg("claim window closed during submission - permanent failure, not retrying")

					// Mark all sessions in this batch as failed (metrics + Redis state for HA)
					for _, snapshot := range groupSnapshots {
						RecordClaimWindowClosed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

						// CRITICAL: Update session state in Redis immediately for HA compatibility
						if lc.sessionCoordinator != nil {
							if err := lc.sessionCoordinator.OnClaimWindowClosed(ctx, snapshot.SessionID); err != nil {
								logger.Warn().
									Err(err).
									Str(logging.FieldSessionID, snapshot.SessionID).
									Msg("failed to mark session as claim_window_closed in Redis")
							}
						}
					}

					break // Don't retry - this is a permanent failure
				}

				logger.Warn().
					Err(submitErr).
					Int(logging.FieldAttempt, attempt).
					Int(logging.FieldMaxRetry, lc.config.ClaimRetryAttempts).
					Int("batch_size", len(claimMsgs)).
					Msg("batched claim submission failed, retrying")

				if attempt < lc.config.ClaimRetryAttempts {
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(lc.config.ClaimRetryDelay):
						continue
					}
				}
			} else {
				// SUCCESS: Claim TX broadcast accepted to mempool
				// Retrieve TX hash from HA client (stored immediately after broadcast)
				if haClient, ok := lc.supplierClient.(*tx.HASupplierClient); ok {
					claimTxHash = haClient.GetLastClaimTxHash()
				}

				currentBlock := lc.blockClient.LastBlock(ctx)
				blocksAfterWindowOpen := float64(currentBlock.Height() - claimWindowOpenHeight)

				// CRITICAL: Save TX hash to Redis IMMEDIATELY (1 line after broadcast)
				// This prevents duplicate submissions if we crash after TX broadcast
				for i, snapshot := range validSnapshots {
					// Update snapshot manager with TX hash for deduplication
					if lc.sessionCoordinator != nil {
						if updateErr := lc.sessionCoordinator.OnSessionClaimed(ctx, snapshot.SessionID, groupRootHashes[i], claimTxHash); updateErr != nil {
							logger.Warn().
								Err(updateErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Str("claim_tx_hash", claimTxHash).
								Msg("failed to update snapshot after claim")
						}
					}

					// Update in-memory snapshot with claimed root hash (for proof requirement check below)
					validSnapshots[i].ClaimedRootHash = groupRootHashes[i]
				}

				// NOTE: Proof requirement check moved to OnSessionsNeedProof.
				// Previously we blocked here waiting for the proof requirement seed block
				// (proofWindowOpen - 1), which is ~19 blocks in the future after claim submission.
				// This caused sequential claim groups to timeout while waiting.
				// Now sessions stay in 'claimed' state until proof window opens, where
				// OnSessionsNeedProof properly checks if proof is required.

				// Record metrics for all sessions in the batch
				for i, snapshot := range validSnapshots {
					RecordClaimSubmitted(snapshot.SupplierOperatorAddress, snapshot.ServiceID)
					RecordClaimSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
					RecordRevenueClaimed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

					// Copy root hash to result (maintain order)
					allRootHashes[sessionIndex] = groupRootHashes[i]
					sessionIndex++
				}

				// Track claim submissions to Redis for debugging
				if lc.submissionTracker != nil {
					for i, snapshot := range validSnapshots {
						claimHash := hex.EncodeToString(groupRootHashes[i])
						if trackErr := lc.submissionTracker.TrackClaimSubmission(
							ctx,
							snapshot.SupplierOperatorAddress,
							snapshot.ServiceID,
							snapshot.ApplicationAddress,
							snapshot.SessionID,
							snapshot.SessionStartHeight,
							snapshot.SessionEndHeight,
							claimHash,
							claimTxHash,
							true, // success
							"",   // no error
							earliestClaimHeight,
							currentBlock.Height(),
							snapshot.RelayCount,
							int64(snapshot.TotalComputeUnits),
							false, // proof_required unknown at claim time
							"",    // proof_requirement_seed unknown at claim time
						); trackErr != nil {
							logger.Warn().
								Err(trackErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to track claim submission")
						}
					}
				}

				// Schedule post-broadcast claim inclusion polling. Each session
				// in the batch gets its own GetClaim poll; the tracker marks
				// the submission record with on_chain_found / on_chain_missing
				// / poll_error once the claim window closes. No tx indexer
				// required — GetClaim reads x/proof module state.
				if lc.inclusionTracker != nil && claimTxHash != "" {
					for _, snapshot := range validSnapshots {
						lc.inclusionTracker.ScheduleClaimCheck(ClaimInclusionCheck{
							Supplier:         snapshot.SupplierOperatorAddress,
							ServiceID:        snapshot.ServiceID,
							SessionID:        snapshot.SessionID,
							SessionEndHeight: snapshot.SessionEndHeight,
							ClaimTxHash:      claimTxHash,
						})
					}
				}

				logger.Info().
					Int("batch_size", len(claimMsgs)).
					Str("claim_tx_hash", claimTxHash).
					Int64("blocks_after_window", int64(blocksAfterWindowOpen)).
					Msg("batched claims submitted successfully")

				break // Success, exit retry loop
			}
		}

		if lastErr != nil {
			// Mark all sessions as failed due to claim TX error (after exhausting retries)
			for _, snapshot := range groupSnapshots {
				RecordClaimTxError(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

				// CRITICAL: Update session state in Redis immediately for HA compatibility
				if lc.sessionCoordinator != nil {
					if err := lc.sessionCoordinator.OnClaimTxError(ctx, snapshot.SessionID); err != nil {
						logger.Warn().
							Err(err).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to mark session as claim_tx_error in Redis")
					}
				}
			}

			// Track failed claim submissions to Redis for debugging
			if lc.submissionTracker != nil {
				for i, snapshot := range validSnapshots {
					claimHash := hex.EncodeToString(groupRootHashes[i])
					if trackErr := lc.submissionTracker.TrackClaimSubmission(
						ctx,
						snapshot.SupplierOperatorAddress,
						snapshot.ServiceID,
						snapshot.ApplicationAddress,
						snapshot.SessionID,
						snapshot.SessionStartHeight,
						snapshot.SessionEndHeight,
						claimHash,
						"",    // no TX hash on failure
						false, // failed
						lastErr.Error(),
						earliestClaimHeight,
						lc.blockClient.LastBlock(ctx).Height(),
						snapshot.RelayCount,
						int64(snapshot.TotalComputeUnits),
						false, // proof_required unknown at claim time
						"",    // proof_requirement_seed unknown at claim time
					); trackErr != nil {
						logger.Warn().
							Err(trackErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to track failed claim submission")
					}
				}
			}

			return nil, fmt.Errorf("batched claim submission failed after %d attempts: %w", lc.config.ClaimRetryAttempts, lastErr)
		}
	}

	return allRootHashes, nil
}

// OnSessionsNeedProof is called when sessions need proofs submitted (batched).
// It waits for the proper timing spread, generates proofs, and submits all proofs in a single transaction.
func (lc *LifecycleCallback) OnSessionsNeedProof(ctx context.Context, snapshots []*SessionSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}

	// All sessions for a single supplier, so we can batch them
	firstSnapshot := snapshots[0]
	logger := lc.logger.With().
		Str(logging.FieldSupplier, firstSnapshot.SupplierOperatorAddress).
		Int("batch_size", len(snapshots)).
		Logger()

	// TEST MODE: Delay proof submission to simulate slow proof generation or test window timeout
	if testCfg := getTestConfig(); testCfg.ProofDelaySeconds > 0 {
		logger.Warn().
			Int("delay_seconds", testCfg.ProofDelaySeconds).
			Msg("TEST MODE: delaying proof submission")
		time.Sleep(time.Duration(testCfg.ProofDelaySeconds) * time.Second)
		logger.Warn().
			Int("delay_seconds", testCfg.ProofDelaySeconds).
			Msg("TEST MODE: proof delay complete, continuing with submission")
	}

	logger.Debug().Msg("batched sessions need proofs - starting proof process")

	// Get shared params
	sharedParams, err := lc.sharedClient.GetParams(ctx)
	if err != nil {
		return fmt.Errorf("failed to get shared params: %w", err)
	}

	// Group sessions by session end height (they might have different proof windows)
	// WORKAROUND: If batching is disabled, create one group per session to avoid
	// cross-contamination where one invalid proof (e.g., difficulty validation failure)
	// causes the entire batch to fail.
	sessionsByEndHeight := make(map[int64][]*SessionSnapshot)
	if lc.config.DisableProofBatching {
		// No batching - each session in its own "group" using unique key
		logger.Info().
			Int("total_sessions", len(snapshots)).
			Bool("batching_disabled", true).
			Msg("PROOF_BATCHING_DISABLED: submitting each session in separate transaction (workaround for difficulty validation)")
		for i, snapshot := range snapshots {
			// Use negative index as key to avoid conflicts with real end heights
			sessionsByEndHeight[int64(-i-1)] = []*SessionSnapshot{snapshot}
		}
	} else {
		// Normal batching - group by session end height
		logger.Info().
			Int("total_sessions", len(snapshots)).
			Bool("batching_enabled", true).
			Msg("proof batching enabled - grouping sessions by end height")
		for _, snapshot := range snapshots {
			sessionsByEndHeight[snapshot.SessionEndHeight] = append(sessionsByEndHeight[snapshot.SessionEndHeight], snapshot)
		}
	}

	logger.Info().
		Int("total_sessions", len(snapshots)).
		Int("num_batches", len(sessionsByEndHeight)).
		Bool("batching_disabled", lc.config.DisableProofBatching).
		Msg("proof batching strategy applied")

	// Process each group (same proof window) separately
	for _, groupSnapshots := range sessionsByEndHeight {
		// Get the actual session end height from the first snapshot in the group
		// (all snapshots in a group have the same end height)
		sessionEndHeight := groupSnapshots[0].SessionEndHeight

		// Wait for proof window to open
		proofWindowOpenHeight := sharedtypes.GetProofWindowOpenHeight(sharedParams, sessionEndHeight)
		proofWindowCloseHeight := sharedtypes.GetProofWindowCloseHeight(sharedParams, sessionEndHeight)
		currentHeight := lc.blockClient.LastBlock(ctx).Height()

		// Build session IDs list for logging (truncate if too many)
		proofSessionIDs := make([]string, 0, len(groupSnapshots))
		for _, s := range groupSnapshots {
			if len(proofSessionIDs) < 5 { // Show first 5 session IDs
				proofSessionIDs = append(proofSessionIDs, s.SessionID[:16]+"...")
			}
		}

		logger.Debug().
			Int64("proof_window_open_height", proofWindowOpenHeight).
			Int64("proof_window_close_height", proofWindowCloseHeight).
			Int64("session_end_height", sessionEndHeight).
			Int64("current_height", currentHeight).
			Int("group_size", len(groupSnapshots)).
			Str("first_service_id", groupSnapshots[0].ServiceID).
			Strs("session_ids", proofSessionIDs).
			Msg("waiting for proof window to open")

		// Wait for proof window to open (we'll use the seed block, not this one)
		_, blockErr := lc.waitForBlock(ctx, proofWindowOpenHeight)
		if blockErr != nil {
			return fmt.Errorf("failed to wait for proof window open: %w", blockErr)
		}

		// Calculate proof requirement seed block height
		// CRITICAL: This MUST match the validator's logic in x/proof/keeper/msg_server_submit_proof.go:328
		// The validator uses BlockHash(earliestSupplierProofCommitHeight - 1) as the seed.
		// Since earliestSupplierProofCommitHeight = proofWindowOpenHeight (distribution disabled in poktroll),
		// the seed block height = proofWindowOpenHeight - 1
		proofRequirementSeedHeight := proofWindowOpenHeight - 1

		// Wait for the seed block to be available
		proofRequirementSeedBlock, seedErr := lc.waitForBlock(ctx, proofRequirementSeedHeight)
		if seedErr != nil {
			logger.Warn().
				Err(seedErr).
				Int64("seed_height", proofRequirementSeedHeight).
				Int64("proof_window_open_height", proofWindowOpenHeight).
				Msg("failed to wait for proof requirement seed block")
			return fmt.Errorf("failed to wait for proof requirement seed block: %w", seedErr)
		}

		logger.Debug().
			Int64("proof_requirement_seed_height", proofRequirementSeedHeight).
			Str("proof_requirement_seed_hash", fmt.Sprintf("%x", proofRequirementSeedBlock.Hash())).
			Msg("obtained proof requirement seed block hash")

		// Filter sessions based on proof requirement (probabilistic proof selection)
		var sessionsNeedingProof []*SessionSnapshot
		for _, snapshot := range groupSnapshots {
			// CRITICAL: Deduplication check - never submit the same proof twice
			if snapshot.ProofTxHash != "" {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str("existing_proof_tx_hash", snapshot.ProofTxHash).
					Msg("skipping proof - already submitted for this session (deduplication)")
				continue // Skip this session
			}

			// Pre-proof GetClaim guard (WS-A).
			//
			// The miner records claim_success=true on CheckTx/mempool acceptance, so a
			// session in SessionStateClaimed does NOT guarantee the claim is on-chain.
			// Submitting a proof when the claim is missing triggers an unrecoverable
			// FailedPrecondition ("no claim found for session ID") and burns gas
			// across three retries. Query GetClaim first; if NotFound, drop the session
			// from this batch and mark it terminal. Fail-open on other RPC errors so a
			// flapping chain node does not lose valid proofs.
			if !lc.config.DisablePreProofClaimVerification && lc.proofQueryClient != nil {
				_, claimErr := lc.proofQueryClient.GetClaim(ctx, snapshot.SupplierOperatorAddress, snapshot.SessionID)
				if claimErr != nil && isClaimNotFoundError(claimErr) {
					logger.Warn().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
						Str("claim_tx_hash", snapshot.ClaimTxHash).
						Msg("pre-proof guard: no on-chain claim found for session — skipping proof, marking session claim_missing")
					RecordProofSkipped(snapshot.SupplierOperatorAddress, snapshot.ServiceID, ProofSkippedReasonClaimMissingOnChain)
					if lc.sessionCoordinator != nil {
						if markErr := lc.sessionCoordinator.OnClaimMissing(ctx, snapshot.SessionID); markErr != nil {
							logger.Warn().
								Err(markErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to mark session as claim_missing")
						}
					}
					continue
				}
				if claimErr != nil {
					logger.Warn().
						Err(claimErr).
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("pre-proof guard: GetClaim RPC error — fail-open, proceeding with proof submission")
				}
			}

			if lc.proofChecker != nil {
				required, checkErr := lc.proofChecker.IsProofRequired(ctx, snapshot, proofRequirementSeedBlock.Hash())
				if checkErr != nil {
					// ErrClaimedRootUnavailable means the session has no
					// authoritative root to anchor a proof on — falling
					// open to submission would produce an on-chain invalid
					// proof. Mark the session as proof_tx_error (terminal
					// failure) so the pipeline doesn't spend gas on a
					// guaranteed reject.
					if errors.Is(checkErr, ErrClaimedRootUnavailable) {
						logger.Error().
							Err(checkErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("cannot submit proof: claimed root unavailable (HA failover with failed OnSessionClaimed write); marking session as proof_tx_error")
						if lc.sessionCoordinator != nil {
							if markErr := lc.sessionCoordinator.OnProofTxError(ctx, snapshot.SessionID); markErr != nil {
								logger.Warn().
									Err(markErr).
									Str(logging.FieldSessionID, snapshot.SessionID).
									Msg("failed to mark session as proof_tx_error after unavailable claimed root")
							}
						}
						continue
					}
					logger.Warn().
						Err(checkErr).
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("failed to check proof requirement, submitting proof anyway to avoid potential penalty")
					sessionsNeedingProof = append(sessionsNeedingProof, snapshot)
				} else if !required {
					// Proof NOT required → transition to probabilistic_proved
					logger.Info().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("proof NOT required for this claim - marking as probabilistically proved")
					RecordRevenueProbabilisticProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

					// CRITICAL: Transition session state to probabilistic_proved
					if lc.sessionCoordinator != nil {
						if probErr := lc.sessionCoordinator.OnProbabilisticProved(ctx, snapshot.SessionID); probErr != nil {
							logger.Warn().
								Err(probErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to mark session as probabilistic_proved")
						}
					}
				} else {
					logger.Info().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("proof IS required for this claim")
					sessionsNeedingProof = append(sessionsNeedingProof, snapshot)
				}
			} else {
				// No proof checker, always submit proofs (legacy behavior)
				sessionsNeedingProof = append(sessionsNeedingProof, snapshot)
			}
		}

		if len(sessionsNeedingProof) == 0 {
			logger.Info().
				Int64("session_end_height", sessionEndHeight).
				Msg("no proofs required for this group")
			continue
		}

		// NOTE: Timing spread DISABLED - submit proofs immediately when window opens
		// The protocol's GetEarliestSupplierProofCommitHeight() spreads suppliers across the window,
		// but this can cause proofs to be submitted too close to window close, resulting in failures.
		// We now submit immediately when the proof window opens to maximize success rate.
		earliestProofHeight := proofWindowOpenHeight // Use window open, not spread height

		logger.Info().
			Int64("proof_window_open", proofWindowOpenHeight).
			Int64("session_end_height", sessionEndHeight).
			Int("proofs_to_submit", len(sessionsNeedingProof)).
			Msg("proof window open - submitting immediately (timing spread disabled)")

		// Calculate proof path seed block height (one before earliest proof height)
		// Since we're using proofWindowOpenHeight, this equals proofRequirementSeedHeight
		proofPathSeedBlockHeight := earliestProofHeight - 1

		// Optimization: Reuse the seed block we already fetched for proof requirement check
		// if the heights match (they should, since distribution is disabled in poktroll)
		var proofPathSeedBlock pocktclient.Block
		if proofPathSeedBlockHeight == proofRequirementSeedHeight {
			// Heights match - reuse the block we already fetched
			proofPathSeedBlock = proofRequirementSeedBlock
			logger.Debug().
				Int64("proof_path_seed_block_height", proofPathSeedBlockHeight).
				Str("proof_path_seed_block_hash", fmt.Sprintf("%x", proofPathSeedBlock.Hash())).
				Msg("reusing proof requirement seed block for proof path (heights match)")
		} else {
			// Heights don't match - this shouldn't happen with current poktroll (distribution disabled),
			// but if it ever gets re-enabled, we need to fetch the correct block
			logger.Warn().
				Int64("proof_path_seed_height", proofPathSeedBlockHeight).
				Int64("proof_requirement_seed_height", proofRequirementSeedHeight).
				Msg("seed block heights don't match - possible distribution enabled in future, re-fetching")

			var seedErr error
			proofPathSeedBlock, seedErr = lc.waitForBlock(ctx, proofPathSeedBlockHeight)
			if seedErr != nil {
				return fmt.Errorf("failed to wait for proof path seed block: %w", seedErr)
			}

			logger.Debug().
				Int64("proof_path_seed_block_height", proofPathSeedBlockHeight).
				Str("proof_path_seed_block_hash", fmt.Sprintf("%x", proofPathSeedBlock.Hash())).
				Msg("obtained proof path seed block hash (re-fetched)")
		}

		// CRITICAL: Verify proof window is still open before proceeding
		// This prevents wasting fees on proofs that will be rejected
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(sharedParams, sessionEndHeight)
		currentBlock := lc.blockClient.LastBlock(ctx)
		if currentBlock.Height() >= proofWindowClose {
			logger.Error().
				Int64("current_height", currentBlock.Height()).
				Int64("proof_window_close", proofWindowClose).
				Int64("session_end_height", sessionEndHeight).
				Int("group_size", len(sessionsNeedingProof)).
				Msg("proof window already closed - cannot submit proofs")

			// Mark all sessions in this group as failed (metrics + Redis state for HA)
			for _, snapshot := range sessionsNeedingProof {
				RecordProofWindowClosed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

				// CRITICAL: Update session state in Redis immediately for HA compatibility
				// If this miner crashes, the new leader must know this session already failed
				if lc.sessionCoordinator != nil {
					if err := lc.sessionCoordinator.OnProofWindowClosed(ctx, snapshot.SessionID); err != nil {
						logger.Warn().
							Err(err).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to mark session as proof_window_closed in Redis")
					}
				}
			}

			return fmt.Errorf("proof window already closed at height %d (current: %d)", proofWindowClose, currentBlock.Height())
		}

		// CRITICAL: Re-check proof requirement RIGHT before building proofs
		// Between initial check (at proof window open) and now (at earliestProofHeight),
		// the blockchain might have already settled claims without requiring proofs.
		// This prevents wasting gas on simulation failures.
		// IMPORTANT: MUST use the same seed block as the validator (proofRequirementSeedBlock),
		// NOT proofPathSeedBlock, even though we're at a later height now.
		if lc.proofChecker != nil {
			stillNeedingProof := make([]*SessionSnapshot, 0, len(sessionsNeedingProof))
			for _, snapshot := range sessionsNeedingProof {
				required, recheckErr := lc.proofChecker.IsProofRequired(ctx, snapshot, proofRequirementSeedBlock.Hash())
				if recheckErr != nil {
					// Same guard as the initial check — a missing claimed
					// root means we'd submit a fabricated proof. Surface
					// as proof_tx_error instead of falling open.
					if errors.Is(recheckErr, ErrClaimedRootUnavailable) {
						logger.Error().
							Err(recheckErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("cannot submit proof on re-check: claimed root unavailable; marking session as proof_tx_error")
						if lc.sessionCoordinator != nil {
							if markErr := lc.sessionCoordinator.OnProofTxError(ctx, snapshot.SessionID); markErr != nil {
								logger.Warn().
									Err(markErr).
									Str(logging.FieldSessionID, snapshot.SessionID).
									Msg("failed to mark session as proof_tx_error after unavailable claimed root")
							}
						}
						continue
					}
					// Other errors - err on side of caution and submit anyway
					logger.Warn().
						Err(recheckErr).
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("failed to re-check proof requirement before submission, will attempt submission anyway")
					stillNeedingProof = append(stillNeedingProof, snapshot)
				} else if !required {
					// Proof NO LONGER required - blockchain settled claim without proof
					logger.Info().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("proof not required (blockchain settled claim without proof)")

					RecordRevenueProbabilisticProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

					if lc.sessionCoordinator != nil {
						if err := lc.sessionCoordinator.OnProbabilisticProved(ctx, snapshot.SessionID); err != nil {
							logger.Warn().
								Err(err).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to mark session as probabilistic_proved")
						}
					}
				} else {
					// Still required - proceed with proof
					stillNeedingProof = append(stillNeedingProof, snapshot)
				}
			}

			if len(stillNeedingProof) == 0 {
				logger.Info().
					Int64("session_end_height", sessionEndHeight).
					Msg("no proofs required after re-check (all claims settled without proof)")
				continue // Skip to next group
			}

			// Update the list to only include sessions that still need proofs
			sessionsNeedingProof = stillNeedingProof
		}

		logger.Debug().
			Int("group_size", len(sessionsNeedingProof)).
			Int64("proof_window_close", proofWindowClose).
			Int64("current_height", currentBlock.Height()).
			Int64("blocks_remaining", proofWindowClose-currentBlock.Height()).
			Msg("proof window timing reached - generating and submitting batched proofs")

		// DEBUG/TEST: Intentionally delay proof submission to test proof expiration tracking
		// Set environment variable TEST_PROOF_DELAY_SECONDS to enable (e.g., "60" for 60 seconds)
		// This simulates scenarios where proof generation takes too long or network delays occur
		if testCfg := getTestConfig(); testCfg.ProofDelaySeconds > 0 {
			delayDuration := time.Duration(testCfg.ProofDelaySeconds) * time.Second
			logger.Warn().
				Int("delay_seconds", testCfg.ProofDelaySeconds).
				Int64("proof_window_close", proofWindowClose).
				Int64("current_height", currentBlock.Height()).
				Msg("TEST MODE: Intentionally delaying proof submission to test expiration tracking")
			time.Sleep(delayDuration)

			// Log current state after delay
			postDelayHeight := lc.blockClient.LastBlock(ctx).Height()
			logger.Warn().
				Int64("height_after_delay", postDelayHeight).
				Int64("proof_window_close", proofWindowClose).
				Bool("window_still_open", postDelayHeight < proofWindowClose).
				Msg("TEST MODE: Delay complete, continuing with proof submission")
		}

		// PARALLEL PROOF BUILDING: Generate proofs and build messages concurrently.
		// Each proof generation is independent and thread-safe, so processing
		// batches of sessions in parallel significantly reduces latency.
		//
		// The result channel is exactly len(sessionsNeedingProof) so every
		// goroutine can write without blocking. The collector below MUST
		// drain every slot — early-return would (a) leak the remaining
		// goroutines permanently and (b) silently drop proofs that built
		// successfully, causing revenue loss when the proof window closes.
		proofResults := make(chan proofBuildResult, len(sessionsNeedingProof))
		numProofTasks := len(sessionsNeedingProof)

		for i, snapshot := range sessionsNeedingProof {
			// Capture loop variables for goroutine
			index := i
			snap := snapshot

			// Submit to bounded build pool if available, otherwise use unbounded goroutine
			buildProofFunc := func() {
				result := proofBuildResult{
					index:    index,
					snapshot: snap,
				}

				// Panic guard: collectProofBuildResults MUST receive exactly
				// numProofTasks values or a goroutine leaks per missing slot.
				// Converts any panic (nil snapshot field, SMST boundary miss,
				// nil seed hash, etc.) into a failed result so the collector
				// drains cleanly. Applies to both pool and fallback goroutine
				// paths because pond absorbs panics without writing our chan.
				defer func() {
					if r := recover(); r != nil {
						logging.PanicRecoveriesTotal.WithLabelValues("proof_build").Inc()
						lc.logger.Error().
							Str(logging.FieldSessionID, snap.SessionID).
							Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
							Str("panic_value", fmt.Sprintf("%v", r)).
							Str("stack_trace", string(debug.Stack())).
							Msg("PANIC RECOVERED in proof build goroutine")
						proofResults <- proofBuildResult{
							index:    index,
							snapshot: snap,
							err:      fmt.Errorf("proof build panic for session %s: %v", snap.SessionID, r),
						}
					}
				}()

				// Record the scheduled proof height for operators
				SetProofScheduledHeight(snap.SupplierOperatorAddress, snap.ServiceID, snap.SessionID, float64(earliestProofHeight))

				// Generate the proof path from the seed block hash
				path := protocol.GetPathForProof(proofPathSeedBlock.Hash(), snap.SessionID)

				lc.logger.Info().
					Str(logging.FieldSessionID, snap.SessionID).
					Str("block_hash_from_blockid", fmt.Sprintf("%x", proofPathSeedBlock.Hash())).
					Str("proof_path_computed", fmt.Sprintf("%x", path)).
					Int64("block_height", proofPathSeedBlockHeight).
					Msg("DEBUG: proof path computation (using BlockID.Hash)")

				// Generate the proof (CPU-bound cryptographic operation, can parallelize)
				proofBytes, proofErr := lc.smstManager.ProveClosest(ctx, snap.SessionID, path)
				if proofErr != nil {
					result.err = fmt.Errorf("failed to generate proof for session %s: %w", snap.SessionID, proofErr)
					proofResults <- result
					return
				}

				// Build the session header (network I/O, can parallelize)
				sessionHeader, headerErr := lc.buildSessionHeader(ctx, snap)
				if headerErr != nil {
					result.err = fmt.Errorf("failed to build session header for %s: %w", snap.SessionID, headerErr)
					proofResults <- result
					return
				}

				// Build proof message
				result.proofMsg = &prooftypes.MsgSubmitProof{
					SupplierOperatorAddress: snap.SupplierOperatorAddress,
					SessionHeader:           sessionHeader,
					Proof:                   proofBytes,
				}

				proofResults <- result
			}

			if lc.buildPool != nil {
				lc.buildPool.Submit(buildProofFunc)
			} else {
				go buildProofFunc()
			}
		}

		// Collect every result. Draining ALL numProofTasks values is required
		// even when some goroutines failed — the alternative is a permanent
		// goroutine leak (one per undrained slot) plus silent revenue loss
		// for sessions whose proof built successfully in the same batch.
		partitionedProofs := collectProofBuildResults(numProofTasks, proofResults)

		// Surface per-session build failures as warnings + metrics and keep
		// the batch moving with whatever built. Mirrors the claim path:
		// one bad session does not invalidate the rest of the batch.
		for _, r := range partitionedProofs.failed {
			snap := r.snapshot
			sessionID, supplier, serviceID := "", "", ""
			if snap != nil {
				sessionID = snap.SessionID
				supplier = snap.SupplierOperatorAddress
				serviceID = snap.ServiceID
			}
			RecordProofSkipped(supplier, serviceID, ProofSkippedReasonBuildFailed)
			logger.Warn().
				Err(r.err).
				Str(logging.FieldSessionID, sessionID).
				Str(logging.FieldSupplier, supplier).
				Str(logging.FieldServiceID, serviceID).
				Msg("session proof build failed - dropping from batch, other sessions continue")
		}

		// If every proof in the batch failed to build there is nothing to
		// submit. Surface the first failure (they are already logged above
		// individually) so the caller can meter / retry on the next cycle.
		if len(partitionedProofs.built) == 0 {
			if len(partitionedProofs.failed) > 0 {
				return fmt.Errorf("all proofs in batch failed to build (batch_size=%d): %w",
					numProofTasks, partitionedProofs.failed[0].err)
			}
			// numProofTasks was zero — nothing to do.
			return nil
		}

		// Preserve input ordering so later metric/state iterations and the
		// submitted tx payload line up with sessionsNeedingProof.
		proofBuildResultsSorted := make([]proofBuildResult, 0, len(partitionedProofs.built))
		proofBuildResultsSorted = append(proofBuildResultsSorted, partitionedProofs.built...)
		sort.SliceStable(proofBuildResultsSorted, func(i, j int) bool {
			return proofBuildResultsSorted[i].index < proofBuildResultsSorted[j].index
		})

		proofMsgs := make([]*prooftypes.MsgSubmitProof, 0, len(proofBuildResultsSorted))
		validProofSnapshots := make([]*SessionSnapshot, 0, len(proofBuildResultsSorted))
		for _, result := range proofBuildResultsSorted {
			proofMsgs = append(proofMsgs, result.proofMsg)
			validProofSnapshots = append(validProofSnapshots, result.snapshot)
		}

		// Convert to interface types for variadic call
		interfaceProofMsgs := make([]pocktclient.MsgSubmitProof, len(proofMsgs))
		for i, msg := range proofMsgs {
			interfaceProofMsgs[i] = msg
		}

		// CRITICAL: Re-check window is still open RIGHT before submission
		// Building proofs (proof generation, headers) takes time - blocks may have advanced!
		currentBlock = lc.blockClient.LastBlock(ctx)
		if currentBlock.Height() >= proofWindowClose {
			logger.Error().
				Int64("current_height", currentBlock.Height()).
				Int64("proof_window_close", proofWindowClose).
				Int64("session_end_height", sessionEndHeight).
				Int("batch_size", len(proofMsgs)).
				Msg("proof window closed while building proofs - cannot submit")

			// Mark the sessions that actually built (and therefore would
			// have been submitted) as window-closed. Sessions whose proof
			// build failed are already accounted for via the per-build
			// warning + RecordProofSkipped("build_failed") above.
			for _, snapshot := range validProofSnapshots {
				RecordProofWindowClosed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

				// CRITICAL: Update session state in Redis immediately for HA compatibility
				if lc.sessionCoordinator != nil {
					if err := lc.sessionCoordinator.OnProofWindowClosed(ctx, snapshot.SessionID); err != nil {
						logger.Warn().
							Err(err).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to mark session as proof_window_closed in Redis")
					}
				}
			}

			return fmt.Errorf("proof window closed while building proofs at height %d (current: %d)", proofWindowClose, currentBlock.Height())
		}

		proofBlocksRemaining := proofWindowClose - currentBlock.Height()
		proofBlockTimeSec := lc.config.BlockTimeSeconds
		if proofBlockTimeSec <= 0 {
			proofBlockTimeSec = 30
		}
		rawProofTimeout := time.Duration(proofBlocksRemaining) * time.Duration(proofBlockTimeSec) * time.Second
		proofCtx := tx.WithTxWindowTimeout(ctx, rawProofTimeout)

		logger.Info().
			Int64("current_height", currentBlock.Height()).
			Int64("proof_window_close", proofWindowClose).
			Int64("blocks_remaining", proofBlocksRemaining).
			Dur("tx_deadline", rawProofTimeout).
			Int("batch_size", len(proofMsgs)).
			Msg("submitting proofs")

		// Submit all proofs in a single transaction with retries
		var lastErr error
		var proofTxHash string
		for attempt := 1; attempt <= lc.config.ProofRetryAttempts; attempt++ {
			submitErr := lc.supplierClient.SubmitProofs(proofCtx, proofWindowClose, interfaceProofMsgs...)
			if submitErr != nil {
				lastErr = submitErr

				// Check if error is due to proof window being closed (permanent failure - don't retry)
				errorMsg := submitErr.Error()
				if strings.Contains(errorMsg, "proof window") || strings.Contains(errorMsg, "proof_window") {
					logger.Error().
						Err(submitErr).
						Int64("current_height", lc.blockClient.LastBlock(ctx).Height()).
						Int64("proof_window_close", proofWindowClose).
						Int("batch_size", len(proofMsgs)).
						Msg("proof window closed during submission - permanent failure, not retrying")

					// Only mark sessions whose proof actually built (and
					// therefore entered the submit tx) as window-closed. Build
					// failures are already metered above.
					for _, snapshot := range validProofSnapshots {
						RecordProofWindowClosed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

						// CRITICAL: Update session state in Redis immediately for HA compatibility
						if lc.sessionCoordinator != nil {
							if err := lc.sessionCoordinator.OnProofWindowClosed(ctx, snapshot.SessionID); err != nil {
								logger.Warn().
									Err(err).
									Str(logging.FieldSessionID, snapshot.SessionID).
									Msg("failed to mark session as proof_window_closed in Redis")
							}
						}
					}

					break // Don't retry - this is a permanent failure
				}

				logger.Warn().
					Err(submitErr).
					Int(logging.FieldAttempt, attempt).
					Int(logging.FieldMaxRetry, lc.config.ProofRetryAttempts).
					Int("batch_size", len(proofMsgs)).
					Msg("batched proof submission failed, retrying")

				if attempt < lc.config.ProofRetryAttempts {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(lc.config.ProofRetryDelay):
						continue
					}
				}
			} else {
				// SUCCESS: Proof TX broadcast accepted to mempool
				// Retrieve TX hash from HA client (stored immediately after broadcast)
				if haClient, ok := lc.supplierClient.(*tx.HASupplierClient); ok {
					proofTxHash = haClient.GetLastProofTxHash()
				}

				currentBlock := lc.blockClient.LastBlock(ctx)
				blocksAfterWindowOpen := float64(currentBlock.Height() - proofWindowOpenHeight)

				// CRITICAL: Save TX hash to Redis IMMEDIATELY (1 line after broadcast)
				// This prevents duplicate submissions if we crash after TX broadcast.
				// Iterate only the snapshots actually submitted — build-failed
				// snapshots do not get a proof tx hash.
				for _, snapshot := range validProofSnapshots {
					if lc.sessionCoordinator != nil {
						if updateErr := lc.sessionCoordinator.OnProofSubmitted(ctx, snapshot.SessionID, proofTxHash); updateErr != nil {
							logger.Warn().
								Err(updateErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Str("proof_tx_hash", proofTxHash).
								Msg("failed to store proof TX hash")
						}
					}
				}

				// Record metrics for sessions whose proof was actually submitted.
				for _, snapshot := range validProofSnapshots {
					RecordProofSubmitted(snapshot.SupplierOperatorAddress, snapshot.ServiceID)
					RecordProofSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
					RecordRevenueProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)
				}

				// Track proof submissions to Redis for debugging. The index
				// into proofMsgs must match validProofSnapshots (both are the
				// built-only ordered set).
				if lc.submissionTracker != nil {
					proofRequirementSeed := hex.EncodeToString(proofRequirementSeedBlock.Hash())
					for i, snapshot := range validProofSnapshots {
						// Get proof hash from proof message
						proofHash := hex.EncodeToString(proofMsgs[i].Proof)

						if trackErr := lc.submissionTracker.TrackProofSubmission(
							ctx,
							snapshot.SupplierOperatorAddress,
							snapshot.SessionEndHeight,
							snapshot.SessionID,
							proofHash,
							proofTxHash,
							true, // success
							"",   // no error
							earliestProofHeight,
							currentBlock.Height(),
							true,                 // proof was required
							proofRequirementSeed, // seed used for proof requirement check
						); trackErr != nil {
							logger.Warn().
								Err(trackErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to track proof submission")
						}
					}
				}

				logger.Info().
					Int("batch_size", len(proofMsgs)).
					Str("proof_tx_hash", proofTxHash).
					Int64("blocks_after_window", int64(blocksAfterWindowOpen)).
					Msg("batched proofs submitted successfully")

				break // Success, exit retry loop
			}
		}

		if lastErr != nil {
			// Mark sessions that entered the tx as failed (the ones that did
			// not build are already counted as build_failed via RecordProofSkipped).
			for _, snapshot := range validProofSnapshots {
				RecordProofTxError(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

				// CRITICAL: Update session state in Redis immediately for HA compatibility
				if lc.sessionCoordinator != nil {
					if err := lc.sessionCoordinator.OnProofTxError(ctx, snapshot.SessionID); err != nil {
						logger.Warn().
							Err(err).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to mark session as proof_tx_error in Redis")
					}
				}
			}

			// Track failed proof submissions to Redis for debugging. The index
			// into proofMsgs corresponds to validProofSnapshots by construction.
			if lc.submissionTracker != nil {
				proofRequirementSeed := hex.EncodeToString(proofRequirementSeedBlock.Hash())
				for i, snapshot := range validProofSnapshots {
					// Get proof hash from proof message (proof was built, but submission failed)
					proofHash := hex.EncodeToString(proofMsgs[i].Proof)

					if trackErr := lc.submissionTracker.TrackProofSubmission(
						ctx,
						snapshot.SupplierOperatorAddress,
						snapshot.SessionEndHeight,
						snapshot.SessionID,
						proofHash,
						"",    // no TX hash on failure
						false, // failed
						lastErr.Error(),
						earliestProofHeight,
						lc.blockClient.LastBlock(ctx).Height(),
						true,                 // proof was required (we attempted submission)
						proofRequirementSeed, // seed used for proof requirement check
					); trackErr != nil {
						logger.Warn().
							Err(trackErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to track failed proof submission")
					}
				}
			}

			return fmt.Errorf("batched proof submission failed after %d attempts: %w", lc.config.ProofRetryAttempts, lastErr)
		}
	}

	return nil
}

// OnSessionProved is called when a session proof is successfully submitted.
// It cleans up resources associated with the session.
func (lc *LifecycleCallback) OnSessionProved(ctx context.Context, snapshot *SessionSnapshot) error {
	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Logger()

	logger.Debug().
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("session proved - cleaning up")

	// Record session proved metrics
	RecordSessionProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID)

	// Clean up session-specific metrics (gauges with session_id label)
	ClearSessionMetrics(snapshot.SupplierOperatorAddress, snapshot.SessionID, snapshot.ServiceID)

	// Clean up SMST
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		logger.Warn().Err(err).Msg("failed to delete SMST tree")
	}

	// Delete the session stream to stop consuming late relays
	if lc.streamDeleter != nil {
		if err := lc.streamDeleter.DeleteStream(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to delete session stream")
		}
	}

	// Update snapshot manager
	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnSessionProved(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to update snapshot after proof")
		}
	}

	// Clean up deduplication local cache entries for this session.
	// This prevents unbounded memory growth in the deduplicator's local cache.
	if lc.deduplicator != nil {
		if err := lc.deduplicator.CleanupSession(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to cleanup deduplication entries")
		}
	}

	// Remove session lock
	lc.removeSessionLock(snapshot.SessionID)

	return nil
}

// OnClaimSkipped is called when the economic viability check rejected a
// session at claim time. It transitions the session to the terminal
// ClaimSkipped state and cleans up local resources (SMST tree, stream,
// dedup cache) the same way a successful claim path would.
func (lc *LifecycleCallback) OnClaimSkipped(ctx context.Context, snapshot *SessionSnapshot) error {
	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Logger()

	logger.Debug().
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("claim skipped for economic reasons - cleaning up")

	ClearSessionMetrics(snapshot.SupplierOperatorAddress, snapshot.SessionID, snapshot.ServiceID)

	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		logger.Warn().Err(err).Msg("failed to delete SMST tree on claim_skipped")
	}

	if lc.streamDeleter != nil {
		if err := lc.streamDeleter.DeleteStream(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to delete session stream on claim_skipped")
		}
	}

	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnClaimSkipped(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to update coordinator on claim_skipped")
			return err
		}
	}

	if lc.deduplicator != nil {
		if err := lc.deduplicator.CleanupSession(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to cleanup deduplication on claim_skipped")
		}
	}

	lc.removeSessionLock(snapshot.SessionID)
	return nil
}

// OnProbabilisticProved is called when a session is probabilistically proved (no proof required).
// It cleans up resources associated with the session.
func (lc *LifecycleCallback) OnProbabilisticProved(ctx context.Context, snapshot *SessionSnapshot) error {
	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Logger()

	logger.Debug().
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("session probabilistically proved (no proof required) - cleaning up")

	// Record session outcome
	RecordSessionProbabilisticProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID)

	// Clean up session-specific metrics (gauges with session_id label)
	ClearSessionMetrics(snapshot.SupplierOperatorAddress, snapshot.SessionID, snapshot.ServiceID)

	// Clean up SMST tree
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		logger.Warn().Err(err).Msg("failed to delete SMST tree")
	}

	// Delete the session stream to stop consuming late relays
	if lc.streamDeleter != nil {
		if err := lc.streamDeleter.DeleteStream(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to delete session stream")
		}
	}

	// Update snapshot manager
	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnProbabilisticProved(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to update session coordinator")
			return err
		}
	}

	// Clean up deduplication local cache entries for this session.
	// This prevents unbounded memory growth in the deduplicator's local cache.
	if lc.deduplicator != nil {
		if err := lc.deduplicator.CleanupSession(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to cleanup deduplication entries")
		}
	}

	// Remove session lock
	lc.removeSessionLock(snapshot.SessionID)

	return nil
}

// OnClaimWindowClosed is called when a session fails due to claim window timeout.

func (lc *LifecycleCallback) OnClaimWindowClosed(ctx context.Context, snapshot *SessionSnapshot) error {
	// Clean up SMST tree to prevent memory leak
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().Err(err).Str("session_id", snapshot.SessionID).Msg("failed to delete SMST tree on claim window closed")
	}

	// Metrics already recorded at failure point, just cleanup
	lc.removeSessionLock(snapshot.SessionID)
	return nil
}

// OnClaimTxError is called when a session fails due to claim transaction error.
func (lc *LifecycleCallback) OnClaimTxError(ctx context.Context, snapshot *SessionSnapshot) error {
	// Clean up SMST tree to prevent memory leak
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().Err(err).Str("session_id", snapshot.SessionID).Msg("failed to delete SMST tree on claim tx error")
	}

	// Metrics already recorded at failure point, just cleanup
	lc.removeSessionLock(snapshot.SessionID)
	return nil
}

// OnProofWindowClosed is called when a session fails due to proof window timeout.
func (lc *LifecycleCallback) OnProofWindowClosed(ctx context.Context, snapshot *SessionSnapshot) error {
	// Clean up SMST tree to prevent memory leak
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().Err(err).Str("session_id", snapshot.SessionID).Msg("failed to delete SMST tree on proof window closed")
	}

	// Metrics already recorded at failure point, just cleanup
	lc.removeSessionLock(snapshot.SessionID)
	return nil
}

// OnProofTxError is called when a session fails due to proof transaction error.
func (lc *LifecycleCallback) OnProofTxError(ctx context.Context, snapshot *SessionSnapshot) error {
	// Clean up SMST tree to prevent memory leak
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().Err(err).Str("session_id", snapshot.SessionID).Msg("failed to delete SMST tree on proof tx error")
	}

	// Metrics already recorded at failure point, just cleanup
	lc.removeSessionLock(snapshot.SessionID)
	return nil
}

// waitForBlock waits for a specific block height to be reached using event-driven
// block notifications. This is more efficient than polling and doesn't block workers.
func (lc *LifecycleCallback) waitForBlock(ctx context.Context, targetHeight int64) (pocktclient.Block, error) {
	startTime := time.Now()

	// Check if we're already at or past the target height
	currentBlock := lc.blockClient.LastBlock(ctx)
	currentHeight := currentBlock.Height()

	if currentHeight >= targetHeight {
		// Already at target height - no wait needed
		lc.logger.Debug().
			Int64("target_height", targetHeight).
			Int64("current_height", currentHeight).
			Dur("elapsed_ms", time.Since(startTime)).
			Msg("waitForBlock: already at target height (no wait)")
		return lc.getBlockAtHeight(ctx, targetHeight)
	}

	// BLOCKING WAIT DETECTED - this will block until target height
	blocksToWait := targetHeight - currentHeight
	lc.logger.Warn().
		Int64("target_height", targetHeight).
		Int64("current_height", currentHeight).
		Int64("blocks_to_wait", blocksToWait).
		Msg("BLOCKING: waitForBlock starting - waiting for future block")

	// Try to use event-driven approach with Subscribe()
	subscriber, ok := lc.blockClient.(interface {
		Subscribe(ctx context.Context, bufferSize int) <-chan *localclient.SimpleBlock
	})
	if !ok {
		// Fallback to polling if Subscribe() not available (shouldn't happen in production)
		lc.logger.Warn().
			Int64("target_height", targetHeight).
			Msg("block client does not support Subscribe(), falling back to polling")
		return lc.waitForBlockPolling(ctx, targetHeight)
	}

	// Subscribe to block events - use small buffer since we only need to detect one block
	blockCh := subscriber.Subscribe(ctx, 10)

	for {
		select {
		case <-ctx.Done():
			lc.logger.Warn().
				Int64("target_height", targetHeight).
				Dur("elapsed_ms", time.Since(startTime)).
				Msg("waitForBlock: context cancelled while waiting")
			return nil, ctx.Err()

		case block, ok := <-blockCh:
			if !ok {
				// Channel closed, fall back to polling
				lc.logger.Warn().
					Int64("target_height", targetHeight).
					Msg("block subscription channel closed, falling back to polling")
				return lc.waitForBlockPolling(ctx, targetHeight)
			}

			if block.Height() >= targetHeight {
				elapsed := time.Since(startTime)
				lc.logger.Info().
					Int64("target_height", targetHeight).
					Int64("reached_height", block.Height()).
					Int64("blocks_waited", block.Height()-currentHeight).
					Dur("elapsed_ms", elapsed).
					Msg("waitForBlock: target height reached")
				return lc.getBlockAtHeight(ctx, targetHeight)
			}
		}
	}
}

// waitForBlockPolling is the fallback polling approach (only used if Subscribe unavailable).
func (lc *LifecycleCallback) waitForBlockPolling(ctx context.Context, targetHeight int64) (pocktclient.Block, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			currentBlock := lc.blockClient.LastBlock(ctx)
			if currentBlock.Height() >= targetHeight {
				return lc.getBlockAtHeight(ctx, targetHeight)
			}

			lc.logger.Debug().
				Int64("current_height", currentBlock.Height()).
				Int64("target_height", targetHeight).
				Msg("waiting for block height (polling fallback)")
		}
	}
}

// getBlockAtHeight fetches the specific block at targetHeight.
// CRITICAL: We must query the exact block to get the correct BlockID.Hash for proof validation.
func (lc *LifecycleCallback) getBlockAtHeight(ctx context.Context, targetHeight int64) (pocktclient.Block, error) {
	blockSubscriber, ok := lc.blockClient.(interface {
		GetBlockAtHeight(context.Context, int64) (pocktclient.Block, error)
	})
	if !ok {
		return nil, fmt.Errorf("BlockClient doesn't support GetBlockAtHeight - proof generation will fail (target_height=%d)", targetHeight)
	}

	block, err := blockSubscriber.GetBlockAtHeight(ctx, targetHeight)
	if err != nil {
		return nil, fmt.Errorf("failed to get block at height %d: %w", targetHeight, err)
	}
	return block, nil
}

// buildSessionHeader builds a session header from the snapshot.
// It queries the session from the blockchain to get complete information.
func (lc *LifecycleCallback) buildSessionHeader(ctx context.Context, snapshot *SessionSnapshot) (*sessiontypes.SessionHeader, error) {
	if lc.sessionClient != nil {
		// Query the session from the blockchain to get the complete header
		session, err := lc.sessionClient.GetSession(
			ctx,
			snapshot.ApplicationAddress,
			snapshot.ServiceID,
			snapshot.SessionStartHeight,
		)
		if err != nil {
			lc.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Msg("failed to query session from blockchain, using snapshot data")
		} else if session != nil {
			return session.Header, nil
		}
	}

	// Fallback: build from snapshot data
	return &sessiontypes.SessionHeader{
		SessionId:               snapshot.SessionID,
		ApplicationAddress:      snapshot.ApplicationAddress,
		ServiceId:               snapshot.ServiceID,
		SessionStartBlockHeight: snapshot.SessionStartHeight,
		SessionEndBlockHeight:   snapshot.SessionEndHeight,
	}, nil
}

// Ensure LifecycleCallback implements SessionLifecycleCallback
var _ SessionLifecycleCallback = (*LifecycleCallback)(nil)
