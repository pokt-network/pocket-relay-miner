package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// SubmissionTrackingRecord tracks claim/proof submission attempts for debugging.
// Stored in Redis with configurable TTL (default: 24h) to enable post-mortem analysis.
type SubmissionTrackingRecord struct {
	// Session identification
	Supplier     string `json:"supplier"`
	Service      string `json:"service"`
	Application  string `json:"application"`
	SessionID    string `json:"session_id"`
	SessionStart int64  `json:"session_start"`
	SessionEnd   int64  `json:"session_end"`

	// Claim tracking
	ClaimHash            string `json:"claim_hash"`
	ClaimTxHash          string `json:"claim_tx_hash"`
	ClaimSuccess         bool   `json:"claim_success"`
	ClaimErrorReason     string `json:"claim_error_reason,omitempty"`
	ClaimSubmitHeight    int64  `json:"claim_submit_height"`
	ClaimSubmitTimestamp int64  `json:"claim_submit_timestamp"` // Unix timestamp
	ClaimSubmitTimeUTC   string `json:"claim_submit_time_utc"`  // RFC3339 UTC time
	ClaimCurrentHeight   int64  `json:"claim_current_height"`   // Current block at submission

	// Proof tracking
	ProofHash            string `json:"proof_hash,omitempty"`
	ProofTxHash          string `json:"proof_tx_hash,omitempty"`
	ProofSuccess         bool   `json:"proof_success"`
	ProofErrorReason     string `json:"proof_error_reason,omitempty"`
	ProofSubmitHeight    int64  `json:"proof_submit_height,omitempty"`
	ProofSubmitTimestamp int64  `json:"proof_submit_timestamp,omitempty"` // Unix timestamp
	ProofSubmitTimeUTC   string `json:"proof_submit_time_utc,omitempty"`  // RFC3339 UTC time
	ProofCurrentHeight   int64  `json:"proof_current_height,omitempty"`   // Current block at submission

	// Metadata
	NumRelays            int64  `json:"num_relays"`
	ComputeUnits         int64  `json:"compute_units"`
	ProofRequired        bool   `json:"proof_required"`
	ProofRequirementSeed string `json:"proof_requirement_seed,omitempty"` // Hex-encoded seed block hash
}

// SubmissionTracker tracks claim/proof submissions to Redis for debugging.
type SubmissionTracker struct {
	logger      logging.Logger
	redisClient *redistransport.Client
	ttl         time.Duration
}

// NewSubmissionTracker creates a new submission tracker.
// ttl specifies how long submission records are kept in Redis for debugging.
func NewSubmissionTracker(logger logging.Logger, redisClient *redistransport.Client, ttl time.Duration) *SubmissionTracker {
	if ttl <= 0 {
		ttl = 24 * time.Hour // Default: 24 hours
	}
	return &SubmissionTracker{
		logger:      logging.ForComponent(logger, "submission_tracker"),
		redisClient: redisClient,
		ttl:         ttl,
	}
}

// TrackClaimSubmission records a claim submission attempt.
func (t *SubmissionTracker) TrackClaimSubmission(
	ctx context.Context,
	supplier string,
	service string,
	application string,
	sessionID string,
	sessionStart int64,
	sessionEnd int64,
	claimHash string,
	claimTxHash string,
	success bool,
	errorReason string,
	submitHeight int64,
	currentHeight int64,
	numRelays int64,
	computeUnits int64,
	proofRequired bool,
	proofRequirementSeed string,
) error {
	key := t.makeKey(supplier, sessionEnd, sessionID)

	now := time.Now()
	record := SubmissionTrackingRecord{
		Supplier:             supplier,
		Service:              service,
		Application:          application,
		SessionID:            sessionID,
		SessionStart:         sessionStart,
		SessionEnd:           sessionEnd,
		ClaimHash:            claimHash,
		ClaimTxHash:          claimTxHash,
		ClaimSuccess:         success,
		ClaimErrorReason:     errorReason,
		ClaimSubmitHeight:    submitHeight,
		ClaimSubmitTimestamp: now.Unix(),
		ClaimSubmitTimeUTC:   now.UTC().Format(time.RFC3339),
		ClaimCurrentHeight:   currentHeight,
		NumRelays:            numRelays,
		ComputeUnits:         computeUnits,
		ProofRequired:        proofRequired,
		ProofRequirementSeed: proofRequirementSeed,
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal tracking record: %w", err)
	}

	if err := t.redisClient.Set(ctx, key, data, t.ttl).Err(); err != nil {
		return fmt.Errorf("failed to store tracking record: %w", err)
	}

	t.logger.Debug().
		Str("supplier", supplier).
		Str("session_id", sessionID).
		Bool("success", success).
		Msg("tracked claim submission")

	return nil
}

