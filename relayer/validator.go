package relayer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/crypto"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// RelayValidator is responsible for validating relay requests.
// It verifies ring signatures and session validity using cached data
// to minimize on-chain queries.
type RelayValidator interface {
	// ValidateRelayRequest validates a relay request.
	// Returns nil if the request is valid, or an error describing the validation failure.
	ValidateRelayRequest(ctx context.Context, relayRequest *servicetypes.RelayRequest) error

	// CheckRewardEligibility checks if a relay is still eligible for rewards.
	// Returns nil if eligible, or an error if the relay is past the claim window.
	CheckRewardEligibility(ctx context.Context, relayRequest *servicetypes.RelayRequest) error

	// GetCurrentBlockHeight returns the current block height used for validation.
	GetCurrentBlockHeight() int64

	// SetCurrentBlockHeight updates the current block height.
	// This should be called when a new block is received.
	SetCurrentBlockHeight(height int64)
}

// ValidatorConfig contains configuration for the relay validator.
type ValidatorConfig struct {
	// AllowedSupplierAddresses is a list of supplier operator addresses
	// that this relayer is authorized to serve relays for.
	AllowedSupplierAddresses []string
}

// relayValidator implements RelayValidator.
type relayValidator struct {
	logger logging.Logger
	config *ValidatorConfig

	// ringClient is used for ring signature verification.
	ringClient crypto.RingClient

	// sessionCache is used for session lookups.
	sessionCache cache.SessionCache

	// sharedParamCache is used for shared parameter lookups.
	sharedParamCache cache.SharedParamCache

	// currentBlockHeight is the latest known block height.
	currentBlockHeight int64
	blockHeightMu      sync.RWMutex

	// allowedSuppliers is a set of allowed supplier operator addresses.
	allowedSuppliers map[string]struct{}
}

// NewRelayValidator creates a new relay validator.
func NewRelayValidator(
	logger logging.Logger,
	config *ValidatorConfig,
	ringClient crypto.RingClient,
	sessionCache cache.SessionCache,
	sharedParamCache cache.SharedParamCache,
) RelayValidator {
	allowedSuppliers := make(map[string]struct{})
	for _, addr := range config.AllowedSupplierAddresses {
		allowedSuppliers[addr] = struct{}{}
	}

	return &relayValidator{
		logger:           logging.ForComponent(logger, logging.ComponentRelayValidator),
		config:           config,
		ringClient:       ringClient,
		sessionCache:     sessionCache,
		sharedParamCache: sharedParamCache,
		allowedSuppliers: allowedSuppliers,
	}
}

