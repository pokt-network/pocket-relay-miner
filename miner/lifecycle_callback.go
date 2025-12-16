package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

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

// LifecycleCallback implements SessionLifecycleCallback to handle claim and proof submission.
// It coordinates SMST operations with transaction submission and uses proper timing spread.
type LifecycleCallback struct {
	logger         logging.Logger
	config         LifecycleCallbackConfig
	supplierClient pocktclient.SupplierClient
	sharedClient   pocktclient.SharedQueryClient
	blockClient        pocktclient.BlockClient
	sessionClient      SessionQueryClient
	smstManager        SMSTManager
	sessionCoordinator *SessionCoordinator

	// proofChecker determines if a proof is required for a claimed session.
	// If nil, proofs are always submitted (legacy behavior).
	proofChecker *ProofRequirementChecker

	// Per-session locks to prevent concurrent claim/proof operations
	sessionLocks   map[string]*sync.Mutex
	sessionLocksMu sync.Mutex
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

// getSessionLock returns a per-session lock.
func (lc *LifecycleCallback) getSessionLock(sessionID string) *sync.Mutex {
	lc.sessionLocksMu.Lock()
	defer lc.sessionLocksMu.Unlock()

	lock, exists := lc.sessionLocks[sessionID]
	if !exists {
		lock = &sync.Mutex{}
		lc.sessionLocks[sessionID] = lock
	}
	return lock
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

// OnSessionActive is called when a new session starts.
// For HA miner, sessions are created on-demand when relays arrive, so this is mostly informational.
func (lc *LifecycleCallback) OnSessionActive(ctx context.Context, snapshot *SessionSnapshot) error {
	lc.logger.Info().
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

	logger.Info().Msg("batched sessions need claims - starting claim process")

	// Get shared params
	sharedParams, err := lc.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	// Group sessions by session end height (they might have different claim windows)
	sessionsByEndHeight := make(map[int64][]*SessionSnapshot)
	for _, snapshot := range snapshots {
		sessionsByEndHeight[snapshot.SessionEndHeight] = append(sessionsByEndHeight[snapshot.SessionEndHeight], snapshot)
	}

	// Process each group (same claim window) separately
	allRootHashes := make([][]byte, len(snapshots))
	sessionIndex := 0

	for sessionEndHeight, groupSnapshots := range sessionsByEndHeight {
		// Wait for claim window to open and get the block hash for timing spread
		claimWindowOpenHeight := sharedtypes.GetClaimWindowOpenHeight(sharedParams, sessionEndHeight)
		logger.Info().
			Int64("claim_window_open_height", claimWindowOpenHeight).
			Int64("session_end_height", sessionEndHeight).
			Int("group_size", len(groupSnapshots)).
			Msg("waiting for claim window to open")

		claimWindowOpenBlock, blockErr := lc.waitForBlock(ctx, claimWindowOpenHeight)
		if blockErr != nil {
			return nil, fmt.Errorf("failed to wait for claim window open: %w", blockErr)
		}

		// Calculate the earliest claim commit height for this supplier (timing spread)
		earliestClaimHeight := sharedtypes.GetEarliestSupplierClaimCommitHeight(
			sharedParams,
			sessionEndHeight,
			claimWindowOpenBlock.Hash(),
			firstSnapshot.SupplierOperatorAddress,
		)

		logger.Info().
			Int64("earliest_claim_height", earliestClaimHeight).
			Int64("session_end_height", sessionEndHeight).
			Msg("waiting for assigned claim timing")

		// Wait for the earliest claim height (timing spread ensures suppliers don't all submit at once)
		if _, waitErr := lc.waitForBlock(ctx, earliestClaimHeight); waitErr != nil {
			return nil, fmt.Errorf("failed to wait for claim timing: %w", waitErr)
		}

		logger.Info().
			Int("group_size", len(groupSnapshots)).
			Msg("claim window timing reached - flushing SMSTs and submitting batched claims")

		// Build all claims for this group
		var claimMsgs []*prooftypes.MsgCreateClaim
		var groupRootHashes [][]byte

		for _, snapshot := range groupSnapshots {
			// CRITICAL: Never submit claims with 0 relays or 0 value - waste of fees
			if snapshot.RelayCount == 0 {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
					Msg("skipping claim - session has 0 relays")
				RecordSessionFailed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "zero_relays")
				continue // Skip this session
			}

			if snapshot.TotalComputeUnits == 0 {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
					Int64("relay_count", snapshot.RelayCount).
					Msg("skipping claim - session has 0 compute units despite having relays")
				RecordSessionFailed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "zero_value")
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

					RecordSessionFailed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "unprofitable")
					continue // Skip this session
				}
			}

			// Record the scheduled claim height for operators
			SetClaimScheduledHeight(snapshot.SupplierOperatorAddress, snapshot.SessionID, float64(earliestClaimHeight))

			// Flush the SMST to get the root hash
			rootHash, flushErr := lc.smstManager.FlushTree(ctx, snapshot.SessionID)
			if flushErr != nil {
				return nil, fmt.Errorf("failed to flush SMST for session %s: %w", snapshot.SessionID, flushErr)
			}
			groupRootHashes = append(groupRootHashes, rootHash)

			// Build the session header
			sessionHeader, headerErr := lc.buildSessionHeader(ctx, snapshot)
			if headerErr != nil {
				return nil, fmt.Errorf("failed to build session header for %s: %w", snapshot.SessionID, headerErr)
			}

			// Build claim message
			claimMsg := &prooftypes.MsgCreateClaim{
				SupplierOperatorAddress: snapshot.SupplierOperatorAddress,
				SessionHeader:           sessionHeader,
				RootHash:                rootHash,
			}
			claimMsgs = append(claimMsgs, claimMsg)
		}

		// Calculate timeout height (claim window close)
		claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(sharedParams, sessionEndHeight)

		// Convert to interface types for variadic call
		interfaceClaimMsgs := make([]pocktclient.MsgCreateClaim, len(claimMsgs))
		for i, msg := range claimMsgs {
			interfaceClaimMsgs[i] = msg
		}

		// Submit all claims in a single transaction with retries
		var lastErr error
		for attempt := 1; attempt <= lc.config.ClaimRetryAttempts; attempt++ {
			if submitErr := lc.supplierClient.CreateClaims(ctx, claimWindowClose, interfaceClaimMsgs...); submitErr != nil {
				lastErr = submitErr
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
				// Success
				currentBlock := lc.blockClient.LastBlock(ctx)
				blocksAfterWindowOpen := float64(currentBlock.Height() - claimWindowOpenHeight)

				// Record metrics for all sessions in the batch
				for i, snapshot := range groupSnapshots {
					RecordClaimSubmitted(snapshot.SupplierOperatorAddress)
					RecordClaimSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
					RecordRevenueClaimed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

					// Update snapshot manager
					if lc.sessionCoordinator != nil {
						if updateErr := lc.sessionCoordinator.OnSessionClaimed(ctx, snapshot.SessionID, groupRootHashes[i]); updateErr != nil {
							logger.Warn().
								Err(updateErr).
								Str("session_id", snapshot.SessionID).
								Msg("failed to update snapshot after claim")
						}
					}

					// Copy root hash to result (maintain order)
					allRootHashes[sessionIndex] = groupRootHashes[i]
					sessionIndex++
				}

				logger.Info().
					Int("batch_size", len(claimMsgs)).
					Int64("blocks_after_window", int64(blocksAfterWindowOpen)).
					Msg("batched claims submitted successfully")

				break // Success, exit retry loop
			}
		}

		if lastErr != nil {
			for _, snapshot := range groupSnapshots {
				RecordClaimError(snapshot.SupplierOperatorAddress, "exhausted_retries")
				RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "claim_exhausted_retries", snapshot.TotalComputeUnits, snapshot.RelayCount)
			}
			return nil, fmt.Errorf("batched claim submission failed after %d attempts: %w", lc.config.ClaimRetryAttempts, lastErr)
		}
	}

	return allRootHashes, nil
}

