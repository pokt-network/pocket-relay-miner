package redis

// StoreHealth says whether Redis can take writes, for the miner and the relayer
// alike: one answer, read by every admission path, so the two binaries never
// disagree about whether the store is full.
//
// Two signals feed it. Redis's own reply is the one that cannot be wrong: a write
// refused with "OOM command not allowed" closes the store at once, whatever the
// last sample said. PING is not a write and is not refused under maxmemory
// (measured, Redis 8.10.1), which is why a heartbeat cannot stand in for it. The
// other signal is INFO memory, sampled every second, which closes the store BEFORE
// Redis starts refusing, while there is still room for the writes that must not
// stop (claims, proofs, deletes), and which is the only thing that reopens it.
//
// Closing and reopening use different amounts of free memory, so a store hovering
// at the line does not flap. A sample that stops arriving also closes the store:
// not knowing is not the same as having room.

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	// storeHealthPollInterval is how often INFO memory is sampled.
	storeHealthPollInterval = time.Second
	// storeHealthSampleMaxAge is how old the last successful sample may be before
	// the store is treated as not operable.
	storeHealthSampleMaxAge = 3 * time.Second
	// storeReserveMaxBytes caps the free memory below which the store closes. Claims
	// and proofs write ~8 MB per 1,000 sessions (measured in the 2026-09-16
	// saturation run), so the cap leaves them room many times over; it is not a
	// measurement of anything larger.
	storeReserveMaxBytes = 256 << 20
)

// Reasons the store is not operable. Bounded, used as a metric label.
const (
	StoreReasonMemoryReserve = "memory_reserve"
	StoreReasonOOMReply      = "oom_reply"
	StoreReasonSampleStale   = "sample_stale"
)

// storeCloseBelow is the free memory below which a store with maxmemory closes:
// a tenth of maxmemory, at most storeReserveMaxBytes. It reopens at twice that.
func storeCloseBelow(maxmemory uint64) uint64 {
	return min(uint64(storeReserveMaxBytes), maxmemory/10)
}

// StoreHealth is safe for concurrent use. A nil *StoreHealth is always operable,
// so a component built without one behaves as before.
type StoreHealth struct {
	logger    logging.Logger
	client    redis.UniversalClient
	component string
	now       func() time.Time

	operable atomic.Bool

	mu         sync.Mutex
	reason     string
	lastSample time.Time
	started    bool
	changed    chan struct{}
	onChange   []func(operable bool)
	noMaxWarn  bool
}

// NewStoreHealth returns an operable StoreHealth that samples through client
// once Start runs. component labels its metrics ("miner", "relayer").
func NewStoreHealth(logger logging.Logger, client redis.UniversalClient, component string) *StoreHealth {
	h := &StoreHealth{
		logger:    logging.ForComponent(logger, "store_health"),
		client:    client,
		component: component,
		now:       time.Now,
		changed:   make(chan struct{}),
	}
	h.operable.Store(true)
	storeOperable.WithLabelValues(component).Set(1)
	return h
}

// Operable reports whether writes should be attempted and work admitted.
func (h *StoreHealth) Operable() bool {
	return h == nil || h.operable.Load()
}

// Changed returns a channel closed at the next transition, in either direction.
// A caller waiting for the store to reopen reads Operable again after it fires.
// A nil StoreHealth never changes.
func (h *StoreHealth) Changed() <-chan struct{} {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

// OnChange registers fn to run at every transition, on the goroutine that caused
// it, after the new state is visible to Operable. fn must not block.
func (h *StoreHealth) OnChange(fn func(operable bool)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onChange = append(h.onChange, fn)
}

// Start samples INFO memory every storeHealthPollInterval until ctx ends. The
// first sample is taken before it returns.
func (h *StoreHealth) Start(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.started = true
	h.lastSample = h.now()
	h.mu.Unlock()
	h.poll(ctx)
	go logging.RecoverGoRoutine(h.logger, "store_health_poll", func(c context.Context) {
		ticker := time.NewTicker(storeHealthPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.Done():
				return
			case <-ticker.C:
				h.poll(c)
			}
		}
	})(ctx)
}

// poll takes one sample and applies it.
func (h *StoreHealth) poll(ctx context.Context) {
	sampleCtx, cancel := context.WithTimeout(ctx, storeHealthSampleMaxAge)
	defer cancel()
	info, err := h.client.Info(sampleCtx, "memory").Result()
	if err != nil {
		h.observeFailure()
		return
	}
	used, maxmemory, ok := parseStoreMemory(info)
	if !ok {
		h.observeFailure()
		return
	}
	h.observe(used, maxmemory)
}

