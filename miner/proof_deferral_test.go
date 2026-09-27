//go:build test

package miner

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// Item 317. A claim is on chain and its proof has not been sent yet. If
// reading the claimed root fails, the miner used to mark the session
// terminal on the FIRST attempt, so nothing looked at it again: the chain
// then forfeits the whole claim and burns a flat slash on top. These tests
// pin the split that makes the miner try again instead.
//
// They run against the REAL Redis (internal/testredis) and break it for
// real by closing the client, because that is what produces the error the
// classifier has to read. A stub ClaimedRootProvider returning an error of
// the author's choosing would exercise neither the %w chain nor
// IsRetryableError, which are the fix.

// seedDeferralTree writes a session's claimed root through the real flush
// path and hands back a manager holding NO tree in memory, so GetTreeRoot
// must go through loadTreeFromRedis — the failover path where the money is
// lost.
func seedDeferralTree(t *testing.T, ctx context.Context, client *redisutil.Client, supplier, sessionID string) *RedisSMSTManager {
	t.Helper()
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        time.Hour,
	})
	require.NoError(t, mgr.UpdateTree(ctx, sessionID, bytes.Clone([]byte("relay-key")), bytes.Clone([]byte("relay-value")), 10))
	root, err := mgr.FlushTree(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, root, SMSTRootLen)

	// A process that just took over holds nothing in memory.
	mgr.treesMu.Lock()
	mgr.trees = make(map[string]*redisSMST)
	mgr.treesMu.Unlock()
	return mgr
}

// TestResolveClaimedRoot_ARedisThatStoppedAnsweringIsDeferredNotTerminal is
// the defect itself. LINK: defer-unreadable
func TestResolveClaimedRoot_ARedisThatStoppedAnsweringIsDeferredNotTerminal(t *testing.T) {
	ctx := context.Background()
	client, prefix := newTestRedis(t)
	const supplier, sessionID = "pokt1defer_unread", "sess-defer-unread"

	// A second client on the same namespace: closing it breaks the read
	// without taking the rest of the test's Redis down with it.
	breakable := sameNamespaceClient(t, prefix)
	mgr := seedDeferralTree(t, ctx, breakable, supplier, sessionID)

	checker := &ProofRequirementChecker{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		rootProvider: mgr,
	}
	snap := &SessionSnapshot{SessionID: sessionID, SupplierOperatorAddress: supplier}

	// The store the claim was written to is still there; only the
	// connection to it died. The tree is intact and the proof is buildable
	// a block later.
	require.NoError(t, breakable.Close())
	require.True(t, keyExists(t, client, client.KB().SMSTRootKey(supplier, sessionID)),
		"precondition: the claimed root is still in Redis — only the reader broke")

	got, err := checker.resolveClaimedRoot(ctx, snap)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClaimedRootUnreadable),
		"a root we could not READ must be deferrable, not terminal: the claim is already on chain")
	assert.False(t, errors.Is(err, ErrClaimedRootUnavailable),
		"it must NOT reach the terminal branch in lifecycle_callback, which drops the session for good")
	assert.True(t, errors.Is(err, redis.ErrClosed),
		"the underlying Redis error must survive in the chain — %w, not %v")
}

// TestResolveClaimedRoot_AnAbsentRootStaysTerminal is the other half: with
// Redis answering normally and no root for the session, there is nothing to
// anchor a proof on and never will be. LINK: defer-absent-is-terminal
func TestResolveClaimedRoot_AnAbsentRootStaysTerminal(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1defer_absent", "sess-defer-absent"

	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        time.Hour,
	})
	checker := &ProofRequirementChecker{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		rootProvider: mgr,
	}
	snap := &SessionSnapshot{SessionID: sessionID, SupplierOperatorAddress: supplier}

	require.False(t, keyExists(t, client, client.KB().SMSTRootKey(supplier, sessionID)),
		"precondition: no claimed root was ever written for this session")

	got, err := checker.resolveClaimedRoot(ctx, snap)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClaimedRootUnavailable),
		"an absent root is terminal: retrying it every block would never succeed")
	assert.False(t, errors.Is(err, ErrClaimedRootUnreadable),
		"it must not be deferred — there is nothing to come back for")
}

