package miner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/puzpuzpuz/xsync/v4"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// SessionLifecycleConfig contains configuration for the session lifecycle manager.
type SessionLifecycleConfig struct {
	// SupplierAddress is the supplier this manager is for.
	SupplierAddress string

	// CheckIntervalBlocks is how often to check for session transitions.
	// Default: 1 (every block)
	CheckIntervalBlocks int64

	// MaxConcurrentTransitions is the max number of sessions transitioning at once.
	// Default: 10
	MaxConcurrentTransitions int

	// CheckInterval is the time interval for checking session transitions.
	// If 0, defaults to 30 * time.Second.
	// For tests, set to a faster value like 100 * time.Millisecond.
	CheckInterval time.Duration
}

// ClaimCycleResult names, BY SESSION ID, the sessions whose claim reached the
// chain during one claim cycle. A session absent from Claimed is one the caller
// must not transition: it was skipped, it failed, or the cycle never reached it.
//
// It replaces a [][]byte returned parallel to the caller's own slice. That shape
// carried the answer in a POSITION while the callback also wrote the same fact
// into the session itself, by identity -- two truths about one thing, and the
// positional one drifted: it was filled through a counter that advanced only for
// sessions that actually submitted, so it left-packed, and the caller then
// transitioned the first k sessions of its slice whatever they happened to be.
// Naming the sessions removes the position, and with it the possibility.
type ClaimCycleResult struct {
	// Claimed holds the session IDs the caller may transition to Claimed.
	// nil is valid and means none.
	Claimed map[string]struct{}
}

// IsClaimed reports whether this cycle got sessionID's claim to the chain.
func (r ClaimCycleResult) IsClaimed(sessionID string) bool {
	_, ok := r.Claimed[sessionID]
	return ok
}

// ProofCycleResult names the sessions whose proof was accepted by the chain's
// mempool during one proof cycle. A session ABSENT from Settled is not a failed
// session: it is a session the caller must not transition, either because its
// group failed or because the cycle stopped before reaching it.
//
// The distinction is the point of the type. The caller marks SessionStateProved,
// and a proved session with no proof on-chain is a slash whose ledger says
// everything went fine -- so "not named" has to mean "leave it alone", never
// "assume the batch's fate applies".
type ProofCycleResult struct {
	// Settled holds the session IDs the caller may transition to Proved.
	// nil is valid and means none.
	Settled map[string]struct{}
}

// IsSettled reports whether this cycle got sessionID's proof to the chain.
func (r ProofCycleResult) IsSettled(sessionID string) bool {
	_, ok := r.Settled[sessionID]
	return ok
}

// SessionLifecycleCallback defines callbacks for lifecycle events.
type SessionLifecycleCallback interface {
	// OnSessionActive is called when a new session starts.
	OnSessionActive(ctx context.Context, snapshot *SessionSnapshot) error

	// OnSessionsNeedClaim is called when sessions need claims submitted (batched).
	// All sessions in the batch are submitted in a single transaction for efficiency.
	// The returned ClaimCycleResult names the sessions whose claim reached the
	// chain; the claimed root hash itself is written into the session, so it is
	// not returned alongside and cannot disagree with it.
	OnSessionsNeedClaim(ctx context.Context, snapshots []*SessionSnapshot) (ClaimCycleResult, error)

	// OnSessionsNeedProof is called when sessions need proofs submitted.
	// The returned ProofCycleResult names the sessions whose proof reached the
	// chain; the error aggregates the groups that failed. Both carry meaning at
	// once: one cycle can settle some sessions and fail others, and before this
	// signature it could not say so -- a nil meant "all proved" and an error
	// meant "none", with nothing in between.
	OnSessionsNeedProof(ctx context.Context, snapshots []*SessionSnapshot) (ProofCycleResult, error)

	// OnSessionProved is called when a session proof is successfully submitted.
	OnSessionProved(ctx context.Context, snapshot *SessionSnapshot) error

	// OnProbabilisticProved is called when a session is probabilistically proved (no proof required).
	OnProbabilisticProved(ctx context.Context, snapshot *SessionSnapshot) error

	// OnClaimWindowClosed is called when a session fails due to claim window timeout.
	OnClaimWindowClosed(ctx context.Context, snapshot *SessionSnapshot) error

	// OnClaimTxError is called when a session fails due to claim transaction error.
	OnClaimTxError(ctx context.Context, snapshot *SessionSnapshot) error

	// OnProofWindowClosed is called when a session fails due to proof window timeout.
	OnProofWindowClosed(ctx context.Context, snapshot *SessionSnapshot) error

	// OnProofTxError is called when a session fails due to proof transaction error.
	OnProofTxError(ctx context.Context, snapshot *SessionSnapshot) error
}

// MeterCleanupPublisher publishes cleanup signals to relayers when a
// supplier's portion of a session leaves active state. Each (session,
// supplier) meter instance is cleaned independently because the relay
// meter enforces per-supplier caps; a shared session with two suppliers
// produces two cleanup calls.
type MeterCleanupPublisher interface {
	PublishMeterCleanup(ctx context.Context, sessionID, supplierAddress string) error
}

// RedisMeterCleanupPublisher implements MeterCleanupPublisher using Redis pub/sub.
// The payload format is "sessionID|supplierAddress"; relayer subscribers
// parse on the '|' separator and call ClearSessionMeter for that exact
// (session, supplier) pair.
type RedisMeterCleanupPublisher struct {
	logger  logging.Logger
	publish func(ctx context.Context, channel string, message interface{}) error
	channel string
}

// NewRedisMeterCleanupPublisher creates a new Redis-based meter cleanup publisher.
// The publish function should be the Redis client's Publish method wrapped to return error.
func NewRedisMeterCleanupPublisher(
	logger logging.Logger,
	publish func(ctx context.Context, channel string, message interface{}) error,
	channel string,
) *RedisMeterCleanupPublisher {
	return &RedisMeterCleanupPublisher{
		logger:  logger,
		publish: publish,
		channel: channel,
	}
}

// PublishMeterCleanup publishes a cleanup signal for a (session, supplier)
// pair via Redis pub/sub. Payload format: "sessionID|supplierAddress".
func (p *RedisMeterCleanupPublisher) PublishMeterCleanup(ctx context.Context, sessionID, supplierAddress string) error {
	return p.publish(ctx, p.channel, sessionID+"|"+supplierAddress)
}

// SessionLifecycleManager manages the lifecycle of sessions from active to settled.
// It monitors block heights and triggers state transitions at the appropriate times.
type SessionLifecycleManager struct {
	logger       logging.Logger
	config       SessionLifecycleConfig
	sessionStore SessionStore
	sharedClient client.SharedQueryClient
	blockClient  client.BlockClient
	callback     SessionLifecycleCallback

	// Optional meter cleanup publisher for notifying relayers when sessions leave active state
	meterCleanupPublisher MeterCleanupPublisher

	// Optional: flushes relays already in the tree but not yet counted, for
	// sessions about to be claimed (the supplier's relayBatch).
	flushPendingRelays func(ctx context.Context, sessionIDs []string)

	// Conditional flush delay -- optional, and the wait is a no-op unless
	// maxNonReclaimHandledMsgIDLookup and lastGeneratedMsgIDLookup are both
	// set (see awaitFlushWatermark). Must be wired before Start(): a session
	// loaded already in SessionStateClaiming can reach a transition check on
	// the very first pass.
	maxNonReclaimHandledMsgIDLookup func() (streamMsgID, bool)
	lastGeneratedMsgIDLookup        func(ctx context.Context) (streamMsgID, bool, error)
	flushDelay                      FlushDelayConfig

	// claimFlushWaiting, when set, is called as the flush delay starts waiting
	// for the stream, and the function it returns when the wait ends. It lets
	// this supplier's consumer read while ingestion is held for proofs:
	// otherwise the claim seals at its cap without the relays still in the
	// stream.
	claimFlushWaiting func() (done func())

	// Active sessions being monitored (lock-free concurrent map)
	activeSessions *xsync.Map[string, *SessionSnapshot]

	// resumedUnsentClaims names the sessions loaded in claiming with no claim
	// hash and moved back to active: a previous process may have broadcast
	// their claim before it died. When their claim window closes they are asked
	// about on chain before being booked failed (see claimOnChainObserver).
	// An entry leaves with its session, wherever activeSessions drops it.
	resumedUnsentClaims *xsync.Map[string, struct{}]

	// Pond subpool for controlled concurrency during transitions
	transitionSubpool pond.Pool

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.RWMutex
	closed   bool
}

