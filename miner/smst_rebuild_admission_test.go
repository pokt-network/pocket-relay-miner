//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/pokt-network/smt"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// heapModel is the process memory an admission reads in a test: objects moves
// with every load and drop, live only when gc runs, and mapped only when the
// test sets it. Its clock starts at a real date and moves by step on every
// read, an hour unless set, so no GC is recent enough to skip a forced one.
// Before any GC its last one reads as the Unix epoch, as debug.ReadGCStats
// reports it.
type heapModel struct {
	mu      sync.Mutex
	objects uint64
	live    uint64
	garbage uint64
	mapped  uint64
	gcs     int
	lastGC  time.Time
	clock   time.Time
	step    time.Duration
}

func (h *heapModel) readObjects() uint64 { h.mu.Lock(); defer h.mu.Unlock(); return h.objects }
func (h *heapModel) readLive() uint64    { h.mu.Lock(); defer h.mu.Unlock(); return h.live }

func (h *heapModel) gc() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.objects -= h.garbage
	h.garbage = 0
	h.live = h.objects
	h.gcs++
	h.startClock()
	h.lastGC = h.clock
}

// startClock sets the clock to a real date the first time. h.mu must be held.
func (h *heapModel) startClock() {
	if h.clock.IsZero() {
		h.clock = time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	}
}

func (h *heapModel) load(n uint64) { h.mu.Lock(); defer h.mu.Unlock(); h.objects += n }

func (h *heapModel) drop(n uint64) { h.mu.Lock(); defer h.mu.Unlock(); h.garbage += n }

func (h *heapModel) gcCount() int { h.mu.Lock(); defer h.mu.Unlock(); return h.gcs }

func (h *heapModel) readMapped() uint64 { h.mu.Lock(); defer h.mu.Unlock(); return h.mapped }

func (h *heapModel) readLastGC() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastGC.IsZero() {
		return time.Unix(0, 0)
	}
	return h.lastGC
}

func (h *heapModel) setMapped(n uint64) { h.mu.Lock(); defer h.mu.Unlock(); h.mapped = n }

func (h *heapModel) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.startClock()
	h.clock = h.clock.Add(d)
}

func (h *heapModel) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.step == 0 {
		h.step = time.Hour
	}
	h.startClock()
	h.clock = h.clock.Add(h.step)
	return h.clock
}

func (h *heapModel) memory(limit uint64) processMemory {
	return processMemory{
		objects: h.readObjects,
		live:    h.readLive,
		mapped:  h.readMapped,
		limit:   func() uint64 { return limit },
		lastGC:  h.readLastGC,
		gc:      h.gc,
		now:     h.now,
	}
}

func (h *heapModel) admission(limit uint64) *RebuildAdmission {
	return newRebuildAdmission(zerolog.Nop(), h.memory(limit))
}

func noRoom() *RebuildAdmission {
	return (&heapModel{}).admission(0)
}

func waitingCount(a *RebuildAdmission) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.waiting)
}

// waitQueued waits until n callers wait in a, reading the queue under its lock.
func waitQueued(t *testing.T, a *RebuildAdmission, n int) {
	t.Helper()
	waitQueuedOr(t, a, n, "")
}

// waitQueuedOr is waitQueued, failing with msg.
func waitQueuedOr(t *testing.T, a *RebuildAdmission, n int, msg string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for waitingCount(a) != n {
		select {
		case <-deadline:
			t.Fatalf("%s: expected %d waiting, have %d", msg, n, waitingCount(a))
		case <-time.After(time.Millisecond):
		}
	}
}

type admittedTree struct {
	loaded, release func()
	err             error
}

func acquireAsync(a *RebuildAdmission, ctx context.Context, estimate, value uint64) <-chan admittedTree {
	out := make(chan admittedTree, 1)
	go func() {
		loaded, release, err := a.acquire(ctx, rebuildKindProof, estimate, value)
		out <- admittedTree{loaded: loaded, release: release, err: err}
	}()
	return out
}

func requireNotAdmitted(t *testing.T, a *RebuildAdmission, got <-chan admittedTree, n int, msg string) {
	t.Helper()
	waitQueuedOr(t, a, n, msg)
	select {
	case <-got:
		t.Fatal(msg)
	default:
	}
}

