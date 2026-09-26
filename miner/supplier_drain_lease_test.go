//go:build test

package miner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// A released supplier's lease used to be deleted as soon as Release ran, while
// its drain -- the old consume loop releasing its batch and delivery buffer,
// then the teardown -- was still going. A peer could claim the supplier in that
// window and write its tree and acknowledge its relays next to the old loop.
// The lease now stays this instance's until the drain ends, and nothing on this
// instance takes the supplier back meanwhile.

// ---------------------------------------------------------------------------
// Claimer only.
// ---------------------------------------------------------------------------

func (f *leaseFixture) owner(t *testing.T) string {
	t.Helper()
	owner, err := f.client.Get(context.Background(), f.claimKey).Result()
	if errors.Is(err, redis.Nil) {
		return ""
	}
	require.NoError(t, err)
	return owner
}

func (f *leaseFixture) peer() *SupplierClaimer {
	return NewSupplierClaimer(zerolog.Nop(), f.client, "peer-instance", SupplierClaimerConfig{})
}

// TestRelease_KeepsTheLeaseUntilFinishRelease is the claimer's half of the
// contract, and the control for everything below: FinishRelease is what frees
// the supplier.
func TestRelease_KeepsTheLeaseUntilFinishRelease(t *testing.T) {
	const instance = "instance-keeps-lease"
	f := newLeaseFixture(t, instance)
	ctx := context.Background()
	peer := f.peer()

	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))

	require.False(t, f.claimer.IsClaimed(f.supplier))
	require.Equal(t, instance, f.owner(t), "released, but the lease is ours until the drain ends")
	require.False(t, peer.TryClaim(ctx, f.supplier), "no peer takes a supplier whose old owner may still write")

	require.NoError(t, f.claimer.FinishRelease(ctx, f.supplier))

	require.Empty(t, f.owner(t), "the drain is over: the lease goes")
	require.True(t, peer.TryClaim(ctx, f.supplier), "and the peer can take it")
}

// TestTryClaim_RefusesADrainingSupplierWhoseKeyIsGone: the SETNX branch. The
// key vanished (expired, deleted) while the drain still runs.
func TestTryClaim_RefusesADrainingSupplierWhoseKeyIsGone(t *testing.T) {
	f := newLeaseFixture(t, "instance-draining-setnx")
	ctx := context.Background()
	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
	claims := countClaims(f)
	require.NoError(t, f.client.Del(ctx, f.claimKey).Err())

	require.False(t, f.claimer.TryClaim(ctx, f.supplier),
		"still draining: taken back now, a new consume loop would start beside the old one")
	require.Zero(t, claims.Load(), "and nothing is started")
	require.False(t, f.keyExists(t), "nor the key taken")
}

// TestTryClaim_RefusesADrainingSupplierWhoseKeyIsStillOurs: the "already ours"
// branch, which used to answer true with nothing in the map and no callback --
// a supplier counted claimed that nothing runs.
func TestTryClaim_RefusesADrainingSupplierWhoseKeyIsStillOurs(t *testing.T) {
	f := newLeaseFixture(t, "instance-draining-ours")
	ctx := context.Background()
	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
	claims := countClaims(f)

	require.False(t, f.claimer.TryClaim(ctx, f.supplier))
	f.claimer.claimMore(1)

	require.False(t, f.claimer.IsClaimed(f.supplier))
	require.Zero(t, claims.Load())
}

// TestTryClaim_ARenewalThatMeetsAReleaseDoesNotCountIt: the release lands after
// TryClaim's first check, so only the check inside "already ours" sees it.
func TestTryClaim_ARenewalThatMeetsAReleaseDoesNotCountIt(t *testing.T) {
	f := newLeaseFixture(t, "instance-renewal-meets-release")
	ctx := context.Background()
	f.client.AddHook(&beforeCmd{name: "get", key: f.claimKey, action: func() {
		require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
	}})

	require.False(t, f.claimer.TryClaim(ctx, f.supplier),
		"released between the SETNX and the GET: the key is ours only because its drain holds it")
}