// NewSessionLifecycleManager creates a new session lifecycle manager.
// The workerPool parameter is required for creating transition subpool.
func NewSessionLifecycleManager(
	logger logging.Logger,
	sessionStore SessionStore,
	sharedClient client.SharedQueryClient,
	blockClient client.BlockClient,
	callback SessionLifecycleCallback,
	config SessionLifecycleConfig,
	workerPool pond.Pool,
) *SessionLifecycleManager {
	if config.CheckIntervalBlocks <= 0 {
		config.CheckIntervalBlocks = 1
	}
	// MaxConcurrentTransitions is always set by the config getter (minimum 10)

	// Create transition subpool from master pool
	// Uses CreateBoundedSubpool to cap at parent pool max and warn if exceeded
	componentLogger := logging.ForSupplierComponent(logger, logging.ComponentSessionLifecycle, config.SupplierAddress)
	transitionSubpool := CreateBoundedSubpool(componentLogger, workerPool, config.MaxConcurrentTransitions, "transition_subpool")

	componentLogger.Debug().
		Int("transition_workers", config.MaxConcurrentTransitions).
		Msg("created transition subpool from master pool")

	return &SessionLifecycleManager{
		logger:              componentLogger,
		config:              config,
		sessionStore:        sessionStore,
		sharedClient:        sharedClient,
		blockClient:         blockClient,
		callback:            callback,
		activeSessions:      xsync.NewMap[string, *SessionSnapshot](),
		resumedUnsentClaims: xsync.NewMap[string, struct{}](),
		transitionSubpool:   transitionSubpool,
	}
}

// SetMeterCleanupPublisher sets the meter cleanup publisher for notifying relayers
// when sessions leave active state. This should be called before Start().
func (m *SessionLifecycleManager) SetMeterCleanupPublisher(publisher MeterCleanupPublisher) {
	m.meterCleanupPublisher = publisher
}

// SetPendingRelayFlusher sets what the claim transition calls before it reads
// its sessions' counters. This should be called before Start().
func (m *SessionLifecycleManager) SetPendingRelayFlusher(flush func(ctx context.Context, sessionIDs []string)) {
	m.flushPendingRelays = flush
}

// FlushDelayConfig groups the conditional-flush-delay tunables.
// PollInterval falls back to 1s when zero. There is no BlockTime or Cap
// field: the cap is a fixed height (the batch's earliest claim-window-open
// height plus 2), read live from the block client, never derived from
// elapsed wall-clock time.
type FlushDelayConfig struct {
	PollInterval time.Duration
}

// SetMaxNonReclaimHandledMsgIDLookup sets what the flush-delay wait polls to
// learn how far this supplier's worker has processed live (non-reclaim)
// deliveries. This should be called before Start().
func (m *SessionLifecycleManager) SetMaxNonReclaimHandledMsgIDLookup(fn func() (streamMsgID, bool)) {
	m.maxNonReclaimHandledMsgIDLookup = fn
}

// SetClaimFlushWaiting sets what the flush-delay wait calls while it waits for
// the stream to drain. This should be called before Start().
func (m *SessionLifecycleManager) SetClaimFlushWaiting(fn func() (done func())) {
	m.claimFlushWaiting = fn
}

// SetLastGeneratedMsgIDLookup sets what the flush-delay wait calls, once,
// right when a claim transition fires, to capture the stream's
// last-generated-id at that instant -- the wait targets THIS captured value,
// never whatever arrives afterward (that belongs to a later claim or the
// following session). This should be called before Start().
func (m *SessionLifecycleManager) SetLastGeneratedMsgIDLookup(fn func(ctx context.Context) (streamMsgID, bool, error)) {
	m.lastGeneratedMsgIDLookup = fn
}

// SetFlushDelayConfig sets the conditional-flush-delay tunables. This
// should be called before Start().
func (m *SessionLifecycleManager) SetFlushDelayConfig(cfg FlushDelayConfig) {
	m.flushDelay = cfg
}

// Start begins monitoring sessions and triggering lifecycle transitions.
func (m *SessionLifecycleManager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("lifecycle manager is closed")
	}

	m.ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	// Verify chain reachability at startup via a shared-params query.
	if _, err := m.sharedClient.GetParams(ctx); err != nil {
		return fmt.Errorf("failed to load shared params: %w", err)
	}

	// Load existing sessions from store
	if err := m.loadExistingSessions(ctx); err != nil {
		m.logger.Warn().Err(err).Msg("failed to load existing sessions, starting fresh")
	}

	m.resumeColdCompactions(ctx)

	// LATE SESSION PRIORITIZATION: Check for sessions needing immediate attention
	// If we have loaded sessions and any are past their claim window, process them now
	// instead of waiting for the next block event (could be 10+ seconds)
	sessionCount := m.activeSessions.Size()
	if sessionCount > 0 {
		block := m.blockClient.LastBlock(ctx)
		if block != nil {
			m.logger.Debug().
				Int64("current_height", block.Height()).
				Int("loaded_sessions", sessionCount).
				Msg("checking for late sessions on startup")
			m.checkSessionTransitions(ctx, block.Height())
		}
	}

	// Start lifecycle checker
	m.wg.Add(1)
	go m.lifecycleChecker(m.ctx)

	m.logger.Info().
		Int("active_sessions", sessionCount).
		Msg("session lifecycle manager started")

	return nil
}

// claimOnChainObserver is what the lifecycle asks before booking a session that
// reached claiming as claim_window_closed. The production callback implements
// it (LifecycleCallback.ObserveClaimOnChain); a callback that does not keeps
// today's behaviour.
type claimOnChainObserver interface {
	ObserveClaimOnChain(ctx context.Context, snapshot *SessionSnapshot) (bool, error)
}

// mayHaveClaimOnChain reports whether a session may have a claim on chain that
// no local record shows: it is in claiming, or it was loaded in claiming with no
// hash and resumed to active.
func (m *SessionLifecycleManager) mayHaveClaimOnChain(session *SessionSnapshot) bool {
	if session.State == SessionStateClaiming && session.ClaimTxHash == "" {
		return true
	}
	_, resumed := m.resumedUnsentClaims.Load(session.SessionID)
	return resumed
}

// claimReadIsFinal reports whether a "no claim" answer read at height is the
// last word for a claim window closing at claimClose. poktroll accepts a claim
// in block claimClose itself, so only a read taken after that block exists --
// height claimClose+1 or later -- cannot be overtaken by it.
func claimReadIsFinal(height, claimClose int64) bool {
	return height > claimClose
}

// resumeUnsentSubmission moves a session loaded in claiming or proving whose
// transaction was never sent back to the state before it. Either state is
// persisted before the claim or proof is built and sent, and only its window
// closing moves a session out of it: a process that stopped in between -- a
// crash, or an instance that let the supplier go -- left a claim or proof that
// nothing would send. Back in active or claimed, the next transition check
// sends it. A session whose transaction hash was stored stays as it is.
func (m *SessionLifecycleManager) resumeUnsentSubmission(ctx context.Context, session *SessionSnapshot) {
	var resumeTo SessionState
	switch {
	case session.State == SessionStateProving && session.ProofTxHash == "":
		resumeTo = SessionStateClaimed
	case session.State == SessionStateClaiming && session.ClaimTxHash == "":
		resumeTo = SessionStateActive
	default:
		return
	}
	from := session.State
	if err := m.sessionStore.UpdateState(ctx, session.SessionID, resumeTo); err != nil {
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, session.SessionID).
			Str(logging.FieldSupplier, session.SupplierOperatorAddress).
			Str("state", string(from)).
			Msg("failed to resume a session whose transaction was never sent: it stays as loaded")
		sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state").Inc()
		return
	}
	session.State = resumeTo
	if from == SessionStateClaiming {
		m.resumedUnsentClaims.Store(session.SessionID, struct{}{})
	}
	sessionSnapshotsResumedAtStartup.WithLabelValues(m.config.SupplierAddress, string(from)).Inc()
	m.logger.Info().
		Str(logging.FieldSessionID, session.SessionID).
		Str(logging.FieldSupplier, session.SupplierOperatorAddress).
		Str(logging.FieldServiceID, session.ServiceID).
		Str("from_state", string(from)).
		Str("to_state", string(resumeTo)).
		Int64("session_end", session.SessionEndHeight).
		Msg("resuming a session whose transaction was never sent")
}