// requireGCCount waits, bounded, for the model to have run exactly n
// collections, and fails by name if it does not.
//
// Being in the queue does NOT mean the GC has already run. unlockAndDispatch
// enqueues under a.mu, RELEASES it, and only then calls collect
// (smst_rebuild_admission.go:341-350, and its own comment says so), while
// waitQueuedOr returns the moment the waiter is visible in the queue. Reading
// gcCount() on the next line therefore asserts an order the production code
// never promised.
//
// Measured 2026-09-20: one invocation in five of `-race -count=5 ./miner/`
// failed at that read with 0, while 20 isolated runs and 50 more at
// GOMAXPROCS=1 stayed green -- what opens the window is the contention of the
// whole package, not the interleaving of these goroutines alone. Rule #1 puts
// the bar at one failure in a thousand runs, so this is two hundred times over
// it, and "pre-existing" is not an exemption.
//
// The fix waits for the FACT, never for a duration: a Sleep here is what the
// same rule forbids one line above.
func requireGCCount(t *testing.T, h *heapModel, n int, msg string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		switch got := h.gcCount(); {
		case got == n:
			return
		case got > n:
			t.Fatalf("%s: %d collections, expected %d", msg, got, n)
		}
		select {
		case <-deadline:
			t.Fatalf("%s: %d collections after 30s, expected %d", msg, h.gcCount(), n)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRebuildAdmission_ALoadedTreeTheLastGCDidNotSeeKeepsTheNextWaitingUntilItIsDone(t *testing.T) {
	const tree = 100 << 20
	heap := &heapModel{objects: 1 << 30, live: 1 << 30}
	// Room above what the process holds for one tree's load and half a tree kept.
	admission := heap.admission(rebuildHeadroomBytes + 1<<30 + 4*tree)

	aLoaded, aRelease, err := admission.acquire(context.Background(), rebuildKindProof, tree, 3)
	require.NoError(t, err)
	heap.load(tree)
	aLoaded()
	require.Equal(t, uint64(1<<30), heap.readLive(), "premise: no GC has seen A")

	b := acquireAsync(admission, context.Background(), tree, 2)
	requireNotAdmitted(t, admission, b, 1,
		"LINK seq-objects: B was admitted while A is loaded and only fits by the live heap no GC updated")
	requireGCCount(t, heap, 1, "a no is asked again after one GC")

	c := acquireAsync(admission, context.Background(), tree, 1)
	requireNotAdmitted(t, admission, c, 2, "C was admitted ahead of B")
	// KNOWN WEAK, and left as it is on purpose (2026-09-20, item 398). This has
	// the same window as the read above, in the opposite direction: the queue
	// becomes visible before collect runs, so a second GC arriving late would be
	// read here as "still 1" and this line would PASS. It is therefore not a
	// flake -- it is an assertion that cannot fail, which is worse, because it
	// reads as coverage and never says so.
	//
	// It has no fix by waiting: one cannot wait for something not to happen.
	// Closing it needs a design (an event the admission publishes, or a barrier
	// the test can hold), which is more than a line and is not this item's
	// scope. Written here rather than in a queue because this is where whoever
	// touches the assertion will be looking.
	require.Equal(t, 1, heap.gcCount(), "no second GC before anything changes")

	heap.drop(tree)
	aRelease()
	bTree := <-b
	require.NoError(t, bTree.err, "LINK seq-done: B starts once A is done")
	requireNotAdmitted(t, admission, c, 1, "C was admitted while B has not loaded")

	heap.load(tree)
	bTree.loaded()
	requireNotAdmitted(t, admission, c, 1, "LINK seq-objects: C was admitted while B is loaded")
	heap.drop(tree)
	bTree.release()
	cTree := <-c
	require.NoError(t, cTree.err)
	cTree.release()

	admission.mu.Lock()
	defer admission.mu.Unlock()
	require.Zero(t, admission.inFlight)
	require.False(t, admission.loading)
}

func TestRebuildAdmission_GarbageAloneDoesNotKeepATreeWaiting(t *testing.T) {
	const tree = 100 << 20
	heap := &heapModel{objects: 1 << 30, live: 1 << 30}
	admission := heap.admission(rebuildHeadroomBytes + 1<<30 + 3*tree)

	aLoaded, aRelease, err := admission.acquire(context.Background(), rebuildKindProof, tree, 2)
	require.NoError(t, err)
	heap.load(tree)
	heap.load(tree)
	heap.drop(tree) // A's build left as much garbage as the tree it holds
	aLoaded()
	defer aRelease()

	b := acquireAsync(admission, context.Background(), tree/2, 1)
	select {
	case bTree := <-b:
		require.NoError(t, bTree.err)
		bTree.release()
	case <-time.After(30 * time.Second):
		t.Fatal("LINK gc-retry: a tree that fits once the garbage is collected must not wait")
	}
	require.Equal(t, 1, heap.gcCount(), "the objects said no, one GC said yes")
}

func TestRebuildAdmission_ATreeIsBudgetedForWhatItsLoadAllocatesNotWhatItKeeps(t *testing.T) {
	const tree = 100 << 20
	heap := &heapModel{objects: 1 << 30, live: 1 << 30}
	// Room for two trees kept, not for the three and a half a load allocates.
	admission := heap.admission(rebuildHeadroomBytes + 1<<30 + 2*tree)

	loaded, release, err := admission.acquire(context.Background(), rebuildKindProof, 1, 2)
	require.NoError(t, err)
	loaded()
	t.Cleanup(release)

	b := acquireAsync(admission, context.Background(), tree, 1)
	requireNotAdmitted(t, admission, b, 1,
		"LINK admission-transient: a tree whose kept size fits but whose load does not waits for the rebuild in flight")
}

func TestRebuildAdmission_WithTheRuntimeOverItsLimitATreeWaitsForTheOneInFlight(t *testing.T) {
	const limit = 8 << 30
	heap := &heapModel{objects: 1 << 30, live: 1 << 30, mapped: limit + 1}
	admission := heap.admission(limit)

	loaded, release, err := admission.acquire(context.Background(), rebuildKindProof, 1, 2)
	require.NoError(t, err, "with nothing in flight a tree loads over the limit too: no proof is given up for memory")
	loaded()

	b := acquireAsync(admission, context.Background(), 1, 1)
	requireNotAdmitted(t, admission, b, 1, "LINK admission-overage: with the runtime over its limit a tree waits for the one in flight")
	require.Zero(t, heap.gcCount(), "no GC is forced: collecting does not bring the runtime under its limit")

	heap.setMapped(limit / 2)
	release()
	got := <-b
	require.NoError(t, got.err, "the rebuild in flight ends and the tree loads")
	got.release()
}

func TestRebuildAdmission_WithTheRuntimeOverItsLimitACompactionWaitsEvenWithNothingInFlight(t *testing.T) {
	const limit = 8 << 30
	heap := &heapModel{objects: 1 << 30, live: 1 << 30, mapped: limit + 1}
	admission := heap.admission(limit)

	compaction := make(chan error, 1)
	go func() {
		_, release, err := admission.acquire(context.Background(), rebuildKindCompaction, 1, 0)
		if err == nil {
			release()
		}
		compaction <- err
	}()
	waitQueuedOr(t, admission, 1, "LINK admission-overage-compaction: a compaction does not load with the runtime over its limit, even with nothing in flight")
	select {
	case <-compaction:
		t.Fatal("LINK admission-overage-compaction: a compaction does not load with the runtime over its limit, even with nothing in flight")
	default:
	}

	_, release, err := admission.acquire(context.Background(), rebuildKindProof, 1, 1)
	require.NoError(t, err, "a proof with nothing in flight loads over the limit: no proof is given up for memory")
	release()
	waitQueuedOr(t, admission, 1, "the compaction still waits once the proof is done")

	heap.setMapped(limit / 2)
	admission.evaluateMemoryBrake()
	select {
	case err := <-compaction:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("LINK admission-overage-wake: back within its limit, the brake's evaluation admits the waiting compaction")
	}
}

func TestRebuildAdmission_ObservesTheHeapALoadGrewOverItsEstimate(t *testing.T) {
	heap := &heapModel{objects: 1 << 30, live: 1 << 30}
	admission := heap.admission(1 << 40)
	count, sum := heapGrowthOverEstimate(t, rebuildKindCompaction)

	loaded, release, err := admission.acquire(context.Background(), rebuildKindCompaction, 100<<20, 0)
	require.NoError(t, err)
	heap.load(350 << 20)
	loaded()
	loaded()
	release()

	gotCount, gotSum := heapGrowthOverEstimate(t, rebuildKindCompaction)
	require.Equal(t, count+1, gotCount, "LINK admission-growth: a load is observed once")
	require.InDelta(t, 3.5, gotSum-sum, 1e-9, "LINK admission-growth: as the heap's growth over its estimate")
}

// heapGrowthOverEstimate reads the count and sum of the heap growth histogram for kind.
func heapGrowthOverEstimate(t *testing.T, kind string) (uint64, float64) {
	t.Helper()
	var m dto.Metric
	require.NoError(t, observability.SMSTRebuildHeapGrowthOverEstimate.WithLabelValues(kind).(interface{ Write(*dto.Metric) error }).Write(&m))
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

func TestRebuildAdmission_AForcedGCDoesNotHoldTheAdmissionLocked(t *testing.T) {
	const tree = 100 << 20
	heap := &heapModel{objects: 1 << 30, live: 1 << 30}
	memory := heap.memory(rebuildHeadroomBytes + 1<<30 + tree/2)
	inGC, finishGC := make(chan struct{}), make(chan struct{})
	var finish sync.Once
	memory.gc = func() {
		close(inGC)
		<-finishGC
		heap.gc()
	}
	admission := newRebuildAdmission(zerolog.Nop(), memory)

	loaded, release, err := admission.acquire(context.Background(), rebuildKindProof, tree, 2)
	require.NoError(t, err)
	loaded()
	// Cleanups run last first: the GC ends before the tree is released, so a
	// failure does not leave release waiting on a GC that never ends.
	t.Cleanup(release)
	t.Cleanup(func() { finish.Do(func() { close(finishGC) }) })
	waiter := acquireAsync(admission, context.Background(), tree, 1)
	<-inGC

	answered := make(chan struct{})
	go func() {
		admission.IngestionPause("pokt1any").Paused()
		admission.oldestProofWait()
		close(answered)
	}()
	select {
	case <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("LINK gc-unlocked: the pause and the oldest wait gauge must answer while a forced GC runs")
	}
	finish.Do(func() { close(finishGC) })
	deadline := time.After(30 * time.Second)
	for collecting := true; collecting; {
		admission.mu.Lock()
		collecting = admission.collecting
		admission.mu.Unlock()
		select {
		case <-deadline:
			t.Fatal("the admission never saw its GC end")
		case <-time.After(time.Millisecond):
		}
	}
	requireNotAdmitted(t, admission, waiter, 1, "the tree still does not fit after the GC")
	require.Equal(t, 1, heap.gcCount())
}

func TestRebuildAdmission_AGCIsForcedOnlyWhenNoneEndedWithinTheWindow(t *testing.T) {
	heap := &heapModel{step: time.Millisecond}
	admission := heap.admission(1 << 40)
	before := testutil.ToFloat64(forcedGCs.WithLabelValues(gcReasonRebuildAdmission))

	admission.collect(gcReasonRebuildAdmission)
	require.Equal(t, 1, heap.gcCount(), "with no GC yet one is forced")
	admission.collect(gcReasonRebuildAdmission)
	require.Equal(t, 1, heap.gcCount(), "LINK gc-recent: the GC just forced stands in for another")

	heap.advance(recentGCWindow)
	admission.collect(gcReasonRebuildAdmission)
	require.Equal(t, 2, heap.gcCount(), "LINK gc-window: past the window a GC is forced again")

	heap.advance(recentGCWindow)
	heap.gc() // the runtime collects on its own
	heap.advance(recentGCWindow - time.Second)
	admission.collect(gcReasonRebuildAdmission)
	require.Equal(t, 3, heap.gcCount(), "LINK gc-recent: a GC the runtime ended within the window stands in for a forced one")
	require.Equal(t, before+2, testutil.ToFloat64(forcedGCs.WithLabelValues(gcReasonRebuildAdmission)), "forced GCs are counted, the runtime's are not")
}

func TestRebuildAdmission_TheNextTreeIsNotAskedWhileOneIsLoading(t *testing.T) {
	heap := &heapModel{}
	admission := heap.admission(1 << 40)

	aLoaded, aRelease, err := admission.acquire(context.Background(), rebuildKindProof, 1, 2)
	require.NoError(t, err)
	b := acquireAsync(admission, context.Background(), 1, 1)
	requireNotAdmitted(t, admission, b, 1, "LINK seq-loading: B was asked while A was still loading")

	aLoaded()
	bTree := <-b
	require.NoError(t, bTree.err, "B starts once A is loaded, with A still in flight")
	admission.mu.Lock()
	require.Equal(t, 2, admission.inFlight, "both trees fit: A proves while B loads")
	admission.mu.Unlock()
	bTree.release()
	aRelease()
}

func TestRebuildAdmission_WithNothingInFlightATreeThatDoesNotFitStillLoads(t *testing.T) {
	admission := noRoom()
	got := acquireAsync(admission, context.Background(), 1<<40, 1)
	select {
	case tree := <-got:
		require.NoError(t, tree.err)
		tree.release()
	case <-time.After(30 * time.Second):
		t.Fatal("LINK always-one: with nothing in flight a tree must load whatever it weighs")
	}
}

func TestRebuildAdmission_AWaiterWhoseContextEndsLeavesWithoutLoading(t *testing.T) {
	admission := noRoom()
	loaded, release, err := admission.acquire(context.Background(), rebuildKindProof, 100, 1)
	require.NoError(t, err)
	loaded()

	ctx, cancel := context.WithCancel(context.Background())
	waiter := acquireAsync(admission, ctx, 200, 2)
	requireNotAdmitted(t, admission, waiter, 1, "premise: the second waits")
	require.True(t, admission.IngestionPause("pokt1any").Paused())
	cancel()
	got := <-waiter
	require.ErrorIs(t, got.err, context.Canceled)
	require.Nil(t, got.release, "nothing to release: it never loaded")
	require.Zero(t, waitingCount(admission), "LINK cancel-queue: a cancelled waiter leaves the queue")
	require.False(t, admission.IngestionPause("pokt1any").Paused(), "and no longer holds ingestion")

	release()
	release()
	admission.mu.Lock()
	require.Zero(t, admission.inFlight, "released once, whatever the calls")
	admission.mu.Unlock()

	_, again, err := admission.acquire(context.Background(), rebuildKindProof, 300, 3)
	require.NoError(t, err, "the next one is admitted")
	again()
}

func TestRebuildAdmission_TheOldestProofWaitIsReportedAndClears(t *testing.T) {
	admission := noRoom()
	require.Zero(t, admission.oldestProofWait())
	loaded, release, err := admission.acquire(context.Background(), rebuildKindProof, 1, 1)
	require.NoError(t, err)
	loaded()
	waiter := acquireAsync(admission, context.Background(), 1, 2)
	waitQueued(t, admission, 1)
	require.Positive(t, admission.oldestProofWait(), "LINK oldest-wait: a waiting proof reports how long it waited")
	release()
	got := <-waiter
	require.NoError(t, got.err)
	got.release()
	require.Zero(t, admission.oldestProofWait())
}

// compactedTree claims a tree of n relays, every one weighing weight, and
// compacts it; it returns the claimed root.
func compactedTree(t *testing.T, ctx context.Context, client *redisutil.Client, supplier, sessionID string, n int, weight uint64) []byte {
	t.Helper()
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	relays := coldRelays(weight, n)
	for i := range relays {
		relays[i].weight = weight
	}
	root := claimColdTree(t, ctx, mgr, sessionID, relays)
	result, err := mgr.CompactColdTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, coldCompacted, result)
	return root
}

// admissionGate records what an admission lets in and holds each admitted
// caller until the test lets it go.
type admissionGate struct {
	mu       sync.Mutex
	admitted []string
	inFlight []int
	arrived  chan struct{}
	proceed  chan struct{}
}

func newAdmissionGate() *admissionGate {
	return &admissionGate{arrived: make(chan struct{}, 16), proceed: make(chan struct{}, 16)}
}

func (g *admissionGate) hook(a *RebuildAdmission) func(kind string, value uint64) {
	return func(kind string, value uint64) {
		a.mu.Lock()
		inFlight := a.inFlight
		a.mu.Unlock()
		g.mu.Lock()
		g.admitted = append(g.admitted, fmt.Sprintf("%s:%d", kind, value))
		g.inFlight = append(g.inFlight, inFlight)
		g.mu.Unlock()
		g.arrived <- struct{}{}
		<-g.proceed
	}
}

func (g *admissionGate) snapshot() ([]string, []int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.admitted...), append([]int(nil), g.inFlight...)
}

func (g *admissionGate) waitArrival(t *testing.T) {
	t.Helper()
	select {
	case <-g.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("no tree was admitted")
	}
}

func TestRebuildAdmission_ProofsLoadByValueWithTheCompactionLastAndAllVerify(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier = "pokt1admission_order"
	const leaves = 300
	low := compactedTree(t, ctx, client, supplier, "sess-low", leaves, 1)
	high := compactedTree(t, ctx, client, supplier, "sess-high", leaves, 9)
	first := compactedTree(t, ctx, client, supplier, "sess-first", leaves, 5)

	admission := noRoom()
	gate := newAdmissionGate()
	admission.admitted = gate.hook(admission)
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier, CacheTTL: time.Hour, RebuildAdmission: admission,
	})

	path := coldPaths(3, 1)[0]
	type proved struct {
		root    []byte
		proofBz []byte
		err     error
	}
	results := make(chan proved, 3)
	prove := func(sessionID string, root []byte) {
		proofBz, err := mgr.ProveClosest(ctx, sessionID, path)
		results <- proved{root: root, proofBz: proofBz, err: err}
	}
	go prove("sess-first", first)
	gate.waitArrival(t)

	// A compaction arrives before both proofs and still goes last.
	compactMgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier, CacheTTL: time.Hour, RebuildAdmission: admission,
	})
	claimColdTree(t, ctx, compactMgr, "sess-compact", coldRelays(77, leaves))
	compacted := make(chan error, 1)
	go func() {
		_, err := compactMgr.CompactColdTree(ctx, "sess-compact")
		compacted <- err
	}()
	waitQueued(t, admission, 1)
	require.False(t, admission.IngestionPause(supplier).Paused(), "a waiting compaction does not hold ingestion")

	go prove("sess-low", low)
	waitQueued(t, admission, 2)
	require.True(t, admission.IngestionPause(supplier).Paused(), "LINK admission-pause: a proof waiting for memory holds ingestion")
	go prove("sess-high", high)
	waitQueued(t, admission, 3)

	for range 3 {
		gate.proceed <- struct{}{}
		gate.waitArrival(t)
	}
	gate.proceed <- struct{}{}
	for range 3 {
		r := <-results
		require.NoError(t, r.err)
		ok, _ := chainVerifies(t, r.proofBz, r.root)
		require.True(t, ok, "every admitted proof verifies")
	}
	require.NoError(t, <-compacted)

	lowSum, _ := smt.MerkleSumRoot(low).Sum()
	highSum, _ := smt.MerkleSumRoot(high).Sum()
	firstSum, _ := smt.MerkleSumRoot(first).Sum()
	admitted, inFlight := gate.snapshot()
	require.Equal(t, []string{
		fmt.Sprintf("proof:%d", firstSum),
		fmt.Sprintf("proof:%d", highSum),
		fmt.Sprintf("proof:%d", lowSum),
		"compaction:0",
	}, admitted, "LINK admission-order: the proof claiming the most goes first, and the compaction after every proof")
	require.Equal(t, []int{1, 1, 1, 1}, inFlight, "with no room, one tree at a time")
	require.False(t, admission.IngestionPause(supplier).Paused())
}

