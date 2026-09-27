//go:build test

package miner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// handlerTestFixture wires the minimum real components needed to exercise
// SupplierWorker.handleRelay end-to-end against miniredis. It does not
// start any background goroutines — the test drives handleRelay directly.
type handlerTestFixture struct {
	t           *testing.T
	ctx         context.Context
	cancel      context.CancelFunc
	redisClient *redisutil.Client
	failRedis   *testredis.FailSwitch

	supplierAddr string

	sessionStore *RedisSessionStore
	coordinator  *SessionCoordinator
	smstMgr      *RedisSMSTManager
	dedup        *RedisDeduplicator

	worker *SupplierWorker
}

func newHandlerTestFixture(t *testing.T, supplierAddr string) *handlerTestFixture {
	t.Helper()

	parent, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	client, _ := newTestRedis(t)
	failRedis := testredis.NewFailSwitch(client)
	t.Cleanup(func() { _ = client.Close() })

	logger := zerolog.Nop()

	sessionStore := NewRedisSessionStore(logger, client, SessionStoreConfig{
		SupplierAddress: supplierAddr,
	})
	coordinator := NewSessionCoordinator(logger, sessionStore, SMSTRecoveryConfig{
		SupplierAddress: supplierAddr,
	})
	smstMgr := NewRedisSMSTManager(logger, client, RedisSMSTManagerConfig{
		SupplierAddress: supplierAddr,
		CacheTTL:        0,
	})
	dedup := NewRedisDeduplicator(logger, client, DeduplicatorConfig{
		TTLBlocks:        10,
		BlockTimeSeconds: 30,
	})

	// Construct a minimal SupplierManager carrying only the fields that
	// handleRelay reads: the suppliers map and the deduplicator.
	mgr := &SupplierManager{
		logger:       logger,
		suppliers:    xsync.NewMap[string, *SupplierState](),
		deduplicator: dedup,
	}
	state := &SupplierState{
		OperatorAddr:       supplierAddr,
		SessionStore:       sessionStore,
		SessionCoordinator: coordinator,
		SMSTManager:        smstMgr,
	}
	state.StoreStatus(SupplierStatusActive)
	mgr.suppliers.Store(supplierAddr, state)

	worker := &SupplierWorker{
		logger: logging.ForComponent(logger, "supplier_worker_test"),
		config: SupplierWorkerConfig{Logger: logger},
	}
	worker.supplierManager = mgr

	return &handlerTestFixture{
		failRedis:    failRedis,
		t:            t,
		ctx:          parent,
		cancel:       cancel,
		redisClient:  client,
		supplierAddr: supplierAddr,
		sessionStore: sessionStore,
		coordinator:  coordinator,
		smstMgr:      smstMgr,
		dedup:        dedup,
		worker:       worker,
	}
}

// newStreamMessage builds a syntactically-valid StreamMessage that exercises
// the full handleRelay pipeline (SMST update + MarkProcessed + coordinator).
func newStreamMessage(supplierAddr, sessionID, payload string, computeUnits uint64) *transport.StreamMessage {
	hash := sha256.Sum256([]byte(payload))
	return &transport.StreamMessage{
		ID:         "0-1",
		StreamName: "test-stream",
		Message: &transport.MinedRelayMessage{
			RelayHash:               hash[:],
			RelayBytes:              []byte(payload),
			ComputeUnitsPerRelay:    computeUnits,
			SessionId:               sessionID,
			SessionStartHeight:      1,
			SessionEndHeight:        10,
			SupplierOperatorAddress: supplierAddr,
			ServiceId:               "svc-1",
			ApplicationAddress:      "pokt1app",
			ArrivalBlockHeight:      2,
		},
	}
}

// TestHandleRelay_HappyPath_AckAndCoordinatorIncremented is a baseline that
// verifies the fixture wiring without error injection. It pins the known-good
// behavior so the two error-path tests below demonstrably diverge from it.
func TestHandleRelay_HappyPath_AckAndCoordinatorIncremented(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1supplier")
	msg := newStreamMessage(f.supplierAddr, "sess-happy", "relay-1", 100)

	err := f.worker.handleRelay(f.ctx, f.supplierAddr, msg)
	require.NoError(t, err, "happy path must ACK")

	snap, getErr := f.sessionStore.Get(f.ctx, "sess-happy")
	require.NoError(t, getErr)
	require.NotNil(t, snap, "session must have been created by coordinator")
	assert.Equal(t, uint64(100), snap.TotalComputeUnits,
		"coordinator must have incremented compute units exactly once")
	assert.Equal(t, int64(1), snap.RelayCount,
		"coordinator must have incremented relay count exactly once")
}

