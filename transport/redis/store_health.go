package redis

// StoreHealth says whether Redis can take writes, for the miner and the relayer
// alike: one answer, read by every admission path, so the two binaries never
// disagree about whether the store is full.
//
// Two signals feed it. Redis's own reply is the one that cannot be wrong: a write
// refused with "OOM command not allowed" closes the store at once, whatever the
// last sample said. PING is not a write and is not refused under maxmemory
// (measured, Redis 8.10.1), which is why a heartbeat cannot stand in for it. The
// other signal is INFO, sampled every second, which closes the store BEFORE
// Redis starts refusing, while there is still room for the writes that must not
// stop (claims, proofs, deletes), and which is the only thing that reopens it.
//
// Closing and reopening use different amounts of free memory, so a store hovering
// at the line does not flap. A sample that stops arriving also closes the store:
// not knowing is not the same as having room.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	// storeHealthPollInterval is how often INFO is sampled.
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
	// StoreReasonMisconfigured is a store that answered with a configuration
	// this miner cannot run on: no memory limit, or a policy that evicts.
	StoreReasonMisconfigured = "misconfigured"
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

// Start samples INFO every storeHealthPollInterval until ctx ends. The
// first sample is taken before it returns, and it decides whether this process
// runs at all: a store that ANSWERED with a configuration this miner cannot run
// on (see storeVersionRefusal and storeConfigRefusal) returns an error, and the
// caller stops.
//
// A store that did not answer is not an error. Its gates are closed by the
// sample's own age, and a Redis that is slow to accept connections is a state
// that passes; refusing to start on it would turn a delay into an outage.
func (h *StoreHealth) Start(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	h.started = true
	h.lastSample = h.now()
	h.mu.Unlock()
	if err := h.preflight(ctx); err != nil {
		return err
	}
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
	return nil
}

// preflight takes the first sample and refuses a store whose configuration this
// process cannot run on. The sample is applied either way, so a store that is
// merely full, or one that has not answered yet, starts with its gates closed
// rather than not at all.
//
// A store that has not answered ONCE is closed here and not left to age out:
// until a sample arrives there is nothing to say Redis can take a write, and
// this process refuses what it cannot record.
func (h *StoreHealth) preflight(ctx context.Context) error {
	sampleCtx, cancel := context.WithTimeout(ctx, storeHealthSampleMaxAge)
	defer cancel()
	info, err := h.sample(sampleCtx)
	if err != nil {
		h.closeAll(StoreReasonSampleStale)
		h.logger.Error().Err(err).Str("process", h.component).
			Msg("redis did not answer INFO at startup: the store stays closed until it does")
		return nil
	}
	s, ok := parseStoreInfo(info)
	if !ok {
		h.closeAll(StoreReasonSampleStale)
		h.logger.Error().Str("process", h.component).
			Msg("redis INFO has no used_memory, maxmemory or maxmemory_policy: the store stays closed")
		return nil
	}
	// Applied before the refusal is returned, so what the metrics show is the
	// sample the process refused to run on.
	return h.apply(s)
}

// sample reads INFO with no section argument. The default reply carries both
// the Server and the Memory sections on every Redis version, while INFO with
// several sections is a newer syntax: an old server would answer it with an
// error, which preflight reads as "did not answer" -- and the process would
// start without ever learning the version it has to refuse.
//
// With a cluster client INFO answers from one node; the supported topology is
// one Redis.
func (h *StoreHealth) sample(ctx context.Context) (string, error) {
	info, err := h.client.Info(ctx).Result()
	if err != nil {
		return "", fmt.Errorf("redis INFO: %w", err)
	}
	return info, nil
}

// apply checks the server's version, then applies the memory sample. A version
// this release does not run on closes every gate as misconfigured, the same way
// a store without a memory limit does, and stays closed until a sample from a
// supported server arrives. It returns why the store cannot be run on, or nil.
//
// The version is checked on every sample, not only at startup: a Redis that
// had not answered when the process started, or one replaced by an older
// server behind the same address, would otherwise never be looked at again.
func (h *StoreHealth) apply(s storeSample) error {
	if err := storeVersionRefusal(s.version, s.fork); err != nil {
		h.mu.Lock()
		h.lastSample = h.now()
		h.lastUsed, h.lastMax = s.used, s.maxmemory
		h.mu.Unlock()
		storeFreeBytes.WithLabelValues(h.component).Set(-1)
		h.closeAll(StoreReasonMisconfigured)
		return err
	}
	h.observe(s.used, s.maxmemory, s.policy)
	return storeConfigRefusal(s.maxmemory, s.policy)
}

