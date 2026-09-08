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

	// resendSpacingBlocks is the minimum gap between two resends of the same
	// entry: one block for the resend to be included, one for the oracle to
	// observe it. Without it the release condition stays true on every later
	// block, so a cap above 1 spends its attempts back to back and buys only
	// fees -- which is why the design says the cap and the spacing are ONE
	// change, not two.
	resendSpacingBlocks = 2

	inclusionFound   = "on_chain_found"
	inclusionMissing = "on_chain_missing"
	inclusionPollErr = "poll_error"
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
	ResubmitMessage(ctx context.Context, phase RebroadcastPhase, supplier string, msgBytes []byte, timeoutHeight int64) (newTxHash string, err error)
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
	// re-submitted within its window: 2 by default, the first due at mid-window
	// and each one spaced resendSpacingBlocks after the last attempt that
	// actually went out. Worst case is that many times the gas. 0 = observe-only
	// (record outcomes, never resend).
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
	MaxRebroadcasts int
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
		MaxRebroadcasts:         2,
		RebroadcastSafetyBlocks: 1,
		PerGroupTimeout:         10 * time.Second,
	}
}

// reconcilePhase holds the phase-specific behaviour, so the reconciler core is
// shared between claims and proofs (one mechanism, not two near-duplicate
// trackers). Built once in NewInclusionReconciler from the injected deps.
type reconcilePhase struct {
	phase             RebroadcastPhase
	windowCloseHeight func(p *sharedtypes.Params, sessionEnd int64) int64
	onChainSessions   func(ctx context.Context, supplier string) (map[string]struct{}, error)
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

	claimPhase reconcilePhase
	proofPhase reconcilePhase

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
	cfg InclusionReconcilerConfig,
) *InclusionReconciler {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 64
	}
	if cfg.TxMaxConcurrent > 0 && cfg.MaxConcurrent > cfg.TxMaxConcurrent {
		cfg.MaxConcurrent = cfg.TxMaxConcurrent
	}
	if cfg.MaxRebroadcasts < 0 {
		cfg.MaxRebroadcasts = 0
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
	// Settled by the wiring change, and the answer is narrower than the question
	// it was left open for. ONE path does leave m.inclusionReconciler nil -- a
	// manager built without a ProofQueryClient -- but that same path returns
	// before the block loop is ever started, so OnBlock is never CALLED on a nil
	// receiver; production binds the method value only after the assignment, and
	// every test receiver is constructed. So nothing reaches this, and it is not
	// deleted for one reason that is about blast radius rather than coverage: the
	// goroutine driving this loop is a bare `go func()` with no panic recovery,
	// so a future caller that did reach it would take the process down instead of
	// losing a reconcile pass. The guards that actually work are r.closed under
	// the mutex and the single-flight below.
	if r == nil {
		return
	}
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

	r.runPass(r.claimPhase, height)
	r.runPass(r.proofPhase, height)
}

