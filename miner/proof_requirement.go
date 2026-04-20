package miner

import (
	"context"
	"errors"
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// ErrClaimedRootUnavailable is returned by IsProofRequired when the caller-
// supplied snapshot has no ClaimedRootHash and the SMSTManager (if wired)
// cannot rehydrate it — i.e. the session cannot be proven because there is
// no authoritative root to anchor the proof to. Callers MUST NOT fall open
// and submit a proof in this case: the resulting on-chain proof would be
// derived from a mismatched or absent root.
//
// This error primarily matters on the HA failover path, where
// OnSessionClaimed's Redis write can fail and is only logged at Warn —
// the next leader loads a snapshot whose ClaimedRootHash field is nil,
// and without this guard the probabilistic-proof check silently produces
// an invalid claim payload.
var ErrClaimedRootUnavailable = errors.New("claimed root hash unavailable for session")

// ClaimedRootProvider is the minimal seam used by ProofRequirementChecker to
// rehydrate a SessionSnapshot's ClaimedRootHash when the value stored in
// Redis is missing. It is satisfied by the full SMSTManager as well as by
// lightweight test doubles.
type ClaimedRootProvider interface {
	GetTreeRoot(ctx context.Context, sessionID string) (rootHash []byte, err error)
}

// ProofRequirementChecker determines if a proof is required for a claimed session.
// It implements the probabilistic proof requirement logic based on:
// 1. Threshold check: High-value claims always require proof
// 2. Probabilistic check: Random selection based on claim hash + block hash
//
// Reference implementation: pkg/relayer/session/proof.go:330-403
type ProofRequirementChecker struct {
	logger        logging.Logger
	proofClient   client.ProofQueryClient
	sharedClient  client.SharedQueryClient
	serviceClient query.ServiceDifficultyClient

	// rootProvider optionally rehydrates the claimed root from the live
	// SMST when the snapshot carries a nil/invalid ClaimedRootHash. Wired
	// via SetClaimedRootProvider; if nil, missing roots are fatal
	// (IsProofRequired returns ErrClaimedRootUnavailable rather than
	// fabricating a proof from a nil root).
	rootProvider ClaimedRootProvider
}

// NewProofRequirementChecker creates a new proof requirement checker.
func NewProofRequirementChecker(
	logger logging.Logger,
	proofClient client.ProofQueryClient,
	sharedClient client.SharedQueryClient,
	serviceClient query.ServiceDifficultyClient,
) *ProofRequirementChecker {
	return &ProofRequirementChecker{
		logger:        logging.ForComponent(logger, logging.ComponentProofChecker),
		proofClient:   proofClient,
		sharedClient:  sharedClient,
		serviceClient: serviceClient,
	}
}

// ServiceDifficultyClient exposes the wrapped height-aware difficulty client
// so other components (e.g. the economic viability check in LifecycleCallback)
// can reuse it without a second plumbing path.
func (c *ProofRequirementChecker) ServiceDifficultyClient() query.ServiceDifficultyClient {
	return c.serviceClient
}

// SetClaimedRootProvider wires an optional SMST root provider. When set,
// IsProofRequired will consult it to rehydrate a snapshot's missing
// ClaimedRootHash before building the claim. When unset, snapshots with a
// nil ClaimedRootHash cause IsProofRequired to return
// ErrClaimedRootUnavailable — the caller must NOT fall open to proof
// submission in that case.
func (c *ProofRequirementChecker) SetClaimedRootProvider(provider ClaimedRootProvider) {
	c.rootProvider = provider
}

// IsProofRequired determines if a proof is required for the given session's claim.
// It uses the on-chain proof parameters to make this determination:
// - If claimedAmount >= ProofRequirementThreshold → proof required (high-value claim)
// - If proofRequirementSampleValue <= ProofRequestProbability → proof required (random selection)
// - Otherwise → proof NOT required
//
// The proofRequirementSeedBlockHash should be the hash of the proof window open block.
func (c *ProofRequirementChecker) IsProofRequired(
	ctx context.Context,
	snapshot *SessionSnapshot,
	proofRequirementSeedBlockHash []byte,
) (isRequired bool, err error) {
	logger := c.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Str(logging.FieldServiceID, snapshot.ServiceID).Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).Logger()

	// Resolve the claimed root. Under HA failover OnSessionClaimed's Redis
	// write can fail (logged as Warn, not returned) and the next leader
	// sees snapshot.ClaimedRootHash == nil. Passing a nil root into
	// claim.GetClaimeduPOKT panics / corrupts the probabilistic proof
	// decision, so we rehydrate from the SMST when a root provider is
	// wired and surface ErrClaimedRootUnavailable otherwise.
	rootHash, rootErr := c.resolveClaimedRoot(ctx, snapshot)
	if rootErr != nil {
		return false, rootErr
	}

	// Build the claim from the snapshot using the validated root.
	claim := claimFromSnapshot(snapshot, rootHash)

	// Get proof module parameters
	proofParams, err := c.proofClient.GetParams(ctx)
	if err != nil {
		RecordProofRequirementCheckError(snapshot.SupplierOperatorAddress, "get_proof_params")
		return false, fmt.Errorf("failed to get proof params: %w", err)
	}

	// Get shared parameters
	sharedParams, err := c.sharedClient.GetParams(ctx)
	if err != nil {
		RecordProofRequirementCheckError(snapshot.SupplierOperatorAddress, "get_shared_params")
		return false, fmt.Errorf("failed to get shared params: %w", err)
	}

	// Get relay mining difficulty for the service at session start height
	relayMiningDifficulty, err := c.serviceClient.GetServiceRelayDifficultyAtHeight(ctx, snapshot.ServiceID, snapshot.SessionStartHeight)
	if err != nil {
		RecordProofRequirementCheckError(snapshot.SupplierOperatorAddress, "get_relay_difficulty")
		return false, fmt.Errorf("failed to get relay mining difficulty for service %s: %w", snapshot.ServiceID, err)
	}

	// Calculate the claimed amount in uPOKT
	claimedAmount, err := claim.GetClaimeduPOKT(*sharedParams, relayMiningDifficulty)
	if err != nil {
		RecordProofRequirementCheckError(snapshot.SupplierOperatorAddress, "calculate_claimed_amount")
		return false, fmt.Errorf("failed to calculate claimed uPOKT: %w", err)
	}

	// Record the check
	RecordProofRequirementCheck(snapshot.SupplierOperatorAddress)

	// THRESHOLD CHECK: High-value claims always require proof
	// This protects against large fraudulent claims
	proofThreshold := proofParams.GetProofRequirementThreshold()
	if claimedAmount.Amount.GTE(proofThreshold.Amount) {
		logger.Info().
			Str("claimed_amount", claimedAmount.String()).
			Str("threshold", proofThreshold.String()).
			Msg("proof required: claim exceeds threshold")

		RecordProofRequirementRequired(snapshot.SupplierOperatorAddress, "threshold")
		return true, nil
	}

	// PROBABILISTIC CHECK: Random selection based on claim hash + block hash
	// This ensures a percentage of claims are randomly selected for proof
	proofRequirementSampleValue, err := claim.GetProofRequirementSampleValue(proofRequirementSeedBlockHash)
	if err != nil {
		RecordProofRequirementCheckError(snapshot.SupplierOperatorAddress, "calculate_sample_value")
		return false, fmt.Errorf("failed to calculate proof requirement sample value: %w", err)
	}

	proofRequestProbability := proofParams.GetProofRequestProbability()

	// A random value between 0 and 1 will be less than or equal to proof_request_probability
	// with probability equal to the proof_request_probability (e.g., 25% default)
	if proofRequirementSampleValue <= proofRequestProbability {
		logger.Info().
			Float64("sample_value", proofRequirementSampleValue).
			Float64("probability", proofRequestProbability).
			Msg("proof required: randomly selected")

		RecordProofRequirementRequired(snapshot.SupplierOperatorAddress, "probabilistic")
		return true, nil
	}

	// NO PROOF REQUIRED
	logger.Info().
		Str("claimed_amount", claimedAmount.String()).
		Str("threshold", proofThreshold.String()).
		Float64("sample_value", proofRequirementSampleValue).
		Float64("probability", proofRequestProbability).
		Msg("proof NOT required: below threshold and not randomly selected")

	RecordProofRequirementSkipped(snapshot.SupplierOperatorAddress)
	return false, nil
}

