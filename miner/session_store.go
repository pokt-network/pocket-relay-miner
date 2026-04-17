package miner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// SessionState represents the lifecycle state of a session.
type SessionState string

const (
	// Active lifecycle
	// SessionStateActive means the session is accepting relays.
	SessionStateActive SessionState = "active"

	// SessionStateClaiming means the session has been flushed and is waiting for claim submission.
	SessionStateClaiming SessionState = "claiming"

	// Claim outcomes
	// SessionStateClaimed means the claim has been successfully submitted.
	SessionStateClaimed SessionState = "claimed"

	// SessionStateClaimWindowClosed means the claim window closed before the claim was submitted.
	SessionStateClaimWindowClosed SessionState = "claim_window_closed"

	// SessionStateClaimTxError means the claim transaction failed (RPC error, gas, signature, etc).
	SessionStateClaimTxError SessionState = "claim_tx_error"

	// SessionStateClaimSkipped means we intentionally did NOT submit a claim
	// for this session because the economic viability check rejected it
	// (expected reward < claim_fee + proof_fee). This is a terminal, non-
	// failure state — the session ended cleanly by operator decision, not
	// because the pipeline broke.
	SessionStateClaimSkipped SessionState = "claim_skipped"

	// Proof outcomes (only from claimed state)
	// SessionStateProving means the session is in the proof submission window.
	SessionStateProving SessionState = "proving"

	// SessionStateProved means the proof transaction was successfully submitted.
	SessionStateProved SessionState = "proved"

	// SessionStateProbabilisticProved means proof was not required and the claim will be settled by protocol.
	SessionStateProbabilisticProved SessionState = "probabilistic_proved"

	// SessionStateProofWindowClosed means the proof window closed before the proof was submitted.
	SessionStateProofWindowClosed SessionState = "proof_window_closed"

	// SessionStateProofTxError means the proof transaction failed (RPC error, gas, etc).
	SessionStateProofTxError SessionState = "proof_tx_error"
)

// IsTerminal returns true if the state is a terminal state (no further transitions).
func (s SessionState) IsTerminal() bool {
	switch s {
	case SessionStateProved,
		SessionStateProbabilisticProved,
		SessionStateClaimWindowClosed,
		SessionStateClaimTxError,
		SessionStateClaimSkipped,
		SessionStateProofWindowClosed,
		SessionStateProofTxError:
		return true
	default:
		return false
	}
}

// IsSuccess returns true if the state represents a successful terminal outcome.
func (s SessionState) IsSuccess() bool {
	switch s {
	case SessionStateProved, SessionStateProbabilisticProved:
		return true
	default:
		return false
	}
}

// IsFailure returns true if the state represents a failure outcome.
func (s SessionState) IsFailure() bool {
	switch s {
	case SessionStateClaimWindowClosed, SessionStateClaimTxError,
		SessionStateProofWindowClosed, SessionStateProofTxError:
		return true
	default:
		return false
	}
}