// runPass reconciles every active group for one phase at the given height,
// fanning out across the bounded worker pool and waiting for the pass to finish.
func (r *InclusionReconciler) runPass(rp reconcilePhase, height int64) {
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
			r.reconcileGroup(rp, g, height)
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
func (r *InclusionReconciler) reconcileGroup(rp reconcilePhase, g RebroadcastGroup, height int64) {
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

	onChain, qErr := rp.onChainSessions(ctx, g.Supplier)
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

		if _, ok := onChain[sessionID]; ok {
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
			if entry.Rebroadcasts < r.cfg.MaxRebroadcasts {
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

// canRebroadcast is the resend gate: at most MaxRebroadcasts resends, each no
// earlier than its threshold and none close enough to window-close that it could
// not land. The count is persisted on the entry, so the cap survives failover.
func (r *InclusionReconciler) canRebroadcast(entry rebroadcastEntry, height, windowClose int64) bool {
	if entry.Rebroadcasts >= r.cfg.MaxRebroadcasts {
		return false
	}
	return height >= r.resendThreshold(entry, windowClose) && height < windowClose-r.cfg.RebroadcastSafetyBlocks
}

// resendThreshold is the earliest height this entry's NEXT resend may go out.
//
// Two rules compose it, and the second is the one that makes a cap above 1 mean
// anything:
//
//  1. The BASE, which depends on whether anything is in flight. A tx that was
//     broadcast OK is unordered with a window-spanning timeout, so it may still
//     land in any later (empty) block: it gets until the window midpoint on its
//     own, because re-sending earlier just floods the mempool with duplicates
//     that fail DeliverTx. A tx that never reached the network (OrigTxHash empty:
//     gap, lazyload-at-submit, transient error) has nothing in flight, so waiting
//     buys nothing and it resends after a 1-block grace.
//  2. The SPACING: resendSpacingBlocks after the attempt that actually went out.
//     `height >= base` alone stays true on every later block, so without this the
//     second attempt leaves in the block after the first.
//
// When the spacing no longer fits, the threshold is COMPRESSED to the last height
// the guard allows rather than the attempt being abandoned. The reason is a
// division of authority: the guard decides whether a send can still land, the
// spacing is only a preference about when it is worth sending, and a preference
// must not veto what the authority permits. A compressed resend costs a duplicate
// fee on an idempotent upsert (§0 of the design accepts exactly that trade);
// dropping the attempt costs the claim. The compression never reaches below the
// base, so a first resend is timed exactly as it was before this rule existed.
//
// With LastAttemptHeight unset -- an entry written by a binary that did not have
// the field, or one an older binary rewrote and stripped -- rule 2 does not
// apply and the schedule is the nominal one.
func (r *InclusionReconciler) resendThreshold(entry rebroadcastEntry, windowClose int64) int64 {
	base := entry.SubmitHeight + (windowClose-entry.SubmitHeight)/2
	if entry.OrigTxHash == "" {
		base = entry.SubmitHeight + 1
	}
	// Spaced from the height the last attempt REALLY went out at when the entry
	// carries it, and from the nominal schedule when it does not. The nominal
	// form assumes each previous attempt left at its own threshold, which is the
	// assumption LastAttemptHeight exists to remove -- but it still spaces, and
	// that is the whole point: falling back to a bare `base` would leave
	// `height >= base` already satisfied for an entry that has resent once, so
	// the next attempt would go out on the very next block. That is not a weaker
	// schedule, it is the defect this function was written to close.
	spaced := base + resendSpacingBlocks*int64(entry.Rebroadcasts)
	if entry.LastAttemptHeight > 0 {
		spaced = entry.LastAttemptHeight + resendSpacingBlocks
	}
	if spaced <= base {
		return base
	}
	if lastAllowed := windowClose - r.cfg.RebroadcastSafetyBlocks - 1; spaced > lastAllowed && lastAllowed > base {
		return lastAllowed
	}
	return spaced
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

	newHash, err := r.resubmitter.ResubmitMessage(ctx, rp.phase, g.Supplier, entry.MsgBytes, windowClose)

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
	// EXCEPT when we never reached the network. The budget is a handful of
	// resends, so counting an attempt that never left the process burns one of
	// the few a claim had, on nothing — and it would move the calendar too,
	// pushing the next real attempt two blocks later for a send that did not
	// happen. The sentinel is the only thing that
	// can tell "the chain rejected it" from "we never asked": saturation means
	// no permit was free, the message was never signed and never sent, and the
	// next block will find the payload exactly where it was.
	if !errors.Is(err, tx.ErrTxConcurrencySaturated) {
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
		rp.recordRebroadcast(g.Supplier, entry.ServiceID, "error")
		// Debug, not Warn: the failure is already captured by the
		// claimRebroadcastsTotal{result="error"} metric, and the attempt is now
		// capped above, so this no longer repeats every block. Expected-transient
		// (mempool reject / doomed claim) — not something needing an operator alert.
		r.logger.Debug().Err(err).
			Str("phase", string(rp.phase)).
			Str("supplier", g.Supplier).
			Str("session_id", sessionID).
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