// resolveClaimedRoot returns a non-nil claimed root for the session or
// ErrClaimedRootUnavailable. It prefers the value stored on the snapshot
// (fast path) and falls back to the injected ClaimedRootProvider (HA
// failover path) when the snapshot carries nothing. A nil/empty root at
// the end of this function means the session is unprovable and the caller
// must NOT submit a proof built from fabricated data.
func (c *ProofRequirementChecker) resolveClaimedRoot(
	ctx context.Context,
	snapshot *SessionSnapshot,
) ([]byte, error) {
	if len(snapshot.ClaimedRootHash) > 0 {
		return snapshot.ClaimedRootHash, nil
	}

	logger := c.logger.With().
		Str(logging.FieldSessionID, snapshot.SessionID).
		Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
		Logger()

	if c.rootProvider == nil {
		logger.Error().Msg("snapshot has nil ClaimedRootHash and no SMST rehydration source configured")
		return nil, fmt.Errorf("%w: session %s", ErrClaimedRootUnavailable, snapshot.SessionID)
	}

	rehydrated, err := c.rootProvider.GetTreeRoot(ctx, snapshot.SessionID)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to rehydrate claimed root from SMST")
		return nil, fmt.Errorf("%w: %v", ErrClaimedRootUnavailable, err)
	}
	if len(rehydrated) == 0 {
		logger.Warn().Msg("SMST has no root for session; cannot rehydrate claimed root")
		return nil, fmt.Errorf("%w: session %s", ErrClaimedRootUnavailable, snapshot.SessionID)
	}

	// Backfill the snapshot so downstream consumers in the same request
	// chain (e.g. proof submission) see the rehydrated root without a
	// second round-trip. We deliberately do NOT persist this back to
	// Redis here — that is OnSessionClaimed's job, and writing a root
	// from the read path would mask the upstream failure we want
	// visibility into.
	snapshot.ClaimedRootHash = rehydrated
	logger.Info().
		Int("root_len", len(rehydrated)).
		Msg("rehydrated claimed root from SMST after missing snapshot field")
	return rehydrated, nil
}

// claimFromSnapshot builds a prooftypes.Claim from a SessionSnapshot and an
// explicit, non-nil claimed root hash. The root is passed as a separate
// argument rather than read off the snapshot so callers are forced to
// think about the HA failover path where snapshot.ClaimedRootHash can be
// nil (see resolveClaimedRoot). Passing a nil rootHash here would
// reintroduce the very bug this signature exists to prevent.
func claimFromSnapshot(snapshot *SessionSnapshot, rootHash []byte) *prooftypes.Claim {
	return &prooftypes.Claim{
		SupplierOperatorAddress: snapshot.SupplierOperatorAddress,
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               snapshot.SessionID,
			ApplicationAddress:      snapshot.ApplicationAddress,
			ServiceId:               snapshot.ServiceID,
			SessionStartBlockHeight: snapshot.SessionStartHeight,
			SessionEndBlockHeight:   snapshot.SessionEndHeight,
		},
		RootHash: rootHash,
	}
}
