package miner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alitto/pond/v2"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/tx"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// InclusionOutcome values are stable strings — metric labels and
// submission-tracker JSON depend on them.
const (
	// Causes for inclusionEntryDroppedTotal and inclusionGroupAbandonedTotal.
	// Both sets are CLOSED and live here beside the code that stamps them: a
	// Prometheus label whose value set drifts is worse than one that disappears.
	//
	// entry_corrupt covers the three paths that meet an entry they cannot
	// decode. A failed clear is NOT one of these -- it has its own metric,
	// because there the entry survives rather than being dropped.
	dropCauseCorrupt = "entry_corrupt"

	abandonCauseListFailed     = "list_failed"
	abandonCauseParamsFailed   = "params_failed"
	abandonCauseIndexMalformed = "index_malformed"
	// index_unreadable is the WIDEST of these: ActiveGroups failing abandons the
	// entire pass for that phase -- every group, not one -- and until it was
	// counted the only trace was a log line on a per-block path.
	abandonCauseIndexUnreadable = "index_unreadable"
	// budget_exhausted is the only one of these that is not a failure of a
	// dependency: PerGroupTimeout covers the listing, the inclusion query and
	// every resend in the group IN SERIES, so a large group or a slow query
	// spends it and the entries still queued get nothing. It is counted per
	// GROUP like its siblings -- the loop stops on the first one, because once
	// the budget is gone every remaining entry would take the same exit.
	//
	// It answers a question inclusion_resend_cap_unused_total cannot: that one
	// measures the DAMAGE (budget left at window close) without saying whether
	// the cause was a window too short, which an operator cannot change, or a
	// group budget too small, which they can. Two causes, opposite actions.
	abandonCauseBudgetExhausted = "budget_exhausted"

	inclusionFound   = "on_chain_found"
	inclusionMissing = "on_chain_missing"
	inclusionPollErr = "poll_error"
	// Causes for proofRejectionDiagnosisTotal. CLOSED set, and the third member
	// is not a filler: the comparison needs a session snapshot that outlives the
	// SMST, and nothing deletes that snapshot -- it expires on SessionTTL, which
	// is CONFIGURABLE, as is the block time that decides how long a proof window
	// lasts. So "the snapshot was still there" is a bound and never a guarantee,
	// and root_unknown is the answer when it was not.
	rejectionRootMismatch = "root_mismatch"
	rejectionRootMatch    = "root_match"
	rejectionRootUnknown  = "root_unknown"

	// on_chain_rejected is NOT a flavour of missing, and separating them is the
	// whole point: missing says the message never landed, rejected says it landed
	// and the EndBlocker condemned it. Reported as missing, the operator reads a
	// delivery problem and looks at the network.
	inclusionRejected = "on_chain_rejected"
)

// rebroadcastEntry is the per-session payload stored in the RebroadcastStore.
// It carries the built message bytes plus the metadata the reconciler needs to
// gate rebroadcasts (SubmitHeight, for the 1-block grace) and record outcomes
// (TxHash, for the claim outcome reconciler which is keyed by tx hash). It is
// JSON-encoded into the store's opaque value.
type rebroadcastEntry struct {
	MsgBytes     []byte `json:"m"`
	SubmitHeight int64  `json:"h"`
	TxHash       string `json:"t"`           // latest tx hash (updated on each rebroadcast)
	OrigTxHash   string `json:"o,omitempty"` // original submit tx hash (immutable) — claim outcome is keyed by it
	ServiceID    string `json:"s,omitempty"` // for the outcome/rebroadcast metric label
	Rebroadcasts int    `json:"n,omitempty"` // # of resends so far (persisted → HA-safe cap across failover)
	// TimeoutSeconds and TimeoutRegime carry the broadcast budget the ORIGINAL
	// submission was born with, so a resend inherits it instead of deriving its
	// own. That is what keeps the budget constant within a window: this side
	// cannot recompute it -- supplier_manager has no access to shared params, so
	// it does not know the window length -- and a resend that guessed would hand
	// the chain a different deadline than the attempt it is replacing, without
	// anything failing.
	//
	// Absent (an entry written before this field existed, or by an older binary
	// that dropped what its struct could not see) means "no inherited budget":
	// the resend falls back to the chain's ceiling under the "unknown" regime,
	// which the regime counter shows rather than hides.
	TimeoutSeconds int64  `json:"ts,omitempty"`
	TimeoutRegime  string `json:"tr,omitempty"`
	// LastAttemptHeight is the height the last resend ACTUALLY went out at, and
	// it is what makes the spacing real rather than nominal: the reconciler does
	// not run at every height -- its only production caller feeds it through a
	// coalescing loop that keeps just the LATEST height, so while one pass is
	// running the heights that go by are never passed to it -- and an attempt
	// can therefore leave later than the threshold that released it.
	//
	// The single-flight guard inside OnBlock is NOT what does this: that caller
	// is a serial processor, so the guard has no concurrent entry to refuse. It
	// is said here because the guard is the obvious place to look, and a reader
	// who stops there concludes the skipping cannot happen.
	//
	// Zero means "not known" and every reader falls back to the nominal
	// schedule. That is not a defensive default, it is the mixed-fleet contract,
	// and it holds in BOTH directions: an entry written by a binary without this
	// field reads as 0 here, and an entry written with it and then rewritten by
	// an older binary LOSES it, because the older struct has no such field and
	// its re-marshal drops what it cannot see. Both directions degrade to the
	// NOMINAL schedule -- spacing counted from where each attempt was due rather
	// than from where it landed -- which is a weaker guarantee and still a
	// spacing. It is deliberately not a fallback to the bare base: that would
	// re-open, during any rolling deploy, exactly the back-to-back resend this
	// change exists to prevent.
	LastAttemptHeight int64 `json:"l,omitempty"`
}

func marshalRebroadcastEntry(e rebroadcastEntry) ([]byte, error) { return json.Marshal(e) }

func unmarshalRebroadcastEntry(b []byte) (rebroadcastEntry, error) {
	var e rebroadcastEntry
	err := json.Unmarshal(b, &e)
	return e, err
}

