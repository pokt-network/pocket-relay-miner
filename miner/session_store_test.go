//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestSessionStore creates a miniredis-backed session store for testing.
func setupTestSessionStore(t *testing.T) (*RedisSessionStore, *miniredis.Miniredis) {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)

	store := NewRedisSessionStore(
		testLogger(),
		client,
		SessionStoreConfig{
			KeyPrefix:       "ha:miner:sessions",
			SupplierAddress: "pokt1test",
			SessionTTL:      1 * time.Hour,
		},
	)

	return store, mr
}

// saveTestSession is a helper that saves a session with the given parameters.
func saveTestSession(t *testing.T, store *RedisSessionStore, sessionID string, state SessionState, relayCount int64, computeUnits uint64) {
	t.Helper()
	ctx := context.Background()
	err := store.Save(ctx, &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-test",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   state,
		RelayCount:              relayCount,
		TotalComputeUnits:       computeUnits,
	})
	require.NoError(t, err)
}

// --- IncrementRelayCount Tests ---

func TestIncrementRelayCount_Basic(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	saveTestSession(t, store, "sess-1", SessionStateActive, 0, 0)

	err := store.IncrementRelayCount(ctx, "sess-1", 100)
	require.NoError(t, err)

	snapshot, err := store.Get(ctx, "sess-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), snapshot.RelayCount)
	assert.Equal(t, uint64(100), snapshot.TotalComputeUnits)
}

func TestIncrementRelayCount_Multiple(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	saveTestSession(t, store, "sess-1", SessionStateActive, 0, 0)

	for i := 0; i < 10; i++ {
		err := store.IncrementRelayCount(ctx, "sess-1", 50)
		require.NoError(t, err)
	}

	snapshot, err := store.Get(ctx, "sess-1")
	require.NoError(t, err)
	assert.Equal(t, int64(10), snapshot.RelayCount)
	assert.Equal(t, uint64(500), snapshot.TotalComputeUnits)
}

func TestIncrementRelayCount_RejectsTerminalState(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	terminalStates := []SessionState{
		SessionStateProved,
		SessionStateProbabilisticProved,
		SessionStateClaimWindowClosed,
		SessionStateClaimTxError,
		SessionStateProofWindowClosed,
		SessionStateProofTxError,
	}

	for _, state := range terminalStates {
		sessionID := fmt.Sprintf("sess-terminal-%s", state)
		saveTestSession(t, store, sessionID, state, 5, 500)

		err := store.IncrementRelayCount(ctx, sessionID, 100)
		assert.ErrorIs(t, err, ErrSessionTerminal, "state %s should be rejected", state)

		// Verify count was NOT modified
		snapshot, err := store.Get(ctx, sessionID)
		require.NoError(t, err)
		assert.Equal(t, int64(5), snapshot.RelayCount, "relay count should be unchanged for state %s", state)
		assert.Equal(t, uint64(500), snapshot.TotalComputeUnits, "compute units should be unchanged for state %s", state)
	}
}

func TestIncrementRelayCount_AllowsNonTerminalStates(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	nonTerminalStates := []SessionState{
		SessionStateActive,
		SessionStateClaiming,
		SessionStateClaimed,
		SessionStateProving,
	}

	for _, state := range nonTerminalStates {
		sessionID := fmt.Sprintf("sess-nonterminal-%s", state)
		saveTestSession(t, store, sessionID, state, 0, 0)

		err := store.IncrementRelayCount(ctx, sessionID, 100)
		assert.NoError(t, err, "state %s should be allowed", state)

		snapshot, err := store.Get(ctx, sessionID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), snapshot.RelayCount, "relay count should be 1 for state %s", state)
	}
}

func TestIncrementRelayCount_SessionNotFound(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	err := store.IncrementRelayCount(ctx, "nonexistent", 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
}

func TestIncrementRelayCount_ConcurrentSafety(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	saveTestSession(t, store, "sess-concurrent", SessionStateActive, 0, 0)

	// Run 50 concurrent increments, each adding 10 compute units.
	// With the old GET+SET, some increments would be lost due to races.
	// With the Lua script, all 50 must be counted.
	const goroutines = 50
	const computeUnitsPerRelay = uint64(10)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			err := store.IncrementRelayCount(ctx, "sess-concurrent", computeUnitsPerRelay)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	snapshot, err := store.Get(ctx, "sess-concurrent")
	require.NoError(t, err)
	assert.Equal(t, int64(goroutines), snapshot.RelayCount,
		"all %d increments must be counted (no lost updates)", goroutines)
	assert.Equal(t, uint64(goroutines)*computeUnitsPerRelay, snapshot.TotalComputeUnits,
		"all compute units must be counted")
}

func TestIncrementRelayCount_PreservesOtherFields(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	// Save a session with all fields populated
	err := store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-fields",
		SupplierOperatorAddress: "pokt1supplier",
		ServiceID:               "svc-eth",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   SessionStateActive,
		RelayCount:              5,
		TotalComputeUnits:       500,
		ClaimTxHash:             "",
		LastWALEntryID:          "wal-123",
	})
	require.NoError(t, err)

	// Increment
	err = store.IncrementRelayCount(ctx, "sess-fields", 100)
	require.NoError(t, err)

	// Verify relay count updated AND other fields preserved
	snapshot, err := store.Get(ctx, "sess-fields")
	require.NoError(t, err)
	assert.Equal(t, int64(6), snapshot.RelayCount)
	assert.Equal(t, uint64(600), snapshot.TotalComputeUnits)
	assert.Equal(t, "pokt1supplier", snapshot.SupplierOperatorAddress)
	assert.Equal(t, "svc-eth", snapshot.ServiceID)
	assert.Equal(t, "pokt1app", snapshot.ApplicationAddress)
	assert.Equal(t, int64(100), snapshot.SessionStartHeight)
	assert.Equal(t, int64(110), snapshot.SessionEndHeight)
	assert.Equal(t, SessionStateActive, snapshot.State)
	assert.Equal(t, "wal-123", snapshot.LastWALEntryID)
}

// --- Save State Index Tests (BUG-6) ---

func TestSave_StateIndexCleanup(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	// Save in active state
	saveTestSession(t, store, "sess-state", SessionStateActive, 1, 100)

	// Verify it's in the active index
	activeIDs, err := store.GetByState(ctx, SessionStateActive)
	require.NoError(t, err)
	assert.Len(t, activeIDs, 1)

	// Transition to claiming
	err = store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-state",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-test",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   SessionStateClaiming,
		RelayCount:              1,
		TotalComputeUnits:       100,
	})
	require.NoError(t, err)

	// Should be in claiming index, NOT in active index
	claimingIDs, err := store.GetByState(ctx, SessionStateClaiming)
	require.NoError(t, err)
	assert.Len(t, claimingIDs, 1)

	activeIDs, err = store.GetByState(ctx, SessionStateActive)
	require.NoError(t, err)
	assert.Len(t, activeIDs, 0, "session should be removed from old state index")
}

// testLogger returns a no-op logger for tests.
func testLogger() logging.Logger {
	return logging.NewLoggerFromConfig(logging.DefaultConfig())
}
