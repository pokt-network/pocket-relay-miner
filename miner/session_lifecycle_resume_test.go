//go:build test

package miner

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// A process that persisted a session's claiming or proving state and died
// before the transaction went out leaves that state in Redis. The process that
// starts next -- a restart, or another instance taking the supplier over --
// loads it from the same Redis.

// resumeCallback records the sessions that reached the claim and proof
// callbacks. Any other callback panics: none is expected here.
type resumeCallback struct {
	SessionLifecycleCallback
	mu     sync.Mutex
	claims []string
	proofs []string
	called chan struct{}
}

func newResumeCallback() *resumeCallback {
	return &resumeCallback{called: make(chan struct{}, 16)}
}

func (c *resumeCallback) OnSessionsNeedClaim(_ context.Context, sessions []*SessionSnapshot) (ClaimCycleResult, error) {
	c.mu.Lock()
	for _, s := range sessions {
		c.claims = append(c.claims, s.SessionID)
	}
	c.mu.Unlock()
	c.called <- struct{}{}
	return ClaimCycleResult{}, nil
}

func (c *resumeCallback) OnSessionsNeedProof(_ context.Context, sessions []*SessionSnapshot) (ProofCycleResult, error) {
	c.mu.Lock()
	for _, s := range sessions {
		c.proofs = append(c.proofs, s.SessionID)
	}
	c.mu.Unlock()
	c.called <- struct{}{}
	return ProofCycleResult{}, nil
}

func (c *resumeCallback) sent() (claims, proofs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.claims...), append([]string(nil), c.proofs...)
}

const (
	resumeSupplier   = "pokt1resume"
	resumeSessionEnd = 100
)

// startAfterRestart saves the snapshots as the process that died left them, and
// starts a new lifecycle manager on the same Redis at height.
func startAfterRestart(t *testing.T, snapshot *SessionSnapshot, height int64, more ...*SessionSnapshot) (*SessionLifecycleManager, *RedisSessionStore, *resumeCallback) {
	t.Helper()
	client, _ := newTestRedis(t)
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	store := NewRedisSessionStore(logger, client, SessionStoreConfig{SupplierAddress: resumeSupplier})
	for _, s := range append([]*SessionSnapshot{snapshot}, more...) {
		require.NoError(t, store.Save(context.Background(), s))
	}

	cb := newResumeCallback()
	pool := pond.NewPool(4)
	t.Cleanup(pool.StopAndWait)
	m := NewSessionLifecycleManager(logger, store, &mockSharedQueryClient{}, &mockBlockClient{currentHeight: height}, cb,
		SessionLifecycleConfig{SupplierAddress: resumeSupplier, CheckIntervalBlocks: 1}, pool)
	// The claim's flush delay ends at once: nothing is left in the stream.
	m.SetMaxNonReclaimHandledMsgIDLookup(func() (streamMsgID, bool) { return streamMsgID{}, false })
	m.SetLastGeneratedMsgIDLookup(func(context.Context) (streamMsgID, bool, error) { return streamMsgID{}, false, nil })
	require.NoError(t, m.Start(context.Background()))
	return m, store, cb
}

func resumeSnapshot(state SessionState) *SessionSnapshot {
	return &SessionSnapshot{
		SessionID:               "sess-resume",
		SupplierOperatorAddress: resumeSupplier,
		ServiceID:               "svc",
		SessionStartHeight:      resumeSessionEnd - 3,
		SessionEndHeight:        resumeSessionEnd,
		State:                   state,
	}
}

// beaconSnapshot is a second session in the same state with no transaction sent,
// which the fix always resumes: waiting for it to reach the callback is what
// makes "the other one never reached it" an answer instead of a race.
func beaconSnapshot(state SessionState) *SessionSnapshot {
	s := resumeSnapshot(state)
	s.SessionID = "sess-beacon"
	if state == SessionStateProving {
		s.ClaimTxHash = "CLAIMTX"
		s.ClaimedRootHash = make([]byte, 48)
	}
	return s
}

func claimedSnapshot(state SessionState) *SessionSnapshot {
	s := resumeSnapshot(state)
	s.ClaimTxHash = "CLAIMTX"
	s.ClaimedRootHash = make([]byte, 48)
	return s
}

// The heights come from the params the manager reads, not from the defaults.
func resumeParams(t *testing.T) *sharedtypes.Params {
	t.Helper()
	params, err := (&mockSharedQueryClient{}).GetParams(context.Background())
	require.NoError(t, err)
	return params
}

func proofWindowOpen(t *testing.T) int64 {
	return sharedtypes.GetProofWindowOpenHeight(resumeParams(t), resumeSessionEnd)
}

func claimWindowOpen(t *testing.T) int64 {
	return sharedtypes.GetClaimWindowOpenHeight(resumeParams(t), resumeSessionEnd)
}

func requireCalled(t *testing.T, cb *resumeCallback, msg string) {
	t.Helper()
	select {
	case <-cb.called:
	case <-time.After(10 * time.Second):
		t.Fatal(msg)
	}
}

func TestLifecycleStart_AClaimedSessionReachesTheProofCallback(t *testing.T) {
	m, _, cb := startAfterRestart(t, claimedSnapshot(SessionStateClaimed), proofWindowOpen(t))
	requireCalled(t, cb, "control: a claimed session with its proof window open reaches the proof callback")
	require.NoError(t, m.Close())
	_, proofs := cb.sent()
	require.Equal(t, []string{"sess-resume"}, proofs)
}