// MessageResubmitter re-broadcasts a previously-built claim/proof message for a
// supplier with the given window-close timeout, returning the new tx hash. The
// concrete implementation (wiring layer) unmarshals the bytes into the right
// proto type and routes to that supplier's client.
type MessageResubmitter interface {
	ResubmitMessage(ctx context.Context, phase RebroadcastPhase, supplier string, msgBytes []byte, timeoutHeight int64, timeout time.Duration, regime string) (newTxHash string, err error)
}

// InclusionReconcilerConfig configures the block-driven inclusion reconciler.
type InclusionReconcilerConfig struct {
	// MaxConcurrent bounds the per-block group-reconcile worker pool. Default 64.
	//
	// It is capped by the transaction client's concurrency limit at
	// construction: workers above that number can only ever start in order to
	// park, and a parked worker spends the group's budget without reaching the
	// chain. Sizing the pool from the semaphore removes the contention this
	// process inflicts on itself; what remains is contention against the
	// lifecycle, which is bounded by PerGroupTimeout and costs a delay of one
	// block, not a lost resend -- the payloads stay in the store.
	MaxConcurrent int
	// MaxRebroadcasts caps how many times a still-missing claim/proof is
	// re-submitted within its window. Worst case is that many times the gas.
	// 0 = observe-only (record outcomes, never resend).
	//
	// Why spaced resends from mid-window, and not one per block: txs are
	// unordered with a block-time-anchored timeout that spans ~the whole window,
	// so the original is valid in any later (empty) block until it times out.
	// Re-sending every block would flood the mempool with copies that mostly
	// fail DeliverTx as duplicates. The failure a resend fixes is mempool
	// eviction during the submit-block burst, and a resend into a later empty
	// block recovers it — the spacing is what gives each one a block to be
	// included and a block to be observed before the next is considered.
	//
	// CORRECTED 2026-09-04. This comment used to say that re-sending every
	// block "would build a fresh tx each time (new timeout → new hash, not
	// deduped)". That was the wrong model of the nonce, and it was written two
	// months AFTER the timeout was anchored to latest_block_time: inside one
	// block the anchor does not move, so two sends in the same block built the
	// SAME timeout and therefore the same unordered nonce (the pair the chain
	// keys on is (timeout.UnixNano(), sender)). Far from "not deduped", they
	// collided, and the second was rejected in CheckTx with "already used
	// timeout". Across blocks the anchor does move, so a resend one block later
	// is genuinely a new nonce -- which is why the mid-window resend works at
	// all, and why it is NOT deduplicated by the chain. What deduplicates it is
	// poktroll's upsert on (sessionId, supplier).
	//
	// REVISED: unset now means NO CAP. The two reasons the paragraph above gives
	// for spacing resends out are both gone -- the window close is enforced by
	// the transaction's own timeout_height, and a redundant resend is refused by
	// the node for free as code 19, classified and exempt from counting. What is
	// left is that a claim only earns anything if it lands, so the resend that
	// matters is the one after the block that lost it.
	//
	// nil is NOT the same as a large number, which is why this is a pointer and
	// not a sentinel: 0 means observe-only and any positive value is a real cap,
	// so a numeric stand-in for "unlimited" would be indistinguishable from an
	// operator asking for exactly that many. The distinction already existed in
	// the operator's own field for the same reason.
	MaxRebroadcasts *int
	// RebroadcastSafetyBlocks stops rebroadcasting once the chain is within this
	// many blocks of window-close (a resend cannot land after the window).
	// Default 1.
	RebroadcastSafetyBlocks int64
	// PerGroupTimeout bounds a single group's reconcile (query + rebroadcasts).
	// Default 10s.
	PerGroupTimeout time.Duration

	// TxMaxConcurrent is the transaction client's permit count. It caps
	// MaxConcurrent so the pool cannot be wider than the number of broadcasts
	// that can actually be in flight. Zero leaves MaxConcurrent alone.
	TxMaxConcurrent int
}

// rebroadcastPersistTimeout bounds the write that records a resend attempt.
// Short on purpose: it runs on a context detached from the group's, so it must
// not become a way for shutdown to hang.
const rebroadcastPersistTimeout = 3 * time.Second

// DefaultInclusionReconcilerConfig returns sensible defaults.
func DefaultInclusionReconcilerConfig() InclusionReconcilerConfig {
	return InclusionReconcilerConfig{
		MaxConcurrent:           64,
		MaxRebroadcasts:         nil, // no cap: resend on every block the window allows
		RebroadcastSafetyBlocks: 0,   // close-1 is the last useful send; see canRebroadcast
		PerGroupTimeout:         10 * time.Second,
	}
}

// reconcilePhase holds the phase-specific behaviour, so the reconciler core is
// shared between claims and proofs (one mechanism, not two near-duplicate
// trackers). Built once in NewInclusionReconciler from the injected deps.
// inclusionVerdict is what ONE phase concludes about one session from what the
// chain says. The phases read the same map and disagree on purpose: a claim that
// exists is found for the claim phase whatever its proof status, while the proof
// phase only counts a VALIDATED one.
type inclusionVerdict uint8

const (
	// verdictMissing: not on chain in the sense THIS phase cares about. The
	// existing path -- resend while the window is open, record missing when it
	// closes. Every state a build does not recognise lands here, never on a
	// terminal one, so an enum value added upstream cannot silence a resend that
	// was still worth making.
	verdictMissing inclusionVerdict = iota
	// verdictFound: on chain in the sense this phase cares about.
	verdictFound
	// verdictRejected: the message reached the chain and the chain refused it.
	// Only the proof phase can reach this -- a claim that exists is found for the
	// claim phase whatever its proof status. Terminal FOR RESENDING THE SAME
	// BYTES, which is a narrower statement than it looks: see the branch that
	// handles it.
	verdictRejected
)

