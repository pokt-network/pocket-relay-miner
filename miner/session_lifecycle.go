package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/alitto/pond/v2"
	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// SessionLifecycleConfig contains configuration for the session lifecycle manager.
type SessionLifecycleConfig struct {
	// SupplierAddress is the supplier this manager is for.
	SupplierAddress string

	// CheckIntervalBlocks is how often to check for session transitions.
	// Default: 1 (every block)
	CheckIntervalBlocks int64

	// MaxConcurrentTransitions is the max number of sessions transitioning at once.
	// Default: 10
	MaxConcurrentTransitions int

	// CheckInterval is the time interval for checking session transitions.
	// If 0, defaults to 30 * time.Second.
	// For tests, set to a faster value like 100 * time.Millisecond.
	CheckInterval time.Duration
}

// SessionLifecycleCallback defines callbacks for lifecycle events.
type SessionLifecycleCallback interface {
	// OnSessionActive is called when a new session starts.
	OnSessionActive(ctx context.Context, snapshot *SessionSnapshot) error

	// OnSessionsNeedClaim is called when sessions need claims submitted (batched).
	// The callback should trigger claim submission and return root hashes in the same order.
	// All sessions in the batch are submitted in a single transaction for efficiency.
	OnSessionsNeedClaim(ctx context.Context, snapshots []*SessionSnapshot) (rootHashes [][]byte, err error)

	// OnSessionsNeedProof is called when sessions need proofs submitted (batched).
	// All sessions in the batch are submitted in a single transaction for efficiency.
	OnSessionsNeedProof(ctx context.Context, snapshots []*SessionSnapshot) error

	// OnSessionProved is called when a session proof is successfully submitted.
	OnSessionProved(ctx context.Context, snapshot *SessionSnapshot) error

	// OnProbabilisticProved is called when a session is probabilistically proved (no proof required).
	OnProbabilisticProved(ctx context.Context, snapshot *SessionSnapshot) error

	// OnClaimWindowClosed is called when a session fails due to claim window timeout.
	OnClaimWindowClosed(ctx context.Context, snapshot *SessionSnapshot) error

	// OnClaimTxError is called when a session fails due to claim transaction error.
	OnClaimTxError(ctx context.Context, snapshot *SessionSnapshot) error

	// OnProofWindowClosed is called when a session fails due to proof window timeout.
	OnProofWindowClosed(ctx context.Context, snapshot *SessionSnapshot) error

	// OnProofTxError is called when a session fails due to proof transaction error.
	OnProofTxError(ctx context.Context, snapshot *SessionSnapshot) error
}

// PendingRelayChecker checks for pending (unconsumed) relays in session streams.
// This is used to detect late-arriving relays before claim submission.
type PendingRelayChecker interface {
	// GetPendingRelayCount returns the number of pending relays for a session stream.
	// Returns 0 if the stream doesn't exist or has no pending messages.
	GetPendingRelayCount(ctx context.Context, sessionID string) (int64, error)
}

// MeterCleanupPublisher publishes cleanup signals to relayers when sessions leave active state.
// This notifies relayers to clear their session meter data and decrement active session metrics.
type MeterCleanupPublisher interface {
	// PublishMeterCleanup publishes a cleanup signal for a session.
	PublishMeterCleanup(ctx context.Context, sessionID string) error
}

// RedisMeterCleanupPublisher implements MeterCleanupPublisher using Redis pub/sub.
// It publishes session IDs to the meter cleanup channel so relayers can clear their
// session meter data and decrement the active sessions metric.
type RedisMeterCleanupPublisher struct {
	logger  logging.Logger
	publish func(ctx context.Context, channel string, message interface{}) error
	channel string
}

// NewRedisMeterCleanupPublisher creates a new Redis-based meter cleanup publisher.
// The publish function should be the Redis client's Publish method wrapped to return error.
func NewRedisMeterCleanupPublisher(
	logger logging.Logger,
	publish func(ctx context.Context, channel string, message interface{}) error,
	channel string,
) *RedisMeterCleanupPublisher {
	return &RedisMeterCleanupPublisher{
		logger:  logger,
		publish: publish,
		channel: channel,
	}
}

// PublishMeterCleanup publishes a cleanup signal for a session via Redis pub/sub.
func (p *RedisMeterCleanupPublisher) PublishMeterCleanup(ctx context.Context, sessionID string) error {
	return p.publish(ctx, p.channel, sessionID)
}