// OnSessionNeedsClaim is DEPRECATED - use OnSessionsNeedClaim instead.
// Kept for backward compatibility with old code paths.
func (lc *LifecycleCallback) OnSessionNeedsClaim(ctx context.Context, snapshot *SessionSnapshot) (rootHash []byte, err error) {
	lock := lc.getSessionLock(snapshot.SessionID)
	lock.Lock()
	defer lock.Unlock()

	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Int64(logging.FieldSessionEndHeight, snapshot.SessionEndHeight).Logger()

	logger.Info().
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("session needs claim - starting claim process")

	// Get shared params
	sharedParams, err := lc.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	// Wait for claim window to open and get the block hash for timing spread
	claimWindowOpenHeight := sharedtypes.GetClaimWindowOpenHeight(sharedParams, snapshot.SessionEndHeight)
	logger.Info().
		Int64("claim_window_open_height", claimWindowOpenHeight).
		Msg("waiting for claim window to open")

	claimWindowOpenBlock, err := lc.waitForBlock(ctx, claimWindowOpenHeight)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for claim window open: %w", err)
	}

	// Calculate the earliest claim commit height for this supplier (timing spread)
	earliestClaimHeight := sharedtypes.GetEarliestSupplierClaimCommitHeight(
		sharedParams,
		snapshot.SessionEndHeight,
		claimWindowOpenBlock.Hash(),
		snapshot.SupplierOperatorAddress,
	)

	// Record the scheduled claim height for operators
	SetClaimScheduledHeight(snapshot.SupplierOperatorAddress, snapshot.SessionID, float64(earliestClaimHeight))

	logger.Info().
		Int64("earliest_claim_height", earliestClaimHeight).
		Msg("waiting for assigned claim timing")

	// Wait for the earliest claim height (timing spread ensures suppliers don't all submit at once)
	if _, err := lc.waitForBlock(ctx, earliestClaimHeight); err != nil {
		return nil, fmt.Errorf("failed to wait for claim timing: %w", err)
	}

	logger.Info().Msg("claim window timing reached - flushing SMST and submitting claim")

	// Flush the SMST to get the root hash
	rootHash, err = lc.smstManager.FlushTree(ctx, snapshot.SessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to flush SMST: %w", err)
	}

	// Build the session header
	sessionHeader, err := lc.buildSessionHeader(ctx, snapshot)
	if err != nil {
		return nil, fmt.Errorf("failed to build session header: %w", err)
	}

	// Calculate timeout height (claim window close)
	claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(sharedParams, snapshot.SessionEndHeight)

	// Build and submit the claim with retries
	claimMsg := &prooftypes.MsgCreateClaim{
		SupplierOperatorAddress: snapshot.SupplierOperatorAddress,
		SessionHeader:           sessionHeader,
		RootHash:                rootHash,
	}

	var lastErr error
	for attempt := 1; attempt <= lc.config.ClaimRetryAttempts; attempt++ {
		if err := lc.supplierClient.CreateClaims(ctx, claimWindowClose, claimMsg); err != nil {
			lastErr = err
			logger.Warn().
				Err(err).
				Int(logging.FieldAttempt, attempt).
				Int(logging.FieldMaxRetry, lc.config.ClaimRetryAttempts).
				Msg("claim submission failed, retrying")

			if attempt < lc.config.ClaimRetryAttempts {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(lc.config.ClaimRetryDelay):
					continue
				}
			}
		} else {
			// Success
			currentBlock := lc.blockClient.LastBlock(ctx)
			blocksAfterWindowOpen := float64(currentBlock.Height() - claimWindowOpenHeight)

			RecordClaimSubmitted(snapshot.SupplierOperatorAddress)
			RecordClaimSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
			RecordRevenueClaimed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

			logger.Info().
				Int("root_hash_len", len(rootHash)).
				Int64("blocks_after_window", int64(blocksAfterWindowOpen)).
				Msg("claim submitted successfully")

			// Update snapshot manager
			if lc.sessionCoordinator != nil {
				if updateErr := lc.sessionCoordinator.OnSessionClaimed(ctx, snapshot.SessionID, rootHash); updateErr != nil {
					logger.Warn().Err(updateErr).Msg("failed to update snapshot after claim")
				}
			}

			return rootHash, nil
		}
	}

	RecordClaimError(snapshot.SupplierOperatorAddress, "exhausted_retries")
	RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "claim_exhausted_retries", snapshot.TotalComputeUnits, snapshot.RelayCount)
	return nil, fmt.Errorf("claim submission failed after %d attempts: %w", lc.config.ClaimRetryAttempts, lastErr)
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

	logger.Info().Msg("batched sessions need proofs - starting proof process")

	// Get shared params
	sharedParams, err := lc.sharedClient.GetParams(ctx)
	if err != nil {
		return fmt.Errorf("failed to get shared params: %w", err)
	}

	// Group sessions by session end height (they might have different proof windows)
	sessionsByEndHeight := make(map[int64][]*SessionSnapshot)
	for _, snapshot := range snapshots {
		sessionsByEndHeight[snapshot.SessionEndHeight] = append(sessionsByEndHeight[snapshot.SessionEndHeight], snapshot)
	}

	// Process each group (same proof window) separately
	for sessionEndHeight, groupSnapshots := range sessionsByEndHeight {
		// Wait for proof window to open
		proofWindowOpenHeight := sharedtypes.GetProofWindowOpenHeight(sharedParams, sessionEndHeight)
		logger.Info().
			Int64("proof_window_open_height", proofWindowOpenHeight).
			Int64("session_end_height", sessionEndHeight).
			Int("group_size", len(groupSnapshots)).
			Msg("waiting for proof window to open")

		proofWindowOpenBlock, blockErr := lc.waitForBlock(ctx, proofWindowOpenHeight)
		if blockErr != nil {
			return fmt.Errorf("failed to wait for proof window open: %w", blockErr)
		}

		// Filter sessions based on proof requirement (probabilistic proof selection)
		var sessionsNeedingProof []*SessionSnapshot
		for _, snapshot := range groupSnapshots {
			if lc.proofChecker != nil {
				required, checkErr := lc.proofChecker.IsProofRequired(ctx, snapshot, proofWindowOpenBlock.Hash())
				if checkErr != nil {
					logger.Warn().
						Err(checkErr).
						Str("session_id", snapshot.SessionID).
						Msg("failed to check proof requirement, submitting proof anyway to avoid potential penalty")
					sessionsNeedingProof = append(sessionsNeedingProof, snapshot)
				} else if !required {
					logger.Info().
						Str("session_id", snapshot.SessionID).
						Msg("proof NOT required for this claim - skipping proof submission")
					RecordRevenueProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)
				} else {
					logger.Info().
						Str("session_id", snapshot.SessionID).
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

		// Calculate the earliest proof commit height for this supplier (timing spread)
		earliestProofHeight := sharedtypes.GetEarliestSupplierProofCommitHeight(
			sharedParams,
			sessionEndHeight,
			proofWindowOpenBlock.Hash(),
			firstSnapshot.SupplierOperatorAddress,
		)

		logger.Info().
			Int64("earliest_proof_height", earliestProofHeight).
			Int64("session_end_height", sessionEndHeight).
			Int("proofs_to_submit", len(sessionsNeedingProof)).
			Msg("waiting for assigned proof timing")

		// Wait for the proof path seed block (one before earliest proof height)
		proofPathSeedBlockHeight := earliestProofHeight - 1
		proofPathSeedBlock, seedErr := lc.waitForBlock(ctx, proofPathSeedBlockHeight)
		if seedErr != nil {
			return fmt.Errorf("failed to wait for proof path seed block: %w", seedErr)
		}

		logger.Info().
			Int("group_size", len(sessionsNeedingProof)).
			Msg("proof window timing reached - generating and submitting batched proofs")

		// Build all proofs for this group
		var proofMsgs []*prooftypes.MsgSubmitProof

		for _, snapshot := range sessionsNeedingProof {
			// Record the scheduled proof height for operators
			SetProofScheduledHeight(snapshot.SupplierOperatorAddress, snapshot.SessionID, float64(earliestProofHeight))

			// Generate the proof path from the seed block hash
			path := protocol.GetPathForProof(proofPathSeedBlock.Hash(), snapshot.SessionID)

			// Generate the proof
			proofBytes, proofErr := lc.smstManager.ProveClosest(ctx, snapshot.SessionID, path)
			if proofErr != nil {
				return fmt.Errorf("failed to generate proof for session %s: %w", snapshot.SessionID, proofErr)
			}

			// Build the session header
			sessionHeader, headerErr := lc.buildSessionHeader(ctx, snapshot)
			if headerErr != nil {
				return fmt.Errorf("failed to build session header for %s: %w", snapshot.SessionID, headerErr)
			}

			// Build proof message
			proofMsg := &prooftypes.MsgSubmitProof{
				SupplierOperatorAddress: snapshot.SupplierOperatorAddress,
				SessionHeader:           sessionHeader,
				Proof:                   proofBytes,
			}
			proofMsgs = append(proofMsgs, proofMsg)
		}

		// Calculate timeout height (proof window close)
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(sharedParams, sessionEndHeight)

		// Convert to interface types for variadic call
		interfaceProofMsgs := make([]pocktclient.MsgSubmitProof, len(proofMsgs))
		for i, msg := range proofMsgs {
			interfaceProofMsgs[i] = msg
		}

		// Submit all proofs in a single transaction with retries
		var lastErr error
		for attempt := 1; attempt <= lc.config.ProofRetryAttempts; attempt++ {
			if submitErr := lc.supplierClient.SubmitProofs(ctx, proofWindowClose, interfaceProofMsgs...); submitErr != nil {
				lastErr = submitErr
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
				// Success
				currentBlock := lc.blockClient.LastBlock(ctx)
				blocksAfterWindowOpen := float64(currentBlock.Height() - proofWindowOpenHeight)

				// Record metrics for all sessions in the batch
				for _, snapshot := range sessionsNeedingProof {
					RecordProofSubmitted(snapshot.SupplierOperatorAddress)
					RecordProofSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
					RecordRevenueProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)
				}

				logger.Info().
					Int("batch_size", len(proofMsgs)).
					Int64("blocks_after_window", int64(blocksAfterWindowOpen)).
					Msg("batched proofs submitted successfully")

				break // Success, exit retry loop
			}
		}

		if lastErr != nil {
			for _, snapshot := range sessionsNeedingProof {
				RecordProofError(snapshot.SupplierOperatorAddress, "exhausted_retries")
				RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "proof_exhausted_retries", snapshot.TotalComputeUnits, snapshot.RelayCount)
			}
			return fmt.Errorf("batched proof submission failed after %d attempts: %w", lc.config.ProofRetryAttempts, lastErr)
		}
	}

	return nil
}

