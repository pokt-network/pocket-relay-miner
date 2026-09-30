//go:build test

package miner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// Beta, 2026-09-29 21:30Z: the gas simulation of a claim batch reached a node
// one block behind the miner, which answered with poktroll's too-early text.
// The miner read "claim window" in it and marked the batch closed and
// permanent with ten blocks of the window still open. These are that text and
// its proof twin, verbatim in shape (poktroll x/proof keeper).
var (
	// errBetaClaimNodeBehind carries what the tx client hands back: a
	// simulation-stage TxRejection naming "message index: 0" (so the ejection
	// path could read it), wrapped with the node's text.
	errBetaClaimNodeBehind = fmt.Errorf("failed to broadcast claims: gas simulation failed (gas_limit=0 requires successful simulation): %w: %s",
		&tx.TxRejection{Stage: tx.TxStageSimulate, HasMsgIndex: true, MsgIndex: 0},
		"rpc error: code = Unknown desc = recv from backend: rpc error: code = Unknown desc = failed to execute message; "+
			"message index: 0: rpc error: code = FailedPrecondition desc = current block height (102) is less than the session's "+
			"earliest claim commit height (103): claim attempted outside of the session's claim window")
	errBetaProofNodeBehind = errors.New("failed to broadcast proofs: gas simulation failed: rpc error: code = FailedPrecondition desc = " +
		"current block height (107) is less than session's earliest proof commit height (108): proof attempted outside of the session's proof window")
)

// nodeBehindSupplier refuses the first submission with the given error and
// accepts every one after it.
type nodeBehindSupplier struct {
	first error
	calls int
}

func (s *nodeBehindSupplier) submit(kind string) (string, tx.SignedTxPayload, error) {
	s.calls++
	if s.calls == 1 {
		return "", tx.SignedTxPayload{}, s.first
	}
	hash, signed := fakeSigned(kind)
	return hash, signed, nil
}

func (s *nodeBehindSupplier) CreateClaimsReturningHash(context.Context, int64, ...pocktclient.MsgCreateClaim) (string, tx.SignedTxPayload, error) {
	return s.submit("claim")
}

func (s *nodeBehindSupplier) SubmitProofsReturningHash(context.Context, int64, ...pocktclient.MsgSubmitProof) (string, tx.SignedTxPayload, error) {
	return s.submit("proof")
}

func (*nodeBehindSupplier) GetEstimatedFeeUpokt(context.Context) uint64 { return 0 }

func (*nodeBehindSupplier) BroadcastRawReturningHash(context.Context, string, tx.SignedTxPayload) (string, error) {
	return "", errors.New("nodeBehindSupplier does not re-inject")
}

func (*nodeBehindSupplier) LatestBlockTime() time.Time { return time.Time{} }

// nodeBehindCallback is a lifecycle callback at our height, whose supplier
// client answers with supplier and whose sessions live in a real store.
func nodeBehindCallback(t *testing.T, height int64, supplier *nodeBehindSupplier) (*LifecycleCallback, *RedisSessionStore) {
	t.Helper()
	store, _ := setupTestSessionStore(t)
	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	t.Cleanup(func() { _ = coord.Close() })
	blocks := &heightedBlocks{}
	blocks.currentHeight = height
	return &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:       &defaultParamsShared{},
		blockClient:        blocks,
		smstManager:        provingSMST{},
		supplierClient:     supplier,
		serviceClient:      erroringService{},
		sessionCoordinator: coord,
		config: LifecycleCallbackConfig{
			ClaimRetryAttempts: 3,
			ClaimRetryDelay:    time.Millisecond,
			ProofRetryAttempts: 3,
			ProofRetryDelay:    time.Millisecond,
		},
	}, store
}

// storedSession saves a session under the default params of these tests (a
// session ending at 100: claim window 102..106, proof window 106..110) and
// returns the snapshot the block engine would hand the callback.
func storedSession(t *testing.T, store *RedisSessionStore, id string, state SessionState) *SessionSnapshot {
	t.Helper()
	snapshot := &SessionSnapshot{
		SessionID: id, SessionEndHeight: 100, SessionStartHeight: 81,
		SupplierOperatorAddress: "pokt1test", ServiceID: "svc", ApplicationAddress: "pokt1app",
		RelayCount: 10, TotalComputeUnits: 100, State: state,
		ClaimedRootHash: make([]byte, SMSTRootLen),
	}
	require.NoError(t, store.Save(context.Background(), snapshot))
	return snapshot
}