// resumeColdCompactions hands the callback the sessions loaded with their claim
// already sent, so their trees are stored as their leaves again. The claim path
// is the only place that queues that compaction and the queue is this process's,
// so a miner that stopped between a claim and its compaction -- a crash, an
// OOM, or an instance that let the supplier go -- left those trees whole in
// Redis until their proof deletes them.
//
// It selects by session STATE, never by which keys are in Redis: claimed_root is
// written before the claim is sent, so a session still in claiming looks the
// same there and is the one that must still send its claim. A session being
// proved is included: its proof reads the leaves blob when the nodes hash is
// gone, and the admission serves proofs before compactions.
func (m *SessionLifecycleManager) resumeColdCompactions(ctx context.Context) {
	resumer, ok := m.callback.(interface {
		OnClaimedSessionsResumed(ctx context.Context, sessions []*SessionSnapshot)
	})
	if !ok {
		return
	}
	var claimed []*SessionSnapshot
	m.activeSessions.Range(func(_ string, session *SessionSnapshot) bool {
		if session.State == SessionStateClaimed || session.State == SessionStateProving {
			claimed = append(claimed, session)
		}
		return true
	})
	if len(claimed) == 0 {
		return
	}
	resumer.OnClaimedSessionsResumed(ctx, claimed)
}

// loadExistingSessions loads sessions from the store on startup.
func (m *SessionLifecycleManager) loadExistingSessions(ctx context.Context) error {
	sessions, err := m.sessionStore.GetBySupplier(ctx)
	if err != nil {
		return err
	}

	for _, session := range sessions {
		// Only track sessions that aren't in terminal state
		if !session.State.IsTerminal() {
			m.resumeUnsentSubmission(ctx, session)
			m.activeSessions.Store(session.SessionID, session)
			sessionSnapshotsLoaded.WithLabelValues(m.config.SupplierAddress).Inc()
		} else {
			// Log and track metrics for skipped sessions (terminal states)
			// These are historical events from before restart, use DEBUG level
			sessionSnapshotsSkippedAtStartup.WithLabelValues(m.config.SupplierAddress, string(session.State)).Inc()

			// Differentiate between success and failure for operator clarity
			if session.State.IsSuccess() {
				m.logger.Debug().
					Str("session_id", session.SessionID).
					Str("service_id", session.ServiceID).
					Str("state", string(session.State)).
					Int64("session_end_height", session.SessionEndHeight).
					Int64("relay_count", session.RelayCount).
					Uint64("compute_units", session.TotalComputeUnits).
					Msg("skipping session at startup: already completed (rewards claimed or proved)")
			} else if session.State.IsFailure() {
				// Failed session - historical data (actual WARN was logged when it failed)
				// Only mention "rewards lost" if there were actually relays
				msg := "skipping session at startup: failed with 0 relays"
				if session.RelayCount > 0 {
					msg = "skipping session at startup: failed (rewards lost)"
				}
				m.logger.Debug().
					Str("session_id", session.SessionID).
					Str("state", string(session.State)).
					Str("service_id", session.ServiceID).
					Int64("session_end_height", session.SessionEndHeight).
					Int64("relay_count", session.RelayCount).
					Uint64("compute_units", session.TotalComputeUnits).
					Msg(msg)
			}
		}
	}

	return nil
}

// TrackSession starts tracking a new session.
func (m *SessionLifecycleManager) TrackSession(ctx context.Context, snapshot *SessionSnapshot) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("lifecycle manager is closed")
	}
	m.mu.RUnlock()

	m.activeSessions.Store(snapshot.SessionID, snapshot)

	// Persist to store
	if err := m.sessionStore.Save(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	m.logger.Debug().
		Str("session_id", snapshot.SessionID).
		Str("state", string(snapshot.State)).
		Msg("started tracking session")

	return nil
}

// RemoveSession removes a session from in-memory tracking.
// This is called by the terminal state callback to update in-memory state
// atomically with Redis updates, preventing session leak.
func (m *SessionLifecycleManager) RemoveSession(sessionID string) {
	// Check if session existed before deletion for logging
	_, existed := m.activeSessions.LoadAndDelete(sessionID)
	if existed {
		m.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Int("remaining_sessions", m.activeSessions.Size()).
			Msg("session_lifecycle_atomic_remove: session removed via terminal callback")
	}
}

// lifecycleChecker monitors blocks and checks sessions for state transitions.
// Uses event-driven block notifications via Subscribe() method.
func (m *SessionLifecycleManager) lifecycleChecker(ctx context.Context) {
	defer m.wg.Done()

	// Check if block client supports Subscribe() method for fan-out
	if subscriber, ok := m.blockClient.(interface {
		Subscribe(ctx context.Context, bufferSize int) <-chan *localclient.SimpleBlock
	}); ok {
		m.logger.Info().Msg("using event-driven block notifications (Subscribe)")
		m.lifecycleCheckerEventDriven(ctx, subscriber)
	} else {
		m.logger.Warn().Msg("block client does not support Subscribe(), falling back to polling")
		m.lifecycleCheckerPolling(ctx)
	}
}

// blockEventSubscriberBuffer is the per-subscriber block-event channel buffer.
// The decoupled reader drains this channel on arrival and block events arrive
// one at a time (one publishToSubscribers send per block), so steady-state
// occupancy is 0–1 and this only needs to cushion transient reader-scheduling
// jitter. 256 is already a huge cushion (256 blocks of reader starvation) at a
// negligible per-subscriber cost; it is NOT the backpressure mechanism —
// backpressure lives in the unbounded transition worker pool, observable as
// pool-queue growth (session_transition_queue_depth). One channel is allocated
// per supplier, so this is multiplied by the supplier count — another reason to
// keep it modest rather than the former 8192 (~32x more memory for cushion the
// instant-draining reader can never use).
const blockEventSubscriberBuffer = 256

// lifecycleCheckerEventDriven processes block events to drive session
// transitions, with INGESTION fully decoupled from PROCESSING so a slow pass can
// never stall block delivery (the failure mode that, at high supplier counts,
// filled the old single-loop's channel and silently stopped claim/proof
// windows from firing).
//
// See runCoalescingBlockLoop: a reader drains the channel instantly (never
// blocking the publisher, never dropping an event), while this goroutine runs
// checkSessionTransitions against the LATEST height. checkSessionTransitions is
// level-triggered (acts when currentHeight >= a window boundary), so coalescing
// redundant ticks never misses a window — and the heavy claim/proof build+submit
// is dispatched to the unbounded transition pool, not done inline here.
func (m *SessionLifecycleManager) lifecycleCheckerEventDriven(ctx context.Context, subscriber interface {
	Subscribe(ctx context.Context, bufferSize int) <-chan *localclient.SimpleBlock
},
) {
	blockCh := subscriber.Subscribe(ctx, blockEventSubscriberBuffer)
	m.logger.Debug().Msg("using Subscribe() for block events (decoupled reader/processor)")

	lastHeight := int64(0)
	runCoalescingBlockLoop(ctx, blockCh, func(height int64) {
		if lastHeight > 0 {
			// Blocks advanced this pass; 1 = keeping up, >1 = coalescing under load.
			sessionBlockProcessingLag.WithLabelValues(m.config.SupplierAddress).Set(float64(height - lastHeight))
		}
		lastHeight = height
		// Sample the transition subpool's queue depth once per pass. Cheap atomic
		// read (no goroutine); surfaces backpressure before it becomes RAM pressure
		// or a missed window.
		m.recordTransitionQueueDepth()
		m.checkSessionTransitions(ctx, height)
	})

	// The loop only returns on ctx cancel (orderly shutdown) or block-channel
	// close. A close while the context is still live means the block source went
	// away under us — the silent-stall failure this component exists to prevent —
	// so surface it loudly rather than exiting quietly.
	if ctx.Err() == nil {
		m.logger.Warn().Msg("block events channel closed unexpectedly; session lifecycle block loop stopped")
	}
}

// recordTransitionQueueDepth samples the transition subpool's queue depth into
// the per-supplier session_transition_queue_depth gauge. The subpool queue is
// unbounded by design (it absorbs window-open bursts instead of blocking block
// ingestion), so a growing depth is the backpressure / OOM early-warning signal.
// Called once per processed block from the lifecycle loop.
func (m *SessionLifecycleManager) recordTransitionQueueDepth() {
	sessionTransitionQueueDepth.WithLabelValues(m.config.SupplierAddress).Set(float64(m.transitionSubpool.WaitingTasks()))
}

