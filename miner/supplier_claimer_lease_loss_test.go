//go:build test

package miner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// afterCmd runs action ONCE, right after the first command whose name and first
// argument match, and lets the command's own result through untouched.
//
// It exists because the two interesting lease-loss branches are RACES inside a
// single renewAllClaims pass -- the key is gone at GET and taken by the time
// SETNX runs -- and there is no way to interleave a peer from outside the call.
// A one-shot hook is the only deterministic way to place the peer's write
// between two commands the code issues back to back. No time.Sleep: the hook
// fires on the command, not on a clock.
type afterCmd struct {
	name   string
	key    string
	fired  atomic.Bool
	action func()
}

func (h *afterCmd) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *afterCmd) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if len(args) >= 2 && cmd.Name() == h.name {
			if k, ok := args[1].(string); ok && k == h.key {
				if h.fired.CompareAndSwap(false, true) {
					h.action()
				}
			}
		}
		return err
	}
}

func (h *afterCmd) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// failCmd makes every matching command return err, for as long as it is armed.
// It is how the fourth branch is induced: a GET that fails with something that
// is NOT redis.Nil, which is the only lease-loss branch reachable when the
// thing that is broken is Redis itself.
type failCmd struct {
	name string
	key  string
	err  atomic.Pointer[error]
}

func (h *failCmd) arm(err error)                               { h.err.Store(&err) }
func (h *failCmd) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *failCmd) ProcessPipelineHook(n redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return n
}

func (h *failCmd) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if e := h.err.Load(); e != nil && cmd.Name() == h.name {
			args := cmd.Args()
			if len(args) >= 2 {
				if k, ok := args[1].(string); ok && k == h.key {
					cmd.SetErr(*e)
					return *e
				}
			}
		}
		return next(ctx, cmd)
	}
}

// leaseFixture is one claimer on its OWN Redis client, so a permanent go-redis
// hook cannot leak into the suite's shared client (hooks cannot be removed).
type leaseFixture struct {
	claimer  *SupplierClaimer
	client   *redisutil.Client
	supplier string
	claimKey string
	released []string
	triggers []string
}