// SessionSnapshot captures the state of a session for HA recovery.
// This is stored in Redis and used by a new leader to recover session state.
type SessionSnapshot struct {
	// SessionID is the unique identifier for this session.
	SessionID string `json:"session_id"`

	// SupplierOperatorAddress is the supplier this session belongs to.
	SupplierOperatorAddress string `json:"supplier_operator_address"`

	// ServiceID is the service this session is for.
	ServiceID string `json:"service_id"`

	// ApplicationAddress is the application this session is with.
	ApplicationAddress string `json:"application_address"`

	// SessionStartHeight is the block height when the session started.
	SessionStartHeight int64 `json:"session_start_height"`

	// SessionEndHeight is the block height when the session ends.
	SessionEndHeight int64 `json:"session_end_height"`

	// State is the current lifecycle state of the session.
	State SessionState `json:"state"`

	// RelayCount is the number of relays processed in this session.
	RelayCount int64 `json:"relay_count"`

	// TotalComputeUnits is the sum of compute units for all relays.
	TotalComputeUnits uint64 `json:"total_compute_units"`

	// ClaimedRootHash is the SMST root hash (set after flush).
	ClaimedRootHash []byte `json:"claimed_root_hash,omitempty"`

	// ClaimTxHash is the transaction hash of the submitted claim (for deduplication).
	ClaimTxHash string `json:"claim_tx_hash,omitempty"`

	// ProofTxHash is the transaction hash of the submitted proof (for deduplication).
	ProofTxHash string `json:"proof_tx_hash,omitempty"`

	// LastWALEntryID is the last WAL entry ID that was processed.
	// Used for recovery to know where to start replaying from.
	LastWALEntryID string `json:"last_wal_entry_id,omitempty"`

	// LastUpdatedAt is when the snapshot was last updated.
	LastUpdatedAt time.Time `json:"last_updated_at"`

	// CreatedAt is when the session was created.
	CreatedAt time.Time `json:"created_at"`

	// Optional on-chain settlement confirmation (hours after session completes)
	// Populated by settlement monitor when EventClaimSettled/EventClaimExpired arrives
	SettlementOutcome *string `json:"settlement_outcome,omitempty"` // "settled_proven", "expired", "slashed", "discarded"
	SettlementHeight  *int64  `json:"settlement_height,omitempty"`  // Block height of settlement event
	SettlementTxHash  *string `json:"settlement_tx_hash,omitempty"` // Settlement transaction hash (if applicable)
}

// SessionStore provides Redis-based storage for session snapshots.
// This enables HA failover by persisting session state that a new leader can recover.
type SessionStore interface {
	// Save persists a session snapshot to Redis.
	Save(ctx context.Context, snapshot *SessionSnapshot) error

	// Get retrieves a session snapshot by session ID.
	Get(ctx context.Context, sessionID string) (*SessionSnapshot, error)

	// GetBySupplier retrieves all active sessions for a supplier.
	GetBySupplier(ctx context.Context) ([]*SessionSnapshot, error)

	// GetByState retrieves all sessions in a given state for a supplier.
	GetByState(ctx context.Context, state SessionState) ([]*SessionSnapshot, error)

	// Delete removes a session snapshot.
	Delete(ctx context.Context, sessionID string) error

	// UpdateState atomically updates the state of a session.
	UpdateState(ctx context.Context, sessionID string, newState SessionState) error

	// UpdateSettlementMetadata updates the optional settlement metadata fields.
	// This is a non-critical update - session may already be cleaned up.
	UpdateSettlementMetadata(ctx context.Context, sessionID string, outcome string, height int64) error

	// UpdateWALPosition updates the last WAL entry ID for a session.
	UpdateWALPosition(ctx context.Context, sessionID string, walEntryID string) error

	// IncrementRelayCount atomically increments the relay count and compute units.
	IncrementRelayCount(ctx context.Context, sessionID string, computeUnits uint64) error

	// Close gracefully shuts down the store.
	Close() error
}

// SessionStoreConfig contains configuration for the session store.
type SessionStoreConfig struct {
	// KeyPrefix is the prefix for all Redis keys.
	KeyPrefix string

	// SupplierAddress is the supplier this store is for.
	SupplierAddress string

	// SessionTTL is how long to keep session data after settlement.
	SessionTTL time.Duration
}

// RedisSessionStore implements SessionStore using Redis.
type RedisSessionStore struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	config      SessionStoreConfig

	mu     sync.Mutex
	closed bool
}

// NewRedisSessionStore creates a new Redis-backed session store.
func NewRedisSessionStore(
	logger logging.Logger,
	redisClient *redisutil.Client,
	config SessionStoreConfig,
) *RedisSessionStore {
	if config.KeyPrefix == "" {
		config.KeyPrefix = "ha:miner:sessions"
	}
	if config.SessionTTL == 0 {
		// Default 2h - aligned with CacheTTL to prevent orphaned sessions.
		// Sessions and SMST trees expire together, avoiding "SMST missing but relay count > 0" warnings.
		config.SessionTTL = 2 * time.Hour
	}

	return &RedisSessionStore{
		logger:      logging.ForSupplierComponent(logger, logging.ComponentSessionStore, config.SupplierAddress),
		redisClient: redisClient,
		config:      config,
	}
}

