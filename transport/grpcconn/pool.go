package grpcconn

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
)

// Compile-time proof of the seam this whole type rests on: the cosmos stubs
// take gogoproto's grpc.ClientConn, which is exactly Invoke plus NewStream --
// the same method set as grpc.ClientConnInterface. If either ever grows a
// method, this breaks HERE rather than at the call site that hands a Pool to
// NewServiceClient.
var _ grpc.ClientConnInterface = (*Pool)(nil)

// Pool sizing. There is a floor and NO CEILING, and both are deliberate.
const (
	// DefaultPoolFloor is the smallest useful pool: one connection is one TCP
	// flow, and while it reconnects every claim and proof fails.
	DefaultPoolFloor = 2

	// poolStreamsPerConn is the per-connection target, deliberately below the
	// client ceiling of 100: past that ceiling grpc-go PARKS the caller with no
	// error and no log, so sizing up to the edge is sizing to sit just under a
	// silent failure.
	//
	// The twenty streams it holds back are ALSO what covers the two RPCs that
	// use these connections without a transaction permit -- the periodic probe,
	// one per connection, and the occasional fee lookup. An extra global
	// reservation on top would discount the same headroom twice, and it is not
	// free: it costs one connection at every exact multiple of eighty.
	//
	// This is the ONLY stream constant the code uses. The ceiling of 100 is
	// named in comments to explain where 80 comes from and is never computed
	// with; reintroducing it as a divisor would look like recovering capacity
	// and would actually be removing the margin.
	poolStreamsPerConn = 80
)

// SizeFor is the pool size for a replica responsible for claimed suppliers.
//
// The rule, in the form an operator can be told it: ONE CONNECTION PER EIGHTY
// SUPPLIERS, never fewer than two. That sentence is the specification; the code
// below is only its arithmetic, and it is kept that plain on purpose because it
// has to appear in a capacity document.
//
// It reads the LEASE count and nothing else -- in particular NOT the permit
// cap. Sizing against the configured cap would mean an operator who raises the
// cap has to restart to get the connections, because the pool was already built
// small; sizing against the leases means the pool is always ready for the work
// this replica is responsible for, and the cap only decides how much of it runs
// at once. An operator who caps themselves wastes pool, which is their call.
//
// THERE IS NO UPPER CLAMP, and that is the harder half of the decision. A
// ceiling would have to be calibrated against some largest-expected supplier
// count, and the only such number anyone has is a field observation from one
// day at one height -- recorded as a floor, not a maximum. A constant derived
// from it becomes an arbitrary limit the moment a fleet grows past it, and it
// fails QUIETLY: the clamp truncates and the connections silently carry more
// streams than the target above. The size is instead a straight proportion of
// the suppliers this replica is responsible for, which is a property rather
// than a limit somebody chose. An operator who puts N suppliers on one replica
// pays N/80 connections, at roughly 64KB and three goroutines each, and at any
// N where that matters it is far from their largest cost.
func SizeFor(claimed int) int {
	if claimed < 0 {
		claimed = 0
	}
	size := (claimed + poolStreamsPerConn - 1) / poolStreamsPerConn
	if size < DefaultPoolFloor {
		return DefaultPoolFloor
	}
	return size
}

// Pool is a set of connections to one target, usable anywhere a
// grpc.ClientConnInterface is expected -- which is the same method set the
// cosmos stubs take (gogoproto's grpc.ClientConn is Invoke plus NewStream), so
// a Pool can be handed to NewQueryClient and NewServiceClient in place of a
// single connection and nothing above them changes.
//
// WHAT IT BUYS, and what it does not. The per-connection ceiling of 100
// concurrent HTTP/2 streams is NOT the reason: every broadcast holds one
// transaction permit for its whole call and issues its RPCs sequentially, so
// demand is bounded by the permit count and one connection has room to spare.
// What a second connection buys is that a ClientConn is ONE TCP flow: while it
// is reconnecting, every claim and proof on it fails, inside a window that does
// not wait. That is why the floor is two, and it is the only reason.
//
// Growth only ever APPENDS. A transaction already inside Invoke resolved its
// connection before the call and holds it to completion, so growing under
// traffic cannot disturb it -- and that asymmetry is why there is no shrink
// path: closing a connection with streams in flight would need the same drain
// as Close, to reclaim 64KB and three goroutines.
type Pool struct {
	target Target
	role   Role

	// members is an immutable slice published atomically: readers on the hot
	// path take a snapshot with one atomic load and never hold a lock, and a
	// grow publishes a whole new slice rather than mutating the old one.
	members atomic.Pointer[[]*poolConn]

	// growMu serializes grow and Close against each other. It is never held
	// by Invoke or NewStream.
	growMu sync.Mutex
	closed atomic.Bool

	// next is the round-robin cursor. Wrapping is harmless: it is reduced
	// modulo the member count at every use.
	next atomic.Uint64
}

// poolConn is one member. Its health is per-member and mutable, so members are
// held by pointer -- the slice is immutable, the health bit inside it is not.
type poolConn struct {
	cc    *grpc.ClientConn
	index int

	// healthy starts FALSE, deliberately. A member that has never been probed
	// must not receive a transaction: that is what makes "every member was
	// verified before the window" a property of the structure rather than of
	// somebody remembering to probe. It also keeps the periodic probe off a
	// connection whose warm-up is still running, since the tick walks the
	// healthy set.
	healthy atomic.Bool
}

