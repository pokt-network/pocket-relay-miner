package miner

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alitto/pond/v2"

	"github.com/pokt-network/pocket-relay-miner/logging"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// InclusionOutcome values are stable strings — metric labels and
// submission-tracker JSON depend on them.
const (
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
}

func marshalRebroadcastEntry(e rebroadcastEntry) ([]byte, error) { return json.Marshal(e) }

func unmarshalRebroadcastEntry(b []byte) (rebroadcastEntry, error) {
	var e rebroadcastEntry
	err := json.Unmarshal(b, &e)
	return e, err
}

// InclusionQueryClient is the per-supplier inclusion signal the reconciler
// needs. *query.proofQueryClient satisfies it. Kept narrow for unit testing.
//
// Both signals read x/proof module state via the AllClaims supplier secondary
// index, NOT proofs: a submitted proof is validated and removed in the
// EndBlocker of its submission block, so proof inclusion is read from the
// claim's ProofValidationStatus (see query.GetSupplierProvenSessions), which is
// durable until settlement.
type InclusionQueryClient interface {
	// GetSupplierClaimSessions: sessions with a claim on-chain (claim phase).
	GetSupplierClaimSessions(ctx context.Context, supplier string) (map[string]struct{}, error)
	// GetSupplierProvenSessions: sessions whose claim is proof-VALIDATED (proof phase).
	GetSupplierProvenSessions(ctx context.Context, supplier string) (map[string]struct{}, error)
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
	// Disabled turns the reconciler off entirely.
	Disabled bool
	// MaxConcurrent bounds the per-block group-reconcile worker pool. Default 64.
	MaxConcurrent int
	// MaxRebroadcasts caps how many times a still-missing claim/proof is
	// re-submitted within its window. Default 1 (a single resend at mid-window
	// — worst case gas x2). 0 = observe-only (record outcomes, never resend).
	//
	// Why a single mid-window resend (not per-block): txs are unordered with a
	// block-time-anchored timeout that spans ~the whole window, so the original
	// is valid in any later (empty) block until it times out. Re-sending every
	// block would build a fresh tx each time (new timeout → new hash, not
	// deduped) and flood the mempool with copies that mostly fail DeliverTx as
	// duplicate proofs. The only failure the resend fixes is mempool eviction
	// during the submit-block burst; one resend into a later empty block
	// recovers it.
	MaxRebroadcasts int
	// RebroadcastSafetyBlocks stops rebroadcasting once the chain is within this
	// many blocks of window-close (a resend cannot land after the window).
	// Default 1.
	RebroadcastSafetyBlocks int64
	// PerGroupTimeout bounds a single group's reconcile (query + rebroadcasts).
	// Default 10s.
	PerGroupTimeout time.Duration
}

// DefaultInclusionReconcilerConfig returns sensible defaults.
func DefaultInclusionReconcilerConfig() InclusionReconcilerConfig {
	return InclusionReconcilerConfig{
		MaxConcurrent:           64,
		MaxRebroadcasts:         1,
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
	recordOutcome func(ctx context.Context, e rebroadcastEntry, supplier string, sessionEnd int64, sessionID, outcome string, inclusionHeight int64)
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
	if !cfg.Disabled {
		// Blocking submit (no non-blocking drop): the active-group count is
		// bounded by #suppliers, so the pool drains within a block; we never
		// want to drop a group silently.
		r.pool = pond.NewPool(cfg.MaxConcurrent)
	}
	return r
}

// OnBlock runs one reconcile pass for the given chain height across both phases.
// Single-flight: if a previous pass is still running (slow node / large set) the
// new block is skipped — the next block catches up. Driven by the miner's block
// event stream.
func (r *InclusionReconciler) OnBlock(height int64) {
	if r == nil || r.cfg.Disabled || r.pool == nil {
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
		_ = group.Wait()
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
				r.clear(ctx, rp.phase, g, sessionID)
				continue
			}
			rp.recordOutcome(ctx, e, g.Supplier, g.SessionEnd, sessionID, inclusionPollErr, 0)
			r.clear(ctx, rp.phase, g, sessionID)
		}
		return
	}

	for sessionID, raw := range pending {
		entry, decErr := unmarshalRebroadcastEntry(raw)
		if decErr != nil {
			r.logger.Warn().Err(decErr).Str("session_id", sessionID).Msg("inclusion reconcile: corrupt rebroadcast entry; dropping")
			r.clear(ctx, rp.phase, g, sessionID)
			continue
		}

		if _, ok := onChain[sessionID]; ok {
			rp.recordOutcome(ctx, entry, g.Supplier, g.SessionEnd, sessionID, inclusionFound, height)
			r.clear(ctx, rp.phase, g, sessionID)
			continue
		}

		// Missing on-chain.
		if windowClosed {
			rp.recordOutcome(ctx, entry, g.Supplier, g.SessionEnd, sessionID, inclusionMissing, 0)
			r.clear(ctx, rp.phase, g, sessionID)
			continue
		}

		// Window still open → single resend at mid-window if still missing.
		if r.canRebroadcast(entry, height, windowClose) {
			r.rebroadcast(ctx, rp, g, sessionID, entry, height, windowClose)
		}
	}
}