// sessionKey returns the Redis key for a session.
func (s *RedisSessionStore) sessionKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:%s", s.config.KeyPrefix, s.config.SupplierAddress, sessionID)
}

// supplierSessionsKey returns the Redis key for the supplier's session index.
func (s *RedisSessionStore) supplierSessionsKey() string {
	return fmt.Sprintf("%s:%s:index", s.config.KeyPrefix, s.config.SupplierAddress)
}

// stateIndexKey returns the Redis key for a state-based index.
func (s *RedisSessionStore) stateIndexKey(state SessionState) string {
	return fmt.Sprintf("%s:%s:state:%s", s.config.KeyPrefix, s.config.SupplierAddress, state)
}

// --- Hash field codec ---------------------------------------------------------

// Hash field names. These are the authoritative on-wire names for the Redis
// Hash layout. Keep in sync with the Lua script and the debug CLI decoder
// in cmd/redis/sessions.go.
const (
	hfSessionID          = "session_id"
	hfSupplierOperator   = "supplier_operator_address"
	hfServiceID          = "service_id"
	hfApplicationAddress = "application_address"
	hfSessionStartHeight = "session_start_height"
	hfSessionEndHeight   = "session_end_height"
	hfState              = "state"
	hfRelayCount         = "relay_count"
	hfTotalComputeUnits  = "total_compute_units"
	hfClaimedRootHash    = "claimed_root_hash"
	hfClaimTxHash        = "claim_tx_hash"
	hfProofTxHash        = "proof_tx_hash"
	hfLastWALEntryID     = "last_wal_entry_id"
	hfCreatedAt          = "created_at"
	hfLastUpdatedAt      = "last_updated_at"
	hfSettlementOutcome  = "settlement_outcome"
	hfSettlementHeight   = "settlement_height"
	hfSettlementTxHash   = "settlement_tx_hash"
)

// encodeSnapshot flattens a SessionSnapshot into a slice of alternating
// field/value pairs suitable for HSET. Includes ALL fields including counters.
// Used only for initial session creation (Save on a new key).
func encodeSnapshot(snap *SessionSnapshot) []any {
	pairs := encodeSnapshotMetadata(snap)
	// Include counters only for initial creation
	pairs = append(pairs,
		hfRelayCount, strconv.FormatInt(snap.RelayCount, 10),
		hfTotalComputeUnits, strconv.FormatUint(snap.TotalComputeUnits, 10),
	)
	return pairs
}

// encodeSnapshotMetadata flattens all non-counter fields of a SessionSnapshot.
// Counter fields (relay_count, total_compute_units) are EXCLUDED because they
// are owned exclusively by the IncrementRelayCount Lua script via HINCRBY.
// Writing them from Go would race with concurrent HINCRBY operations.
func encodeSnapshotMetadata(snap *SessionSnapshot) []any {
	pairs := []any{
		hfSessionID, snap.SessionID,
		hfSupplierOperator, snap.SupplierOperatorAddress,
		hfServiceID, snap.ServiceID,
		hfApplicationAddress, snap.ApplicationAddress,
		hfSessionStartHeight, strconv.FormatInt(snap.SessionStartHeight, 10),
		hfSessionEndHeight, strconv.FormatInt(snap.SessionEndHeight, 10),
		hfState, string(snap.State),
		hfCreatedAt, snap.CreatedAt.Format(time.RFC3339Nano),
		hfLastUpdatedAt, snap.LastUpdatedAt.Format(time.RFC3339Nano),
	}
	if len(snap.ClaimedRootHash) > 0 {
		pairs = append(pairs, hfClaimedRootHash, string(snap.ClaimedRootHash))
	}
	if snap.ClaimTxHash != "" {
		pairs = append(pairs, hfClaimTxHash, snap.ClaimTxHash)
	}
	if snap.ProofTxHash != "" {
		pairs = append(pairs, hfProofTxHash, snap.ProofTxHash)
	}
	if snap.LastWALEntryID != "" {
		pairs = append(pairs, hfLastWALEntryID, snap.LastWALEntryID)
	}
	if snap.SettlementOutcome != nil {
		pairs = append(pairs, hfSettlementOutcome, *snap.SettlementOutcome)
	}
	if snap.SettlementHeight != nil {
		pairs = append(pairs, hfSettlementHeight, strconv.FormatInt(*snap.SettlementHeight, 10))
	}
	if snap.SettlementTxHash != nil {
		pairs = append(pairs, hfSettlementTxHash, *snap.SettlementTxHash)
	}
	return pairs
}

