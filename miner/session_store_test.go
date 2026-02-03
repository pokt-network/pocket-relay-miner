//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/suite"

	"github.com/pokt-network/pocket-relay-miner/testutil"
)

// SessionStoreTestSuite tests the RedisSessionStore implementation.
// This characterizes how session metadata is persisted in Redis for HA recovery.
type SessionStoreTestSuite struct {
	testutil.RedisTestSuite
	store *RedisSessionStore
}

// SetupTest creates a fresh session store for each test.
func (s *SessionStoreTestSuite) SetupTest() {
	// Call parent to flush Redis
	s.RedisTestSuite.SetupTest()

	// Create session store with test config
	config := SessionStoreConfig{
		KeyPrefix:       "ha:miner:sessions",
		SupplierAddress: testutil.TestSupplierAddress(),
		SessionTTL:      1 * time.Hour,
	}

	logger := zerolog.Nop()
	s.store = NewRedisSessionStore(logger, s.RedisClient, config)
}

// TearDownTest cleans up the session store.
func (s *SessionStoreTestSuite) TearDownTest() {
	if s.store != nil {
		_ = s.store.Close()
	}
}

// createTestSnapshot creates a test session snapshot with given session ID.
func (s *SessionStoreTestSuite) createTestSnapshot(sessionID string, state SessionState) *SessionSnapshot {
	return &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: testutil.TestSupplierAddress(),
		ServiceID:               testutil.TestServiceID,
		ApplicationAddress:      testutil.TestAppAddress(),
		SessionStartHeight:      100,
		SessionEndHeight:        104,
		State:                   state,
		RelayCount:              5,
		TotalComputeUnits:       500,
		ClaimedRootHash:         nil,
		ClaimTxHash:             "",
		ProofTxHash:             "",
		LastWALEntryID:          "",
		LastUpdatedAt:           time.Now(),
		CreatedAt:               time.Now(),
	}
}

// TestSessionStore_SaveAndGet verifies session metadata can be saved and retrieved.
func (s *SessionStoreTestSuite) TestSessionStore_SaveAndGet() {
	ctx := context.Background()
	snapshot := s.createTestSnapshot("session_001", SessionStateActive)

	// Save snapshot
	err := s.store.Save(ctx, snapshot)
	s.Require().NoError(err)

	// Retrieve snapshot
	retrieved, err := s.store.Get(ctx, "session_001")
	s.Require().NoError(err)
	s.Require().NotNil(retrieved)

	// Verify fields match
	s.Require().Equal(snapshot.SessionID, retrieved.SessionID)
	s.Require().Equal(snapshot.SupplierOperatorAddress, retrieved.SupplierOperatorAddress)
	s.Require().Equal(snapshot.ServiceID, retrieved.ServiceID)
	s.Require().Equal(snapshot.ApplicationAddress, retrieved.ApplicationAddress)
	s.Require().Equal(snapshot.SessionStartHeight, retrieved.SessionStartHeight)
	s.Require().Equal(snapshot.SessionEndHeight, retrieved.SessionEndHeight)
	s.Require().Equal(snapshot.State, retrieved.State)
	s.Require().Equal(snapshot.RelayCount, retrieved.RelayCount)
	s.Require().Equal(snapshot.TotalComputeUnits, retrieved.TotalComputeUnits)
}

// TestSessionStore_UpdateState verifies session state can be updated.
func (s *SessionStoreTestSuite) TestSessionStore_UpdateState() {
	ctx := context.Background()
	snapshot := s.createTestSnapshot("session_002", SessionStateActive)

	// Save initial snapshot
	err := s.store.Save(ctx, snapshot)
	s.Require().NoError(err)

	// Update state
	err = s.store.UpdateState(ctx, "session_002", SessionStateClaiming)
	s.Require().NoError(err)

	// Retrieve and verify state changed
	retrieved, err := s.store.Get(ctx, "session_002")
	s.Require().NoError(err)
	s.Require().Equal(SessionStateClaiming, retrieved.State)
}

