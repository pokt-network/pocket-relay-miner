//go:build test

package miner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// A real session end on the 4-block grid of mockSharedQueryClient's params, so
// the window heights the cut computes are the ones the chain would.
const (
	cutSessionStart = int64(97)
	cutSessionEnd   = int64(100)
)

// redisCommandCounter counts every command the fixture's client sends, piped or
// not, so a test can say a path did not touch Redis at all.
type redisCommandCounter struct{ commands atomic.Int64 }

func (c *redisCommandCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *redisCommandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.commands.Add(1)
		return next(ctx, cmd)
	}
}

func (c *redisCommandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.commands.Add(int64(len(cmds)))
		return next(ctx, cmds)
	}
}

// cutFixture wires the handleRelay fixture with an observed height and shared
// params, and takes the session store away from the supplier state: the cut
// must decide before, and without, the session read.
type cutFixture struct {
	*handlerTestFixture
	shared     *mockSharedQueryClient
	params     *sharedtypes.Params
	open       int64 // claim window open height of the session
	close      int64 // claim window close height of the session
	paramReads atomic.Int32
}

func newCutFixture(t *testing.T, supplier string, height int64) *cutFixture {
	t.Helper()
	f := &cutFixture{handlerTestFixture: newHandlerTestFixture(t, supplier), shared: &mockSharedQueryClient{}}
	params, err := f.shared.GetParams(context.Background())
	require.NoError(t, err)
	f.params = params
	f.open = sharedtypes.GetClaimWindowOpenHeight(params, cutSessionEnd)
	f.close = sharedtypes.GetClaimWindowCloseHeight(params, cutSessionEnd)
	require.Greater(t, f.close, f.open+claimFlushCapBlocks,
		"premise: the flush cap falls inside the claim window, so the two reasons have their own heights")

	f.shared.paramsAtHeightFn = func(_ context.Context, h int64) (*sharedtypes.Params, error) {
		f.paramReads.Add(1)
		require.Equal(t, cutSessionEnd, h, "params must be read at the session end height, never at the current one")
		return params, nil
	}
	mgr := f.worker.supplierManager
	mgr.config.BlockClient = &mockBlockClient{currentHeight: height}
	mgr.config.SharedClient = f.shared

	state, ok := mgr.suppliers.Load(supplier)
	require.True(t, ok)
	state.SessionStore = nil
	return f
}

func (f *cutFixture) message(sessionID string) *transport.StreamMessage {
	msg := newStreamMessage(f.supplierAddr, sessionID, "payload-"+sessionID, 7)
	msg.Message.SessionStartHeight = cutSessionStart
	msg.Message.SessionEndHeight = cutSessionEnd
	return msg
}

func (f *cutFixture) rejected(reason string) float64 {
	return testutil.ToFloat64(relaysRejected.WithLabelValues(f.supplierAddr, reason, "svc-1"))
}

func (f *cutFixture) added() float64 {
	return testutil.ToFloat64(relaysAddedToSMST.WithLabelValues(f.supplierAddr, "svc-1"))
}

// requireNothingWritten asserts the relay left no session and no tree behind.
func (f *cutFixture) requireNothingWritten(t *testing.T, sessionID string) {
	t.Helper()
	snapshot, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, snapshot, "a dropped relay must not create its session")
	nodes, err := f.redisClient.Exists(f.ctx, f.redisClient.KB().SMSTNodesKey(f.supplierAddr, sessionID)).Result()
	require.NoError(t, err)
	require.Zero(t, nodes, "a dropped relay must not build a tree")
}

