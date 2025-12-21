package miner

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	pond "github.com/alitto/pond/v2"
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

	// ClaimSubmissionBuffer is blocks before window close to start claiming.
	// This provides buffer time for transaction confirmation.
	// Default: 2
	ClaimSubmissionBuffer int64

	// ProofSubmissionBuffer is blocks before window close to start proving.
	// Default: 2
	ProofSubmissionBuffer int64

	// MaxConcurrentTransitions is the max number of sessions transitioning at once.
	// Default: 10
	MaxConcurrentTransitions int

	// CheckInterval is the time interval for checking session transitions.
	// If 0, defaults to 30 * time.Second.
	// For tests, set to a faster value like 100 * time.Millisecond.
	CheckInterval time.Duration
}

// DefaultSessionLifecycleConfig returns sensible defaults.
func DefaultSessionLifecycleConfig() SessionLifecycleConfig {
	return SessionLifecycleConfig{
		CheckIntervalBlocks:      1,
		ClaimSubmissionBuffer:    2,
		ProofSubmissionBuffer:    2,
		MaxConcurrentTransitions: 10,
	}
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

	// OnSessionSettled is called when a session is fully settled.
	OnSessionSettled(ctx context.Context, snapshot *SessionSnapshot) error

	// OnSessionExpired is called when a session expires without settling.
	OnSessionExpired(ctx context.Context, snapshot *SessionSnapshot, reason string) error
}