// poll takes one sample and applies it.
func (h *StoreHealth) poll(ctx context.Context) {
	sampleCtx, cancel := context.WithTimeout(ctx, storeHealthSampleMaxAge)
	defer cancel()
	info, err := h.sample(sampleCtx)
	if err != nil {
		h.observeFailure()
		return
	}
	s, ok := parseStoreInfo(info)
	if !ok {
		h.observeFailure()
		return
	}
	// The refusal is already carried by the closed gates and their reason.
	_ = h.apply(s)
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

// storeEvictionPolicy is the only maxmemory-policy this miner runs on. Every
// other one evicts, and what Redis holds here -- the SMST nodes a claim is
// proved from, the relays not yet in a tree -- is not a cache: a key it drops
// is a proof this supplier can no longer produce.
const storeEvictionPolicy = "noeviction"

// storeMinRedisVersion is the oldest Redis this release runs on. 8.10 is the
// version it was built, tested and measured on (8.10.1); the stream consumer
// releases what it cannot process with XNACK, which Redis before 8.8 does not have.
var storeMinRedisVersion = version.Must(version.NewVersion("8.10.0"))

// storeForkVersionKeys are INFO fields that name a Redis-compatible server which
// is not Redis. Such a server reports a compatibility redis_version of its own
// choosing, so naming the fork is the only message that leads the operator
// somewhere. Valkey's field is valkey_version; others are not listed because
// their field names were not verified against a live server.
var storeForkVersionKeys = []string{"valkey_version"}

// storeVersionRefusal reports why a server's version cannot be run on, or nil
// when it can. Anything that does not read as a plain dotted version -- empty,
// absent, "v8.10", a pre-release -- is refused: this process does not start on
// a server it cannot identify.
func storeVersionRefusal(redisVersion, fork string) error {
	if fork != "" {
		return fmt.Errorf("redis INFO reports %s (redis_version %q): this server is not Redis; run Redis %s or newer", fork, redisVersion, storeMinRedisVersion.Original())
	}
	if redisVersion == "" || redisVersion[0] < '0' || redisVersion[0] > '9' {
		return fmt.Errorf("redis INFO reports redis_version %q, which is not a version this release can check: run Redis %s or newer", redisVersion, storeMinRedisVersion.Original())
	}
	v, err := version.NewVersion(redisVersion)
	if err != nil {
		return fmt.Errorf("redis INFO reports redis_version %q, which is not a version this release can check (%w): run Redis %s or newer", redisVersion, err, storeMinRedisVersion.Original())
	}
	if v.LessThan(storeMinRedisVersion) {
		return fmt.Errorf("redis_version is %s: this release runs on Redis %s or newer; upgrade Redis", redisVersion, storeMinRedisVersion.Original())
	}
	return nil
}

// storeConfigRefusal reports why a store's memory configuration cannot be run
// on, or nil when it can. It names the value read, so the operator does not
// have to find it.
func storeConfigRefusal(maxmemory uint64, policy string) error {
	switch {
	case maxmemory == 0:
		return fmt.Errorf("redis maxmemory is 0 (no memory limit): set it below the memory its container has, so Redis refuses writes instead of being killed")
	case policy != storeEvictionPolicy:
		return fmt.Errorf("redis maxmemory-policy is %q: set it to %s, or Redis silently drops the nodes a claim is proved from", policy, storeEvictionPolicy)
	}
	return nil
}

// observe applies a sample of used_memory, maxmemory and maxmemory-policy to
// every gate.
//
// A store with no memory limit, or one that evicts, is closed and stays closed:
// without a limit Redis never refuses a write, it is killed instead, and the
// relays written since its last save go with it; with an evicting policy it
// drops SMST nodes, which is a claim this miner can no longer prove. Neither is
// a condition that passes: only a sample with a usable configuration reopens.
func (h *StoreHealth) observe(used, maxmemory uint64, policy string) {
	h.mu.Lock()
	h.lastSample = h.now()
	h.lastUsed, h.lastMax = used, maxmemory
	h.mu.Unlock()

	if err := storeConfigRefusal(maxmemory, policy); err != nil {
		storeFreeBytes.WithLabelValues(h.component).Set(-1)
		h.closeAll(StoreReasonMisconfigured)
		return
	}
	free := uint64(0)
	if used < maxmemory {
		free = maxmemory - used
	}
	storeFreeBytes.WithLabelValues(h.component).Set(float64(free))
	for gate, st := range h.gates {
		operable := st.operable.Load()
		h.mu.Lock()
		reason := st.reason
		h.mu.Unlock()
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

// storeSample is one INFO reply, as StoreHealth reads it.
type storeSample struct {
	used, maxmemory uint64
	policy          string
	// version is redis_version, "" when the reply has none.
	version string
	// fork is "<field> <value>" for the first storeForkVersionKeys field the
	// reply carries, "" when it carries none.
	fork string
}

// parseStoreInfo reads a sample from an INFO reply. A reply without
// used_memory, maxmemory and maxmemory_policy is not a sample: the caller
// treats it as a store that did not answer. A missing redis_version is not a
// missing sample: the server answered, and apply refuses what it cannot name.
func parseStoreInfo(info string) (storeSample, bool) {
	var s storeSample
	used, maxmemory, policy, ok := parseStoreMemory(info)
	if !ok {
		return s, false
	}
	s.used, s.maxmemory, s.policy = used, maxmemory, policy
	for _, line := range strings.Split(info, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		if key == "redis_version" {
			s.version = value
			continue
		}
		for _, fk := range storeForkVersionKeys {
			if key == fk && s.fork == "" {
				s.fork = fk + " " + value
			}
		}
	}
	return s, true
}

// parseStoreMemory reads used_memory, maxmemory and maxmemory_policy from an
// INFO reply. A reply without all three is not a sample: the caller treats it
// as a store that did not answer.
func parseStoreMemory(info string) (used, maxmemory uint64, policy string, ok bool) {
	var haveUsed, haveMax, havePolicy bool
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
		case "maxmemory_policy":
			// INFO writes it with an underscore; CONFIG GET spells the same
			// setting with a hyphen, and CONFIG is refused by managed Redis.
			policy, havePolicy = value, true
		}
	}
	return used, maxmemory, policy, haveUsed && haveMax && havePolicy
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