func TestRebuildAdmission_WithNoRoomAtAllEveryProofStillVerifies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, _ := newTestRedis(t)
	const supplier = "pokt1admission_zero"
	const trees = 4
	roots := make([][]byte, trees)
	for i := range roots {
		roots[i] = compactedTree(t, ctx, client, supplier, fmt.Sprintf("sess-zero-%d", i), 200, uint64(i+1))
	}
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier, CacheTTL: time.Hour, RebuildAdmission: noRoom(),
	})

	path := coldPaths(4, 1)[0]
	type proved struct {
		index   int
		proofBz []byte
		err     error
	}
	results := make(chan proved, trees)
	for i := range trees {
		go func() {
			proofBz, err := mgr.ProveClosest(ctx, fmt.Sprintf("sess-zero-%d", i), path)
			results <- proved{index: i, proofBz: proofBz, err: err}
		}()
	}
	for range trees {
		r := <-results
		require.NoError(t, r.err, "LINK always-one-proof: no proof is given up for memory")
		ok, _ := chainVerifies(t, r.proofBz, roots[r.index])
		require.True(t, ok)
	}
}

// readRecorder stores the highest entry ID an XREADGROUP returned from stream.
type readRecorder struct {
	stream string
	read   *atomic.Pointer[streamMsgID]
}

