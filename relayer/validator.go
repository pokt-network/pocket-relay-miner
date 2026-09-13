package relayer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/crypto"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// ErrSessionExpired is returned by getTargetSessionBlockHeight when a relay
// arrives after its session's grace period has elapsed. Wrapped with %w up to
// the transport-level rejection classifier, so it can be told apart from a
// generic validation failure without matching on error text.
var ErrSessionExpired = errors.New("session expired")

// RelayValidator is responsible for validating relay requests.
// It verifies ring signatures and session validity using cached data
// to minimize on-chain queries.
type RelayValidator interface {
	// ValidateRelayRequest validates a relay request.
	// Returns nil if the request is valid, or an error describing the validation failure.
	ValidateRelayRequest(ctx context.Context, relayRequest *servicetypes.RelayRequest) error

	// GetCurrentBlockHeight returns the current block height used for validation.
	GetCurrentBlockHeight() int64

	// SetCurrentBlockHeight updates the current block height.
	// This should be called when a new block is received.
	SetCurrentBlockHeight(height int64)
}

// ValidatorConfig contains configuration for the relay validator.
type ValidatorConfig struct {
	// OwnsSupplierKey reports whether this relayer holds the signing key for a
	// supplier operator address, and so is authorized to serve relays for it.
	//
	// A function and not a list of addresses: the key set changes while the
	// process runs (hot reload of the signing keys), and a slice copied at
	// startup would keep rejecting a supplier whose key was added afterwards --
	// removals would take effect and additions would not, which is a worse
	// state than no hot reload at all. cmd_relayer wires this to
	// ResponseSigner.HasSigner, which reads the live key set.
	//
	// Nil means "serve any supplier", which is what an empty address list meant
	// before: the gate is off, and the signing key itself is the backstop.
	OwnsSupplierKey func(operatorAddr string) bool
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

	// ownsSupplierKey is the live check for "is this relayer authorized to
	// serve this supplier". See ValidatorConfig.OwnsSupplierKey.
	ownsSupplierKey func(operatorAddr string) bool
}