// OnSessionNeedsProof is DEPRECATED - use OnSessionsNeedProof instead.
// Kept for backward compatibility with old code paths.
func (lc *LifecycleCallback) OnSessionNeedsProof(ctx context.Context, snapshot *SessionSnapshot) error {
	lock := lc.getSessionLock(snapshot.SessionID)
	lock.Lock()
	defer lock.Unlock()

	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Int64(logging.FieldSessionEndHeight, snapshot.SessionEndHeight).Logger()

	logger.Info().Msg("session needs proof - starting proof process")

	// Get shared params
	sharedParams, err := lc.sharedClient.GetParams(ctx)
	if err != nil {
		return fmt.Errorf("failed to get shared params: %w", err)
	}

	// Wait for proof window to open
	proofWindowOpenHeight := sharedtypes.GetProofWindowOpenHeight(sharedParams, snapshot.SessionEndHeight)
	logger.Info().
		Int64("proof_window_open_height", proofWindowOpenHeight).
		Msg("waiting for proof window to open")

	proofWindowOpenBlock, err := lc.waitForBlock(ctx, proofWindowOpenHeight)
	if err != nil {
		return fmt.Errorf("failed to wait for proof window open: %w", err)
	}

	// CHECK IF PROOF IS REQUIRED (probabilistic proof selection)
	// This must be done after the proof window opens, as the block hash is used for the check
	if lc.proofChecker != nil {
		required, checkErr := lc.proofChecker.IsProofRequired(ctx, snapshot, proofWindowOpenBlock.Hash())
		if checkErr != nil {
			// If we fail to check, err on the side of caution and submit proof anyway
			// Missing a required proof incurs a 320 POKT penalty!
			logger.Warn().
				Err(checkErr).
				Msg("failed to check proof requirement, submitting proof anyway to avoid potential penalty")
		} else if !required {
			// Proof is NOT required - skip proof generation and submission
			logger.Info().
				Msg("proof NOT required for this claim (below threshold and not randomly selected) - skipping proof submission")

			// Still record compute units as settled since claim was accepted
			RecordRevenueProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

			return nil
		}
		// If required == true, continue with proof generation and submission
		logger.Info().Msg("proof IS required for this claim - proceeding with proof generation")
	}

	// Calculate the earliest proof commit height for this supplier (timing spread)
	earliestProofHeight := sharedtypes.GetEarliestSupplierProofCommitHeight(
		sharedParams,
		snapshot.SessionEndHeight,
		proofWindowOpenBlock.Hash(),
		snapshot.SupplierOperatorAddress,
	)

	// Record the scheduled proof height for operators
	SetProofScheduledHeight(snapshot.SupplierOperatorAddress, snapshot.SessionID, float64(earliestProofHeight))

	logger.Info().
		Int64("earliest_proof_height", earliestProofHeight).
		Msg("waiting for assigned proof timing")

	// Wait for the proof path seed block (one before earliest proof height)
	proofPathSeedBlockHeight := earliestProofHeight - 1
	proofPathSeedBlock, err := lc.waitForBlock(ctx, proofPathSeedBlockHeight)
	if err != nil {
		return fmt.Errorf("failed to wait for proof path seed block: %w", err)
	}

	logger.Info().Msg("proof window timing reached - generating and submitting proof")

	// Generate the proof path from the seed block hash
	path := protocol.GetPathForProof(proofPathSeedBlock.Hash(), snapshot.SessionID)

	// Generate the proof
	proofBytes, err := lc.smstManager.ProveClosest(ctx, snapshot.SessionID, path)
	if err != nil {
		return fmt.Errorf("failed to generate proof: %w", err)
	}

	// Build the session header
	sessionHeader, err := lc.buildSessionHeader(ctx, snapshot)
	if err != nil {
		return fmt.Errorf("failed to build session header: %w", err)
	}

	// Calculate timeout height (proof window close)
	proofWindowClose := sharedtypes.GetProofWindowCloseHeight(sharedParams, snapshot.SessionEndHeight)

	// Build and submit the proof with retries
	proofMsg := &prooftypes.MsgSubmitProof{
		SupplierOperatorAddress: snapshot.SupplierOperatorAddress,
		SessionHeader:           sessionHeader,
		Proof:                   proofBytes,
	}

	var lastErr error
	for attempt := 1; attempt <= lc.config.ProofRetryAttempts; attempt++ {
		if err := lc.supplierClient.SubmitProofs(ctx, proofWindowClose, proofMsg); err != nil {
			lastErr = err
			logger.Warn().
				Err(err).
				Int(logging.FieldAttempt, attempt).
				Int(logging.FieldMaxRetry, lc.config.ProofRetryAttempts).
				Msg("proof submission failed, retrying")

			if attempt < lc.config.ProofRetryAttempts {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(lc.config.ProofRetryDelay):
					continue
				}
			}
		} else {
			// Success
			currentBlock := lc.blockClient.LastBlock(ctx)
			blocksAfterWindowOpen := float64(currentBlock.Height() - proofWindowOpenHeight)

			RecordProofSubmitted(snapshot.SupplierOperatorAddress)
			RecordProofSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
			RecordRevenueProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

			logger.Info().
				Int("proof_len", len(proofBytes)).
				Int64("blocks_after_window", int64(blocksAfterWindowOpen)).
				Msg("proof submitted successfully")

			return nil
		}
	}

	RecordProofError(snapshot.SupplierOperatorAddress, "exhausted_retries")
	RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "proof_exhausted_retries", snapshot.TotalComputeUnits, snapshot.RelayCount)
	return fmt.Errorf("proof submission failed after %d attempts: %w", lc.config.ProofRetryAttempts, lastErr)
}