func requireState(t *testing.T, store *RedisSessionStore, snapshot *SessionSnapshot, want SessionState) {
	t.Helper()
	assert.Equal(t, want, snapshot.State, "the snapshot the block engine reads")
	got, err := store.Get(context.Background(), snapshot.SessionID)
	require.NoError(t, err)
	assert.Equal(t, want, got.State, "and Redis with it")
}

// TestOnSessionsNeedClaim_ANodeBehindUsReturnsTheGroupToActive is beta: the
// refusal arrives with our height inside the claim window (103 of 102..106).
// It is neither a closed window, a failed attempt nor a failed cycle: the whole
// group goes back to active at once, with nothing ejected and no attempt spent,
// and the block engine sends it again on the next block.
func TestOnSessionsNeedClaim_ANodeBehindUsReturnsTheGroupToActive(t *testing.T) {
	supplier := &nodeBehindSupplier{first: errBetaClaimNodeBehind}
	lc, store := nodeBehindCallback(t, 103, supplier)
	a := storedSession(t, store, "claim-a", SessionStateClaiming)
	b := storedSession(t, store, "claim-b", SessionStateClaiming)

	result, err := lc.OnSessionsNeedClaim(context.Background(), []*SessionSnapshot{a, b})
	require.NoError(t, err, "a group returned to the next block is not a failed cycle")
	assert.False(t, result.IsClaimed("claim-a"))
	assert.Equal(t, 1, supplier.calls, "the refusal is not retried in place")
	requireState(t, store, a, SessionStateActive)
	requireState(t, store, b, SessionStateActive)
}

// TestOnSessionsNeedProof_ANodeBehindUsReturnsTheGroupToClaimed is the proof
// twin: our height 108 is inside the proof window 106..110.
func TestOnSessionsNeedProof_ANodeBehindUsReturnsTheGroupToClaimed(t *testing.T) {
	supplier := &nodeBehindSupplier{first: errBetaProofNodeBehind}
	lc, store := nodeBehindCallback(t, 108, supplier)
	snapshot := storedSession(t, store, "proof-node-behind", SessionStateProving)

	result, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{snapshot})
	require.NoError(t, err, "a group returned to the next block is not a failed cycle")
	_, settled := result.Settled["proof-node-behind"]
	assert.False(t, settled)
	assert.Equal(t, 1, supplier.calls)
	requireState(t, store, snapshot, SessionStateClaimed)
}

// TestOnSessionsNeedClaim_AnExpiredTransactionStaysFinal is the control: the
// timeout height passed on the node, which a node BEHIND us cannot say, so the
// refusal is final. The session is not returned to active; it stays in
// claiming until the window's close books it (claimReadIsFinal).
func TestOnSessionsNeedClaim_AnExpiredTransactionStaysFinal(t *testing.T) {
	supplier := &nodeBehindSupplier{first: fmt.Errorf("broadcast: %w", tx.ErrTxWindowExpired)}
	lc, store := nodeBehindCallback(t, 103, supplier)
	snapshot := storedSession(t, store, "claim-expired", SessionStateClaiming)

	result, _ := lc.OnSessionsNeedClaim(context.Background(), []*SessionSnapshot{snapshot})
	assert.False(t, result.IsClaimed("claim-expired"))
	assert.Equal(t, 1, supplier.calls)
	requireState(t, store, snapshot, SessionStateClaiming)
}

// TestWindowRefusalIsFinal_OurHeightDecides pins the rule on its own: the
// node's text never decides, our height and the transaction's timeout do.
func TestWindowRefusalIsFinal_OurHeightDecides(t *testing.T) {
	at := func(height int64) *LifecycleCallback {
		blocks := &silentBlocks{}
		blocks.currentHeight = height
		return &LifecycleCallback{blockClient: blocks}
	}
	ctx := context.Background()
	assert.False(t, at(105).windowRefusalIsFinal(ctx, errBetaClaimNodeBehind, 106), "window open at our height")
	assert.True(t, at(106).windowRefusalIsFinal(ctx, errBetaClaimNodeBehind, 106), "our height reached the close")
	assert.True(t, at(105).windowRefusalIsFinal(ctx, fmt.Errorf("broadcast: %w", tx.ErrTxWindowExpired), 106),
		"an expired timeout is final whatever our height")
}