// TestHandleRelay_PastTheFlushCapIsDroppedBeforeTouchingRedis: once the claim
// flush has stopped waiting for its session, a relay is dropped at the entry,
// without a single Redis command -- not the session read, not the discovery
// write -- and leaves no session or tree behind.
func TestHandleRelay_PastTheFlushCapIsDroppedBeforeTouchingRedis(t *testing.T) {
	const supplier = "pokt1cut_past_cap"
	f := newCutFixture(t, supplier, 0)
	f.worker.supplierManager.config.BlockClient = &mockBlockClient{currentHeight: f.open + claimFlushCapBlocks}
	// The discovery write is wired as in production, so a Redis command sent
	// before the cut has something real to count.
	f.worker.config.RedisClient = f.redisClient
	f.worker.discovered = xsync.NewMap[string, struct{}]()

	counter := &redisCommandCounter{}
	f.redisClient.AddHook(testredis.ProductCommands(counter))
	openBefore, addedBefore := f.rejected(claimWindowReasonOpen), f.added()

	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, f.message("sess-cut-past-cap")),
		"a relay that can no longer reach a claim is ACKed, not retried")

	require.Zero(t, counter.commands.Load(), "the cut must decide before handleRelay sends anything to Redis")
	require.Equal(t, openBefore+1, f.rejected(claimWindowReasonOpen), "the drop must be announced under claim_window_open")
	require.Equal(t, addedBefore, f.added(), "relays_added_to_smst_total must not move")
	f.requireNothingWritten(t, "sess-cut-past-cap")
}

// TestHandleRelay_InsideTheFlushCapIsAdmitted is the control: one block before
// the cap the flush may still take the relay, so it goes into the tree.
func TestHandleRelay_InsideTheFlushCapIsAdmitted(t *testing.T) {
	const supplier = "pokt1cut_inside_cap"
	f := newCutFixture(t, supplier, 0)
	f.worker.supplierManager.config.BlockClient = &mockBlockClient{currentHeight: f.open + claimFlushCapBlocks - 1}
	openBefore, addedBefore := f.rejected(claimWindowReasonOpen), f.added()

	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, f.message("sess-cut-inside-cap")))

	require.Equal(t, openBefore, f.rejected(claimWindowReasonOpen), "a relay the flush may still take must not be dropped")
	require.Equal(t, addedBefore+1, f.added(), "the relay must reach the tree")
	require.Equal(t, int32(1), f.paramReads.Load(), "deciding past the session end reads the params once")
}

// TestHandleRelay_PastTheCloseKeepsTheClosedReason: a closed claim window is
// also past the flush cap, and the drop keeps the reason the live gate already
// excuses.
func TestHandleRelay_PastTheCloseKeepsTheClosedReason(t *testing.T) {
	const supplier = "pokt1cut_closed"
	f := newCutFixture(t, supplier, 0)
	f.worker.supplierManager.config.BlockClient = &mockBlockClient{currentHeight: f.close}
	openBefore, closedBefore := f.rejected(claimWindowReasonOpen), f.rejected(claimWindowReasonClosed)

	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, f.message("sess-cut-closed")))

	require.Equal(t, closedBefore+1, f.rejected(claimWindowReasonClosed), "past the close the reason is claim_window_closed")
	require.Equal(t, openBefore, f.rejected(claimWindowReasonOpen), "and not claim_window_open")
	f.requireNothingWritten(t, "sess-cut-closed")
}

// TestHandleRelay_UnreadableParamsAdmitAndCount: without the params at the
// session end height the cut cannot tell, so the relay is admitted -- dropping
// on an unknown is how served work stops being paid -- and the blind admission
// is counted.
func TestHandleRelay_UnreadableParamsAdmitAndCount(t *testing.T) {
	const supplier = "pokt1cut_params_fail"
	f := newCutFixture(t, supplier, 0)
	f.worker.supplierManager.config.BlockClient = &mockBlockClient{currentHeight: f.close}
	f.shared.paramsAtHeightFn = func(context.Context, int64) (*sharedtypes.Params, error) {
		return nil, errors.New("injected: params at height unreadable")
	}
	uncheckedBefore := testutil.ToFloat64(claimWindowOpenUnchecked)
	closedBefore, addedBefore := f.rejected(claimWindowReasonClosed), f.added()

	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, f.message("sess-cut-params-fail")))

	require.Equal(t, uncheckedBefore+1, testutil.ToFloat64(claimWindowOpenUnchecked), "the blind admission must be counted")
	require.Equal(t, closedBefore, f.rejected(claimWindowReasonClosed), "an unknown must not drop the relay")
	require.Equal(t, addedBefore+1, f.added(), "the relay must reach the tree")
}