// TestReleaseLost_OfADrainingSupplierIsANoOp: the release got there first and
// its drain owns the teardown. A second one would count a hand-over as a lost
// lease.
func TestReleaseLost_OfADrainingSupplierIsANoOp(t *testing.T) {
	const instance = "instance-lost-while-draining"
	f := newLeaseFixture(t, instance)
	ctx := context.Background()
	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
	lost := supplierLeaseLostTotal.WithLabelValues(triggerLeaseStolen, instance)
	before := testutil.ToFloat64(lost)

	f.claimer.releaseLost(ctx, f.supplier, triggerLeaseStolen)

	require.Equal(t, []string{f.supplier}, f.released, "one drain, the release's")
	require.Equal(t, before, testutil.ToFloat64(lost))
}

// TestReleaseLost_KeepsTheSupplierUntilItsDrainFinishes: a lost lease starts a
// drain too, and a re-take must wait for it like one after a release.
func TestReleaseLost_KeepsTheSupplierUntilItsDrainFinishes(t *testing.T) {
	f := newLeaseFixture(t, "instance-lost-then-retake")
	ctx := context.Background()
	f.setPeerOwner(t)
	f.claimer.renewAllClaims() // the peer holds it: lease lost, drain started
	require.Equal(t, []string{triggerLeaseStolen}, f.triggers, "premise: the lease was reported lost")
	require.NoError(t, f.client.Del(ctx, f.claimKey).Err()) // the peer's lease then expires

	require.False(t, f.claimer.TryClaim(ctx, f.supplier), "its drain is still running")

	require.NoError(t, f.claimer.FinishRelease(ctx, f.supplier))
	require.True(t, f.claimer.TryClaim(ctx, f.supplier), "the drain is over")
}

// TestRelease_OfADrainingSupplierLeavesItToTheDrainUnderWay: a second release
// would start a second drain, find nothing to tear down, and delete the lease
// while the first one still runs.
func TestRelease_OfADrainingSupplierLeavesItToTheDrainUnderWay(t *testing.T) {
	const instance = "instance-double-release"
	f := newLeaseFixture(t, instance)
	ctx := context.Background()

	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerKeyRemoval))

	require.Equal(t, []string{f.supplier}, f.released, "one drain")
	require.Equal(t, instance, f.owner(t))
}

// TestRelease_AFailedCallbackUndoesTheRelease: the supplier is back in the map,
// out of draining, and its lease untouched.
func TestRelease_AFailedCallbackUndoesTheRelease(t *testing.T) {
	const instance = "instance-release-undone"
	f := newLeaseFixture(t, instance)
	ctx := context.Background()
	f.claimer.SetCallbacks(
		func(context.Context, string) error { return nil },
		func(context.Context, string, string) error { return errors.New("injected: manager closing") },
	)

	require.Error(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))

	require.True(t, f.claimer.IsClaimed(f.supplier), "the claim is kept, renewed as before")
	require.False(t, f.claimer.isDraining(f.supplier), "and not left draining, which nothing would ever finish")
	require.Equal(t, instance, f.owner(t))
}

// TestFinishRelease_CountsADrainThatOutranItsBudget: past the budget the key
// may have expired and a peer taken the supplier while the old loop wrote.
func TestFinishRelease_CountsADrainThatOutranItsBudget(t *testing.T) {
	const instance, budget = "instance-overrun", 30 * time.Second
	overrun := supplierDrainLeaseOverrunTotal.WithLabelValues(triggerRebalanceRelease, instance)

	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		counted float64
	}{
		{"within the budget", budget, 0},
		{"past the budget", budget + time.Second, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLeaseFixture(t, instance)
			ctx := context.Background()
			now := time.Now()
			f.claimer.nowFn = func() time.Time { return now } // loops not started: see nowFn
			before := testutil.ToFloat64(overrun)

			require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
			require.NoError(t, f.claimer.ExtendDrainLease(ctx, f.supplier, budget))
			ttl, err := f.client.PTTL(ctx, f.claimKey).Result()
			require.NoError(t, err)
			require.True(t, ttl > 0 && ttl <= budget, "the lease lives the drain budget, not the claim TTL: %v", ttl)

			now = now.Add(tc.elapsed)
			require.NoError(t, f.claimer.FinishRelease(ctx, f.supplier))

			require.Equal(t, before+tc.counted, testutil.ToFloat64(overrun), "a drain past its budget is counted, one within it is not")
		})
	}
}

// failScript makes every script run on key -- EVALSHA, or the EVAL go-redis
// falls back to -- fail with err while it is armed. KEYS[1] sits at argument 3.
type failScript struct {
	key string
	err atomic.Pointer[error]
}