// TestDeferProof_KeepsTheSnapshotOfASessionAnotherMinerProved: the refusal
// leaves the in-memory state alone too.
func TestDeferProof_KeepsTheSnapshotOfASessionAnotherMinerProved(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()
	const sessionID = "proof-proved-elsewhere"
	saveTestSession(t, store, sessionID, SessionStateProved, 10, 100)

	lc := &LifecycleCallback{logger: logging.NewLoggerFromConfig(logging.DefaultConfig()), sessionCoordinator: coord}
	snapshot := &SessionSnapshot{SessionID: sessionID, SupplierOperatorAddress: "pokt1test", State: SessionStateProving}
	lc.deferProof(context.Background(), snapshot)
	assert.Equal(t, SessionStateProving, snapshot.State, "a session proved elsewhere must not be rewound to claimed")
}

// silentBlocks is a block client whose subscription stays open and never
// delivers -- the silent-but-open feed of issue #45 -- while its height moves.
type silentBlocks struct{ mockBlockClient }

// TestWaitForHeight_RecoversFromAnOpenButSilentSubscription is issue #45: the
// event channel never delivers, the height reaches the target anyway, and the
// wait must end without its context being cancelled.
func TestWaitForHeight_RecoversFromAnOpenButSilentSubscription(t *testing.T) {
	blocks := &silentBlocks{}
	blocks.heightSequence = []int64{100, 100, 105}
	lc := &LifecycleCallback{
		logger:      logging.NewLoggerFromConfig(logging.DefaultConfig()),
		blockClient: blocks,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, lc.waitForHeight(ctx, 105), "the re-read must see the height the silent channel never announced")
}

// TestOnSessionsNeedClaim_AWaitThatFailsReturnsTheSessionToActive: a claim
// group whose wait for the window stops (here: the context ends) goes back to
// active in Redis and in memory, so the block engine claims it again next
// block instead of leaving it in claiming until the window closes.
func TestOnSessionsNeedClaim_AWaitThatFailsReturnsTheSessionToActive(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()
	const sessionID = "claim-wait-fails"
	saveTestSession(t, store, sessionID, SessionStateClaiming, 10, 100)

	blocks := &silentBlocks{}
	blocks.currentHeight = 100 // below the claim window, and it never moves
	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:       &defaultParamsShared{},
		blockClient:        blocks,
		sessionCoordinator: coord,
	}
	snapshot := &SessionSnapshot{
		SessionID: sessionID, SessionEndHeight: 100, SessionStartHeight: 81,
		SupplierOperatorAddress: "pokt1test", ServiceID: "svc-test", State: SessionStateClaiming,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := lc.OnSessionsNeedClaim(ctx, []*SessionSnapshot{snapshot})
	require.Error(t, err)
	assert.Equal(t, SessionStateActive, snapshot.State, "the snapshot the block engine reads must be rewound")
	got, getErr := store.Get(context.Background(), sessionID)
	require.NoError(t, getErr)
	assert.Equal(t, SessionStateActive, got.State, "and Redis with it")
}

// TestOnClaimDeferred_DoesNotRewindASessionWithAClaim: another miner's claim
// must not be claimed twice.
func TestOnClaimDeferred_DoesNotRewindASessionWithAClaim(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()
	const sessionID = "claim-already-claimed"
	saveTestSession(t, store, sessionID, SessionStateClaimed, 10, 100)

	require.Error(t, coord.OnClaimDeferred(context.Background(), sessionID))
	got, err := store.Get(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, SessionStateClaimed, got.State)
}