// TrackProofSubmission updates the record with proof submission details.
func (t *SubmissionTracker) TrackProofSubmission(
	ctx context.Context,
	supplier string,
	sessionEnd int64,
	sessionID string,
	proofHash string,
	proofTxHash string,
	success bool,
	errorReason string,
	submitHeight int64,
	currentHeight int64,
	proofRequired bool,
	proofRequirementSeed string,
) error {
	key := t.makeKey(supplier, sessionEnd, sessionID)

	// Get existing record
	data, err := t.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		// If record doesn't exist, create minimal one (shouldn't happen normally)
		t.logger.Warn().
			Str("supplier", supplier).
			Str("session_id", sessionID).
			Msg("proof tracking record not found, creating new one")

		now := time.Now()
		record := SubmissionTrackingRecord{
			Supplier:             supplier,
			SessionID:            sessionID,
			SessionEnd:           sessionEnd,
			ProofHash:            proofHash,
			ProofTxHash:          proofTxHash,
			ProofSuccess:         success,
			ProofErrorReason:     errorReason,
			ProofSubmitHeight:    submitHeight,
			ProofSubmitTimestamp: now.Unix(),
			ProofSubmitTimeUTC:   now.UTC().Format(time.RFC3339),
			ProofCurrentHeight:   currentHeight,
		}

		newData, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			return fmt.Errorf("failed to marshal new tracking record: %w", marshalErr)
		}

		if setErr := t.redisClient.Set(ctx, key, newData, t.ttl).Err(); setErr != nil {
			return fmt.Errorf("failed to store new tracking record: %w", setErr)
		}

		return nil
	}

	// Update existing record
	var record SubmissionTrackingRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("failed to unmarshal tracking record: %w", err)
	}

	now := time.Now()
	record.ProofHash = proofHash
	record.ProofTxHash = proofTxHash
	record.ProofSuccess = success
	record.ProofErrorReason = errorReason
	record.ProofSubmitHeight = submitHeight
	record.ProofSubmitTimestamp = now.Unix()
	record.ProofSubmitTimeUTC = now.UTC().Format(time.RFC3339)
	record.ProofCurrentHeight = currentHeight
	record.ProofRequired = proofRequired
	record.ProofRequirementSeed = proofRequirementSeed

	updatedData, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal updated tracking record: %w", err)
	}

	if err := t.redisClient.Set(ctx, key, updatedData, t.ttl).Err(); err != nil {
		return fmt.Errorf("failed to update tracking record: %w", err)
	}

	t.logger.Debug().
		Str("supplier", supplier).
		Str("session_id", sessionID).
		Bool("success", success).
		Msg("tracked proof submission")

	return nil
}

// GetRecord retrieves a tracking record.
func (t *SubmissionTracker) GetRecord(ctx context.Context, supplier string, sessionEnd int64, sessionID string) (*SubmissionTrackingRecord, error) {
	key := t.makeKey(supplier, sessionEnd, sessionID)

	data, err := t.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		return nil, fmt.Errorf("failed to get tracking record: %w", err)
	}

	var record SubmissionTrackingRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal tracking record: %w", err)
	}

	return &record, nil
}

// ListRecordsForSupplier returns all tracking records for a supplier.
func (t *SubmissionTracker) ListRecordsForSupplier(ctx context.Context, supplier string) ([]*SubmissionTrackingRecord, error) {
	pattern := fmt.Sprintf("ha:tx:track:%s:*", supplier)

	keys, err := t.redisClient.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list keys: %w", err)
	}

	var records []*SubmissionTrackingRecord
	for _, key := range keys {
		data, getErr := t.redisClient.Get(ctx, key).Bytes()
		if getErr != nil {
			t.logger.Warn().Err(getErr).Str("key", key).Msg("failed to get record")
			continue
		}

		var record SubmissionTrackingRecord
		if unmarshalErr := json.Unmarshal(data, &record); unmarshalErr != nil {
			t.logger.Warn().Err(unmarshalErr).Str("key", key).Msg("failed to unmarshal record")
			continue
		}

		records = append(records, &record)
	}

	return records, nil
}

// makeKey generates the Redis key for a tracking record.
// Format: ha:tx:track:{supplier}:{sessionEndHeight}:{sessionID}
func (t *SubmissionTracker) makeKey(supplier string, sessionEnd int64, sessionID string) string {
	return fmt.Sprintf("ha:tx:track:%s:%d:%s", supplier, sessionEnd, sessionID)
}