func (r readRecorder) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (r readRecorder) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		if read, ok := cmd.(*goredis.XStreamSliceCmd); ok && cmd.Name() == "xreadgroup" {
			for _, stream := range read.Val() {
				for _, msg := range stream.Messages {
					if id, parseErr := parseStreamMsgID(msg.ID); parseErr == nil && stream.Stream == r.stream {
						r.read.Store(&id)
					}
				}
			}
		}
		return err
	}
}

func (r readRecorder) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

// panicOnGet panics inside go-redis when one key is read with GET.
type panicOnGet struct{ key string }

func (p panicOnGet) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (p panicOnGet) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if cmd.Name() == "get" && len(cmd.Args()) > 1 && cmd.Args()[1] == p.key {
			panic("injected panic reading the leaves blob")
		}
		return next(ctx, cmd)
	}
}

func (p panicOnGet) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func TestRebuildAdmission_EveryWayALoadEndsLeavesNothingInFlight(t *testing.T) {
	ctx := context.Background()
	client, prefix := newTestRedis(t)
	const supplier = "pokt1admission_paths"
	kb := client.KB()
	root := compactedTree(t, ctx, client, supplier, "sess-ok", 50, 1)
	compactedTree(t, ctx, client, supplier, "sess-corrupt", 50, 1)
	compactedTree(t, ctx, client, supplier, "sess-panic", 50, 1)
	blob, err := client.Get(ctx, kb.SMSTLeavesKey(supplier, "sess-corrupt")).Bytes()
	require.NoError(t, err)
	blob[len(blob)-1] ^= 0xff
	require.NoError(t, client.Set(ctx, kb.SMSTLeavesKey(supplier, "sess-corrupt"), blob, time.Hour).Err())

	panicking := sameNamespaceClient(t, prefix)
	panicking.AddHook(panicOnGet{key: kb.SMSTLeavesKey(supplier, "sess-panic")})

	admission := noRoom()
	var admitted atomic.Int32
	admission.admitted = func(string, uint64) { admitted.Add(1) }
	config := RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour, RebuildAdmission: admission}
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, config)
	panicMgr := NewRedisSMSTManager(zerolog.Nop(), panicking, config)
	path := coldPaths(1, 1)[0]
	requireNothingInFlight := func(way string) {
		t.Helper()
		admission.mu.Lock()
		defer admission.mu.Unlock()
		require.Zero(t, admission.inFlight, "LINK paths-release: a load that %s released its turn", way)
		require.False(t, admission.loading, "LINK paths-release: a load that %s is not left loading", way)
	}

	proofBz, err := mgr.ProveClosest(ctx, "sess-ok", path)
	require.NoError(t, err, "control: a sound blob proves")
	ok, _ := chainVerifies(t, proofBz, root)
	require.True(t, ok)
	requireNothingInFlight("proved")

	_, err = mgr.ProveClosest(ctx, "sess-corrupt", path)
	require.Error(t, err, "control: a corrupt blob does not prove")
	requireNothingInFlight("failed")

	require.Panics(t, func() { _, _ = panicMgr.ProveClosest(ctx, "sess-panic", path) }, "control: the injected panic fires")
	requireNothingInFlight("panicked")

	holdCtx, holdCancel := context.WithTimeout(ctx, 10*time.Second)
	defer holdCancel()
	_, holdRelease, err := admission.acquire(holdCtx, rebuildKindProof, 1, 1)
	require.NoError(t, err, "LINK paths-release: with the loads above ended, nothing in flight blocks the next")
	cancelled, cancel := context.WithCancel(ctx)
	waited := make(chan error, 1)
	go func() {
		_, err := mgr.ProveClosest(cancelled, "sess-ok", path)
		waited <- err
	}()
	waitQueued(t, admission, 1)
	cancel()
	require.ErrorIs(t, <-waited, context.Canceled, "control: a cancelled wait returns its context's error")
	holdRelease()

	require.Equal(t, int32(4), admitted.Load(), "control: ok, corrupt, panic and the hold were admitted; the cancelled wait was not")
	admission.mu.Lock()
	defer admission.mu.Unlock()
	require.Zero(t, admission.inFlight, "LINK paths-release: every load that ended released its turn")
	require.False(t, admission.loading, "LINK paths-release: no load is left marked loading")
	require.Empty(t, admission.waiting)
}