// TestResolveClaimedRoot_AShutdownCancelIsDeferred covers the graceful
// shutdown, which IsRetryableError deliberately excludes. Without the
// second half of the predicate every in-flight session is marked terminal
// on the way out, with its claim already on chain.
// LINK: defer-shutdown
func TestResolveClaimedRoot_AShutdownCancelIsDeferred(t *testing.T) {
	_, prefix := newTestRedis(t)
	const supplier, sessionID = "pokt1defer_cancel", "sess-defer-cancel"

	seeded := sameNamespaceClient(t, prefix)
	mgr := seedDeferralTree(t, context.Background(), seeded, supplier, sessionID)

	checker := &ProofRequirementChecker{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		rootProvider: mgr,
	}
	snap := &SessionSnapshot{SessionID: sessionID, SupplierOperatorAddress: supplier}

	// The worker context is cancelled: this is the shutdown the l3k run
	// exercised at h=62, not a failure of the store.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := checker.resolveClaimedRoot(ctx, snap)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.False(t, IsRetryableError(err),
		"precondition: IsRetryableError alone does NOT cover a shutdown cancel — that is why the predicate has a second half")
	assert.True(t, errors.Is(err, ErrClaimedRootUnreadable),
		"a session interrupted by shutdown must be deferred, not written off with its claim on chain")
	assert.False(t, errors.Is(err, ErrClaimedRootUnavailable))
}

// TestOnProofDeferred_ReturnsTheSessionToClaimedWithoutGoingTerminal pins
// the coordinator half: the state goes back to claimed in Redis and the
// terminal callback — wired to RemoveSession — is NOT invoked.
// LINK: defer-returns-to-claimed
func TestOnProofDeferred_ReturnsTheSessionToClaimedWithoutGoingTerminal(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestSessionStore(t)

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()

	var terminalStates []SessionState
	coord.SetOnSessionTerminalCallback(func(_ string, state SessionState) {
		terminalStates = append(terminalStates, state)
	})

	const sessionID = "sess-defer-coord"
	saveTestSession(t, store, sessionID, SessionStateProving, 5, 50)

	require.NoError(t, coord.OnProofDeferred(ctx, sessionID))

	got, err := store.Get(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, SessionStateClaimed, got.State,
		"the session must go back to claimed: from proving the only exit is proof_window_closed, which is the same money lost")
	assert.Empty(t, terminalStates,
		"the terminal callback removes the session from activeSessions — invoking it is exactly what makes the proof never happen")
}

// TestOnProofDeferred_DoesNotRewindASessionAnotherMinerAlreadyProved keeps
// the HA guard its terminal twin has: rewinding a proved session to claimed
// would make this miner submit a second proof, and a duplicate proof
// settles the whole batch as an error here.
// LINK: defer-not-over-a-proved-session
func TestOnProofDeferred_DoesNotRewindASessionAnotherMinerAlreadyProved(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestSessionStore(t)

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()

	const sessionID = "sess-defer-proved"
	saveTestSession(t, store, sessionID, SessionStateProved, 5, 50)

	require.NoError(t, coord.OnProofDeferred(ctx, sessionID))

	got, err := store.Get(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, SessionStateProved, got.State,
		"a session another miner proved must stay proved: rewinding it buys a duplicate proof")
}

// TestDeferProof_RewindsTheInMemorySnapshotTheEngineReads is the half that
// decides whether the retry happens at all. checkSessionTransitions reads
// the in-memory snapshot, and from proving the only way out is the window
// closing — so writing Redis alone would defer nothing.
// LINK: defer-rewinds-the-snapshot
func TestDeferProof_RewindsTheInMemorySnapshotTheEngineReads(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestSessionStore(t)

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()

	const sessionID = "sess-defer-snapshot"
	saveTestSession(t, store, sessionID, SessionStateProving, 5, 50)

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:             DefaultLifecycleCallbackConfig(),
		sessionCoordinator: coord,
	}
	snapshot := &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: "pokt1test",
		State:                   SessionStateProving,
	}

	lc.deferProof(ctx, snapshot)

	assert.Equal(t, SessionStateClaimed, snapshot.State,
		"the snapshot activeSessions holds must be rewound too, or checkSessionTransitions never asks for the proof again")
}

// TestDeferProof_LeavesTheSnapshotAloneWhenRedisRefusesTheWrite states the
// floor: when the write does not land, the session is NOT rewound in
// memory either, so it ages out through proof_window_closed — which at
// least COUNTS the loss. Same rule as resumeUnsentSubmission.
// LINK: defer-write-failure-keeps-the-count
func TestDeferProof_LeavesTheSnapshotAloneWhenRedisRefusesTheWrite(t *testing.T) {
	ctx := context.Background()
	_, prefix := newTestRedis(t)

	breakable := sameNamespaceClient(t, prefix)
	store := NewRedisSessionStore(testLogger(), breakable, SessionStoreConfig{
		SupplierAddress: "pokt1test",
		SessionTTL:      time.Hour,
	})
	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()

	const sessionID = "sess-defer-writefail"
	saveTestSession(t, store, sessionID, SessionStateProving, 5, 50)

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:             DefaultLifecycleCallbackConfig(),
		sessionCoordinator: coord,
	}
	snapshot := &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: "pokt1test",
		State:                   SessionStateProving,
	}

	require.NoError(t, breakable.Close())

	lc.deferProof(ctx, snapshot)

	assert.Equal(t, SessionStateProving, snapshot.State,
		"with the write refused the session must stay as it was: a snapshot that says claimed while Redis says proving is a lie a failover would read")
}
