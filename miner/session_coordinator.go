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
// It updates the session state to claimed and stores the claim root hash.
func (c *SessionCoordinator) OnSessionClaimed(
	ctx context.Context,
	sessionID string,
	claimRootHash []byte,
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

	// Update with claim root hash
	snapshot.ClaimedRootHash = claimRootHash
	snapshot.State = SessionStateClaimed

	if err := c.sessionStore.Save(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to save session snapshot: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Int("root_hash_len", len(claimRootHash)).
		Msg("session claimed")

	return nil
}

// OnSessionSettled should be called when a session proof is successfully submitted and settled.
// It updates the session state to settled.
func (c *SessionCoordinator) OnSessionSettled(
	ctx context.Context,
	sessionID string,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Update state to settled
	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateSettled); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to settled")
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Msg("session settled")

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