// canRebroadcast is the resend gate: a single resend at the window midpoint.
// The original tx is unordered with a block-time-anchored timeout spanning ~the
// whole window, so it is valid in any later (empty) block until it times out —
// re-sending earlier or repeatedly just floods the mempool with duplicates that
// fail DeliverTx. We wait until the midpoint (giving the original maximal chance
// to land on its own), then, if still missing and the resend can still land
// before window-close (safety margin), re-send exactly once (MaxRebroadcasts,
// default 1). The count is persisted on the entry, so the cap survives failover.
func (r *InclusionReconciler) canRebroadcast(entry rebroadcastEntry, height, windowClose int64) bool {
	if entry.Rebroadcasts >= r.cfg.MaxRebroadcasts {
		return false
	}
	var threshold int64
	if entry.OrigTxHash == "" {
		// The original submit FAILED (build-OK but tx never accepted: gap,
		// lazyload-at-submit, transient error). Nothing is in flight, so this is
		// an emergency self-heal — resend as soon as past a 1-block grace rather
		// than waiting for the midpoint.
		threshold = entry.SubmitHeight + 1
	} else {
		// The original was broadcast OK and is unordered with a window-spanning
		// timeout, so it may still land in any later (empty) block. Give it until
		// the window midpoint before a single self-try resend.
		threshold = entry.SubmitHeight + (windowClose-entry.SubmitHeight)/2
	}
	return height >= threshold && height < windowClose-r.cfg.RebroadcastSafetyBlocks
}

// rebroadcast resends one session's stored message once, incrementing and
// persisting the resend count (so the MaxRebroadcasts cap holds across blocks
// and across leader failover) and refreshing the latest tx hash. Outcome
// recording happens later when the claim/proof lands or the window closes.
func (r *InclusionReconciler) rebroadcast(ctx context.Context, rp reconcilePhase, g RebroadcastGroup, sessionID string, entry rebroadcastEntry, height, windowClose int64) {
	if r.resubmitter == nil {
		return
	}
	newHash, err := r.resubmitter.ResubmitMessage(ctx, rp.phase, g.Supplier, entry.MsgBytes, windowClose)

	// Count this attempt regardless of outcome and persist it, so MaxRebroadcasts
	// bounds the total number of resend tries. Without counting failures, a
	// persistently failing resend (e.g. a CUPR-doomed claim whose gas simulation
	// always fails) would re-fire — and re-log — on every block until the window
	// closes. Persisting also keeps the cap across leader failover.
	entry.Rebroadcasts++
	if err == nil && newHash != "" {
		entry.TxHash = newHash
	}
	if b, mErr := marshalRebroadcastEntry(entry); mErr == nil {
		if pErr := r.store.Put(ctx, rp.phase, g.Supplier, g.SessionEnd, sessionID, b); pErr != nil {
			r.logger.Warn().Err(pErr).Str("session_id", sessionID).Msg("inclusion reconcile: failed to persist resend count/hash")
		}
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
	if r.pool != nil {
		r.pool.StopAndWait()
	}
	return nil
}