// decodeSnapshot turns an HGETALL result into a SessionSnapshot. Returns
// nil, nil when the map is empty (key did not exist).
func decodeSnapshot(fields map[string]string) (*SessionSnapshot, error) {
	if len(fields) == 0 {
		return nil, nil
	}

	snap := &SessionSnapshot{
		SessionID:               fields[hfSessionID],
		SupplierOperatorAddress: fields[hfSupplierOperator],
		ServiceID:               fields[hfServiceID],
		ApplicationAddress:      fields[hfApplicationAddress],
		State:                   SessionState(fields[hfState]),
		ClaimTxHash:             fields[hfClaimTxHash],
		ProofTxHash:             fields[hfProofTxHash],
		LastWALEntryID:          fields[hfLastWALEntryID],
	}

	if v := fields[hfSessionStartHeight]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", hfSessionStartHeight, err)
		}
		snap.SessionStartHeight = n
	}
	if v := fields[hfSessionEndHeight]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", hfSessionEndHeight, err)
		}
		snap.SessionEndHeight = n
	}
	if v := fields[hfRelayCount]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", hfRelayCount, err)
		}
		snap.RelayCount = n
	}
	if v := fields[hfTotalComputeUnits]; v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", hfTotalComputeUnits, err)
		}
		snap.TotalComputeUnits = n
	}
	if v := fields[hfCreatedAt]; v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", hfCreatedAt, err)
		}
		snap.CreatedAt = t
	}
	if v := fields[hfLastUpdatedAt]; v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", hfLastUpdatedAt, err)
		}
		snap.LastUpdatedAt = t
	}
	if v, ok := fields[hfClaimedRootHash]; ok && v != "" {
		candidate := []byte(v)
		// A persisted ClaimedRootHash with the wrong length will later panic
		// the smt library on import and also produces an invalid MsgCreateClaim
		// payload if it is ever passed through to the chain. Drop it here so
		// the session looks unclaimed and the normal flush path runs.
		if len(candidate) != SMSTRootLen {
			snap.ClaimedRootHash = nil
		} else {
			snap.ClaimedRootHash = candidate
		}
	}
	if v, ok := fields[hfSettlementOutcome]; ok {
		vv := v
		snap.SettlementOutcome = &vv
	}
	if v, ok := fields[hfSettlementHeight]; ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", hfSettlementHeight, err)
		}
		snap.SettlementHeight = &n
	}
	if v, ok := fields[hfSettlementTxHash]; ok {
		vv := v
		snap.SettlementTxHash = &vv
	}

	return snap, nil
}

// --- Core operations ----------------------------------------------------------

