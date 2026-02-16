package miner

import (
	"context"
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

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

	// Build the claim from the snapshot
	claim := claimFromSnapshot(snapshot)

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

// claimFromSnapshot builds a prooftypes.Claim from a SessionSnapshot.
// This allows us to use the claim's methods for calculating proof requirements.
func claimFromSnapshot(snapshot *SessionSnapshot) *prooftypes.Claim {
	return &prooftypes.Claim{
		SupplierOperatorAddress: snapshot.SupplierOperatorAddress,
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               snapshot.SessionID,
			ApplicationAddress:      snapshot.ApplicationAddress,
			ServiceId:               snapshot.ServiceID,
			SessionStartBlockHeight: snapshot.SessionStartHeight,
			SessionEndBlockHeight:   snapshot.SessionEndHeight,
		},
		RootHash: snapshot.ClaimedRootHash,
	}
}
