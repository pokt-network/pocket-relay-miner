//go:build test

package miner

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// A miner that stopped between a claim and its compaction left the tree whole
// in Redis: the claim path is the only place that queues the compaction, and
// the queue is the process's. The miner that starts next reads the same Redis.

const resumeColdSupplier = "pokt1cold_resume"

// resumedCallback records what the lifecycle hands back on startup.
type resumedCallback struct {
	SessionLifecycleCallback
	resumed []string
}

func (c *resumedCallback) OnClaimedSessionsResumed(_ context.Context, sessions []*SessionSnapshot) {
	for _, s := range sessions {
		c.resumed = append(c.resumed, s.SessionID)
	}
}

// startLifecycleWith saves the snapshots as the process that died left them and
// starts a lifecycle manager on the same Redis, with the given callback.
func startLifecycleWith(t *testing.T, callback SessionLifecycleCallback, snapshots ...*SessionSnapshot) *SessionLifecycleManager {
	t.Helper()
	ctx := context.Background()
	client, _ := newTestRedis(t)
	store := NewRedisSessionStore(zerolog.Nop(), client, SessionStoreConfig{SupplierAddress: resumeColdSupplier})
	for _, s := range snapshots {
		require.NoError(t, store.Save(ctx, s))
	}
	pool := pond.NewPool(4)
	t.Cleanup(pool.StopAndWait)
	m := NewSessionLifecycleManager(zerolog.Nop(), store, &mockSharedQueryClient{}, &mockBlockClient{currentHeight: 6}, callback,
		SessionLifecycleConfig{SupplierAddress: resumeColdSupplier, CheckIntervalBlocks: 1}, pool)
	m.SetMaxNonReclaimHandledMsgIDLookup(func() (streamMsgID, bool) { return streamMsgID{}, false })
	m.SetLastGeneratedMsgIDLookup(func(context.Context) (streamMsgID, bool, error) { return streamMsgID{}, false, nil })
	require.NoError(t, m.Start(ctx))
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func coldSnapshot(sessionID string, state SessionState) *SessionSnapshot {
	return &SessionSnapshot{
		SessionID: sessionID, SupplierOperatorAddress: resumeColdSupplier, ServiceID: "svc",
		SessionStartHeight: 1, SessionEndHeight: 4, State: state,
		ClaimTxHash: "CLAIMTX", ClaimedRootHash: make([]byte, SMSTRootLen),
	}
}

// startedWithEveryState loads one session per state, so each test below reads
// the same hand-back from its own angle and a defect names the property it
// broke instead of whichever assertion runs first.
func startedWithEveryState(t *testing.T) []string {
	t.Helper()
	cb := &resumedCallback{}
	startLifecycleWith(t, cb,
		coldSnapshot("sess-cold-claimed", SessionStateClaimed),
		coldSnapshot("sess-cold-proving", SessionStateProving),
		coldSnapshot("sess-cold-claiming", SessionStateClaiming),
		coldSnapshot("sess-cold-active", SessionStateActive),
		coldSnapshot("sess-cold-proved", SessionStateProved),
	)
	return cb.resumed
}

func TestLifecycleStart_HandsBackTheSessionsWhoseClaimWasSent(t *testing.T) {
	resumed := startedWithEveryState(t)
	require.Contains(t, resumed, "sess-cold-claimed",
		"LINK cold-resume: a session loaded with its claim sent is handed back")
	require.Contains(t, resumed, "sess-cold-proving",
		"LINK cold-resume: a session being proved is handed back too")
}

func TestLifecycleStart_DoesNotHandBackASessionStillSendingItsClaim(t *testing.T) {
	resumed := startedWithEveryState(t)
	require.NotContains(t, resumed, "sess-cold-claiming",
		"LINK cold-resume-claiming: a session that still has to send its claim is not handed back -- its claimed_root is already in Redis")
	require.NotContains(t, resumed, "sess-cold-active",
		"a session being served is not handed back")
}

// A terminal session is kept out twice: loadExistingSessions does not load it,
// and the hand-back takes only claimed and proving. No single injection turns
// this red -- each half covers the other -- so this is a guard against both
// being removed, not a test with a tooth of its own.
func TestLifecycleStart_DoesNotHandBackATerminalSession(t *testing.T) {
	require.True(t, SessionStateProved.IsTerminal(), "premise: the state is terminal")
	require.NotContains(t, startedWithEveryState(t), "sess-cold-proved",
		"a terminal session is not handed back")
}

// The real callback turns that hand-back into the compaction the claim path
// queues, and the tree's proof still verifies against the claim that was sent.
func TestOnClaimedSessionsResumed_CompactsTheTreeAndItsProofStillVerifies(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	smst := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: resumeColdSupplier, CacheTTL: time.Hour})
	const session = "sess-cold-callback"
	claimed := claimColdTree(t, ctx, smst, session, coldRelays(61, 40))
	// A new process holds no tree: the compaction works from Redis only.
	smst.treesMu.Lock()
	smst.trees = make(map[string]*redisSMST)
	smst.treesMu.Unlock()

	lc := &LifecycleCallback{
		logger:      logging.NewLoggerFromConfig(logging.DefaultConfig()),
		smstManager: smst,
		config:      LifecycleCallbackConfig{SupplierAddress: resumeColdSupplier},
	}
	lc.OnClaimedSessionsResumed(ctx, []*SessionSnapshot{{SessionID: session, SupplierOperatorAddress: resumeColdSupplier, State: SessionStateClaimed}})

	compacted, err := smst.coldTreeCompacted(ctx, session)
	require.NoError(t, err)
	require.True(t, compacted, "LINK cold-resume-compacts: the tree of a session claimed before the restart is stored as its leaves")

	path := sha256.Sum256([]byte("cold-resume-callback-path"))
	proof, err := smst.ProveClosest(ctx, session, path[:])
	require.NoError(t, err)
	ok, _ := chainVerifies(t, proof, claimed)
	require.True(t, ok, "LINK cold-resume-compacts: the proof of the compacted tree verifies against its claimed root")
}

