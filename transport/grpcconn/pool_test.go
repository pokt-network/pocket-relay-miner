package grpcconn

import (
	"google.golang.org/grpc"

	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// testTarget is never dialled: grpc.NewClient is lazy, so a pool can be built
// and exercised with no server listening.
var testTarget = Target{Endpoint: "127.0.0.1:59999", UseTLS: false}

// TestSizeFor_IsProportionalWithAFloorAndNoCeiling pins the sizing function
// across the range the design claims to support.
//
// The large cases are the point, not decoration. 771 suppliers is a field
// observation recorded as the worst case SO FAR -- the design has to hold from
// there UPWARD -- so a table whose biggest row is 771 exercises none of the
// range the function exists for, and would have passed just as well against the
// clamped version that returned 8 forever.
func TestSizeFor_IsProportionalWithAFloorAndNoCeiling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claimed int
		want    int
	}{
		{"no leases yet: the floor", 0, DefaultPoolFloor},
		{"one lease: still the floor", 1, 2},
		{"floor still absorbs", 100, 2},
		{"exactly two connections' worth", 160, 2},
		{"breakpoint two to three", 161, 3},
		{"exactly three connections' worth", 240, 3},
		{"breakpoint three to four", 241, 4},
		{"worst case measured in the field", 771, 10},
		{"well past the field figure", 2000, 25},
		{"further still", 5000, 63},
		{"an order of magnitude past the field figure", 10000, 125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, SizeFor(tc.claimed))
		})
	}
}

// TestSizeFor_ExactMultiplesDoNotRoundUp is what stops the headroom being
// counted twice.
//
// The rule is one connection per eighty suppliers, and the eighty is already the
// margin: the real client ceiling is a hundred, so every connection is holding
// twenty streams back, which is where the probe and the fee lookup live. An
// implementation that reserved streams GLOBALLY on top of that would discount
// the same headroom a second time, and the only place it shows is here -- at an
// exact multiple, where the extra reservation tips the division into one more
// connection. Every rounder case gives the same answer either way, which is why
// the table above cannot make this assertion on its own.
func TestSizeFor_ExactMultiplesDoNotRoundUp(t *testing.T) {
	for _, claimed := range []int{160, 240, 800, 2000, 8000} {
		require.Equal(t, claimed/poolStreamsPerConn, SizeFor(claimed),
			"%d suppliers is an exact multiple of %d and must not need an extra connection",
			claimed, poolStreamsPerConn)
	}
}

func TestSizeFor_NegativeClaimedIsTheFloor(t *testing.T) {
	require.Equal(t, DefaultPoolFloor, SizeFor(-1))
}

// TestNewPool_StartsAtTheFloorWithEveryMemberUnhealthy is the structural half of
// "no member serves a transaction before it was verified".
func TestNewPool_StartsAtTheFloorWithEveryMemberUnhealthy(t *testing.T) {
	p, err := NewPool(testTarget, RoleTx, DefaultPoolFloor)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.Equal(t, DefaultPoolFloor, p.Len())
	require.Len(t, p.Members(), DefaultPoolFloor)
	require.Empty(t, p.HealthyMembers(),
		"a member that has never been probed must not be healthy")
}