func TestColdRebuildEstimate_ReadsTheDecodedSizeAndCountFromTheHeader(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	kb := client.KB()
	for _, tc := range []struct {
		name      string
		extraSize int
	}{
		{name: "700B", extraSize: 0},
		{name: "8KB", extraSize: 8 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supplier := "pokt1estimate_" + tc.name
			const sessionID, n = "sess-estimate", 400
			mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
			relays := coldRelays(11, n)
			for i := range relays {
				relays[i].value = append(relays[i].value, make([]byte, tc.extraSize)...)
			}
			claimColdTree(t, ctx, mgr, sessionID, relays)
			_, err := mgr.CompactColdTree(ctx, sessionID)
			require.NoError(t, err)

			blob, err := client.Get(ctx, kb.SMSTLeavesKey(supplier, sessionID)).Bytes()
			require.NoError(t, err)
			_, dec, err := coldCodec()
			require.NoError(t, err)
			raw, err := dec.DecodeAll(blob[coldLeavesHeaderLen:], nil)
			require.NoError(t, err)

			estimate, err := mgr.coldRebuildEstimate(ctx, sessionID)
			require.NoError(t, err)
			require.Equal(t, 2*uint64(len(raw))+n*rebuildLeafOverheadBytes, estimate,
				"LINK estimate-fcs: the estimate counts what the frame decodes to, from the header alone")
			require.Greater(t, len(raw), n*(700+tc.extraSize), "control: the decoded frame carries every value")

			missing, err := mgr.coldRebuildEstimate(ctx, "sess-absent")
			require.NoError(t, err)
			require.Zero(t, missing, "a missing blob estimates zero")
		})
	}
}