func TestLifecycleStart_SendsTheProofOfASessionLeftProvingWithoutOne(t *testing.T) {
	resumed := testutil.ToFloat64(sessionSnapshotsResumedAtStartup.WithLabelValues(resumeSupplier, string(SessionStateProving)))
	m, store, cb := startAfterRestart(t, claimedSnapshot(SessionStateProving), proofWindowOpen(t))
	requireCalled(t, cb, "LINK resume-proving: a session left proving with no proof sent gets its proof sent after a restart")
	require.Equal(t, resumed+1, testutil.ToFloat64(sessionSnapshotsResumedAtStartup.WithLabelValues(resumeSupplier, string(SessionStateProving))),
		"LINK resume-counted: the resumed session is counted by the state it was loaded in")
	require.NoError(t, m.Close())
	_, proofs := cb.sent()
	require.Equal(t, []string{"sess-resume"}, proofs, "once")

	got, err := store.Get(context.Background(), "sess-resume")
	require.NoError(t, err)
	require.Equal(t, SessionStateProving, got.State, "it is proving again, as any session whose proof is being sent")
}

func TestLifecycleStart_DoesNotResendAProofAlreadySent(t *testing.T) {
	sent := claimedSnapshot(SessionStateProving)
	sent.ProofTxHash = "PROOFTX"
	m, store, cb := startAfterRestart(t, sent, proofWindowOpen(t), beaconSnapshot(SessionStateProving))
	requireCalled(t, cb, "premise: the beacon session left proving with no proof reaches the proof callback")
	require.NoError(t, m.Close()) // drains whatever Start dispatched
	claims, proofs := cb.sent()
	require.Equal(t, []string{"sess-beacon"}, proofs, "LINK resume-proof-sent: a proof already sent is not sent again")
	require.Empty(t, claims)

	got, err := store.Get(context.Background(), "sess-resume")
	require.NoError(t, err)
	require.Equal(t, SessionStateProving, got.State)
	require.Equal(t, "PROOFTX", got.ProofTxHash)
}

func TestLifecycleStart_SendsTheClaimOfASessionLeftClaimingWithoutOne(t *testing.T) {
	m, _, cb := startAfterRestart(t, resumeSnapshot(SessionStateClaiming), claimWindowOpen(t))
	requireCalled(t, cb, "LINK resume-claiming: a session left claiming with no claim sent gets its claim sent after a restart")
	require.NoError(t, m.Close())
	claims, _ := cb.sent()
	require.Equal(t, []string{"sess-resume"}, claims, "once")
}

func TestLifecycleStart_DoesNotResendAClaimAlreadySent(t *testing.T) {
	sent := claimedSnapshot(SessionStateClaiming)
	resumed := testutil.ToFloat64(sessionSnapshotsResumedAtStartup.WithLabelValues(resumeSupplier, string(SessionStateClaiming)))
	m, store, cb := startAfterRestart(t, sent, claimWindowOpen(t), beaconSnapshot(SessionStateClaiming))
	requireCalled(t, cb, "premise: the beacon session left claiming with no claim reaches the claim callback")
	require.NoError(t, m.Close())
	claims, proofs := cb.sent()
	require.Equal(t, []string{"sess-beacon"}, claims, "only the session with no claim sent is claimed")
	require.Empty(t, proofs, "its proof window is not open")

	// The claim path dedups by hash on its own (lifecycle_callback.go:1119 and
	// session_lifecycle.go:1232), so what is observable here is that the
	// session was not moved back at all: its state and the counter.
	got, err := store.Get(context.Background(), "sess-resume")
	require.NoError(t, err)
	require.Equal(t, SessionStateClaiming, got.State, "LINK resume-claim-sent: a session whose claim was sent is not moved back")
	require.Equal(t, "CLAIMTX", got.ClaimTxHash)
	require.Equal(t, resumed+1, testutil.ToFloat64(sessionSnapshotsResumedAtStartup.WithLabelValues(resumeSupplier, string(SessionStateClaiming))),
		"LINK resume-claim-sent: only the beacon is counted as resumed")
}

// A session left claiming goes back to active, and its claim is built again.
// The tree it claims is the one the process that died had already sealed: its
// root must be the sealed one, whatever the claim it never sent would have
// carried, and no relay may enter it any more.
func TestFlushTree_AfterARestartClaimsTheSealedRootOfASessionLeftClaiming(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	relays := coldRelays(41, 500)

	const session = "sess-resume-root"
	sealed := claimColdTree(t, ctx, NewRedisSMSTManager(zerolog.Nop(), client,
		RedisSMSTManagerConfig{SupplierAddress: resumeSupplier, CacheTTL: time.Hour}), session, relays)

	// The process that starts next has no tree in memory and reads the same Redis.
	next := NewRedisSMSTManager(zerolog.Nop(), client,
		RedisSMSTManagerConfig{SupplierAddress: resumeSupplier, CacheTTL: time.Hour})
	again, err := next.FlushTree(ctx, session)
	require.NoError(t, err, "LINK resume-claim-root: the claim after the restart finds the sealed tree in Redis")
	require.Equal(t, sealed, again, "LINK resume-claim-root: the claim after the restart carries the root that was sealed")

	late := coldRelays(42, 1)[0]
	err = next.UpdateTree(ctx, session, late.key, late.value, late.weight)
	require.ErrorIs(t, err, ErrSessionClaimed, "LINK resume-claim-root: no relay enters a sealed tree after the restart")

	third, err := next.FlushTree(ctx, session)
	require.NoError(t, err)
	require.Equal(t, sealed, third, "LINK resume-claim-root: the root does not move on a second claim")
	path := sha256.Sum256([]byte("resume-root-path"))
	proof, err := next.ProveClosest(ctx, session, path[:])
	require.NoError(t, err)
	ok, _ := chainVerifies(t, proof, sealed)
	require.True(t, ok, "LINK resume-claim-root: the proof of the resumed claim verifies against the sealed root")
}
