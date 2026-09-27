//go:build test

package miner

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// beforeCmd runs action ONCE, right before the first command whose name and
// first argument match -- the mirror of afterCmd, for placing a write before a
// command the code under test is about to issue.
type beforeCmd struct {
	name   string
	key    string
	fired  atomic.Bool
	action func()
}

func (h *beforeCmd) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *beforeCmd) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if len(args) >= 2 && cmd.Name() == h.name {
			if k, ok := args[1].(string); ok && k == h.key && h.fired.CompareAndSwap(false, true) {
				h.action()
			}
		}
		return next(ctx, cmd)
	}
}

func (h *beforeCmd) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// afterReleaseScript runs action ONCE, right after the check-and-delete script
// FinishRelease runs on key succeeds -- EVALSHA or, on NOSCRIPT, the EVAL go-redis
// falls back to. KEYS[1] sits at argument 3.
type afterReleaseScript struct {
	key    string
	fired  atomic.Bool
	action func()
}

func (h *afterReleaseScript) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *afterReleaseScript) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if err == nil && (cmd.Name() == "evalsha" || cmd.Name() == "eval") && len(args) >= 4 {
			if k, ok := args[3].(string); ok && k == h.key && h.fired.CompareAndSwap(false, true) {
				h.action()
			}
		}
		return err
	}
}

func (h *afterReleaseScript) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// countClaims replaces the fixture's callbacks with ones that count re-takes
// and accept releases.
func countClaims(f *leaseFixture) *atomic.Int32 {
	var claims atomic.Int32
	f.claimer.SetCallbacks(
		func(context.Context, string) error { claims.Add(1); return nil },
		func(context.Context, string, string) error { return nil },
	)
	return &claims
}

// releaseAndDrain releases the supplier and then finishes the release, as the
// manager's drain does once the teardown is over: Release alone keeps the lease.
func (f *leaseFixture) releaseAndDrain(t *testing.T) {
	t.Helper()
	require.NoError(t, f.claimer.Release(context.Background(), f.supplier, triggerRebalanceRelease))
	require.NoError(t, f.claimer.FinishRelease(context.Background(), f.supplier))
}

func (f *leaseFixture) keyExists(t *testing.T) bool {
	t.Helper()
	n, err := f.client.Exists(context.Background(), f.claimKey).Result()
	require.NoError(t, err)
	return n == 1
}

// The L3 of df5441c (2026-09-11) saw one supplier claimed and released 31 times,
// every ~32 s: renewAllClaims took its snapshot of the claimed map, rebalance
// released the supplier on its own goroutine, and the renewal read the missing
// key as an expired lease and took it back. The next rebalance released it
// again, and each cycle tore the supplier down and handed its relays back.

// TestRenewAllClaims_DoesNotRetakeASupplierReleasedBeforeTheGet: the release,
// and the drain that deletes its lease, land between the snapshot and the GET,
// so the GET finds no key.
func TestRenewAllClaims_DoesNotRetakeASupplierReleasedBeforeTheGet(t *testing.T) {
	const instance = "instance-release-before-get"
	f := newLeaseFixture(t, instance)
	claims := countClaims(f)
	claimed := supplierClaimedTotal.WithLabelValues(f.supplier, instance)
	before := testutil.ToFloat64(claimed)

	f.client.AddHook(&beforeCmd{name: "get", key: f.claimKey, action: func() { f.releaseAndDrain(t) }})

	f.claimer.renewAllClaims()

	require.False(t, f.claimer.IsClaimed(f.supplier), "released during the pass: the renewal must not take it back")
	require.False(t, f.keyExists(t), "and the lease key stays gone, free for the peer it was released to")
	require.Zero(t, claims.Load(), "no re-take, so no second start of the supplier")
	require.Equal(t, before, testutil.ToFloat64(claimed))
}

// TestRenewAllClaims_DoesNotRetakeASupplierReleasedBetweenGetAndExpire: the
// release and its drain land after the GET saw the lease as ours, so EXPIRE
// finds no key.
func TestRenewAllClaims_DoesNotRetakeASupplierReleasedBetweenGetAndExpire(t *testing.T) {
	const instance = "instance-release-before-expire"
	f := newLeaseFixture(t, instance)
	claims := countClaims(f)
	claimed := supplierClaimedTotal.WithLabelValues(f.supplier, instance)
	before := testutil.ToFloat64(claimed)

	f.client.AddHook(&afterCmd{name: "get", key: f.claimKey, action: func() { f.releaseAndDrain(t) }})

	f.claimer.renewAllClaims()

	require.False(t, f.claimer.IsClaimed(f.supplier))
	require.False(t, f.keyExists(t))
	require.Zero(t, claims.Load())
	require.Equal(t, before, testutil.ToFloat64(claimed))
}

// TestRelease_ARenewalRightAfterTheDeleteLeavesNoOrphanLease is the other
// window: a renewal that runs right after the lease is deleted. With the key
// deleted before the local bookkeeping, the renewal re-took the supplier and
// Release then erased the re-take from the map -- a lease held in Redis that
// nothing renews, which the peer reads as "already claimed by another instance"
// for its TTL. The delete now comes last of all, when the drain finishes.
func TestRelease_ARenewalRightAfterTheDeleteLeavesNoOrphanLease(t *testing.T) {
	f := newLeaseFixture(t, "instance-renew-after-del")
	claims := countClaims(f)

	f.client.AddHook(&afterReleaseScript{key: f.claimKey, action: func() { f.claimer.renewAllClaims() }})

	f.releaseAndDrain(t)

	require.False(t, f.keyExists(t),
		"no lease may survive the release: one that exists while IsClaimed is false is renewed by nobody and blocks the peer")
	require.False(t, f.claimer.IsClaimed(f.supplier))
	require.Zero(t, claims.Load())
}

// TestRenewAllClaims_RecoversALeaseRetakenInsideTheReleaseCooldown: a supplier
// released and then legitimately taken back (claimMore does not look at the
// cooldown, and nothing clears it) must still have a lease that REALLY expires
// recovered. Reading the cooldown as "released on purpose" left it in the map,
// signing, with no lease and no report, until the cooldown ran out.
func TestRenewAllClaims_RecoversALeaseRetakenInsideTheReleaseCooldown(t *testing.T) {
	f := newLeaseFixture(t, "instance-retaken-in-cooldown")
	ctx := context.Background()
	var lost atomic.Int32
	f.claimer.SetCallbacks(
		func(context.Context, string) error { return nil },
		func(context.Context, string, string) error { lost.Add(1); return nil },
	)

	f.releaseAndDrain(t)
	require.True(t, f.claimer.TryClaim(ctx, f.supplier), "taken back legitimately, as claimMore does")
	require.True(t, f.claimer.inRecentReleaseCooldown(f.supplier), "premise: still inside the release cooldown")
	lost.Store(0)

	require.NoError(t, f.client.Del(ctx, f.claimKey).Err()) // the lease really expires

	f.claimer.renewAllClaims()

	require.True(t, f.keyExists(t), "a lease that really expired must be taken back, cooldown or not")
	require.True(t, f.claimer.IsClaimed(f.supplier))
	require.Zero(t, lost.Load(), "recovered, so nothing is reported lost")
}
