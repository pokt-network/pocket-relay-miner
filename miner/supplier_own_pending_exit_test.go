//go:build test

package miner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// The consume loop's exit settles the batch and the delivery buffer. What is
// pending under the consumer's name outside them -- the relay being processed
// when the supplier was cancelled, the rest of a read batch the consumer was
// handing over -- stayed there. The name is this process's, so only this
// process taking the supplier again could read it.

// strandUnderTheName delivers n more relays to the fixture's consumer and leaves
// them in neither the batch nor the buffer. They carry a real relay: the pass
// reads them from the stream, and one it cannot parse it deletes as malformed.
func (f *keyRemovalFixture) strandUnderTheName(t *testing.T, n int) []string {
	t.Helper()
	buf, err := (&transport.MinedRelayMessage{
		SessionId: "sess-key-removal", SupplierOperatorAddress: f.w.supplier, ServiceId: "svc-1",
	}).Marshal()
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		require.NoError(t, f.w.client.XAdd(f.w.ctx, &redis.XAddArgs{
			Stream: f.w.stream, Values: map[string]any{"data": string(buf)},
		}).Err())
	}
	read, err := f.w.client.XReadGroup(f.w.ctx, &redis.XReadGroupArgs{
		Group: f.w.group, Consumer: f.w.consumerName, Streams: []string{f.w.stream, ">"}, Count: int64(n),
	}).Result()
	require.NoError(t, err)
	require.Len(t, read[0].Messages, n)
	ids := make([]string, n)
	for i, m := range read[0].Messages {
		ids[i] = m.ID
	}
	f.total += n
	require.Equal(t, int64(f.total), f.w.pending(), "premise: all of them are pending")
	return ids
}

func (f *keyRemovalFixture) pendingUnderTheName(t *testing.T) int {
	t.Helper()
	pending, err := f.w.client.XPendingExt(f.w.ctx, &redis.XPendingExtArgs{
		Stream: f.w.stream, Group: f.w.group, Start: "-", End: "+", Count: 100, Consumer: f.w.consumerName,
	}).Result()
	require.NoError(t, err)
	return len(pending)
}

// TestTeardown_ARebalanceHandsBackWhatIsLeftUnderTheConsumersName: a peer
// must be able to finish all six, the two stranded ones included.
func TestTeardown_ARebalanceHandsBackWhatIsLeftUnderTheConsumersName(t *testing.T) {
	f := newKeyRemovalFixture(t, "pokt1ownpending_rebalance")
	f.strandUnderTheName(t, 2)

	f.release(t, triggerRebalanceRelease)

	require.Zero(t, f.pendingUnderTheName(t), "nothing may stay under a name only this process reads")
	f.requireReleasedToPeer(t)
}

// TestTeardown_AKeyRemovalAcksWhatIsLeftUnderTheConsumersNameAsLost: nobody in
// the fleet can sign them, so they are acknowledged and counted, the way the
// buffer's are.
func TestTeardown_AKeyRemovalAcksWhatIsLeftUnderTheConsumersNameAsLost(t *testing.T) {
	f := newKeyRemovalFixture(t, "pokt1ownpending_keyremoval")
	f.strandUnderTheName(t, 2)
	noKey := relaysDroppedNoKey.WithLabelValues(f.w.supplier, "svc-1")
	before := testutil.ToFloat64(noKey)

	f.release(t, triggerKeyRemoval)

	require.Zero(t, f.w.pending(), "a removed key: acknowledged, not released to a fleet that cannot sign them")
	require.Zero(t, f.w.streamLen(), "and never delivered again")
	require.Equal(t, before+float64(f.total), testutil.ToFloat64(noKey), "each one counted as dropped for want of a key")
}

// TestClose_HandsBackWhatIsLeftUnderTheConsumersName: the shutdown collects
// its suppliers outside teardownSupplier, so it needs the same pass.
func TestClose_HandsBackWhatIsLeftUnderTheConsumersName(t *testing.T) {
	f := newKeyRemovalFixture(t, "pokt1ownpending_close")
	f.strandUnderTheName(t, 2)

	require.NoError(t, f.w.mgr.Close())

	require.Zero(t, f.pendingUnderTheName(t), "the shutdown left entries under a name that dies with the process")
	f.requireReleasedToPeer(t)
}