// SessionLifecycleManager manages the lifecycle of sessions from active to settled.
// It monitors block heights and triggers state transitions at the appropriate times.
type SessionLifecycleManager struct {
	logger       logging.Logger
	config       SessionLifecycleConfig
	sessionStore SessionStore
	sharedClient client.SharedQueryClient
	blockClient  client.BlockClient
	callback     SessionLifecycleCallback

	// Optional pending relay checker for late relay detection
	pendingChecker PendingRelayChecker

	// Optional meter cleanup publisher for notifying relayers when sessions leave active state
	meterCleanupPublisher MeterCleanupPublisher

	// Current shared params (cached)
	sharedParams   *sharedtypes.Params
	sharedParamsMu sync.RWMutex

	// Active sessions being monitored
	activeSessions   map[string]*SessionSnapshot // sessionID -> snapshot
	activeSessionsMu sync.RWMutex

	// Pond subpool for controlled concurrency during transitions
	transitionSubpool pond.Pool

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.RWMutex
	closed   bool
}

// NewSessionLifecycleManager creates a new session lifecycle manager.
// The pendingChecker parameter is optional - if provided, it enables late relay detection.
// The workerPool parameter is required for creating transition subpool.
func NewSessionLifecycleManager(
	logger logging.Logger,
	sessionStore SessionStore,
	sharedClient client.SharedQueryClient,
	blockClient client.BlockClient,
	callback SessionLifecycleCallback,
	config SessionLifecycleConfig,
	pendingChecker PendingRelayChecker,
	workerPool pond.Pool,
) *SessionLifecycleManager {
	if config.CheckIntervalBlocks <= 0 {
		config.CheckIntervalBlocks = 1
	}
	// MaxConcurrentTransitions is always set by the config getter (minimum 10)

	// Create transition subpool from master pool
	// Uses CreateBoundedSubpool to cap at parent pool max and warn if exceeded
	componentLogger := logging.ForSupplierComponent(logger, logging.ComponentSessionLifecycle, config.SupplierAddress)
	transitionSubpool := CreateBoundedSubpool(componentLogger, workerPool, config.MaxConcurrentTransitions, "transition_subpool")

	componentLogger.Debug().
		Int("transition_workers", config.MaxConcurrentTransitions).
		Msg("created transition subpool from master pool")

	return &SessionLifecycleManager{
		logger:            componentLogger,
		config:            config,
		sessionStore:      sessionStore,
		sharedClient:      sharedClient,
		blockClient:       blockClient,
		callback:          callback,
		pendingChecker:    pendingChecker,
		activeSessions:    make(map[string]*SessionSnapshot),
		transitionSubpool: transitionSubpool,
	}
}

// SetMeterCleanupPublisher sets the meter cleanup publisher for notifying relayers
// when sessions leave active state. This should be called before Start().
func (m *SessionLifecycleManager) SetMeterCleanupPublisher(publisher MeterCleanupPublisher) {
	m.meterCleanupPublisher = publisher
}

// Start begins monitoring sessions and triggering lifecycle transitions.
func (m *SessionLifecycleManager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("lifecycle manager is closed")
	}

	m.ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	// Load initial shared params
	if err := m.refreshSharedParams(ctx); err != nil {
		return fmt.Errorf("failed to load shared params: %w", err)
	}

	// Load existing sessions from store
	if err := m.loadExistingSessions(ctx); err != nil {
		m.logger.Warn().Err(err).Msg("failed to load existing sessions, starting fresh")
	}

	// LATE SESSION PRIORITIZATION: Check for sessions needing immediate attention
	// If we have loaded sessions and any are past their claim window, process them now
	// instead of waiting for the next block event (could be 10+ seconds)
	if len(m.activeSessions) > 0 {
		block := m.blockClient.LastBlock(ctx)
		if block != nil {
			m.logger.Debug().
				Int64("current_height", block.Height()).
				Int("loaded_sessions", len(m.activeSessions)).
				Msg("checking for late sessions on startup")
			m.checkSessionTransitions(ctx, block.Height())
		}
	}

	// Start lifecycle checker
	m.wg.Add(1)
	go m.lifecycleChecker(m.ctx)

	m.logger.Info().
		Int("active_sessions", len(m.activeSessions)).
		Msg("session lifecycle manager started")

	return nil
}

