package miner

// Rebuilding a compacted tree holds all of it in the heap: every leaf value
// twice (the decoded frame, and the leaf node that copies it) and every inner
// node. A cohort of sessions reaches its proof window at the same height, so
// without a bound the process rebuilds as many trees at once as it has rebuild
// workers, whatever they weigh.
//
// RebuildAdmission loads one tree at a time. The next is asked only once the
// previous one is loaded, so its memory is already in the process when the
// question is asked: does this tree's estimate fit between the heap and
// GOMEMLIMIT, less rebuildHeadroomBytes? The first answer reads the heap's
// objects, which every allocation updates and which include garbage: a yes
// never misses the tree loaded last. On a no, a GC runs and the question is
// asked again of the live heap that GC just measured, so garbage alone does
// not keep a tree waiting. If it still does not fit, the tree waits for a
// rebuild in flight to end. With nothing in flight it is admitted whatever it
// weighs, so no proof is ever given up for memory.
//
// Proofs go first, the one that claims the most compute units first; a
// compaction starts only when no proof waits. While a proof waits, the stream
// consumers stop reading, so ingestion does not grow the heap the proof is
// waiting for -- except a supplier's whose claim is waiting to drain its
// stream, since that claim seals at its height cap with or without the relays.

import (
	"context"
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	// rebuildHeadroomBytes is what admission leaves free under GOMEMLIMIT.
	rebuildHeadroomBytes = 512 << 20

	// rebuildLeafOverheadBytes is the heap a rebuilt leaf holds besides its
	// value's two copies: its inner nodes, map entries and slice headers.
	// Derived from a synthetic measurement (~2.1 KB per leaf with 700 B
	// values, 1k-100k leaves) less those copies; rounded up.
	rebuildLeafOverheadBytes = 1 << 10

	// compactionLeafEstimateBytes is what a compaction holds per leaf before
	// its size is known: the leaves read from the hash, the encoded frame, and
	// the rebuild that verifies it. Not calibrated for large relay values.
	compactionLeafEstimateBytes = 4 << 10
)

// Admission kinds, used as a metric label.
const (
	rebuildKindProof      = "proof"
	rebuildKindCompaction = "compaction"
)

// processRebuildAdmission is the process's admission, read by the oldest
// proof wait gauge.
var processRebuildAdmission atomic.Pointer[RebuildAdmission]

var _ = observability.MinerFactory.NewGaugeFunc(
	prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Subsystem: "smst",
		Name:      "rebuild_oldest_proof_wait_seconds",
		Help:      "How long the proof that has waited longest for memory to rebuild its compacted SMST has waited; 0 when none waits",
	},
	func() float64 { return processRebuildAdmission.Load().oldestProofWait().Seconds() },
)

// RebuildAdmission is shared by every supplier's SMST manager in a process. A
// nil *RebuildAdmission admits everything at once.
type RebuildAdmission struct {
	logger logging.Logger
	// objects reads the heap's objects, live and dead; live the heap the last
	// GC marked; limit GOMEMLIMIT; gc runs a collection.
	objects func() uint64
	live    func() uint64
	limit   func() uint64
	gc      func()
	// admitted, when set, runs on the admitted caller's goroutine before
	// acquire returns. Tests only.
	admitted func(kind string, value uint64)

	mu       sync.Mutex
	loading  bool
	inFlight int
	// collected is set once a GC ran for a tree that did not fit, and cleared
	// when a tree loads or a rebuild ends: until then another GC finds
	// nothing new to free.
	collected bool
	waiting   []*rebuildWaiter
	seq       uint64
	proofs    int
	flushes   map[string]int
	changed   chan struct{}
}

type rebuildWaiter struct {
	kind     string
	estimate uint64
	value    uint64
	seq      uint64
	since    time.Time
	ready    chan struct{}
	granted  bool
}

