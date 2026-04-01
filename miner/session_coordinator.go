package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// SessionCreatedCallback is called when a new session is created.
// This allows external components (like SessionLifecycleManager) to be notified.
type SessionCreatedCallback func(ctx context.Context, snapshot *SessionSnapshot) error

// SessionTerminalCallback is called when a session transitions to a terminal state.
// This allows external components (like SessionLifecycleManager) to update in-memory state
// atomically with Redis updates.
type SessionTerminalCallback func(sessionID string, state SessionState)

// SMSTRecoveryConfig contains configuration for session recovery.
type SMSTRecoveryConfig struct {
	// SupplierAddress is the supplier this recovery service is for.
	SupplierAddress string

	// RecoveryTimeout is the maximum time allowed for recovery.
	RecoveryTimeout time.Duration
}

// SessionCoordinator manages session lifecycle events (creation, relay tracking).
// This replaces SMSTSnapshotManager which was tightly coupled to the now-removed WAL.
type SessionCoordinator struct {
	logger       logging.Logger
	sessionStore SessionStore
	config       SMSTRecoveryConfig

	// onSessionCreated is called when a new session is created
	onSessionCreated SessionCreatedCallback

	// onSessionTerminal is called when a session transitions to a terminal state.
	// This allows in-memory state to be updated atomically with Redis.
	onSessionTerminal SessionTerminalCallback

	mu     sync.Mutex
	closed bool
}

// NewSessionCoordinator creates a new session coordinator.
func NewSessionCoordinator(
	logger logging.Logger,
	sessionStore SessionStore,
	config SMSTRecoveryConfig,
) *SessionCoordinator {
	return &SessionCoordinator{
		logger:       logging.ForSupplierComponent(logger, logging.ComponentSMSTSnapshot, config.SupplierAddress),
		sessionStore: sessionStore,
		config:       config,
	}
}

// SetOnSessionCreatedCallback sets the callback to be invoked when a new session is created.
func (c *SessionCoordinator) SetOnSessionCreatedCallback(callback SessionCreatedCallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onSessionCreated = callback
}

// SetOnSessionTerminalCallback sets the callback to be invoked when a session transitions to terminal state.
// This allows SessionLifecycleManager to update in-memory state atomically with Redis updates.
func (c *SessionCoordinator) SetOnSessionTerminalCallback(callback SessionTerminalCallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onSessionTerminal = callback
}

// OnRelayProcessed should be called when a relay is successfully processed and added to SMST.
// It ensures the session exists and updates the relay count.
// If the session doesn't exist, it will be created automatically using the provided metadata.
func (c *SessionCoordinator) OnRelayProcessed(
	ctx context.Context,
	sessionID string,
	computeUnits uint64,
	supplierAddress, serviceID, applicationAddress string,
	sessionStartHeight, sessionEndHeight int64,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Check if session exists, create if not
	snapshot, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to check session existence")
	}

	if snapshot == nil {
		// Session doesn't exist, create it
		if supplierAddress == "" || serviceID == "" {
			c.logger.Warn().
				Str(logging.FieldSessionID, sessionID).
				Msg("session not found and missing metadata to create it")
		} else {
			c.logger.Info().
				Str(logging.FieldSessionID, sessionID).
				Str(logging.FieldService, serviceID).
				Str(logging.FieldSupplier, supplierAddress).
				Int64("start_height", sessionStartHeight).
				Int64("end_height", sessionEndHeight).
				Msg("creating new session on first relay")

			if err := c.OnSessionCreated(ctx, sessionID, supplierAddress, serviceID, applicationAddress, sessionStartHeight, sessionEndHeight); err != nil {
				c.logger.Warn().
					Err(err).
					Str(logging.FieldSessionID, sessionID).
					Msg("failed to create session")
			} else {
				// Record session creation metric for operator visibility
				RecordSessionCreated(supplierAddress, serviceID)
			}
		}
	}

	// Update session relay count (critical for claim submission)
	if err := c.sessionStore.IncrementRelayCount(ctx, sessionID, computeUnits); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session relay count")
	}

	return nil
}

// OnSessionCreated should be called when a new session is created.
func (c *SessionCoordinator) OnSessionCreated(
	ctx context.Context,
	sessionID string,
	supplierAddress, serviceID, applicationAddress string,
	startHeight, endHeight int64,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Create session snapshot in Redis
	snapshot := &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: supplierAddress,
		ServiceID:               serviceID,
		ApplicationAddress:      applicationAddress,
		SessionStartHeight:      startHeight,
		SessionEndHeight:        endHeight,
		State:                   SessionStateActive,
		RelayCount:              0,
		TotalComputeUnits:       0,
		ClaimedRootHash:         nil,
	}

	if err := c.sessionStore.Save(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to save session snapshot: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Str(logging.FieldSupplier, supplierAddress).
		Msg("session created and snapshot saved")

	// Invoke callback if registered
	c.mu.Lock()
	callback := c.onSessionCreated
	c.mu.Unlock()

	if callback != nil {
		if err := callback(ctx, snapshot); err != nil {
			c.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Msg("session created callback failed")
		}
	}

	return nil
}