// claimPhaseVerdict and proofPhaseVerdict are the ONLY interpretations of an
// on-chain state, named here so production and tests share one copy. A hand copy
// in a test file is the shape this repository has already paid for: it compiles,
// it agrees with the original on the day it is written, and it stops agreeing
// silently -- the test then measures the copy and reports on the code.
//
// A claim that exists is found, whatever the chain thinks of its proof. This
// phase asks only whether the claim landed.
func claimPhaseVerdict(_ query.SessionProofState, present bool) inclusionVerdict {
	if present {
		return verdictFound
	}
	return verdictMissing
}

// Three answers, not two. VALIDATED is found; REJECTED reached the chain and was
// refused, so resending the same bytes is pointless; everything else -- pending,
// absent, or a status this build does not recognise -- keeps the missing path.
// Unknown lands with missing and NEVER with rejected: a value poktroll adds later
// must not be inferred to be a refusal and used to abandon a live session.
func proofPhaseVerdict(state query.SessionProofState, _ bool) inclusionVerdict {
	switch state {
	case query.SessionProofValidated:
		return verdictFound
	case query.SessionProofRejected:
		return verdictRejected
	default:
		return verdictMissing
	}
}

type reconcilePhase struct {
	phase             RebroadcastPhase
	windowCloseHeight func(p *sharedtypes.Params, sessionEnd int64) int64
	// verdict interprets one session's on-chain state FOR THIS PHASE. present is
	// false when the supplier has no claim for that session at all, which the
	// state alone cannot express -- the zero state is Unknown, and "absent" and
	// "unrecognised" must stay distinguishable for the claim phase even though
	// both mean "keep going" for the proof phase.
	verdict func(state query.SessionProofState, present bool) inclusionVerdict
	// recordOutcome persists the terminal outcome + emits the phase's outcome
	// metric. inclusionHeight is the poll-granularity height for a found outcome.
	//
	// It returns an error when the outcome could NOT be fully acted upon. The
	// caller uses that to keep the pending entry instead of clearing it: an
	// observation is the only thing that can rescue a session whose broadcast
	// reported failure, so losing one to a transient Redis error would be
	// permanent. Metric-only outcomes never fail.
	recordOutcome func(ctx context.Context, e rebroadcastEntry, supplier string, sessionEnd int64, sessionID, outcome string, inclusionHeight int64) error
	// recordRebroadcast emits the phase's rebroadcast metric.
	recordRebroadcast func(supplier, serviceID, result string)
	// diagnoseRejection runs ONCE when the chain refuses a message, carrying the
	// root the chain holds for that claim so the caller can compare it against
	// the one this miner stored. Nil for the claim phase, which has no rejection
	// to diagnose -- a claim that exists is found whatever its proof status.
	//
	// Separate from recordOutcome rather than an eighth argument to it: only one
	// phase has anything to say here, and widening the shared signature for it
	// would put a parameter that is always nil in front of every other caller.
	diagnoseRejection func(ctx context.Context, e rebroadcastEntry, supplier string, sessionEnd int64, sessionID string, height int64, onChainRoot []byte)
}

// inclusionOracle answers "what does the chain say about this supplier" ONCE per
// (supplier, height), for every group of BOTH phases in a single OnBlock pass.
//
// It exists because the two phases used to ask separately, walking the same
// AllClaims index for identical bytes and differing only in how they filtered
// them -- and the unit of that walk was the GROUP, keyed by (supplier, session
// end), so a supplier with pending entries at several session ends paid the walk
// once for each, per phase, per block. The deduplication key is therefore the
// supplier and the height, not the phase.
//
// The gate is a one-slot channel rather than a mutex so a waiter still honours
// its OWN deadline: groups each carry PerGroupTimeout, and blocking one group
// past its budget on another group's query is the shape that already cost this
// reconciler a burned retry budget. A waiter whose context expires leaves with
// its own error and takes the degraded path, exactly as a failed query does.
type inclusionOracle struct {
	height  int64
	fetch   func(ctx context.Context, supplier string) (map[string]query.SessionClaim, error)
	mu      sync.Mutex
	entries map[string]*oracleEntry
}

type oracleEntry struct {
	gate   chan struct{} // capacity 1: a context-aware mutex
	done   bool
	states map[string]query.SessionClaim
	err    error
}

func newInclusionOracle(height int64, fetch func(context.Context, string) (map[string]query.SessionClaim, error)) *inclusionOracle {
	return &inclusionOracle{height: height, fetch: fetch, entries: make(map[string]*oracleEntry)}
}

// states returns the supplier's session states for this pass, querying at most
// once however many groups and phases ask. A failed query is REMEMBERED for the
// pass: re-asking would send the same failing request to the same node in the
// same second, which is precisely when it is least affordable.
func (o *inclusionOracle) states(ctx context.Context, supplier string) (map[string]query.SessionClaim, error) {
	o.mu.Lock()
	e, ok := o.entries[supplier]
	if !ok {
		e = &oracleEntry{gate: make(chan struct{}, 1)}
		o.entries[supplier] = e
	}
	o.mu.Unlock()

	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.done {
		return e.states, e.err
	}
	e.states, e.err = o.fetch(ctx, supplier)
	e.done = true
	return e.states, e.err
}

// InclusionReconciler verifies on-chain inclusion of submitted claims/proofs and
// re-broadcasts those still missing while their window is open. It is
// block-driven: OnBlock runs one reconcile pass over the active per-supplier
// groups (from the RebroadcastStore index). Work per block is bounded by the
// number of suppliers with unconfirmed submissions — there is no per-session
// long-lived worker and no unbounded queue. State lives in Redis, so a new
// leader resumes verification after failover.
//
// Inclusion is resolved from x/proof module state (AllClaims/AllProofs by
// supplier), never the tx indexer, so it works on nodes with tx_index=null.
type InclusionReconciler struct {
	logger       logging.Logger
	sharedClient pocktclient.SharedQueryClient
	store        *RebroadcastStore
	resubmitter  MessageResubmitter
	cfg          InclusionReconcilerConfig

	claimPhase  reconcilePhase
	proofPhase  reconcilePhase
	fetchStates func(ctx context.Context, supplier string) (map[string]query.SessionClaim, error)

	pool pond.Pool

	// ownsSupplier filters groups to the suppliers THIS replica controls.
	// Claim/proof submission is coordinated by per-supplier ownership
	// (SupplierClaimer SetNX), NOT global leadership — so the reconciler must
	// only verify/record/rebroadcast its own suppliers, else replicas would
	// double-record outcomes and clear each other's state. nil means "own all"
	// (tests). A replica also only holds tx clients for owned suppliers, so a
	// non-owned rebroadcast would fail anyway; filtering up front avoids the
	// double-record. On failover the new owner reads the still-present Redis
	// entries and resumes.
	ownsSupplier func(supplier string) bool

	mu           sync.Mutex
	closed       bool
	lastHeight   atomic.Int64
	passInFlight atomic.Bool
}

