package miner

// The ingestion memory brake holds every supplier's stream consumer while the
// heap nears the process's memory limit: reading relays grows the trees in the
// heap, and past the container's memory the process is killed. It closes when
// the live heap is above the limit less a margin, and reopens only below the
// limit less a margin and a half, so it does not open and close on every
// collection.
//
// Only ingestion stops. Claims, proofs and compactions go on, and they are
// what frees the heap. A supplier whose claim waits for its stream to drain
// still reads, as under the rebuild admission's pause: that claim seals at its
// height cap with or without the relays.
//
// The heap's objects are read first. They include garbage, so under a
// threshold they settle it without a GC; over it, the live heap a GC measured
// decides, forced unless the runtime ran one in the last second.

import (
	"context"
	"time"

	"github.com/pokt-network/pocket-relay-miner/internal/memlimit"
)

// RunMemoryBrake evaluates the ingestion memory brake every forcedGCInterval
// until ctx ends.
func (a *RebuildAdmission) RunMemoryBrake(ctx context.Context) {
	limit := a.limit()
	closeAbove, reopenBelow := memlimit.BrakeThresholds(limit)
	a.logger.Info().
		Uint64("limit_bytes", limit).
		Uint64("close_above_bytes", closeAbove).
		Uint64("reopen_below_bytes", reopenBelow).
		Msg("ingestion memory brake started")

	ticker := time.NewTicker(forcedGCInterval)
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
// heap. Only RunMemoryBrake's goroutine calls it.
func (a *RebuildAdmission) evaluateMemoryBrake() {
	limit := a.limit()
	closeAbove, reopenBelow := memlimit.BrakeThresholds(limit)
	a.mu.Lock()
	closed := a.brakeClosed
	a.mu.Unlock()

	objects := a.objects()
	heap := objects
	if closed && objects >= reopenBelow || !closed && objects > closeAbove {
		a.collect(gcReasonMemoryBrake)
		heap = a.live()
	}

	switch {
	case !closed && heap > closeAbove:
		a.setMemoryBrake(true)
		a.logger.Warn().
			Uint64("live_bytes", heap).
			Uint64("objects_bytes", objects).
			Uint64("limit_bytes", limit).
			Uint64("close_above_bytes", closeAbove).
			Uint64("reopen_below_bytes", reopenBelow).
			Msg("ingestion memory brake closed: stream consumers stop reading")
	case closed && heap < reopenBelow:
		closedFor := a.setMemoryBrake(false)
		a.logger.Info().
			Uint64("heap_bytes", heap).
			Uint64("limit_bytes", limit).
			Uint64("reopen_below_bytes", reopenBelow).
			Dur("closed_for", closedFor).
			Msg("ingestion memory brake reopened: stream consumers read again")
	}
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
