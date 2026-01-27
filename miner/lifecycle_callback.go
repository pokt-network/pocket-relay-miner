package miner

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alitto/pond/v2"

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
	testConfig     TestConfig
	testConfigOnce sync.Once
)

// getTestConfig returns the test configuration, reading environment variables once.
func getTestConfig() TestConfig {
	testConfigOnce.Do(func() {
		testConfig.ForceClaimTxError = os.Getenv("TEST_FORCE_CLAIM_TX_ERROR") == "true"
		testConfig.ForceProofTxError = os.Getenv("TEST_FORCE_PROOF_TX_ERROR") == "true"

		if delayStr := os.Getenv("TEST_CLAIM_DELAY_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				testConfig.ClaimDelaySeconds = delay
			}
		}

		// Support both TEST_DELAY_PROOF_SECONDS and TEST_PROOF_DELAY_SECONDS for backward compatibility
		if delayStr := os.Getenv("TEST_DELAY_PROOF_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				testConfig.ProofDelaySeconds = delay
			}
		}
		if delayStr := os.Getenv("TEST_PROOF_DELAY_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				testConfig.ProofDelaySeconds = delay
			}
		}
	})
	return testConfig
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

// removeSessionLock removes a per-session lock.
func (lc *LifecycleCallback) removeSessionLock(sessionID string) {
	lc.sessionLocksMu.Lock()
	defer lc.sessionLocksMu.Unlock()
	delete(lc.sessionLocks, sessionID)
}

// isClaimEconomicallyViable checks if submitting a claim is profitable.
// Returns false if the expected reward is less than the estimated transaction fee.
func (lc *LifecycleCallback) isClaimEconomicallyViable(
	snapshot *SessionSnapshot,
	computeUnitsToTokensMultiplier uint64,
	estimatedFeeUpokt uint64,
) bool {
	// Calculate expected reward in upokt
	// Formula: reward = TotalComputeUnits * ComputeUnitsToTokensMultiplier
	expectedRewardUpokt := snapshot.TotalComputeUnits * computeUnitsToTokensMultiplier

	// Compare reward vs fee
	return expectedRewardUpokt > estimatedFeeUpokt
}