// TestSessionStore_ListByState verifies sessions can be filtered by state.
func (s *SessionStoreTestSuite) TestSessionStore_ListByState() {
	ctx := context.Background()

	// Create sessions in different states
	active1 := s.createTestSnapshot("session_003_active1", SessionStateActive)
	active2 := s.createTestSnapshot("session_004_active2", SessionStateActive)
	claiming := s.createTestSnapshot("session_005_claiming", SessionStateClaiming)
	claimed := s.createTestSnapshot("session_006_claimed", SessionStateClaimed)

	// Save all
	s.Require().NoError(s.store.Save(ctx, active1))
	s.Require().NoError(s.store.Save(ctx, active2))
	s.Require().NoError(s.store.Save(ctx, claiming))
	s.Require().NoError(s.store.Save(ctx, claimed))

	// List active sessions
	activeSessions, err := s.store.GetByState(ctx, SessionStateActive)
	s.Require().NoError(err)
	s.Require().Len(activeSessions, 2, "should have 2 active sessions")

	// List claiming sessions
	claimingSessions, err := s.store.GetByState(ctx, SessionStateClaiming)
	s.Require().NoError(err)
	s.Require().Len(claimingSessions, 1, "should have 1 claiming session")

	// List claimed sessions
	claimedSessions, err := s.store.GetByState(ctx, SessionStateClaimed)
	s.Require().NoError(err)
	s.Require().Len(claimedSessions, 1, "should have 1 claimed session")
}

// TestSessionStore_DeleteSession verifies session can be deleted and removed from indexes.
func (s *SessionStoreTestSuite) TestSessionStore_DeleteSession() {
	ctx := context.Background()
	snapshot := s.createTestSnapshot("session_007", SessionStateActive)

	// Save snapshot
	err := s.store.Save(ctx, snapshot)
	s.Require().NoError(err)

	// Verify it exists
	retrieved, err := s.store.Get(ctx, "session_007")
	s.Require().NoError(err)
	s.Require().NotNil(retrieved)

	// Delete session
	err = s.store.Delete(ctx, "session_007")
	s.Require().NoError(err)

	// Verify it's gone
	retrieved, err = s.store.Get(ctx, "session_007")
	s.Require().NoError(err)
	s.Require().Nil(retrieved, "session should be deleted")

	// Verify not in state index
	sessions, err := s.store.GetByState(ctx, SessionStateActive)
	s.Require().NoError(err)
	for _, sess := range sessions {
		s.Require().NotEqual("session_007", sess.SessionID, "deleted session should not be in state index")
	}
}

// TestSessionStore_NonExistentSession verifies Get returns nil for non-existent sessions.
func (s *SessionStoreTestSuite) TestSessionStore_NonExistentSession() {
	ctx := context.Background()

	// Try to get a session that doesn't exist
	retrieved, err := s.store.Get(ctx, "nonexistent_session")
	s.Require().NoError(err)
	s.Require().Nil(retrieved, "non-existent session should return nil")
}

// TestSessionStore_ConcurrentUpdates verifies concurrent updates to different sessions are safe.
func (s *SessionStoreTestSuite) TestSessionStore_ConcurrentUpdates() {
	ctx := context.Background()

	var wg sync.WaitGroup
	goroutines := 10
	wg.Add(goroutines)

	// Multiple goroutines save different sessions concurrently
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			sessionID := fmt.Sprintf("concurrent_session_%d", idx)
			snapshot := s.createTestSnapshot(sessionID, SessionStateActive)
			_ = s.store.Save(ctx, snapshot)
		}(i)
	}

	wg.Wait()

	// Verify all sessions were saved
	sessions, err := s.store.GetBySupplier(ctx)
	s.Require().NoError(err)
	s.Require().Len(sessions, goroutines, "all concurrent sessions should be saved")
}

// TestSessionStore_StateIndexConsistency verifies state index is updated when state changes.
func (s *SessionStoreTestSuite) TestSessionStore_StateIndexConsistency() {
	ctx := context.Background()
	snapshot := s.createTestSnapshot("session_008", SessionStateActive)

	// Save in active state
	err := s.store.Save(ctx, snapshot)
	s.Require().NoError(err)

	// Verify in active index
	activeSessions, err := s.store.GetByState(ctx, SessionStateActive)
	s.Require().NoError(err)
	s.Require().Len(activeSessions, 1)
	s.Require().Equal("session_008", activeSessions[0].SessionID)

	// Update to claiming state
	err = s.store.UpdateState(ctx, "session_008", SessionStateClaiming)
	s.Require().NoError(err)

	// Verify removed from active index
	activeSessions, err = s.store.GetByState(ctx, SessionStateActive)
	s.Require().NoError(err)
	s.Require().Len(activeSessions, 0, "session should be removed from active index")

	// Verify in claiming index
	claimingSessions, err := s.store.GetByState(ctx, SessionStateClaiming)
	s.Require().NoError(err)
	s.Require().Len(claimingSessions, 1)
	s.Require().Equal("session_008", claimingSessions[0].SessionID)
}

