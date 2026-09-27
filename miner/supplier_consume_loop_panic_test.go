//go:build test

package miner

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// consumeForSupplier had one recover, deferred at the top: a panic that
// reached the loop ended the goroutine, and the supplier stayed in the map
// with its lease renewed and nothing reading its stream, until a restart.
// No path reaches that recover today; consumeLoopFlushHook makes the loop
// itself panic, at a flush tick, before the flush.

type panicLoopFixture struct {
	w         *batchWorker
	hookCalls atomic.Int32
	processed chan string
	loopDone  chan struct{}
}

func newPanicLoopFixture(t *testing.T, supplier string, hook func(call int32)) *panicLoopFixture {
	t.Helper()
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, supplier, "a")
	w.mgr.config.RedisClient = client
	w.mgr.config.MinerID = "instance-" + supplier
	w.mgr.config.RelayBatchFlushInterval = time.Millisecond
	f := &panicLoopFixture{w: w, processed: make(chan string, 64), loopDone: make(chan struct{})}
	relay := w.mgr.onRelay
	w.mgr.onRelay = func(ctx context.Context, addr string, msg *transport.StreamMessage) error {
		id := msg.ID
		err := relay(ctx, addr, msg)
		select {
		case f.processed <- id:
		default:
		}
		return err
	}
	w.mgr.consumeLoopFlushHook = func() { hook(f.hookCalls.Add(1)) }
	return f
}

// withClaimer gives the manager a claimer holding the supplier's lease, as
// after Start, and returns the lease key.
func (f *panicLoopFixture) withClaimer(t *testing.T) string {
	t.Helper()
	claimer := NewSupplierClaimer(zerolog.Nop(), f.w.client, f.w.mgr.config.MinerID, SupplierClaimerConfig{})
	claimer.SetCallbacks(func(context.Context, string) error { return nil }, f.w.mgr.onSupplierReleased)
	claimer.allSuppliers = []string{f.w.supplier}
	claimerCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	claimer.ctx, claimer.cancelFn = claimerCtx, stop
	require.True(t, claimer.TryClaim(f.w.ctx, f.w.supplier), "premise: this instance holds the lease")
	f.w.mgr.claimer = claimer
	return f.w.client.KB().MinerClaimKey(f.w.supplier)
}

// start runs the supplier's real consume loop, as addSupplierWithData does.
func (f *panicLoopFixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(f.w.ctx)
	f.w.state.cancelFn = cancel
	f.w.state.wg.Add(1)
	go func() {
		f.w.mgr.consumeForSupplier(ctx, f.w.state)
		close(f.loopDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.loopDone:
			f.w.mgr.waitDrains()
		case <-time.After(30 * time.Second):
			// A bound on failure, not a synchronisation: a loop stuck for
			// good must fail this test, not hang the package until its
			// timeout.
			t.Error("the consume loop never returned")
		}
	})
}

func (f *panicLoopFixture) waitLoopDone(t *testing.T, what string) {
	t.Helper()
	select {
	case <-f.loopDone:
	case <-time.After(10 * time.Second):
		t.Fatal(what)
	}
}

// addRelay appends a relay the consumer can parse and process, and returns
// its stream ID.
func (f *panicLoopFixture) addRelay(t *testing.T, sessionID, payload string) string {
	t.Helper()
	buf, err := newStreamMessage(f.w.supplier, sessionID, payload, 100).Message.Marshal()
	require.NoError(t, err)
	id, err := f.w.client.XAdd(f.w.ctx, &redis.XAddArgs{
		Stream: f.w.stream, Values: map[string]any{"data": string(buf)},
	}).Result()
	require.NoError(t, err)
	return id
}

// batchedTestRelay is one relay batchRelays put in, kept around so a test can
// rebuild the exact same message later (e.g. to redeliver it as a reclaim).
type batchedTestRelay struct {
	id, payload string
}