// SetOwnershipFilter wires the per-supplier ownership predicate. runPass skips
// groups for suppliers this replica does not own.
func (r *InclusionReconciler) SetOwnershipFilter(ownsSupplier func(supplier string) bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.ownsSupplier = ownsSupplier
	r.mu.Unlock()
}

// NewInclusionReconciler builds the reconciler. claimPhase/proofPhase wire the
// phase-specific query, window, and outcome-recording behaviour.
func NewInclusionReconciler(
	logger logging.Logger,
	sharedClient pocktclient.SharedQueryClient,
	store *RebroadcastStore,
	resubmitter MessageResubmitter,
	claimPhase reconcilePhase,
	proofPhase reconcilePhase,
	// fetchStates is the single on-chain read both phases share. It is one
	// argument and not one per phase deliberately: two of them is what the pair
	// of queries this replaced looked like.
	fetchStates func(ctx context.Context, supplier string) (map[string]query.SessionClaim, error),
	cfg InclusionReconcilerConfig,
) *InclusionReconciler {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 64
	}
	if cfg.TxMaxConcurrent > 0 && cfg.MaxConcurrent > cfg.TxMaxConcurrent {
		cfg.MaxConcurrent = cfg.TxMaxConcurrent
	}
	// A negative cap is nonsense and is read as observe-only, the nearest
	// meaningful value. nil is left alone: it means no cap, which is the default.
	if cfg.MaxRebroadcasts != nil && *cfg.MaxRebroadcasts < 0 {
		zero := 0
		cfg.MaxRebroadcasts = &zero
	}
	if cfg.RebroadcastSafetyBlocks < 0 {
		cfg.RebroadcastSafetyBlocks = 0
	}
	if cfg.PerGroupTimeout <= 0 {
		cfg.PerGroupTimeout = 10 * time.Second
	}

	r := &InclusionReconciler{
		logger:       logging.ForComponent(logger, "inclusion_reconciler"),
		sharedClient: sharedClient,
		store:        store,
		resubmitter:  resubmitter,
		cfg:          cfg,
		claimPhase:   claimPhase,
		proofPhase:   proofPhase,
		fetchStates:  fetchStates,
	}
	// Blocking submit (no non-blocking drop): the active-group count is
	// bounded by #suppliers, so the pool drains within a block; we never
	// want to drop a group silently.
	r.pool = pond.NewPool(cfg.MaxConcurrent)
	return r
}

// OnBlock runs one reconcile pass for the given chain height across both phases.
// Single-flight: if a previous pass is still running (slow node / large set) the
// new block is skipped — the next block catches up. Driven by the miner's block
// event stream.
func (r *InclusionReconciler) OnBlock(height int64) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}

	// De-dupe duplicate block events at the same height.
	if prev := r.lastHeight.Load(); height <= prev {
		return
	}
	if !r.passInFlight.CompareAndSwap(false, true) {
		r.logger.Debug().Int64("height", height).Msg("inclusion reconcile pass still in flight; skipping (next block catches up)")
		return
	}
	r.lastHeight.Store(height)
	defer r.passInFlight.Store(false)

	// ONE oracle for the whole pass, so both phases and every group of a supplier
	// share a single walk of the AllClaims index at this height.
	oracle := newInclusionOracle(height, r.fetchStates)
	r.runPass(r.claimPhase, height, oracle)
	r.runPass(r.proofPhase, height, oracle)
}

// runPass reconciles every active group for one phase at the given height,
// fanning out across the bounded worker pool and waiting for the pass to finish.
func (r *InclusionReconciler) runPass(rp reconcilePhase, height int64, oracle *inclusionOracle) {
	ctx := context.Background()
	groups, err := r.store.ActiveGroups(ctx, rp.phase)
	if err != nil {
		inclusionGroupAbandonedTotal.WithLabelValues(string(rp.phase), abandonCauseIndexUnreadable).Inc()
		r.logger.Warn().Err(err).Str("phase", string(rp.phase)).Msg("inclusion reconcile: failed to list active groups")
		return
	}
	if len(groups) == 0 {
		return
	}

	r.mu.Lock()
	ownsSupplier := r.ownsSupplier
	r.mu.Unlock()

	group := r.pool.NewGroup()
	submitted := 0
	for _, g := range groups {
		// Only reconcile suppliers this replica owns (per-supplier ownership is
		// the submission coordination model; see ownsSupplier).
		if ownsSupplier != nil && !ownsSupplier(g.Supplier) {
			continue
		}
		g := g
		group.Submit(func() {
			r.reconcileGroup(rp, g, height, oracle)
		})
		submitted++
	}
	if submitted > 0 {
		// Discarding this error made a panic in reconcileGroup vanish -- no log, no
		// metric, no crash -- because pond recovers panics by default (pool.go:534)
		// and hands them back through this channel, against the repo's convention
		// that a recovered panic is counted AND logged (logging/recovery.go).
		//
		// The rule below is deliberately a rule and not a list. Tasks go in as
		// func(), so no task error is possible, but the channel still carries
		// ErrPoolStopped for a Submit made after the pool stopped (result.go:77-83),
		// ErrGroupStopped for a stopped group (group.go:12), and the context error if
		// the pool's context is cancelled -- and Close() marks the reconciler closed
		// BEFORE stopping the pool, so a pass already past that check can be
		// submitting while the pool goes down. Only a recovered panic is counted and
		// raised; anything else here is shutdown, and shutdown at Error would spend
		// the very signal this handling exists to create on every rollout.
		//
		// PanicRecoveriesTotal is enough and no loss-specific counter is added,
		// because the work is retried: a task that panicked never reached clear(), so
		// its entry stays pending and the next block's pass picks it up.
		if err := group.Wait(); err != nil {
			if errors.Is(err, pond.ErrPanic) {
				logging.PanicRecoveriesTotal.WithLabelValues("inclusion_reconcile_group").Inc()
				r.logger.Error().Err(err).Str("phase", string(rp.phase)).
					Msg("inclusion reconcile: a pass task panicked")
			} else {
				r.logger.Debug().Err(err).Str("phase", string(rp.phase)).
					Msg("inclusion reconcile: pass abandoned (pool shutting down)")
			}
		}
	}
}