func (h *failScript) arm(err error) { h.err.Store(&err) }
func (h *failScript) disarm()       { h.err.Store(nil) }

func (h *failScript) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failScript) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if e := h.err.Load(); e != nil && (cmd.Name() == "evalsha" || cmd.Name() == "eval") &&
			len(args) >= 4 && args[3] == h.key {
			cmd.SetErr(*e)
			return *e
		}
		return next(ctx, cmd)
	}
}

func (h *failScript) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestTryClaim_ALeaseLeftOursIsClaimedTheWholeWay: a release whose final delete
// failed leaves the key ours with nothing running the supplier. The "already
// ours" branch answered true to that -- a claim counted that nothing started,
// and never renewed, since it is not in the map.
func TestTryClaim_ALeaseLeftOursIsClaimedTheWholeWay(t *testing.T) {
	const instance = "instance-leftover-lease"
	f := newLeaseFixture(t, instance)
	ctx := context.Background()
	claims := countClaims(f)
	f.leaveLeaseOurs(t, instance)

	require.True(t, f.claimer.TryClaim(ctx, f.supplier))

	require.True(t, f.claimer.IsClaimed(f.supplier), "taken the whole way: in the map, so renewed from now on")
	require.Equal(t, int32(1), claims.Load(), "and started")
}

// leaveLeaseOurs releases the supplier and fails the delete that ends the
// release: the key stays this instance's with nothing running the supplier.
func (f *leaseFixture) leaveLeaseOurs(t *testing.T, instance string) {
	t.Helper()
	ctx := context.Background()
	lostDelete := &failScript{key: f.claimKey}
	f.client.AddHook(lostDelete)
	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
	lostDelete.arm(errors.New("injected: the delete never reached Redis"))
	require.Error(t, f.claimer.FinishRelease(ctx, f.supplier))
	lostDelete.disarm()
	require.Equal(t, instance, f.owner(t), "premise: the lease is still ours")
	require.False(t, f.claimer.isDraining(f.supplier), "premise: the drain is over")
}

// TestTryClaim_TwoCallersOnALeaseLeftOursStartItOnce: both reach the "already
// ours" branch of the same leftover lease, and only one may start the supplier.
// The first is held at the log line it writes on the way to starting it; the
// second runs its whole TryClaim meanwhile. Checked and inserted under one lock,
// the first has already taken the supplier when it logs, and the second only
// renews it. Checked first and inserted after the log, both would start it.
func TestTryClaim_TwoCallersOnALeaseLeftOursStartItOnce(t *testing.T) {
	const instance = "instance-leftover-race"
	f := newLeaseFixture(t, instance)
	ctx := context.Background()
	claims := countClaims(f)
	f.leaveLeaseOurs(t, instance)
	claimedTotal := supplierClaimedTotal.WithLabelValues(f.supplier, instance)
	before := testutil.ToFloat64(claimedTotal)

	firstLogged, secondDone := make(chan struct{}), make(chan struct{})
	var arrivals atomic.Int32
	f.claimer.logger = zerolog.New(io.Discard).Hook(zerolog.HookFunc(func(_ *zerolog.Event, _ zerolog.Level, msg string) {
		if msg == "lease already held by this instance with nothing running it; claiming it" && arrivals.Add(1) == 1 {
			close(firstLogged)
			<-secondDone
		}
	}))

	var firstTook bool
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		firstTook = f.claimer.TryClaim(ctx, f.supplier)
	}()
	<-firstLogged
	secondTook := f.claimer.TryClaim(ctx, f.supplier)
	close(secondDone)
	<-firstDone

	require.Equal(t, int32(1), claims.Load(), "two callers on a lease left ours started the supplier twice")
	require.Equal(t, before+1, testutil.ToFloat64(claimedTotal), "and counted it twice")
	require.True(t, firstTook && secondTook, "both hold it: one started it, the other renewed it")
	require.True(t, f.claimer.IsClaimed(f.supplier))
}