// batchRelays puts n relays in the supplier's batch the way its loop would:
// read under the consumer's name, processed, held unacknowledged.
func (f *panicLoopFixture) batchRelays(t *testing.T, sessionID string, n int) []batchedTestRelay {
	t.Helper()
	relays := make([]batchedTestRelay, 0, n)
	for i := 0; i < n; i++ {
		payload := sessionID + "-batched-" + string(rune('a'+i))
		id := f.addRelay(t, sessionID, payload)
		read, err := f.w.client.XReadGroup(f.w.ctx, &redis.XReadGroupArgs{
			Group: f.w.group, Consumer: f.w.consumerName, Streams: []string{f.w.stream, ">"}, Count: 1,
		}).Result()
		require.NoError(t, err)
		require.Equal(t, id, read[0].Messages[0].ID)
		f.w.deliver(f.w.msg(id, sessionID, payload, 100))
		relays = append(relays, batchedTestRelay{id: id, payload: payload})
	}
	require.Equal(t, n, f.w.held(sessionID), "premise: the batch holds them")
	return relays
}

func loopPanics() float64 {
	return testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("supplier_consume_loop"))
}

// TestConsumeLoop_APanicHandsTheBatchBackAndTheLoopRunsAgain: one panic, with
// two relays in the batch.
//
// Item 263: releasing an entry (XNACK) leaves it idle=-1 in Redis, which both
// XPENDING's IDLE filter and XCLAIM's MinIdle treat as "always eligible" --
// ClaimIdleTimeout gives no protection against ANY consumer, including this
// same one, reclaiming it right back on its next sweep (measured with
// redis-cli against a bare server: XPENDING ... IDLE 60000 still returns an
// entry released a millisecond ago). So "pending == 0 at the exact instant
// the loop restarts" is not a promise this design makes -- ReleaseMessage
// says so explicitly (visible "to every consumer including the one that let
// go"), and ReleaseAll's own contract is only that each entry is counted
// once. This test fixes two things instead, neither tied to that instant:
// (a) the interrupted batch was actually handed back, and (b) each of its
// relays is still counted exactly once even when a same-instance reclaim
// redelivers it.
//
// (a) here only checks the in-memory side (held(sessionID) == 0, read from a
// hook that runs on the one instant nothing else can have added back to it --
// see afterReleaseAfterPanicHook). The Redis side of the same release --
// ReleaseMessage actually reaching XNACK, not merely returning nil -- is
// TestExitCheckpoint_TheNextOwnerResumesEveryBatchedRelay
// (relay_batch_exit_checkpoint_test.go), which runs no live consume loop and
// so has no reclaim race to confound an ownership check.
func TestConsumeLoop_APanicHandsTheBatchBackAndTheLoopRunsAgain(t *testing.T) {
	const supplier, sessionID = "pokt1loop_panic_once", "sess-loop-panic-once"
	restarted := make(chan struct{})
	handedBack := make(chan bool, 1)
	f := newPanicLoopFixture(t, supplier, func(call int32) {
		switch call {
		case 1:
			panic("injected consume-loop panic")
		case 2: // the loop is running again: the batch went back in between
			close(restarted)
		}
	})
	// afterReleaseAfterPanicHook runs on the SAME goroutine as the panic and
	// the release, strictly between releaseBatchAfterPanic returning and
	// runConsumeLoop being called again -- the only reader of msgChan, and the
	// only thing that can call relayBatch.Add, is that same goroutine, and it
	// is blocked inside this call. So nothing (not deliverOwnPending, not the
	// reclaimLoop's own sweep) can have added anything back to the in-memory
	// batch by the time this runs: held(sessionID) here reads exactly what the
	// release left, with no race to argue about (item 263 -- a Redis PEL
	// heuristic here, like RetryCount, is NOT race-free: the reclaimLoop's
	// sweep and deliverOwnPending run on their own goroutines and can reorder
	// around the release in ways that defeat any fixed threshold).
	f.w.mgr.afterReleaseAfterPanicHook = func() {
		select {
		case handedBack <- f.w.held(sessionID) == 0:
		default:
		}
	}
	batched := f.batchRelays(t, sessionID, 2)
	before := loopPanics()

	f.start(t)

	// (a) Handed back: read directly off the hook, not a poll -- there is
	// exactly one instant this can be checked without a race, and the hook
	// runs on it.
	select {
	case ok := <-handedBack:
		require.True(t, ok, "the interrupted batch is still held in-memory right after the release attempt returned: releaseBatchAfterPanic did not hand it back")
	case <-time.After(10 * time.Second):
		t.Fatal("the panic never reached the release attempt")
	}
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not run again after the panic")
	}

	// (b) Counted exactly once, forced deterministically instead of racing the
	// real same-instance reclaim (item 263 measured it at ~3% of runs, too rare
	// to exercise a teeth reliably): redeliver both relays of the interrupted
	// batch exactly as deliverOwnPending and the reclaimLoop's own sweep both
	// do it (IsReclaim=true -- verified in the source, consumer.go:666,799:
	// both paths set it, so there is only this one shape of second copy to
	// prove safe, not a separate "non-reclaim" one).
	//
	// This has to happen in two rounds, not one: the FIRST forced copy is
	// itself the first time these two relays are ever fully processed (the
	// panic interrupted them before their first flush), so it proves nothing
	// about duplicate protection by itself -- a fresh SADD member has nothing
	// to reject. Only a SECOND copy, landing on a session whose count already
	// includes them, tests that.
	drained := map[string]int{}
	drainProcessed := func() {
		for {
			select {
			case id := <-f.processed:
				drained[id]++
			default:
				return
			}
		}
	}
	redeliver := func(r batchedTestRelay) {
		msg := f.w.msg(r.id, sessionID, r.payload, 100)
		msg.IsReclaim = true
		f.w.deliver(msg)
	}
	waitFor := func(want map[string]int, what string) {
		t.Helper()
		ok := func() bool {
			for id, n := range want {
				if drained[id] < n {
					return false
				}
			}
			return true
		}
		for !ok() {
			select {
			case id := <-f.processed:
				drained[id]++
			case <-time.After(10 * time.Second):
				t.Fatal(what)
			}
		}
	}

	drainProcessed() // absorb whatever already happened before this point
	for _, r := range batched {
		redeliver(r)
	}
	fresh := f.addRelay(t, sessionID, "after-the-panic")
	waitFor(map[string]int{batched[0].id: 1, batched[1].id: 1, fresh: 1},
		"the first (reclaim-shaped) redelivery of the interrupted batch, or the fresh relay, was never consumed")
	f.w.batch.FlushAll(f.w.ctx) // settle deterministically instead of waiting on the flush ticker

	snap, err := f.w.store.Get(f.w.ctx, sessionID)
	require.NoError(t, err)
	require.EqualValues(t, 3, snap.RelayCount, "2 batched + 1 fresh, first count")

	// Now land a SECOND copy of the same two relays on a session that already
	// counted them -- this is the one that actually exercises the SADD-based
	// dedup gate (relayBatchScript, the real correctness gate; the per-relay
	// IsDuplicate check upstream is only an optimisation, per its own comment).
	for _, r := range batched {
		redeliver(r)
	}
	waitFor(map[string]int{batched[0].id: 2, batched[1].id: 2},
		"the second (duplicate) redelivery of the interrupted batch was never consumed")
	f.w.batch.FlushAll(f.w.ctx)

	snap, err = f.w.store.Get(f.w.ctx, sessionID)
	require.NoError(t, err)
	require.EqualValues(t, 3, snap.RelayCount,
		"a relay already counted must not be counted again when a same-instance reclaim redelivers it")

	require.Equal(t, before+1, loopPanics(), "each panic is counted")
	require.Contains(t, f.w.logs.String(), "PANIC RECOVERED in the consume loop")
	_, stillThere := f.w.mgr.GetSupplierState(supplier)
	require.True(t, stillThere, "one panic does not give the supplier up")
}