// loadExistingSessions loads sessions from the store on startup.
func (m *SessionLifecycleManager) loadExistingSessions(ctx context.Context) error {
	sessions, err := m.sessionStore.GetBySupplier(ctx)
	if err != nil {
		return err
	}

	m.activeSessionsMu.Lock()
	defer m.activeSessionsMu.Unlock()

	for _, session := range sessions {
		// Only track sessions that aren't in terminal state
		if !session.State.IsTerminal() {
			m.activeSessions[session.SessionID] = session
			sessionSnapshotsLoaded.WithLabelValues(m.config.SupplierAddress).Inc()
		} else {
			// Log and track metrics for skipped sessions (terminal states)
			// These are historical events from before restart, use DEBUG level
			sessionSnapshotsSkippedAtStartup.WithLabelValues(m.config.SupplierAddress, string(session.State)).Inc()

			// Differentiate between success and failure for operator clarity
			if session.State.IsSuccess() {
				m.logger.Debug().
					Str("session_id", session.SessionID).
					Str("service_id", session.ServiceID).
					Str("state", string(session.State)).
					Int64("session_end_height", session.SessionEndHeight).
					Int64("relay_count", session.RelayCount).
					Uint64("compute_units", session.TotalComputeUnits).
					Msg("skipping session at startup: already completed (rewards claimed or proved)")
			} else if session.State.IsFailure() {
				// Failed session - historical data (actual WARN was logged when it failed)
				// Only mention "rewards lost" if there were actually relays
				msg := "skipping session at startup: failed with 0 relays"
				if session.RelayCount > 0 {
					msg = "skipping session at startup: failed (rewards lost)"
				}
				m.logger.Debug().
					Str("session_id", session.SessionID).
					Str("state", string(session.State)).
					Str("service_id", session.ServiceID).
					Int64("session_end_height", session.SessionEndHeight).
					Int64("relay_count", session.RelayCount).
					Uint64("compute_units", session.TotalComputeUnits).
					Msg(msg)
			}
		}
	}

	return nil
}

// refreshSharedParams refreshes the cached shared params.
func (m *SessionLifecycleManager) refreshSharedParams(ctx context.Context) error {
	params, err := m.sharedClient.GetParams(ctx)
	if err != nil {
		return err
	}

	m.sharedParamsMu.Lock()
	m.sharedParams = params
	m.sharedParamsMu.Unlock()

	return nil
}

// getSharedParams returns the cached shared params.
func (m *SessionLifecycleManager) getSharedParams() *sharedtypes.Params {
	m.sharedParamsMu.RLock()
	defer m.sharedParamsMu.RUnlock()
	return m.sharedParams
}

// TrackSession starts tracking a new session.
func (m *SessionLifecycleManager) TrackSession(ctx context.Context, snapshot *SessionSnapshot) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("lifecycle manager is closed")
	}
	m.mu.RUnlock()

	m.activeSessionsMu.Lock()
	m.activeSessions[snapshot.SessionID] = snapshot
	m.activeSessionsMu.Unlock()

	// Persist to store
	if err := m.sessionStore.Save(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	m.logger.Debug().
		Str("session_id", snapshot.SessionID).
		Str("state", string(snapshot.State)).
		Msg("started tracking session")

	return nil
}

// GetSession returns a tracked session by ID.
func (m *SessionLifecycleManager) GetSession(sessionID string) *SessionSnapshot {
	m.activeSessionsMu.RLock()
	defer m.activeSessionsMu.RUnlock()
	return m.activeSessions[sessionID]
}

// GetActiveSessions returns all sessions in the active state.
func (m *SessionLifecycleManager) GetActiveSessions() []*SessionSnapshot {
	m.activeSessionsMu.RLock()
	defer m.activeSessionsMu.RUnlock()

	result := make([]*SessionSnapshot, 0)
	for _, session := range m.activeSessions {
		if session.State == SessionStateActive {
			result = append(result, session)
		}
	}
	return result
}

// GetSessionsByState returns all sessions in a given state.
func (m *SessionLifecycleManager) GetSessionsByState(state SessionState) []*SessionSnapshot {
	m.activeSessionsMu.RLock()
	defer m.activeSessionsMu.RUnlock()

	result := make([]*SessionSnapshot, 0)
	for _, session := range m.activeSessions {
		if session.State == state {
			result = append(result, session)
		}
	}
	return result
}

// UpdateSessionRelayCount updates the relay count for a session.
func (m *SessionLifecycleManager) UpdateSessionRelayCount(ctx context.Context, sessionID string, computeUnits uint64) error {
	m.activeSessionsMu.Lock()
	session, exists := m.activeSessions[sessionID]
	if !exists {
		m.activeSessionsMu.Unlock()
		return fmt.Errorf("session not found: %s", sessionID)
	}

	session.RelayCount++
	session.TotalComputeUnits += computeUnits
	session.LastUpdatedAt = time.Now()
	m.activeSessionsMu.Unlock()

	// Persist update asynchronously
	go func() {
		if err := m.sessionStore.IncrementRelayCount(ctx, sessionID, computeUnits); err != nil {
			m.logger.Warn().Err(err).Str("session_id", sessionID).Msg("failed to persist relay count")
		}
	}()

	return nil
}