// runCoalescingBlockLoop consumes block-height events without ever blocking the
// producer and without dropping a single event, by fully decoupling ingestion
// from processing:
//
//   - A reader goroutine drains blockCh the instant an event arrives, recording
//     only the newest height. It does no work, so the channel never backs up and
//     no block event is ever dropped, no matter how slow onHeight is.
//   - The processor (this goroutine) runs onHeight against the LATEST height each
//     time it is free. Block heights are level-triggered by every caller (they
//     act when current height >= some target), so processing only the latest
//     never skips work — it merely coalesces redundant ticks. A single processor
//     also guarantees two heights are never processed concurrently (which would
//     race on shared state / double-dispatch).
//
// onHeight is only ever invoked with strictly increasing heights. The loop
// returns when ctx is cancelled or blockCh is closed.
func runCoalescingBlockLoop(ctx context.Context, blockCh <-chan *localclient.SimpleBlock, onHeight func(height int64)) {
	var latest atomic.Int64
	wake := make(chan struct{}, 1) // coalesced wake: at most one pending signal
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case block, ok := <-blockCh:
				if !ok {
					return
				}
				// This goroutine is the sole writer of latest, so a plain
				// monotonic store suffices — no compare-and-swap race to guard.
				if h := block.Height(); h > latest.Load() {
					latest.Store(h)
				}
				select {
				case wake <- struct{}{}:
				default: // already signalled — the processor reads the latest height
				}
			}
		}
	}()

	var lastProcessed int64
	process := func() {
		if h := latest.Load(); h > lastProcessed {
			lastProcessed = h
			onHeight(h)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-readerDone:
			// The block channel closed (source ended, not a ctx cancel): process
			// any final coalesced height before exiting, so the last block is never
			// lost to select picking readerDone over a still-pending wake. On a ctx
			// cancel we stop immediately without a final pass.
			if ctx.Err() == nil {
				process()
			}
			return
		case <-wake:
			process()
		}
	}
}

// lifecycleCheckerPolling uses time-based polling as a fallback.
func (m *SessionLifecycleManager) lifecycleCheckerPolling(ctx context.Context) {
	// AGGRESSIVE: 1 second polling by default.
	// Claims/proofs = money. Missing them = losing money.
	// Better to poll frequently than risk missing a window.
	checkInterval := m.config.CheckInterval
	if checkInterval == 0 {
		checkInterval = 1 * time.Second
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	lastHeight := int64(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Get current block height
			block := m.blockClient.LastBlock(ctx)
			currentHeight := block.Height()

			// Only process if height changed
			if currentHeight <= lastHeight {
				continue
			}
			lastHeight = currentHeight

			// Check all sessions for transitions
			m.checkSessionTransitions(ctx, currentHeight)
		}
	}
}

// sessionMightNeedTransition performs a quick in-memory check to determine if a session
// MIGHT need a transition at the current block height. This is an optimization to avoid
// unnecessary Redis calls for sessions that can't possibly transition yet.
//
// This check is conservative - it may return true for sessions that ultimately don't
// transition, but it should never return false for sessions that DO need to transition.
func (m *SessionLifecycleManager) sessionMightNeedTransition(
	snapshot *SessionSnapshot,
	currentHeight int64,
	params *sharedtypes.Params,
) bool {
	// Terminal states never need transitions (they should be cleaned up)
	if snapshot.State.IsTerminal() {
		return true // Return true to trigger cleanup in the main loop
	}

	// Fail open: if the session's epoch params could not be resolved, do NOT silently
	// skip it — let it through to the full Redis-backed check so a transient
	// param-query failure never drops a session that may need to claim/prove.
	if params == nil {
		return true
	}

	switch snapshot.State {
	case SessionStateActive:
		// Active sessions only need checking when claim window opens
		claimWindowOpen := sharedtypes.GetClaimWindowOpenHeight(params, snapshot.SessionEndHeight)
		return currentHeight >= claimWindowOpen

	case SessionStateClaiming:
		// Claiming sessions need checking for timeout detection
		// (the callback handles success, but we need to detect if it didn't run)
		return true

	case SessionStateClaimed:
		// Claimed sessions only need checking when proof window opens
		proofWindowOpen := sharedtypes.GetProofWindowOpenHeight(params, snapshot.SessionEndHeight)
		return currentHeight >= proofWindowOpen

	case SessionStateProving:
		// Proving sessions need checking for timeout detection
		// (the callback handles success, but we need to detect if it didn't run)
		return true

	default:
		// Unknown state - check it to be safe
		return true
	}
}