// TestHandleRelay_ContextCanceled_DoesNotLeakToStreamPending reproduces Bug A.
//
// Before the fix:
//   - IsRetryableError(context.Canceled) == true
//   - UpdateTree wrapped context.Canceled in ErrSMSTUpdateFailed
//   - handleRelay returned a non-nil error
//   - The stream message would stay in XPENDING (leak)
//
// After the fix:
//   - IsShutdownCancelError detects the cancellation
//   - handleRelay ACKs the message (returns nil)
//   - The same message is re-delivered by XREADGROUP to the next consumer
//     after restart, so no relay is lost.
func TestHandleRelay_ContextCanceled_DoesNotLeakToStreamPending(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1supplier")

	// Pre-cancel the context to simulate graceful shutdown / supplier
	// removal. go-redis v9 returns context.Canceled from Redis calls when
	// the ctx is already cancelled before the call is issued.
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()

	msg := newStreamMessage(f.supplierAddr, "sess-shutdown", "relay-shutdown", 100)

	err := f.worker.handleRelay(ctx, f.supplierAddr, msg)
	require.NoError(t, err,
		"handleRelay must return nil on shutdown-origin cancel so the stream message is ACK'd")

	// Belt-and-suspenders: even if Redis didn't fail fast, the ctx.Err()
	// check in handleRelay should still catch the cancellation. Confirm no
	// coordinator increment leaked through.
	snap, _ := f.sessionStore.Get(f.ctx, "sess-shutdown")
	if snap != nil {
		assert.LessOrEqual(t, snap.TotalComputeUnits, uint64(100),
			"at most one increment must reach the coordinator even if the SMST update succeeded")
	}
}

// errorOnIncrementStore wraps a RedisSessionStore and returns a non-terminal
// error on IncrementRelayCount so we can force SessionCoordinator.OnRelayProcessed
// to fail without touching coordinator internals.
type errorOnIncrementStore struct {
	inner      SessionStore
	incCalls   atomic.Int32
	injectErr  error
	delegateOK bool
}

func (s *errorOnIncrementStore) Save(ctx context.Context, snapshot *SessionSnapshot) error {
	return s.inner.Save(ctx, snapshot)
}
func (s *errorOnIncrementStore) CreateIfAbsent(ctx context.Context, snapshot *SessionSnapshot) (bool, error) {
	return s.inner.CreateIfAbsent(ctx, snapshot)
}
func (s *errorOnIncrementStore) Get(ctx context.Context, sessionID string) (*SessionSnapshot, error) {
	return s.inner.Get(ctx, sessionID)
}
func (s *errorOnIncrementStore) GetBySupplier(ctx context.Context) ([]*SessionSnapshot, error) {
	return s.inner.GetBySupplier(ctx)
}
func (s *errorOnIncrementStore) GetByState(ctx context.Context, state SessionState) ([]*SessionSnapshot, error) {
	return s.inner.GetByState(ctx, state)
}
func (s *errorOnIncrementStore) Delete(ctx context.Context, sessionID string) error {
	return s.inner.Delete(ctx, sessionID)
}
func (s *errorOnIncrementStore) UpdateState(ctx context.Context, sessionID string, newState SessionState) error {
	return s.inner.UpdateState(ctx, sessionID, newState)
}
func (s *errorOnIncrementStore) ReactivateClaimed(ctx context.Context, sessionID string, claimedRootHash []byte, claimTxHash string) (bool, error) {
	return s.inner.ReactivateClaimed(ctx, sessionID, claimedRootHash, claimTxHash)
}
func (s *errorOnIncrementStore) IncrementRelayCount(ctx context.Context, sessionID string, computeUnits uint64) error {
	s.incCalls.Add(1)
	if s.delegateOK {
		return s.inner.IncrementRelayCount(ctx, sessionID, computeUnits)
	}
	return s.injectErr
}
func (s *errorOnIncrementStore) Close() error { return s.inner.Close() }