// Member is a snapshot handle. The index is stable for the life of the pool --
// members are never removed and never reordered, so it is safe to use as a
// bounded-cardinality metric label.
type Member struct {
	Conn  *grpc.ClientConn
	Index int
}

// NewPool dials floor connections to target. Every member starts unhealthy; the
// caller warms them and reports the result with MarkHealth.
func NewPool(target Target, role Role, floor int) (*Pool, error) {
	if floor < 1 {
		return nil, fmt.Errorf("grpcconn: pool floor must be at least 1, got %d", floor)
	}

	p := &Pool{target: target, role: role}
	empty := make([]*poolConn, 0, floor)
	p.members.Store(&empty)

	if _, err := p.Grow(floor); err != nil {
		// Close whatever was dialled before the failure rather than leaking it.
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

// Grow raises the member count to want and returns the members it added. It is LEVEL-TRIGGERED: it compares want against the current
// size rather than reacting to a change, because the block loop that drives it
// coalesces and skips heights under load, so an edge can be missed exactly when
// the load that needs the capacity is present.
//
// A want at or below the current size is a no-op, not an error: there is no
// shrink path.
func (p *Pool) Grow(want int) ([]Member, error) {
	p.growMu.Lock()
	defer p.growMu.Unlock()

	// Checked under growMu, which Close also takes: without this a grow racing
	// a shutdown would append a connection past the point where Close has
	// already walked the members, and nothing would ever close it.
	if p.closed.Load() {
		return nil, fmt.Errorf("grpcconn: pool is closed")
	}

	current := *p.members.Load()
	if want <= len(current) {
		return nil, nil
	}

	grown := make([]*poolConn, len(current), want)
	copy(grown, current)

	added := make([]Member, 0, want-len(current))
	for i := len(current); i < want; i++ {
		cc, err := New(p.target, p.role)
		if err != nil {
			// Publish what was dialled before failing: those connections are
			// real and Close must find them.
			p.members.Store(&grown)
			return added, fmt.Errorf("grpcconn: growing pool to %d: %w", want, err)
		}
		member := &poolConn{cc: cc, index: i}
		grown = append(grown, member)
		added = append(added, Member{Conn: cc, Index: i})
	}

	p.members.Store(&grown)
	return added, nil
}

// Members snapshots every member, healthy or not.
func (p *Pool) Members() []Member {
	current := *p.members.Load()
	out := make([]Member, len(current))
	for i, m := range current {
		out[i] = Member{Conn: m.cc, Index: m.index}
	}
	return out
}

// HealthyMembers snapshots only the members that a probe has confirmed. The
// periodic probe walks this set, which is what keeps it off a member whose
// warm-up has not finished.
func (p *Pool) HealthyMembers() []Member {
	current := *p.members.Load()
	out := make([]Member, 0, len(current))
	for _, m := range current {
		if m.healthy.Load() {
			out = append(out, Member{Conn: m.cc, Index: m.index})
		}
	}
	return out
}

// MarkHealth records whether member index carried an RPC end to end.
//
// "Carried an RPC" and not grpc.ClientConn.GetState(): the failure this exists
// to catch is a middlebox dropping an idle flow, which leaves both ends
// believing the connection is READY. What grpc-go believes is the bug.
func (p *Pool) MarkHealth(index int, ok bool) {
	current := *p.members.Load()
	if index < 0 || index >= len(current) {
		return
	}
	current[index].healthy.Store(ok)
}

// Len is the number of members.
func (p *Pool) Len() int { return len(*p.members.Load()) }

// pick returns the connection for the next call, round-robin over the healthy
// members.
//
// With NO healthy member it falls back to round-robin over all of them, rather
// than returning an error. That is deliberate: a pool that refused to dial
// would invent a failure mode the transaction classifiers do not know, and the
// honest degradation is the behaviour of a single connection -- send, and let
// the RPC fail with the error the chain or the transport actually produced.
func (p *Pool) pick() *grpc.ClientConn {
	current := *p.members.Load()
	if len(current) == 0 {
		return nil
	}

	start := int(p.next.Add(1) % uint64(len(current)))
	for i := 0; i < len(current); i++ {
		candidate := current[(start+i)%len(current)]
		if candidate.healthy.Load() {
			return candidate.cc
		}
	}
	return current[start].cc
}

// Invoke performs a unary RPC on the next connection.
func (p *Pool) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	cc := p.pick()
	if cc == nil {
		return fmt.Errorf("grpcconn: pool has no connections")
	}
	return cc.Invoke(ctx, method, args, reply, opts...)
}

// NewStream begins a streaming RPC on the next connection.
func (p *Pool) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	cc := p.pick()
	if cc == nil {
		return nil, fmt.Errorf("grpcconn: pool has no connections")
	}
	return cc.NewStream(ctx, desc, method, opts...)
}

// Close closes every member. It is idempotent, and it reports the first
// failure while still attempting the rest -- a connection left open because an
// earlier one failed to close is a leak with no upside.
func (p *Pool) Close() error {
	p.growMu.Lock()
	defer p.growMu.Unlock()

	if p.closed.Swap(true) {
		return nil
	}

	var firstErr error
	for _, m := range *p.members.Load() {
		if err := m.cc.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("grpcconn: closing pool connection %d: %w", m.index, err)
		}
	}
	return firstErr
}