// NewRebuildAdmission reads the runtime's live heap and memory limit, and
// becomes the one the process's metrics report. Without GOMEMLIMIT every tree
// fits, and trees still load one at a time.
func NewRebuildAdmission(logger logging.Logger) *RebuildAdmission {
	a := newRebuildAdmission(logger, runtimeHeapObjects, runtimeHeapLive, runtimeMemoryLimit, runtime.GC)
	if runtimeMemoryLimit() == math.MaxInt64 {
		a.logger.Warn().Msg("GOMEMLIMIT is not set: rebuilds of compacted SMSTs are not bounded by memory")
	}
	processRebuildAdmission.Store(a)
	return a
}

func newRebuildAdmission(logger logging.Logger, objects, live, limit func() uint64, gc func()) *RebuildAdmission {
	return &RebuildAdmission{
		logger:  logging.ForComponent(logger, "smst_rebuild_admission"),
		objects: objects,
		live:    live,
		limit:   limit,
		gc:      gc,
		flushes: make(map[string]int),
		changed: make(chan struct{}),
	}
}

func runtimeHeapObjects() uint64 { return readRuntimeBytes("/memory/classes/heap/objects:bytes") }

func runtimeHeapLive() uint64 { return readRuntimeBytes("/gc/heap/live:bytes") }