// TestHandleRelay_OnRelayProcessedError_AcksAfterSmstAndDedup reproduces Bug B.
//
// Before the fix:
//   - UpdateTree succeeds (tree committed to Redis).
//   - MarkProcessed succeeds (dedup hash present).
//   - OnRelayProcessed errors (simulated Redis failure on IncrementRelayCount).
//   - handleRelay returned the wrapped error.
//   - The stream message would stay pending; the reclaim takes it; dedup
//     correctly rejects the reclaim for the SMST but the TotalComputeUnits
//     counter could be double-incremented if the dedup entry expired or
//     was cleaned up between the crash and the reclaim.
//
// After the fix:
//   - handleRelay ACKs (returns nil) even when OnRelayProcessed errors.
//   - SMST and dedup are left in truth; coordinator is best-effort.
func TestHandleRelay_OnRelayProcessedError_AcksAfterSmstAndDedup(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1supplier")

	injected := errors.New("injected coordinator failure")
	fakeStore := &errorOnIncrementStore{
		inner:     f.sessionStore,
		injectErr: injected,
	}
	// Swap the coordinator's backing store so OnRelayProcessed -> Save
	// still works (session creation) but IncrementRelayCount errors.
	logger := zerolog.Nop()
	f.coordinator = NewSessionCoordinator(logger, fakeStore, SMSTRecoveryConfig{
		SupplierAddress: f.supplierAddr,
	})
	state, ok := f.worker.supplierManager.GetSupplierState(f.supplierAddr)
	require.True(t, ok)
	state.SessionCoordinator = f.coordinator

	msg := newStreamMessage(f.supplierAddr, "sess-coord-err", "relay-coord-err", 200)
	relayHash := append([]byte(nil), msg.Message.RelayHash...)

	err := f.worker.handleRelay(f.ctx, f.supplierAddr, msg)
	require.NoError(t, err,
		"handleRelay must ACK after SMST + dedup succeed even when coordinator errors")

	// Dedup must have recorded the relay hash (proves MarkProcessed ran).
	isDup, dupErr := f.dedup.IsDuplicate(f.ctx, relayHash, "sess-coord-err")
	require.NoError(t, dupErr)
	assert.True(t, isDup, "MarkProcessed must have stamped the dedup set before the coordinator call")

	// IncrementRelayCount must have been attempted exactly once (not
	// retried). This is the concrete guard against double-count on reclaim:
	// the coordinator write happens at most once per handleRelay invocation.
	assert.Equal(t, int32(1), fakeStore.incCalls.Load(),
		"OnRelayProcessed must be attempted exactly once per handleRelay call")
}

// TestHandleRelay_EmptyRelayHash_RecomputedFromBytes verifies the defensive
// recompute path added so a publisher bug that ships MinedRelayMessage with
// RelayHash=nil does not collapse every event into a single SMST leaf.
// Two distinct RelayBytes with nil RelayHash must land as two distinct dedup
// entries (i.e. recomputed to two different hashes) and two distinct relays
// in the coordinator.
//
// handleRelay clears RelayHash/RelayBytes on its in-memory msg after the
// SMST update for GC; the observable effects are the dedup set contents and
// the session coordinator counters, not the message fields post-call.
func TestHandleRelay_EmptyRelayHash_RecomputedFromBytes(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1supplier")

	// First message: nil RelayHash, payload "relay-A".
	msgA := newStreamMessage(f.supplierAddr, "sess-recompute", "relay-A", 100)
	msgA.Message.RelayHash = nil
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, msgA),
		"first empty-hash relay must be recomputed and ACK'd")

	// Second message: nil RelayHash, DIFFERENT payload "relay-B".
	msgB := newStreamMessage(f.supplierAddr, "sess-recompute", "relay-B", 100)
	msgB.Message.RelayHash = nil
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, msgB))

	// Coordinator must have counted both (not collapsed via nil-key).
	snap, err := f.sessionStore.Get(f.ctx, "sess-recompute")
	require.NoError(t, err)
	require.NotNil(t, snap)
	assert.Equal(t, int64(2), snap.RelayCount,
		"two distinct payloads with nil RelayHash must land as two distinct relays")
	assert.Equal(t, uint64(200), snap.TotalComputeUnits)

	// Dedup must contain both recomputed hashes — proves the recompute
	// produced two different keys rather than collapsing to a shared empty key.
	expectedA := sha256.Sum256([]byte("relay-A"))
	expectedB := sha256.Sum256([]byte("relay-B"))
	isDupA, dupErrA := f.dedup.IsDuplicate(f.ctx, expectedA[:], "sess-recompute")
	require.NoError(t, dupErrA)
	assert.True(t, isDupA, "dedup must have recorded the recomputed hash of RelayBytes A")
	isDupB, dupErrB := f.dedup.IsDuplicate(f.ctx, expectedB[:], "sess-recompute")
	require.NoError(t, dupErrB)
	assert.True(t, isDupB, "dedup must have recorded the recomputed hash of RelayBytes B")
}