// ValidateRelayRequest validates a relay request.
func (rv *relayValidator) ValidateRelayRequest(
	ctx context.Context,
	relayRequest *servicetypes.RelayRequest,
) error {
	// Basic validation
	step1 := time.Now()
	if err := relayRequest.ValidateBasic(); err != nil {
		return fmt.Errorf("basic validation failed: %w", err)
	}
	step1Duration := time.Since(step1)

	meta := relayRequest.GetMeta()
	sessionHeader := meta.GetSessionHeader()

	// Check if the supplier is allowed
	supplierAddr := meta.GetSupplierOperatorAddress()
	if _, ok := rv.allowedSuppliers[supplierAddr]; !ok && len(rv.allowedSuppliers) > 0 {
		return fmt.Errorf("supplier %s is not allowed by this relayer", supplierAddr)
	}

	// Get target session block height
	step2 := time.Now()
	sessionBlockHeight, err := rv.getTargetSessionBlockHeight(ctx, relayRequest)
	if err != nil {
		return fmt.Errorf("session timing validation failed: %w", err)
	}
	step2Duration := time.Since(step2)

	// Verify ring signature
	step3 := time.Now()
	if sigErr := rv.ringClient.VerifyRelayRequestSignature(ctx, relayRequest); sigErr != nil {
		return fmt.Errorf("ring signature verification failed: %w", sigErr)
	}
	step3Duration := time.Since(step3)

	// Verify session validity
	appAddress := sessionHeader.GetApplicationAddress()
	serviceID := sessionHeader.GetServiceId()

	step4 := time.Now()
	session, err := rv.sessionCache.GetSession(ctx, appAddress, serviceID, sessionBlockHeight)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	step4Duration := time.Since(step4)

	rv.logger.Debug().
		Dur("validate_basic", step1Duration).
		Dur("session_block_height", step2Duration).
		Dur("ring_signature", step3Duration).
		Dur("session_cache", step4Duration).
		Str(logging.FieldSessionID, sessionHeader.GetSessionId()).
		Msg("validation timing breakdown")

	// Verify session ID matches
	if session.SessionId != sessionHeader.GetSessionId() {
		return fmt.Errorf(
			"session ID mismatch, expected: %s, got: %s",
			session.SessionId,
			sessionHeader.GetSessionId(),
		)
	}

	// Verify supplier is in session
	supplierFound := false
	currentHeight := rv.GetCurrentBlockHeight()

	for _, supplier := range session.Suppliers {
		if supplier.OperatorAddress == supplierAddr {
			supplierFound = true
			bytes, _ := json.Marshal(session)
			rv.logger.Info().
				Str("supplier", supplier.OperatorAddress).
				Str("session_id", session.SessionId).
				Str("service_id", serviceID).
				Int64("session_height", sessionBlockHeight).
				Int64("current_height", currentHeight).
				Str("raw", string(bytes)).
				Msg("supplier found in session")
			break
		}
	}
	if !supplierFound {
		bytes, _ := json.Marshal(session)
		rv.logger.Info().
			Str("supplier", supplierAddr).
			Str("session_id", session.SessionId).
			Str("service_id", serviceID).
			Int64("session_height", sessionBlockHeight).
			Int64("current_height", currentHeight).
			Str("raw", string(bytes)).
			Msg("supplier not found in session")
		return fmt.Errorf("supplier %s not found in session", supplierAddr)
	}

	return nil
}

// CheckRewardEligibility checks if a relay is still eligible for rewards.
// Returns nil if eligible, or an error if the relay is past the acceptance deadline.
//
// CRITICAL TIMING: Relays must be rejected 1 block BEFORE grace period ends to allow
// processing time (validation → Redis publish → miner consume → SMST insert).
// The grace period is a PROCESSING BUFFER, not extended acceptance time.
func (rv *relayValidator) CheckRewardEligibility(
	ctx context.Context,
	relayRequest *servicetypes.RelayRequest,
) error {
	currentHeight := rv.GetCurrentBlockHeight()
	if currentHeight == 0 {
		// If we don't have block height info, assume it's eligible
		return nil
	}

	sharedParams, err := rv.sharedParamCache.GetLatestSharedParams(ctx)
	if err != nil {
		return fmt.Errorf("failed to get shared params: %w", err)
	}

	sessionStartHeight := relayRequest.Meta.SessionHeader.GetSessionStartBlockHeight()
	sessionEndHeight := relayRequest.Meta.SessionHeader.GetSessionEndBlockHeight()
	serviceID := relayRequest.Meta.SessionHeader.GetServiceId()
	applicationAddress := relayRequest.Meta.SessionHeader.GetApplicationAddress()

	gracePeriodEndOffset := int64(sharedParams.GetGracePeriodEndOffsetBlocks())

	// CRITICAL: Stop accepting 1 block BEFORE grace period ends to allow processing time
	// Pipeline: Validate → Redis → Miner → SMST can take 1-2 blocks
	gracePeriodLastAcceptBlock := sessionEndHeight + gracePeriodEndOffset - 1

	// REJECT: Relay arrived too late (at or after grace period processing buffer)
	if currentHeight >= gracePeriodLastAcceptBlock {
		rv.logger.Warn().
			Str("application", applicationAddress).
			Str("service_id", serviceID).
			Int64("session_start", sessionStartHeight).
			Int64("session_end", sessionEndHeight).
			Int64("current_height", currentHeight).
			Int64("grace_period_last_accept", gracePeriodLastAcceptBlock).
			Int64("blocks_late", currentHeight-gracePeriodLastAcceptBlock).
			Msg("LATE RELAY REJECTED: Application sent relay too late - relay arrived at/after grace period processing buffer. Relay will be rejected and NOT REWARDED. Application should send relays during session blocks, not grace period.")

		return fmt.Errorf(
			"relay too late: session ended at block %d, grace period processing buffer at block %d, current height %d (late by %d blocks) - relay will not be rewarded",
			sessionEndHeight,
			gracePeriodLastAcceptBlock,
			currentHeight,
			currentHeight-gracePeriodLastAcceptBlock,
		)
	}

	// WARN: Relay arrived in last block of session (risky - might not process in time)
	// This helps identify applications that are cutting it too close
	if currentHeight == sessionEndHeight {
		rv.logger.Warn().
			Str("application", applicationAddress).
			Str("service_id", serviceID).
			Int64("session_start", sessionStartHeight).
			Int64("session_end", sessionEndHeight).
			Int64("current_height", currentHeight).
			Int64("grace_period_last_accept", gracePeriodLastAcceptBlock).
			Msg("RISKY RELAY TIMING: Application sent relay in the LAST BLOCK of session. While accepted, this is risky due to processing latency. Application should send relays earlier in the session to ensure reliable processing.")
	}

	return nil
}