// NewRelayValidator creates a new relay validator.
func NewRelayValidator(
	logger logging.Logger,
	config *ValidatorConfig,
	ringClient crypto.RingClient,
	sessionCache cache.SessionCache,
	sharedParamCache cache.SharedParamCache,
) RelayValidator {
	return &relayValidator{
		logger:           logging.ForComponent(logger, logging.ComponentRelayValidator),
		config:           config,
		ringClient:       ringClient,
		sessionCache:     sessionCache,
		sharedParamCache: sharedParamCache,
		ownsSupplierKey:  config.OwnsSupplierKey,
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
	if rv.ownsSupplierKey != nil && !rv.ownsSupplierKey(supplierAddr) {
		return fmt.Errorf("supplier %s is not allowed by this relayer", supplierAddr)
	}

	// Cheap, query-free bound on the client-supplied session heights BEFORE the
	// first at-height chain read.
	//
	// getTargetSessionBlockHeight (immediately below) resolves shared params at the
	// client's session END height on the grace-period branch, and that happens
	// BEFORE the ring signature is verified further down. Every transport reaches
	// this function — HTTP re-checks earlier still, ahead of its eager meter
	// (proxy.go handleRelay), but gRPC (relay_grpc_service.go) and WebSocket
	// (websocket.go) only pass through here — so the bound belongs here too, or
	// those two transports keep the pre-signature amplification surface.
	//
	// rv.GetCurrentBlockHeight() rather than a caller-supplied arrival height: the
	// WebSocket bridge pins its arrival height at connection setup and never
	// refreshes it, so a long-lived connection would drift out of the band.
	if sessionHeader != nil {
		if !sessionHeightsPlausible(
			sessionHeader.GetSessionStartBlockHeight(),
			sessionHeader.GetSessionEndBlockHeight(),
			rv.GetCurrentBlockHeight(),
		) {
			return fmt.Errorf(
				"implausible session heights: start %d, end %d (current height %d)",
				sessionHeader.GetSessionStartBlockHeight(),
				sessionHeader.GetSessionEndBlockHeight(),
				rv.GetCurrentBlockHeight(),
			)
		}
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

	// Verify the client-supplied session header matches the on-chain one FIELD BY
	// FIELD, not just by session ID.
	//
	// service_id and application_address drive the lookup above, but
	// session_start_block_height and session_end_block_height are client-supplied and
	// were previously unverified. Because getTargetSessionBlockHeight returns the
	// CLAIMED start height for an active session, a client could supply any height
	// inside the real session's range, pass the ID check, and have the forged height
	// flow into the difficulty target and MinedRelayMessage.SessionStartHeight — and,
	// from v0.1.35, into the CUPR the chain prices the claim with.
	//
	// At proof time the chain re-compares the sampled relay's header against the
	// claim's (x/proof compareSessionHeaders). A forged relay sampled from the tree
	// yields ErrProofInvalidRelay -> invalid proof -> the supplier is SLASHED.
	//
	// Costs no extra query: the on-chain session is already fetched above.
	if err := compareSessionHeaders(session.GetHeader(), sessionHeader); err != nil {
		return err
	}

	// Verify supplier is in session
	supplierFound := false
	currentHeight := rv.GetCurrentBlockHeight()

	for _, supplier := range session.Suppliers {
		if supplier.OperatorAddress == supplierAddr {
			supplierFound = true
			rv.logger.Debug().
				Str("supplier", supplier.OperatorAddress).
				Str("session_id", session.SessionId).
				Str("service_id", serviceID).
				Str("application", appAddress).
				Int64("session_height", sessionBlockHeight).
				Int64("current_height", currentHeight).
				Msg("supplier found in session")
			break
		}
	}
	if !supplierFound {
		rv.logger.Debug().
			Str("supplier", supplierAddr).
			Str("application", appAddress).
			Str("session_id", session.SessionId).
			Str("service_id", serviceID).
			Int64("session_height", sessionBlockHeight).
			Int64("current_height", currentHeight).
			Msg("supplier not found in session")
		return fmt.Errorf("supplier %s not found in session", supplierAddr)
	}

	return nil
}

// compareSessionHeaders verifies every field of a client-supplied session header
// against the on-chain session's header. Ported from poktroll's
// pkg/relayer/relay_authenticator/relay_verifier.go so this relayer rejects a
// forged header at ingest rather than mining it into the tree, where it becomes a
// slashable invalid relay at proof time.
//
// The session ID is compared last, and it is the ONLY session-ID equality check
// in this relayer — unlike poktroll's relay_authenticator (the source of this
// port), there is no separate upstream "is your full node in sync" check here, so
// this comparison must not be removed on the assumption that one exists.
func compareSessionHeaders(onchainSessionHeader, requestSessionHeader *sessiontypes.SessionHeader) error {
	if onchainSessionHeader == nil {
		return fmt.Errorf("onchain session header is nil")
	}

	if requestSessionHeader.GetApplicationAddress() != onchainSessionHeader.GetApplicationAddress() {
		return fmt.Errorf(
			"session header application address mismatch, expecting: %q, got: %q",
			onchainSessionHeader.GetApplicationAddress(),
			requestSessionHeader.GetApplicationAddress(),
		)
	}

	if requestSessionHeader.GetServiceId() != onchainSessionHeader.GetServiceId() {
		return fmt.Errorf(
			"session header service ID mismatch, expecting: %q, got: %q",
			onchainSessionHeader.GetServiceId(),
			requestSessionHeader.GetServiceId(),
		)
	}

	if requestSessionHeader.GetSessionStartBlockHeight() != onchainSessionHeader.GetSessionStartBlockHeight() {
		return fmt.Errorf(
			"session header session start height mismatch, expecting: %d, got: %d",
			onchainSessionHeader.GetSessionStartBlockHeight(),
			requestSessionHeader.GetSessionStartBlockHeight(),
		)
	}

	if requestSessionHeader.GetSessionEndBlockHeight() != onchainSessionHeader.GetSessionEndBlockHeight() {
		return fmt.Errorf(
			"session header session end height mismatch, expecting: %d, got: %d",
			onchainSessionHeader.GetSessionEndBlockHeight(),
			requestSessionHeader.GetSessionEndBlockHeight(),
		)
	}

	if requestSessionHeader.GetSessionId() != onchainSessionHeader.GetSessionId() {
		return fmt.Errorf(
			"session ID mismatch, expected: %s, got: %s",
			onchainSessionHeader.GetSessionId(),
			requestSessionHeader.GetSessionId(),
		)
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

	// Session has ended, check grace period.
	//
	// grace_period_end_offset_blocks is session TIMING, so it resolves at the
	// session END height, mirroring the chain (x/session's hydrator and x/proof
	// both resolve it at-height) and poktroll's getTargetSessionBlockHeight. A
	// live read here would disagree with itself the moment governance moves the
	// offset: this same at-height value is what the chain measured the session
	// against, not whatever the offset is today.
	sharedParams, err := rv.sharedParamCache.GetSharedParams(ctx, sessionEndHeight)
	if err != nil {
		return 0, fmt.Errorf("failed to get shared params: %w", err)
	}

	// Check if still within grace period (using on-chain params only)
	// NOTE: grace_period_extra_blocks was removed - it created a second,
	// disagreeing resolution of the same params epoch.
	if !sharedtypes.IsGracePeriodElapsed(sharedParams, sessionEndHeight, currentHeight) {
		// Within grace period, use session end height for lookup
		return sessionEndHeight, nil
	}

	return 0, fmt.Errorf(
		"%w, session end height: %d, current height: %d (grace period elapsed)",
		ErrSessionExpired,
		sessionEndHeight,
		currentHeight,
	)
}