func newLeaseFixture(t *testing.T, instanceID string) *leaseFixture {
	t.Helper()

	client, _ := newTestRedis(t)
	claimer := NewSupplierClaimer(zerolog.Nop(), client, instanceID, SupplierClaimerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	claimer.ctx, claimer.cancelFn = ctx, cancel

	f := &leaseFixture{
		claimer:  claimer,
		client:   client,
		supplier: "pokt1supplier-lease",
	}
	f.claimKey = client.KB().MinerClaimKey(f.supplier)
	claimer.allSuppliers = []string{f.supplier}

	claimer.SetCallbacks(
		func(context.Context, string) error { return nil },
		func(_ context.Context, supplier, trigger string) error {
			f.released = append(f.released, supplier)
			f.triggers = append(f.triggers, trigger)
			return nil
		},
	)

	require.NoError(t, claimer.registerInstance(ctx))
	require.True(t, claimer.TryClaim(ctx, f.supplier), "fixture must start owning the lease")
	return f
}

// setPeerOwner writes another instance's id into the lease key.
func (f *leaseFixture) setPeerOwner(t *testing.T) {
	t.Helper()
	require.NoError(t, f.client.Set(context.Background(), f.claimKey, "peer-instance", time.Minute).Err())
}

// ---------------------------------------------------------------------------
// THE DISCRIMINANT PAIR.
//
// Written before its twin on purpose: a test written after the fix tends to
// describe the fix. This one says what must NOT happen, and it is the assertion
// that stops a broad-brush "release on every branch" implementation -- which
// would destroy a supplier we still own every time Redis drops a key.
//
// NOTE ON ITS COLOUR: this test is GREEN against the tree as it stands, because
// today NO branch ever notifies. That green is empty -- it proves nothing until
// the fix exists. It is not coverage; it is a trap set for the fix.
// ---------------------------------------------------------------------------

// TestLeaseLoss_ExpiredButRecovered_DoesNotRelease: the lease key vanished, and
// TryClaim got it back. We still own the supplier, so nothing may be drained.
func TestLeaseLoss_ExpiredButRecovered_DoesNotRelease(t *testing.T) {
	f := newLeaseFixture(t, "instance-recovered")

	require.NoError(t, f.client.Del(context.Background(), f.claimKey).Err())

	f.claimer.renewAllClaims()

	require.Empty(t, f.released,
		"TryClaim recovered the lease, so the supplier is still ours: draining it would destroy a live supplier")
	require.True(t, f.claimer.IsClaimed(f.supplier), "recovered lease must be back in the claimed map")
	require.False(t, f.claimer.inRecentReleaseCooldown(f.supplier),
		"a recovered lease was never released, so it must not enter the release cooldown")
}

// TestLeaseLoss_ExpiredAndTaken_Releases: the lease key vanished and a peer took
// it before TryClaim could. We no longer own the supplier and must say so.
func TestLeaseLoss_ExpiredAndTaken_Releases(t *testing.T) {
	f := newLeaseFixture(t, "instance-expired")

	require.NoError(t, f.client.Del(context.Background(), f.claimKey).Err())

	// Place the peer's write BETWEEN the GET that returns redis.Nil and the
	// SETNX that TryClaim issues. That interleaving is the whole case.
	f.client.AddHook(&afterCmd{
		name: "get", key: f.claimKey,
		action: func() { f.setPeerOwner(t) },
	})

	f.claimer.renewAllClaims()

	require.Equal(t, []string{f.supplier}, f.released, "a lease we could not recover must be reported exactly once")
	require.Equal(t, []string{triggerLeaseExpired}, f.triggers)
	require.False(t, f.claimer.IsClaimed(f.supplier))
}

// TestLeaseLoss_Stolen_Releases: the key holds another instance's id. There is
// nothing to recover -- the peer owns it -- so the loss is reported immediately.
func TestLeaseLoss_Stolen_Releases(t *testing.T) {
	f := newLeaseFixture(t, "instance-stolen")
	before := testutil.ToFloat64(supplierLeaseLostTotal.WithLabelValues(triggerLeaseStolen, "instance-stolen"))
	f.setPeerOwner(t)

	f.claimer.renewAllClaims()

	require.Equal(t, []string{f.supplier}, f.released)
	require.Equal(t, []string{triggerLeaseStolen}, f.triggers)
	require.False(t, f.claimer.IsClaimed(f.supplier))

	// The lease loss lands on its OWN series, labelled by trigger. It must not
	// borrow supplier_released_total: that one means "handed over on purpose",
	// and a panel counting releases would start counting split-brain windows
	// with no way to tell them apart.
	require.Equal(t, before+1,
		testutil.ToFloat64(supplierLeaseLostTotal.WithLabelValues(triggerLeaseStolen, "instance-stolen")),
		"a lost lease must be counted under its trigger")
}

// TestLeaseLoss_RenewRaceRecovered_DoesNotRelease: GET says we own it, the key
// dies before EXPIRE, and TryClaim gets it back. Same discriminant, other branch.
func TestLeaseLoss_RenewRaceRecovered_DoesNotRelease(t *testing.T) {
	f := newLeaseFixture(t, "instance-renewrace-ok")

	f.client.AddHook(&afterCmd{
		name: "get", key: f.claimKey,
		action: func() { _ = f.client.Del(context.Background(), f.claimKey).Err() },
	})

	f.claimer.renewAllClaims()

	require.Empty(t, f.released, "the renewal lost the race but TryClaim recovered the lease")
	require.True(t, f.claimer.IsClaimed(f.supplier))
}

// TestLeaseLoss_RenewRaceTaken_Releases: same race, but a peer takes the key
// before TryClaim. Two chained one-shots: kill the key after GET so EXPIRE
// reports false, then hand it to the peer after EXPIRE so SETNX fails.
func TestLeaseLoss_RenewRaceTaken_Releases(t *testing.T) {
	f := newLeaseFixture(t, "instance-renewrace-lost")

	f.client.AddHook(&afterCmd{
		name: "get", key: f.claimKey,
		action: func() { _ = f.client.Del(context.Background(), f.claimKey).Err() },
	})
	f.client.AddHook(&afterCmd{
		name: "expire", key: f.claimKey,
		action: func() { f.setPeerOwner(t) },
	})

	f.claimer.renewAllClaims()

	require.Equal(t, []string{f.supplier}, f.released)
	require.Equal(t, []string{triggerLeaseExpired}, f.triggers)
	require.False(t, f.claimer.IsClaimed(f.supplier))
}

// TestLeaseLoss_RenewStalled_Releases covers the fourth branch: the GET fails
// with something that is NOT redis.Nil. Today that branch logs and continues,
// so the supplier stays in the claimed map AND in the manager, signing, while
// the key quietly expires and a peer picks it up.
//
// It is the only branch that can fire when the broken thing is Redis itself --
// every other branch asks Redis in order to learn that it lost Redis -- so the
// detection cannot be a Redis call. It is a local clock: no renewal has
// succeeded for longer than the lease could have survived.
func TestLeaseLoss_RenewStalled_Releases(t *testing.T) {
	f := newLeaseFixture(t, "instance-stalled")

	h := &failCmd{name: "get", key: f.claimKey}
	h.arm(errors.New("connection reset by peer"))
	f.client.AddHook(h)

	// First stalled pass: the lease could still be alive, so nothing is reported.
	f.claimer.renewAllClaims()
	require.Empty(t, f.released, "one failed GET is not proof the lease is gone")

	// Advance the claimer's own clock past the TTL. nowFn is a struct field
	// captured at construction and read on this goroutine, never inside one the
	// claimer spawns -- reading it there is what produced a DATA RACE before.
	f.claimer.nowFn = func() time.Time { return time.Now().Add(2 * f.claimer.config.ClaimTTL) }

	f.claimer.renewAllClaims()

	require.Equal(t, []string{f.supplier}, f.released,
		"no renewal has succeeded for longer than ClaimTTL: the lease is gone whatever Redis says")
	require.Equal(t, []string{triggerRenewStalled}, f.triggers)
	require.False(t, f.claimer.IsClaimed(f.supplier))
}

// TestLeaseLoss_WritesReleaseCooldown is deliberately separate from the tests
// above and stays RED while they are green if the cooldown write is left out.
//
// It separates "tell the manager" from "put the guard up". Without the cooldown,
// claimOrphaned re-takes the supplier while our own drain is still tearing it
// down, onSupplierClaimed returns early on idempotency, and the drain then
// deletes the supplier we just re-took: the item-35 defect inverted, created by
// its own fix.
func TestLeaseLoss_WritesReleaseCooldown(t *testing.T) {
	f := newLeaseFixture(t, "instance-cooldown")
	f.setPeerOwner(t)

	f.claimer.renewAllClaims()

	require.True(t, f.claimer.inRecentReleaseCooldown(f.supplier),
		"a lost lease must enter the cooldown: claimOrphaned must not re-take a supplier we are still draining")
}

// TestOnSupplierReleased_ReturnsBeforeTheAuditQuery pins the invariant that
// makes the inline call in renewAllClaims safe.
//
// renewAllClaims walks every supplier SERIALLY, and the drain callback reaches
// a chain query with a 5s timeout whose RESULT IS DISCARDED -- the comment on
// verifySupplierUnstaked says it no longer vetoes the drain. Left inline, K
// lost leases cost up to K*5s inside a loop that has 90s of total headroom
// (ClaimTTL 90s / RenewRate 10s), and the event that loses leases -- Redis
// blinking, a slow full node -- is the SAME event that makes that query slow.
// One lost lease would have cascaded into more lost leases.
//
// The invariant lives HERE and not in the claimer on purpose: the claimer does
// call its callback inline, and that is safe only for as long as the callback
// returns promptly. This test is what makes that "only for as long as" hold --
// move the query back in front of the goroutine and it fails.
func TestOnSupplierReleased_ReturnsBeforeTheAuditQuery(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	client := &testSupplierQueryClient{
		getSupplierFn: func(_ context.Context, addr string) (sharedtypes.Supplier, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-blocked
			return sharedtypes.Supplier{OperatorAddress: addr}, nil
		},
	}
	mgr := newTestSupplierManager(t, client)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_ = mgr.onSupplierReleased(context.Background(), "pokt1slow", triggerRebalanceRelease)
	}()

	// The callback must come back while the query is still in flight.
	<-entered
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		close(blocked)
		t.Fatal("onSupplierReleased waited for the audit query: inline, this stalls every supplier behind it in the serial renewal loop")
	}

	close(blocked)
	mgr.waitDrains()
}

// TestOnSupplierReleased_KeyRemovalSkipsTheSecondAudit: the key-removal path
// already ran this exact 5s verification one call up, on a supplier an operator
// touched by hand. Two chain queries for one decision bought nothing.
func TestOnSupplierReleased_KeyRemovalSkipsTheSecondAudit(t *testing.T) {
	var calls atomic.Int32
	client := &testSupplierQueryClient{
		getSupplierFn: func(_ context.Context, addr string) (sharedtypes.Supplier, error) {
			calls.Add(1)
			return sharedtypes.Supplier{OperatorAddress: addr}, nil
		},
	}
	mgr := newTestSupplierManager(t, client)

	require.NoError(t, mgr.onSupplierReleased(context.Background(), "pokt1key", triggerKeyRemoval))
	mgr.waitDrains()

	require.Zero(t, calls.Load(), "the key-removal path already audited this supplier; the second query is pure cost")
}
