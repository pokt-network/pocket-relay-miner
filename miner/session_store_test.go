//go:build test

package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestSessionStore creates a session store on its own namespace of the
// real Redis.
//
// The key prefix comes from the client's KeyBuilder, not the "ha:miner:sessions"
// literal it used to carry: with a per-test namespace a hardcoded prefix writes
// outside the subtree the cleanup sweeps and the other tests read, so every
// assertion here would miss and the keys would be left on a shared server.
func setupTestSessionStore(t *testing.T) (*RedisSessionStore, *redisutil.Client) {
	t.Helper()

	client, _ := newTestRedis(t)

	store := NewRedisSessionStore(
		testLogger(),
		client,
		SessionStoreConfig{
			SupplierAddress: "pokt1test",
			SessionTTL:      1 * time.Hour,
		},
	)

	return store, client
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
		SessionStateClaimSkipped,
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

// --- Wave 3: Redis Hash layout tests ---

// TestSave_HashFieldRoundTrip verifies every SessionSnapshot field
// round-trips through the hash encode/decode path.
func TestSave_HashFieldRoundTrip(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	original := &SessionSnapshot{
		SessionID:               "sess-roundtrip",
		SupplierOperatorAddress: "pokt1supplier",
		ServiceID:               "svc-eth",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      500,
		SessionEndHeight:        510,
		State:                   SessionStateClaimed,
		RelayCount:              42,
		TotalComputeUnits:       4200,
		// A real SMST root is exactly 48 bytes (32-byte hash || 8-byte count
		// || 8-byte sum). decodeSnapshot drops anything else as corrupt, so
		// the round-trip assertion below only holds for a shape-valid root.
		ClaimedRootHash: bytes.Repeat([]byte{0x5a}, SMSTRootLen),
		ClaimTxHash:     "claimtx",
		ProofTxHash:     "prooftx",
	}
	require.NoError(t, store.Save(ctx, original))

	// Confirm Redis actually stored the key as a hash, not a string.
	key := client.KB().MinerSessionKey("pokt1test", original.SessionID)
	keyType := redisType(t, client, key)
	assert.Equal(t, "hash", keyType, "Wave 3 must store sessions as Redis Hash")

	got, err := store.Get(ctx, original.SessionID)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, original.SessionID, got.SessionID)
	assert.Equal(t, original.SupplierOperatorAddress, got.SupplierOperatorAddress)
	assert.Equal(t, original.ServiceID, got.ServiceID)
	assert.Equal(t, original.ApplicationAddress, got.ApplicationAddress)
	assert.Equal(t, original.SessionStartHeight, got.SessionStartHeight)
	assert.Equal(t, original.SessionEndHeight, got.SessionEndHeight)
	assert.Equal(t, original.State, got.State)
	assert.Equal(t, original.RelayCount, got.RelayCount)
	assert.Equal(t, original.TotalComputeUnits, got.TotalComputeUnits)
	assert.Equal(t, original.ClaimedRootHash, got.ClaimedRootHash)
	assert.Equal(t, original.ClaimTxHash, got.ClaimTxHash)
	assert.Equal(t, original.ProofTxHash, got.ProofTxHash)
	assert.False(t, got.CreatedAt.IsZero())
	assert.False(t, got.LastUpdatedAt.IsZero())
}

// TestSave_ClearsStaleOptionalFields ensures that when a subsequent Save
// omits an optional field, the old value is not leaked across writes.
func TestSave_ClearsStaleOptionalFields(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-optional",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      1,
		SessionEndHeight:        2,
		State:                   SessionStateActive,
		ClaimTxHash:             "initial-tx",
		ClaimedRootHash:         []byte("roothash"),
	}))

	// Re-save with optional fields cleared.
	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-optional",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      1,
		SessionEndHeight:        2,
		State:                   SessionStateActive,
	}))

	got, err := store.Get(ctx, "sess-optional")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got.ClaimTxHash, "stale optional string field must not leak")
	assert.Empty(t, got.ClaimedRootHash, "stale optional bytes field must not leak")
}

// TestGet_LegacyJSONFallback verifies Get transparently decodes a legacy
// JSON string key written by a pre-Wave-3 miner during rolling upgrade.
func TestGet_LegacyJSONFallback(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	// Write a legacy JSON string directly to Redis, mimicking the old layout.
	legacy := SessionSnapshot{
		SessionID:               "sess-legacy",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      1,
		SessionEndHeight:        2,
		State:                   SessionStateActive,
		RelayCount:              7,
		TotalComputeUnits:       70,
		CreatedAt:               time.Now(),
		LastUpdatedAt:           time.Now(),
	}
	buf, err := json.Marshal(&legacy)
	require.NoError(t, err)
	key := client.KB().MinerSessionKey("pokt1test", "sess-legacy")
	require.NoError(t, client.Set(ctx, key, string(buf), 0).Err())

	got, err := store.Get(ctx, "sess-legacy")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(7), got.RelayCount)
	assert.Equal(t, uint64(70), got.TotalComputeUnits)
	assert.Equal(t, SessionStateActive, got.State)
}