// refuseReleaseOf fails the XNACK of the given entries, so ReleaseMessage fails
// for them.
type refuseReleaseOf map[string]bool

func (refuseReleaseOf) DialHook(next redis.DialHook) redis.DialHook { return next }

func (refuseReleaseOf) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h refuseReleaseOf) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "xnack" {
			for _, arg := range cmd.Args() {
				if id, ok := arg.(string); ok && h[id] {
					err := errors.New("injected: release refused")
					cmd.SetErr(err)
					return err
				}
			}
		}
		return next(ctx, cmd)
	}
}

// TestTeardown_AnEntryItCannotHandBackIsCountedAndReported: the two stranded
// entries cannot be released; the batch and the buffer can.
func TestTeardown_AnEntryItCannotHandBackIsCountedAndReported(t *testing.T) {
	const supplier = "pokt1ownpending_refused"
	f := newKeyRemovalFixture(t, supplier)
	refused := refuseReleaseOf{}
	for _, id := range f.strandUnderTheName(t, 2) {
		refused[id] = true
	}
	f.w.client.AddHook(refused)
	abandoned := shutdownAbandonedRelays.WithLabelValues(supplier)
	before := testutil.ToFloat64(abandoned)

	f.release(t, triggerRebalanceRelease)

	require.Equal(t, 2, f.pendingUnderTheName(t), "premise: the two could not be handed back")
	require.Equal(t, before+2, testutil.ToFloat64(abandoned), "what the pass could not settle is counted")
	require.Contains(t, f.w.logs.String(), "could not settle every entry left under this consumer's name")
}

// holdNewReadsUntilThePass holds the consumer's reads for new entries (">") on
// stream until the second read of its own pending list has completed -- the
// first is the consumer's own at start, the second the teardown's pass -- and
// then lets them out. A read whose context ends first is dropped, unless the
// pass has completed by then.
type holdNewReadsUntilThePass struct {
	stream   string
	ownReads atomic.Int32
	reached  chan struct{} // closed when the read loop first waits to read new entries
	passDone chan struct{}
	onceHeld sync.Once
	once     sync.Once
}

func (h *holdNewReadsUntilThePass) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *holdNewReadsUntilThePass) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *holdNewReadsUntilThePass) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() != "xreadgroup" || len(args) < 2 || args[len(args)-2] != h.stream {
			return next(ctx, cmd)
		}
		if args[len(args)-1] == ">" {
			h.onceHeld.Do(func() { close(h.reached) })
			// Once the pass is done the read goes out, whatever the context:
			// without the Stop, Close's cancel lands just after the pass, and
			// letting it win would hide the read the test is about.
			select {
			case <-h.passDone:
				return next(context.WithoutCancel(ctx), cmd)
			case <-ctx.Done():
				select {
				case <-h.passDone:
					return next(context.WithoutCancel(ctx), cmd)
				default:
					cmd.SetErr(ctx.Err())
					return ctx.Err()
				}
			}
		}
		err := next(ctx, cmd)
		if h.ownReads.Add(1) == 2 {
			h.once.Do(func() { close(h.passDone) })
		}
		return err
	}
}

// TestTeardown_StopsTheConsumerBeforeItsPass: a relay reaches the stream while
// the consumer's read loop is still running. Read after the pass, it would be
// left under the name; stopped first, the loop never reads it.
func TestTeardown_StopsTheConsumerBeforeItsPass(t *testing.T) {
	f := newDrainLeaseFixture(t, "pokt1ownpending_stop_first")
	hold := &holdNewReadsUntilThePass{stream: f.w.stream, reached: make(chan struct{}), passDone: make(chan struct{})}
	f.w.client.AddHook(hold)
	f.w.consumer.Consume(f.w.ctx)
	select {
	case <-hold.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("premise: the read loop never came to read new entries")
	}
	require.NoError(t, f.w.client.XAdd(f.w.ctx, &redis.XAddArgs{
		Stream: f.w.stream, Values: map[string]any{"data": "x"},
	}).Err())

	require.NoError(t, f.claimer.Release(f.w.ctx, f.w.supplier, triggerRebalanceRelease))
	close(f.hold)
	f.w.mgr.waitDrains()

	require.Zero(t, f.w.pending(), "a read after the pass left an entry under a name only this process reads")
}