func readRuntimeBytes(name string) uint64 {
	sample := []metrics.Sample{{Name: name}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return sample[0].Value.Uint64()
}

func runtimeMemoryLimit() uint64 {
	return uint64(debug.SetMemoryLimit(-1))
}

// acquire waits until a tree of estimate bytes may load. It returns loaded,
// which the caller runs once the tree is in memory so the next one may be
// asked, and release, which the caller runs when the tree is dropped. Both are
// safe to run more than once, and release also ends a load that never
// finished. Proofs are ordered by value, highest first; compactions come after
// every proof. If ctx ends first it returns ctx's error and nothing to release.
func (a *RebuildAdmission) acquire(ctx context.Context, kind string, estimate, value uint64) (loaded, release func(), err error) {
	if a == nil {
		return func() {}, func() {}, nil
	}
	w := &rebuildWaiter{kind: kind, estimate: estimate, value: value, since: time.Now(), ready: make(chan struct{})}

	a.mu.Lock()
	a.seq++
	w.seq = a.seq
	a.enqueue(w)
	a.dispatch()
	a.mu.Unlock()

	select {
	case <-w.ready:
	case <-ctx.Done():
		a.mu.Lock()
		granted := w.granted
		if !granted {
			a.remove(w)
			a.dispatch()
		}
		a.mu.Unlock()
		if granted {
			_, release := a.slot()
			release()
		}
		return nil, nil, ctx.Err()
	}

	observability.SMSTRebuildWaitSeconds.WithLabelValues(kind).Observe(time.Since(w.since).Seconds())
	if a.admitted != nil {
		a.admitted(kind, value)
	}
	loaded, release = a.slot()
	return loaded, release, nil
}

// slot returns the loaded and release functions of one admitted tree.
func (a *RebuildAdmission) slot() (loaded, release func()) {
	var mu sync.Mutex
	isLoaded, isReleased := false, false
	loaded = func() {
		mu.Lock()
		defer mu.Unlock()
		if isLoaded || isReleased {
			return
		}
		isLoaded = true
		a.mu.Lock()
		a.loading = false
		a.collected = false
		a.dispatch()
		a.mu.Unlock()
	}
	release = func() {
		mu.Lock()
		defer mu.Unlock()
		if isReleased {
			return
		}
		isReleased = true
		a.mu.Lock()
		if !isLoaded {
			a.loading = false
		}
		a.inFlight--
		a.collected = false
		a.dispatch()
		a.mu.Unlock()
	}
	return loaded, release
}

// enqueue places w by priority: proofs before compactions, a proof by value
// descending, ties and compactions in arrival order.
func (a *RebuildAdmission) enqueue(w *rebuildWaiter) {
	i := len(a.waiting)
	for j, queued := range a.waiting {
		if admitsBefore(w, queued) {
			i = j
			break
		}
	}
	a.waiting = slices.Insert(a.waiting, i, w)
	if w.kind == rebuildKindProof {
		a.proofs++
		a.signal()
	}
	observability.SMSTRebuildWaiting.WithLabelValues(w.kind).Inc()
}

// admitsBefore reports whether x goes ahead of y.
func admitsBefore(x, y *rebuildWaiter) bool {
	if (x.kind == rebuildKindProof) != (y.kind == rebuildKindProof) {
		return x.kind == rebuildKindProof
	}
	if x.kind == rebuildKindProof && x.value != y.value {
		return x.value > y.value
	}
	return x.seq < y.seq
}

func (a *RebuildAdmission) remove(w *rebuildWaiter) {
	i := slices.Index(a.waiting, w)
	if i < 0 {
		return
	}
	a.waiting = slices.Delete(a.waiting, i, i+1)
	if w.kind == rebuildKindProof {
		a.proofs--
		a.signal()
	}
	observability.SMSTRebuildWaiting.WithLabelValues(w.kind).Dec()
}

// dispatch admits the head of the queue when no tree is loading and it fits,
// or when nothing is in flight. The head blocks the rest: a smaller tree
// behind it does not jump ahead.
func (a *RebuildAdmission) dispatch() {
	if a.loading || len(a.waiting) == 0 {
		return
	}
	w := a.waiting[0]
	if a.inFlight > 0 && !a.fits(w.estimate, a.objects) {
		if a.collected {
			return
		}
		a.gc()
		a.collected = true
		if !a.fits(w.estimate, a.live) {
			return
		}
	}
	a.remove(w)
	a.loading = true
	a.inFlight++
	w.granted = true
	close(w.ready)
}

func (a *RebuildAdmission) fits(estimate uint64, heap func() uint64) bool {
	limit := a.limit()
	if limit == math.MaxInt64 {
		return true
	}
	if limit <= rebuildHeadroomBytes {
		return false
	}
	return heap()+estimate <= limit-rebuildHeadroomBytes
}

// signal wakes whoever waits on the pause changing.
func (a *RebuildAdmission) signal() {
	close(a.changed)
	a.changed = make(chan struct{})
}

func (a *RebuildAdmission) oldestProofWait() time.Duration {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var oldest time.Time
	for _, w := range a.waiting {
		if w.kind == rebuildKindProof && (oldest.IsZero() || w.since.Before(oldest)) {
			oldest = w.since
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return time.Since(oldest)
}

// claimFlushWaiting records that supplier's claim is waiting for its stream to
// drain, which lets that supplier's consumer read while proofs wait. The
// returned function ends it.
func (a *RebuildAdmission) claimFlushWaiting(supplier string) (done func()) {
	if a == nil {
		return func() {}
	}
	a.mu.Lock()
	a.flushes[supplier]++
	a.signal()
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			if a.flushes[supplier]--; a.flushes[supplier] <= 0 {
				delete(a.flushes, supplier)
			}
			a.signal()
			a.mu.Unlock()
		})
	}
}

// IngestionPause returns supplier's view of the pause, for its stream consumer.
func (a *RebuildAdmission) IngestionPause(supplier string) IngestionPauseView {
	return IngestionPauseView{a: a, supplier: supplier}
}

// IngestionPauseView holds one supplier's stream consumer while a proof waits
// for memory, unless that supplier's claim is waiting for its stream.
type IngestionPauseView struct {
	a        *RebuildAdmission
	supplier string
}

// Paused reports whether the supplier's consumer must not read.
func (v IngestionPauseView) Paused() bool {
	if v.a == nil {
		return false
	}
	v.a.mu.Lock()
	defer v.a.mu.Unlock()
	return v.a.proofs > 0 && v.a.flushes[v.supplier] == 0
}

// PauseChanged returns a channel closed the next time Paused may have changed.
func (v IngestionPauseView) PauseChanged() <-chan struct{} {
	if v.a == nil {
		return nil
	}
	v.a.mu.Lock()
	defer v.a.mu.Unlock()
	return v.a.changed
}