// TestIncrementRelayCount_MigratesLegacyJSON verifies the WRONGTYPE
// rescue path: an old JSON string key is migrated to the hash layout and
// the increment succeeds on retry.
func TestIncrementRelayCount_MigratesLegacyJSON(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	legacy := SessionSnapshot{
		SessionID:               "sess-migrate",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      1,
		SessionEndHeight:        2,
		State:                   SessionStateActive,
		RelayCount:              3,
		TotalComputeUnits:       30,
		CreatedAt:               time.Now(),
		LastUpdatedAt:           time.Now(),
	}
	buf, err := json.Marshal(&legacy)
	require.NoError(t, err)
	key := client.KB().MinerSessionKey("pokt1test", "sess-migrate")
	require.NoError(t, client.Set(ctx, key, string(buf), 0).Err())

	// First increment triggers WRONGTYPE → migrate → retry.
	require.NoError(t, store.IncrementRelayCount(ctx, "sess-migrate", 10))

	// Key should now be a hash.
	assert.Equal(t, "hash", redisType(t, client, key))

	got, err := store.Get(ctx, "sess-migrate")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(4), got.RelayCount)
	assert.Equal(t, uint64(40), got.TotalComputeUnits)

	// Subsequent increments take the fast path.
	require.NoError(t, store.IncrementRelayCount(ctx, "sess-migrate", 10))
	got, err = store.Get(ctx, "sess-migrate")
	require.NoError(t, err)
	assert.Equal(t, int64(5), got.RelayCount)
}

// writeLegacyJSONKey writes a pre-Wave-3 JSON string straight into Redis, as an
// old binary would have.
//
// The SHAPE is the legacy one — a JSON string where the store now keeps a hash;
// that is the point of the test. The ADDRESS is the current KeyBuilder's, so
// the store under test actually finds it. Spelling the old "ha:..." literal
// here would only prove that a key nobody reads can be ignored.
func writeLegacyJSONKey(t *testing.T, client *redisutil.Client, snap *SessionSnapshot) string {
	t.Helper()
	ctx := context.Background()
	buf, err := json.Marshal(snap)
	require.NoError(t, err)
	key := client.KB().MinerSessionKey("pokt1test", snap.SessionID)
	require.NoError(t, client.Set(ctx, key, string(buf), 0).Err())
	// Mimic the old supplier index entry so list paths see the session.
	require.NoError(t, client.SAdd(ctx, client.KB().MinerSessionsIndexKey("pokt1test"), snap.SessionID).Err())
	require.NoError(t, client.SAdd(ctx,
		client.KB().MinerSessionStateIndexKey("pokt1test", string(snap.State)), snap.SessionID).Err())
	return key
}

// TestSave_MigratesLegacyJSONKey verifies Save() replaces a legacy string
// key with a hash layout in one transaction, preserving the old state so
// the state index is updated correctly.
func TestSave_MigratesLegacyJSONKey(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	legacy := &SessionSnapshot{
		SessionID:               "sess-save-migrate",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      1,
		SessionEndHeight:        2,
		State:                   SessionStateActive,
		RelayCount:              11,
		TotalComputeUnits:       110,
		CreatedAt:               time.Now(),
		LastUpdatedAt:           time.Now(),
	}
	key := writeLegacyJSONKey(t, client, legacy)
	require.Equal(t, "string", redisType(t, client, key))

	// Save with a new state → must DEL the legacy string, write a hash,
	// and move the session from the active index to the claiming index.
	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-save-migrate",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      1,
		SessionEndHeight:        2,
		State:                   SessionStateClaiming,
		RelayCount:              11,
		TotalComputeUnits:       110,
	}))

	assert.Equal(t, "hash", redisType(t, client, key), "legacy key must be rewritten as hash")

	got, err := store.Get(ctx, "sess-save-migrate")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, SessionStateClaiming, got.State)
	assert.Equal(t, int64(11), got.RelayCount)

	active, err := store.GetByState(ctx, SessionStateActive)
	require.NoError(t, err)
	assert.Len(t, active, 0, "old state index entry must be cleaned up")

	claiming, err := store.GetByState(ctx, SessionStateClaiming)
	require.NoError(t, err)
	assert.Len(t, claiming, 1)
}