func TestAwaitFlushWatermark_ASupplierWhoseClaimWaitsReadsWhileProofsHoldEveryOtherSupplier(t *testing.T) {
	client, _ := newTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const supplier, other, serviceID = "pokt1flush_reads", "pokt1flush_held", "svc-flush-pause"

	consumer, err := redisutil.NewStreamsConsumer(zerolog.Nop(), client, transport.ConsumerConfig{
		StreamPrefix: client.KB().StreamPrefix(), SupplierOperatorAddress: supplier,
		ConsumerGroup: client.KB().ConsumerGroup(), ConsumerName: "flush", BatchSize: 10, ClaimIdleTimeout: 60000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = consumer.Close() })
	require.NoError(t, client.XAdd(ctx, &goredis.XAddArgs{Stream: consumer.StreamName(), Values: map[string]any{"data": "relay"}}).Err())

	admission := noRoom()
	_, holdRelease, err := admission.acquire(ctx, rebuildKindProof, 1, 1)
	require.NoError(t, err)
	defer holdRelease()
	waiter := acquireAsync(admission, ctx, 1, 2)
	requireNotAdmitted(t, admission, waiter, 1, "premise: a proof waits for memory")
	consumer.SetIngestionPause(admission.IngestionPause(supplier))
	require.True(t, admission.IngestionPause(supplier).Paused(), "premise: ingestion is held")

	// What the consumer read, seen on the wire: the entry is not a relay the
	// worker could handle, and reading it is the whole point.
	var handled atomic.Pointer[streamMsgID]
	client.AddHook(readRecorder{stream: consumer.StreamName(), read: &handled})
	messages := consumer.Consume(ctx)
	go func() {
		for range messages {
		}
	}()

	var otherHeldThroughout, flushSupplierReleased atomic.Bool
	otherHeldThroughout.Store(true)
	bc := &mockBlockClient{currentHeight: 100}
	m := newFlushDelayManager(bc,
		func() (streamMsgID, bool) {
			if !admission.IngestionPause(other).Paused() {
				otherHeldThroughout.Store(false)
			}
			if !admission.IngestionPause(supplier).Paused() {
				flushSupplierReleased.Store(true)
			}
			if id := handled.Load(); id != nil {
				return *id, true
			}
			return streamMsgID{}, false
		},
		func(ctx context.Context) (streamMsgID, bool, error) {
			id, err := consumer.LastGeneratedID(ctx)
			if err != nil || id == "" || id == "0-0" {
				return streamMsgID{}, false, err
			}
			parsed, err := parseStreamMsgID(id)
			return parsed, err == nil, err
		},
	)
	m.SetClaimFlushWaiting(func() func() { return admission.claimFlushWaiting(supplier) })
	capped := testutil.ToFloat64(claimFlushCapped.WithLabelValues(serviceID))

	proceed := m.awaitFlushWatermark(ctx, sessionsWithServiceID(serviceID), 100)

	require.True(t, proceed, "LINK flush-reads: the claim's wait ends because the held supplier read its stream")
	require.Equal(t, capped, testutil.ToFloat64(claimFlushCapped.WithLabelValues(serviceID)), "the claim did not seal at its cap")
	require.True(t, flushSupplierReleased.Load(), "control: the waiting claim's supplier was released")
	require.True(t, otherHeldThroughout.Load(), "LINK flush-only-its-own: another supplier stays held while the claim waits")
	require.True(t, admission.IngestionPause(supplier).Paused(), "the hold returns once the claim's wait ends")
}