// GetCurrentBlockHeight returns the current block height.
func (rv *relayValidator) GetCurrentBlockHeight() int64 {
	rv.blockHeightMu.RLock()
	defer rv.blockHeightMu.RUnlock()
	return rv.currentBlockHeight
}

// SetCurrentBlockHeight updates the current block height.
func (rv *relayValidator) SetCurrentBlockHeight(height int64) {
	rv.blockHeightMu.Lock()
	defer rv.blockHeightMu.Unlock()
	rv.currentBlockHeight = height
}

// getTargetSessionBlockHeight determines the block height to use for session lookup.
// It handles grace period logic.
func (rv *relayValidator) getTargetSessionBlockHeight(
	ctx context.Context,
	relayRequest *servicetypes.RelayRequest,
) (int64, error) {
	sessionStartHeight := relayRequest.Meta.SessionHeader.GetSessionStartBlockHeight()
	sessionEndHeight := relayRequest.Meta.SessionHeader.GetSessionEndBlockHeight()
	currentHeight := rv.GetCurrentBlockHeight()

	// CRITICAL: For active sessions, use sessionStartHeight for cache consistency!
	// Session start height is constant for the session duration (~10 blocks),
	// while currentHeight changes every block, causing L1 cache misses.
	// This matches the logic in session_validator.go
	if currentHeight == 0 || sessionEndHeight >= currentHeight {
		// Session is active - use sessionStartHeight as canonical height
		return sessionStartHeight, nil
	}

	// Session has ended, check grace period
	sharedParams, err := rv.sharedParamCache.GetLatestSharedParams(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get shared params: %w", err)
	}

	// Check if still within grace period (using on-chain params only)
	// NOTE: grace_period_extra_blocks was removed - it created inconsistency between
	// getTargetSessionBlockHeight() and CheckRewardEligibility() causing relays to be
	// accepted but marked as ineligible for rewards.
	// 10 - 572340 - 572320
	if !sharedtypes.IsGracePeriodElapsed(sharedParams, sessionEndHeight, currentHeight) {
		// Within grace period, use session end height for lookup
		return sessionEndHeight, nil
	}

	return 0, fmt.Errorf(
		"session expired, session end height: %d, current height: %d (grace period elapsed)",
		sessionEndHeight,
		currentHeight,
	)
}