// OnSessionClaimed should be called when a session's claim is submitted.
// It updates the session state to claimed and stores the claim root hash and TX hash.
func (c *SessionCoordinator) OnSessionClaimed(
	ctx context.Context,
	sessionID string,
	claimRootHash []byte,
	claimTxHash string,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Get current snapshot
	snapshot, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session snapshot: %w", err)
	}
	if snapshot == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Update with claim root hash and TX hash
	snapshot.ClaimedRootHash = claimRootHash
	snapshot.ClaimTxHash = claimTxHash
	snapshot.State = SessionStateClaimed

	if err := c.sessionStore.Save(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to save session snapshot: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Str("claim_tx_hash", claimTxHash).
		Int("root_hash_len", len(claimRootHash)).
		Msg("session claimed")

	return nil
}

// OnProofSubmitted should be called when a session's proof TX is broadcast.
// It stores the proof TX hash for deduplication and tracking.
func (c *SessionCoordinator) OnProofSubmitted(
	ctx context.Context,
	sessionID string,
	proofTxHash string,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Get current snapshot
	snapshot, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session snapshot: %w", err)
	}
	if snapshot == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Update with proof TX hash (state is already set to Proving by state machine)
	snapshot.ProofTxHash = proofTxHash

	if err := c.sessionStore.Save(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to save session snapshot: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Str("proof_tx_hash", proofTxHash).
		Msg("proof TX hash stored")

	return nil
}

// OnSessionProved should be called when a session proof is successfully submitted.
// It updates the session state to proved.
func (c *SessionCoordinator) OnSessionProved(
	ctx context.Context,
	sessionID string,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	// Update state to proved
	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateProved); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to proved")
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateProved)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateProved)
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Msg("session proved")

	return nil
}

// OnClaimWindowClosed marks session as failed due to claim window timeout.
// Updates state immediately in Redis for HA compatibility.
func (c *SessionCoordinator) OnClaimWindowClosed(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateClaimWindowClosed); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to claim_window_closed")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateClaimWindowClosed)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateClaimWindowClosed)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("claim window closed")
	return nil
}

// OnClaimTxError marks session as failed due to claim transaction error.
// Updates state immediately in Redis for HA compatibility.
//
// IMPORTANT: This method checks the current Redis state before overwriting.
// In multi-miner setups, another miner may have already successfully claimed
// the session. Overwriting SessionStateClaimed with SessionStateClaimTxError
// would kill the session and prevent proof submission, causing lost rewards.
func (c *SessionCoordinator) OnClaimTxError(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	// Check current state in Redis before overwriting — another miner may have
	// already claimed this session successfully. Marking a successfully-claimed
	// session as ClaimTxError would prevent proof submission and lose rewards.
	current, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to read session state before marking claim_tx_error")
		// Fall through — better to risk a no-op than to skip the error marking entirely
	} else if current != nil && (current.State == SessionStateClaimed || current.ClaimTxHash != "") {
		c.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Str("current_state", string(current.State)).
			Str("claim_tx_hash", current.ClaimTxHash).
			Msg("NOT marking claim_tx_error: session already claimed by another miner")
		return nil
	}

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateClaimTxError); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to claim_tx_error")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateClaimTxError)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateClaimTxError)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("claim tx error")
	return nil
}

// OnProofWindowClosed marks session as failed due to proof window timeout.
// Updates state immediately in Redis for HA compatibility.
func (c *SessionCoordinator) OnProofWindowClosed(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateProofWindowClosed); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to proof_window_closed")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateProofWindowClosed)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateProofWindowClosed)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("proof window closed")
	return nil
}

// OnProofTxError marks session as failed due to proof transaction error.
// Updates state immediately in Redis for HA compatibility.
//
// IMPORTANT: This method checks the current Redis state before overwriting.
// In multi-miner setups, another miner may have already successfully proved
// the session. Overwriting SessionStateProved with SessionStateProofTxError
// would incorrectly mark a successful session as failed.
func (c *SessionCoordinator) OnProofTxError(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	// Check current state in Redis before overwriting — another miner may have
	// already proved this session successfully.
	current, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to read session state before marking proof_tx_error")
	} else if current != nil && (current.State == SessionStateProved || current.State == SessionStateProbabilisticProved || current.ProofTxHash != "") {
		c.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Str("current_state", string(current.State)).
			Str("proof_tx_hash", current.ProofTxHash).
			Msg("NOT marking proof_tx_error: session already proved by another miner")
		return nil
	}

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateProofTxError); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to proof_tx_error")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateProofTxError)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateProofTxError)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("proof tx error")
	return nil
}

// OnProbabilisticProved marks session as probabilistically proved (no proof required).
// Updates state immediately in Redis for HA compatibility.
func (c *SessionCoordinator) OnProbabilisticProved(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateProbabilisticProved); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to probabilistic_proved")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateProbabilisticProved)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateProbabilisticProved)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("probabilistic proved (no proof required)")
	return nil
}

// Close gracefully shuts down the session coordinator.
func (c *SessionCoordinator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	c.logger.Info().Msg("session coordinator closed")
	return nil
}