// lifecycleChecker monitors blocks and checks sessions for state transitions.
// Uses event-driven block notifications via Subscribe() method.
func (m *SessionLifecycleManager) lifecycleChecker(ctx context.Context) {
	defer m.wg.Done()

	_ = m.getSharedParams() // Ensure params are cached

	// Check if block client supports Subscribe() method for fan-out
	if subscriber, ok := m.blockClient.(interface {
		Subscribe(ctx context.Context, bufferSize int) <-chan *localclient.SimpleBlock
	}); ok {
		m.logger.Info().Msg("using event-driven block notifications (Subscribe)")
		m.lifecycleCheckerEventDriven(ctx, subscriber)
	} else {
		m.logger.Warn().Msg("block client does not support Subscribe(), falling back to polling")
		m.lifecycleCheckerPolling(ctx)
	}
}

// lifecycleCheckerEventDriven uses block events for immediate session transition checks.
func (m *SessionLifecycleManager) lifecycleCheckerEventDriven(ctx context.Context, subscriber interface {
	Subscribe(ctx context.Context, bufferSize int) <-chan *localclient.SimpleBlock
}) {
	lastHeight := int64(0)

	// Subscribe to block events with 2000-block buffer
	blockCh := subscriber.Subscribe(ctx, 2000)
	m.logger.Debug().Msg("using Subscribe() for block events (fan-out mode)")

	for {
		select {
		case <-ctx.Done():
			return

		case block, ok := <-blockCh:
			if !ok {
				// Channel closed, block client shut down
				m.logger.Warn().Msg("block events channel closed")
				return
			}

			currentHeight := block.Height()

			// Only process if height actually increased
			if currentHeight <= lastHeight {
				continue
			}
			lastHeight = currentHeight
			currentBlockHeight.Set(float64(currentHeight))

			// Refresh shared params periodically (every 10 blocks)
			if currentHeight%10 == 0 {
				if err := m.refreshSharedParams(ctx); err != nil {
					m.logger.Warn().Err(err).Msg("failed to refresh shared params")
				}
			}

			// Check all sessions for transitions at this block height
			m.checkSessionTransitions(ctx, currentHeight)
		}
	}
}

// lifecycleCheckerPolling uses time-based polling as a fallback.
func (m *SessionLifecycleManager) lifecycleCheckerPolling(ctx context.Context) {
	// AGGRESSIVE: 1 second polling by default.
	// Claims/proofs = money. Missing them = losing money.
	// Better to poll frequently than risk missing a window.
	checkInterval := m.config.CheckInterval
	if checkInterval == 0 {
		checkInterval = 1 * time.Second
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	lastHeight := int64(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Get current block height
			block := m.blockClient.LastBlock(ctx)
			currentHeight := block.Height()

			// Only process if height changed
			if currentHeight <= lastHeight {
				continue
			}
			lastHeight = currentHeight
			currentBlockHeight.Set(float64(currentHeight))

			// Refresh shared params periodically (every 10 blocks)
			if currentHeight%10 == 0 {
				if err := m.refreshSharedParams(ctx); err != nil {
					m.logger.Warn().Err(err).Msg("failed to refresh shared params")
				}
			}

			// Check all sessions for transitions
			m.checkSessionTransitions(ctx, currentHeight)
		}
	}
}