// reconcileGroup verifies one supplier's batch for one session_end at the given
// height: query inclusion once, then for each still-pending session either
// record a terminal outcome (found / window-closed-missing) and clear it, or
// rebroadcast it (missing, window open, past the grace + safety gates).
func (r *InclusionReconciler) reconcileGroup(rp reconcilePhase, g RebroadcastGroup, height int64, oracle *inclusionOracle) {
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.PerGroupTimeout)
	defer cancel()

	pending, err := r.store.List(ctx, rp.phase, g.Supplier, g.SessionEnd)
	if err != nil {
		inclusionGroupAbandonedTotal.WithLabelValues(string(rp.phase), abandonCauseListFailed).Inc()
		r.logger.Warn().Err(err).Str("phase", string(rp.phase)).Str("supplier", g.Supplier).Msg("inclusion reconcile: failed to list pending payloads")
		return
	}
	if len(pending) == 0 {
		// Group drained (or TTL-expired) — reap any ghost index membership so
		// ActiveGroups doesn't keep returning it.
		if cErr := r.store.CleanupIfEmpty(ctx, rp.phase, g.Supplier, g.SessionEnd); cErr != nil {
			r.logger.Debug().Err(cErr).Str("phase", string(rp.phase)).Str("supplier", g.Supplier).Msg("inclusion reconcile: cleanup empty group failed")
		}
		return
	}

	params, err := r.sharedClient.GetParamsAtHeight(ctx, g.SessionEnd)
	if err != nil {
		// Can't compute the window; retry next block. If params never resolve the
		// payloads age out via TTL (no silent forfeit beyond observability gap).
		inclusionGroupAbandonedTotal.WithLabelValues(string(rp.phase), abandonCauseParamsFailed).Inc()
		r.logger.Warn().Err(err).Str("phase", string(rp.phase)).Int64("session_end", g.SessionEnd).Msg("inclusion reconcile: failed to get shared params")
		return
	}
	windowClose := rp.windowCloseHeight(params, g.SessionEnd)
	windowClosed := height > windowClose

	onChain, qErr := oracle.states(ctx, g.Supplier)
	if qErr != nil {
		if !windowClosed {
			// Window still open but we can't compute `missing` without a successful
			// query. Forfeiture is worse than a wasted resend (the chain rejects an
			// already-included claim/proof as a duplicate), so blind-rebroadcast the
			// gated pending instead of returning empty-handed.
			r.logger.Warn().Err(qErr).Str("phase", string(rp.phase)).Str("supplier", g.Supplier).
				Msg("inclusion reconcile: on-chain query failed with window open; blind-rebroadcasting pending (degraded mode)")
			for sessionID, raw := range pending {
				entry, decErr := unmarshalRebroadcastEntry(raw)
				if decErr != nil {
					// Was a bare continue: no log, no clear, no outcome, no
					// metric. It leaves the entry in place, so the same
					// undecodable payload is met again on every block for as
					// long as the query keeps failing with the window open.
					inclusionEntryDroppedTotal.WithLabelValues(string(rp.phase), dropCauseCorrupt).Inc()
					r.logger.Warn().Err(decErr).Str("phase", string(rp.phase)).Str("session_id", sessionID).
						Msg("inclusion reconcile: corrupt rebroadcast entry in degraded mode; dropping")
					r.clear(ctx, rp.phase, g, sessionID)
					continue
				}
				if r.canRebroadcast(entry, height, windowClose) {
					r.rebroadcast(ctx, rp, g, sessionID, entry, height, windowClose)
				}
			}
			return
		}
		// Window closed and we still can't confirm — record poll_error so the
		// outcome is not silently lost, then clear.
		for sessionID, raw := range pending {
			e, decErr := unmarshalRebroadcastEntry(raw)
			if decErr != nil {
				inclusionEntryDroppedTotal.WithLabelValues(string(rp.phase), dropCauseCorrupt).Inc()
				r.logger.Warn().Err(decErr).Str("phase", string(rp.phase)).Str("session_id", sessionID).
					Msg("inclusion reconcile: corrupt rebroadcast entry with window closed; dropping")
				r.clear(ctx, rp.phase, g, sessionID)
				continue
			}
			// The discard is safe by STRUCTURE, not by luck, and there is no test holding
			// it -- so this says what it rests on. recordOutcome returns a non-nil error
			// only from reactivateClaimedSession, which sits inside `if outcome ==
			// inclusionFound` in recordClaimOutcome; recordProofOutcome has no error path at
			// all. This call passes inclusionPollErr, so the value is invariantly nil. The one
			// caller that DOES pass inclusionFound checks it, keeps the entry and retries.
			//
			// Three edits break that, and none of them would fail a test: moving the
			// `return err` out of the inclusionFound branch, giving recordProofOutcome an
			// error path (item 37 would), or a new caller passing inclusionFound here.
			_ = rp.recordOutcome(ctx, e, g.Supplier, g.SessionEnd, sessionID, inclusionPollErr, 0) //nolint:errcheck // invariantly nil here; see above
			r.clear(ctx, rp.phase, g, sessionID)
		}
		return
	}

	for sessionID, raw := range pending {
		// Stop on the first entry that finds the budget gone. Continuing would
		// walk the rest of the group taking the same exit on every one, and
		// nothing below this point can succeed on an expired context -- the
		// outcome writes and the clears use it too.
		//
		// Debug and not Warn, unlike its four siblings: those report a failing
		// dependency and are rare, while this one fires once per group per block
		// for as long as the timeout stays too small. The metric is the
		// alertable signal here, which is the rule this repo already applies to
		// anything that can repeat per cycle.
		if ctxErr := ctx.Err(); ctxErr != nil {
			inclusionGroupAbandonedTotal.WithLabelValues(string(rp.phase), abandonCauseBudgetExhausted).Inc()
			r.logger.Debug().Err(ctxErr).
				Str("phase", string(rp.phase)).
				Str("supplier", g.Supplier).
				Msg("inclusion reconcile: group budget spent; the rest of this group waits for the next block")
			break
		}

		entry, decErr := unmarshalRebroadcastEntry(raw)
		if decErr != nil {
			inclusionEntryDroppedTotal.WithLabelValues(string(rp.phase), dropCauseCorrupt).Inc()
			r.logger.Warn().Err(decErr).Str("session_id", sessionID).Msg("inclusion reconcile: corrupt rebroadcast entry; dropping")
			r.clear(ctx, rp.phase, g, sessionID)
			continue
		}

		claim, present := onChain[sessionID]
		if rp.verdict(claim.ProofState, present) == verdictFound {
			if oErr := rp.recordOutcome(ctx, entry, g.Supplier, g.SessionEnd, sessionID, inclusionFound, height); oErr != nil {
				// KEEP the entry. The claim IS on-chain; acting on that
				// observation is what keeps the proof coming, so a transient
				// failure must get another block rather than be cleared away.
				// Only the entry TTL bounds this retrying: a found outcome
				// `continue`s and never reaches rebroadcast(), so the
				// MaxRebroadcasts cap is not what holds it — do not read this
				// as doubly bounded.
				r.logger.Warn().Err(oErr).
					Str("phase", string(rp.phase)).
					Str("supplier", g.Supplier).
					Str("session_id", sessionID).
					Msg("inclusion reconcile: on-chain outcome observed but not fully recorded; keeping entry for retry")
				continue
			}
			r.clear(ctx, rp.phase, g, sessionID)
			continue
		}

		if rp.verdict(claim.ProofState, present) == verdictRejected {
			// The chain executed this proof and refused it. Resending the SAME
			// bytes cannot change that: none of the seven rejection causes
			// depends on WHEN the message is sent -- the ring is built at the
			// session end height, the closest path from a block hash anchored to
			// the session, and the root comes from the claim -- so the same bytes
			// against the same claim produce the same verdict with certainty.
			// That is determinism, not caution.
			//
			// It does NOT mean the chain closed the door. validateProof
			// overwrites ProofValidationStatus without reading the previous one,
			// so a DIFFERENT, valid proof inside the same window still flips this
			// to VALIDATED. What closes the door is us: OnSessionProved deletes
			// the SMST as soon as the proof transaction goes out, before the
			// EndBlocker rules, so by the time we read INVALID the tree the proof
			// was built from no longer exists. Read "terminal" as "we have
			// nothing left to build a better proof from", never as "impossible".
			//
			// The clear is not hygiene, it is half the fix. When the on-chain
			// read FAILS with the window still open, this loop is skipped
			// entirely and a degraded path blind-rebroadcasts everything still
			// pending -- deliberately, because a forfeit is worse than a wasted
			// resend. That path never consults the oracle, so a rejection it
			// cannot see would be resent on the first block whose query fails.
			// Deleting the entry is what makes the verdict outlive the oracle.
			//
			// Which is also why a FAILED clear matters more here than in the
			// sibling case below it. There, the comment can say the next block
			// walks the same path to the same verdict; here that is only true
			// while queries succeed, so inclusionClearFailedTotal stops being
			// observability and becomes the only remaining net.
			//
			// Recorded at the height it was OBSERVED rather than at window close:
			// this is the one verdict a human could still act on while the window
			// is open, and a record that arrives after it closes arrives after
			// anything could be done.
			//
			// The attempt is not counted, and it is worth saying exactly what
			// holds that today: this branch records, diagnoses, clears and
			// continues, so it never writes the entry back. The count does not
			// survive because nothing persists it -- it is a property of the
			// path, not a decision to skip an increment. The reason it SHOULD
			// stay uncounted is the same as its "no proof was required" sibling:
			// this one reached the network and can never succeed, so spending a
			// resend from the budget would charge the session for a decision the
			// chain already made. Anyone adding a persist here has to make that
			// reason explicit, because the guarantee will stop being free.
			_ = rp.recordOutcome(ctx, entry, g.Supplier, g.SessionEnd, sessionID, inclusionRejected, height) //nolint:errcheck // invariantly nil: recordOutcome only errors inside its inclusionFound branch
			if rp.diagnoseRejection != nil {
				rp.diagnoseRejection(ctx, entry, g.Supplier, g.SessionEnd, sessionID, height, claim.RootHash)
			}
			r.clear(ctx, rp.phase, g, sessionID)
			continue
		}

		// Missing on-chain.
		if windowClosed {
			// The window is over and this session never landed, so a resend was
			// always warranted -- a session found on-chain `continue`s above and
			// never reaches here. Budget left over therefore says the schedule
			// could not spend it, not that it was not needed, and those two have
			// to give different signals: an operator who configures a cap of 2
			// and observes one resend can otherwise only guess which happened.
			// The common cause is a window too short to hold the spacing (the
			// chain's own default close offset is 4 blocks, not the 10 mainnet
			// and localnet use), and a claim submitted late by the retry loop
			// shortens it further.
			// Only meaningful when a cap exists. With no cap there is no unused
			// budget to report -- every entry would qualify, and a metric that
			// fires on everything says nothing.
			if r.cfg.MaxRebroadcasts != nil && entry.Rebroadcasts < *r.cfg.MaxRebroadcasts {
				inclusionResendCapUnusedTotal.WithLabelValues(string(rp.phase)).Inc()
			}
			// The discard is safe by STRUCTURE, not by luck, and there is no test holding
			// it -- so this says what it rests on. recordOutcome returns a non-nil error
			// only from reactivateClaimedSession, which sits inside `if outcome ==
			// inclusionFound` in recordClaimOutcome; recordProofOutcome has no error path at
			// all. This call passes inclusionMissing, so the value is invariantly nil. The one
			// caller that DOES pass inclusionFound checks it, keeps the entry and retries.
			//
			// Three edits break that, and none of them would fail a test: moving the
			// `return err` out of the inclusionFound branch, giving recordProofOutcome an
			// error path (item 37 would), or a new caller passing inclusionFound here.
			_ = rp.recordOutcome(ctx, entry, g.Supplier, g.SessionEnd, sessionID, inclusionMissing, 0) //nolint:errcheck // invariantly nil here; see above
			r.clear(ctx, rp.phase, g, sessionID)
			continue
		}

		// Window still open → resend on the calendar if still missing.
		if r.canRebroadcast(entry, height, windowClose) {
			r.rebroadcast(ctx, rp, g, sessionID, entry, height, windowClose)
		}
	}
}