// observeFailure closes the store when the last good sample is too old.
func (h *StoreHealth) observeFailure() {
	h.mu.Lock()
	stale := h.started && h.now().Sub(h.lastSample) > storeHealthSampleMaxAge
	h.mu.Unlock()
	if stale {
		h.transition(false, StoreReasonSampleStale)
	}
}

// observe applies a sample of used_memory and maxmemory.
func (h *StoreHealth) observe(used, maxmemory uint64) {
	h.mu.Lock()
	h.lastSample = h.now()
	reason := h.reason
	warnNoMax := maxmemory == 0 && !h.noMaxWarn
	if warnNoMax {
		h.noMaxWarn = true
	}
	h.mu.Unlock()

	if warnNoMax {
		h.logger.Warn().
			Str("component", h.component).
			Msg("Redis has no maxmemory: the store is only closed by refused writes or a lost sample, never before Redis runs out")
	}
	if maxmemory == 0 {
		storeFreeBytes.WithLabelValues(h.component).Set(-1)
		if !h.Operable() {
			h.transition(true, reason)
		}
		return
	}

	free := uint64(0)
	if used < maxmemory {
		free = maxmemory - used
	}
	storeFreeBytes.WithLabelValues(h.component).Set(float64(free))
	closeBelow := storeCloseBelow(maxmemory)
	switch {
	case h.Operable() && free < closeBelow:
		h.transition(false, StoreReasonMemoryReserve)
	case !h.Operable() && free >= 2*closeBelow:
		h.transition(true, reason)
	case !h.Operable() && reason == StoreReasonSampleStale && free >= closeBelow:
		// Closed for a lost sample, not for memory: a sample with room reopens it.
		h.transition(true, reason)
	}
}

// ReportOOM closes the store because Redis refused a write for memory.
func (h *StoreHealth) ReportOOM() {
	if h == nil {
		return
	}
	h.transition(false, StoreReasonOOMReply)
}

// transition moves to operable, recording why the store closed, or why it was
// closed when it reopens. Repeating the current state does nothing.
func (h *StoreHealth) transition(operable bool, reason string) {
	h.mu.Lock()
	if h.operable.Load() == operable {
		h.mu.Unlock()
		return
	}
	h.operable.Store(operable)
	if !operable {
		h.reason = reason
	}
	close(h.changed)
	h.changed = make(chan struct{})
	callbacks := append([]func(bool){}, h.onChange...)
	h.mu.Unlock()

	state := "closed"
	value := 0.0
	if operable {
		state, value = "open", 1
	}
	storeOperable.WithLabelValues(h.component).Set(value)
	storeTransitions.WithLabelValues(h.component, state, reason).Inc()
	if operable {
		h.logger.Info().Str("component", h.component).Str("reason", reason).
			Msg("Redis operable again: admitting work")
	} else {
		h.logger.Warn().Str("component", h.component).Str("reason", reason).
			Msg("Redis not operable: no new work admitted until it has room")
	}
	for _, fn := range callbacks {
		fn(operable)
	}
}

// parseStoreMemory reads used_memory and maxmemory from an INFO memory reply.
func parseStoreMemory(info string) (used, maxmemory uint64, ok bool) {
	var haveUsed, haveMax bool
	for _, line := range strings.Split(info, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		switch key {
		case "used_memory":
			n, err := strconv.ParseUint(value, 10, 64)
			used, haveUsed = n, err == nil
		case "maxmemory":
			n, err := strconv.ParseUint(value, 10, 64)
			maxmemory, haveMax = n, err == nil
		}
	}
	return used, maxmemory, haveUsed && haveMax
}

// Hook returns a go-redis hook that reports every OOM reply to h. Add it to every
// client the binary writes through.
func (h *StoreHealth) Hook() redis.Hook {
	return storeHealthHook{h: h}
}

type storeHealthHook struct{ h *StoreHealth }

func (k storeHealthHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (k storeHealthHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if redis.IsOOMError(cmd.Err()) {
			k.h.ReportOOM()
		}
		return err
	}
}

func (k storeHealthHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			if redis.IsOOMError(cmd.Err()) {
				k.h.ReportOOM()
				break
			}
		}
		return err
	}
}