// checkSessionTransitions checks all active sessions for required state transitions.
func (m *SessionLifecycleManager) checkSessionTransitions(ctx context.Context, currentHeight int64) {
	m.activeSessionsMu.RLock()
	sessionIDs := make([]string, 0, len(m.activeSessions))
	for sessionID := range m.activeSessions {
		sessionIDs = append(sessionIDs, sessionID)
	}
	m.activeSessionsMu.RUnlock()

	params := m.getSharedParams()
	if params == nil {
		m.logger.Warn().Msg("shared params not available, skipping transition check")
		return
	}

	// Group sessions by transition type (claiming, proving, terminal states)
	var claimingSessions []*SessionSnapshot
	var provingSessions []*SessionSnapshot
	// Terminal sessions stored as alternating (state, session) pairs for individual handling
	var terminalSessions []interface{}

	for _, sessionID := range sessionIDs {
		// CRITICAL: Reload session from Redis to get latest state (not stale in-memory copy)
		// Callbacks may have updated state to terminal (e.g., proof_tx_error) which must not be overwritten
		session, err := m.sessionStore.Get(ctx, sessionID)
		if err != nil || session == nil {
			m.logger.Warn().Err(err).Str("session_id", sessionID).Msg("failed to reload session from Redis")
			continue
		}

		// CRITICAL: Skip sessions already in terminal states - they should NEVER transition again
		// Terminal states (proved, probabilistic_proved, claim_tx_error, proof_tx_error, etc.)
		// represent final outcomes and must not be overwritten by window timeout logic
		if session.State.IsTerminal() {
			continue
		}

		// Check if this session needs a transition
		newState, _ := m.determineTransition(session, currentHeight, params)
		if newState == "" || newState == session.State {
			continue
		}

		// Group by transition type for batching
		switch newState {
		case SessionStateClaiming:
			claimingSessions = append(claimingSessions, session)
		case SessionStateProving:
			provingSessions = append(provingSessions, session)
		case SessionStateProbabilisticProved,
			SessionStateClaimWindowClosed,
			SessionStateClaimTxError,
			SessionStateProofWindowClosed,
			SessionStateProofTxError:
			// Terminal states: store (state, session) pairs
			terminalSessions = append(terminalSessions, newState, session)
		}
	}

	// Execute batched transitions using pond subpool
	// Non-blocking submission with unbounded queue (tasks queue if workers are busy)
	//
	// CRITICAL: Persist state changes to REDIS before submitting async callbacks.
	// This prevents duplicate submissions where the same sessions are resubmitted
	// every block because Redis still shows them in Active state.
	// Without this, sessions can be submitted dozens of times before the callback
	// completes, flooding the worker pool and causing claim window timeouts.
	if len(claimingSessions) > 0 {
		// Persist state to Redis FIRST to prevent duplicate submissions
		// If Redis update fails, we skip submitting to avoid inconsistent state
		var validClaimingSessions []*SessionSnapshot
		for _, session := range claimingSessions {
			if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateClaiming); err != nil {
				m.logger.Error().
					Err(err).
					Str("session_id", session.SessionID).
					Msg("failed to persist claiming state to Redis - skipping to prevent duplicate submission")
				sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state_claiming").Inc()
				continue
			}

			// Publish meter cleanup signal BEFORE updating in-memory state.
			// Once a session transitions to claiming, it's no longer "active" from the
			// relayer's perspective. This notifies relayers to clear their session meter
			// data and decrement the active sessions metric.
			if m.meterCleanupPublisher != nil {
				if cleanupErr := m.meterCleanupPublisher.PublishMeterCleanup(ctx, session.SessionID); cleanupErr != nil {
					m.logger.Warn().
						Err(cleanupErr).
						Str("session_id", session.SessionID).
						Msg("failed to publish meter cleanup signal")
				} else {
					m.logger.Debug().
						Str("session_id", session.SessionID).
						Msg("published meter cleanup signal for session leaving active state")
				}
			}

			session.State = SessionStateClaiming
			session.LastUpdatedAt = time.Now()
			validClaimingSessions = append(validClaimingSessions, session)
		}

		if len(validClaimingSessions) > 0 {
			// Capture for closure
			capturedSessions := validClaimingSessions
			m.transitionSubpool.Submit(func() {
				m.executeBatchedClaimTransition(ctx, capturedSessions)
			})
		}
	}

	if len(provingSessions) > 0 {
		// Persist state to Redis FIRST to prevent duplicate submissions
		var validProvingSessions []*SessionSnapshot
		for _, session := range provingSessions {
			if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateProving); err != nil {
				m.logger.Error().
					Err(err).
					Str("session_id", session.SessionID).
					Msg("failed to persist proving state to Redis - skipping to prevent duplicate submission")
				sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state_proving").Inc()
				continue
			}
			session.State = SessionStateProving
			session.LastUpdatedAt = time.Now()
			validProvingSessions = append(validProvingSessions, session)
		}

		if len(validProvingSessions) > 0 {
			// Capture for closure
			capturedSessions := validProvingSessions
			m.transitionSubpool.Submit(func() {
				m.executeBatchedProofTransition(ctx, capturedSessions)
			})
		}
	}

	// Terminal states handled individually (no batching benefit)
	// Process pairs: [state, session, state, session, ...]
	for i := 0; i < len(terminalSessions); i += 2 {
		newState := terminalSessions[i].(SessionState)
		session := terminalSessions[i+1].(*SessionSnapshot)

		// Capture for closure
		capturedState := newState
		capturedSession := session
		m.transitionSubpool.Submit(func() {
			m.executeTransition(ctx, capturedSession, capturedState, string(capturedState))
		})
	}
}