// The nodes hash goes only while the leaves blob is the verified one: another
// process compacting the same session must not be able to leave it with
// neither the hash nor a blob that rebuilds its claim.
func TestUnlinkNodesIfBlob_KeepsTheNodesWhenTheBlobChanged(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	kb := client.KB()
	nodesKey := kb.SMSTNodesKey(resumeColdSupplier, "sess-cold-script")
	leavesKey := kb.SMSTLeavesKey(resumeColdSupplier, "sess-cold-script")
	require.NoError(t, client.HSet(ctx, nodesKey, "node", "value").Err())
	require.NoError(t, client.Set(ctx, leavesKey, "verified-blob", time.Hour).Err())

	deleted, err := unlinkNodesIfBlobScript.Run(ctx, client, []string{nodesKey, leavesKey}, []byte("another-blob")).Int64()
	require.NoError(t, err)
	require.Zero(t, deleted, "LINK cold-unlink-conditional: a blob that is not the verified one leaves the nodes hash")
	exists, err := client.Exists(ctx, nodesKey).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, exists, "LINK cold-unlink-conditional: the nodes hash is untouched")
	blob, err := client.Get(ctx, leavesKey).Result()
	require.NoError(t, err)
	require.Equal(t, "verified-blob", blob, "LINK cold-unlink-conditional: the blob is untouched")

	deleted, err = unlinkNodesIfBlobScript.Run(ctx, client, []string{nodesKey, leavesKey}, []byte("verified-blob")).Int64()
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "LINK cold-unlink-conditional: the verified blob deletes the nodes hash")
	exists, err = client.Exists(ctx, nodesKey).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "LINK cold-unlink-conditional: the nodes hash is gone")
}