// TestConsumeLoop_ThePanicAtTheBudgetLetsTheSupplierGo: the loop panics at
// every flush tick. At the budget's panic the lease goes, for another
// instance to take.
func TestConsumeLoop_ThePanicAtTheBudgetLetsTheSupplierGo(t *testing.T) {
	const supplier = "pokt1loop_panic_budget"
	f := newPanicLoopFixture(t, supplier, func(int32) { panic("injected consume-loop panic") })
	claimKey := f.withClaimer(t)
	decision := supplierDrainDecisionTotal.WithLabelValues(triggerConsumeLoopPanicked, "no_query_client")
	decisionsBefore := testutil.ToFloat64(decision)
	before := loopPanics()

	f.start(t)
	f.waitLoopDone(t, "the loop keeps running again: the supplier is never let go")
	f.w.mgr.waitDrains()

	require.Equal(t, int32(consumeLoopPanicBudget), f.hookCalls.Load(),
		"the supplier is let go at the budget's panic, not before and not after")
	require.Equal(t, before+consumeLoopPanicBudget, loopPanics())
	exists, err := f.w.client.Exists(f.w.ctx, claimKey).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "the drain is over and the lease is gone, for another instance to take")
	_, stillThere := f.w.mgr.GetSupplierState(supplier)
	require.False(t, stillThere, "the supplier left this instance")
	require.Equal(t, decisionsBefore+1, testutil.ToFloat64(decision), "the drain names why it happened")
}

