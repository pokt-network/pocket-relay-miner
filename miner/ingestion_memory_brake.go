package miner

// The ingestion memory brake holds every supplier's stream consumer while the
// process nears its memory limit: reading relays grows the trees in the heap,
// and past the container's memory the process is killed. It closes when the
// memory the limit bounds stays above the limit -- the runtime no longer holds
// it -- or when the live heap is above the limit less a margin, where the GC
// nears running without pause. It reopens only when the live heap has been
// below the limit less a margin and a half, and the runtime within its limit,
// for brakeReopenAfter.
//
// The limit bounds more than the live heap: with GOGC at 100 the runtime fills
// it with garbage and free pages it has not returned: measured under load, the
// live heap was half of that memory, and the process was killed before the
// live heap alone reached its threshold.
//
// Only ingestion stops. Claims, proofs and compactions go on, and they are
// what frees the heap. A supplier whose claim waits for its stream to drain
// still reads, as under the rebuild admission's pause: that claim seals at its
// height cap with or without the relays.
//
// Every reading is taken without a GC: open, the live heap is the one the
// runtime's own last GC measured.

import (
	"context"
	"time"

	"github.com/pokt-network/pocket-relay-miner/internal/memlimit"
)

// brakeInterval is how often the brake reads the process's memory.
const brakeInterval = time.Second

// brakeOverageTicks is how many evaluations in a row must find the runtime
// over its limit to close the brake. A single one is the runtime's own
// overshoot, which its next collection corrects: closing on it opened and
// closed the brake every few seconds under load (L3, a proof window).
const brakeOverageTicks = 2

// brakeReopenAfter is how long the process must stay within its limit, with
// the live heap below the reopen threshold, for the brake to reopen. The
// runtime holds its memory at the limit, not below it, so a margin under the
// limit could never be reached; time spent within it can.
const brakeReopenAfter = 10 * time.Second

// brakeCollectAfter is how long the brake, closed, lets the runtime go without
// a GC before it forces one to see the live heap fall.
const brakeCollectAfter = time.Minute

// RunMemoryBrake evaluates the ingestion memory brake every brakeInterval
// until ctx ends.
func (a *RebuildAdmission) RunMemoryBrake(ctx context.Context) {
	limit := a.limit()
	closeAbove, reopenBelow := memlimit.BrakeThresholds(limit)
	a.logger.Info().
		Uint64("limit_bytes", limit).
		Uint64("close_above_bytes", closeAbove).
		Uint64("reopen_below_bytes", reopenBelow).
		Msg("ingestion memory brake started")

	ticker := time.NewTicker(brakeInterval)
	defer ticker.Stop()
	for {
		a.evaluateMemoryBrake()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// evaluateMemoryBrake closes or reopens the brake from one reading of the
// process's memory, and asks the admission again: a compaction held while the
// runtime was over its limit has no other event to wake it. Only
// RunMemoryBrake's goroutine calls it.
func (a *RebuildAdmission) evaluateMemoryBrake() {
	defer func() {
		a.mu.Lock()
		a.unlockAndDispatch()
	}()
	limit := a.limit()
	closeAbove, reopenBelow := memlimit.BrakeThresholds(limit)
	a.mu.Lock()
	closed := a.brakeClosed
	a.mu.Unlock()

	mapped, objects, live := a.mapped(), a.objects(), a.live()
	if mapped > limit {
		a.brakeOverages++
	} else {
		a.brakeOverages = 0
	}

	if !closed {
		reason := ""
		switch {
		case a.brakeOverages >= brakeOverageTicks:
			reason = memoryBrakeReasonOverage
		case live > closeAbove:
			reason = memoryBrakeReasonLive
		default:
			return
		}
		a.brakeWithinSince = time.Time{}
		a.setMemoryBrake(true)
		a.logger.Warn().
			Str("reason", reason).
			Uint64("mapped_bytes", mapped).
			Uint64("live_bytes", live).
			Uint64("objects_bytes", objects).
			Uint64("limit_bytes", limit).
			Uint64("close_above_bytes", closeAbove).
			Uint64("reopen_below_bytes", reopenBelow).
			Msg("ingestion memory brake closed: stream consumers stop reading")
		return
	}

	// Closed, the process may allocate too little for the runtime to collect,
	// and trees the claims and proofs let go stay counted in the live heap
	// until a GC. One is forced when none ended for brakeCollectAfter.
	if live >= reopenBelow && a.now().Sub(a.lastGC()) >= brakeCollectAfter {
		a.collect(gcReasonMemoryBrake)
		live, mapped = a.live(), a.mapped()
	}
	if live >= reopenBelow || mapped > limit {
		a.brakeWithinSince = time.Time{}
		return
	}
	if now := a.now(); a.brakeWithinSince.IsZero() {
		a.brakeWithinSince = now
		return
	} else if now.Sub(a.brakeWithinSince) < brakeReopenAfter {
		return
	}
	closedFor := a.setMemoryBrake(false)
	a.logger.Info().
		Uint64("mapped_bytes", mapped).
		Uint64("live_bytes", live).
		Uint64("limit_bytes", limit).
		Uint64("reopen_below_bytes", reopenBelow).
		Dur("closed_for", closedFor).
		Msg("ingestion memory brake reopened: stream consumers read again")
}

// setMemoryBrake closes or reopens the brake and wakes the consumers waiting
// on the pause. Reopening returns how long it was closed.
func (a *RebuildAdmission) setMemoryBrake(closed bool) (closedFor time.Duration) {
	now := a.now()
	a.mu.Lock()
	a.brakeClosed = closed
	if closed {
		a.brakeClosedAt = now
	} else {
		closedFor = now.Sub(a.brakeClosedAt)
	}
	a.signal()
	a.mu.Unlock()

	state := memoryBrakeOpen
	if closed {
		state = memoryBrakeClosed
		ingestionMemoryBrakeClosed.Set(1)
	} else {
		ingestionMemoryBrakeClosed.Set(0)
	}
	ingestionMemoryBrakeTransitions.WithLabelValues(state).Inc()
	return closedFor
}
