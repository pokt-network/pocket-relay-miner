package relayer

import (
	"context"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// SessionMonitor monitors global session boundaries and notifies all WebSocket
// connections when the session (including grace period) expires.
//
// This is efficient because all sessions in Pocket Network are globally synchronized:
// - All sessions start at the same block height
// - All sessions end at the same block height
// - Grace period is the same for all sessions
//
// One SessionMonitor serves thousands of WebSocket connections.
type SessionMonitor struct {
	logger              logging.Logger
	blockHeightProvider func() int64
	gracePeriodBlocks   int64

	// Session tracking
	currentSessionEnd int64
	mu                sync.RWMutex

	// Bridge registration
	bridges   map[*WebSocketBridge]struct{}
	bridgesMu sync.RWMutex

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	started  bool
}

// NewSessionMonitor creates a new global session monitor.
func NewSessionMonitor(
	logger logging.Logger,
	blockHeightProvider func() int64,
	gracePeriodBlocks int64,
) *SessionMonitor {
	ctx, cancelFn := context.WithCancel(context.Background())

	return &SessionMonitor{
		logger:              logger.With().Str(logging.FieldComponent, "session_monitor").Logger(),
		blockHeightProvider: blockHeightProvider,
		gracePeriodBlocks:   gracePeriodBlocks,
		bridges:             make(map[*WebSocketBridge]struct{}),
		ctx:                 ctx,
		cancelFn:            cancelFn,
	}
}

// Start begins monitoring session expiration.
// Safe to call multiple times - only starts once.
func (sm *SessionMonitor) Start() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.started {
		return
	}

	sm.started = true
	sm.wg.Add(1)

	go logging.RecoverGoRoutine(sm.logger, "session_monitor_loop", func(ctx context.Context) {
		sm.monitorLoop()
	})(sm.ctx)

	sm.logger.Info().
		Int64("grace_period_blocks", sm.gracePeriodBlocks).
		Msg("global session monitor started")
}

// RegisterBridge registers a WebSocket bridge to receive session expiration notifications.
func (sm *SessionMonitor) RegisterBridge(bridge *WebSocketBridge, sessionEndHeight int64) {
	sm.bridgesMu.Lock()
	defer sm.bridgesMu.Unlock()

	sm.bridges[bridge] = struct{}{}

	sm.mu.Lock()
	// Update current session end if this is newer (shouldn't happen, but defensive)
	if sessionEndHeight > sm.currentSessionEnd {
		sm.currentSessionEnd = sessionEndHeight
		sm.logger.Debug().
			Int64("session_end_height", sessionEndHeight).
			Msg("updated session end height")
	}
	sm.mu.Unlock()

	sm.logger.Debug().
		Int("total_bridges", len(sm.bridges)).
		Int64("session_end_height", sessionEndHeight).
		Msg("registered WebSocket bridge")
}

// UnregisterBridge removes a WebSocket bridge from monitoring.
func (sm *SessionMonitor) UnregisterBridge(bridge *WebSocketBridge) {
	sm.bridgesMu.Lock()
	defer sm.bridgesMu.Unlock()

	delete(sm.bridges, bridge)

	sm.logger.Debug().
		Int("total_bridges", len(sm.bridges)).
		Msg("unregistered WebSocket bridge")
}

// monitorLoop checks session expiration every 2 seconds.
// With 30s mainnet blocks, this detects expiration within 2s of grace period ending.
func (sm *SessionMonitor) monitorLoop() {
	defer sm.wg.Done()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sm.ctx.Done():
			sm.logger.Debug().Msg("session monitor stopped")
			return

		case <-ticker.C:
			sm.checkSessionExpiration()
		}
	}
}

// checkSessionExpiration checks if the current session has expired and notifies bridges.
func (sm *SessionMonitor) checkSessionExpiration() {
	sm.mu.RLock()
	sessionEndHeight := sm.currentSessionEnd
	sm.mu.RUnlock()

	if sessionEndHeight == 0 {
		// No active session yet
		return
	}

	currentHeight := sm.blockHeightProvider()
	graceEndHeight := sessionEndHeight + sm.gracePeriodBlocks
	blocksRemaining := graceEndHeight - currentHeight

	sm.logger.Debug().
		Int64("current_height", currentHeight).
		Int64("session_end_height", sessionEndHeight).
		Int64("grace_end_height", graceEndHeight).
		Int64("blocks_remaining", blocksRemaining).
		Msg("checking session expiration")

	// Check if grace period has expired
	if currentHeight > graceEndHeight {
		sm.logger.Warn().
			Int64("current_height", currentHeight).
			Int64("session_end_height", sessionEndHeight).
			Int64("grace_end_height", graceEndHeight).
			Int64("blocks_over", currentHeight-graceEndHeight).
			Msg("SESSION EXPIRED - notifying all WebSocket bridges")

		sm.notifyBridges(sessionEndHeight, graceEndHeight, currentHeight)

		// Reset for next session
		sm.mu.Lock()
		sm.currentSessionEnd = 0
		sm.mu.Unlock()
	}
}

// notifyBridges sends session expiration notifications to all registered bridges.
func (sm *SessionMonitor) notifyBridges(sessionEndHeight, graceEndHeight, currentHeight int64) {
	sm.bridgesMu.RLock()
	defer sm.bridgesMu.RUnlock()

	notified := 0
	for bridge := range sm.bridges {
		// Only notify bridges for this specific session
		if bridge.sessionEndHeight == sessionEndHeight {
			// Signal the bridge to close via its context
			// The bridge's sessionExpiredChan will be closed
			go bridge.handleSessionExpiration(sessionEndHeight, graceEndHeight, currentHeight)
			notified++
		}
	}

	sm.logger.Info().
		Int("bridges_notified", notified).
		Int("total_bridges", len(sm.bridges)).
		Int64("session_end_height", sessionEndHeight).
		Msg("session expiration notifications sent")
}

// Close stops the session monitor.
func (sm *SessionMonitor) Close() error {
	sm.cancelFn()
	sm.wg.Wait()
	sm.logger.Info().Msg("session monitor stopped")
	return nil
}