// TestConsumeLoop_WithNoClaimerThePanicAtTheBudgetStillTearsItDown: no lease
// to release, but the supplier must not stay with nothing consuming.
func TestConsumeLoop_WithNoClaimerThePanicAtTheBudgetStillTearsItDown(t *testing.T) {
	const supplier = "pokt1loop_panic_noclaimer"
	f := newPanicLoopFixture(t, supplier, func(int32) { panic("injected consume-loop panic") })
	decision := supplierDrainDecisionTotal.WithLabelValues(triggerConsumeLoopPanicked, "no_query_client")
	decisionsBefore := testutil.ToFloat64(decision)

	f.start(t)
	// The let-go runs on the loop's own goroutine; a teardown that waits for
	// it there never returns.
	select {
	case <-f.loopDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the let-go deadlocked: the teardown waits on the goroutine that started it")
	}
	f.w.mgr.waitDrains()

	_, stillThere := f.w.mgr.GetSupplierState(supplier)
	require.False(t, stillThere, "with no claimer the supplier is torn down all the same")
	require.Equal(t, decisionsBefore+1, testutil.ToFloat64(decision))
}

// TestConsumeLoop_ARelaysOwnPanicSpendsNoBudget: a relay's panic is the
// relay's, recovered where it happens. The loop panics only after it, and is
// let go at its own budget's panic.
func TestConsumeLoop_ARelaysOwnPanicSpendsNoBudget(t *testing.T) {
	const supplier, sessionID = "pokt1loop_panic_relay", "sess-loop-panic-relay"
	var relayPanicked atomic.Bool
	var loopPanicsSeen atomic.Int32
	f := newPanicLoopFixture(t, supplier, func(int32) {
		if relayPanicked.Load() {
			loopPanicsSeen.Add(1)
			panic("injected consume-loop panic")
		}
	})
	relay := f.w.mgr.onRelay
	f.w.mgr.onRelay = func(ctx context.Context, addr string, msg *transport.StreamMessage) error {
		if relayPanicked.CompareAndSwap(false, true) {
			panic("injected relay panic")
		}
		return relay(ctx, addr, msg)
	}
	f.addRelay(t, sessionID, "the-relay-that-panics")

	f.start(t)
	f.waitLoopDone(t, "the loop keeps running again: the supplier is never let go")

	require.True(t, relayPanicked.Load(), "premise: the relay panicked")
	require.Equal(t, int32(consumeLoopPanicBudget), loopPanicsSeen.Load(),
		"a relay's own panic must not spend the loop's budget")
}

// TestConsumeLoop_APanicHandingTheBatchBackLetsTheSupplierGoAtOnce: the batch
// cannot go back without panicking again; running the loop over it would only
// repeat that, so the supplier is let go at the first panic.
func TestConsumeLoop_APanicHandingTheBatchBackLetsTheSupplierGoAtOnce(t *testing.T) {
	const supplier = "pokt1loop_panic_release"
	var f *panicLoopFixture
	f = newPanicLoopFixture(t, supplier, func(int32) {
		f.w.batch.mu.Lock()
		f.w.batch.sessions["poisoned"] = nil // releasing a nil session panics
		f.w.batch.mu.Unlock()
		panic("injected consume-loop panic")
	})
	before := loopPanics()

	f.start(t)
	f.waitLoopDone(t, "the loop keeps running again: the supplier is never let go")
	f.w.mgr.waitDrains()

	require.Equal(t, int32(1), f.hookCalls.Load(), "let go at the first panic, without running the loop again")
	require.Equal(t, before+2, loopPanics(), "the loop's panic and the release's")
	require.Equal(t, 1, strings.Count(f.w.logs.String(), "PANIC RECOVERED handing the relay batch back"))
	_, stillThere := f.w.mgr.GetSupplierState(supplier)
	require.False(t, stillThere)
}