// TestHandleRelay_ASessionNotYetEndedAsksNoParams: a session that has not ended
// cannot be past its claim window, so the cut answers without a params read --
// which would also cache today's params under a future height.
func TestHandleRelay_ASessionNotYetEndedAsksNoParams(t *testing.T) {
	const supplier = "pokt1cut_not_ended"
	f := newCutFixture(t, supplier, cutSessionEnd)
	addedBefore := f.added()

	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, f.message("sess-cut-not-ended")))

	require.Zero(t, f.paramReads.Load(), "a session at or after the observed height must not read params")
	require.Equal(t, addedBefore+1, f.added())
}

// TestHandleRelay_ARedeliveredCopyPastTheCapIsNamedAsSuch: the live gate
// excuses a missing relay only by the reason without the suffix, so a
// redelivered copy dropped by the cut must carry it.
func TestHandleRelay_ARedeliveredCopyPastTheCapIsNamedAsSuch(t *testing.T) {
	const supplier = "pokt1cut_redelivered"
	f := newCutFixture(t, supplier, 0)
	f.worker.supplierManager.config.BlockClient = &mockBlockClient{currentHeight: f.open + claimFlushCapBlocks}
	msg := f.message("sess-cut-redelivered")
	msg.IsReclaim = true
	openBefore := f.rejected(claimWindowReasonOpen)
	redeliveredBefore := f.rejected(dropReason(claimWindowReasonOpen, true))

	require.NoError(t, f.worker.handleRelay(f.ctx, supplier, msg))

	require.Equal(t, redeliveredBefore+1, f.rejected(dropReason(claimWindowReasonOpen, true)))
	require.Equal(t, openBefore, f.rejected(claimWindowReasonOpen), "a redelivered copy must not be counted as a first delivery")
}

// TestTheEntryCutAndTheClaimFlushAgreeOnTheCap: the flush waits for relays up to
// claimFlushCapBlocks past the window opening, and the entry admits them for
// exactly as long. At each height around the cap, "the flush has stopped
// waiting" and "the entry drops" must be the same answer: if one side moves its
// cap, relays the flush would still take are dropped, or relays it no longer
// waits for are admitted.
func TestTheEntryCutAndTheClaimFlushAgreeOnTheCap(t *testing.T) {
	shared := &mockSharedQueryClient{}
	params, err := shared.GetParams(context.Background())
	require.NoError(t, err)
	open := sharedtypes.GetClaimWindowOpenHeight(params, cutSessionEnd)

	for _, height := range []int64{open, open + claimFlushCapBlocks - 1, open + claimFlushCapBlocks, open + claimFlushCapBlocks + 1} {
		bc := &mockBlockClient{currentHeight: height}

		// A cancelled context ends the wait at its first poll, so a flush still
		// waiting answers false and one past its cap answers true without polling.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		flush := newFlushDelayManager(bc,
			func() (streamMsgID, bool) { return streamMsgID{}, false },
			func(context.Context) (streamMsgID, bool, error) { return streamMsgID{ms: 999999999}, true, nil },
		)
		flushStoppedWaiting := flush.awaitFlushWatermark(ctx, sessionsWithServiceID("svc-agree"), open)

		entry := &SupplierManager{config: SupplierManagerConfig{BlockClient: bc, SharedClient: shared}}
		entryDrops := entry.claimWindowReached(context.Background(), cutSessionEnd) != ""

		require.Equal(t, flushStoppedWaiting, entryDrops,
			"at height %d (window open %d): the flush stopped waiting = %v, the entry drops = %v",
			height, open, flushStoppedWaiting, entryDrops)
	}
}