// TestRenewAllClaims_DoesNotStretchTheLeaseOfAReleasedSupplier: a renewal pass
// whose snapshot predates the release still finds the key ours -- its drain
// holds it on a budget -- and must not renew it to ClaimTTL.
func TestRenewAllClaims_DoesNotStretchTheLeaseOfAReleasedSupplier(t *testing.T) {
	f := newLeaseFixture(t, "instance-no-stretch")
	ctx := context.Background()
	const budget = 20 * time.Second // below the default ClaimTTL of 90 s
	f.client.AddHook(&beforeCmd{name: "get", key: f.claimKey, action: func() {
		require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
		require.NoError(t, f.claimer.ExtendDrainLease(ctx, f.supplier, budget))
	}})

	f.claimer.renewAllClaims()

	ttl, err := f.client.PTTL(ctx, f.claimKey).Result()
	require.NoError(t, err)
	require.True(t, ttl > 0 && ttl <= budget,
		"a pass whose snapshot predates the release renewed the drain's lease past its budget: %v", ttl)
}

// TestFinishRelease_AndFinishShutdown_LeaveAPeersLeaseAlone: both delete a
// lease only if it is still this instance's. A drain that outran its budget, or
// a shutdown after a peer took a supplier, would otherwise free the peer's
// lease under it.
func TestFinishRelease_AndFinishShutdown_LeaveAPeersLeaseAlone(t *testing.T) {
	ctx := context.Background()
	requirePeerKept := func(t *testing.T, f *leaseFixture, what string) {
		t.Helper()
		require.Equal(t, "peer-instance", f.owner(t), "%s deleted a lease a peer holds", what)
		ttl, err := f.client.PTTL(ctx, f.claimKey).Result()
		require.NoError(t, err)
		require.True(t, ttl > 0 && ttl <= time.Minute, "and the peer's lease keeps its own TTL: %v", ttl)
	}

	draining := newLeaseFixture(t, "instance-finish-peer")
	require.NoError(t, draining.claimer.Release(ctx, draining.supplier, triggerRebalanceRelease))
	draining.setPeerOwner(t)
	require.NoError(t, draining.claimer.FinishRelease(ctx, draining.supplier))
	requirePeerKept(t, draining, "FinishRelease")

	claimed := newLeaseFixture(t, "instance-shutdown-peer")
	claimed.setPeerOwner(t)
	claimed.claimer.FinishShutdown(ctx)
	requirePeerKept(t, claimed, "FinishShutdown")
}

// TestExtendDrainLease_LeavesAPeersLeaseAlone: the budget applies to this
// instance's lease only; set on a peer's, it would cut the peer's lease short.
func TestExtendDrainLease_LeavesAPeersLeaseAlone(t *testing.T) {
	f := newLeaseFixture(t, "instance-extend-peer")
	ctx := context.Background()
	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
	require.NoError(t, f.client.Set(ctx, f.claimKey, "peer-instance", 90*time.Second).Err())

	require.NoError(t, f.claimer.ExtendDrainLease(ctx, f.supplier, 20*time.Second))

	require.Equal(t, "peer-instance", f.owner(t))
	ttl, err := f.client.PTTL(ctx, f.claimKey).Result()
	require.NoError(t, err)
	require.True(t, ttl > 20*time.Second, "the drain budget was set on a lease a peer holds: %v", ttl)
}

// beforeLeaseDelete runs action ONCE, right before the lease-delete script on
// key -- EVALSHA or its EVAL fallback, with one ARGV (the extension has two).
type beforeLeaseDelete struct {
	key    string
	fired  atomic.Bool
	action func()
}

func (h *beforeLeaseDelete) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *beforeLeaseDelete) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) == 5 && args[3] == h.key &&
			h.fired.CompareAndSwap(false, true) {
			h.action()
		}
		return next(ctx, cmd)
	}
}

func (h *beforeLeaseDelete) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestFinishRelease_NoClaimBetweenTheEndOfDrainingAndTheDelete: FinishRelease
// deletes first and only then clears draining. The other way round, a TryClaim
// landing in between finds the key ours with nothing running it, claims it --
// and the delete then frees the lease of the supplier just started.
func TestFinishRelease_NoClaimBetweenTheEndOfDrainingAndTheDelete(t *testing.T) {
	f := newLeaseFixture(t, "instance-finish-order")
	ctx := context.Background()
	require.NoError(t, f.claimer.Release(ctx, f.supplier, triggerRebalanceRelease))
	claims := countClaims(f)
	var tookBeforeDelete bool
	f.client.AddHook(&beforeLeaseDelete{key: f.claimKey, action: func() {
		tookBeforeDelete = f.claimer.TryClaim(ctx, f.supplier)
	}})

	require.NoError(t, f.claimer.FinishRelease(ctx, f.supplier))

	require.False(t, tookBeforeDelete, "claimed right before the delete that ends its drain")
	require.False(t, f.claimer.IsClaimed(f.supplier))
	require.Zero(t, claims.Load())
}