// TestUpdateState_MigratesLegacyJSONKey verifies UpdateState transparently
// handles a legacy JSON key during rolling upgrade.
func TestUpdateState_MigratesLegacyJSONKey(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	legacy := &SessionSnapshot{
		SessionID:               "sess-update-migrate",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      1,
		SessionEndHeight:        2,
		State:                   SessionStateActive,
		RelayCount:              4,
		TotalComputeUnits:       40,
		CreatedAt:               time.Now(),
		LastUpdatedAt:           time.Now(),
	}
	key := writeLegacyJSONKey(t, client, legacy)

	require.NoError(t, store.UpdateState(ctx, "sess-update-migrate", SessionStateClaimed))

	assert.Equal(t, "hash", redisType(t, client, key))
	got, err := store.Get(ctx, "sess-update-migrate")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, SessionStateClaimed, got.State)
	assert.Equal(t, int64(4), got.RelayCount, "counters must survive migration")
	assert.Equal(t, uint64(40), got.TotalComputeUnits)
}

// TestGetBySupplier_MixedFormats verifies the list paths return both legacy
// JSON sessions and new hash sessions during a rolling upgrade window.
func TestGetBySupplier_MixedFormats(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	// New-format session via Save.
	saveTestSession(t, store, "sess-new", SessionStateActive, 1, 10)

	// Legacy-format session written directly.
	writeLegacyJSONKey(t, client, &SessionSnapshot{
		SessionID:               "sess-old",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      1,
		SessionEndHeight:        2,
		State:                   SessionStateActive,
		RelayCount:              2,
		TotalComputeUnits:       20,
		CreatedAt:               time.Now(),
		LastUpdatedAt:           time.Now(),
	})

	all, err := store.GetBySupplier(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2, "both legacy and new sessions must be listed")

	byState, err := store.GetByState(ctx, SessionStateActive)
	require.NoError(t, err)
	assert.Len(t, byState, 2, "state index must surface both layouts")
}

// --- Race Condition Tests ---
// These tests verify that counter fields (relay_count, total_compute_units)
// are never clobbered by concurrent Save/UpdateState operations. This was
// the root cause of reward drops at scale: Save() used DEL+HSET which
// would wipe counters that IncrementRelayCount's HINCRBY had updated
// between the Get() and the pipeline Exec().

// TestIncrementRelayCount_RaceWithUpdateState runs IncrementRelayCount and
// UpdateState concurrently on the same session. Before the fix, UpdateState
// called Get→modify→Save which did DEL+HSET, wiping relay counts accumulated
// by concurrent HINCRBY operations.
func TestIncrementRelayCount_RaceWithUpdateState(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	saveTestSession(t, store, "sess-race-state", SessionStateActive, 0, 0)

	const incrementors = 100
	const computeUnitsPerRelay = uint64(10)

	var wg sync.WaitGroup

	// Start incrementors
	wg.Add(incrementors)
	for i := 0; i < incrementors; i++ {
		go func() {
			defer wg.Done()
			err := store.IncrementRelayCount(ctx, "sess-race-state", computeUnitsPerRelay)
			assert.NoError(t, err)
		}()
	}

	// Concurrently do state transitions (these used to clobber counters)
	wg.Add(3)
	go func() {
		defer wg.Done()
		_ = store.UpdateState(ctx, "sess-race-state", SessionStateClaiming)
	}()
	go func() {
		defer wg.Done()
		_ = store.UpdateState(ctx, "sess-race-state", SessionStateClaimed)
	}()
	go func() {
		defer wg.Done()
		_ = store.UpdateState(ctx, "sess-race-state", SessionStateProving)
	}()

	wg.Wait()

	snapshot, err := store.Get(ctx, "sess-race-state")
	require.NoError(t, err)
	assert.Equal(t, int64(incrementors), snapshot.RelayCount,
		"all %d increments must survive concurrent state transitions", incrementors)
	assert.Equal(t, uint64(incrementors)*computeUnitsPerRelay, snapshot.TotalComputeUnits,
		"all compute units must survive concurrent state transitions")
}