// checkSessionTransitions checks all active sessions for required state transitions.
func (m *SessionLifecycleManager) checkSessionTransitions(ctx context.Context, currentHeight int64) {
	// Resolve shared params at EACH session's own end height rather than from a single
	// live snapshot. After a session-length change (poktroll #543 anchored grid), an
	// old-epoch session's claim/proof windows must be computed with the params that were
	// effective at that session — otherwise this transition filter would disagree with
	// the already-height-aware submission path and fire (or time out) at the wrong height.
	// Memoized per pass; GetParamsAtHeight's fast path returns cached live params with no
	// RPC for current-epoch sessions, so the steady-state per-block cost is unchanged.
	paramsByEndHeight := make(map[int64]*sharedtypes.Params)
	resolveParams := func(sessionEndHeight int64) *sharedtypes.Params {
		if p, ok := paramsByEndHeight[sessionEndHeight]; ok {
			return p
		}
		// An ACTIVE session's end height is in the FUTURE. An at-height read there
		// resolves against the live grid anyway (poktroll GetParamsAtHeight walks
		// back to the newest entry <= the height), so it returns today's value —
		// but caches it under a future-height key, where the query layer treats it
		// as immutable and masks a later governance change until the TTL lapses.
		// Read live while the session is still running; switch to the immutable
		// at-height read once its end height is in the past.
		var (
			p   *sharedtypes.Params
			err error
		)
		if currentHeight <= 0 || sessionEndHeight >= currentHeight {
			p, err = m.sharedClient.GetParams(ctx)
		} else {
			p, err = m.sharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
		}
		if err != nil {
			// Cache the failure to avoid a retry storm within this pass. Callers fail
			// open (pre-filter) / fail safe (determineTransition) on a nil result.
			m.logger.Warn().
				Err(err).
				Int64("session_end_height", sessionEndHeight).
				Msg("failed to resolve shared params at session height; session check will fail open/safe")
			paramsByEndHeight[sessionEndHeight] = nil
			return nil
		}
		paramsByEndHeight[sessionEndHeight] = p
		return p
	}

	// OPTIMIZATION: Pre-filter sessions using in-memory state to avoid unnecessary Redis calls.
	// Only collect sessions that MIGHT need a transition at this block height.
	// This reduces Redis calls from O(all sessions) to O(sessions in relevant windows).
	totalSessions := 0
	candidateSessionIDs := make([]string, 0)
	m.activeSessions.Range(func(sessionID string, snapshot *SessionSnapshot) bool {
		totalSessions++
		if m.sessionMightNeedTransition(snapshot, currentHeight, resolveParams(snapshot.SessionEndHeight)) {
			candidateSessionIDs = append(candidateSessionIDs, sessionID)
		}
		return true // continue iteration
	})

	// Log filtering stats for observability
	filteredOut := totalSessions - len(candidateSessionIDs)
	if totalSessions > 0 {
		m.logger.Debug().
			Int64("current_height", currentHeight).
			Int("total_in_memory", totalSessions).
			Int("candidates_to_check", len(candidateSessionIDs)).
			Int("filtered_out", filteredOut).
			Msg("session_lifecycle_filter: window-based filtering applied")
	}

	// Group sessions by transition type (claiming, proving, terminal states)
	var claimingSessions []*SessionSnapshot
	var provingSessions []*SessionSnapshot
	// Terminal sessions stored as alternating (state, session) pairs for individual handling
	var terminalSessions []interface{}

	// Counters for instrumentation
	redisExpired, terminalCleaned, noTransition, stateByType := 0, 0, 0, map[SessionState]int{}

	for _, sessionID := range candidateSessionIDs {
		// CRITICAL: Reload session from Redis to get latest state (not stale in-memory copy)
		// Callbacks may have updated state to terminal (e.g., proof_tx_error) which must not be overwritten
		session, err := m.sessionStore.Get(ctx, sessionID)
		if err != nil || session == nil {
			// Session expired from Redis (TTL) or was deleted - remove from in-memory tracking
			// This is expected behavior: sessions complete and expire, this prevents endless reload attempts
			m.activeSessions.Delete(sessionID)
			m.resumedUnsentClaims.Delete(sessionID)
			redisExpired++
			m.logger.Debug().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Int64("current_height", currentHeight).
				Msg("session expired from Redis - removed from active tracking")
			continue
		}

		stateByType[session.State]++

		// CRITICAL: Skip sessions already in terminal states - they should NEVER transition again
		// Terminal states (proved, probabilistic_proved, claim_tx_error, proof_tx_error, etc.)
		// represent final outcomes and must not be overwritten by window timeout logic
		if session.State.IsTerminal() {
			m.activeSessions.Delete(session.SessionID)
			m.resumedUnsentClaims.Delete(session.SessionID)
			terminalCleaned++

			m.logger.Debug().
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSessionState, string(session.State)).
				Str(logging.FieldServiceID, session.ServiceID).
				Msg("cleaned up stale terminal session from in-memory tracking")

			continue
		}

		// Check if this session needs a transition (params resolved at the session's
		// own end height, see resolveParams above).
		newState, reason := m.determineTransition(session, currentHeight, resolveParams(session.SessionEndHeight))
		if newState == "" || newState == session.State {
			noTransition++
			continue
		}

		m.logger.Info().
			Str(logging.FieldSessionID, session.SessionID).
			Str(logging.FieldSupplier, session.SupplierOperatorAddress).
			Str(logging.FieldServiceID, session.ServiceID).
			Str("current_state", string(session.State)).
			Str("target_state", string(newState)).
			Str("reason", reason).
			Int64("current_height", currentHeight).
			Int64("session_end", session.SessionEndHeight).
			Msg("session transition determined")

		// A session that reached claiming with no hash stored may have its claim
		// on chain: a process can die after the broadcast and before the hash
		// is written. Before it is booked claim_window_closed -- terminal, its
		// tree deleted -- the chain is asked. Found, it is booked claimed and
		// goes on to its proof; unanswered, it stays as it is and is asked
		// again next pass. Only sessions that reached claiming are asked, so an
		// outage does not turn every expired active session into a query.
		if newState == SessionStateClaimWindowClosed && m.mayHaveClaimOnChain(session) {
			// The chain still accepts a claim IN block close, so a "no claim"
			// read at close can be overtaken by that block. The verdict waits
			// until close has been built on: from close+1 a negative is final.
			claimClose := sharedtypes.GetClaimWindowCloseHeight(resolveParams(session.SessionEndHeight), session.SessionEndHeight)
			if !claimReadIsFinal(currentHeight, claimClose) {
				noTransition++
				continue
			}
			if observer, ok := m.callback.(claimOnChainObserver); ok {
				observed, err := observer.ObserveClaimOnChain(ctx, session)
				if err != nil {
					m.logger.Warn().
						Err(err).
						Str(logging.FieldSessionID, session.SessionID).
						Str(logging.FieldSupplier, session.SupplierOperatorAddress).
						Msg("could not ask the chain for the session's claim: not booked claim_window_closed, asking again next pass")
					continue
				}
				if observed {
					m.resumedUnsentClaims.Delete(session.SessionID)
					if fresh, getErr := m.sessionStore.Get(ctx, session.SessionID); getErr == nil && fresh != nil {
						m.activeSessions.Store(session.SessionID, fresh)
					}
					continue
				}
			}
			m.resumedUnsentClaims.Delete(session.SessionID)
		}

		// Group by transition type for batching. The terminal group is every
		// state IsTerminal names, not a list kept here by hand: that list
		// omitted SessionStateProved, so a proving session with its proof sent
		// was judged proved at its window's close and the verdict was dropped
		// on every block -- measured 2026-09-22, 58 sessions left in proving
		// with their proofs on chain. A target that is neither is a verdict
		// this dispatcher cannot carry out, and it says so instead of dropping
		// it in silence.
		switch {
		case newState == SessionStateClaiming:
			claimingSessions = append(claimingSessions, session)
		case newState == SessionStateProving:
			provingSessions = append(provingSessions, session)
		case newState.IsTerminal():
			// Terminal states: store (state, session) pairs
			terminalSessions = append(terminalSessions, newState, session)
		default:
			m.logger.Error().
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSupplier, session.SupplierOperatorAddress).
				Str("current_state", string(session.State)).
				Str("target_state", string(newState)).
				Str("reason", reason).
				Msg("session transition has no dispatcher: the session stays where it is")
			sessionTransitionsUndispatched.WithLabelValues(m.config.SupplierAddress, string(newState)).Inc()
		}
	}

	// Log transition batch summary for debugging proof pipeline issues
	if len(candidateSessionIDs) > 0 {
		m.logger.Info().
			Int64("current_height", currentHeight).
			Int("candidates", len(candidateSessionIDs)).
			Int("redis_expired", redisExpired).
			Int("terminal_cleaned", terminalCleaned).
			Int("no_transition_needed", noTransition).
			Int("claiming_batch", len(claimingSessions)).
			Int("proving_batch", len(provingSessions)).
			Int("terminal_batch", len(terminalSessions)/2).
			Interface("state_distribution", stateByType).
			Msg("lifecycle_check: transition batch summary")
	}

	// Execute batched transitions using pond subpool
	// Non-blocking submission with unbounded queue (tasks queue if workers are busy)
	//
	// CRITICAL: Persist state changes to REDIS before submitting async callbacks.
	// This prevents duplicate submissions where the same sessions are resubmitted
	// every block because Redis still shows them in Active state.
	// Without this, sessions can be submitted dozens of times before the callback
	// completes, flooding the worker pool and causing claim window timeouts.
	if len(claimingSessions) > 0 {
		// Persist state to Redis FIRST to prevent duplicate submissions
		// If Redis update fails, we skip submitting to avoid inconsistent state
		var validClaimingSessions []*SessionSnapshot
		// minClaimWindowOpen anchors the flush-delay cap for this batch --
		// the earliest opening height among its sessions, so no session in a
		// mixed batch waits past its own cap. Params are guaranteed non-nil
		// here: determineTransition already required them non-nil to return
		// SessionStateClaiming for this same session, and resolveParams is
		// memoized on session.SessionEndHeight.
		minClaimWindowOpen := int64(-1)
		for _, session := range claimingSessions {
			wo := sharedtypes.GetClaimWindowOpenHeight(resolveParams(session.SessionEndHeight), session.SessionEndHeight)
			if minClaimWindowOpen == -1 || wo < minClaimWindowOpen {
				minClaimWindowOpen = wo
			}
		}
		for _, session := range claimingSessions {
			if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateClaiming); err != nil {
				m.logger.Error().
					Err(err).
					Str(logging.FieldSessionID, session.SessionID).
					Str(logging.FieldSupplier, session.SupplierOperatorAddress).
					Str(logging.FieldServiceID, session.ServiceID).
					Msg("failed to persist claiming state to Redis - skipping to prevent duplicate submission")
				sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state_claiming").Inc()
				continue
			}

			// Publish meter cleanup signal BEFORE updating in-memory state.
			// Once a session transitions to claiming, this supplier's portion
			// of the meter is no longer needed. The signal carries both the
			// session ID and supplier address so a co-supplier (different
			// miner, same session) keeps its own meter intact.
			if m.meterCleanupPublisher != nil {
				if cleanupErr := m.meterCleanupPublisher.PublishMeterCleanup(ctx, session.SessionID, session.SupplierOperatorAddress); cleanupErr != nil {
					m.logger.Warn().
						Err(cleanupErr).
						Str(logging.FieldSessionID, session.SessionID).
						Str(logging.FieldSupplier, session.SupplierOperatorAddress).
						Str(logging.FieldServiceID, session.ServiceID).
						Msg("failed to publish meter cleanup signal")
				} else {
					m.logger.Debug().
						Str(logging.FieldSessionID, session.SessionID).
						Str(logging.FieldSupplier, session.SupplierOperatorAddress).
						Str(logging.FieldServiceID, session.ServiceID).
						Msg("published meter cleanup signal for session leaving active state")
				}
			}

			session.State = SessionStateClaiming
			session.LastUpdatedAt = time.Now()
			validClaimingSessions = append(validClaimingSessions, session)
		}

		if len(validClaimingSessions) > 0 {
			// Capture for closure
			capturedSessions := validClaimingSessions
			capturedWindowOpen := minClaimWindowOpen
			// The flush-delay wait runs on its own goroutine, NOT inside
			// transitionSubpool. That pool is shared with proof/terminal
			// transitions of every other session on this supplier; a wait
			// sitting in one of its slots would starve them. The actual
			// claim-building work (executeBatchedClaimTransition) still goes
			// through transitionSubpool, only after the wait clears it.
			// Declared without a test: no test pins the wait running outside
			// transitionSubpool at this call site.
			go logging.RecoverGoRoutine(m.logger, "claim_flush_delay_wait", func(waitCtx context.Context) {
				if !m.awaitFlushWatermark(waitCtx, capturedSessions, capturedWindowOpen) {
					m.logger.Debug().
						Str(logging.FieldSupplier, m.config.SupplierAddress).
						Int("batch_size", len(capturedSessions)).
						Msg("aborting claim transition, ctx cancelled during flush-delay wait")
					return
				}
				m.transitionSubpool.Submit(func() {
					m.executeBatchedClaimTransition(ctx, capturedSessions)
				})
			})(ctx)
		}
	}

	if len(provingSessions) > 0 {
		validProvingSessions := m.persistProving(ctx, provingSessions)

		if len(validProvingSessions) > 0 {
			// Capture for closure
			capturedSessions := validProvingSessions
			m.transitionSubpool.Submit(func() {
				m.executeBatchedProofTransition(ctx, capturedSessions)
			})
		}
	}

	// Terminal states handled individually (no batching benefit)
	// Process pairs: [state, session, state, session, ...]
	for i := 0; i < len(terminalSessions); i += 2 {
		newState := terminalSessions[i].(SessionState)
		session := terminalSessions[i+1].(*SessionSnapshot)

		// Capture for closure
		capturedState := newState
		capturedSession := session
		m.transitionSubpool.Submit(func() {
			m.executeTransition(ctx, capturedSession, capturedState, string(capturedState))
		})
	}
}