// poolTimeoutOnNodesFlush fails the first N HSETs against ONE session's SMST
// node hash with redis.ErrPoolTimeout -- by identity, not by message -- and
// counts that it actually fired.
//
// Scoped rather than testredis.FailSwitch, for two independent reasons. Fail
// builds a fresh errors.New (internal/testredis/inject.go:117-120), so the
// injected value would not satisfy errors.Is against the sentinel and the test
// would be asserting on go-redis's wording; and it fails EVERY command, so the
// session read and the deduplicator would break too and the relay could die
// before it ever reaches the flush.
//
// Exactly one HSET per relay reaches this key on this path: updateTree calls
// BeginPipeline (smst_manager.go:660-662), which makes RedisMapStore.Set buffer
// (redis_mapstore.go:150-166) instead of issuing its own HSET, leaving
// FlushPipeline's single multi-field HSET (redis_mapstore.go:333) as the only
// one. fired checks that rather than trusting it.
type poolTimeoutOnNodesFlush struct {
	key      string
	failNext atomic.Int64
	fired    atomic.Int64
}

func (h *poolTimeoutOnNodesFlush) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *poolTimeoutOnNodesFlush) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "hset" && len(args) > 1 && fmt.Sprint(args[1]) == h.key &&
			h.failNext.Add(-1) >= 0 {
			h.fired.Add(1)
			// Client.Process puts this on the Cmd, so HSet(...).Err()
			// returns it (go-redis v9.22.0 redis.go, Client.Process).
			return redis.ErrPoolTimeout
		}
		return next(ctx, cmd)
	}
}

func (h *poolTimeoutOnNodesFlush) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

// TestHandleRelay_APoolTimeoutIsRetriedNotDiscarded is the I2b defect, walked
// end to end: a relay that was already served, signed and answered to the
// client must not be thrown away because every pooled connection happened to be
// busy when its tree was written.
//
// redis.ErrPoolTimeout is a bare errors.New (go-redis v9.22.0
// internal/pool/pool.go:65), so it is not a net.Error, not a context error and
// not ErrClosed. Before the fix IsRetryableError said false, the flush error's
// ErrSMSTCommitFailed wrapper (smst_manager.go:679) made IsPermanentSMSTError
// say true, and handleRelay returned nil -- an ACK, which is XAckDel with
// DELREF, so the entry is DELETED and no replica can rescue it. It was counted
// as "session_sealed": arrived late. It did not arrive late.
//
// The assertions are the consequence, not the classification: the return value
// decides between AckMessage and ReleaseMessage in handleStreamMessage
// (supplier_manager.go:2248-2280), and the reason label decides what the
// operator is told it cost.
func TestHandleRelay_APoolTimeoutIsRetriedNotDiscarded(t *testing.T) {
	const sessionID = "sess-pool-timeout"
	f := newHandlerTestFixture(t, "pokt1poolto")

	nodesKey := f.redisClient.KB().SMSTNodesKey(f.supplierAddr, sessionID)
	hook := &poolTimeoutOnNodesFlush{key: nodesKey}
	hook.failNext.Store(1)
	f.redisClient.AddHook(hook)

	// Child series with this test's own supplier label: relays_failed_smst is
	// touched by other tests in this binary, and the whole vec would count
	// their traffic.
	sealed := relaysFailedSMST.WithLabelValues(f.supplierAddr, "svc-1", "session_sealed")
	transient := relaysFailedSMST.WithLabelValues(f.supplierAddr, "svc-1", "transient_error")
	rejected := relaysRejected.WithLabelValues(f.supplierAddr, "session_sealed", "svc-1")
	sealedBefore := testutil.ToFloat64(sealed)
	transientBefore := testutil.ToFloat64(transient)
	rejectedBefore := testutil.ToFloat64(rejected)

	msg := newStreamMessage(f.supplierAddr, sessionID, "relay-pool-timeout", 100)
	err := f.worker.handleRelay(f.ctx, f.supplierAddr, msg)

	// The control comes first: without it a hook that matched nothing looks
	// exactly like a fix that works, and every assertion below would pass on
	// a relay that never met the injected failure.
	require.Equal(t, int64(1), hook.fired.Load(),
		"the injected pool timeout must have hit a real command: if the hook matched "+
			"nothing, this test proves nothing")

	require.Error(t, err,
		"a pool timeout must be handed back for another delivery; returning nil ACKs the "+
			"relay and XAckDel deletes it from the stream")
	require.ErrorIs(t, err, redis.ErrPoolTimeout,
		"and it must still carry the cause, so handleStreamMessage's release is traceable")

	assert.Equal(t, sealedBefore, testutil.ToFloat64(sealed),
		"a busy pool is not a sealed session: counting it as session_sealed tells the "+
			"operator the relay arrived late when it arrived on time")
	assert.Equal(t, rejectedBefore, testutil.ToFloat64(rejected),
		"and it must not be counted as a rejected relay either")
	assert.Equal(t, transientBefore+1, testutil.ToFloat64(transient),
		"it is the transient class, and it has to be visible as such")
}