// determineTransition determines if a session needs to transition.
func (m *SessionLifecycleManager) determineTransition(
	session *SessionSnapshot,
	currentHeight int64,
	params *sharedtypes.Params,
) (newState SessionState, action string) {
	switch session.State {
	case SessionStateActive:
		// Check if session ended and claim window is approaching
		claimWindowOpen := sharedtypes.GetClaimWindowOpenHeight(params, session.SessionEndHeight)
		claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(params, session.SessionEndHeight)

		// DEBUG: Log window calculation details
		m.logger.Debug().
			Str("session_id", session.SessionID).
			Str("current_state", string(session.State)).
			Int64("current_height", currentHeight).
			Int64("session_start", session.SessionStartHeight).
			Int64("session_end", session.SessionEndHeight).
			Int64("claim_window_open", claimWindowOpen).
			Int64("claim_window_close", claimWindowClose).
			Msg("evaluating active session window")

		// If claim window has passed without claiming, window closed
		if currentHeight >= claimWindowClose {
			return SessionStateClaimWindowClosed, "claim_window_timeout"
		}

		// If we're in the claim window, transition to claiming
		// Protocol handles submission timing via GetEarliestSupplierClaimCommitHeight()
		// Pre-submission checks in lifecycle_callback ensure sufficient time remaining
		if currentHeight >= claimWindowOpen && currentHeight < claimWindowClose {
			return SessionStateClaiming, "claim_window_open"
		}

	case SessionStateClaiming:
		// Transition to claimed shappens after callback succeeds
		claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(params, session.SessionEndHeight)

		// If claim window passed without submitting, window closed (fallback if callback didn't run)
		if currentHeight >= claimWindowClose {
			return SessionStateClaimWindowClosed, "claim_timeout"
		}

	case SessionStateClaimed:
		// IMPORTANT: If we're in claimed state, proof MUST be required.
		// If proof was NOT required, lifecycle callback would have immediately
		// transitioned to probabilistic_proved after claim success.

		proofWindowOpen := sharedtypes.GetProofWindowOpenHeight(params, session.SessionEndHeight)
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(params, session.SessionEndHeight)

		// Missed proof window while proof was required → FAILURE
		if currentHeight >= proofWindowClose {
			return SessionStateProofWindowClosed, "proof_timeout"
		}

		// Proof window open and proof required → GO SUBMIT NOW!
		// Protocol handles submission timing via GetEarliestSupplierProofCommitHeight()
		// Pre-submission checks in lifecycle_callback ensure sufficient time remaining
		if currentHeight >= proofWindowOpen && currentHeight < proofWindowClose {
			return SessionStateProving, "proof_window_open"
		}

		// Still waiting for proof window to open
		// (session remains in claimed state)

	case SessionStateProving:
		// Transition happens after callback succeeds
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(params, session.SessionEndHeight)

		// ❌ OLD BUG: returned SessionStateSettled on timeout (wrong - proof was required but not submitted!)
		// ✅ FIX: Proof window closed = failure (fallback if callback didn't run)
		if currentHeight >= proofWindowClose {
			return SessionStateProofWindowClosed, "proof_timeout"
		}
	}

	return "", ""
}

// executeBatchedClaimTransition executes batched claim transitions.
func (m *SessionLifecycleManager) executeBatchedClaimTransition(ctx context.Context, sessions []*SessionSnapshot) {
	if len(sessions) == 0 {
		return
	}

	m.logger.Debug().
		Int("batch_size", len(sessions)).
		Msg("executing batched claim transition")

	// LATE RELAY DETECTION: Check for pending (unconsumed) relays before claiming
	if m.pendingChecker != nil {
		for _, session := range sessions {
			pendingCount, checkErr := m.pendingChecker.GetPendingRelayCount(ctx, session.SessionID)
			if checkErr != nil {
				m.logger.Debug().
					Err(checkErr).
					Str("session_id", session.SessionID).
					Msg("failed to check pending relays")
				continue
			}

			if pendingCount > 0 {
				m.logger.Warn().
					Str("session_id", session.SessionID).
					Int64("pending_relays", pendingCount).
					Msg("LATE RELAYS DETECTED: relays arrived but not yet consumed before claim")

				// Update metrics
				sessionLateRelays.WithLabelValues(m.config.SupplierAddress, session.SessionID).Add(float64(pendingCount))
				sessionLateRelaysTotal.WithLabelValues(m.config.SupplierAddress).Add(float64(pendingCount))
			}
		}
	}

	// CRITICAL: Refresh session snapshots from Redis to get latest relay counts.
	// The in-memory activeSessions may have stale RelayCount values because
	// SessionCoordinator.OnRelayProcessed() updates Redis directly without
	// updating the in-memory map. This ensures claim decisions use fresh data.
	for _, session := range sessions {
		freshSnapshot, err := m.sessionStore.Get(ctx, session.SessionID)
		if err != nil {
			m.logger.Warn().
				Err(err).
				Str("session_id", session.SessionID).
				Msg("failed to refresh session from Redis, using in-memory data")
			continue
		}
		if freshSnapshot != nil {
			// Update in-memory state with fresh Redis data
			m.activeSessionsMu.Lock()
			session.RelayCount = freshSnapshot.RelayCount
			session.TotalComputeUnits = freshSnapshot.TotalComputeUnits
			session.LastUpdatedAt = freshSnapshot.LastUpdatedAt
			m.activeSessionsMu.Unlock()

			m.logger.Debug().
				Str("session_id", session.SessionID).
				Int64("relay_count", freshSnapshot.RelayCount).
				Uint64("compute_units", freshSnapshot.TotalComputeUnits).
				Msg("refreshed session snapshot from Redis")
		}
	}

	// Call the batched claim callback
	rootHashes, claimErr := m.callback.OnSessionsNeedClaim(ctx, sessions)
	if claimErr != nil {
		m.logger.Error().Err(claimErr).Int("batch_size", len(sessions)).Msg("batched claim callback failed")
		claimErrors.WithLabelValues(m.config.SupplierAddress, "callback_failed").Inc()
		return
	}

	if len(rootHashes) != len(sessions) {
		m.logger.Error().
			Int("expected", len(sessions)).
			Int("got", len(rootHashes)).
			Msg("root hash count mismatch")
		return
	}

	// Update all sessions with their root hashes and transition to claimed
	for i, session := range sessions {
		session.ClaimedRootHash = rootHashes[i]

		// Update session state
		m.activeSessionsMu.Lock()
		session.State = SessionStateClaimed
		session.LastUpdatedAt = time.Now()
		m.activeSessionsMu.Unlock()

		// Persist the state change
		if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateClaimed); err != nil {
			m.logger.Error().
				Err(err).
				Str("session_id", session.SessionID).
				Msg("failed to persist claimed state")
			sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state").Inc()
			continue
		}

		// Record the transition
		sessionStateTransitions.WithLabelValues(
			m.config.SupplierAddress,
			session.ServiceID,
			string(SessionStateClaiming),
			string(SessionStateClaimed),
		).Inc()
	}
}