func TestAddSupplierWithData_WiresTheProcessAdmissionIntoTheConsumerAndTheSMSTManager(t *testing.T) {
	redisClient, _ := newTestRedis(t)
	pool := pond.NewPool(8)
	defer pool.StopAndWait()
	mgr := NewSupplierManager(zerolog.Nop(), nil, nil, SupplierManagerConfig{
		RedisClient: redisClient, MinerID: "test-miner-admission", WorkerPool: pool,
		ConsumerName: "admission-wiring", BatchSize: 10, ClaimIdleTimeout: time.Minute,
	})
	require.NotNil(t, mgr.rebuildAdmission, "LINK wiring-built: the process has a rebuild admission")
	t.Cleanup(func() { _ = mgr.Close() })

	const supplier = "pokt1admission_wiring"
	require.NoError(t, mgr.addSupplierWithData(context.Background(), supplier, nil))
	state, ok := mgr.suppliers.Load(supplier)
	require.True(t, ok)
	require.Same(t, mgr.rebuildAdmission, state.SMSTManager.config.RebuildAdmission,
		"LINK wiring-smst: the supplier's SMST manager loads trees through the process admission")
	require.Equal(t, redisutil.IngestionPause(mgr.rebuildAdmission.IngestionPause(supplier)), state.Consumer.IngestionPauseForTest(),
		"LINK wiring-consumer: the supplier's consumer is held by its own view of the process admission")

	// The running consumer is not wired twice: an idle one takes the call.
	idle, err := redisutil.NewStreamsConsumer(zerolog.Nop(), redisClient, transport.ConsumerConfig{
		StreamPrefix: redisClient.KB().StreamPrefix(), SupplierOperatorAddress: supplier,
		ConsumerGroup: redisClient.KB().ConsumerGroup(), ConsumerName: "idle", BatchSize: 10, ClaimIdleTimeout: 60000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = idle.Close() })
	lm := &SessionLifecycleManager{}
	mgr.wireRebuildAdmission(supplier, idle, lm)
	require.NotNil(t, lm.claimFlushWaiting, "LINK wiring-flush: the claim's flush delay can release its supplier")
	done := lm.claimFlushWaiting()
	mgr.rebuildAdmission.mu.Lock()
	require.Equal(t, 1, mgr.rebuildAdmission.flushes[supplier], "the flush is recorded against its own supplier")
	mgr.rebuildAdmission.mu.Unlock()
	done()
	mgr.rebuildAdmission.mu.Lock()
	defer mgr.rebuildAdmission.mu.Unlock()
	require.NotContains(t, mgr.rebuildAdmission.flushes, supplier, "and cleared when it ends")
}
