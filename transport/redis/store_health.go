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
)

// StoreGate names a consumer of the same sample. Every gate closes at the same
// amount of free memory and reopens at its own: the miner's reopens first, so it
// starts draining what waited in the streams before the relayer admits new traffic.
type StoreGate string

const (
	// StoreGateAdmission is the relayer's: new relays.
	StoreGateAdmission StoreGate = "admission"
	// StoreGateIngestion is the miner's: reading the relay streams.
	StoreGateIngestion StoreGate = "ingestion"
)

// storeReserveMaxBytes is the free memory below which every gate closes, at most.
// With 256 MiB, measured under load on 2026-09-16, the store reopened at 512 MiB
// free and closed again about 66 s later, over and over until the cold tree
// compaction freed more than 3 GiB. 1 GiB, with admission reopening 1 GiB above it
// and ingestion 512 MiB above it, is the owner's call on that measurement.
const storeReserveMaxBytes = 1 << 30

// storeGateReopenMargin is how far above the close line each gate reopens, as a
// fraction of it: all of it for admission (1 GiB), half for ingestion (512 MiB).
var storeGateReopenMargin = map[StoreGate]uint64{
	StoreGateAdmission: 1,
	StoreGateIngestion: 2,
}

// Reasons the store is not operable. Bounded, used as a metric label.
const (
	StoreReasonMemoryReserve = "memory_reserve"
	StoreReasonOOMReply      = "oom_reply"
	StoreReasonSampleStale   = "sample_stale"
)

// storeCloseBelow is the free memory below which every gate closes:
// storeReserveMaxBytes, or an eighth of maxmemory when that is smaller, so a small
// Redis is not closed from the start.
func storeCloseBelow(maxmemory uint64) uint64 {
	return min(uint64(storeReserveMaxBytes), maxmemory/8)
}

// storeReopenAt is the free memory at which gate reopens: the close line plus the
// gate's margin (2 GiB for admission, 1.5 GiB for ingestion at the full reserve).
func storeReopenAt(gate StoreGate, maxmemory uint64) uint64 {
	closeBelow := storeCloseBelow(maxmemory)
	return closeBelow + closeBelow/storeGateReopenMargin[gate]
}

// storeGateState is one gate's view of the shared sample.
type storeGateState struct {
	gate     StoreGate
	operable atomic.Bool
	// Guarded by StoreHealth.mu.
	reason   string
	closedAt time.Time
	changed  chan struct{}
	onChange []func(operable bool)
}

// StoreHealth is safe for concurrent use. A nil *StoreHealth is always operable,
// so a component built without one behaves as before. Operable, Changed and
// OnChange answer for the gate the process was built with; Gate answers for any.
type StoreHealth struct {
	logger      logging.Logger
	client      redis.UniversalClient
	component   string
	defaultGate StoreGate
	now         func() time.Time

	mu         sync.Mutex
	gates      map[StoreGate]*storeGateState
	lastSample time.Time
	started    bool
	noMaxWarn  bool
	lastUsed   uint64
	lastMax    uint64
}

// NewStoreHealth returns an operable StoreHealth that samples through client once
// Start runs. component labels its metrics ("miner", "relayer"); gate is the
// threshold Operable, Changed and OnChange use.
func NewStoreHealth(logger logging.Logger, client redis.UniversalClient, component string, gate StoreGate) *StoreHealth {
	h := &StoreHealth{
		logger:      logging.ForComponent(logger, "store_health"),
		client:      client,
		component:   component,
		defaultGate: gate,
		now:         time.Now,
		gates:       make(map[StoreGate]*storeGateState, len(storeGateReopenMargin)),
	}
	for g := range storeGateReopenMargin {
		st := &storeGateState{gate: g, changed: make(chan struct{})}
		st.operable.Store(true)
		h.gates[g] = st
		storeOperable.WithLabelValues(component, string(g)).Set(1)
	}
	return h
}

// StoreGateView is one gate of a StoreHealth.
type StoreGateView struct {
	h    *StoreHealth
	gate StoreGate
}

// Gate returns the view of h at gate.
func (h *StoreHealth) Gate(gate StoreGate) StoreGateView { return StoreGateView{h: h, gate: gate} }

// Operable reports whether the gate admits work.
func (v StoreGateView) Operable() bool {
	return v.h == nil || v.h.gates[v.gate].operable.Load()
}

// Changed returns a channel closed at the gate's next transition. nil never changes.
func (v StoreGateView) Changed() <-chan struct{} {
	if v.h == nil {
		return nil
	}
	v.h.mu.Lock()
	defer v.h.mu.Unlock()
	return v.h.gates[v.gate].changed
}

// OnChange registers fn to run at every transition of the gate, on the goroutine
// that caused it, after the new state is visible to Operable. fn must not block.
func (v StoreGateView) OnChange(fn func(operable bool)) {
	if v.h == nil {
		return
	}
	v.h.mu.Lock()
	defer v.h.mu.Unlock()
	st := v.h.gates[v.gate]
	st.onChange = append(st.onChange, fn)
}