// TestOnSessionsNeedProof_AGroupThatStopsReturnsItsSessionsToClaimed: a proof
// group that stopped for a transient reason (here: params unreadable) goes back
// to claimed, so the block engine asks for the proof again next block instead
// of leaving it in proving until the window closes.
func TestOnSessionsNeedProof_AGroupThatStopsReturnsItsSessionsToClaimed(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()
	const sessionID = "proof-group-stops"
	saveTestSession(t, store, sessionID, SessionStateProving, 10, 100)

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:       &heightSpyShared{failWi: errors.New("params unavailable at this height")},
		sessionCoordinator: coord,
	}
	snapshot := &SessionSnapshot{
		SessionID: sessionID, SessionEndHeight: 110, SessionStartHeight: 100,
		SupplierOperatorAddress: "pokt1test", ServiceID: "svc-test", State: SessionStateProving,
	}

	_, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{snapshot})
	require.Error(t, err)
	assert.Equal(t, SessionStateClaimed, snapshot.State)
	got, getErr := store.Get(context.Background(), sessionID)
	require.NoError(t, getErr)
	assert.Equal(t, SessionStateClaimed, got.State)
}

// TestDeferProof_RewindsASessionRedisStillHoldsAsClaimed: under Redis OOM the
// write of proving can fail while the proof cycle runs, leaving Redis at
// claimed; that session is still an unsent proof and must be asked for again.
func TestDeferProof_RewindsASessionRedisStillHoldsAsClaimed(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()
	const sessionID = "proof-proving-write-lost"
	saveTestSession(t, store, sessionID, SessionStateClaimed, 10, 100)

	lc := &LifecycleCallback{logger: logging.NewLoggerFromConfig(logging.DefaultConfig()), sessionCoordinator: coord}
	snapshot := &SessionSnapshot{SessionID: sessionID, SupplierOperatorAddress: "pokt1test", State: SessionStateProving}
	lc.deferProof(context.Background(), snapshot)
	assert.Equal(t, SessionStateClaimed, snapshot.State)
}

// TestUpdateStateActive_IsRefusedOverASentClaimAtomically: the claim rewind is
// guarded inside the same script that writes it, so a claim sent between the
// coordinator's read and its write is not rewound.
func TestUpdateStateActive_IsRefusedOverASentClaimAtomically(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()
	saveTestSession(t, store, "claimed-under-us", SessionStateClaimed, 10, 100)
	require.ErrorIs(t, store.UpdateState(ctx, "claimed-under-us", SessionStateActive), ErrSessionNotDeferred)

	saveTestSession(t, store, "unsent-claim", SessionStateClaiming, 10, 100)
	require.NoError(t, store.UpdateState(ctx, "unsent-claim", SessionStateActive))
}

// TestUpdateStateClaimed_IsRefusedOverASentProofAtomically is the proof twin:
// claimed is never written over a proof that was sent or a proof phase that
// ended, while the claim path's claiming -> claimed still writes.
func TestUpdateStateClaimed_IsRefusedOverASentProofAtomically(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	ctx := context.Background()
	sent := storedSession(t, store, "proof-sent-under-us", SessionStateProving)
	sent.ProofTxHash = "ABCD"
	require.NoError(t, store.Save(ctx, sent))
	require.ErrorIs(t, store.UpdateState(ctx, "proof-sent-under-us", SessionStateClaimed), ErrSessionNotDeferred)

	saveTestSession(t, store, "proved-elsewhere", SessionStateProved, 10, 100)
	require.ErrorIs(t, store.UpdateState(ctx, "proved-elsewhere", SessionStateClaimed), ErrSessionNotDeferred)

	saveTestSession(t, store, "unsent-proof", SessionStateProving, 10, 100)
	require.NoError(t, store.UpdateState(ctx, "unsent-proof", SessionStateClaimed))
	saveTestSession(t, store, "claim-sent", SessionStateClaiming, 10, 100)
	require.NoError(t, store.UpdateState(ctx, "claim-sent", SessionStateClaimed))
}

// TestDeferProof_LandsWhenTheCycleContextEnded: the claimed-root read can fail
// because the cycle's context ended; the rewind must still reach Redis, or the
// session stays in proving until the window closes.
func TestDeferProof_LandsWhenTheCycleContextEnded(t *testing.T) {
	store, _ := setupTestSessionStore(t)
	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	defer func() { _ = coord.Close() }()
	snapshot := storedSession(t, store, "proof-cycle-cancelled", SessionStateProving)

	lc := &LifecycleCallback{logger: logging.NewLoggerFromConfig(logging.DefaultConfig()), sessionCoordinator: coord}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lc.deferProof(ctx, snapshot)
	requireState(t, store, snapshot, SessionStateClaimed)
}