// TestGrow_AppendsAndLeavesExistingMembersIDENTICAL is C2: a transaction already
// inside Invoke resolved its connection before the call, so growth must not
// replace what it is holding.
//
// It compares the connection POINTERS at the old indices, not the count: a grow
// that rebuilt the pool would keep the count correct and swap the objects
// underneath, which is exactly the failure and exactly what counting cannot see.
func TestGrow_AppendsAndLeavesExistingMembersIdentical(t *testing.T) {
	p, err := NewPool(testTarget, RoleTx, 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	before := p.Members()
	require.Len(t, before, 2)

	// Mark the existing members healthy BEFORE growing. Without this the
	// health assertions below ask about an empty set and pass for any answer.
	for _, m := range before {
		p.MarkHealth(m.Index, true)
	}

	added, err := p.Grow(5)
	require.NoError(t, err)
	require.Len(t, added, 3, "grow must report only what it added")
	require.Equal(t, 5, p.Len())

	after := p.Members()
	for i, old := range before {
		require.Same(t, old.Conn, after[i].Conn,
			"member %d was replaced by the grow; an in-flight call holds the old pointer", i)
	}

	// Walk EVERY member, not just the ones that existed before. Indices are
	// reassigned by the grow or they are not, and the subset that already
	// existed is exactly where that defect cannot show: a grow that numbered
	// its new members from zero again would leave these two untouched and
	// still be wrong. Duplicate indices are not a cosmetic label problem --
	// MarkHealth indexes by them, so the probe of a new member would write the
	// health of the old one that shares its number.
	for i, m := range after {
		require.Equal(t, i, m.Index,
			"member at position %d carries index %d: indices must continue the sequence", i, m.Index)
	}
	for k, m := range added {
		require.Equal(t, len(before)+k, m.Index,
			"the %dth added member must be numbered after the ones that existed", k)
	}

	healthy := p.HealthyMembers()
	require.Len(t, healthy, len(before),
		"a member added by Grow must start unhealthy, so only the warm-up admits it")
	for _, m := range healthy {
		require.Less(t, m.Index, len(before), "member %d was healthy without a probe", m.Index)
	}
}

func TestGrow_BelowOrAtCurrentSizeIsANoOp(t *testing.T) {
	p, err := NewPool(testTarget, RoleTx, 3)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	added, err := p.Grow(2)
	require.NoError(t, err)
	require.Empty(t, added)
	require.Equal(t, 3, p.Len(), "there is no shrink path")
}

// TestPick_SkipsTheUnhealthyMemberAndSaysWhichOne is C3.
//
// The assertion is on the INDEX the picker returned, not on "no traffic reached
// the sick one". The latter is satisfied by a round-robin that never advances
// and sends everything to member zero, so it cannot tell a working skip from a
// broken cursor.
func TestPick_SkipsTheUnhealthyMemberAndSaysWhichOne(t *testing.T) {
	p, err := NewPool(testTarget, RoleTx, 3)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	p.MarkHealth(0, true)
	p.MarkHealth(1, false)
	p.MarkHealth(2, true)

	sick := p.Members()[1].Conn
	healthyHits := map[int]int{}
	for i := 0; i < 60; i++ {
		got := p.pick()
		require.NotSame(t, sick, got, "the unhealthy member must never be picked")
		for _, m := range p.Members() {
			if m.Conn == got {
				healthyHits[m.Index]++
			}
		}
	}

	require.Positive(t, healthyHits[0], "member 0 must take a share")
	require.Positive(t, healthyHits[2], "member 2 must take a share")
	require.Zero(t, healthyHits[1])
	require.Equal(t, 60, healthyHits[0]+healthyHits[2],
		"every pick must land on a healthy member")
}

// TestPick_WithNoHealthyMemberStillReturnsAConnection is C4, and it asserts
// which connections came back rather than how many calls returned something.
//
// A pool that refused would invent a failure the transaction classifiers do not
// know how to read, so the fallback sends anyway and lets the RPC fail with the
// transport's own error. But the fallback must still ROTATE, and asserting only
// that each call returned non-nil cannot see that: a fallback pinned to member
// zero satisfies it on every call. Measured -- pinning the fallback to
// current[0] left this test green until it counted distinct connections.
//
// Rotation is load bearing during recovery. Members only return to healthy when
// the probe runs, up to a full interval later, and every call in that window
// takes the fallback: rotating means the connection that recovered first starts
// serving as soon as it is picked, while pinning sends every claim to one
// connection that may be the one still down.
func TestPick_WithNoHealthyMemberStillRotatesAcrossConnections(t *testing.T) {
	p, err := NewPool(testTarget, RoleTx, 3)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.Empty(t, p.HealthyMembers(), "premise: nothing has been probed")

	seen := map[*grpc.ClientConn]int{}
	for i := 0; i < 30; i++ {
		got := p.pick()
		require.NotNil(t, got, "with nothing healthy the pool must still hand back a connection")
		seen[got]++
	}

	require.Len(t, seen, 3,
		"the fallback must rotate across every member, not pin to one: a pinned fallback "+
			"sends every claim to one connection during the whole recovery window")
	for _, m := range p.Members() {
		require.Positive(t, seen[m.Conn], "member %d never took a turn", m.Index)
	}
}

// TestClose_ClosesEveryMemberAndIsIdempotent is C5.
func TestClose_ClosesEveryMemberAndIsIdempotent(t *testing.T) {
	p, err := NewPool(testTarget, RoleTx, 4)
	require.NoError(t, err)

	members := p.Members()
	require.Len(t, members, 4)

	require.NoError(t, p.Close())
	for _, m := range members {
		require.Equal(t, "SHUTDOWN", m.Conn.GetState().String(),
			"member %d was not closed", m.Index)
	}
	require.NoError(t, p.Close(), "Close must be idempotent")
}

// TestGrow_AfterCloseIsRefused is the fourth council finding: a resize running
// on another goroutine must not append a connection past the point where Close
// has already walked the members, because nothing would ever close it.
func TestGrow_AfterCloseIsRefused(t *testing.T) {
	p, err := NewPool(testTarget, RoleTx, 2)
	require.NoError(t, err)
	require.NoError(t, p.Close())

	added, err := p.Grow(6)
	require.Error(t, err)
	require.Empty(t, added)
	require.Equal(t, 2, p.Len(), "a refused grow must not change the pool")
}

// TestPool_ConcurrentPickGrowAndMarkHealth exercises the hot path against both
// writers at once, which is the shape production has: transactions pick while a
// window resize grows and the periodic probe reports health.
//
// The point of the atomic slice is that pick never takes a lock, so the race
// detector is the only thing that can prove the publication is correct -- a
// functional assertion would pass against a plain unsynchronised field on most
// runs.
func TestPool_ConcurrentPickGrowAndMarkHealth(t *testing.T) {
	p, err := NewPool(testTarget, RoleTx, 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	const readers = 8
	done := make(chan struct{})
	var wg sync.WaitGroup

	// Failures are recorded rather than asserted in place: testify's require
	// calls FailNow, which is only valid on the test's own goroutine.
	var nilPicks atomic.Int64
	var growErrs atomic.Int64

	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					if p.pick() == nil {
						nilPicks.Add(1)
					}
					_ = p.HealthyMembers()
					_ = p.Len()
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for want := 3; want <= 12; want++ {
			if _, growErr := p.Grow(want); growErr != nil {
				growErrs.Add(1)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p.MarkHealth(i%12, i%2 == 0)
		}
		// Both writers are done once this returns; stopping the readers here
		// rather than on a timer keeps the test deterministic.
		close(done)
	}()

	wg.Wait()
	require.Zero(t, nilPicks.Load(), "pick returned nil while the pool was being grown")
	require.Zero(t, growErrs.Load())
	require.Equal(t, 12, p.Len())
}