// determineTransition determines if a session needs to transition.
func (m *SessionLifecycleManager) determineTransition(
	session *SessionSnapshot,
	currentHeight int64,
	params *sharedtypes.Params,
) (newState SessionState, action string) {
	// Fail safe: without the session's epoch params we cannot compute its windows.
	// Never fire a (possibly wrong) timeout/transition on a transient param-query
	// failure — leave the session untouched until params resolve on a later pass.
	if params == nil {
		return "", ""
	}

	switch session.State {
	case SessionStateActive:
		// Check if session ended and claim window is approaching
		claimWindowOpen := sharedtypes.GetClaimWindowOpenHeight(params, session.SessionEndHeight)
		claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(params, session.SessionEndHeight)

		// DEBUG: Log window calculation details
		m.logger.Debug().
			Str(logging.FieldSessionID, session.SessionID).
			Str(logging.FieldSupplier, session.SupplierOperatorAddress).
			Str(logging.FieldServiceID, session.ServiceID).
			Str(logging.FieldApplication, session.ApplicationAddress).
			Str("current_state", string(session.State)).
			Int64("current_height", currentHeight).
			Int64("session_start", session.SessionStartHeight).
			Int64("session_end", session.SessionEndHeight).
			Int64("claim_window_open", claimWindowOpen).
			Int64("claim_window_close", claimWindowClose).
			Msg("evaluating active session window")

		// If claim window has passed without claiming, window closed
		if currentHeight >= claimWindowClose {
			return SessionStateClaimWindowClosed, "claim_window_timeout"
		}

		// If we're in the claim window, transition to claiming
		// Protocol handles submission timing via GetEarliestSupplierClaimCommitHeight()
		// Pre-submission checks in lifecycle_callback ensure sufficient time remaining
		if currentHeight >= claimWindowOpen && currentHeight < claimWindowClose {
			return SessionStateClaiming, "claim_window_open"
		}

	case SessionStateClaiming:
		// Transition to claimed shappens after callback succeeds
		claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(params, session.SessionEndHeight)

		// If claim window passed without submitting, window closed (fallback if callback didn't run)
		if currentHeight >= claimWindowClose {
			return SessionStateClaimWindowClosed, "claim_timeout"
		}

	case SessionStateClaimed:
		// IMPORTANT: If we're in claimed state, proof MUST be required.
		// If proof was NOT required, lifecycle callback would have immediately
		// transitioned to probabilistic_proved after claim success.

		proofWindowOpen := sharedtypes.GetProofWindowOpenHeight(params, session.SessionEndHeight)
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(params, session.SessionEndHeight)

		// Missed proof window while proof was required → FAILURE
		if currentHeight >= proofWindowClose {
			return SessionStateProofWindowClosed, "proof_timeout"
		}

		// Proof window open and proof required → GO SUBMIT NOW!
		// Protocol handles submission timing via GetEarliestSupplierProofCommitHeight()
		// Pre-submission checks in lifecycle_callback ensure sufficient time remaining
		if currentHeight >= proofWindowOpen && currentHeight < proofWindowClose {
			return SessionStateProving, "proof_window_open"
		}

		// Still waiting for proof window to open
		// (session remains in claimed state)

	case SessionStateProving:
		// Transition happens after callback succeeds
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(params, session.SessionEndHeight)

		// ❌ OLD BUG: returned SessionStateSettled on timeout (wrong - proof was required but not submitted!)
		// ✅ FIX: Proof window closed = failure (fallback if callback didn't run)
		if currentHeight >= proofWindowClose {
			// A hash means the mempool accepted a proof transaction for this
			// session, which is exactly what `proved` records -- "the proof
			// transaction was successfully submitted", not that it was
			// included. A session sitting in `proving` WITH a hash at the close
			// is that same case with the callback dead: the process was killed
			// between the broadcast and the transition that writes `proved`.
			// Measured 2026-09-18: five sessions ended here after a kill -9,
			// all five on chain, all five paid, and all five booked as a
			// timeout.
			//
			// Whether the transaction actually landed is a different fact with
			// its own series, written only after asking the chain
			// (proof_inclusion_outcome_total). This function does not guess it:
			// it reports what the snapshot knows.
			if session.ProofTxHash != "" {
				return SessionStateProved, "proof_submitted_before_close"
			}
			return SessionStateProofWindowClosed, "proof_timeout"
		}
	}

	return "", ""
}

// claimFlushCapBlocks is how many blocks past a claim window's opening the claim
// flush may still wait for relays already in the stream (awaitFlushWatermark,
// rule 1). It is also where the entry stops admitting relays for that session
// (SupplierManager.claimWindowReached): from that height the flush no longer
// waits, so a relay handled then cannot be counted on to reach the claim. One
// constant for both, because the two drifting apart either drops relays the
// flush would still have taken or admits relays it no longer waits for.
const claimFlushCapBlocks = 2

// awaitFlushWatermark is the conditional flush delay. windowOpenHeight is
// the earliest claim-window-open height among sessions, of this batch,
// transitioning to Claiming. It holds off the caller (which submits the
// actual claim transition to transitionSubpool once this returns true)
// according to Jorge's rule, height-anchored throughout -- never wall-clock,
// never the miner's local clock:
//
//  1. If the live chain height is already >= windowOpenHeight+2 (the cap),
//     seal now. The miner picked this batch up late (restart, a skipped
//     check, HA handoff) and no wait would help.
//  2. Else, if there is nothing more to arrive right now -- the highest
//     live (non-reclaim) stream ID this supplier has processed already
//     reaches the stream's last-generated-id, read at this instant -- seal
//     now, with zero polling.
//  3. Else, wait for the processed watermark to reach THAT captured
//     last-generated-id specifically (not whatever the stream generates
//     afterward -- that belongs to a later claim or the next session),
//     polling live height against the cap the whole time.
//
// It returns false only if ctx is cancelled mid-wait, telling the caller to
// abort the transition entirely rather than flush and claim on a dying
// context. Reclaimed and self-pending entries are outside all three rules:
// best effort, unchanged from before this delay existed.
//
// A no-op (returns true immediately) unless maxNonReclaimHandledMsgIDLookup,
// lastGeneratedMsgIDLookup and the block client are all wired -- unwired,
// this behaves exactly as if it did not exist, rather than guessing.
func (m *SessionLifecycleManager) awaitFlushWatermark(ctx context.Context, sessions []*SessionSnapshot, windowOpenHeight int64) (proceed bool) {
	if m.maxNonReclaimHandledMsgIDLookup == nil || m.lastGeneratedMsgIDLookup == nil || m.blockClient == nil {
		return true
	}

	capHeight := windowOpenHeight + claimFlushCapBlocks
	pollInterval := m.flushDelay.PollInterval
	if pollInterval <= 0 {
		pollInterval = time.Second
	}

	recordCapped := func() {
		for _, s := range sessions {
			RecordClaimFlushCapped(s.ServiceID)
		}
	}

	// Rule 1: already at or past the cap height.
	if m.blockClient.LastBlock(ctx).Height() >= capHeight {
		recordCapped()
		return true
	}

	target, hasTarget, genErr := m.lastGeneratedMsgIDLookup(ctx)
	if genErr != nil {
		m.logger.Warn().
			Err(genErr).
			Str(logging.FieldSupplier, m.config.SupplierAddress).
			Msg("failed to read the stream's last-generated-id, skipping flush delay for this batch")
		return true
	}

	// Rule 2: nothing captured to wait for.
	if handled, ok := m.maxNonReclaimHandledMsgIDLookup(); !hasTarget || (ok && handled.atLeast(target)) {
		return true
	}

	// Rule 3.
	if m.claimFlushWaiting != nil {
		defer m.claimFlushWaiting()()
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pollInterval):
		}

		if handled, ok := m.maxNonReclaimHandledMsgIDLookup(); ok && handled.atLeast(target) {
			return true
		}
		if m.blockClient.LastBlock(ctx).Height() >= capHeight {
			recordCapped()
			return true
		}
	}
}