// TestReleaseLost_AFailedDrainStartDoesNotLeaveItDraining: no drain started, so
// nothing would ever take the supplier out of draining -- unclaimable here for
// good.
func TestReleaseLost_AFailedDrainStartDoesNotLeaveItDraining(t *testing.T) {
	f := newLeaseFixture(t, "instance-lost-drain-failed")
	ctx := context.Background()
	f.claimer.SetCallbacks(
		func(context.Context, string) error { return nil },
		func(context.Context, string, string) error { return errors.New("injected: manager closing") },
	)

	f.claimer.releaseLost(ctx, f.supplier, triggerLeaseStolen)

	require.False(t, f.claimer.isDraining(f.supplier), "a failed drain start left the supplier draining")
	require.NoError(t, f.client.Del(ctx, f.claimKey).Err())
	require.True(t, f.claimer.TryClaim(ctx, f.supplier), "and it can be claimed again")
}

// ---------------------------------------------------------------------------
// Manager and claimer together.
// ---------------------------------------------------------------------------

// drainLeaseFixture is a real supplier whose consume loop is a stand-in that,
// once cancelled, holds state.wg until the test lets it go: the drain is held
// exactly where the old loop could still write.
type drainLeaseFixture struct {
	w         *batchWorker
	claimer   *SupplierClaimer
	peer      *SupplierClaimer
	claimKey  string
	loopCtx   context.Context
	hold      chan struct{}
	instance  string
	claimsRun *atomic.Int32
}

func newDrainLeaseFixture(t *testing.T, supplier string) *drainLeaseFixture {
	t.Helper()
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, supplier, "a")
	const instance = "instance-drain-lease"
	w.mgr.config.RedisClient = client
	w.mgr.config.MinerID = instance

	loopCtx, cancel := context.WithCancel(w.ctx)
	hold := make(chan struct{})
	w.state.cancelFn = cancel
	w.state.wg.Add(1)
	go func() {
		defer w.state.wg.Done()
		<-loopCtx.Done()
		<-hold
	}()

	var claimsRun atomic.Int32
	claimer := NewSupplierClaimer(zerolog.Nop(), client, instance, SupplierClaimerConfig{})
	claimer.SetCallbacks(
		func(context.Context, string) error { claimsRun.Add(1); return nil },
		w.mgr.onSupplierReleased,
	)
	claimer.allSuppliers = []string{supplier}
	claimerCtx, stopClaimer := context.WithCancel(context.Background()) // the loops' context, as Start sets it
	t.Cleanup(stopClaimer)
	claimer.ctx, claimer.cancelFn = claimerCtx, stopClaimer
	require.True(t, claimer.TryClaim(w.ctx, supplier), "premise: this instance holds the lease")
	claimsRun.Store(0)
	w.mgr.claimer = claimer

	return &drainLeaseFixture{
		w: w, claimer: claimer,
		peer:     NewSupplierClaimer(zerolog.Nop(), client, "peer-instance", SupplierClaimerConfig{}),
		claimKey: client.KB().MinerClaimKey(supplier),
		loopCtx:  loopCtx, hold: hold, instance: instance, claimsRun: &claimsRun,
	}
}

// gateScript holds the first run of the lease-extension script on key until
// open is closed, and runs afterRun once, when it has succeeded -- EVALSHA, or
// the EVAL go-redis falls back to on NOSCRIPT. KEYS[1] sits at argument 3; the
// extension is the lease script with a second ARGV (the budget), so the delete
// is never held.
type gateScript struct {
	key      string
	fired    atomic.Bool
	open     chan struct{}
	afterRun func()
	ranOnce  sync.Once
}

func newGateScript(key string, afterRun func()) *gateScript {
	return &gateScript{key: key, open: make(chan struct{}), afterRun: afterRun}
}