// PendingRelayChecker checks for pending (unconsumed) relays in session streams.
// This is used to detect late-arriving relays before claim submission.
type PendingRelayChecker interface {
	// GetPendingRelayCount returns the number of pending relays for a session stream.
	// Returns 0 if the stream doesn't exist or has no pending messages.
	GetPendingRelayCount(ctx context.Context, sessionID string) (int64, error)
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
	if config.ClaimSubmissionBuffer <= 0 {
		config.ClaimSubmissionBuffer = 2
	}
	if config.ProofSubmissionBuffer <= 0 {
		config.ProofSubmissionBuffer = 2
	}
	if config.MaxConcurrentTransitions <= 0 {
		// Dynamic allocation: 25% of (NumCPU * 2) for mixed workload (crypto + network)
		// Miner is heavy lift, so we use NumCPU * 2 as base
		numCPU := runtime.NumCPU()
		config.MaxConcurrentTransitions = int(float64(numCPU*2) * 0.25)
		if config.MaxConcurrentTransitions < 2 {
			config.MaxConcurrentTransitions = 2 // Minimum 2 workers
		}
	}

	// Create transition subpool from master pool
	transitionSubpool := workerPool.NewSubpool(config.MaxConcurrentTransitions)

	logger.Debug().
		Int("transition_workers", config.MaxConcurrentTransitions).
		Int("num_cpu", runtime.NumCPU()).
		Msg("created transition subpool with dynamic CPU-based allocation")

	return &SessionLifecycleManager{
		logger:            logging.ForSupplierComponent(logger, logging.ComponentSessionLifecycle, config.SupplierAddress),
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
	sessions, err := m.sessionStore.GetBySupplier(ctx, m.config.SupplierAddress)
	if err != nil {
		return err
	}

	m.activeSessionsMu.Lock()
	defer m.activeSessionsMu.Unlock()

	for _, session := range sessions {
		// Only track sessions that aren't settled or expired
		if session.State != SessionStateSettled && session.State != SessionStateExpired {
			m.activeSessions[session.SessionID] = session
			sessionSnapshotsLoaded.WithLabelValues(m.config.SupplierAddress).Inc()
		} else {
			// Log and track metrics for skipped sessions (settled or expired)
			// These are historical events from before restart, use DEBUG level
			sessionSnapshotsSkippedAtStartup.WithLabelValues(m.config.SupplierAddress, string(session.State)).Inc()

			// Differentiate between settled (success) and expired (failure) for operator clarity
			if session.State == SessionStateSettled {
				m.logger.Debug().
					Str("session_id", session.SessionID).
					Str("service_id", session.ServiceID).
					Int64("session_end_height", session.SessionEndHeight).
					Int64("relay_count", session.RelayCount).
					Uint64("compute_units", session.TotalComputeUnits).
					Msg("skipping session at startup: already settled (rewards claimed)")
			} else {
				// Expired without settling - historical data (actual WARN was logged when it expired)
				// Only mention "rewards lost" if there were actually relays
				msg := "skipping session at startup: expired with 0 relays"
				if session.RelayCount > 0 {
					msg = "skipping session at startup: expired without settling (rewards lost)"
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
	sessions := make([]*SessionSnapshot, 0, len(m.activeSessions))
	for _, session := range m.activeSessions {
		sessions = append(sessions, session)
	}
	m.activeSessionsMu.RUnlock()

	params := m.getSharedParams()
	if params == nil {
		m.logger.Warn().Msg("shared params not available, skipping transition check")
		return
	}

	// Group sessions by transition type (claiming, proving, settled, expired)
	var claimingSessions []*SessionSnapshot
	var provingSessions []*SessionSnapshot
	var settledSessions []*SessionSnapshot
	var expiredSessions []*SessionSnapshot

	for _, session := range sessions {
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
		case SessionStateSettled:
			settledSessions = append(settledSessions, session)
		case SessionStateExpired:
			expiredSessions = append(expiredSessions, session)
		}
	}

	// Execute batched transitions using pond subpool
	// Non-blocking submission with unbounded queue (tasks queue if workers are busy)
	//
	// CRITICAL: Update session states SYNCHRONOUSLY before submitting async callbacks.
	// This prevents race conditions where the next block event sees stale state
	// and incorrectly expires a session that's already transitioning.
	if len(claimingSessions) > 0 {
		// Update states synchronously to prevent race conditions
		m.activeSessionsMu.Lock()
		for _, session := range claimingSessions {
			session.State = SessionStateClaiming
			session.LastUpdatedAt = time.Now()
		}
		m.activeSessionsMu.Unlock()

		// Capture for closure
		capturedSessions := claimingSessions
		m.transitionSubpool.Submit(func() {
			m.executeBatchedClaimTransition(ctx, capturedSessions)
		})
	}

	if len(provingSessions) > 0 {
		// Update states synchronously to prevent race conditions
		m.activeSessionsMu.Lock()
		for _, session := range provingSessions {
			session.State = SessionStateProving
			session.LastUpdatedAt = time.Now()
		}
		m.activeSessionsMu.Unlock()

		// Capture for closure
		capturedSessions := provingSessions
		m.transitionSubpool.Submit(func() {
			m.executeBatchedProofTransition(ctx, capturedSessions)
		})
	}

	// Settled and expired are still handled individually (no batching benefit)
	for _, session := range settledSessions {
		// Capture for closure
		capturedSession := session
		m.transitionSubpool.Submit(func() {
			m.executeTransition(ctx, capturedSession, SessionStateSettled, "settled")
		})
	}

	for _, session := range expiredSessions {
		// Capture for closure
		capturedSession := session
		m.transitionSubpool.Submit(func() {
			m.executeTransition(ctx, capturedSession, SessionStateExpired, "expired")
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
			Int64("claim_submission_buffer", m.config.ClaimSubmissionBuffer).
			Msg("evaluating active session window")

		// If claim window has passed without claiming, session expired
		if currentHeight >= claimWindowClose {
			return SessionStateExpired, "claim_window_missed"
		}

		// If we're in the claim window (including buffer zone), transition to claiming
		// The buffer zone is for giving tx confirmation time, but if we're late starting,
		// we should still try to claim rather than just waiting to expire
		if currentHeight >= claimWindowOpen && currentHeight < claimWindowClose {
			if currentHeight >= claimWindowClose-m.config.ClaimSubmissionBuffer {
				// We're in the buffer zone - log warning but still try to claim
				m.logger.Warn().
					Str("session_id", session.SessionID).
					Int64("current_height", currentHeight).
					Int64("claim_window_close", claimWindowClose).
					Int64("blocks_remaining", claimWindowClose-currentHeight).
					Msg("LATE CLAIM: starting claim in buffer zone - tx may not confirm in time")
				return SessionStateClaiming, "claim_window_late"
			}
			return SessionStateClaiming, "claim_window_open"
		}

	case SessionStateClaiming:
		// Transition to claimed happens after callback succeeds
		claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(params, session.SessionEndHeight)

		// If claim window passed without submitting, expired
		if currentHeight >= claimWindowClose {
			return SessionStateExpired, "claim_failed"
		}

	case SessionStateClaimed:
		// Check if proof window has opened
		proofWindowOpen := sharedtypes.GetProofWindowOpenHeight(params, session.SessionEndHeight)
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(params, session.SessionEndHeight)

		// If proof window passed without proving, session may still settle
		// (proof is optional if not selected for proof requirement)
		if currentHeight >= proofWindowClose {
			return SessionStateSettled, "proof_window_passed"
		}

		// If we're in the proof window (including buffer zone), transition to proving
		// The buffer zone is for giving tx confirmation time, but if we're late starting,
		// we should still try to prove rather than just waiting
		if currentHeight >= proofWindowOpen && currentHeight < proofWindowClose {
			if currentHeight >= proofWindowClose-m.config.ProofSubmissionBuffer {
				// We're in the buffer zone - log warning but still try to prove
				m.logger.Warn().
					Str("session_id", session.SessionID).
					Int64("current_height", currentHeight).
					Int64("proof_window_close", proofWindowClose).
					Int64("blocks_remaining", proofWindowClose-currentHeight).
					Msg("LATE PROOF: starting proof in buffer zone - tx may not confirm in time")
				return SessionStateProving, "proof_window_late"
			}
			return SessionStateProving, "proof_window_open"
		}

	case SessionStateProving:
		// Transition to settled happens after callback succeeds
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(params, session.SessionEndHeight)

		// If proof window passed, check if settled
		if currentHeight >= proofWindowClose {
			return SessionStateSettled, "proof_submitted"
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

	// Update all sessions and transition to settled
	for _, session := range sessions {
		// Update session state
		m.activeSessionsMu.Lock()
		session.State = SessionStateSettled
		session.LastUpdatedAt = time.Now()
		m.activeSessionsMu.Unlock()

		// Persist the state change
		if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateSettled); err != nil {
			m.logger.Error().
				Err(err).
				Str("session_id", session.SessionID).
				Msg("failed to persist settled state")
			sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state").Inc()
			continue
		}

		// Record the transition
		sessionStateTransitions.WithLabelValues(
			m.config.SupplierAddress,
			string(SessionStateProving),
			string(SessionStateSettled),
		).Inc()

		// Call OnSessionSettled for cleanup (stream deletion, SMST cleanup, metrics)
		if settleErr := m.callback.OnSessionSettled(ctx, session); settleErr != nil {
			m.logger.Warn().
				Err(settleErr).
				Str("session_id", session.SessionID).
				Msg("settle callback failed in batched transition")
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

	switch newState {
	case SessionStateSettled:
		if settleErr := m.callback.OnSessionSettled(ctx, session); settleErr != nil {
			sessionLogger.Warn().Err(settleErr).Msg("settle callback failed")
		}

	case SessionStateExpired:
		if expireErr := m.callback.OnSessionExpired(ctx, session, action); expireErr != nil {
			sessionLogger.Warn().Err(expireErr).Msg("expire callback failed")
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
		string(oldState),
		string(newState),
	).Inc()

	// Remove settled/expired sessions from active tracking
	if newState == SessionStateSettled || newState == SessionStateExpired {
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