// executeBatchedProofTransition executes batched proof transitions.
func (m *SessionLifecycleManager) executeBatchedProofTransition(ctx context.Context, sessions []*SessionSnapshot) {
	if len(sessions) == 0 {
		return
	}

	m.logger.Debug().
		Int("batch_size", len(sessions)).
		Msg("executing batched proof transition")

	// Call the batched proof callback
	if proofErr := m.callback.OnSessionsNeedProof(ctx, sessions); proofErr != nil {
		m.logger.Error().Err(proofErr).Int("batch_size", len(sessions)).Msg("batched proof callback failed")
		proofErrors.WithLabelValues(m.config.SupplierAddress, "callback_failed").Inc()
		return
	}

	// Update all sessions and transition to proved
	for _, session := range sessions {
		// Update session state
		m.activeSessionsMu.Lock()
		session.State = SessionStateProved
		session.LastUpdatedAt = time.Now()
		m.activeSessionsMu.Unlock()

		// Persist the state change
		if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateProved); err != nil {
			m.logger.Error().
				Err(err).
				Str("session_id", session.SessionID).
				Msg("failed to persist proved state")
			sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state").Inc()
			continue
		}

		// Record the transition
		sessionStateTransitions.WithLabelValues(
			m.config.SupplierAddress,
			session.ServiceID,
			string(SessionStateProving),
			string(SessionStateProved),
		).Inc()

		// Call OnSessionProved for cleanup (stream deletion, SMST cleanup, metrics)
		if proveErr := m.callback.OnSessionProved(ctx, session); proveErr != nil {
			m.logger.Warn().
				Err(proveErr).
				Str("session_id", session.SessionID).
				Msg("proved callback failed in batched transition")
		}

		// Remove from active tracking
		m.activeSessionsMu.Lock()
		delete(m.activeSessions, session.SessionID)
		m.activeSessionsMu.Unlock()

		m.logger.Info().
			Str("session_id", session.SessionID).
			Int64("relay_count", session.RelayCount).
			Msg("session lifecycle complete (batched)")
	}
}