func (h *gateScript) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *gateScript) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if (cmd.Name() != "evalsha" && cmd.Name() != "eval") || len(args) < 6 || args[3] != h.key {
			return next(ctx, cmd)
		}
		if h.fired.CompareAndSwap(false, true) {
			<-h.open
		}
		err := next(ctx, cmd)
		if err == nil {
			h.ranOnce.Do(h.afterRun)
		}
		return err
	}
}

func (h *gateScript) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f *drainLeaseFixture) owner(t *testing.T) string {
	t.Helper()
	owner, err := f.w.client.Get(f.w.ctx, f.claimKey).Result()
	if errors.Is(err, redis.Nil) {
		return ""
	}
	require.NoError(t, err)
	return owner
}

// TestDrain_TheLeaseOutlivesTheOldConsumeLoop is T1 and T4, with the control
// at the end: a rebalance release, the old loop not yet done. The drain's first
// Redis call, the lease extension, is held too: the release must have stopped
// the old loop without waiting on any of the drain's work.
func TestDrain_TheLeaseOutlivesTheOldConsumeLoop(t *testing.T) {
	f := newDrainLeaseFixture(t, "pokt1drain_lease_held")
	ctx := f.w.ctx
	var extendedTTL atomic.Int64 // the lease's TTL right after the drain set it
	gate := newGateScript(f.claimKey, func() {
		ttl, err := f.w.client.PTTL(ctx, f.claimKey).Result()
		if err == nil {
			extendedTTL.Store(int64(ttl))
		}
	})
	f.w.client.AddHook(gate)

	require.NoError(t, f.claimer.Release(ctx, f.w.supplier, triggerRebalanceRelease))

	require.Error(t, f.loopCtx.Err(), "the old loop is told to stop before the release returns")
	_, stillThere := f.w.mgr.GetSupplierState(f.w.supplier)
	require.False(t, stillThere, "and its state has left the map")

	require.Equal(t, f.instance, f.owner(t), "while the old loop may still write, the lease is ours")
	require.False(t, f.peer.TryClaim(ctx, f.w.supplier), "and no peer can take the supplier")

	close(gate.open)
	close(f.hold)
	f.w.mgr.waitDrains()

	ttl := time.Duration(extendedTTL.Load())
	require.True(t, ttl > 0 && ttl <= drainLeaseBudget(),
		"the lease lives the drain budget, not the claim TTL, so a hung drain gives it up: %v", ttl)

	require.Empty(t, f.owner(t), "the drain is over: the lease goes")
	require.True(t, f.peer.TryClaim(ctx, f.w.supplier), "and the peer takes it")
}

// TestDrain_WithNoStateStillFreesTheLease: a claim whose callback failed built
// no state, and its release still has to end, or the supplier stays draining --
// and unclaimable by this instance -- for good.
func TestDrain_WithNoStateStillFreesTheLease(t *testing.T) {
	f := newLeaseFixture(t, "instance-drain-no-state")
	mgr := newTestSupplierManager(t, nil)
	f.claimer.SetCallbacks(func(context.Context, string) error { return nil }, mgr.onSupplierReleased)
	mgr.claimer = f.claimer

	require.NoError(t, f.claimer.Release(context.Background(), f.supplier, triggerClaimCallbackFailed))
	mgr.waitDrains()

	require.False(t, f.keyExists(t), "no state to tear down, but the lease must still go")
	require.False(t, f.claimer.isDraining(f.supplier))
}

// TestDrain_ItsLeaseCallsOutliveTheReleasersContext: a drain outlives the call
// that released it -- a renewal pass, a reconcile, Close's cancelled context --
// so extending and deleting the lease must not run on that caller's context.
// The release's context is cancelled while the extension is held and the drain
// with it.
func TestDrain_ItsLeaseCallsOutliveTheReleasersContext(t *testing.T) {
	f := newDrainLeaseFixture(t, "pokt1drain_ctx_outlived")
	var extendedTTL atomic.Int64
	gate := newGateScript(f.claimKey, func() {
		ttl, err := f.w.client.PTTL(f.w.ctx, f.claimKey).Result()
		if err == nil {
			extendedTTL.Store(int64(ttl))
		}
	})
	f.w.client.AddHook(gate)
	releaseCtx, cancelRelease := context.WithCancel(f.w.ctx)

	require.NoError(t, f.claimer.Release(releaseCtx, f.w.supplier, triggerRebalanceRelease))
	cancelRelease()
	close(gate.open)
	close(f.hold)
	f.w.mgr.waitDrains()

	ttl := time.Duration(extendedTTL.Load())
	require.True(t, ttl > 0 && ttl <= drainLeaseBudget(),
		"the extension ran on the releaser's cancelled context: the lease kept its claim TTL, %v", ttl)
	require.Empty(t, f.owner(t), "the delete ran on the releaser's cancelled context: the lease outlived its drain")
}