// executeBatchedClaimTransition executes batched claim transitions.
func (m *SessionLifecycleManager) executeBatchedClaimTransition(ctx context.Context, sessions []*SessionSnapshot) {
	if len(sessions) == 0 {
		return
	}

	m.logger.Debug().
		Int("batch_size", len(sessions)).
		Str(logging.FieldSupplier, m.config.SupplierAddress).
		Msg("executing batched claim transition")

	// Note: true "late relays" (those that arrive after the SMST tree is sealed
	// in FlushTree) are already detected and rejected downstream by the SMST
	// manager's two-phase seal; the supplier worker emits them via
	// RecordRelayRejected(..., "session_sealed", ...). Any per-session check at
	// this earlier point would be racy and inaccurate because:
	//   1. The single-stream-per-supplier architecture makes XPENDING a global
	//      count, not per-session, so it conflates the closing session with the
	//      currently-active one.
	//   2. Relays sent within grace_period_end_offset_blocks are still valid
	//      for the closing session and should be consumed normally before the
	//      SMST flush.

	// Relays of these sessions that are in the tree but still batched get
	// counted now, so the refresh below reads a relay_count that includes them.
	// Money does not depend on it -- the claim is built from the tree -- but the
	// leaves-versus-relays comparison at claim time does. A relay that reaches
	// the tree after this and before the seal is counted by the next flush.
	if m.flushPendingRelays != nil {
		sessionIDs := make([]string, len(sessions))
		for i, session := range sessions {
			sessionIDs[i] = session.SessionID
		}
		m.flushPendingRelays(ctx, sessionIDs)
	}

	// CRITICAL: Refresh session snapshots from Redis to get latest relay counts
	// AND claim deduplication state (ClaimTxHash, State).
	// The in-memory activeSessions may have stale values because:
	// 1. SessionCoordinator.OnRelayProcessed() updates Redis directly without
	//    updating the in-memory map (stale RelayCount).
	// 2. In multi-miner setups, another miner may have already claimed this
	//    session — refreshing ClaimTxHash and State enables cross-instance
	//    deduplication in the lifecycle callback.
	var refreshedSessions []*SessionSnapshot
	for _, session := range sessions {
		freshSnapshot, err := m.sessionStore.Get(ctx, session.SessionID)
		if err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSupplier, session.SupplierOperatorAddress).
				Str(logging.FieldServiceID, session.ServiceID).
				Msg("failed to refresh session from Redis, using in-memory data")
			refreshedSessions = append(refreshedSessions, session)
			continue
		}
		if freshSnapshot != nil {
			// Check if another miner already claimed this session
			if freshSnapshot.ClaimTxHash != "" || freshSnapshot.State == SessionStateClaimed {
				m.logger.Warn().
					Str(logging.FieldSessionID, session.SessionID).
					Str(logging.FieldSupplier, session.SupplierOperatorAddress).
					Str("redis_state", string(freshSnapshot.State)).
					Str("claim_tx_hash", freshSnapshot.ClaimTxHash).
					Msg("skipping claim: session already claimed by another miner (cross-instance dedup)")

				// Update in-memory state to match Redis so we don't retry
				session.State = freshSnapshot.State
				session.ClaimTxHash = freshSnapshot.ClaimTxHash
				session.ClaimedRootHash = freshSnapshot.ClaimedRootHash
				continue
			}

			// Update in-memory state with fresh Redis data
			// Note: Session is a pointer in the map, and each session is processed
			// by one goroutine at a time, so direct field updates are safe
			session.RelayCount = freshSnapshot.RelayCount
			session.TotalComputeUnits = freshSnapshot.TotalComputeUnits
			session.LastUpdatedAt = freshSnapshot.LastUpdatedAt
			session.ClaimTxHash = freshSnapshot.ClaimTxHash

			m.logger.Debug().
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSupplier, session.SupplierOperatorAddress).
				Str(logging.FieldServiceID, session.ServiceID).
				Int64("relay_count", freshSnapshot.RelayCount).
				Uint64("compute_units", freshSnapshot.TotalComputeUnits).
				Msg("refreshed session snapshot from Redis")
		}
		refreshedSessions = append(refreshedSessions, session)
	}
	sessions = refreshedSessions

	if len(sessions) == 0 {
		m.logger.Debug().Msg("all sessions already claimed by another miner, skipping batch")
		return
	}

	// Call the batched claim callback
	// The error and the result are BOTH read: a cycle that failed one group can
	// still have claimed another, and returning early on the error would leave
	// those sessions in `claiming` with their claim already on-chain, to be
	// forfeited when the window closes.
	result, claimErr := m.callback.OnSessionsNeedClaim(ctx, sessions)
	if claimErr != nil {
		m.logger.Error().
			Err(claimErr).
			Int("batch_size", len(sessions)).
			Int("claimed", len(result.Claimed)).
			Msg("claim cycle reported failures — transitioning only the sessions it claimed")
		claimErrors.WithLabelValues(m.config.SupplierAddress, "callback_failed").Inc()
	}

	// Transition the sessions the cycle named, and only those. A session it did
	// not name decided its own outcome inside the callback (economic skip, zero
	// relays, zero compute units, dedup hit) and already carries a terminal
	// state; UpdateState does not check IsTerminal, so writing Claimed over one
	// of them resurrects a session whose SMST is already gone.
	//
	// The claimed root hash is NOT read back from the callback: it is written
	// into the session by identity while the claim is built, and into Redis by
	// OnSessionClaimed. There is nothing to copy here, and nothing that can
	// disagree with it.
	claimedCount := 0
	skippedCount := 0
	for _, session := range sessions {
		if !result.IsClaimed(session.SessionID) {
			skippedCount++
			m.logger.Info().
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSupplier, session.SupplierOperatorAddress).
				Str(logging.FieldServiceID, session.ServiceID).
				Msg("claim cycle did not claim this session — it was skipped (economic/empty/dedup) or never reached")
			continue
		}
		claimedCount++

		// Update session state (pointer update, safe without mutex)
		session.State = SessionStateClaimed
		session.LastUpdatedAt = time.Now()

		// Persist the state change
		if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateClaimed); err != nil {
			m.logger.Error().
				Err(err).
				Str("session_id", session.SessionID).
				Msg("failed to persist claimed state")
			sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state").Inc()
			continue
		}

		// Record the transition
		sessionStateTransitions.WithLabelValues(
			m.config.SupplierAddress,
			session.ServiceID,
			string(SessionStateClaiming),
			string(SessionStateClaimed),
		).Inc()
	}

	m.logger.Info().
		Int("total_sessions", len(sessions)).
		Int("claimed_count", claimedCount).
		Int("skipped_count", skippedCount).
		Msg("claim batch complete — sessions now in 'claimed' state awaiting proof window")
}