// Save persists a session snapshot to Redis as a Hash. Only metadata fields
// are written — counter fields (relay_count, total_compute_units) are NEVER
// touched because they are owned exclusively by IncrementRelayCount's Lua
// script via HINCRBY. Writing counters here would race with concurrent
// increments and cause lost relay counts under load.
//
// For NEW sessions (key does not exist), counters are initialized to their
// snapshot values via encodeSnapshot. For EXISTING sessions, only metadata
// fields are upserted via encodeSnapshotMetadata.
func (s *RedisSessionStore) Save(ctx context.Context, snapshot *SessionSnapshot) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("session store is closed")
	}
	s.mu.Unlock()

	// Get existing session to check for state change (for index cleanup).
	// Get() transparently handles legacy JSON keys, so rolling upgrades
	// still see the old state for index maintenance.
	var oldState SessionState
	existingSnapshot, err := s.Get(ctx, snapshot.SessionID)
	if err != nil {
		s.logger.Warn().
			Err(err).
			Str("session_id", snapshot.SessionID).
			Msg("failed to get existing session for state index cleanup")
	}
	if existingSnapshot != nil {
		oldState = existingSnapshot.State
	}

	snapshot.LastUpdatedAt = time.Now()
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = snapshot.LastUpdatedAt
	}

	key := s.sessionKey(snapshot.SessionID)

	// Check if key is a legacy JSON string that needs migration.
	// Legacy keys must be DEL'd before HSET to avoid WRONGTYPE errors.
	isLegacyKey := false
	if existingSnapshot != nil {
		keyType, typeErr := s.redisClient.Type(ctx, key).Result()
		if typeErr == nil && keyType == "string" {
			isLegacyKey = true
		}
	}

	pipe := s.redisClient.TxPipeline()

	if isLegacyKey {
		// Legacy JSON string: must DEL first to avoid WRONGTYPE on HSET.
		// Write all fields including counters (migration from JSON blob).
		pipe.Del(ctx, key)
		pipe.HSet(ctx, key, encodeSnapshot(snapshot)...)
	} else if existingSnapshot == nil {
		// New session: write all fields including counters.
		// No concurrent IncrementRelayCount can be running because the key
		// doesn't exist yet (the Lua script checks EXISTS first).
		pipe.HSet(ctx, key, encodeSnapshot(snapshot)...)
	} else {
		// Existing hash: write ONLY metadata fields. Counter fields
		// (relay_count, total_compute_units) are left untouched — they are
		// owned by the IncrementRelayCount Lua script.
		pipe.HSet(ctx, key, encodeSnapshotMetadata(snapshot)...)

		// Clean up optional fields that were cleared. Since we no longer
		// DEL+HSET, stale optional hash fields would persist unless we
		// explicitly remove them.
		var staleFields []string
		if len(snapshot.ClaimedRootHash) == 0 {
			staleFields = append(staleFields, hfClaimedRootHash)
		}
		if snapshot.ClaimTxHash == "" {
			staleFields = append(staleFields, hfClaimTxHash)
		}
		if snapshot.ProofTxHash == "" {
			staleFields = append(staleFields, hfProofTxHash)
		}
		if snapshot.LastWALEntryID == "" {
			staleFields = append(staleFields, hfLastWALEntryID)
		}
		if snapshot.SettlementOutcome == nil {
			staleFields = append(staleFields, hfSettlementOutcome)
		}
		if snapshot.SettlementHeight == nil {
			staleFields = append(staleFields, hfSettlementHeight)
		}
		if snapshot.SettlementTxHash == nil {
			staleFields = append(staleFields, hfSettlementTxHash)
		}
		if len(staleFields) > 0 {
			pipe.HDel(ctx, key, staleFields...)
		}
	}
	pipe.Expire(ctx, key, s.config.SessionTTL)

	// Supplier-wide session index
	pipe.SAdd(ctx, s.supplierSessionsKey(), snapshot.SessionID)
	pipe.Expire(ctx, s.supplierSessionsKey(), s.config.SessionTTL)

	// New state index
	pipe.SAdd(ctx, s.stateIndexKey(snapshot.State), snapshot.SessionID)
	pipe.Expire(ctx, s.stateIndexKey(snapshot.State), s.config.SessionTTL)

	// Old state index cleanup on state change
	if oldState != "" && oldState != snapshot.State {
		pipe.SRem(ctx, s.stateIndexKey(oldState), snapshot.SessionID)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to save session snapshot: %w", err)
	}

	sessionSnapshotsSaved.WithLabelValues(s.config.SupplierAddress).Inc()

	s.logger.Debug().
		Str("session_id", snapshot.SessionID).
		Str("state", string(snapshot.State)).
		Int64("relay_count", snapshot.RelayCount).
		Msg("saved session snapshot")

	return nil
}