// TestIncrementRelayCount_RaceWithSave runs IncrementRelayCount and Save
// concurrently. Save on an existing hash key must NOT overwrite counter fields.
func TestIncrementRelayCount_RaceWithSave(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	saveTestSession(t, store, "sess-race-save", SessionStateActive, 0, 0)

	const incrementors = 100
	const computeUnitsPerRelay = uint64(5)

	var wg sync.WaitGroup

	// Start incrementors
	wg.Add(incrementors)
	for i := 0; i < incrementors; i++ {
		go func() {
			defer wg.Done()
			err := store.IncrementRelayCount(ctx, "sess-race-save", computeUnitsPerRelay)
			assert.NoError(t, err)
		}()
	}

	// Concurrently save metadata changes (e.g., claim tx hash updates)
	wg.Add(3)
	for i := 0; i < 3; i++ {
		txHash := fmt.Sprintf("tx-hash-%d", i)
		go func() {
			defer wg.Done()
			snap, _ := store.Get(ctx, "sess-race-save")
			if snap != nil {
				snap.ClaimTxHash = txHash
				_ = store.Save(ctx, snap)
			}
		}()
	}

	wg.Wait()

	snapshot, err := store.Get(ctx, "sess-race-save")
	require.NoError(t, err)
	assert.Equal(t, int64(incrementors), snapshot.RelayCount,
		"all %d increments must survive concurrent Save calls", incrementors)
	assert.Equal(t, uint64(incrementors)*computeUnitsPerRelay, snapshot.TotalComputeUnits,
		"all compute units must survive concurrent Save calls")
}

// TestUpdateState_DoesNotClobberCounters verifies UpdateState preserves
// counter values that were set by IncrementRelayCount.
func TestUpdateState_DoesNotClobberCounters(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	saveTestSession(t, store, "sess-state-counters", SessionStateActive, 0, 0)

	// Simulate relay processing: increment counters
	for i := 0; i < 100; i++ {
		require.NoError(t, store.IncrementRelayCount(ctx, "sess-state-counters", 10))
	}

	// Verify counters before state transition
	snap, err := store.Get(ctx, "sess-state-counters")
	require.NoError(t, err)
	assert.Equal(t, int64(100), snap.RelayCount)
	assert.Equal(t, uint64(1000), snap.TotalComputeUnits)

	// Transition through states (this is the sequence that triggers claim/proof)
	require.NoError(t, store.UpdateState(ctx, "sess-state-counters", SessionStateClaiming))
	require.NoError(t, store.UpdateState(ctx, "sess-state-counters", SessionStateClaimed))
	require.NoError(t, store.UpdateState(ctx, "sess-state-counters", SessionStateProving))
	require.NoError(t, store.UpdateState(ctx, "sess-state-counters", SessionStateProved))

	// Counters must be exactly the same after all state transitions
	snap, err = store.Get(ctx, "sess-state-counters")
	require.NoError(t, err)
	assert.Equal(t, int64(100), snap.RelayCount,
		"relay count must survive all state transitions")
	assert.Equal(t, uint64(1000), snap.TotalComputeUnits,
		"compute units must survive all state transitions")
	assert.Equal(t, SessionStateProved, snap.State)
}

// TestSave_ExistingKey_PreservesCounters verifies that Save on an existing
// hash key does NOT overwrite relay_count and total_compute_units.
func TestSave_ExistingKey_PreservesCounters(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()

	// Create session and add relays
	saveTestSession(t, store, "sess-preserve", SessionStateActive, 0, 0)
	for i := 0; i < 50; i++ {
		require.NoError(t, store.IncrementRelayCount(ctx, "sess-preserve", 10))
	}

	// Re-save with metadata change (e.g., adding claim tx hash)
	// The snapshot from Get has relay_count=50, but the important thing is
	// that Save does NOT write these counter fields at all.
	snap, err := store.Get(ctx, "sess-preserve")
	require.NoError(t, err)
	snap.ClaimTxHash = "0xabc123"
	require.NoError(t, store.Save(ctx, snap))

	// Even more increments after the Save
	for i := 0; i < 25; i++ {
		require.NoError(t, store.IncrementRelayCount(ctx, "sess-preserve", 10))
	}

	// Final check: 50 + 25 = 75 relays
	snap, err = store.Get(ctx, "sess-preserve")
	require.NoError(t, err)
	assert.Equal(t, int64(75), snap.RelayCount,
		"all increments before and after Save must be preserved")
	assert.Equal(t, uint64(750), snap.TotalComputeUnits)
	assert.Equal(t, "0xabc123", snap.ClaimTxHash,
		"metadata from Save must be persisted")
}

// testLogger returns a no-op logger for tests.
func testLogger() logging.Logger {
	return logging.NewLoggerFromConfig(logging.DefaultConfig())
}

// redisType returns a key's Redis type ("hash", "string", ...), replacing
// miniredis's DB(0).Type. The distinction is the subject of the migration
// tests: Wave 3 stores a session as a hash where the old binary wrote a JSON
// string, and only the type tells the two apart at the same address.
func redisType(t *testing.T, client *redisutil.Client, key string) string {
	t.Helper()
	typ, err := client.Type(context.Background(), key).Result()
	require.NoError(t, err)
	return typ
}