// TestDrain_NoReTakeWhileTheDrainRuns is E4: this instance's own rebalance
// wants the supplier back before its drain ended. Taken back, the new claim
// would be counted with no state behind it once the drain finished.
func TestDrain_NoReTakeWhileTheDrainRuns(t *testing.T) {
	f := newDrainLeaseFixture(t, "pokt1drain_no_retake")
	ctx := f.w.ctx
	require.NoError(t, f.claimer.Release(ctx, f.w.supplier, triggerRebalanceRelease))

	f.claimer.claimMore(1)
	f.claimer.claimOrphaned()

	require.False(t, f.claimer.IsClaimed(f.w.supplier), "claimed with no state behind it")
	require.Zero(t, f.claimsRun.Load())

	close(f.hold)
	f.w.mgr.waitDrains()
	f.claimer.claimMore(1)
	require.True(t, f.claimer.IsClaimed(f.w.supplier), "the drain is over: it can be claimed again")
}

// TestClose_KeepsEveryLeaseUntilItsSupplierIsTornDown is T2: the shutdown used
// to release every lease before cancelling a single supplier.
func TestClose_KeepsEveryLeaseUntilItsSupplierIsTornDown(t *testing.T) {
	f := newDrainLeaseFixture(t, "pokt1drain_lease_close")
	ctx := f.w.ctx

	closed := make(chan error, 1)
	go func() { closed <- f.w.mgr.Close() }()
	<-f.loopCtx.Done() // Close has cancelled the supplier; its loop is held

	require.Equal(t, f.instance, f.owner(t), "shutting down, but the old loop may still write: the lease is ours")
	require.False(t, f.peer.TryClaim(ctx, f.w.supplier))

	close(f.hold)
	require.NoError(t, <-closed)

	require.Empty(t, f.owner(t), "torn down: the lease goes")
}

// TestClose_ARefusedReleaseKeepsTheClaimForTheShutdown: once Close has started,
// a release -- a key removal, say -- would start a drain Close no longer waits
// for. It is refused, and the claim stays for Close to tear down and delete.
func TestClose_ARefusedReleaseKeepsTheClaimForTheShutdown(t *testing.T) {
	f := newDrainLeaseFixture(t, "pokt1drain_release_after_close")
	close(f.hold)
	f.w.mgr.mu.Lock()
	f.w.mgr.closed = true // Close's first step, without the rest of it
	f.w.mgr.mu.Unlock()

	require.Error(t, f.claimer.Release(f.w.ctx, f.w.supplier, triggerKeyRemoval),
		"a release after Close has started must be refused")

	require.True(t, f.claimer.IsClaimed(f.w.supplier), "kept for the shutdown")
	_, stillThere := f.w.mgr.GetSupplierState(f.w.supplier)
	require.True(t, stillThere, "and so is its state, for Close to collect")
	require.Equal(t, f.instance, f.owner(t))
}

// TestOnSupplierClaimed_AfterCloseStartsNothing: a supplier added once Close
// has collected the others would never be torn down.
func TestOnSupplierClaimed_AfterCloseStartsNothing(t *testing.T) {
	var warmedUp atomic.Bool
	mgr := newTestSupplierManager(t, &testSupplierQueryClient{
		getSupplierFn: func(context.Context, string) (sharedtypes.Supplier, error) {
			warmedUp.Store(true)
			return sharedtypes.Supplier{}, grpcstatus.Error(codes.NotFound, "not found")
		},
	})
	require.NoError(t, mgr.Close())

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("went past the closed check: %v", r)
			}
		}()
		return mgr.onSupplierClaimed(context.Background(), "pokt1claimed_after_close")
	}()

	require.False(t, warmedUp.Load(), "a supplier claimed after Close must not start: nothing would tear it down")
	require.ErrorContains(t, err, "supplier manager is closed")
}