// getHash performs an HGETALL-based read against the hash layout.
// Returns (nil, nil) when the key does not exist.
func (s *RedisSessionStore) getHash(ctx context.Context, key string) (*SessionSnapshot, error) {
	fields, err := s.redisClient.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to hgetall session snapshot: %w", err)
	}
	return decodeSnapshot(fields)
}

// getLegacyJSON reads a legacy JSON string key (pre-Wave-3) and decodes it
// via encoding/json. Returns (nil, nil) when the key does not exist.
// Rolling-upgrade fallback only; remove in a follow-up after one full
// session cycle (~60 min mainnet) — see HANDOFF-WAVE-3-HINCRBY.md.
func (s *RedisSessionStore) getLegacyJSON(ctx context.Context, key string) (*SessionSnapshot, error) {
	data, err := s.redisClient.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get legacy session snapshot: %w", err)
	}
	var snapshot SessionSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("failed to unmarshal legacy session snapshot: %w", err)
	}
	return &snapshot, nil
}

// Get retrieves a session snapshot by session ID. Transparently handles
// both the new Hash layout and the legacy JSON string layout during rolling
// upgrade (Option B in HANDOFF-WAVE-3-HINCRBY.md). Remove the legacy branch
// in a follow-up PR after breeze has cycled through one session window.
func (s *RedisSessionStore) Get(ctx context.Context, sessionID string) (*SessionSnapshot, error) {
	key := s.sessionKey(sessionID)

	keyType, err := s.redisClient.Type(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to check session key type: %w", err)
	}

	switch keyType {
	case "none":
		return nil, nil
	case "hash":
		return s.getHash(ctx, key)
	case "string":
		// Legacy JSON blob, written by pre-Wave-3 miners.
		return s.getLegacyJSON(ctx, key)
	default:
		return nil, fmt.Errorf("unexpected redis type for session key %s: %s", key, keyType)
	}
}

// GetBySupplier retrieves all sessions for a supplier.
func (s *RedisSessionStore) GetBySupplier(ctx context.Context) ([]*SessionSnapshot, error) {
	sessionIDs, err := s.redisClient.SMembers(ctx, s.supplierSessionsKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get session IDs: %w", err)
	}
	return s.fetchSessions(ctx, sessionIDs, "")
}

// GetByState retrieves all sessions in a given state for a supplier.
func (s *RedisSessionStore) GetByState(ctx context.Context, state SessionState) ([]*SessionSnapshot, error) {
	sessionIDs, err := s.redisClient.SMembers(ctx, s.stateIndexKey(state)).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get session IDs by state: %w", err)
	}
	return s.fetchSessions(ctx, sessionIDs, state)
}