// TestSessionStore_IncrementRelayCount verifies relay count can be incremented.
func (s *SessionStoreTestSuite) TestSessionStore_IncrementRelayCount() {
	ctx := context.Background()
	snapshot := s.createTestSnapshot("session_009", SessionStateActive)
	snapshot.RelayCount = 0
	snapshot.TotalComputeUnits = 0

	// Save initial snapshot
	err := s.store.Save(ctx, snapshot)
	s.Require().NoError(err)

	// Increment relay count 3 times
	for i := 0; i < 3; i++ {
		err = s.store.IncrementRelayCount(ctx, "session_009", 100)
		s.Require().NoError(err)
	}

	// Verify counts
	retrieved, err := s.store.Get(ctx, "session_009")
	s.Require().NoError(err)
	s.Require().Equal(int64(3), retrieved.RelayCount)
	s.Require().Equal(uint64(300), retrieved.TotalComputeUnits)
}

// TestSessionStore_IncrementRelayCount_TerminalState verifies relay count cannot be incremented in terminal states.
func (s *SessionStoreTestSuite) TestSessionStore_IncrementRelayCount_TerminalState() {
	ctx := context.Background()

	// Test all terminal states
	terminalStates := []SessionState{
		SessionStateProved,
		SessionStateProbabilisticProved,
		SessionStateClaimWindowClosed,
		SessionStateClaimTxError,
		SessionStateProofWindowClosed,
		SessionStateProofTxError,
	}

	for _, state := range terminalStates {
		sessionID := fmt.Sprintf("terminal_%s", state)
		snapshot := s.createTestSnapshot(sessionID, state)

		// Save in terminal state
		err := s.store.Save(ctx, snapshot)
		s.Require().NoError(err)

		// Try to increment - should fail
		err = s.store.IncrementRelayCount(ctx, sessionID, 100)
		s.Require().Error(err, "should not allow increment in terminal state %s", state)
	}
}

// TestSessionStore_UpdateWALPosition verifies WAL position can be updated.
func (s *SessionStoreTestSuite) TestSessionStore_UpdateWALPosition() {
	ctx := context.Background()
	snapshot := s.createTestSnapshot("session_010", SessionStateActive)
	snapshot.LastWALEntryID = ""

	// Save initial snapshot
	err := s.store.Save(ctx, snapshot)
	s.Require().NoError(err)

	// Update WAL position
	err = s.store.UpdateWALPosition(ctx, "session_010", "1234567890-0")
	s.Require().NoError(err)

	// Verify updated
	retrieved, err := s.store.Get(ctx, "session_010")
	s.Require().NoError(err)
	s.Require().Equal("1234567890-0", retrieved.LastWALEntryID)
}

// TestSessionStore_UpdateSettlementMetadata verifies settlement metadata can be updated.
func (s *SessionStoreTestSuite) TestSessionStore_UpdateSettlementMetadata() {
	ctx := context.Background()
	snapshot := s.createTestSnapshot("session_011", SessionStateProved)

	// Save initial snapshot
	err := s.store.Save(ctx, snapshot)
	s.Require().NoError(err)

	// Update settlement metadata
	err = s.store.UpdateSettlementMetadata(ctx, "session_011", "settled_proven", 1000)
	s.Require().NoError(err)

	// Verify updated
	retrieved, err := s.store.Get(ctx, "session_011")
	s.Require().NoError(err)
	s.Require().NotNil(retrieved.SettlementOutcome)
	s.Require().Equal("settled_proven", *retrieved.SettlementOutcome)
	s.Require().NotNil(retrieved.SettlementHeight)
	s.Require().Equal(int64(1000), *retrieved.SettlementHeight)
}

// TestSessionStore_GetBySupplier verifies all sessions for a supplier can be retrieved.
func (s *SessionStoreTestSuite) TestSessionStore_GetBySupplier() {
	ctx := context.Background()

	// Create multiple sessions
	for i := 0; i < 5; i++ {
		sessionID := fmt.Sprintf("supplier_session_%d", i)
		snapshot := s.createTestSnapshot(sessionID, SessionStateActive)
		err := s.store.Save(ctx, snapshot)
		s.Require().NoError(err)
	}

	// Retrieve all sessions
	sessions, err := s.store.GetBySupplier(ctx)
	s.Require().NoError(err)
	s.Require().Len(sessions, 5, "should retrieve all 5 sessions")
}

// TestSessionStoreTestSuite runs the test suite.
func TestSessionStoreTestSuite(t *testing.T) {
	suite.Run(t, new(SessionStoreTestSuite))
}
