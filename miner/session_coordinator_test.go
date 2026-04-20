//go:build test

package miner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnRelayProcessed_ConcurrentFirstRelay_ExactlyOneCreateCallback is the
// reproducer for the TOCTOU bug in SessionCoordinator.OnRelayProcessed. N
// concurrent relays for the same brand-new session used to observe
// snapshot == nil and all fired OnSessionCreated → TrackSession, causing
// duplicate lifecycle entries and lost relay counters (two racing Save
// calls would both take the isLegacyKey==false && existingSnapshot==nil
// branch and HSET the full snapshot with relay_count=0).
//
// After the fix (first-write-wins via SessionStore.CreateIfAbsent) exactly
// one callback fires no matter how many goroutines race.
func TestOnRelayProcessed_ConcurrentFirstRelay_ExactlyOneCreateCallback(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	defer store.Close()

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{
		SupplierAddress: "pokt1test",
	})
	defer coord.Close()

	var callbackCount atomic.Int32
	var callbackSessionIDs sync.Map // sessionID -> count (only one expected)
	coord.SetOnSessionCreatedCallback(func(_ context.Context, snap *SessionSnapshot) error {
		callbackCount.Add(1)
		// Track per-session invocations — duplicate invocations for the
		// same session ID is the exact misbehaviour we're pinning.
		if existing, loaded := callbackSessionIDs.LoadOrStore(snap.SessionID, 1); loaded {
			if n, ok := existing.(int); ok {
				callbackSessionIDs.Store(snap.SessionID, n+1)
			}
		}
		return nil
	})

	const (
		sessionID       = "session-race"
		supplierAddress = "pokt1test"
		serviceID       = "svc-race"
		appAddress      = "pokt1app"
		startHeight     = int64(100)
		endHeight       = int64(110)
		numGoroutines   = 32
	)

	ctx := context.Background()
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(cu uint64) {
			defer wg.Done()
			<-start // release all goroutines together to maximise race
			err := coord.OnRelayProcessed(
				ctx,
				sessionID,
				cu,
				supplierAddress,
				serviceID,
				appAddress,
				startHeight,
				endHeight,
			)
			// OnRelayProcessed may return nil or a terminal error depending
			// on how the relay count update interleaves with create; both
			// are acceptable for this test. The pinned contract is the
			// callback count, not the return code.
			_ = err
		}(uint64(10 * (i + 1)))
	}
	close(start)
	wg.Wait()

	// Callback must have fired EXACTLY once.
	assert.Equal(t, int32(1), callbackCount.Load(),
		"session-created callback must fire exactly once for N concurrent first-relay calls")

	// And only for the one sessionID we used.
	var distinctSessions int
	callbackSessionIDs.Range(func(_, v any) bool {
		distinctSessions++
		if n, ok := v.(int); ok {
			assert.Equal(t, 1, n,
				"callback must fire exactly once per session ID")
		}
		return true
	})
	assert.Equal(t, 1, distinctSessions,
		"exactly one session ID must have triggered the create callback")

	// Session must exist in Redis with the expected metadata.
	snap, err := store.Get(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, snap, "session snapshot must be persisted by the winning CreateIfAbsent call")
	assert.Equal(t, sessionID, snap.SessionID)
	assert.Equal(t, SessionStateActive, snap.State)
	assert.Equal(t, supplierAddress, snap.SupplierOperatorAddress)
	assert.Equal(t, serviceID, snap.ServiceID)

	// Relay counters: every increment that observed a non-terminal session
	// must have stuck. Losing counts to a racing HSET(full) overwrite was
	// the second half of the original bug.
	assert.Equal(t, int64(numGoroutines), snap.RelayCount,
		"all concurrent relay increments must be counted exactly once")
}

// TestOnSessionCreated_IdempotentOnRepeatCall verifies that calling
// OnSessionCreated twice in sequence fires the callback exactly once. This
// is the sequential-caller analogue of the concurrent test above — the
// Lua-script first-write-wins gate must reject the second create.
func TestOnSessionCreated_IdempotentOnRepeatCall(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	defer store.Close()

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{
		SupplierAddress: "pokt1test",
	})
	defer coord.Close()

	var callbackCount atomic.Int32
	coord.SetOnSessionCreatedCallback(func(_ context.Context, _ *SessionSnapshot) error {
		callbackCount.Add(1)
		return nil
	})

	ctx := context.Background()
	const sessionID = "session-repeat"

	err := coord.OnSessionCreated(ctx, sessionID, "pokt1test", "svc", "pokt1app", 100, 110)
	require.NoError(t, err)

	err = coord.OnSessionCreated(ctx, sessionID, "pokt1test", "svc", "pokt1app", 100, 110)
	require.NoError(t, err, "second OnSessionCreated must not error — it simply reports no-op")

	assert.Equal(t, int32(1), callbackCount.Load(),
		"session-created callback must fire exactly once even for repeated calls")
}

// TestCreateIfAbsent_RejectsSecondCreate directly exercises the
// RedisSessionStore gate at the store layer, independent of the
// coordinator. This isolates the Lua script's EXISTS guard.
func TestCreateIfAbsent_RejectsSecondCreate(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	defer store.Close()

	ctx := context.Background()
	snap := &SessionSnapshot{
		SessionID:               "sess-gate",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   SessionStateActive,
	}

	created, err := store.CreateIfAbsent(ctx, snap)
	require.NoError(t, err)
	assert.True(t, created, "first CreateIfAbsent must report created=true")

	created2, err := store.CreateIfAbsent(ctx, snap)
	require.NoError(t, err)
	assert.False(t, created2, "second CreateIfAbsent must report created=false")
}