// checkClaimCeiling checks if the claimed amount exceeds the configured ceiling.
// This is a WARNING ONLY - we do not cap claims, as relays have already been accepted.
// The warning helps operators understand when they may be doing unpaid work.
func (lc *LifecycleCallback) checkClaimCeiling(
	ctx context.Context,
	snapshot *SessionSnapshot,
	sharedParams *sharedtypes.Params,
	computeUnitsToTokensMultiplier uint64,
) {
	// Skip if no service factor provider or app client configured
	if lc.serviceFactorProvider == nil || lc.appClient == nil {
		return
	}

	logger := lc.logger.With().
		Str(logging.FieldSessionID, snapshot.SessionID).
		Str(logging.FieldServiceID, snapshot.ServiceID).
		Str(logging.FieldApplication, snapshot.ApplicationAddress).
		Logger()

	// Get application stake
	app, err := lc.appClient.GetApplication(ctx, snapshot.ApplicationAddress)
	if err != nil {
		logger.Debug().
			Err(err).
			Msg("failed to get application for claim ceiling check")
		return
	}

	appStake := app.Stake.Amount.Int64()
	if appStake <= 0 {
		logger.Debug().
			Int64("app_stake", appStake).
			Msg("skipping ceiling check - invalid app stake")
		return
	}

	// Get proof_window_close_offset_blocks for baseLimit calculation
	proofWindowCloseBlocks := int64(sharedParams.GetProofWindowCloseOffsetBlocks())
	if proofWindowCloseBlocks <= 0 {
		proofWindowCloseBlocks = 1 // Avoid division by zero
	}

	// Query session to get numSuppliersPerSession
	session, err := lc.sessionClient.GetSession(
		ctx,
		snapshot.ApplicationAddress,
		snapshot.ServiceID,
		snapshot.SessionStartHeight,
	)
	if err != nil {
		logger.Debug().
			Err(err).
			Msg("failed to get session for claim ceiling check")
		return
	}

	numSuppliers := int64(len(session.Suppliers))
	if numSuppliers <= 0 {
		numSuppliers = 1 // Avoid division by zero
	}

	// Calculate baseLimit: (appStake / numSuppliers) / proof_window_close_offset_blocks
	appStakePerSupplier := appStake / numSuppliers
	baseLimitUpokt := appStakePerSupplier / proofWindowCloseBlocks

	// Get serviceFactor for this service
	serviceFactor, hasServiceFactor := lc.serviceFactorProvider.GetServiceFactor(snapshot.ServiceID)

	// Calculate ceiling
	var ceilingUpokt int64
	if hasServiceFactor && serviceFactor > 0 {
		// ServiceFactor provided: apply directly to appStake
		ceilingUpokt = int64(float64(appStake) * serviceFactor)
	} else {
		// No serviceFactor: use baseLimit (protocol match)
		ceilingUpokt = baseLimitUpokt
	}

	// Calculate claimed amount in uPOKT
	claimedUpokt := int64(snapshot.TotalComputeUnits * computeUnitsToTokensMultiplier)

	// Check if claimed exceeds ceiling
	if claimedUpokt > ceilingUpokt {
		potentiallyUnpaidUpokt := claimedUpokt - ceilingUpokt

		// Record metric for monitoring/alerting
		RecordClaimCeilingExceeded(snapshot.SupplierOperatorAddress, snapshot.ServiceID, potentiallyUnpaidUpokt)

		logger.Warn().
			Int64("claimed_upokt", claimedUpokt).
			Int64("ceiling_upokt", ceilingUpokt).
			Int64("base_limit_upokt", baseLimitUpokt).
			Int64("potentially_unpaid_upokt", potentiallyUnpaidUpokt).
			Int64("app_stake_upokt", appStake).
			Int64("num_suppliers", numSuppliers).
			Int64("proof_window_close_blocks", proofWindowCloseBlocks).
			Bool("has_service_factor", hasServiceFactor).
			Float64("service_factor", serviceFactor).
			Uint64("total_compute_units", snapshot.TotalComputeUnits).
			Int64("relay_count", snapshot.RelayCount).
			Msg("CLAIM EXCEEDS CEILING - potential unpaid work detected (this is informational, claim will still be submitted)")
	} else if hasServiceFactor && claimedUpokt < baseLimitUpokt {
		// Informational: serviceFactor is conservative (below protocol guarantee)
		logger.Debug().
			Int64("claimed_upokt", claimedUpokt).
			Int64("ceiling_upokt", ceilingUpokt).
			Int64("base_limit_upokt", baseLimitUpokt).
			Float64("service_factor", serviceFactor).
			Msg("claim is below configured ceiling (conservative serviceFactor)")
	}
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

		// Build all claims for this group (PARALLELIZED for performance)
		// Pre-filter sessions for validation before parallel processing
		var validSnapshots []*SessionSnapshot
		for _, snapshot := range groupSnapshots {
			// CRITICAL: Deduplication check - never submit the same claim twice
			// This prevents duplicate claims if we crash and restart between TX broadcast and Redis save
			if snapshot.ClaimTxHash != "" {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
					Str("existing_claim_tx_hash", snapshot.ClaimTxHash).
					Msg("skipping claim - already submitted for this session (deduplication)")
				continue // Skip this session
			}

			// CRITICAL: Never submit claims with 0 relays or 0 value - waste of fees
			if snapshot.RelayCount == 0 {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
					Msg("skipping claim - session has 0 relays")
				// Session remains in active state - not a failure, just skipped
				continue // Skip this session
			}

			if snapshot.TotalComputeUnits == 0 {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
					Int64("relay_count", snapshot.RelayCount).
					Msg("skipping claim - session has 0 compute units despite having relays")
				// Session remains in active state - not a failure, just skipped
				continue // Skip this session
			}

			// CRITICAL: Economic validation - never submit claims where fee > reward
			// This prevents wasting fees on unprofitable claims
			if haClient, ok := lc.supplierClient.(*tx.HASupplierClient); ok {
				estimatedFeeUpokt := haClient.GetEstimatedFeeUpokt()
				computeUnitsToTokensMultiplier := sharedParams.GetComputeUnitsToTokensMultiplier()

				if !lc.isClaimEconomicallyViable(snapshot, computeUnitsToTokensMultiplier, estimatedFeeUpokt) {
					expectedRewardUpokt := snapshot.TotalComputeUnits * computeUnitsToTokensMultiplier

					logger.Warn().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
						Uint64("expected_reward_upokt", expectedRewardUpokt).
						Uint64("estimated_fee_upokt", estimatedFeeUpokt).
						Int64("relay_count", snapshot.RelayCount).
						Uint64("total_compute_units", snapshot.TotalComputeUnits).
						Msg("skipping claim - estimated fee exceeds expected reward (unprofitable)")

					// Session remains in active state - not a failure, just skipped
					continue // Skip this session
				}

				// Check if claim exceeds configured ceiling (warning only, does not block claim)
				lc.checkClaimCeiling(ctx, snapshot, sharedParams, computeUnitsToTokensMultiplier)
			}

			// Record the scheduled claim height for operators
			SetClaimScheduledHeight(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.SessionID, float64(earliestClaimHeight))

			validSnapshots = append(validSnapshots, snapshot)
		}

		// PARALLEL CLAIM BUILDING: Flush SMSTs and build claim messages concurrently
		// Each SMST flush is independent and thread-safe (has its own mutex)
		// This significantly reduces latency when processing batches of sessions
		type claimBuildResult struct {
			index    int
			snapshot *SessionSnapshot
			claimMsg *prooftypes.MsgCreateClaim
			rootHash []byte
			err      error
		}

		results := make(chan claimBuildResult, len(validSnapshots))
		numTasks := len(validSnapshots)

		for i, snapshot := range validSnapshots {
			// Capture loop variables for goroutine
			index := i
			snap := snapshot

			// Submit to bounded build pool if available, otherwise use unbounded goroutine
			buildFunc := func() {
				result := claimBuildResult{
					index:    index,
					snapshot: snap,
				}

				// Flush the SMST to get the root hash (CPU-bound, can parallelize)
				rootHash, flushErr := lc.smstManager.FlushTree(ctx, snap.SessionID)
				if flushErr != nil {
					result.err = fmt.Errorf("failed to flush SMST for session %s: %w", snap.SessionID, flushErr)
					results <- result
					return
				}
				result.rootHash = rootHash

				// Build the session header (network I/O, can parallelize)
				sessionHeader, headerErr := lc.buildSessionHeader(ctx, snap)
				if headerErr != nil {
					result.err = fmt.Errorf("failed to build session header for %s: %w", snap.SessionID, headerErr)
					results <- result
					return
				}

				// Build claim message
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

		// Collect all results (blocking until all tasks complete)
		claimResults := make([]claimBuildResult, numTasks)
		for i := 0; i < numTasks; i++ {
			result := <-results
			if result.err != nil {
				return nil, result.err
			}
			claimResults[result.index] = result
		}

		// Extract messages and root hashes in order
		var claimMsgs []*prooftypes.MsgCreateClaim
		var groupRootHashes [][]byte
		for _, result := range claimResults {
			claimMsgs = append(claimMsgs, result.claimMsg)
			groupRootHashes = append(groupRootHashes, result.rootHash)
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

		logger.Debug().
			Int64("current_height", currentBlock.Height()).
			Int64("claim_window_close", claimWindowClose).
			Int64("blocks_remaining", claimWindowClose-currentBlock.Height()).
			Int("batch_size", len(claimMsgs)).
			Msg("final window check passed - submitting claims")

		// Submit all claims in a single transaction with retries
		var lastErr error
		var claimTxHash string
		for attempt := 1; attempt <= lc.config.ClaimRetryAttempts; attempt++ {
			submitErr := lc.supplierClient.CreateClaims(ctx, claimWindowClose, interfaceClaimMsgs...)
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

			if lc.proofChecker != nil {
				required, checkErr := lc.proofChecker.IsProofRequired(ctx, snapshot, proofRequirementSeedBlock.Hash())
				if checkErr != nil {
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
					// Error checking - err on side of caution and submit anyway
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

		// PARALLEL PROOF BUILDING: Generate proofs and build messages concurrently
		// Each proof generation is independent and thread-safe
		// This significantly reduces latency when processing batches of sessions
		type proofBuildResult struct {
			index    int
			snapshot *SessionSnapshot
			proofMsg *prooftypes.MsgSubmitProof
			err      error
		}

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

		// Collect all results (blocking until all tasks complete)
		proofBuildResults := make([]proofBuildResult, numProofTasks)
		for i := 0; i < numProofTasks; i++ {
			result := <-proofResults
			if result.err != nil {
				return result.err
			}
			proofBuildResults[result.index] = result
		}

		// Extract messages in order
		var proofMsgs []*prooftypes.MsgSubmitProof
		for _, result := range proofBuildResults {
			proofMsgs = append(proofMsgs, result.proofMsg)
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

			// Mark all sessions in this batch as failed (metrics + Redis state for HA)
			for _, snapshot := range sessionsNeedingProof {
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

		logger.Debug().
			Int64("current_height", currentBlock.Height()).
			Int64("proof_window_close", proofWindowClose).
			Int64("blocks_remaining", proofWindowClose-currentBlock.Height()).
			Int("batch_size", len(proofMsgs)).
			Msg("final window check passed - submitting proofs")

		// Submit all proofs in a single transaction with retries
		var lastErr error
		var proofTxHash string
		for attempt := 1; attempt <= lc.config.ProofRetryAttempts; attempt++ {
			submitErr := lc.supplierClient.SubmitProofs(ctx, proofWindowClose, interfaceProofMsgs...)
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

					// Mark all sessions in this batch as failed (metrics + Redis state for HA)
					for _, snapshot := range sessionsNeedingProof {
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
				// This prevents duplicate submissions if we crash after TX broadcast
				for _, snapshot := range sessionsNeedingProof {
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

				// Record metrics for all sessions in the batch
				for _, snapshot := range sessionsNeedingProof {
					RecordProofSubmitted(snapshot.SupplierOperatorAddress, snapshot.ServiceID)
					RecordProofSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
					RecordRevenueProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)
				}

				// Track proof submissions to Redis for debugging
				if lc.submissionTracker != nil {
					proofRequirementSeed := hex.EncodeToString(proofRequirementSeedBlock.Hash())
					for i, snapshot := range sessionsNeedingProof {
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
			// Mark all sessions as failed due to proof TX error (after exhausting retries)
			for _, snapshot := range sessionsNeedingProof {
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

			// Track failed proof submissions to Redis for debugging
			if lc.submissionTracker != nil {
				proofRequirementSeed := hex.EncodeToString(proofRequirementSeedBlock.Hash())
				for i, snapshot := range sessionsNeedingProof {
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
func (lc *LifecycleCallback) OnClaimWindowClosed(_ context.Context, snapshot *SessionSnapshot) error {
	// Metrics already recorded at failure point, just cleanup
	lc.removeSessionLock(snapshot.SessionID)
	return nil
}

// OnClaimTxError is called when a session fails due to claim transaction error.
func (lc *LifecycleCallback) OnClaimTxError(_ context.Context, snapshot *SessionSnapshot) error {
	// Metrics already recorded at failure point, just cleanup
	lc.removeSessionLock(snapshot.SessionID)
	return nil
}

// OnProofWindowClosed is called when a session fails due to proof window timeout.
func (lc *LifecycleCallback) OnProofWindowClosed(_ context.Context, snapshot *SessionSnapshot) error {
	// Metrics already recorded at failure point, just cleanup
	lc.removeSessionLock(snapshot.SessionID)
	return nil
}

// OnProofTxError is called when a session fails due to proof transaction error.
func (lc *LifecycleCallback) OnProofTxError(_ context.Context, snapshot *SessionSnapshot) error {
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