// canRebroadcast is the resend gate, and it now asks one question: is the window
// still open with room for the transaction to land?
//
// It used to ask three -- a cap, a per-entry schedule, and the window -- and the
// first two existed for reasons that no longer hold. The cap and the spacing
// were there because a resend was expensive and possibly harmful: it cost a
// simulation and a signature, and a redundant one looked to the caller exactly
// like a failure. Neither is true any more. A transaction the node already holds
// is refused for free as code 19, recognised and exempt from counting; and the
// window close is enforced by the chain itself through timeout_height, so a late
// resend cannot be accepted no matter who sends it.
//
// What was being protected by resending three times out of ten blocks was the
// wrong thing. A claim that never lands earns nothing, and the resend that
// matters is the one after the block that lost it -- which the old schedule
// could not know in advance and therefore mostly missed.
//
// TWO GUARDS SURVIVE, and both are about not spending on the impossible:
//
//   - height > SubmitHeight: never resend in the same block the original left
//     in. Inside one block the timeout anchor does not move, so the resend would
//     carry the same unordered nonce and be refused as a duplicate of itself.
//   - height < windowClose - RebroadcastSafetyBlocks, with the safety at 0: the
//     last useful send is at close-1, because a transaction sent there can still
//     be included in close. Sending AT close cannot be included by anything.
//
// The count is still persisted on the entry, and still bounds resends when an
// operator sets an explicit cap.
func (r *InclusionReconciler) canRebroadcast(entry rebroadcastEntry, height, windowClose int64) bool {
	if r.cfg.MaxRebroadcasts != nil && entry.Rebroadcasts >= *r.cfg.MaxRebroadcasts {
		return false
	}
	return height > entry.SubmitHeight && height < windowClose-r.cfg.RebroadcastSafetyBlocks
}