// persistProving writes the proving state of each session before its proof is
// built, and returns the sessions whose proof goes out.
func (m *SessionLifecycleManager) persistProving(ctx context.Context, sessions []*SessionSnapshot) []*SessionSnapshot {
	// Persist state to Redis FIRST to prevent duplicate submissions
	var valid []*SessionSnapshot
	for _, session := range sessions {
		if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateProving); err != nil && redistransport.IsOOMError(err) {
			// Redis is full, not the session doubtful: skipping here to avoid a
			// duplicate loses the claim, while a duplicate proof is an upsert
			// that costs one fee. So the proof goes out anyway. Debug: the metric
			// below is the signal, and this fires once per session.
			m.logger.Debug().
				Err(err).
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSupplier, session.SupplierOperatorAddress).
				Msg("could not persist proving state: Redis is out of memory; submitting the proof anyway")
			sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state_proving_oom").Inc()
		} else if err != nil {
			m.logger.Error().
				Err(err).
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSupplier, session.SupplierOperatorAddress).
				Str(logging.FieldServiceID, session.ServiceID).
				Msg("failed to persist proving state to Redis - skipping to prevent duplicate submission")
			sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state_proving").Inc()
			continue
		}
		session.State = SessionStateProving
		session.LastUpdatedAt = time.Now()
		valid = append(valid, session)
	}
	return valid
}

// executeBatchedProofTransition executes batched proof transitions.
func (m *SessionLifecycleManager) executeBatchedProofTransition(ctx context.Context, sessions []*SessionSnapshot) {
	if len(sessions) == 0 {
		return
	}

	m.logger.Info().
		Int("batch_size", len(sessions)).
		Msg("executing batched proof transition — submitting proofs")

	// Call the proof callback. The error and the result are BOTH read: a cycle
	// that failed one group can still have settled another, and returning early
	// on the error would leave those sessions in `proving` with their proof
	// already on-chain -- forfeited at window close while the chain holds the
	// proof that would have paid them.
	result, proofErr := m.callback.OnSessionsNeedProof(ctx, sessions)
	if proofErr != nil {
		m.logger.Error().
			Err(proofErr).
			Int("batch_size", len(sessions)).
			Int("settled", len(result.Settled)).
			Msg("proof cycle reported failures — transitioning only the sessions it settled")
		proofErrors.WithLabelValues(m.config.SupplierAddress, "callback_failed").Inc()
	}

	// Update the settled sessions and transition them to proved.
	for _, session := range sessions {
		// Not settled: either its group failed, or the cycle never reached it.
		// Its state is owned by whoever did reach a verdict on it (the callback
		// marks window-closed / tx-error / probabilistically-proved itself), or
		// by the window closing. Touching it here is what would invent a proof.
		if !result.IsSettled(session.SessionID) {
			continue
		}
		// Update session state (pointer update, safe without mutex)
		session.State = SessionStateProved
		session.LastUpdatedAt = time.Now()

		// Persist the state change
		if err := m.sessionStore.UpdateState(ctx, session.SessionID, SessionStateProved); err != nil {
			m.logger.Error().
				Err(err).
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSupplier, session.SupplierOperatorAddress).
				Str(logging.FieldServiceID, session.ServiceID).
				Msg("failed to persist proved state")
			sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state").Inc()
			// No continue: the proof is out, and the cleanup below frees the tree
			// with a DEL, which Redis takes even when it refuses writes. Holding
			// the tree until this write succeeds holds the memory the write needs;
			// OnSessionProved deletes the tree first and writes the state again
			// after.
		} else {
			// Record the transition
			sessionStateTransitions.WithLabelValues(
				m.config.SupplierAddress,
				session.ServiceID,
				string(SessionStateProving),
				string(SessionStateProved),
			).Inc()
		}

		// Call OnSessionProved for cleanup (stream deletion, SMST cleanup, metrics)
		if proveErr := m.callback.OnSessionProved(ctx, session); proveErr != nil {
			m.logger.Warn().
				Err(proveErr).
				Str(logging.FieldSessionID, session.SessionID).
				Str(logging.FieldSupplier, session.SupplierOperatorAddress).
				Str(logging.FieldServiceID, session.ServiceID).
				Msg("proved callback failed in batched transition")
		}

		// Remove from active tracking (lock-free delete)
		m.activeSessions.Delete(session.SessionID)
		m.resumedUnsentClaims.Delete(session.SessionID)

		m.logger.Info().
			Str(logging.FieldSessionID, session.SessionID).
			Str(logging.FieldSupplier, session.SupplierOperatorAddress).
			Str(logging.FieldServiceID, session.ServiceID).
			Int64("relay_count", session.RelayCount).
			Msg("session lifecycle complete (batched)")
	}
}

// executeTransition executes a state transition for a session (non-batched transitions).
func (m *SessionLifecycleManager) executeTransition(
	ctx context.Context,
	session *SessionSnapshot,
	newState SessionState,
	action string,
) {
	oldState := session.State

	// Create session-scoped logger for this transition
	sessionLogger := logging.WithSession(m.logger, session.SessionID)

	sessionLogger.Debug().
		Str(logging.FieldOldState, string(oldState)).
		Str(logging.FieldNewState, string(newState)).
		Str(logging.FieldAction, action).
		Msg("executing session transition")

	// THE STATE IS PERSISTED BEFORE THE CALLBACKS, AND THE ORDER IS THE POINT.
	//
	// The terminal callbacks are where a session's money is counted. Counting
	// first and persisting after is how the same session is counted twice, and
	// the path is not hypothetical: on a persist failure this function used to
	// return below WITHOUT removing the session from activeSessions, leaving
	// Redis still saying `claiming`. A replica that takes this supplier over
	// reads that state, loadExistingSessions accepts it because it is not
	// terminal, its sweep reaches the same window verdict, and the same relays
	// and uPOKT are counted a second time by a different process.
	//
	// With the write first, a failure means nothing was counted and nothing was
	// cleaned up: the session stays tracked and in `claiming`, and whoever owns
	// it next -- this replica on a later pass, or a new one after failover --
	// settles it exactly once. Under-counting until the write succeeds is the
	// direction this codebase already chose everywhere else in this ledger.
	//
	// session.State is still assigned after the callbacks, so they see the same
	// snapshot they have always seen.
	if err := m.sessionStore.UpdateState(ctx, session.SessionID, newState); err != nil {
		if errors.Is(err, ErrClaimAlreadyOnChain) {
			// The inclusion reconciler booked this session claimed between the
			// verdict and this write. Nothing was written, so nothing is counted
			// and the tree stays: the claim is the truth.
			sessionLogger.Debug().Err(err).Msg("claim-phase failure refused: the session holds its claim")
			return
		}
		sessionLogger.Error().Err(err).Msg("failed to persist state change")
		sessionStoreErrors.WithLabelValues(m.config.SupplierAddress, "update_state").Inc()
		return
	}

	// Execute terminal state callbacks
	switch newState {
	case SessionStateProved:
		if err := m.callback.OnSessionProved(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("proved callback failed")
		}

	case SessionStateProbabilisticProved:
		if err := m.callback.OnProbabilisticProved(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("probabilistic proved callback failed")
		}

	case SessionStateClaimWindowClosed:
		if err := m.callback.OnClaimWindowClosed(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("claim window closed callback failed")
		}

	case SessionStateClaimTxError:
		if err := m.callback.OnClaimTxError(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("claim tx error callback failed")
		}

	case SessionStateProofWindowClosed:
		if err := m.callback.OnProofWindowClosed(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("proof window closed callback failed")
		}

	case SessionStateProofTxError:
		if err := m.callback.OnProofTxError(ctx, session); err != nil {
			sessionLogger.Warn().Err(err).Msg("proof tx error callback failed")
		}
	}

	// Update session state (pointer update, safe without mutex)
	session.State = newState
	session.LastUpdatedAt = time.Now()

	// Record the transition
	sessionStateTransitions.WithLabelValues(
		m.config.SupplierAddress,
		session.ServiceID,
		string(oldState),
		string(newState),
	).Inc()

	// Remove terminal sessions from active tracking (lock-free delete)
	if newState.IsTerminal() {
		m.activeSessions.Delete(session.SessionID)
		m.resumedUnsentClaims.Delete(session.SessionID)

		sessionLogger.Info().
			Str(logging.FieldNewState, string(newState)).
			Int64(logging.FieldCount, session.RelayCount).
			Msg("session lifecycle complete")
	}
}

// Close gracefully shuts down the lifecycle manager.
func (m *SessionLifecycleManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	if m.cancelFn != nil {
		m.cancelFn()
	}

	m.wg.Wait()

	// Stop transition subpool gracefully (drains queued tasks)
	if m.transitionSubpool != nil {
		m.transitionSubpool.StopAndWait()
	}

	m.logger.Info().Msg("session lifecycle manager closed")
	return nil
}