// fetchSessions loads a batch of session IDs through Get() (which handles
// both hash and legacy JSON layouts). When filterState is non-empty, only
// snapshots matching that state are returned (the index may be stale).
func (s *RedisSessionStore) fetchSessions(
	ctx context.Context,
	sessionIDs []string,
	filterState SessionState,
) ([]*SessionSnapshot, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	snapshots := make([]*SessionSnapshot, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		snap, err := s.Get(ctx, sessionID)
		if err != nil {
			continue
		}
		if snap == nil {
			continue
		}
		if filterState != "" && snap.State != filterState {
			continue
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

// Delete removes a session snapshot.
func (s *RedisSessionStore) Delete(ctx context.Context, sessionID string) error {
	snapshot, err := s.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if snapshot == nil {
		return nil
	}

	pipe := s.redisClient.TxPipeline()
	pipe.Del(ctx, s.sessionKey(sessionID))
	pipe.SRem(ctx, s.supplierSessionsKey(), sessionID)
	pipe.SRem(ctx, s.stateIndexKey(snapshot.State), sessionID)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to delete session snapshot: %w", err)
	}

	s.logger.Debug().
		Str("session_id", sessionID).
		Msg("deleted session snapshot")

	return nil
}

// UpdateState atomically updates the state of a session. Uses Save() so the
// write goes through the same DEL+HSET+index transaction, which also
// migrates any legacy JSON keys seen during a rolling upgrade.
func (s *RedisSessionStore) UpdateState(ctx context.Context, sessionID string, newState SessionState) error {
	key := s.sessionKey(sessionID)
	now := time.Now().Format(time.RFC3339Nano)

	// Atomically read old state and set new state in one Lua script.
	// This avoids the Get→modify→Save round-trip that races with
	// concurrent IncrementRelayCount HINCRBY operations.
	oldStateStr, err := updateStateScript.Run(
		ctx,
		s.redisClient,
		[]string{key},
		string(newState),
		now,
		int64(s.config.SessionTTL.Seconds()),
	).Text()
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "session not found") {
			return fmt.Errorf("session not found: %s", sessionID)
		}
		if strings.Contains(errMsg, "legacy key") {
			// Legacy JSON string key — fall back to Get→Save migration path
			snapshot, getErr := s.Get(ctx, sessionID)
			if getErr != nil {
				return getErr
			}
			if snapshot == nil {
				return fmt.Errorf("session not found: %s", sessionID)
			}
			snapshot.State = newState
			return s.Save(ctx, snapshot)
		}
		return fmt.Errorf("failed to update session state: %w", err)
	}

	oldState := SessionState(oldStateStr)
	if oldState == newState {
		return nil
	}

	// Update state indexes (add to new, remove from old)
	pipe := s.redisClient.TxPipeline()
	pipe.SAdd(ctx, s.stateIndexKey(newState), sessionID)
	pipe.Expire(ctx, s.stateIndexKey(newState), s.config.SessionTTL)
	if oldState != "" {
		pipe.SRem(ctx, s.stateIndexKey(oldState), sessionID)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		s.logger.Warn().Err(err).
			Str("session_id", sessionID).
			Msg("failed to update state indexes after state change")
	}

	s.logger.Debug().
		Str("session_id", sessionID).
		Str("old_state", string(oldState)).
		Str("new_state", string(newState)).
		Msg("updated session state")

	return nil
}

// UpdateSettlementMetadata updates the optional settlement metadata fields.
// This is a non-critical update - session may already be cleaned up.
func (s *RedisSessionStore) UpdateSettlementMetadata(
	ctx context.Context,
	sessionID string,
	outcome string,
	height int64,
) error {
	key := s.sessionKey(sessionID)
	now := time.Now().Format(time.RFC3339Nano)

	pipe := s.redisClient.TxPipeline()
	pipe.HSet(ctx, key,
		hfSettlementOutcome, outcome,
		hfSettlementHeight, strconv.FormatInt(height, 10),
		hfLastUpdatedAt, now,
	)
	pipe.Expire(ctx, key, s.config.SessionTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to update settlement metadata: %w", err)
	}

	s.logger.Debug().
		Str("session_id", sessionID).
		Str("settlement_outcome", outcome).
		Int64("settlement_height", height).
		Msg("updated settlement metadata")

	return nil
}

// UpdateWALPosition updates the last WAL entry ID for a session.
func (s *RedisSessionStore) UpdateWALPosition(ctx context.Context, sessionID string, walEntryID string) error {
	key := s.sessionKey(sessionID)
	now := time.Now().Format(time.RFC3339Nano)

	pipe := s.redisClient.TxPipeline()
	pipe.HSet(ctx, key, hfLastWALEntryID, walEntryID, hfLastUpdatedAt, now)
	pipe.Expire(ctx, key, s.config.SessionTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to update WAL position: %w", err)
	}
	return nil
}

// updateStateScript atomically reads the old state and sets the new state
// plus last_updated_at on a session hash key. Returns the old state string.
// This avoids the Get→modify→Save pattern that races with HINCRBY.
//
// KEYS[1] = session hash key
// ARGV[1] = new state
// ARGV[2] = RFC3339Nano timestamp for last_updated_at
// ARGV[3] = TTL seconds
//
// Returns: old state string, or error "session not found" / "legacy key"
var updateStateScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
	return redis.error_reply('session not found')
end
local ktype = redis.call('TYPE', KEYS[1])['ok']
if ktype ~= 'hash' then
	return redis.error_reply('legacy key')