// executeTransition executes a state transition for a session (non-batched transitions).
func (m *SessionLifecycleManager) executeTransition(
	ctx context.Context,
	session *SessionSnapshot,
	newState SessionState,
	action string,
) {
	oldState := session.State

	// Create session-scoped logger for this transition
	sessionLogger := logging.WithSession(m.logger, session.SessionID)

	sessionLogger.Debug().
		Str(logging.FieldOldState, string(oldState)).
		Str(logging.FieldNewState, string(newState)).
		Str(logging.FieldAction, action).
		Msg("executing session transition")

	var err error

	// Execute terminal state callbacks
	switch newState {
	case SessionStateProved:
		if err := m.callback.OnSessionProved(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("proved callback failed")
		}

	case SessionStateProbabilisticProved:
		if err := m.callback.OnProbabilisticProved(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("probabilistic proved callback failed")
		}

	case SessionStateClaimWindowClosed:
		if err := m.callback.OnClaimWindowClosed(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("claim window closed callback failed")
		}

	case SessionStateClaimTxError:
		if err := m.callback.OnClaimTxError(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("claim tx error callback failed")
		}

	case SessionStateProofWindowClosed:
		if err := m.callback.OnProofWindowClosed(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("proof window closed callback failed")
		}

	case SessionStateProofTxError:
		if err := m.callback.OnProofTxError(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("proof tx error callback failed")
		}
	}

	// Update session state
	m.activeSessionsMu.Lock()
	session.State = newState
	session.LastUpdatedAt = time.Now()
	m.activeSessionsMu.Unlock()

	// Persist the state change
	err = m.sessionStore.UpdateState(ctx, session.SessionID, newState)
	if err != nil {
		sessionLogger.Error().Err(err).Msg("failed to persist state change")
		sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state").Inc()
		return
	}

	// Record the transition
	sessionStateTransitions.WithLabelValues(
		m.config.SupplierAddress,
		session.ServiceID,
		string(oldState),
		string(newState),
	).Inc()

	// Remove terminal sessions from active tracking
	if newState.IsTerminal() {
		m.activeSessionsMu.Lock()
		delete(m.activeSessions, session.SessionID)
		m.activeSessionsMu.Unlock()

		sessionLogger.Info().
			Str(logging.FieldNewState, string(newState)).
			Int64(logging.FieldCount, session.RelayCount).
			Msg("session lifecycle complete")
	}
}

// HasPendingSessions returns true if there are sessions not yet settled.
func (m *SessionLifecycleManager) HasPendingSessions() bool {
	m.activeSessionsMu.RLock()
	defer m.activeSessionsMu.RUnlock()
	return len(m.activeSessions) > 0
}

// GetPendingSessionCount returns the count of sessions pending settlement.
func (m *SessionLifecycleManager) GetPendingSessionCount() int {
	m.activeSessionsMu.RLock()
	defer m.activeSessionsMu.RUnlock()
	return len(m.activeSessions)
}

// WaitForSettlement waits for all pending sessions to settle.
func (m *SessionLifecycleManager) WaitForSettlement(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !m.HasPendingSessions() {
				return nil
			}

			m.logger.Debug().
				Int("pending", m.GetPendingSessionCount()).
				Msg("waiting for sessions to settle")
		}
	}
}

// Close gracefully shuts down the lifecycle manager.
func (m *SessionLifecycleManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	if m.cancelFn != nil {
		m.cancelFn()
	}

	m.wg.Wait()

	// Stop transition subpool gracefully (drains queued tasks)
	if m.transitionSubpool != nil {
		m.transitionSubpool.StopAndWait()
	}

	m.logger.Info().Msg("session lifecycle manager closed")
	return nil
}

// SessionWindow represents the timing windows for a session.
type SessionWindow struct {
	SessionEndHeight int64
	GracePeriodEnd   int64
	ClaimWindowOpen  int64
	ClaimWindowClose int64
	ProofWindowOpen  int64
	ProofWindowClose int64
}

// CalculateSessionWindow calculates all timing windows for a session.
func CalculateSessionWindow(params *sharedtypes.Params, sessionEndHeight int64) SessionWindow {
	return SessionWindow{
		SessionEndHeight: sessionEndHeight,
		GracePeriodEnd:   sessionEndHeight + int64(params.GetGracePeriodEndOffsetBlocks()),
		ClaimWindowOpen:  sharedtypes.GetClaimWindowOpenHeight(params, sessionEndHeight),
		ClaimWindowClose: sharedtypes.GetClaimWindowCloseHeight(params, sessionEndHeight),
		ProofWindowOpen:  sharedtypes.GetProofWindowOpenHeight(params, sessionEndHeight),
		ProofWindowClose: sharedtypes.GetProofWindowCloseHeight(params, sessionEndHeight),
	}
}

// IsInClaimWindow returns true if the current height is within the claim window.
func (w SessionWindow) IsInClaimWindow(currentHeight int64) bool {
	return currentHeight >= w.ClaimWindowOpen && currentHeight < w.ClaimWindowClose
}

// IsInProofWindow returns true if the current height is within the proof window.
func (w SessionWindow) IsInProofWindow(currentHeight int64) bool {
	return currentHeight >= w.ProofWindowOpen && currentHeight < w.ProofWindowClose
}

// BlocksUntilClaimWindowClose returns blocks remaining until claim window closes.
func (w SessionWindow) BlocksUntilClaimWindowClose(currentHeight int64) int64 {
	if currentHeight >= w.ClaimWindowClose {
		return 0
	}
	return w.ClaimWindowClose - currentHeight
}

// BlocksUntilProofWindowClose returns blocks remaining until proof window closes.
func (w SessionWindow) BlocksUntilProofWindowClose(currentHeight int64) int64 {
	if currentHeight >= w.ProofWindowClose {
		return 0
	}
	return w.ProofWindowClose - currentHeight
}