// Operable reports whether the process's gate admits work.
func (h *StoreHealth) Operable() bool { return h.view().Operable() }

// Changed is Changed of the process's gate.
func (h *StoreHealth) Changed() <-chan struct{} { return h.view().Changed() }

// OnChange is OnChange of the process's gate.
func (h *StoreHealth) OnChange(fn func(operable bool)) { h.view().OnChange(fn) }

func (h *StoreHealth) view() StoreGateView {
	if h == nil {
		return StoreGateView{}
	}
	return h.Gate(h.defaultGate)
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

// observeFailure closes every gate when the last good sample is too old.
func (h *StoreHealth) observeFailure() {
	h.mu.Lock()
	stale := h.started && h.now().Sub(h.lastSample) > storeHealthSampleMaxAge
	h.mu.Unlock()
	if stale {
		h.closeAll(StoreReasonSampleStale)
	}
}

// observe applies a sample of used_memory and maxmemory to every gate.
func (h *StoreHealth) observe(used, maxmemory uint64) {
	h.mu.Lock()
	h.lastSample = h.now()
	h.lastUsed, h.lastMax = used, maxmemory
	warnNoMax := maxmemory == 0 && !h.noMaxWarn
	if warnNoMax {
		h.noMaxWarn = true
	}
	h.mu.Unlock()

	if warnNoMax {
		h.logger.Warn().
			Str("process", h.component).
			Msg("Redis has no maxmemory: the store is only closed by refused writes or a lost sample, never before Redis runs out")
	}
	free := uint64(0)
	if used < maxmemory {
		free = maxmemory - used
	}
	if maxmemory == 0 {
		storeFreeBytes.WithLabelValues(h.component).Set(-1)
	} else {
		storeFreeBytes.WithLabelValues(h.component).Set(float64(free))
	}
	for gate, st := range h.gates {
		operable := st.operable.Load()
		h.mu.Lock()
		reason := st.reason
		h.mu.Unlock()
		if maxmemory == 0 {
			if !operable {
				h.transition(gate, true, reason)
			}
			continue
		}
		closeBelow := storeCloseBelow(maxmemory)
		switch {
		case operable && free < closeBelow:
			h.transition(gate, false, StoreReasonMemoryReserve)
		case !operable && free >= storeReopenAt(gate, maxmemory):
			h.transition(gate, true, reason)
		case !operable && reason == StoreReasonSampleStale && free >= closeBelow:
			// Closed for a lost sample, not for memory: a sample with room reopens it.
			h.transition(gate, true, reason)
		}
	}
}

// ReportOOM closes every gate because Redis refused a write for memory.
func (h *StoreHealth) ReportOOM() {
	if h == nil {
		return
	}
	h.closeAll(StoreReasonOOMReply)
}

func (h *StoreHealth) closeAll(reason string) {
	for gate := range h.gates {
		h.transition(gate, false, reason)
	}
}

// transition moves gate to operable, recording why it closed, or why it was
// closed when it reopens. Repeating the current state does nothing.
func (h *StoreHealth) transition(gate StoreGate, operable bool, reason string) {
	h.mu.Lock()
	st := h.gates[gate]
	if st.operable.Load() == operable {
		h.mu.Unlock()
		return
	}
	st.operable.Store(operable)
	var closedFor time.Duration
	if operable {
		closedFor = h.now().Sub(st.closedAt)
	} else {
		st.reason = reason
		st.closedAt = h.now()
	}
	close(st.changed)
	st.changed = make(chan struct{})
	callbacks := append([]func(bool){}, st.onChange...)
	used, maxmemory := h.lastUsed, h.lastMax
	h.mu.Unlock()

	state := "closed"
	value := 0.0
	if operable {
		state, value = "open", 1
	}
	storeOperable.WithLabelValues(h.component, string(gate)).Set(value)
	storeTransitions.WithLabelValues(h.component, string(gate), state, reason).Inc()
	if operable {
		storeClosedSeconds.WithLabelValues(h.component, string(gate), reason).Add(closedFor.Seconds())
	}
	free := uint64(0)
	if used < maxmemory {
		free = maxmemory - used
	}
	closeBelow := storeCloseBelow(maxmemory)
	// "process", not "component": the logger already carries component=store_health,
	// and a second component key made the JSON line hold the same key twice.
	if operable {
		h.logger.Info().Str("process", h.component).Str("gate", string(gate)).Str("reason", reason).
			Uint64("free_bytes", free).
			Uint64("reopen_at_bytes", storeReopenAt(gate, maxmemory)).
			Dur("closed_for", closedFor).
			Msg("Redis operable again: admitting work")
	} else {
		h.logger.Warn().Str("process", h.component).Str("gate", string(gate)).Str("reason", reason).
			Uint64("used_memory", used).
			Uint64("maxmemory", maxmemory).
			Uint64("free_bytes", free).
			Uint64("close_below_bytes", closeBelow).
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