// OnSessionSettled is called when a session is fully settled.
// It cleans up resources associated with the session.
func (lc *LifecycleCallback) OnSessionSettled(ctx context.Context, snapshot *SessionSnapshot) error {
	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Logger()

	logger.Info().
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("session settled - cleaning up")

	// Record session settled metrics
	RecordSessionSettled(snapshot.SupplierOperatorAddress, snapshot.ServiceID)

	// Clean up session-specific metrics (gauges with session_id label)
	ClearSessionMetrics(snapshot.SupplierOperatorAddress, snapshot.SessionID, snapshot.ServiceID)

	// Clean up SMST
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		logger.Warn().Err(err).Msg("failed to delete SMST tree")
	}

	// Update snapshot manager
	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnSessionSettled(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to update snapshot after settlement")
		}
	}

	// Remove session lock
	lc.removeSessionLock(snapshot.SessionID)

	return nil
}

// OnSessionExpired is called when a session expires without settling.
// This typically means the claim or proof window was missed.
func (lc *LifecycleCallback) OnSessionExpired(ctx context.Context, snapshot *SessionSnapshot, reason string) error {
	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Logger()

	logger.Warn().
		Str(logging.FieldReason, reason).
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("session expired - rewards lost")

	// Record session failed metrics
	RecordSessionFailed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, reason)
	RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, reason, snapshot.TotalComputeUnits, snapshot.RelayCount)

	// Clean up session-specific metrics (gauges with session_id label)
	ClearSessionMetrics(snapshot.SupplierOperatorAddress, snapshot.SessionID, snapshot.ServiceID)

	// Clean up SMST
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		logger.Warn().Err(err).Msg("failed to delete SMST tree")
	}

	// Remove session lock
	lc.removeSessionLock(snapshot.SessionID)

	return nil
}

// waitForBlock waits for a specific block height to be reached.
func (lc *LifecycleCallback) waitForBlock(ctx context.Context, targetHeight int64) (pocktclient.Block, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			block := lc.blockClient.LastBlock(ctx)
			if block.Height() >= targetHeight {
				return block, nil
			}

			lc.logger.Debug().
				Int64("current_height", block.Height()).
				Int64("target_height", targetHeight).
				Msg("waiting for block height")
		}
	}
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