end
local old_state = redis.call('HGET', KEYS[1], 'state')
redis.call('HSET', KEYS[1], 'state', ARGV[1], 'last_updated_at', ARGV[2])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
return old_state
`)

// incrementRelayCountScript atomically increments relay_count and
// total_compute_units on a session hash key, guarded by the terminal-state
// check. This is a single Redis round-trip with no cjson parsing.
//
// KEYS[1] = session hash key
// ARGV[1] = compute units to add (uint64)
// ARGV[2] = RFC3339Nano timestamp for last_updated_at
// ARGV[3] = TTL seconds
//
// Returns:
//
//	0 = success
//	1 = session not found
//	2 = session in terminal state
//
// Terminal states MUST match SessionState.IsTerminal() in Go. When adding
// a new terminal state, update both places.
var incrementRelayCountScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
	return 1
end

local state = redis.call('HGET', KEYS[1], 'state')
if state == 'proved' or state == 'probabilistic_proved'
	or state == 'claim_window_closed' or state == 'claim_tx_error'
	or state == 'proof_window_closed' or state == 'proof_tx_error'
	or state == 'claim_skipped' then
	return 2
end

redis.call('HINCRBY', KEYS[1], 'relay_count', 1)
redis.call('HINCRBY', KEYS[1], 'total_compute_units', tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'last_updated_at', ARGV[2])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
return 0
`)

// IncrementRelayCount atomically increments the relay count and compute
// units using HINCRBY inside a small Lua script. On a WRONGTYPE error (the
// key is still a legacy JSON string from a pre-Wave-3 miner), transparently
// migrate the snapshot to the new hash layout via Get+Save and retry the
// script once.
func (s *RedisSessionStore) IncrementRelayCount(ctx context.Context, sessionID string, computeUnits uint64) error {
	key := s.sessionKey(sessionID)
	ttlSeconds := int64(s.config.SessionTTL.Seconds())
	now := time.Now().Format(time.RFC3339Nano)

	result, err := incrementRelayCountScript.Run(
		ctx,
		s.redisClient,
		[]string{key},
		computeUnits,
		now,
		ttlSeconds,
	).Int64()

	if err != nil && isWrongTypeErr(err) {
		// Legacy JSON string still in place — migrate to hash and retry.
		// This path only fires during a rolling upgrade and disappears
		// once all in-flight sessions have been rewritten.
		if migrateErr := s.migrateLegacyKey(ctx, sessionID); migrateErr != nil {
			return fmt.Errorf("failed to migrate legacy session key: %w", migrateErr)
		}
		result, err = incrementRelayCountScript.Run(
			ctx,
			s.redisClient,
			[]string{key},
			computeUnits,
			now,
			ttlSeconds,
		).Int64()
	}

	if err != nil {
		return fmt.Errorf("failed to increment relay count: %w", err)
	}

	switch result {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("session not found: %s", sessionID)
	case 2:
		return ErrSessionTerminal
	default:
		return fmt.Errorf("unexpected result from increment script: %d", result)
	}
}

// migrateLegacyKey reads a legacy JSON session key and rewrites it in the
// new hash format. Used as a one-shot rescue path when IncrementRelayCount
// hits a WRONGTYPE error during rolling upgrade.
func (s *RedisSessionStore) migrateLegacyKey(ctx context.Context, sessionID string) error {
	snap, err := s.getLegacyJSON(ctx, s.sessionKey(sessionID))
	if err != nil {
		return err
	}
	if snap == nil {
		return nil // Disappeared between the script run and the migrate read.
	}
	return s.Save(ctx, snap)
}

// isWrongTypeErr reports whether a Redis error is the WRONGTYPE error raised
// when a hash operation is attempted against a string key (or vice-versa).
func isWrongTypeErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "WRONGTYPE")
}

// Close gracefully shuts down the store.
func (s *RedisSessionStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	s.logger.Info().Msg("session store closed")
	return nil
}

// Verify interface compliance.
var _ SessionStore = (*RedisSessionStore)(nil)