// rebroadcast resends one session's stored message once, incrementing and
// persisting the resend count (so the MaxRebroadcasts cap holds across blocks
// and across leader failover) and refreshing the latest tx hash. Outcome
// recording happens later when the claim/proof lands or the window closes.
func (r *InclusionReconciler) rebroadcast(ctx context.Context, rp reconcilePhase, g RebroadcastGroup, sessionID string, entry rebroadcastEntry, height, windowClose int64) {
	if r.resubmitter == nil {
		return
	}

	// The group's whole budget -- PerGroupTimeout -- covers the listing, the
	// inclusion query AND every resend in this group, in series. Once it is
	// spent, the entries still queued behind it would each get a resend that
	// fails with a context error, and the counter does NOT exempt that: the
	// sentinel only exempts a saturated permit. Each of those would burn one of
	// the few attempts a claim has, without a message ever being signed or sent,
	// and the persist below (deliberately on its own context) would make the
	// burn survive.
	//
	// Checked BEFORE the call rather than inferred from the error afterwards,
	// because the two are not the same question. A deadline that expires DURING
	// the broadcast leaves a signed message that may well be in the network, and
	// that IS an attempt -- counting it is correct. Only "I never got to try" is
	// exempt, and the only way to know that is to ask before trying.
	if err := ctx.Err(); err != nil {
		r.logger.Debug().Err(err).
			Str("phase", string(rp.phase)).
			Str("session_id", sessionID).
			Msg("inclusion reconcile: group budget spent before this resend; leaving the entry untouched")
		return
	}

	newHash, err := r.resubmitter.ResubmitMessage(ctx, rp.phase, g.Supplier, entry.MsgBytes, windowClose,
		time.Duration(entry.TimeoutSeconds)*time.Second, entry.TimeoutRegime)

	// The chain says this proof is not required. Mirror image of the saturation
	// case below: that one never reached the network, this one did and can never
	// succeed -- the requirement is seeded from a fixed block hash and read with
	// params at the session's own heights, so every future resend asks the same
	// question. There is no inclusion left to verify, so the entry is DROPPED
	// rather than counted. Counting would happen to work only because
	// MaxRebroadcasts is small; the entry would still be re-read on every
	// block until the window closes.
	//
	// A context of its own, for the same reason the persist below has one: the
	// group context may already be expired by the send. And if the delete fails
	// it is logged and dropped -- what keeps the proof from being re-sent then is
	// that the next block walks the same path to the same verdict, not the
	// delete having succeeded.
	if errors.Is(err, tx.ErrTxProofNotRequired) {
		clearCtx, cancelClear := context.WithTimeout(context.WithoutCancel(ctx), rebroadcastPersistTimeout)
		r.clear(clearCtx, rp.phase, g, sessionID)
		cancelClear()
		rp.recordRebroadcast(g.Supplier, entry.ServiceID, "not_required")
		return
	}

	// Count this attempt and persist it, so MaxRebroadcasts bounds the total
	// number of resend tries. Without counting failures, a persistently failing
	// resend (e.g. a CUPR-doomed claim whose gas simulation always fails) would
	// re-fire — and re-log — on every block until the window closes. Persisting
	// also keeps the cap across leader failover.
	//
	// EXCEPT for the two answers that mean NOTHING WAS SPENT. The budget is a
	// handful of resends, so counting an attempt that changed nothing burns one
	// of the few a claim had — and it moves the calendar too, pushing the next
	// real attempt further out for a send that did not happen. A sentinel is the
	// only thing that can tell these apart from a genuine rejection, because on
	// the wire they look like any other refusal.
	//
	//   - SATURATION: no permit was free, so the message was never signed and
	//     never sent. The next block finds the payload exactly where it was.
	//   - ALREADY QUEUED: the node answered "I already hold this transaction"
	//     without transmitting anything. Nothing was consumed and nothing
	//     changed, so the previous send is still the one in flight.
	//
	// The second one is not an edge case: re-sending to the same node while its
	// mempool still holds the transaction is the EXPECTED answer, and the
	// commonest one once resends happen on every block. Counting it would spend
	// the whole budget on a claim whose transaction was already on its way,
	// silently and without a single packet leaving for the chain -- which is the
	// most expensive way to be wrong here, because it looks like progress.
	if !errors.Is(err, tx.ErrTxConcurrencySaturated) && !errors.Is(err, tx.ErrTxAlreadyQueued) {
		entry.Rebroadcasts++
		// Inside this guard and NOT beside the TxHash assignment below, which
		// sits outside it: a saturated attempt never left the process, so moving
		// the calendar for it would spend two blocks of the window on a send
		// that did not happen. The two fields move together for that reason --
		// the count and the schedule answer the same question.
		entry.LastAttemptHeight = height
	}
	if err == nil && newHash != "" {
		entry.TxHash = newHash
	}
	if b, mErr := marshalRebroadcastEntry(entry); mErr == nil {
		// Persist with a context of its own. The group context may already be
		// expired by the send above -- PerGroupTimeout bounds the whole group --
		// and reusing it means the attempt happens but is never recorded, so the
		// next block resends again and the cap does not hold from the other
		// side either. The counter has to reflect what actually happened.
		putCtx, cancelPut := context.WithTimeout(context.WithoutCancel(ctx), rebroadcastPersistTimeout)
		if pErr := r.store.Put(putCtx, rp.phase, g.Supplier, g.SessionEnd, sessionID, b); pErr != nil {
			r.logger.Warn().Err(pErr).Str("session_id", sessionID).Msg("inclusion reconcile: failed to persist resend count/hash")
		}
		cancelPut()
	}

	if err != nil {
		// A window that closed under us is not a transport failure, and until
		// now both landed here as result="error" -- one bucket holding "the node
		// was unreachable", which the next block may fix, together with "these
		// bytes can never be accepted again", which nothing fixes. Separating
		// them costs one label value, and it is the only way an operator can
		// tell a flapping endpoint from a resend calendar that runs too late.
		//
		// It changes no control flow. The attempt was already counted and the
		// entry already persisted above, both deliberately: a doomed resend must
		// still spend its attempt or it re-fires on every block until the window
		// closes, which is the policy written where that counter lives.
		//
		// The margin being reported on is thin by construction --
		// RebroadcastSafetyBlocks defaults to 0, so canRebroadcast authorises a
		// resend up to windowClose-1 and the transaction has one block to land.
		result := "error"
		switch {
		case errors.Is(err, tx.ErrTxWindowExpired):
			result = "window_closed"
		case errors.Is(err, tx.ErrTxAlreadyQueued):
			// Not an error at all: the node already holds the transaction. It
			// gets its own value rather than sharing "error" because once
			// resends run every block this becomes the commonest outcome, and
			// leaving it in the error bucket would bury a real failure under a
			// rate that only says the loop is working.
			result = "already_queued"
		}
		rp.recordRebroadcast(g.Supplier, entry.ServiceID, result)
		// Debug, not Warn: the failure is already captured by the
		// claimRebroadcastsTotal{result=...} metric, and the attempt is now
		// capped above, so this no longer repeats every block. Expected-transient
		// (mempool reject / doomed claim) — not something needing an operator alert.
		r.logger.Debug().Err(err).
			Str("phase", string(rp.phase)).
			Str("supplier", g.Supplier).
			Str("session_id", sessionID).
			Str("result", result).
			Int("attempt", entry.Rebroadcasts).
			Msg("inclusion reconcile: rebroadcast failed")
		return
	}

	rp.recordRebroadcast(g.Supplier, entry.ServiceID, "success")
	r.logger.Info().
		Str("phase", string(rp.phase)).
		Str("supplier", g.Supplier).
		Str("session_id", sessionID).
		Int("attempt", entry.Rebroadcasts).
		Int64("height", height).
		Int64("window_close", windowClose).
		Str("new_tx_hash", newHash).
		Msg("rebroadcast (accepted to mempool but not yet on-chain)")
}

func (r *InclusionReconciler) clear(ctx context.Context, phase RebroadcastPhase, g RebroadcastGroup, sessionID string) {
	if err := r.store.Delete(ctx, phase, g.Supplier, g.SessionEnd, sessionID); err != nil {
		// The entry survives, so the next block reconciles it again and emits
		// its outcome a second time. Counting it is what makes that visible:
		// the log alone cannot be alerted on, and a duplicated outcome is
		// otherwise indistinguishable from two real ones.
		inclusionClearFailedTotal.WithLabelValues(string(phase)).Inc()
		r.logger.Warn().Err(err).Str("phase", string(phase)).Str("session_id", sessionID).Msg("inclusion reconcile: failed to clear pending entry")
	}
}

// Close drains the worker pool. Idempotent.
func (r *InclusionReconciler) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.pool.StopAndWait()
	return nil
}
