package miner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/cache"
	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/relayer"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/pocket-relay-miner/tx"
	"github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
	"github.com/pokt-network/smt"
)

// SupplierQueryClient queries supplier information from the blockchain.
type SupplierQueryClient interface {
	GetSupplier(ctx context.Context, supplierOperatorAddress string) (sharedtypes.Supplier, error)
	GetParams(ctx context.Context) (*suppliertypes.Params, error)
	// InvalidateSupplier removes a supplier from the query cache so
	// the next GetSupplier call fetches fresh data from the chain.
	InvalidateSupplier(operatorAddress string)
}

// SupplierStatus represents the state of a supplier in the miner.
type SupplierStatus int

const (
	// SupplierStatusActive means the supplier is actively processing relays.
	SupplierStatusActive SupplierStatus = iota
	// SupplierStatusDraining means the supplier is being removed but waiting for pending work.
	SupplierStatusDraining
)

// supplierStakeView is what the drain write has to republish: the service list
// and the per-transport stake view, carried here so that write never depends on
// reading the store back.
//
// They live behind one atomic pointer because they are written AFTER the state
// is already reachable from m.suppliers (addSupplierWithData stores the state
// and starts consuming before it resolves them), and removeSupplier can load
// that same state and read them concurrently. Copying the slices at the read
// fixes aliasing, not the race on the slice headers themselves.
type supplierStakeView struct {
	Services        []string
	StakedEndpoints []cache.StakedEndpoint
}

// SupplierState holds the state for a single supplier in the miner.
//
// Status is stored atomically (int32) so consumeForSupplier on the relay
// hot path can read the draining flag without coordinating with the
// teardown writer. The owning map is xsync.Map (lock-free); this atomic
// is the per-state field equivalent. Callers must use LoadStatus /
// StoreStatus — do not access `status` directly.
// drainReason says WHY a supplier is being torn down, because the three reasons
// have three different right answers for the relays still in its delivery
// buffer, and one code path serves all three.
type drainReason int32

const (
	// drainShutdown: the process is going away. We still hold the key, but the
	// work is released rather than finished -- best effort, bounded window.
	drainShutdown drainReason = iota
	// drainRebalance: another replica claimed this supplier. It can finish the
	// work; we must not destroy it.
	drainRebalance
	// drainKeyRemoved: the operator withdrew the signing key. NOBODY in this
	// fleet can build an SMST, a claim or a proof for these relays, so holding
	// them pending only makes another consumer rediscover that. They are
	// acknowledged deliberately -- and counted as LOSS, never as a successful
	// drain.
	drainKeyRemoved
)

// SupplierTxClient is what this manager needs from a supplier's tx client, and
// it is declared so the manager can be exercised without one.
//
// The reason is a specific test that could not be written: the resend budget
// degrades inside ResubmitMessage -- an absent budget falls to the ceiling under
// the `unknown` regime -- and NOTHING asserted it. The proof is that forcing
// that branch to always fall through left ./miner/ entirely green. It could not
// be asserted because the method returns early when the supplier has no client,
// so the branch is unreachable without one, and *tx.HASupplierClient is a
// concrete struct whose fields belong to another package: there was nothing to
// substitute. Not a missing test -- a missing seam.
//
// FOUR METHODS, WHICH IS ALL OF THE THIRTEEN THAT miner/ ACTUALLY CALLS. The
// count is the point: an interface holding the whole surface would declare
// capability nobody uses, and the next reader could not tell what this manager
// depends on from what the client happens to offer.
type SupplierTxClient interface {
	CreateClaimsReturningHash(ctx context.Context, timeoutHeight int64, claimMsgs ...client.MsgCreateClaim) (string, tx.SignedTxPayload, error)
	SubmitProofsReturningHash(ctx context.Context, timeoutHeight int64, proofMsgs ...client.MsgSubmitProof) (string, tx.SignedTxPayload, error)
	BroadcastRawReturningHash(ctx context.Context, txType string, p tx.SignedTxPayload) (string, error)
	LatestBlockTime() time.Time
}

// The production implementation. The assertion sits here so that changing either
// side fails at compile time rather than at wiring.
var _ SupplierTxClient = (*tx.HASupplierClient)(nil)

type SupplierState struct {
	OperatorAddr string

	// drainReason is set before cancelFn fires and read by the drain.
	drainReason atomic.Int32

	stakeView atomic.Pointer[supplierStakeView]
	status    atomic.Int32

	// Redis stream consumer for this supplier
	Consumer *redistransport.StreamsConsumer

	// Session management
	SessionStore       *RedisSessionStore
	SessionCoordinator *SessionCoordinator

	// SMST management (for building and managing session trees)
	SMSTManager *RedisSMSTManager

	// relayBatch finishes, per session, the relays already in the SMST: dedup
	// mark, counters and stream acknowledgement in one script. nil means every
	// relay is finished on its own.
	relayBatch *relayBatch

	// maxNonReclaimHandledMsgID is the highest stream ID this supplier's
	// worker has finished handling among LIVE deliveries (msg.IsReclaim ==
	// false). It excludes reclaims and self-pending redeliveries on purpose:
	// those can carry an older ID than one already handled, and updating on
	// them would make the watermark go backwards.
	maxNonReclaimHandledMsgID atomic.Pointer[streamMsgID]

	// Lifecycle management (for claim/proof submission with timing spread)
	LifecycleManager  *SessionLifecycleManager
	LifecycleCallback *LifecycleCallback
	SupplierClient    SupplierTxClient

	// Lifecycle
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// streamMsgID is a parsed Redis stream entry ID ("<ms>-<seq>"), kept as two
// int64s so watermark comparisons are numeric, never lexicographic ("999-0"
// sorts after "1000-0" as a string, backwards from its real order).
type streamMsgID struct {
	ms  int64
	seq int64
}

// parseStreamMsgID parses a Redis stream entry ID of the form "<ms>-<seq>".
func parseStreamMsgID(id string) (streamMsgID, error) {
	dash := strings.IndexByte(id, '-')
	if dash < 0 {
		return streamMsgID{}, fmt.Errorf("malformed stream id %q: no '-'", id)
	}
	ms, err := strconv.ParseInt(id[:dash], 10, 64)
	if err != nil {
		return streamMsgID{}, fmt.Errorf("malformed stream id %q: %w", id, err)
	}
	seq, err := strconv.ParseInt(id[dash+1:], 10, 64)
	if err != nil {
		return streamMsgID{}, fmt.Errorf("malformed stream id %q: %w", id, err)
	}
	return streamMsgID{ms: ms, seq: seq}, nil
}

// before reports whether id sorts before other under Redis' stream ordering.
func (id streamMsgID) before(other streamMsgID) bool {
	if id.ms != other.ms {
		return id.ms < other.ms
	}
	return id.seq < other.seq
}

// atLeast reports whether id is equal to or after other.
func (id streamMsgID) atLeast(other streamMsgID) bool {
	return !id.before(other)
}

// recordNonReclaimHandled advances the supplier's live-delivery watermark to
// id, monotonically: a CAS loop that only ever moves it forward, so an older
// ID arriving after a newer one cannot walk it back.
func (s *SupplierState) recordNonReclaimHandled(id streamMsgID) {
	for {
		cur := s.maxNonReclaimHandledMsgID.Load()
		if cur != nil && cur.atLeast(id) {
			return
		}
		if s.maxNonReclaimHandledMsgID.CompareAndSwap(cur, &id) {
			return
		}
	}
}

// loadMaxNonReclaimHandledMsgID returns the current watermark and whether
// this supplier has handled any live delivery yet.
func (s *SupplierState) loadMaxNonReclaimHandledMsgID() (streamMsgID, bool) {
	p := s.maxNonReclaimHandledMsgID.Load()
	if p == nil {
		return streamMsgID{}, false
	}
	return *p, true
}

// LoadStatus returns the current supplier status.
//
// Uses atomic load so callers on the relay hot path (see
// consumeForSupplier) can read the draining flag without taking the
// manager-level suppliersMu mutex.
func (s *SupplierState) LoadStatus() SupplierStatus {
	return SupplierStatus(s.status.Load())
}

// StoreStatus replaces the supplier status atomically.
//
// Writers that also mutate other SupplierState fields (e.g.
// addSupplierWithData initializing the struct, removeSupplier marking
// a drain) must use StoreStatus so concurrent LoadStatus readers
// observe a well-defined transition.
func (s *SupplierState) StoreStatus(status SupplierStatus) {
	s.status.Store(int32(status))
}

// SupplierManagerConfig contains configuration for the SupplierManager.
type SupplierManagerConfig struct {
	// Redis connection
	RedisClient *redistransport.Client

	// StoreHealth pauses stream consumption and tracking writes while Redis
	// cannot take writes. nil never pauses.
	StoreHealth *redistransport.StoreHealth

	// Stream configuration
	StreamPrefix  string
	ConsumerGroup string
	ConsumerName  string

	// Session configuration
	SessionTTL time.Duration

	// CacheTTL is the TTL for cached data (SMST trees, params, etc.)
	CacheTTL time.Duration

	// SMSTLiveRootCheckpointInterval bounds the relay loss window on
	// mid-session leader kills. Zero means use the SMST manager default
	// (DefaultLiveRootCheckpointInterval). See config.Config docs for
	// the operator-facing trade-off.
	SMSTLiveRootCheckpointInterval int

	// Batch configuration
	BatchSize int64 // Number of messages to fetch per XREADGROUP

	// Redis stream configuration
	// Note: stream consumption blocks for one block interval per XREADGROUP
	// (not BLOCK 0, which could not be interrupted on shutdown) - not configurable
	ClaimIdleTimeout time.Duration // How long a message can be pending before being claimed

	// RelayBatchFlushInterval is how often each supplier flushes its relay
	// batch. Zero disables the tick, leaving the size cap, the claim transition
	// and the supplier's exit as the only flushes (tests construct it that way).
	RelayBatchFlushInterval time.Duration

	// SupplierCache for publishing supplier state to relayers
	SupplierCache *cache.SupplierCache

	// MinerID identifies this miner instance (for debugging/tracking)
	MinerID string

	// SupplierQueryClient queries supplier information from the blockchain
	// Used to fetch the supplier's staked services
	SupplierQueryClient SupplierQueryClient

	// TxClient for submitting claims and proofs to the blockchain
	// This is a shared client for all suppliers
	TxClient *tx.TxClient

	// BlockClient for monitoring block heights (claim/proof timing) and for the
	// per-block stream the inclusion reconciler runs on. The type demands
	// Subscribe so a client that cannot deliver blocks fails the build at the
	// wiring site rather than leaving a constructed reconciler with no trigger.
	BlockClient localclient.SubscribingBlockClient

	// SharedClient for querying shared parameters (claim/proof windows)
	SharedClient client.SharedQueryClient

	// SessionClient for querying session information
	SessionClient SessionQueryClient

	// ProofChecker determines if a proof is required for a claimed session.
	// If nil, proofs are always submitted (legacy behavior).
	ProofChecker *ProofRequirementChecker

	// ProofQueryClient is used by the pre-proof GetClaim guard to verify each
	// session's claim exists on-chain before proof submission, and by the
	// inclusion reconciler to record the real on-chain outcome after each claim
	// broadcast. If nil, the guard is skipped (legacy behavior, unsafe in
	// production — sessions whose claim tx was evicted from mempool will
	// trigger "claim not found" FailedPrecondition storms).
	//
	// The type is query.ProofQueryClient and not poktroll's: the reconciler
	// needs the two supplier-indexed inclusion reads as well, and requiring them
	// HERE is what makes a client that lacks them a build failure at the wiring
	// site instead of a reconciler that silently does not start.
	ProofQueryClient query.ProofQueryClient

	// InclusionReconcilerConfig controls the block-driven claim+proof inclusion
	// reconciler + rebroadcaster. See miner.InclusionReconcilerConfig for fields.
	InclusionReconcilerConfig InclusionReconcilerConfig

	// ServiceClient queries the current service CUPR for the claim-build
	// CUPR-mismatch guard. If nil, the guard is skipped.
	ServiceClient client.ServiceQueryClient

	// SessionLifecycleConfig contains configuration for session lifecycle management.
	SessionLifecycleConfig SessionLifecycleConfig

	// WorkerPool is the master worker pool shared across all concurrent operations.
	// MUST be set by caller. Should be limited to runtime.NumCPU().
	// Subpools will be created from this for different workloads.
	WorkerPool pond.Pool

	// ClaimerConfig contains configuration for the SupplierClaimer.
	// Used for distributed supplier claiming across multiple miners via Redis leases.
	ClaimerConfig SupplierClaimerConfig

	// DisablePreProofClaimVerification turns off the pre-proof GetClaim guard.
	// See LifecycleCallbackConfig.DisablePreProofClaimVerification for details.
	// Default: false (guard enabled).
	DisablePreProofClaimVerification bool

	// SubmissionTrackingTTL is the TTL for claim/proof submission tracking records.
	// Default: 24h
	SubmissionTrackingTTL time.Duration

	// QueryWorkers is the number of workers for bounded supplier queries.
	// Default: 20 (if 0 or not set)
	QueryWorkers int

	// SupplierReconcileInterval is how often the manager re-checks on-chain
	// staking status for every key in the keyring and pushes the result
	// into the claimer. Closes the window between "operator stakes a
	// supplier after miner startup" and "miner picks it up" without
	// requiring a restart or a keyring file edit. A value of 0 disables
	// the background reconcile loop entirely (tests that drive reconcile
	// manually rely on this). Default when unset: 60 seconds.
	SupplierReconcileInterval time.Duration

	// BlockTimeSeconds is forwarded to LifecycleCallbackConfig so the TX
	// deadline can be computed from remaining window blocks. Default: 30.
	BlockTimeSeconds int64
}

// DefaultSupplierReconcileInterval is the default polling cadence for the
// on-chain stake reconciler.
const DefaultSupplierReconcileInterval = 60 * time.Second

// supplierDrainAuditTimeout bounds the observational chain query the drain
// records. It matches the 5s that verifySupplierUnstaked already applies; it is
// named here because the drain now owns the context rather than borrowing the
// caller's, and an unbounded one would keep a shutdown goroutine alive.
const supplierDrainAuditTimeout = 5 * time.Second

// drainLeaseCallTimeout bounds each lease call a drain makes -- extending the
// lease at its start, deleting it at its end. Detached from the caller's
// context, which Close may already have cancelled.
const drainLeaseCallTimeout = 5 * time.Second

// drainLeaseBudget is how long a released supplier's lease is kept while its
// drain runs: the audit, the batch release, the delivery-buffer drain, the
// entries left under the consumer's name and the exit checkpoint of its trees
// (each bounded by shutdownDrainWindow), the consumer's blocked read, which
// returns within one block interval (transport/redis blockInterval, 5 s), and a
// margin.
// A drain that outlives it gives the lease up when the key expires. A func
// because shutdownDrainWindow is a var a test may shrink.
func drainLeaseBudget() time.Duration {
	const consumerBlockInterval = 5 * time.Second
	const margin = 10 * time.Second
	return supplierDrainAuditTimeout + 4*shutdownDrainWindow + consumerBlockInterval + margin
}

// SupplierManager manages multiple suppliers in the HA Miner.
// It handles dynamic addition/removal of suppliers based on key changes.
type SupplierManager struct {
	logger     logging.Logger
	config     SupplierManagerConfig
	keyManager keys.KeyManager
	registry   *SupplierRegistry

	// Per-supplier state. xsync.Map provides lock-free reads and atomic
	// LoadOrStore / LoadAndDelete, which means slow per-supplier teardown
	// (Consumer.Close → wg.Wait) cannot block the relay/claimer hot paths
	// — there is no global lock to acquire.
	suppliers *xsync.Map[string, *SupplierState]

	// Message processing callback
	onRelay func(ctx context.Context, supplierAddr string, msg *transport.StreamMessage) error

	// consumeLoopFlushHook, when set, runs at every flush tick of every
	// supplier's consume loop, before the flush. For tests only: nil in
	// production. It is how a test makes the loop itself panic -- no path known
	// today reaches the loop's recover, because handleStreamMessage and
	// relayBatch.flushSession recover what they call. The restart and the
	// panic budget are a defence against a defect that is not there yet.
	consumeLoopFlushHook func()

	// consumeLoopByteFlushedHook, when set, runs right after the consume loop's
	// byte-triggered flush returns, on the loop's goroutine. Nil in production;
	// a test sets it before the loop starts, to learn that the flush is over.
	consumeLoopByteFlushedHook func()

	// firstFlushTimer, when set, stands in for the timer of a consume loop's
	// first flush. For tests only: nil in production.
	firstFlushTimer func(d time.Duration) (fire <-chan time.Time, stop func() bool)

	// afterReleaseAfterPanicHook, when set, runs once in consumeForSupplier
	// right after releaseBatchAfterPanic returns and strictly before
	// runConsumeLoop is called again. For tests only: nil in production. The
	// point of running here, and not from inside the flush hook, is that
	// nothing else can have called relayBatch.Add or touched this consumer's
	// PEL between the release and this call: runConsumeLoop -- the only
	// reader of msgChan -- has not been invoked yet, so a test observing state
	// here sees exactly what the release left, with no race against
	// deliverOwnPending or the reclaimLoop's own sweep (item 263).
	afterReleaseAfterPanicHook func()

	// drainWG tracks the drain goroutines onSupplierReleased starts, so a
	// test can await the audit without polling a clock. See waitDrains for
	// what it deliberately does NOT do.
	drainWG sync.WaitGroup

	// Pond subpool for bounded supplier queries (prevents unbounded goroutine spawning)
	querySubpool pond.Pool

	// Subpool shared by every supplier's SMST manager for cold tree
	// compaction. Nil only without a WorkerPool (tests), where the manager
	// runs it inline.
	coldCompactionPool pond.Pool

	// rebuildAdmission bounds the trees proofs and compactions load, and
	// holds every supplier's stream consumer while a proof waits for it.
	rebuildAdmission *RebuildAdmission

	// Distributed claiming (optional)
	claimer *SupplierClaimer

	// Deduplicator (shared across suppliers). Prevents counter drift when Redis
	// Streams redeliver a relay (consumer reclaim, transient ack failure).
	deduplicator Deduplicator

	// inclusionReconciler is the process-wide, block-driven verifier +
	// rebroadcaster for BOTH claims and proofs. It reads the rebroadcastStore
	// (what we submitted) and x/proof module state (what's on-chain), then
	// re-broadcasts the still-missing while the window is open. Owned by
	// SupplierManager so Close() drains its pool and stops the block loop.
	inclusionReconciler *InclusionReconciler

	// rebroadcastStore persists built claim/proof messages for the reconciler.
	rebroadcastStore RebroadcastStorage

	// reconcilerCancel stops the block-subscription loop driving the reconciler.
	reconcilerCancel context.CancelFunc
	reconcilerWG     sync.WaitGroup

	// poolResizeCancel stops the block-subscription loop that sizes the
	// transaction connection pool.
	poolResizeCancel context.CancelFunc
	poolResizeWG     sync.WaitGroup

	// sharedSubmissionTracker + sharedTrackersOnce make the submission tracker,
	// rebroadcast store, and inclusion reconciler process-wide singletons (one
	// worker pool / one block loop) instead of one-per-supplier-key. Operators
	// run hundreds of keys per miner; per-key instances meant hundreds of pools
	// and a Close() that drained only the last key's. They are stateless across
	// suppliers — identity is passed per check — so a single shared instance is
	// correct.
	sharedSubmissionTracker *SubmissionTracker
	sharedTrackersOnce      sync.Once

	// Lifecycle
	//
	// mu protects ctx / cancelFn / closed. It is an RWMutex so the
	// key-manager callback (onKeyChange) can capture ctx with a
	// short RLock window without blocking other concurrent callback
	// firings. Start() and Close() take Lock for the composite
	// ctx+closed update. See keyChangeReadCtx.
	ctx      context.Context
	cancelFn context.CancelFunc
	closed   bool
	mu       sync.RWMutex
}

// NewSupplierManager creates a new supplier manager.
func NewSupplierManager(
	logger logging.Logger,
	keyManager keys.KeyManager,
	registry *SupplierRegistry,
	config SupplierManagerConfig,
) *SupplierManager {
	// Create subpool for bounded supplier queries (prevents system overwhelm)
	// Configurable via worker_pools.query_workers (default: 20)
	// Uses CreateBoundedSubpool to cap at parent pool max and warn if exceeded
	queryWorkers := config.QueryWorkers
	if queryWorkers <= 0 {
		queryWorkers = 20 // default
	}
	componentLogger := logging.ForComponent(logger, logging.ComponentSupplierManager)
	querySubpool := CreateBoundedSubpool(componentLogger, config.WorkerPool, queryWorkers, "query_subpool")

	mgr := &SupplierManager{
		logger:       logging.ForComponent(logger, logging.ComponentSupplierManager),
		config:       config,
		keyManager:   keyManager,
		registry:     registry,
		suppliers:    xsync.NewMap[string, *SupplierState](),
		querySubpool: querySubpool,
	}

	// Sizes are not measured under load: a 100k-leaf rebuild took 0.34 s on a
	// synthetic tree, and holds its leaves and every node in memory while it
	// runs.
	if config.WorkerPool != nil {
		mgr.coldCompactionPool = CreateBoundedSubpool(componentLogger, config.WorkerPool, coldCompactionWorkers, "smst_cold_compaction")
		mgr.rebuildAdmission = NewRebuildAdmission(componentLogger)
	}

	// Construct a shared deduplicator if we have a Redis client. Falls back to
	// nil if Redis is absent (e.g. tests) — handleRelay treats nil as fail-open.
	if config.RedisClient != nil {
		// KeyPrefix empty → defaults to "ha:miner:dedup" (matches KeyBuilder.MinerDedupKey).
		//
		// BlockTimeSeconds forwarded from config, not left zero: an empty
		// DeduplicatorConfig here used to mean the operator's configured
		// block_time_seconds was silently dropped, and NewRedisDeduplicator's
		// own fallback (30) took over regardless of what was set. On mainnet
		// (verified live 2026-08-21, ~64s/block) that produced a dedup TTL
		// (TTLBlocks=10 x 30s = 5min) roughly HALF the wall-clock window it
		// was meant to cover (~10.7min) -- a relay duplicate arriving after 5
		// minutes but within the intended 10-block window would no longer be
		// caught, and would be counted a second time.
		mgr.deduplicator = NewRedisDeduplicator(
			componentLogger,
			config.RedisClient,
			DeduplicatorConfig{BlockTimeSeconds: config.BlockTimeSeconds},
		)
	}

	return mgr
}

// Deduplicator returns the shared deduplicator (may be nil).
func (m *SupplierManager) Deduplicator() Deduplicator {
	return m.deduplicator
}

// SetRelayHandler sets the callback for processing incoming relays.
func (m *SupplierManager) SetRelayHandler(handler func(ctx context.Context, supplierAddr string, msg *transport.StreamMessage) error) {
	m.onRelay = handler
}

// Start starts the supplier manager and begins processing.
func (m *SupplierManager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("supplier manager is closed")
	}
	m.ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	if m.rebuildAdmission != nil {
		go logging.RecoverGoRoutine(m.logger, "ingestion_memory_brake", m.rebuildAdmission.RunMemoryBrake)(m.ctx)
	}

	// Register for key changes
	m.keyManager.OnKeyChange(m.onKeyChange)

	if err := m.startWithDistributedClaiming(ctx, m.keyManager.ListSuppliers()); err != nil {
		return err
	}

	// 0 disables (tests drive reconcile directly); negative picks up the default.
	interval := m.config.SupplierReconcileInterval
	if interval < 0 {
		interval = DefaultSupplierReconcileInterval
	}
	if interval > 0 {
		go m.reconcileLoop(m.ctx, interval)
		m.logger.Info().
			Dur("interval", interval).
			Msg("supplier stake reconcile loop started")
	}

	return nil
}

// startWithDistributedClaiming starts the manager with distributed supplier claiming.
// Suppliers are claimed via Redis leases and distributed fairly across miners.
//
// The claimer is created unconditionally — even with zero staked suppliers —
// so the background reconciler has a target to push into once a key's
// on-chain stake lands. Before this, an operator who started the miner with
// a key that was not yet staked on-chain had no way for the miner to pick
// up the stake without a process restart.
func (m *SupplierManager) startWithDistributedClaiming(ctx context.Context, supplierAddrs []string) error {
	m.logger.Debug().
		Int("total_keys", len(supplierAddrs)).
		Msg("starting with distributed claiming")

	// Filter to only staked suppliers - don't claim keys that aren't staked on-chain
	stakedSuppliers := m.filterStakedSuppliers(ctx, supplierAddrs)

	m.logger.Debug().
		Int("total_keys", len(supplierAddrs)).
		Int("staked_suppliers", len(stakedSuppliers)).
		Int("skipped_non_staked", len(supplierAddrs)-len(stakedSuppliers)).
		Msg("filtered suppliers by staking status")

	// Create the claimer (always, even with empty staked set).
	m.claimer = NewSupplierClaimer(
		m.logger,
		m.config.RedisClient,
		m.config.MinerID,
		m.config.ClaimerConfig,
	)
	m.claimer.SetCallbacks(
		m.onSupplierClaimed,
		m.onSupplierReleased,
	)

	if err := m.claimer.Start(ctx, stakedSuppliers); err != nil {
		return fmt.Errorf("failed to start supplier claimer: %w", err)
	}

	m.startConnPoolResizeLoop()

	m.logger.Info().
		Int("claimed", m.claimer.ClaimedCount()).
		Int("staked_suppliers", len(stakedSuppliers)).
		Int("total_keys", len(supplierAddrs)).
		Bool("distributed_claiming", true).
		Msg("supplier manager started with distributed claiming")

	m.checkPoolSize(len(stakedSuppliers))

	// Start periodic stream trimming (removes entries older than CacheTTL).
	// Safe because relays older than CacheTTL are already invalid
	// (session/claim windows are closed, so they can't earn rewards).
	go m.runStreamTrimmer(ctx)

	return nil
}

// reconcile re-runs the staking filter over the keyring and pushes the
// result into the claimer. Called by the background poller in Start and
// directly by tests that want deterministic behaviour.
func (m *SupplierManager) reconcile(ctx context.Context) {
	if m.claimer == nil {
		return
	}
	configured := m.filterStakedSuppliers(ctx, m.keyManager.ListSuppliers())
	m.claimer.UpdateSuppliers(configured)
	m.releaseUnconfigured(ctx, configured)
}

// releaseUnconfigured releases the lease on every supplier this instance still
// holds that the staking filter no longer returns.
//
// Without it, dropping out of the configured list changed nothing but a slice:
// UpdateSuppliers replaced allSuppliers and nothing compared that against the
// leased set, so the supplier's consumer, SMST manager, lifecycle manager and
// claim-key renewal all kept running forever for an address that is no longer
// staked. releaseExcess could not cover it either -- it picks victims
// newest-claimed-first out of the leased set to hit a fair-share target, so it
// cannot target a specific dropped address, and it only fires when the instance
// holds MORE than its share.
//
// Release is cheap here and does not serialise: it makes no Redis call, and
// onSupplierReleased hands the actual teardown -- and the delete of the lease at
// its end -- to its own goroutine. The drain window each teardown then spends is
// paid in parallel, not one after another.
//
// Suppliers with pending sessions are NOT dropped by the filter in the first
// place (it keeps them so claim and proof can finish), so nothing here can cut
// a session short.
func (m *SupplierManager) releaseUnconfigured(ctx context.Context, configured []string) {
	claimed := m.claimer.ClaimedSuppliers()
	if len(claimed) == 0 {
		return
	}

	keep := make(map[string]struct{}, len(configured))
	for _, addr := range configured {
		keep[addr] = struct{}{}
	}

	for _, addr := range claimed {
		if _, ok := keep[addr]; ok {
			continue
		}
		m.logger.Info().
			Str(logging.FieldSupplier, addr).
			Msg("releasing lease: supplier is no longer staked or no longer configured")
		// Two causes arrive here and only one is a removed key: a supplier that
		// unstaked still has its key, and the fleet can still sign its pending
		// work, so it is handed over. One whose key is gone is torn down as a key
		// removal -- this is also where a key-change release that kept its claim
		// is retried.
		trigger := triggerRebalanceRelease
		if !m.teardownCanFinishWork(addr) {
			trigger = triggerKeyRemoval
		}
		if err := m.claimer.Release(ctx, addr, trigger); err != nil {
			// Release already logged the reason and kept the claim; the next
			// reconcile pass retries. Nothing is stranded by a single failure.
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSupplier, addr).
				Msg("failed to release lease for an unconfigured supplier; will retry next reconcile")
		}
	}
}

// reconcileLoop runs the background stake poller until ctx is cancelled.
// Interval is driven by SupplierReconcileInterval; a zero interval means
// the loop is disabled.
func (m *SupplierManager) reconcileLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

// checkPoolSize validates that the Redis connection pool is large enough for the number of suppliers.
// Each supplier holds 1 connection for its blocking stream read, re-issued
// every block interval, so the connection is held continuously in practice.
// Formula: poolSize = numSuppliers + 20 overhead
func (m *SupplierManager) checkPoolSize(numSuppliers int) {
	poolSize := m.config.RedisClient.PoolSize()
	minRequired := numSuppliers + 20 // Formula: numSuppliers + 20 overhead

	if poolSize < minRequired {
		m.logger.Warn().
			Int("pool_size", poolSize).
			Int("num_suppliers", numSuppliers).
			Int("min_required", minRequired).
			Msg("INSUFFICIENT Redis pool size! Formula: pool_size = numSuppliers + 20. " +
				"You WILL see 'redis: connection pool timeout' errors. " +
				"Set redis.pool_size in config to at least the min_required value.")
	} else {
		m.logger.Info().
			Int("pool_size", poolSize).
			Int("num_suppliers", numSuppliers).
			Int("min_required", minRequired).
			Int("headroom", poolSize-minRequired).
			Msg("Redis pool size is sufficient for TRUE PUSH consumption")
	}
}

// filterStakedSuppliers queries the chain to check staking status for ALL addresses.
// Writes ALL addresses to Redis cache with their staking status (staked: true/false).
// Returns only addresses that are actually staked as suppliers on-chain.
func (m *SupplierManager) filterStakedSuppliers(ctx context.Context, supplierAddrs []string) []string {
	if m.config.SupplierQueryClient == nil {
		m.logger.Warn().Msg("no supplier query client - cannot filter by staking status, using all keys")
		return supplierAddrs
	}

	stakedSuppliers := make([]string, 0, len(supplierAddrs))
	var unstakedCount int
	var queryErrors int

	for _, addr := range supplierAddrs {
		// Invalidate cache before querying to ensure fresh chain data.
		// Without this, staking changes (e.g. new services) are invisible
		// until the miner restarts (see BUG-SUPPLIER-CACHE-FIX.md).
		m.config.SupplierQueryClient.InvalidateSupplier(addr)

		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		supplier, err := m.config.SupplierQueryClient.GetSupplier(queryCtx, addr)
		cancel()

		if err != nil {
			// Check if it's a NotFound error (not staked)
			if query.IsEntityNotFound(err) {
				// Write NOT STAKED status to Redis cache for visibility
				m.writeSupplierStatusToCache(ctx, addr, false, nil, nil, 0)

				// Drain-gated removal: if the supplier still has non-terminal
				// sessions in Redis (active / claiming / claimed / proving),
				// keep it in the claimer's list so the per-supplier mining
				// pipeline (stream consumer, SMST, lifecycle manager) stays
				// alive until claim+proof settle. Removing it now would tear
				// down the pipeline and orphan the pending work — the claim
				// would never be submitted and the relays would be lost.
				//
				// Once every session for this supplier is terminal (Proved /
				// ProbabilisticProved / ClaimSkipped / *WindowClosed /
				// *TxError), the next reconcile pass will see NotFound + no
				// pending work and drop the supplier, which triggers the
				// normal release → verifySupplierUnstaked → removeSupplier
				// drain path in onSupplierReleased.
				//
				// releaseUnconfigured is what closes that loop: dropping out
				// of this list only changes a slice, so something has to
				// compare the configured list against the leased set and call
				// Release. Until 2026-08-20 nothing did, and this paragraph
				// described a teardown that never happened -- the pipeline for
				// an unstaked supplier ran forever.
				if m.hasPendingSessions(ctx, addr) {
					m.logger.Info().
						Str("address", addr).
						Msg("supplier unstaked on-chain but still has pending sessions; keeping in claimer until they settle")
					stakedSuppliers = append(stakedSuppliers, addr)
					continue
				}

				m.logger.Debug().
					Str("address", addr).
					Msg("skipping non-staked address (not a supplier on-chain)")
				unstakedCount++
				continue
			}
			// Network/timeout error — fail-open: treat as staked to avoid false
			// drains. The cache entry is deliberately left untouched: a stale
			// answer beats a wrong one, and the next pass with a working node
			// writes the truth.
			//
			// Counted under the same label the sibling path in
			// resolveAndPublishSupplierState uses for this event, because it IS
			// the same event: a cache write skipped because the chain could not
			// be read. Until this line existed only one of the two paths
			// reported it, so "the entry is current" and "we never managed to
			// check" were indistinguishable in Prometheus.
			//
			// Debug, not Warn, for the per-supplier line: this fires once per
			// supplier per reconcile pass, so an unreachable fullnode turns it
			// into one line per supplier every interval. The operator-facing
			// signal is the single aggregated Warn after the loop plus this
			// metric -- per-entity conditions do not belong at Warn.
			queryErrors++
			supplierCacheWriteSkipped.WithLabelValues("chain_query_error").Inc()
			m.logger.Debug().
				Err(err).
				Str("address", addr).
				Msg("failed to query supplier status, treating as staked (fail-open)")
			stakedSuppliers = append(stakedSuppliers, addr)
			continue
		}

		// Supplier is staked. Resolve services height-aware, with a boot-time
		// fallback (see resolveSupplierServices). An unreliable result (no
		// height AND empty snapshot) must never be persisted — that is the
		// contaminated tuple — so the previous cache entry is preserved and
		// the next reconcile pass (with a real height) writes the truth.
		services, endpoints, reliable := m.resolveSupplierServices(ctx, &supplier, addr)
		if reliable {
			m.writeSupplierStatusToCache(ctx, addr, true, services, endpoints, supplier.GetUnstakeSessionEndHeight())
			// Refresh the in-memory view too, or the drain write republishes
			// the ADD-TIME services and endpoints over what this pass just
			// wrote: a supplier that restaked to add a service after being
			// claimed would have that service dropped for the whole drain
			// window, and decideSupplierServe would answer wrong_service.
			if st, ok := m.suppliers.Load(addr); ok {
				st.stakeView.Store(&supplierStakeView{Services: services, StakedEndpoints: endpoints})
			}
		} else {
			supplierCacheWriteSkipped.WithLabelValues("unreliable_boot_snapshot").Inc()
		}
		stakedSuppliers = append(stakedSuppliers, addr)
	}

	// One aggregated line per pass, not one per supplier: this is the signal an
	// operator reads during a fullnode outage, and it must be visible without
	// turning on debug logging. Warn rather than Info because every supplier it
	// counts is now being served on an unverified assumption -- fail-open is the
	// right call, but it is still an assumption, and its scale is the thing worth
	// seeing.
	if queryErrors > 0 {
		m.logger.Warn().
			Int("query_errors", queryErrors).
			Int("total_keys", len(supplierAddrs)).
			Msg("could not read staking status for some addresses; treated as staked (fail-open) " +
				"and their cache entries left untouched")
	}

	m.logger.Debug().
		Int("staked", len(stakedSuppliers)).
		Int("not_staked", unstakedCount).
		Int("query_errors", queryErrors).
		Int("total_keys", len(supplierAddrs)).
		Msg("checked staking status for all key addresses")

	return stakedSuppliers
}

// hasPendingSessions returns true when the given supplier still has at least
// one session in a non-terminal state persisted in Redis. Used by the
// reconcile path to defer the removal of a NotFound-on-chain supplier until
// its in-flight claim+proof work has settled.
//
// A session is "pending" while its SessionState is anything other than a
// terminal state (see SessionState.IsTerminal). Terminal states include
// SessionStateProved, SessionStateProbabilisticProved, SessionStateClaimSkipped,
// SessionStateClaimWindowClosed, SessionStateClaimTxError,
// SessionStateProofWindowClosed, and SessionStateProofTxError.
//
// On Redis errors we conservatively return true so the supplier stays in the
// claimer: losing revenue to a false-drain is worse than carrying a dead
// supplier for one extra reconcile interval.
func (m *SupplierManager) hasPendingSessions(ctx context.Context, supplierAddr string) bool {
	if m.config.RedisClient == nil {
		return false
	}
	store := NewRedisSessionStore(
		m.logger,
		m.config.RedisClient,
		SessionStoreConfig{
			SupplierAddress: supplierAddr,
			SessionTTL:      m.config.SessionTTL,
		},
	)
	defer func() { _ = store.Close() }()

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sessions, err := store.GetBySupplier(queryCtx)
	if err != nil {
		m.logger.Warn().
			Err(err).
			Str("address", supplierAddr).
			Msg("failed to enumerate sessions for drain-gate check; treating as pending (fail-safe)")
		return true
	}
	for _, snap := range sessions {
		if snap == nil {
			continue
		}
		if !snap.State.IsTerminal() {
			return true
		}
	}
	return false
}

// writeSupplierStatusToCache writes a supplier's staking status to Redis cache.
// This allows the CLI and other tools to see all configured addresses and their status.
//
// unstakeSessionEndHeight should be set to supplier.GetUnstakeSessionEndHeight() for
// staked suppliers, or 0 for not-staked suppliers. A non-zero value causes the
// status to be written as SupplierStatusUnstaking instead of SupplierStatusActive,
// reflecting that the supplier is mid-unstake but still serving relays until its
// service configs deactivate at the next session boundary.
func (m *SupplierManager) writeSupplierStatusToCache(ctx context.Context, addr string, staked bool, services []string, endpoints []cache.StakedEndpoint, unstakeSessionEndHeight uint64) {
	if m.config.SupplierCache == nil {
		return
	}

	var status string
	if !staked {
		status = cache.SupplierStatusNotStaked
	} else if unstakeSessionEndHeight > 0 {
		status = cache.SupplierStatusUnstaking
	} else {
		status = cache.SupplierStatusActive
	}

	state := &cache.SupplierState{
		Status:                  status,
		Staked:                  staked,
		OperatorAddress:         addr,
		Services:                services,
		StakedEndpoints:         endpoints,
		UnstakeSessionEndHeight: unstakeSessionEndHeight,
		UpdatedBy:               m.config.MinerID,
	}

	if err := m.config.SupplierCache.SetSupplierState(ctx, state); err != nil {
		m.logger.Warn().
			Err(err).
			Str("address", addr).
			Bool("staked", staked).
			Msg("failed to write supplier status to cache")
	} else {
		m.logger.Debug().
			Str("address", addr).
			Bool("staked", staked).
			Str("status", status).
			Uint64("unstake_session_end_height", unstakeSessionEndHeight).
			Msg("wrote supplier status to cache")
	}
}

// verifySupplierUnstaked queries the chain to confirm a supplier is genuinely unstaked
// before proceeding with a drain. Returns (shouldDrain, verifyResult).
// Fail-safe: on network/timeout errors, returns shouldDrain=false to avoid draining
// a potentially-staked supplier.
func (m *SupplierManager) verifySupplierUnstaked(ctx context.Context, addr string, drainReason string) (shouldDrain bool, verifyResult string) {
	if m.config.SupplierQueryClient == nil {
		return false, "no_query_client"
	}

	// Invalidate cache to get fresh staking status from chain.
	m.config.SupplierQueryClient.InvalidateSupplier(addr)

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := m.config.SupplierQueryClient.GetSupplier(queryCtx, addr)
	if err == nil {
		// Supplier IS staked on-chain — abort drain
		return false, "staked"
	}

	if query.IsEntityNotFound(err) {
		// Supplier genuinely not staked
		return true, "not_found"
	}

	// Network/timeout error — fail-safe: don't drain
	m.logger.Warn().
		Err(err).
		Str("address", addr).
		Str("drain_reason", drainReason).
		Msg("failed to verify supplier staking status, aborting drain (fail-safe)")
	return false, "error"
}

// onSupplierClaimed is called when a supplier is successfully claimed.
// It starts the supplier lifecycle (consumer, SMST, claim/proof submission).
func (m *SupplierManager) onSupplierClaimed(ctx context.Context, supplier string) error {
	m.logger.Debug().
		Str("supplier", supplier).
		Msg("claimed supplier, starting handoff validation")

	// Added now, the supplier would start after Close collected the others,
	// and nothing would tear it down.
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return fmt.Errorf("supplier manager is closed")
	}

	// Check if we already have this supplier
	if _, exists := m.suppliers.Load(supplier); exists {
		m.logger.Debug().Str("supplier", supplier).Msg("supplier already initialized")
		return nil
	}

	// Warmup this supplier's data from chain
	warmupData := m.warmupSingleSupplier(ctx, supplier)

	// Add the supplier with handoff validation
	if err := m.addSupplierWithHandoff(ctx, supplier, warmupData); err != nil {
		return fmt.Errorf("failed to add claimed supplier: %w", err)
	}

	return nil
}

// onSupplierReleased is called when a supplier claim is released.
//
// All callsites of SupplierClaimer.Release that invoke this callback
// (rebalance, claim-callback-failure) operate on suppliers that
// are expected to remain staked on-chain — the release is an internal
// handoff between miner instances, not a chain-level unstake. Issue #7:
// vetoing drains here when the supplier is still staked permanently
// pinned the supplier to the original miner and blocked every
// fair-share rebalance. We still query the chain so the existing
// drain-decision metric retains its observability value, but the result
// no longer vetoes the drain.
//
// The supplier leaves the map and its consume loop is cancelled HERE, before
// the release returns -- both local and instant -- so the old loop stops
// writing now. The rest of the drain runs in its own goroutine because
// Consumer.Close can sit on a blocked XREAD for tens of seconds while the
// consumer goroutine notices ctx cancellation; running it synchronously here
// would block the claimer's rebalance and renewal loops. The lease stays this
// instance's until that goroutine ends and deletes it (FinishRelease), so no
// peer writes the supplier's tree or acknowledges its relays while the old loop
// still does.
func (m *SupplierManager) onSupplierReleased(ctx context.Context, supplier, trigger string) error {
	// No release is a shutdown: Close tears its suppliers down itself.
	reason := drainRebalance
	if trigger == triggerKeyRemoval {
		// With a claimer -- every manager past Start -- this is how the key-change
		// callback takes a removed key to the teardown. Mapped to a rebalance, the
		// batch and the delivery buffer were RELEASED to a fleet that cannot sign
		// them, and relays_dropped_no_key never counted.
		reason = drainKeyRemoved
	}

	// The key-removal path already ran this exact verification one call up
	// (see the "Key removed" branch), on a supplier an operator touched by
	// hand. Two 5s chain queries for one decision bought nothing.
	audit := trigger != triggerKeyRemoval

	// The audit is observational: its bool is discarded and the comment on
	// verifySupplierUnstaked says the result no longer vetoes the drain. It
	// must therefore not sit in front of anything. It used to run inline, and
	// the renewal loop -- which is SERIAL over every supplier -- now calls
	// this on lease loss: K lost leases would have cost up to K*5s inside a
	// loop with 90s of total headroom, and the event that loses leases (Redis
	// blinking, a slow full node) is the same one that makes this query slow.
	// One lost lease would have cascaded into more.
	//
	// The context is deliberately NOT inherited from a cancellable parent:
	// Close cancels the manager context while drains started before it may
	// still be running, and every one of them would record "error".
	auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(ctx), supplierDrainAuditTimeout)

	// Checked and added under the lock Close takes to set closed: no drain is
	// added once Close is waiting for them. A release refused here is undone
	// by the claimer, and Close tears the supplier down and deletes its lease.
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		cancelAudit()
		return fmt.Errorf("supplier manager is closed")
	}
	m.drainWG.Add(1)
	m.mu.RUnlock()

	state, exists := m.stopSupplier(supplier, reason)

	drain := func(context.Context) {
		defer m.drainWG.Done()
		defer cancelAudit()
		// Deferred so it runs even if the teardown panics: a supplier left
		// draining could never be claimed by this instance again.
		defer m.finishDrainLease(ctx, supplier)
		m.extendDrainLease(ctx, supplier)
		if audit {
			_, verifyResult := m.verifySupplierUnstaked(auditCtx, supplier, trigger)
			supplierDrainDecisionTotal.WithLabelValues(trigger, verifyResult).Inc()

			m.logger.Info().
				Str("supplier", supplier).
				Str("drain_trigger", trigger).
				Str("on_chain_result", verifyResult).
				Str("instance_id", m.config.MinerID).
				Msg("drain decision audit")
		}
		if exists {
			m.teardownSupplier(state)
		}
	}

	go logging.RecoverGoRoutine(m.logger, "supplier_drain", drain)(auditCtx)
	return nil
}

// extendDrainLease keeps a released supplier's lease for the drain budget.
func (m *SupplierManager) extendDrainLease(ctx context.Context, supplier string) {
	if m.claimer == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainLeaseCallTimeout)
	defer cancel()
	if err := m.claimer.ExtendDrainLease(callCtx, supplier, drainLeaseBudget()); err != nil {
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSupplier, supplier).
			Msg("failed to extend the lease of a draining supplier; it keeps its current TTL")
	}
}

// finishDrainLease deletes a released supplier's lease once its drain is over.
func (m *SupplierManager) finishDrainLease(ctx context.Context, supplier string) {
	if m.claimer == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainLeaseCallTimeout)
	defer cancel()
	if err := m.claimer.FinishRelease(callCtx, supplier); err != nil {
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSupplier, supplier).
			Msg("failed to delete the lease of a drained supplier; it expires on its drain budget")
	}
}

// warmupSingleSupplier queries chain data for a single supplier.
func (m *SupplierManager) warmupSingleSupplier(ctx context.Context, supplier string) *SupplierWarmupData {
	// Invalidate cache so warmup always gets the latest on-chain state.
	m.config.SupplierQueryClient.InvalidateSupplier(supplier)

	chainSupplier, err := m.config.SupplierQueryClient.GetSupplier(ctx, supplier)
	if err != nil {
		m.logger.Warn().
			Err(err).
			Str("supplier", supplier).
			Msg("failed to query supplier from chain during warmup")
		return nil
	}

	services, endpoints, reliable := m.resolveSupplierServices(ctx, &chainSupplier, supplier)
	if !reliable {
		// An empty snapshot with no observed height carries no usable
		// information; returning nil makes the caller fall back to the
		// chain-query path, whose own resolve run will skip the cache write
		// (see resolveAndPublishSupplierState) instead of persisting the
		// contaminated tuple.
		return nil
	}

	return &SupplierWarmupData{
		OwnerAddress:    chainSupplier.OwnerAddress,
		Services:        services,
		StakedEndpoints: endpoints,
	}
}

// serviceIDs extracts the non-nil service IDs from a config list.
func serviceIDs(configs []*sharedtypes.SupplierServiceConfig) []string {
	out := make([]string, 0, len(configs))
	for _, svc := range configs {
		if svc != nil {
			out = append(out, svc.ServiceId)
		}
	}
	return out
}

// resolveSupplierServices returns the service IDs a supplier should serve,
// plus whether the result is reliable enough to PERSIST to the shared cache.
//
// Preferred source is the height-aware active set from ServiceConfigHistory:
// poktroll schedules service additions/removals via activation_height /
// deactivation_height applied at session boundaries, and the denormalized
// supplier.Services snapshot cuts too fast (a service removed mid-session
// must keep serving until its deactivation_height; same in reverse for
// future activations).
//
// BOOT FALLBACK: at miner boot this runs before the first Redis block event
// arrives, so BlockClient.LastBlock reports height 0 and the height-aware
// set is empty for every mainnet supplier (all activation heights > 0).
// Persisting that empty set is the contaminated tuple {staked, active,
// services:[]}: relayers treat it as a cache miss and 503 the supplier's
// relays until the next reconcile heals it (observed live: 1585 rejections
// in one second after a restart). When the height is unknown, fall back to
// the denormalized snapshot — slightly stale on scheduled changes for at
// most one reconcile interval, never empty for a serving supplier.
//
// reliable=false means "do not persist": the height is unknown AND the
// snapshot is empty, so writing would recreate the contaminated tuple the
// commit forbids — callers preserve the previous cache entry instead. A
// legitimately-empty active set at a KNOWN height stays reliable: that is
// authoritative on-chain state.
func (m *SupplierManager) resolveSupplierServices(ctx context.Context, supplier *sharedtypes.Supplier, addr string) (services []string, endpoints []cache.StakedEndpoint, reliable bool) {
	// A nil BlockClient (tooling/tests) can never observe a height; the
	// snapshot is permanently the best available source, so it is used
	// without the boot metric/log (which would otherwise fire forever and
	// break the metric's "nonzero outside boot windows" alert contract).
	if m.config.BlockClient == nil {
		configs := supplier.Services
		return serviceIDs(configs), extractStakedEndpoints(configs), len(configs) > 0
	}

	var currentHeight int64
	if block := m.config.BlockClient.LastBlock(ctx); block != nil {
		currentHeight = block.Height()
	}

	if currentHeight == 0 {
		configs := supplier.Services
		services = serviceIDs(configs)
		supplierBootServicesFallback.Inc()
		// Debug, not Info: this fires per supplier and, if block events are
		// broken after a restart, on every reconcile pass — the sustained
		// fallback counter rate is the operator signal, not the log line.
		m.logger.Debug().
			Str(logging.FieldSupplier, addr).
			Int("services", len(services)).
			Msg("no block height observed yet (boot); using denormalized service snapshot until first reconcile")
		return services, extractStakedEndpoints(configs), len(services) > 0
	}

	configs := supplier.GetActiveServiceConfigs(currentHeight)
	return serviceIDs(configs), extractStakedEndpoints(configs), true
}

// extractStakedEndpoints flattens a supplier's service configs into per-transport
// (service, rpc_type) endpoints for the shared registry, so a relayer can tell
// which transports the supplier actually declared on-chain. The RpcType enum is
// mapped to the canonical relayer backend-type string; endpoints whose enum this
// build cannot map are skipped (a future protocol transport). Deduplicated per
// (service, backend-type) — a service may list the same transport on several
// endpoint URLs. Derived from the SAME config list as the service projection so
// the two views never disagree.
func extractStakedEndpoints(configs []*sharedtypes.SupplierServiceConfig) []cache.StakedEndpoint {
	seen := make(map[string]struct{})
	var endpoints []cache.StakedEndpoint
	for _, svc := range configs {
		if svc == nil {
			continue
		}
		for _, ep := range svc.Endpoints {
			if ep == nil {
				continue
			}
			backendType, ok := relayer.RPCTypeEnumToBackendType(ep.RpcType)
			if !ok {
				continue
			}
			key := svc.ServiceId + "\x00" + backendType
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			endpoints = append(endpoints, cache.StakedEndpoint{ServiceID: svc.ServiceId, RpcType: backendType})
		}
	}
	return endpoints
}

// addSupplierWithHandoff adds a supplier with handoff validation.
// This logs the inherited sessions and validates SMST state.
func (m *SupplierManager) addSupplierWithHandoff(ctx context.Context, supplier string, warmupData *SupplierWarmupData) error {
	// First, load existing sessions from Redis to validate handoff
	sessionStore := NewRedisSessionStore(
		m.logger,
		m.config.RedisClient,
		SessionStoreConfig{
			SupplierAddress: supplier,
			SessionTTL:      m.config.SessionTTL,
		},
	)

	// Get all sessions for this supplier
	sessions, err := sessionStore.GetBySupplier(ctx)
	if err != nil {
		m.logger.Warn().
			Err(err).
			Str("supplier", supplier).
			Msg("failed to load existing sessions during handoff")
	} else {
		m.logger.Debug().
			Str("supplier", supplier).
			Int("sessions", len(sessions)).
			Msg("loaded existing sessions during handoff")

		// Validate each session's SMST exists — but only for sessions that
		// still need processing. Terminal sessions (claimed, proved, etc.) have
		// already had their SMST flushed and submitted, so missing SMST is expected.
		activeCount := 0
		terminalCount := 0
		missingSmstCount := 0
		for _, session := range sessions {
			// A session whose claim is sent or being sent takes no more relays, so
			// a missing SMST is not a defect here. It is not gone either: its
			// proof is built from it, and it is deleted only when the session
			// reaches a terminal state (DeleteTree, from the lifecycle callback).
			if session.State.IsTerminal() || session.State == SessionStateClaimed || session.State == SessionStateClaiming {
				terminalCount++
				continue
			}

			activeCount++
			smstKey := m.config.RedisClient.KB().SMSTNodesKey(supplier, session.SessionID)
			exists, _ := m.config.RedisClient.Exists(ctx, smstKey).Result()

			if exists == 0 && session.RelayCount > 0 {
				missingSmstCount++
				m.logger.Warn().
					Str("supplier", supplier).
					Str("session_id", session.SessionID).
					Int64("relay_count", session.RelayCount).
					Str("state", string(session.State)).
					Msg("HANDOFF: active session has relays but no SMST tree, relays will be re-consumed from stream")
			}
		}

		if len(sessions) > 0 {
			m.logger.Info().
				Str("supplier", supplier).
				Int("total_sessions", len(sessions)).
				Int("active", activeCount).
				Int("terminal", terminalCount).
				Int("missing_smst", missingSmstCount).
				Msg("handoff session summary")
		}
	}

	// Now add the supplier normally
	return m.addSupplierWithData(ctx, supplier, warmupData)
}

// SupplierWarmupData holds pre-fetched supplier data from the chain.
type SupplierWarmupData struct {
	OwnerAddress    string
	Services        []string
	StakedEndpoints []cache.StakedEndpoint
}

// keyChangeReadCtx captures m.ctx under a short m.mu.RLock.
//
// Start() assigns m.ctx under m.mu.Lock(); onKeyChange can fire on an
// arbitrary key-manager goroutine concurrently with Start and Close.
// Capturing into a local with a brief read lock gives the callback a
// stable context for the rest of its work without holding the mutex
// across network calls. Mirrors the pattern Close() uses for
// m.closed.
func (m *SupplierManager) keyChangeReadCtx() context.Context {
	m.mu.RLock()
	ctx := m.ctx
	m.mu.RUnlock()
	return ctx
}

// onKeyChange handles key addition/removal notifications.
//
// Runs on the key-manager's callback goroutine. Captures m.ctx once
// under m.mu.RLock() at entry and uses the local for every downstream
// call — do NOT read m.ctx directly anywhere below, that is a race
// against Start() / Close() (which write m.ctx under m.mu).
func (m *SupplierManager) onKeyChange(operatorAddr string, added bool) {
	ctx := m.keyChangeReadCtx()
	m.handleKeyChange(ctx, operatorAddr, added)
}

// handleKeyChange is the body of onKeyChange with the lifecycle
// context passed in explicitly. Split out so tests can drive the
// callback shape without touching m.ctx through the package lock.
func (m *SupplierManager) handleKeyChange(ctx context.Context, operatorAddr string, added bool) {
	if added {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("key added via hot-reload")

		// Check if supplier is staked on-chain before processing
		if m.config.SupplierQueryClient != nil {
			queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := m.config.SupplierQueryClient.GetSupplier(queryCtx, operatorAddr)
			cancel()

			if err != nil {
				// Check if it's a NotFound error (not staked)
				if query.IsEntityNotFound(err) {
					// The stake tx may land after the keyring change fires
					// the hot-reload callback. Return without claiming; the
					// periodic reconcile loop re-runs filterStakedSuppliers
					// over every keyring entry at SupplierReconcileInterval
					// and will pick this key up once the stake is visible.
					m.logger.Info().
						Str("address", operatorAddr).
						Msg("hot-reloaded key is not yet staked on-chain; will retry at next reconcile tick")
					return
				}
				// Network/timeout error — fail-open: proceed with adding (same principle as filterStakedSuppliers)
				m.logger.Warn().
					Err(err).
					Str("address", operatorAddr).
					Msg("failed to query supplier status for hot-reloaded key, proceeding (fail-open)")
			}
		}

		// Supplier is staked - proceed with adding
		if m.claimer != nil {
			// Distributed claiming mode: update claimer's supplier list
			// The claimer will handle claiming via rebalance
			allSuppliers := m.keyManager.ListSuppliers()
			stakedSuppliers := m.filterStakedSuppliers(ctx, allSuppliers)
			m.claimer.UpdateSuppliers(stakedSuppliers)
			m.logger.Debug().
				Str(logging.FieldSupplier, operatorAddr).
				Int("total_staked", len(stakedSuppliers)).
				Msg("updated claimer with hot-reloaded staked supplier")
		} else {
			// Single-miner mode: add directly
			if err := m.addSupplierWithData(ctx, operatorAddr, nil); err != nil {
				m.logger.Error().
					Err(err).
					Str(logging.FieldSupplier, operatorAddr).
					Msg("failed to add supplier")
			}
		}
	} else {
		// Key removed — verify on-chain, but per user decision: drain even if staked (operator explicit action)
		shouldDrain, verifyResult := m.verifySupplierUnstaked(ctx, operatorAddr, "key_removal")
		supplierDrainDecisionTotal.WithLabelValues("key_removal", verifyResult).Inc()

		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Str("drain_trigger", "key_removal").
			Str("on_chain_result", verifyResult).
			Str("instance_id", m.config.MinerID).
			Msg("drain decision audit")

		if !shouldDrain && verifyResult == "staked" {
			m.logger.Warn().
				Str(logging.FieldSupplier, operatorAddr).
				Str("on_chain_result", verifyResult).
				Msg("CRITICAL: draining staked supplier due to explicit key removal by operator")
		}

		// Update claimer if in distributed mode
		if m.claimer == nil {
			// Single-miner mode: no lease exists, so tear the pipeline down directly.
			go m.removeSupplier(operatorAddr, drainKeyRemoved)
			return
		}

		allSuppliers := m.keyManager.ListSuppliers()
		stakedSuppliers := m.filterStakedSuppliers(ctx, allSuppliers)
		m.claimer.UpdateSuppliers(stakedSuppliers)

		// Release rather than calling removeSupplier directly. Both tear the
		// pipeline down -- Release reaches it through onSupplierReleased -- but
		// only Release, through the drain it starts, deletes this instance's
		// claim key and drops the address from the leased set. Going straight
		// to removeSupplier left the lease behind, and renewAllClaims iterates
		// the leased set, so the miner kept EXPIREing ha:miner:claim:{addr}
		// forever for a supplier it no longer had any state for. No other
		// instance could take that supplier over while the key kept being
		// renewed.
		//
		// The audit above is the ONLY one for this path: onSupplierReleased
		// skips its own when the trigger is key_removal, because it would be
		// the same 5s chain query about the same supplier for the same
		// decision. There is one drain_decision entry here, not two.
		if err := m.claimer.Release(ctx, operatorAddr, triggerKeyRemoval); err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to release lease after key removal; the next reconcile retries")
		}
	}
}

// addSupplierWithData adds a new supplier to the manager with optional pre-warmed data.
// If prewarmedData is nil, it will query fresh data from the chain.
func (m *SupplierManager) addSupplierWithData(ctx context.Context, operatorAddr string, prewarmedData *SupplierWarmupData) error {
	// Fast-path duplicate check. The atomic LoadOrStore below is the
	// authoritative guard against concurrent adds; this early Load just
	// avoids building ~hundreds of objects when the answer is obvious.
	if _, exists := m.suppliers.Load(operatorAddr); exists {
		return nil
	}

	// Create supplier-specific context
	supplierCtx, cancelFn := context.WithCancel(ctx)

	// Create session store for this supplier
	sessionStore := NewRedisSessionStore(
		m.logger,
		m.config.RedisClient,
		SessionStoreConfig{
			SupplierAddress: operatorAddr,
			SessionTTL:      m.config.SessionTTL,
		},
	)

	// Create session coordinator (replaces WAL-based SMSTSnapshotManager)
	// No WAL needed - SMST persists to Redis via Commit(), and relay streams act as WAL
	sessionCoordinator := NewSessionCoordinator(
		m.logger,
		sessionStore,
		SMSTRecoveryConfig{
			SupplierAddress: operatorAddr,
			RecoveryTimeout: 5 * time.Minute,
		},
	)

	// Create consumer for this supplier (single stream per supplier, fast 100ms polling)
	consumer, err := redistransport.NewStreamsConsumer(
		m.logger,
		m.config.RedisClient,
		transport.ConsumerConfig{
			StreamPrefix:            m.config.RedisClient.KB().StreamPrefix(), // Namespace-aware prefix (e.g., "ha:relays")
			SupplierOperatorAddress: operatorAddr,
			ConsumerGroup:           m.config.RedisClient.KB().ConsumerGroup(), // Namespace-aware group (e.g., "ha-miners")
			ConsumerName:            m.config.ConsumerName,
			BatchSize:               int64(m.config.BatchSize),                // Use config value (default: 1000)
			ClaimIdleTimeout:        m.config.ClaimIdleTimeout.Milliseconds(), // From config (default: 60000ms)
			// Note: blocks for one block interval per read - hardcoded in consumer
		},
	)
	if err == nil {
		consumer.SetStoreHealth(m.config.StoreHealth)
	}
	if err != nil {
		cancelFn()
		return fmt.Errorf("failed to create consumer for %s: %w", operatorAddr, err)
	}

	// Create SMST manager for building session trees (Redis-backed for HA)
	smstManager := NewRedisSMSTManager(
		m.logger,
		m.config.RedisClient,
		RedisSMSTManagerConfig{
			SupplierAddress:            operatorAddr,
			CacheTTL:                   m.config.CacheTTL,
			LiveRootCheckpointInterval: m.config.SMSTLiveRootCheckpointInterval,
			ColdCompactionPool:         m.coldCompactionPool,
			RebuildAdmission:           m.rebuildAdmission,
		},
	)

	// Built before the lifecycle manager, whose claim transition flushes it.
	relayBatch := newRelayBatch(
		m.logger, m.config.RedisClient, operatorAddr,
		sessionStore, m.deduplicator, smstManager, sessionCoordinator, consumer,
	)

	// SMST trees are lazy-loaded from Redis on-demand:
	//   - UpdateTree (relay path) → GetOrCreateTree creates/loads tree from Redis
	//   - ProveClosest / GetTreeRoot → loadTreeFromRedis for HA failover recovery
	//     (when this miner becomes leader AFTER the original leader already flushed
	//      the SMST and moved to proof submission)
	// The SMT library itself lazy-loads nodes from Redis as needed.
	m.logger.Debug().
		Str(logging.FieldSupplier, operatorAddr).
		Msg("SMST manager ready (lazy-loads trees on first operation: relay, proof, or recovery)")

	// Create supplier client for claim/proof submission
	var supplierClient *tx.HASupplierClient
	var lifecycleCallback *LifecycleCallback
	var lifecycleManager *SessionLifecycleManager

	// Only create lifecycle components if TxClient and BlockClient are provided
	if m.config.TxClient != nil && m.config.BlockClient != nil && m.config.SharedClient != nil {
		supplierClient = tx.NewHASupplierClient(
			m.config.TxClient,
			operatorAddr,
			m.logger,
		)

		// Create lifecycle callback for claim/proof submission
		lifecycleCallbackConfig := DefaultLifecycleCallbackConfig()
		lifecycleCallbackConfig.SupplierAddress = operatorAddr
		lifecycleCallbackConfig.BlockTimeSeconds = m.config.BlockTimeSeconds
		lifecycleCallbackConfig.DisablePreProofClaimVerification = m.config.DisablePreProofClaimVerification
		lifecycleCallback = NewLifecycleCallback(
			m.logger,
			supplierClient,
			m.config.SharedClient,
			m.config.BlockClient,
			m.config.SessionClient,
			smstManager,
			sessionCoordinator,
			m.config.ProofChecker, // May be nil - if so, proofs are always submitted (legacy)
			lifecycleCallbackConfig,
		)

		// Teach the coordinator when a claim window has already closed, so a
		// relay for an unknown session past its window is dropped instead of
		// creating a session nothing can ever claim. Wired only here, where the
		// block and shared-params clients are known to exist; without them the
		// predicate stays nil and the check answers false.
		sessionCoordinator.SetClaimWindowClosedFn(func(sessionEndHeight int64) bool {
			return m.claimWindowClosedAt(supplierCtx, sessionEndHeight)
		})

		if m.config.ServiceClient != nil {
			lifecycleCallback.SetServiceClient(m.config.ServiceClient)
		}
		// Pre-proof GetClaim guard (WS-A): skips proof submission for sessions
		// whose claim is not on-chain, preventing FailedPrecondition retry
		// storms and wasted gas.
		if m.config.ProofQueryClient != nil {
			lifecycleCallback.SetProofQueryClient(m.config.ProofQueryClient)
		}

		// Wire the SMST as the claimed-root rehydration source for the
		// proof-requirement check. Under HA failover the snapshot's
		// ClaimedRootHash can be nil (OnSessionClaimed's Redis write
		// failed and was only logged at Warn); without this provider
		// IsProofRequired would fabricate a proof from a nil root.
		if m.config.ProofChecker != nil {
			m.config.ProofChecker.SetClaimedRootProvider(smstManager)
		}

		// Wire the process-wide submission tracker + rebroadcast store (built
		// once; see ensureSharedTrackers and the field docs). The submission
		// tracker records tx hashes / success / errors / timing; the rebroadcast
		// store persists each built claim/proof message so the block-driven
		// InclusionReconciler can verify on-chain inclusion (x/proof module
		// state — tx_index=null safe) and re-broadcast a still-missing
		// claim/proof into its still-open window (fix for silent
		// CLAIM_MISSING/PROOF_MISSING forfeits).
		m.ensureSharedTrackers()
		lifecycleCallback.SetSubmissionTracker(m.sharedSubmissionTracker)
		if m.rebroadcastStore != nil {
			lifecycleCallback.SetRebroadcastStore(m.rebroadcastStore)
		}

		// Wire the deduplicator so terminal session events
		// (OnSessionProved/OnClaimSkipped/OnProbabilisticProved) purge the
		// per-session dedup set (ha:miner:dedup:session:{sessionID}) via
		// CleanupSession. Without this the callback's lc.deduplicator stays nil
		// and the cleanup is skipped, leaving dedup state to expire only by Redis
		// TTL — unbounded interim growth at 1000+ RPS. m.deduplicator is built in
		// the constructor (always non-nil here); the callback nil-guards anyway.
		lifecycleCallback.SetDeduplicator(m.deduplicator)

		// Wire build pool for bounded parallel claim/proof building
		// Uses master pool to avoid unbounded goroutine spawning
		lifecycleCallback.SetBuildPool(m.config.WorkerPool)

		// Create lifecycle manager for monitoring sessions and triggering claim/proof
		lifecycleConfig := m.config.SessionLifecycleConfig
		lifecycleConfig.SupplierAddress = operatorAddr // Override for this supplier
		lifecycleManager = NewSessionLifecycleManager(
			m.logger,
			sessionStore,
			m.config.SharedClient,
			m.config.BlockClient,
			lifecycleCallback,
			lifecycleConfig,
			m.config.WorkerPool, // Pass master worker pool for transition subpool
		)

		// Wire meter cleanup publisher for notifying relayers when sessions leave active state.
		// This publishes cleanup signals to ha:meter:cleanup so relayers can decrement their
		// active sessions metric and clear session meter data.
		meterCleanupChannel := m.config.RedisClient.KB().MeterCleanupChannel()
		redisClient := m.config.RedisClient
		meterCleanupPublisher := NewRedisMeterCleanupPublisher(
			m.logger,
			func(ctx context.Context, channel string, message interface{}) error {
				return redisClient.Publish(ctx, channel, message).Err()
			},
			meterCleanupChannel,
		)
		lifecycleManager.SetMeterCleanupPublisher(meterCleanupPublisher)

		// A session about to be claimed gets its batched relays counted first,
		// so the relay_count the claim transition refreshes includes them.
		// Set before Start: the transitions read it on their own goroutines.
		if relayBatch != nil {
			lifecycleManager.SetPendingRelayFlusher(relayBatch.FlushSessions)
		}
	} else {
		m.logger.Warn().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("lifecycle management disabled - TxClient, BlockClient, or SharedClient not configured")
	}

	state := &SupplierState{
		OperatorAddr:       operatorAddr,
		Consumer:           consumer,
		SessionStore:       sessionStore,
		SessionCoordinator: sessionCoordinator,
		SMSTManager:        smstManager,
		relayBatch:         relayBatch,
		LifecycleManager:   lifecycleManager,
		LifecycleCallback:  lifecycleCallback,
		SupplierClient:     supplierClient,
		cancelFn:           cancelFn,
	}
	state.StoreStatus(SupplierStatusActive)
	m.wireRebuildAdmission(operatorAddr, consumer, lifecycleManager)

	if lifecycleManager != nil {
		// Conditional flush delay -- wired BEFORE Start(), not alongside
		// the other lifecycleManager.Set* calls above: it needs `state`,
		// which this function only builds once every other supplier
		// component is ready. Wiring it AFTER Start() would race a session
		// this instance loads already in SessionStateClaiming (an HA
		// handoff, or a restart): its first transition check can run the
		// moment Start's background loop begins, on another goroutine,
		// reading these fields while this one is still writing them.
		lifecycleManager.SetMaxNonReclaimHandledMsgIDLookup(state.loadMaxNonReclaimHandledMsgID)
		if consumer != nil {
			lifecycleManager.SetLastGeneratedMsgIDLookup(func(ctx context.Context) (streamMsgID, bool, error) {
				idStr, lastGenErr := consumer.LastGeneratedID(ctx)
				if lastGenErr != nil {
					return streamMsgID{}, false, lastGenErr
				}
				if idStr == "" || idStr == "0-0" {
					return streamMsgID{}, false, nil
				}
				id, parseErr := parseStreamMsgID(idStr)
				if parseErr != nil {
					return streamMsgID{}, false, parseErr
				}
				return id, true, nil
			})
		}

		// Start lifecycle manager
		if startErr := lifecycleManager.Start(supplierCtx); startErr != nil {
			m.logger.Warn().
				Err(startErr).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to start lifecycle manager, continuing without lifecycle management")
			lifecycleManager = nil
			state.LifecycleManager = nil
		} else {
			// Wire up callback so session coordinator notifies lifecycle manager of new sessions
			// This is critical for tracking sessions created after startup
			lm := lifecycleManager // capture for closure
			sessionCoordinator.SetOnSessionCreatedCallback(func(ctx context.Context, snapshot *SessionSnapshot) error {
				return lm.TrackSession(ctx, snapshot)
			})

			// Wire up terminal state callback so in-memory state is updated atomically with Redis
			// This prevents session leak where terminal sessions stay in activeSessions
			sessionCoordinator.SetOnSessionTerminalCallback(func(sessionID string, state SessionState) {
				lm.RemoveSession(sessionID)
			})

			m.logger.Info().
				Str(logging.FieldSupplier, operatorAddr).
				Msg("session_lifecycle_callbacks_wired: creation and terminal callbacks registered for atomic state updates")
		}
	}

	// Atomic insert. If another goroutine raced us and stored its own
	// state, drop ours (and tear down the resources we just constructed)
	// so we don't leak a consumer goroutine and Redis connection.
	if _, loaded := m.suppliers.LoadOrStore(operatorAddr, state); loaded {
		cancelFn()
		if lifecycleManager != nil {
			_ = lifecycleManager.Close()
		}
		if smstManager != nil {
			_ = smstManager.Close()
		}
		_ = consumer.Close()
		_ = sessionCoordinator.Close()
		_ = sessionStore.Close()
		return nil
	}

	// Start consuming in background
	state.wg.Add(1)
	go m.consumeForSupplier(supplierCtx, state)

	// Publish to registry
	if m.registry != nil {
		if err := m.registry.PublishSupplierUpdate(ctx, SupplierUpdateActionAdd, operatorAddr, nil); err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to publish supplier add to registry")
		}
	}

	// Resolve supplier services + owner and publish the resulting state
	// to the shared cache. Extracted into a helper so we can unit-test the
	// "don't overwrite with empty services on chain query error" guard rail
	// without spinning up the full addSupplierWithData pipeline.
	_, services, endpoints := m.resolveAndPublishSupplierState(ctx, operatorAddr, prewarmedData)
	state.stakeView.Store(&supplierStakeView{Services: services, StakedEndpoints: endpoints})

	supplierManagerSuppliersActive.Inc()

	m.logger.Debug().
		Str(logging.FieldSupplier, operatorAddr).
		Msg("supplier added and consuming")

	return nil
}

// wireRebuildAdmission holds the supplier's consumer while a proof waits for
// memory, and lets its claim's flush delay release that hold. Called before
// the consumer and the lifecycle manager start.
func (m *SupplierManager) wireRebuildAdmission(operatorAddr string, consumer *redistransport.StreamsConsumer, lifecycleManager *SessionLifecycleManager) {
	if m.rebuildAdmission == nil {
		return
	}
	admission := m.rebuildAdmission
	consumer.SetIngestionPause(admission.IngestionPause(operatorAddr))
	if lifecycleManager != nil {
		lifecycleManager.SetClaimFlushWaiting(func() func() { return admission.claimFlushWaiting(operatorAddr) })
	}
}

// supplierDataSource describes how we obtained (or failed to obtain)
// supplier data. It gates whether we may overwrite the shared cache.
type supplierDataSource int

const (
	supplierDataSourceNoQueryClient supplierDataSource = iota
	supplierDataSourcePrewarmed
	supplierDataSourceChainOK
	supplierDataSourceChainNotFound
	supplierDataSourceChainError
	supplierDataSourceUnreliableBoot
)

// resolveAndPublishSupplierState obtains a supplier's services + owner
// (from prewarmed data if available, otherwise the chain) and publishes
// the resulting state to the shared supplier cache.
//
// Critical invariant: we must NEVER persist {Staked:true, Services:[]}
// on the chain-query-error path. A single fullnode glitch during startup
// would otherwise leave relayers rejecting every relay for that supplier
// until a full restart, because warmup reloads the poisoned cache entry
// on every boot (see fix(miner): don't persist empty supplier services
// on failed chain query).
//
// Returns the resolved ownerAddr, services and per-transport stake view so
// callers can populate per-supplier state; empty values are returned on any
// failure path. The endpoints are returned, and not only published, because the
// drain write has to republish them without reading the store back.
func (m *SupplierManager) resolveAndPublishSupplierState(
	ctx context.Context,
	operatorAddr string,
	prewarmedData *SupplierWarmupData,
) (ownerAddr string, services []string, endpoints []cache.StakedEndpoint) {
	source := supplierDataSourceNoQueryClient

	switch {
	case prewarmedData != nil:
		ownerAddr = prewarmedData.OwnerAddress
		services = prewarmedData.Services
		endpoints = prewarmedData.StakedEndpoints
		source = supplierDataSourcePrewarmed
		m.logger.Debug().
			Str(logging.FieldSupplier, operatorAddr).
			Int("services", len(services)).
			Msg("using pre-warmed supplier data")

	case m.config.SupplierQueryClient != nil:
		supplier, queryErr := m.config.SupplierQueryClient.GetSupplier(ctx, operatorAddr)
		if queryErr != nil {
			if query.IsEntityNotFound(queryErr) {
				source = supplierDataSourceChainNotFound
				m.logger.Debug().
					Str(logging.FieldSupplier, operatorAddr).
					Msg("supplier not staked on-chain yet (pre-loaded key)")
			} else {
				source = supplierDataSourceChainError
				m.logger.Warn().
					Err(queryErr).
					Str(logging.FieldSupplier, operatorAddr).
					Msg("failed to query supplier from blockchain")
			}
		} else {
			ownerAddr = supplier.OwnerAddress
			// Height-aware resolve with boot fallback — this was the third
			// writer path still publishing the raw denormalized snapshot at
			// ANY height (review finding): with a known height it must
			// respect activation/deactivation heights like every other
			// writer, and an unreliable boot result must not be persisted.
			var reliable bool
			services, endpoints, reliable = m.resolveSupplierServices(ctx, &supplier, operatorAddr)
			if reliable {
				source = supplierDataSourceChainOK
			} else {
				source = supplierDataSourceUnreliableBoot
			}
			m.logger.Debug().
				Str(logging.FieldSupplier, operatorAddr).
				Str("services", fmt.Sprintf("%v", services)).
				Msg("queried supplier services from blockchain")
		}
	}

	if m.config.SupplierCache == nil {
		return ownerAddr, services, endpoints
	}

	switch source {
	case supplierDataSourcePrewarmed, supplierDataSourceChainOK:
		supplierState := &cache.SupplierState{
			Status:          cache.SupplierStatusActive,
			Staked:          true,
			OperatorAddress: operatorAddr,
			OwnerAddress:    ownerAddr,
			Services:        services,
			StakedEndpoints: endpoints,
			UpdatedBy:       m.config.MinerID,
		}
		if cacheErr := m.config.SupplierCache.SetSupplierState(ctx, supplierState); cacheErr != nil {
			m.logger.Warn().
				Err(cacheErr).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to publish supplier state to cache")
		} else {
			m.logger.Debug().
				Str(logging.FieldSupplier, operatorAddr).
				Str("services", fmt.Sprintf("%v", services)).
				Msg("published supplier state to cache")
		}

	case supplierDataSourceChainNotFound:
		// Supplier legitimately unstaked — mark as such so any stale
		// Staked:true entry gets corrected and relayers can skip it.
		supplierState := &cache.SupplierState{
			Status:          cache.SupplierStatusNotStaked,
			Staked:          false,
			OperatorAddress: operatorAddr,
			OwnerAddress:    ownerAddr,
			Services:        nil,
			UpdatedBy:       m.config.MinerID,
		}
		if cacheErr := m.config.SupplierCache.SetSupplierState(ctx, supplierState); cacheErr != nil {
			m.logger.Warn().
				Err(cacheErr).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to publish not-staked supplier state to cache")
		} else {
			m.logger.Debug().
				Str(logging.FieldSupplier, operatorAddr).
				Msg("published not-staked supplier state to cache")
		}

	case supplierDataSourceChainError:
		// Do NOT overwrite an existing cache entry with empty services.
		// Let the next refresh on a healthy fullnode repopulate it.
		supplierCacheWriteSkipped.WithLabelValues("chain_query_error").Inc()
		m.logger.Warn().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("skipping cache update: chain query failed, preserving previous state")

	case supplierDataSourceUnreliableBoot:
		// No observed height AND an empty snapshot: nothing trustworthy to
		// write. Preserve the previous entry; the first reconcile with a
		// real height persists the truth.
		supplierCacheWriteSkipped.WithLabelValues("unreliable_boot_snapshot").Inc()
		m.logger.Debug().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("skipping cache update: no block height yet and empty service snapshot, preserving previous state")

	case supplierDataSourceNoQueryClient:
		// No query client and no prewarmed data — nothing to publish.
	}

	return ownerAddr, services, endpoints
}

// shutdownDrainWindow bounds how long a supplier's graceful shutdown spends
// finishing relays already sitting in its delivery buffer.
//
// It is deliberately well under Kubernetes' default 30s termination grace period,
// which the whole miner shutdown has to fit inside -- and this drain runs per
// supplier, so the budget is shared. It is NOT sized to empty a full 5000-slot
// buffer: at Redis round-trip speed that would take far longer than any grace
// period allows. Whatever does not fit stays pending and the reclaim recovers it,
// so the window trades completeness for a bounded, predictable shutdown.
//
// A var, not a const, only so a test can shrink it: nothing in production writes it.
var shutdownDrainWindow = 5 * time.Second

// consumeLoopPanicBudget is the consume-loop panic at which a supplier is let
// go for another instance to take: the panics before it are recovered and the
// loop runs again on the same delivery channel, and at this one it stops, since
// a loop that keeps panicking here would keep its lease and consume nothing.
// The count is not replenished: a supplier that panics this often has a
// defect, and restarting it forever would hide it.
const consumeLoopPanicBudget = 3

// consumeForSupplier runs the consume loop for a single supplier with immediate ACK.
// Each message is ACK'd immediately after successful processing to prevent race conditions
// with the reclaim taking messages that were already processed but not yet ACK'd.
//
// A panic in the loop must not end this goroutine: with it gone, the supplier
// stays in the map and its lease keeps being renewed while nothing reads its
// stream, and no other instance can take it -- until a restart. A relay's
// processing and a batch's flush recover their own panics (handleStreamMessage,
// relayBatch.flushSession); runConsumeLoop recovers whatever reaches the loop
// itself, and this runs it again until the consumeLoopPanicBudget-th panic.
func (m *SupplierManager) consumeForSupplier(ctx context.Context, state *SupplierState) {
	defer state.wg.Done()

	msgChan := state.Consumer.Consume(ctx)
	for panics := 1; ; panics++ {
		if !m.runConsumeLoop(ctx, state, msgChan) {
			return
		}
		// The panic may have left the batch half-built: it goes back before
		// the loop runs again. A panic while it goes back ends the restarts.
		ok := m.releaseBatchAfterPanic(ctx, state)
		if m.afterReleaseAfterPanicHook != nil {
			m.afterReleaseAfterPanicHook()
		}
		if !ok || panics >= consumeLoopPanicBudget {
			m.letGoAfterPanics(ctx, state)
			return
		}
	}
}

// unloadEndedSessionTrees drops from memory the trees of this supplier's
// sessions past their grace period and before their claim window, keeping them
// in Redis. At a claim height
// under load, the relays those sessions held put their trees at about half the
// live heap (estimated from relays consumed, not profiled per session), twelve
// blocks after their last relay. It runs right after the flush, on the consume loop's
// goroutine, so the relays of those sessions are checkpointed and acknowledged
// by then.
//
// From the claim window on it leaves them: the claim leaves its tree unloaded
// (FlushTree), and a proof may hold a tree's lock while it waits for memory,
// which would hold this loop, and the supplier's relays, behind it.
func (m *SupplierManager) unloadEndedSessionTrees(ctx context.Context, state *SupplierState) {
	if state.LifecycleManager == nil || state.SMSTManager == nil || m.config.BlockClient == nil || m.config.SharedClient == nil {
		return
	}
	block := m.config.BlockClient.LastBlock(ctx)
	if block == nil {
		return
	}
	height := block.Height()
	type unloadedAtEnd struct {
		graceEnd, claimOpen         int64
		trees, leaves, computeUnits uint64
	}
	var unloaded map[int64]*unloadedAtEnd
	state.LifecycleManager.activeSessions.Range(func(sessionID string, session *SessionSnapshot) bool {
		if session.SessionEndHeight <= 0 || session.SessionEndHeight >= height {
			return true
		}
		params, err := m.config.SharedClient.GetParamsAtHeight(ctx, session.SessionEndHeight)
		if err != nil || params == nil {
			return true
		}
		graceEnd := sharedtypes.GetSessionGracePeriodEndHeight(params, session.SessionEndHeight)
		claimOpen := sharedtypes.GetClaimWindowOpenHeight(params, session.SessionEndHeight)
		if height <= graceEnd || height >= claimOpen {
			return true
		}
		root, err := state.SMSTManager.UnloadTree(ctx, sessionID)
		if err != nil {
			m.logger.Debug().Err(err).
				Str(logging.FieldSupplier, state.OperatorAddr).
				Str(logging.FieldSessionID, sessionID).
				Msg("failed to unload an ended session's tree: it stays in memory")
			return true
		}
		if root == nil {
			return true
		}
		// The count and the sum are in the root's bytes: nothing is read.
		leaves, _ := smt.MerkleSumRoot(root).Count()
		computeUnits, _ := smt.MerkleSumRoot(root).Sum()
		m.logger.Debug().
			Str(logging.FieldSupplier, state.OperatorAddr).
			Str(logging.FieldSessionID, sessionID).
			Int64("height", height).
			Int64("session_end_height", session.SessionEndHeight).
			Uint64("leaves", leaves).
			Uint64("compute_units", computeUnits).
			Msg("unloaded an ended session's tree from memory")
		if unloaded == nil {
			unloaded = make(map[int64]*unloadedAtEnd)
		}
		at := unloaded[session.SessionEndHeight]
		if at == nil {
			at = &unloadedAtEnd{graceEnd: graceEnd, claimOpen: claimOpen}
			unloaded[session.SessionEndHeight] = at
		}
		at.trees++
		at.leaves += leaves
		at.computeUnits += computeUnits
		return true
	})
	for sessionEnd, at := range unloaded {
		m.logger.Info().
			Str(logging.FieldSupplier, state.OperatorAddr).
			Int64("height", height).
			Int64("session_end_height", sessionEnd).
			Int64("grace_period_end_height", at.graceEnd).
			Int64("claim_window_open_height", at.claimOpen).
			Uint64("trees", at.trees).
			Uint64("leaves", at.leaves).
			Uint64("compute_units", at.computeUnits).
			Msg("unloaded ended sessions' trees from memory")
	}
}

// flushPhase is how long a supplier's first flush waits: its address hashed
// into [0, interval), so suppliers started together flush apart.
func flushPhase(supplier string, interval time.Duration) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(supplier))
	return time.Duration(h.Sum64() % uint64(interval))
}

func (m *SupplierManager) newFirstFlushTimer(d time.Duration) (<-chan time.Time, func() bool) {
	if m.firstFlushTimer != nil {
		return m.firstFlushTimer(d)
	}
	timer := time.NewTimer(d)
	return timer.C, timer.Stop
}

// runConsumeLoop consumes until the delivery channel closes or ctx ends, and
// reports whether it stopped on a recovered panic instead.
func (m *SupplierManager) runConsumeLoop(
	ctx context.Context,
	state *SupplierState,
	msgChan <-chan transport.StreamMessage,
) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			logging.PanicRecoveriesTotal.WithLabelValues("supplier_consume_loop").Inc()
			m.logger.Error().
				Str(logging.FieldSupplier, state.OperatorAddr).
				Str("panic_value", fmt.Sprintf("%v", r)).
				Str("stack_trace", string(debug.Stack())).
				Msg("PANIC RECOVERED in the consume loop -- running it again")
		}
	}()

	// The flush tick runs on this goroutine, between deliveries, so a flush
	// never races the relays being added: the lifecycle's claim transition is
	// the only other caller, and the batch serialises it.
	//
	// Every supplier's loop starts at once, and a flush commits every node the
	// relays since the last one dirtied: with the tickers in phase, all the
	// suppliers' commits allocated together, hundreds of MiB within a second.
	// The first flush waits a phase of the interval taken from the supplier's
	// address, and the ticker starts from there.
	var flushTick <-chan time.Time
	var flushTicker *time.Ticker
	interval := m.config.RelayBatchFlushInterval
	if state.relayBatch != nil && interval > 0 {
		fire, stop := m.newFirstFlushTimer(flushPhase(state.OperatorAddr, interval))
		defer stop()
		flushTick = fire
		defer func() {
			if flushTicker != nil {
				flushTicker.Stop()
			}
		}()
	}

	for {
		select {
		case msg, ok := <-msgChan:
			if !ok {
				// Channel closed, exit
				m.releaseRelayBatchOnExit(ctx, state)
				return false
			}
			state.Consumer.MarkDelivered(msg)
			m.handleStreamMessage(ctx, state, msg)
			// The byte trigger: a supplier of big relays flushes when its
			// leaves took relayBatchFlushBytes, instead of holding a whole
			// interval of them. Only the flush -- ending sessions are left to
			// the tick, which asks the chain about each one.
			if state.relayBatch != nil && state.SMSTManager != nil &&
				state.SMSTManager.LeafBytesSinceFlush() >= relayBatchFlushBytes {
				RecordRelayBatchFlush(state.OperatorAddr, relayBatchFlushBytesTrigger)
				state.relayBatch.FlushAll(ctx)
				if m.consumeLoopByteFlushedHook != nil {
					m.consumeLoopByteFlushedHook()
				}
			}

		case <-flushTick:
			if flushTicker == nil {
				flushTicker = time.NewTicker(interval)
				flushTick = flushTicker.C
			}
			if m.consumeLoopFlushHook != nil {
				m.consumeLoopFlushHook()
			}
			RecordRelayBatchFlush(state.OperatorAddr, relayBatchFlushTime)
			state.relayBatch.FlushAll(ctx)
			m.unloadEndedSessionTrees(ctx, state)

		case <-ctx.Done():
			// The batch goes back first, while the exit budget is whole.
			m.releaseRelayBatchOnExit(ctx, state)
			// Do NOT abandon what is already in the delivery buffer. Those
			// relays were handed over by XREADGROUP, so they are in this
			// consumer's pending list under a name that embeds the pid and
			// will not exist after a restart.
			m.drainDeliveryBuffer(ctx, state, msgChan)
			return false
		}
	}
}

// releaseBatchAfterPanic hands the supplier's relay batch back after a
// recovered consume-loop panic, and reports whether it got through without
// panicking itself.
func (m *SupplierManager) releaseBatchAfterPanic(ctx context.Context, state *SupplierState) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			logging.PanicRecoveriesTotal.WithLabelValues("supplier_consume_loop").Inc()
			m.logger.Error().
				Str(logging.FieldSupplier, state.OperatorAddr).
				Str("panic_value", fmt.Sprintf("%v", r)).
				Str("stack_trace", string(debug.Stack())).
				Msg("PANIC RECOVERED handing the relay batch back after a consume-loop panic")
		}
	}()
	m.releaseRelayBatchOnExit(ctx, state)
	return true
}

// letGoAfterPanics lets a supplier whose consume loop keeps panicking go, for
// another instance to take: through the claimer, which keeps the lease until
// the drain ends, or, with no claimer, straight to the drain. It is called on
// the loop's own goroutine and does not wait: onSupplierReleased cancels the
// supplier and runs the teardown on a goroutine of its own, which waits for
// this one to return. removeSupplier would wait for it here, on itself.
func (m *SupplierManager) letGoAfterPanics(ctx context.Context, state *SupplierState) {
	m.logger.Error().
		Str(logging.FieldSupplier, state.OperatorAddr).
		Msg("the consume loop keeps panicking; letting the supplier go for another instance to take")
	var err error
	if m.claimer != nil {
		err = m.claimer.Release(ctx, state.OperatorAddr, triggerConsumeLoopPanicked)
	} else {
		err = m.onSupplierReleased(ctx, state.OperatorAddr, triggerConsumeLoopPanicked)
	}
	if err != nil {
		m.logger.Error().
			Err(err).
			Str(logging.FieldSupplier, state.OperatorAddr).
			Msg("could not let go of a supplier whose consume loop keeps panicking")
	}
}

// releaseRelayBatchOnExit hands the supplier's relay batch back to the group,
// unflushed, as its consume loop ends -- shutdown, rebalance or key removal --
// which is before removeSupplier and Close close the consumer, so the releases
// can still be sent. It releases rather than flushes because flushing every
// held session does not fit the exit window (Jorge, 2026-09-10); the relays are
// already in the tree and were never marked, so the next consumer counts them
// once.
//
// The exception is a removed key, decided the way drainDeliveryBuffer decides
// it: nobody in this fleet can claim those relays, so the batch is acknowledged
// and counted as dropped for want of a key instead of left pending forever.
//
// Like drainDeliveryBuffer it detaches from the supplier's context, which is
// already cancelled, and bounds itself by shutdownDrainWindow.
func (m *SupplierManager) releaseRelayBatchOnExit(ctx context.Context, state *SupplierState) {
	if state.relayBatch == nil {
		return
	}
	exitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownDrainWindow)
	defer cancel()
	if drainReason(state.drainReason.Load()) == drainKeyRemoved {
		state.relayBatch.AckAllAsLost(exitCtx)
		return
	}
	state.relayBatch.ReleaseAll(exitCtx)
}

// handBackOnExit settles one entry a stopping supplier holds and will not
// process, and reports whether Redis took it. With the key removed nobody in
// this fleet can build an SMST, a claim or a proof for the relay, so it is
// acknowledged deliberately and counted as LOSS: left pending, the next
// consumer would only rediscover the same dead end. Otherwise someone else can
// still finish it, so it is RELEASED, never acknowledged -- acknowledging
// deletes it from the stream and takes it out of reach of the reclaim. Either
// way the pooled message goes back.
func (m *SupplierManager) handBackOnExit(ctx context.Context, state *SupplierState, msg transport.StreamMessage) bool {
	defer transport.ReleaseMinedRelayMessage(msg.Message)
	if drainReason(state.drainReason.Load()) == drainKeyRemoved {
		if err := state.Consumer.AckMessage(ctx, msg); err != nil {
			return false
		}
		RecordRelayDroppedNoKey(state.OperatorAddr, msg.Message.ServiceId)
		return true
	}
	if err := state.Consumer.ReleaseMessage(ctx, msg); err != nil {
		return false
	}
	RecordShutdownDrainedRelay(state.OperatorAddr)
	return true
}

// releaseOwnPendingOnExit stops the supplier's consumer and settles, with
// handBackOnExit, every entry still pending under its name: what the consume
// loop's exit did not reach -- the relay being processed when the supplier was
// cancelled, if settling it failed on that context; the rest of a read batch
// or of a reclaim page the consumer's producers were handing over when they
// stopped; whatever a delivery-buffer drain cut short by its window left in
// the channel. The name belongs to this process, so an entry left there waits
// until the process takes the supplier again or restarts. Stopped first, the
// producers add nothing after the pass. On a context of its own, bounded by
// shutdownDrainWindow like the rest of the exit.
func (m *SupplierManager) releaseOwnPendingOnExit(state *SupplierState) {
	state.Consumer.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainWindow)
	defer cancel()
	settled, failed := 0, 0
	err := state.Consumer.EachOwnPending(ctx, func(msg transport.StreamMessage) {
		if m.handBackOnExit(ctx, state, msg) {
			settled++
		} else {
			failed++
		}
	})
	RecordShutdownAbandonedRelays(state.OperatorAddr, failed)
	if err != nil || failed > 0 {
		// Once per supplier torn down, not per relay.
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSupplier, state.OperatorAddr).
			Int("settled", settled).
			Int("failed", failed).
			Msg("could not settle every entry left under this consumer's name; they stay pending until this process takes the supplier again or restarts")
		return
	}
	if settled > 0 {
		m.logger.Info().
			Str(logging.FieldSupplier, state.OperatorAddr).
			Int("settled", settled).
			Msg("settled the entries left under this consumer's name")
	}
}

// handleStreamMessage runs one delivered relay to completion: process, release
// the pooled message, and acknowledge on success. Returns whether the message
// was actually acknowledged -- true when the relay was processed, and also when
// it was LOST to a recovered panic (deterministic, so it is acked and counted
// rather than retried forever); false on any other processing failure, where the
// entry is handed back for a later delivery, for a relay handed to the relay
// batch (acknowledged when the batch flushes, not by this call), OR on an
// AckMessage error (Redis hiccup, connection reset), which a caller MUST check
// before treating the relay as done rather than assuming success from
// "handleStreamMessage returned" (review 2026-08-21: the shutdown drain's
// metric used to do exactly that).
//
// It takes its own ctx rather than closing over the supplier's, because the
// graceful-shutdown drain calls it with a context that is deliberately still
// alive after the supplier's has been cancelled.
func (m *SupplierManager) handleStreamMessage(
	ctx context.Context,
	state *SupplierState,
	msg transport.StreamMessage,
) (acked bool) {
	// Advance the live-delivery watermark on every exit path (defer, not "at
	// the end") -- this function has several early returns below, and a
	// watermark only updated on the happy path would never move on a
	// transient error, permanently understating what this supplier already
	// handled. Reclaims and self-pending redeliveries (msg.IsReclaim) never
	// touch it: see streamMsgID / recordNonReclaimHandled.
	if !msg.IsReclaim {
		if id, parseErr := parseStreamMsgID(msg.ID); parseErr == nil {
			defer state.recordNonReclaimHandled(id)
		} else {
			m.logger.Warn().
				Err(parseErr).
				Str(logging.FieldSupplier, state.OperatorAddr).
				Str("message_id", msg.ID).
				Msg("malformed stream message id, flush watermark not advanced for this message")
		}
	}

	// Track relay consumed from Redis Stream (relayer → miner)
	RecordRelayConsumedFromStream(state.OperatorAddr, msg.Message.ServiceId)

	// When draining, we continue processing existing messages
	// but log that we're in drain mode for visibility.
	// LoadStatus is lock-free — removeSupplier publishes the
	// draining flag via StoreStatus and this read stays off
	// any shared mutex on the hot path.
	if state.LoadStatus() == SupplierStatusDraining {
		m.logger.Debug().
			Str(logging.FieldSupplier, state.OperatorAddr).
			Msg("processing relay during drain")
	}

	// Per-relay panic guard: if anything below (including code
	// paths the runSMSTSafely boundary does NOT cover — pool
	// release, deduplicator, session_store access) panics, we
	// log, increment a metric, and move on to the next message
	// rather than dying and leaving the supplier with no
	// consumer. This is the difference between "a single relay
	// is lost" (acceptable) and "the supplier stops earning
	// until a restart" (the Anaski incident shape).
	serviceID := msg.Message.ServiceId
	sessionID := msg.Message.SessionId
	var processErr error
	batched := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				logging.PanicRecoveriesTotal.WithLabelValues("supplier_consume_relay").Inc()
				m.logger.Error().
					Str(logging.FieldSupplier, state.OperatorAddr).
					Str("session_id", sessionID).
					Str("panic_value", fmt.Sprintf("%v", r)).
					Str("stack_trace", string(debug.Stack())).
					Msg("PANIC RECOVERED during relay processing — relay dropped, consumer continuing")
				processErr = fmt.Errorf("%w: %v", ErrRelayPanicRecovered, r)
			}
		}()
		if m.onRelay != nil {
			startTime := time.Now()
			processErr = m.onRelay(ctx, state.OperatorAddr, &msg)
			// Processed, and its acknowledgement belongs to the relay batch:
			// a success, not a failure to hand back.
			if errors.Is(processErr, ErrRelayBatched) {
				batched = true
				processErr = nil
			}
			status := "success"
			if processErr != nil {
				status = "error"
			}
			RecordRelayProcessingLatency(state.OperatorAddr, serviceID, status, time.Since(startTime).Seconds())
		}

		// Pool release must run inside the recover scope so that
		// a panic mid-processing still returns the borrowed
		// MinedRelayMessage to the pool — otherwise the pool
		// leaks one slot per panic and eventually starves.
		transport.ReleaseMinedRelayMessage(msg.Message)
		msg.Message = nil
	}()

	if processErr != nil {
		// A recovered panic is ACKed rather than handed back: a panic that is a
		// pure function of the relay bytes repeats on every redelivery, so
		// returning the entry only spends the failure on a loop. The relay was
		// already served -- the backend did the work and the client has its
		// answer -- so it is lost work, and it is counted as such. The loud
		// Error log with the stack lives at the recover() above; this is the
		// accounting half, which did not exist.
		//
		// NOT VERIFIED that every panic arriving here is deterministic, and the
		// guard above says why: it deliberately also covers paths that are
		// state-dependent (pool release, deduplicator, session_store access). One
		// of those failing while a component is torn down could panic for one
		// in-flight relay on this replica and not on another. AckMessage is
		// XAckDel with DELREF, so this DELETES the entry and no replica can
		// rescue it afterwards.
		//
		// Losing that relay rather than risking a loop is an owner decision, not
		// a property of the code (Jorge, 2026-09-02: "the panic must
		// unfortunately be lost... or it will stay alive without billing"). It
		// is written here so it is not re-litigated and not read as something
		// that was measured.
		if errors.Is(processErr, ErrRelayPanicRecovered) {
			RecordRelayLostToPanic(state.OperatorAddr, serviceID)
			if ackErr := state.Consumer.AckMessage(ctx, msg); ackErr != nil {
				m.logger.Warn().
					Err(ackErr).
					Str(logging.FieldSupplier, state.OperatorAddr).
					Str("session_id", sessionID).
					Msg("failed to ack a relay lost to a panic; it will be redelivered and panic again")
				return false
			}
			return true
		}

		m.logger.Debug().
			Err(processErr).
			Str(logging.FieldSupplier, state.OperatorAddr).
			Str("session_id", sessionID).
			Msg("failed to process relay")

		// Everything else is the transient class -- the worker already ACKs and
		// counts the permanent ones before they reach here (see
		// supplier_worker.go: IsRetryableError / IsPermanentSMSTError). Hand the
		// entry BACK so a later delivery retries it.
		//
		// Not simply "leave it pending": measured 2026-09-02, the reclaim skips
		// any entry still owned by this consumer, so on a single-miner fleet a
		// pending entry is stranded until the process restarts and its name
		// changes. Releasing parks it under a sentinel owner (8.4.6) or unowned
		// (XNACK, 8.8+), which is what makes it visible again.
		if relErr := state.Consumer.ReleaseMessage(ctx, msg); relErr != nil {
			m.logger.Warn().
				Err(relErr).
				Str(logging.FieldSupplier, state.OperatorAddr).
				Str("session_id", sessionID).
				Msg("failed to release a relay after a processing error; it stays pending until this process restarts")
		}
		return false
	}

	if batched {
		return false // not acknowledged by this call: the batch acknowledges it when it flushes
	}

	// ACK immediately after successful processing
	// This prevents race conditions where the reclaim picks up already-processed messages
	if err := state.Consumer.AckMessage(ctx, msg); err != nil {
		m.logger.Debug().
			Err(err).
			Str(logging.FieldSupplier, state.OperatorAddr).
			Str("message_id", msg.ID).
			Msg("failed to acknowledge message")
		return false
	}
	return true
}

// drainDeliveryBuffer finishes the relays already handed to this consumer once
// the supplier's context has been cancelled.
//
// Without it, everything sitting in the delivery channel is dropped on the floor:
// never processed, never acknowledged, and its pooled MinedRelayMessage never
// returned. Those entries stay in the pending list of a consumer name that embeds
// this pid, so nothing here can ever reclaim them again — only another consumer's
// reclaim can, one full claim_idle_timeout later, and only if one is running.
//
// The window is best-effort by design. What does not fit stays pending and IS
// recovered by the reclaim; that is the same path a crash takes. The difference a
// graceful drain makes is that the work completes now instead of after the idle
// timeout, and the pool slots come back. A SIGKILL runs none of this, and nothing
// can be done about that.
//
// The buffer being momentarily empty is NOT the same as the producer being done:
// the transport-layer consumer's read loop shares this same cancelled context, but
// a cancelled context is only observed when its blocking XREADGROUP call elapses
// (transport/redis/consumer.go documents why go-redis sets no read deadline from
// it), and msgChan is only closed after BOTH of its producer goroutines return.
// Racing ahead of that with a non-blocking check used to let this return before a
// message the producer was already mid-delivery on ever arrived — dropped exactly
// the way this function exists to prevent, just on a narrower, timing-dependent
// window (review 2026-08-21). Waiting on drainCtx as a second select case, instead
// of falling through a bare default, is what makes the window it is named after
// actually apply to this case too.
func (m *SupplierManager) drainDeliveryBuffer(
	ctx context.Context,
	state *SupplierState,
	msgChan <-chan transport.StreamMessage,
) {
	// The supplier's context is already cancelled, so every Redis call made with
	// it would fail and be classified as a shutdown-cancel (ACK-and-discard).
	// Detaching from that cancellation is what makes the drain actually process
	// the work rather than throw it away with extra steps.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownDrainWindow)
	defer cancel()

	drained := 0
	abandoned := 0

	for {
		select {
		case msg, ok := <-msgChan:
			if !ok {
				m.reportDrain(state, drained, abandoned)
				return
			}
			state.Consumer.MarkDelivered(msg)
			select {
			case <-drainCtx.Done():
				// Out of time. Release the pooled message so the slot returns,
				// but leave the entry unacknowledged for the reclaim.
				transport.ReleaseMinedRelayMessage(msg.Message)
				msg.Message = nil
				abandoned++
				continue
			default:
			}
			if m.handBackOnExit(drainCtx, state, msg) {
				drained++
			} else {
				// The entry is still PENDING, same as anything the window ran
				// out on -- counting it as drained would tell an operator this
				// finished when Redis says otherwise.
				abandoned++
			}

		case <-drainCtx.Done():
			// Out of time and nothing is sitting in the buffer right now.
			// Anything the producer still delivers after this stays
			// unacknowledged in this consumer's PEL for the reclaim, same as
			// any entry the window did not fit -- an already-closed msgChan
			// (the ordinary idle-supplier case) is still picked immediately
			// above, since a closed channel read is always ready.
			m.reportDrain(state, drained, abandoned)
			return
		}
	}
}

// reportDrain emits the one log line a shutdown drain produces. It fires once per
// supplier per shutdown -- a state change, not a per-request event -- so Info is
// the right level and an operator reading a restart sees it without debug logging.
func (m *SupplierManager) reportDrain(state *SupplierState, drained, abandoned int) {
	RecordShutdownAbandonedRelays(state.OperatorAddr, abandoned)
	if drained == 0 && abandoned == 0 {
		return
	}
	m.logger.Info().
		Str(logging.FieldSupplier, state.OperatorAddr).
		Int("drained", drained).
		Int("abandoned", abandoned).
		Msg("drained delivery buffer on shutdown; abandoned entries stay pending for the reclaim")
}

// teardownCanFinishWork reports whether this fleet still holds the signing key
// for a supplier being torn down — that is, whether "draining" describes
// anything real. Without the key no relay response, claim or proof can be
// signed, so there is no pending work to drain, only messages to stop
// consuming, and saying "waiting for pending work" describes work that cannot
// happen. That line is what an operator reads mid-incident.
//
// It asks the key manager instead of taking the caller's word, because the
// caller's reason names what STARTED the teardown, not whether the key still
// exists: a rebalance or a lost lease can tear down a supplier whose key the
// operator removed meanwhile (not observed, but nothing orders the two). The key
// manager is local and authoritative.
//
// A nil key manager (tests only) answers true, which keeps the message it had
// before this distinction existed.
func (m *SupplierManager) teardownCanFinishWork(operatorAddr string) bool {
	if m.keyManager == nil {
		return true
	}

	_, err := m.keyManager.GetSigner(operatorAddr)

	return err == nil
}

// publishUnstakingState writes the draining supplier's state to the shared
// cache. Extracted from removeSupplier so the write can be unit-tested without
// standing up a consumer, a session store and an SMST manager -- the same
// reason resolveAndPublishSupplierState is a helper.
//
// A supplier keeps serving while it drains (IsActive is true for unstaking, see
// cache.SupplierState.IsActive), and SetSupplierState marshals the whole struct
// and overwrites. So every field the relayer reads has to be carried here or
// this write silently erases it.
// It copies the slices it publishes rather than aliasing the caller's state,
// and it does the copying itself so that no untested glue sits between the
// state and the write.
func (m *SupplierManager) publishUnstakingState(ctx context.Context, state *SupplierState) {
	if m.config.SupplierCache == nil {
		return
	}

	// nil means the supplier was removed before addSupplierWithData finished
	// resolving it. Treated as empty, which is exactly what the plain fields
	// used to yield -- this function fixed a race, and deliberately did not
	// change what gets written. That this write can publish an empty view over
	// a healthy one is a separate question, filed rather than answered here.
	view := state.stakeView.Load()
	if view == nil {
		view = &supplierStakeView{}
	}

	servicesCopy := make([]string, len(view.Services))
	copy(servicesCopy, view.Services)
	endpointsCopy := make([]cache.StakedEndpoint, len(view.StakedEndpoints))
	copy(endpointsCopy, view.StakedEndpoints)

	supplierState := &cache.SupplierState{
		Status:          cache.SupplierStatusUnstaking,
		Staked:          true, // Still staked, just unstaking
		OperatorAddress: state.OperatorAddr,
		Services:        servicesCopy,
		StakedEndpoints: endpointsCopy,
		UpdatedBy:       m.config.MinerID,
	}
	if cacheErr := m.config.SupplierCache.SetSupplierState(ctx, supplierState); cacheErr != nil {
		m.logger.Warn().
			Err(cacheErr).
			Str(logging.FieldSupplier, state.OperatorAddr).
			Msg("failed to update supplier state to unstaking in cache")
	}
}

// removeSupplier gracefully removes a supplier (waits for pending work).
//
// Drain-window semantics (post commit 8eb604c):
//
//  1. The state leaves m.suppliers and its context (state.cancelFn) is
//     cancelled in one synchronous step, stopSupplier, before anything
//     slow: the cancel signal must reach every in-flight
//     handleRelay/UpdateTree before the cleanup in teardownSupplier
//     (Consumer.Close, SMSTManager.Close, etc.).
//
//  2. Once cancelFn() fires, a concurrent
//     handleRelay call may already be mid-way through a Redis write
//     (UpdateTree, ACK, dedup set insert). Those calls now operate
//     with a cancelled context. Each such Redis operation returns a
//     context.Canceled-wrapped error.
//
//  3. The shutdown-cancel classifier (IsShutdownCancelError in
//     errors.go) recognises those wrapped context.Canceled errors and
//     routes the message through the ACK-and-discard path: the relay
//     is acknowledged on the stream so the consumer group does not
//     redeliver it to the survivor (avoiding double-count), and the
//     SMST write is treated as a no-op. If the miner restarts before
//     the stream entry's idle-claim timeout, the entry will be
//     redelivered via XCLAIM and retried cleanly.
//
//  4. DeadlineExceeded is intentionally NOT classified as a shutdown
//     cancel — an UpdateTree that times out on a slow Redis is a real
//     transient failure and must be retried, not swallowed.
//
// Net result: the drain window is safe. An in-flight handleRelay that
// observes ctx.Canceled during UpdateTree returns a
// context.Canceled-wrapped error, IsShutdownCancelError returns true,
// the caller ACKs the message, and the relay is accounted for on
// restart (if the miner recovers) or accepted as a bounded loss (if
// the supplier is genuinely being removed).
//
// state.wg.Wait() below then blocks until every handleRelay goroutine
// for this supplier has returned, so the subsequent Close() calls on
// Consumer/SessionCoordinator/SessionStore run against a fully quiesced
// supplier — no mid-flight writer can resurrect state after the map
// delete.
func (m *SupplierManager) removeSupplier(operatorAddr string, reason drainReason) {
	if state, ok := m.stopSupplier(operatorAddr, reason); ok {
		m.teardownSupplier(state)
	}
}

// stopSupplier takes the supplier out of the map and cancels its consume loop.
// It makes no network call, so a release can run it before returning.
//
// Atomic remove-and-take: the state leaves the map FIRST so the slow
// per-supplier teardown (Consumer.Close -> wg.Wait can sit on a blocked XREAD
// for tens of seconds) holds no map coordination at all. A later claim of the
// same supplier on this miner sees an empty slot and constructs a fresh state;
// the old state's resources are private to its teardown.
func (m *SupplierManager) stopSupplier(operatorAddr string, reason drainReason) (*SupplierState, bool) {
	state, exists := m.suppliers.LoadAndDelete(operatorAddr)
	if !exists {
		return nil, false
	}
	state.drainReason.Store(int32(reason))
	state.StoreStatus(SupplierStatusDraining)
	state.cancelFn()
	return state, true
}

// teardownSupplier waits for a stopped supplier's consume loop and closes
// everything it owned.
func (m *SupplierManager) teardownSupplier(state *SupplierState) {
	operatorAddr := state.OperatorAddr

	// Capture the lifecycle context once under m.mu.RLock. removeSupplier
	// can run concurrently with Close() (Close() writes m.ctx under m.mu),
	// so every direct `m.ctx` read inside this function would be a data
	// race. Close() cancels m.ctx but does not nil it, so the captured
	// local is always a usable context — cancelled if Close() ran first,
	// which is fine because every downstream call respects ctx.Done() and
	// fails fast rather than leaving half-drained state.
	ctx := m.keyChangeReadCtx()
	if ctx == nil {
		// Defensive: keyChangeReadCtx can only return nil before Start()
		// assigns m.ctx. Fall back to Background so the cleanup still
		// runs on a best-effort basis rather than panicking downstream.
		ctx = context.Background()
	}

	m.publishUnstakingState(ctx, state)

	if m.teardownCanFinishWork(operatorAddr) {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("supplier marked as draining, waiting for pending work...")
	} else {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("supplier torn down: this fleet no longer holds its signing key, so no " +
				"pending relay, claim or proof can be signed for it")
	}

	// Wait for pending work (TODO: implement proper tracking)
	// For now, just wait for consumer to finish.
	// No global lock — the state was already atomically removed from the map,
	// and its context cancelled, by stopSupplier.
	state.wg.Wait()

	if state.LifecycleManager != nil {
		if err := state.LifecycleManager.Close(); err != nil {
			m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("error closing lifecycle manager")
		}
	}

	m.releaseOwnPendingOnExit(state)
	m.checkpointTreesOnExit(state)

	if state.SMSTManager != nil {
		if err := state.SMSTManager.Close(); err != nil {
			m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("error closing SMST manager")
		}
	}

	if err := state.Consumer.Close(); err != nil {
		m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("error closing consumer")
	}
	if err := state.SessionCoordinator.Close(); err != nil {
		m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("error closing session coordinator")
	}
	if err := state.SessionStore.Close(); err != nil {
		m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("error closing session store")
	}

	// Only remove from registry and cache if no other miner has already claimed
	// this supplier. During rebalance, miner1 may release a supplier that miner2
	// has already claimed and registered — deleting here would clobber miner2's entries.
	claimKey := m.config.RedisClient.KB().MinerClaimKey(operatorAddr)
	claimOwner, claimErr := m.config.RedisClient.Get(ctx, claimKey).Result()
	reclaimedByOther := claimErr == nil && claimOwner != "" && claimOwner != m.config.MinerID

	if reclaimedByOther {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Str("new_owner", claimOwner).
			Msg("supplier already reclaimed by another miner, skipping registry/cache cleanup")
	} else {
		// Publish removal to registry
		if m.registry != nil {
			if err := m.registry.PublishSupplierUpdate(ctx, SupplierUpdateActionRemove, operatorAddr, nil); err != nil {
				m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("failed to publish removal status")
			}
		}

		// The supplier's cache entry is deliberately NOT deleted.
		//
		// That entry is not our bookkeeping: it is the CHAIN's answer, cached. Its
		// Services list is derived from GetActiveServiceConfigs at the observed
		// height, so after a supplier unstakes it empties by itself at the session
		// boundary poktroll scheduled (MsgUnstakeSupplier sets DeactivationHeight =
		// next session start on every service config). That list IS the thing that
		// stops the relayer serving, at exactly the right block.
		//
		// Deleting it replaces a correct answer with "unknown" -- and the relayer
		// reads a missing entry as permission to serve optimistically whenever it
		// still holds the signing key (see decideSupplierServe). So the delete
		// undid the very protection the teardown had just written, and made
		// removing a supplier cause MORE serving, not less.
		//
		// What ages the entry out instead is the orphan sweep, which reports and
		// lets an operator clean up; nothing auto-deletes state that gates money.
	}

	supplierManagerSuppliersActive.Dec()

	m.logger.Info().
		Str(logging.FieldSupplier, operatorAddr).
		Msg("supplier gracefully removed")
}

// checkpointTreesOnExit writes the covering live_root of every tree a stopped
// supplier still holds, once its consume loop and its lifecycle have stopped
// and before this instance deletes its lease. The next owner resumes from live_root, and the
// relays finished one at a time since the last checkpoint are already
// acknowledged: nothing redelivers them. On a context of its own, because the
// supplier's is cancelled, and during Close so is the manager's.
func (m *SupplierManager) checkpointTreesOnExit(state *SupplierState) {
	if state.SMSTManager == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainWindow)
	defer cancel()
	written, failed, err := state.SMSTManager.CheckpointAllOnExit(ctx)
	// Added even when zero, so a supplier that has been torn down exports the
	// series and a query can tell "none failed" from "not exported".
	smstExitCheckpointFailedTotal.WithLabelValues(state.OperatorAddr).Add(float64(failed))
	if err != nil {
		// Once per supplier torn down, not per relay.
		m.logger.Warn().
			Err(err).
			Str(logging.FieldSupplier, state.OperatorAddr).
			Int("written", written).
			Int("failed", failed).
			Msg("could not checkpoint every tree on exit; the next owner may resume without relays this one acknowledged")
		return
	}
	m.logger.Debug().
		Str(logging.FieldSupplier, state.OperatorAddr).
		Int("written", written).
		Msg("checkpointed trees on exit")
}

// GetSupplierState returns the state for a specific supplier.
func (m *SupplierManager) GetSupplierState(operatorAddr string) (*SupplierState, bool) {
	return m.suppliers.Load(operatorAddr)
}

// ListSuppliers returns all active supplier addresses.
func (m *SupplierManager) ListSuppliers() []string {
	suppliers := make([]string, 0, m.suppliers.Size())
	m.suppliers.Range(func(addr string, _ *SupplierState) bool {
		suppliers = append(suppliers, addr)
		return true
	})
	return suppliers
}

// Close gracefully shuts down the supplier manager.
func (m *SupplierManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true

	if m.cancelFn != nil {
		m.cancelFn()
	}
	m.mu.Unlock()

	// Stop the claimer's loops first -- no rebalance, renewal or orphan claim
	// may change the supplier set under the teardown -- but release NOTHING yet:
	// every lease stays ours until its supplier's consume loop has stopped
	// writing, and FinishShutdown deletes them at the end.
	if m.claimer != nil {
		m.claimer.StopLoops()
	}

	// Stop growing the connection pool before anything starts closing it: a
	// resize landing during teardown would dial members that the shutdown
	// already walked past.
	if m.poolResizeCancel != nil {
		m.poolResizeCancel()
	}
	m.poolResizeWG.Wait()

	// Stop the reconciler BEFORE tearing down suppliers, so an in-flight
	// OnBlock pass cannot reconcile / ResubmitMessage against a supplier whose
	// client is being closed out from under it.
	if m.reconcilerCancel != nil {
		m.reconcilerCancel()
	}
	m.reconcilerWG.Wait()
	if m.inclusionReconciler != nil {
		_ = m.inclusionReconciler.Close()
	}

	// Signal EVERY supplier to stop before waiting on any of them.
	//
	// The waits below are not instant: a stream consumer parked on a blocking
	// XREADGROUP only notices the cancellation when its block elapses, because
	// go-redis sets no read deadline from the context. Cancelling and waiting
	// in the same pass makes those waits SERIAL -- one block interval per
	// supplier -- and this miner has already run ~594 suppliers on one
	// instance, which is hours of shutdown and a SIGKILL long before the end.
	// Cancelled together, the intervals overlap and the whole teardown costs
	// one of them.
	m.suppliers.Range(func(_ string, state *SupplierState) bool {
		// The process is going away, so the buffered work is RELEASED rather
		// than destroyed. Set explicitly instead of leaning on the zero value:
		// a default that happens to be right is not the same as a decision.
		state.drainReason.Store(int32(drainShutdown))
		state.cancelFn()
		return true
	})

	// Now collect. Range is safe under concurrent mutation in xsync.Map; we
	// LoadAndDelete each entry so a parallel claimer release doesn't
	// double-close the resources.
	m.suppliers.Range(func(addr string, _ *SupplierState) bool {
		state, ok := m.suppliers.LoadAndDelete(addr)
		if !ok {
			return true
		}
		state.wg.Wait()

		if state.LifecycleManager != nil {
			_ = state.LifecycleManager.Close()
		}
		m.releaseOwnPendingOnExit(state)
		m.checkpointTreesOnExit(state)
		if state.SMSTManager != nil {
			_ = state.SMSTManager.Close()
		}
		_ = state.Consumer.Close()
		_ = state.SessionCoordinator.Close()
		_ = state.SessionStore.Close()
		return true
	})

	// Drains released before Close delete their own leases when they end;
	// only then is every writer gone and the rest of the leases safe to delete.
	m.drainWG.Wait()
	if m.claimer != nil {
		m.claimer.FinishShutdown(context.Background())
	}

	// Stop query subpool gracefully (drains queued tasks)
	if m.querySubpool != nil {
		m.querySubpool.StopAndWait()
	}
	// Every SMST manager is closed by now, so a queued compaction returns on
	// its closed flag instead of starting.
	if m.coldCompactionPool != nil {
		m.coldCompactionPool.StopAndWait()
	}

	m.logger.Info().Msg("supplier manager closed")
	return nil
}

// runStreamTrimmer periodically trims old entries from supplier streams.
// Runs every hour (or CacheTTL/2 if shorter) and removes entries older than CacheTTL.
// This is safe because relays older than CacheTTL are already invalid
// (session/claim windows are closed, so they can't earn rewards anyway).
func (m *SupplierManager) runStreamTrimmer(ctx context.Context) {
	// Calculate trim interval: every hour or CacheTTL/2, whichever is shorter
	trimInterval := time.Hour
	if m.config.CacheTTL > 0 && m.config.CacheTTL/2 < trimInterval {
		trimInterval = m.config.CacheTTL / 2
	}

	// Use CacheTTL as the max age for entries
	maxAge := m.config.CacheTTL
	if maxAge == 0 {
		maxAge = 2 * time.Hour // Default if not configured
	}

	m.logger.Info().
		Dur("trim_interval", trimInterval).
		Dur("max_age", maxAge).
		Msg("stream trimmer started - will remove entries older than max_age")

	ticker := time.NewTicker(trimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info().Msg("stream trimmer stopped")
			return

		case <-ticker.C:
			m.trimAllSupplierStreams(ctx, maxAge)
		}
	}
}

// trimAllSupplierStreams trims old entries from all claimed supplier streams.
// Submits work to the pool for parallel execution and waits for completion.
func (m *SupplierManager) trimAllSupplierStreams(ctx context.Context, maxAge time.Duration) {
	suppliers := make([]*SupplierState, 0, m.suppliers.Size())
	m.suppliers.Range(func(_ string, state *SupplierState) bool {
		suppliers = append(suppliers, state)
		return true
	})

	if len(suppliers) == 0 {
		return
	}

	m.logger.Debug().
		Int("suppliers", len(suppliers)).
		Dur("max_age", maxAge).
		Msg("starting stream trimming for all suppliers")

	// Create a task group to wait for all trim operations
	group := m.querySubpool.NewGroup()
	var totalTrimmed int64
	var trimmedSuppliers int
	var mu sync.Mutex // Protect counters

	for _, state := range suppliers {
		// Skip if consumer is nil (shouldn't happen but defensive)
		if state.Consumer == nil {
			continue
		}

		// Submit trimming work to the group
		supplier := state // capture for closure
		group.SubmitErr(func() error {
			trimmed, err := supplier.Consumer.TrimStream(ctx, maxAge)
			if err != nil {
				m.logger.Warn().
					Err(err).
					Str("supplier", supplier.OperatorAddr).
					Msg("failed to trim stream")
				return nil // Don't fail the group for individual stream errors
			}
			if trimmed > 0 {
				mu.Lock()
				totalTrimmed += trimmed
				trimmedSuppliers++
				mu.Unlock()
			}
			return nil
		})
	}

	// Wait for all trim operations to complete.
	//
	// The previous comment called this Wait invariant-nil and it was not. It
	// reasoned only about task errors -- tasks go in via SubmitErr(func() error)
	// and the only one that exists swallows its own with `return nil // Don't fail
	// the group for individual stream errors`, so that half was right -- and never
	// mentioned panics: pond recovers them by default (pool.go:534) and delivers
	// them through this same channel, so the old `_ =` made a panic in TrimStream
	// vanish with no log, no metric and no crash, against the repo's convention
	// that a recovered panic is counted AND logged (logging/recovery.go).
	//
	// The rule below is deliberately a rule and not a list, because this channel
	// carries more than those two: ErrPoolStopped for a Submit made after the pool
	// stopped (result.go:77-83), ErrGroupStopped for a stopped group (group.go:12),
	// and the context error if the pool's context is cancelled -- and this pool is
	// stopped on shutdown (StopAndWait, :2375). Enumerating them ages badly. Only a
	// recovered panic is counted and raised; anything else this channel brings is
	// shutdown, and shutdown at Error would spend the very signal this handling
	// exists to create on every rollout that lands in that window.
	//
	// PanicRecoveriesTotal is enough and no loss-specific counter is added, because
	// the work is retried: trimming runs off a ticker, so the next tick covers
	// whatever this pass dropped.
	if err := group.Wait(); err != nil {
		if errors.Is(err, pond.ErrPanic) {
			logging.PanicRecoveriesTotal.WithLabelValues("supplier_stream_trim").Inc()
			m.logger.Error().Err(err).Msg("stream trimming: a trim task panicked")
		} else {
			m.logger.Debug().Err(err).Msg("stream trimming: pass abandoned (pool shutting down)")
		}
	}

	if totalTrimmed > 0 {
		m.logger.Info().
			Int64("total_trimmed", totalTrimmed).
			Int("suppliers_trimmed", trimmedSuppliers).
			Int("total_suppliers", len(suppliers)).
			Dur("max_age", maxAge).
			Msg("stream trimming completed")
	}
}

// ensureSharedTrackers lazily constructs the process-wide submission tracker and
// the claim/proof inclusion trackers exactly once, regardless of how many
// supplier keys this manager drives. They are stateless across suppliers
// (supplier/session identity is passed per call), so one shared instance per
// miner — rather than one per key — is both correct and far cheaper at the
// hundreds-of-keys scale operators actually run. Safe under concurrent
// addSupplier* calls via sync.Once.
func (m *SupplierManager) ensureSharedTrackers() {
	m.sharedTrackersOnce.Do(func() {
		m.sharedSubmissionTracker = NewSubmissionTracker(m.logger, m.config.RedisClient, m.config.SubmissionTrackingTTL)

		// Only build the rebroadcast store + reconciler when we can actually run
		// it. Leaving m.rebroadcastStore nil means the lifecycle callback skips
		// persistence entirely — no orphaned Redis writes that nothing consumes.
		if m.config.ProofQueryClient == nil {
			m.logger.Warn().Msg("no proof query client; inclusion reconciler disabled (fire-once claim/proof, no rebroadcast)")
			return
		}
		// The TYPE guarantees a block client can Subscribe; it cannot guarantee
		// there IS one, and a nil interface satisfies any interface field. This
		// check is not defensive padding: without it the reconciler is built with
		// a live store and its loop dereferences nil one line into the goroutine,
		// which is a SIGSEGV rather than a missing feature. It belongs here and
		// not in the loop for the reason the comment above gives -- a reconciler
		// with no trigger still writes rebroadcast entries nothing will ever read.
		if m.config.BlockClient == nil {
			m.logger.Warn().Msg("no block client; inclusion reconciler disabled (fire-once claim/proof, no rebroadcast)")
			return
		}
		inclusionQuery := m.config.ProofQueryClient
		m.rebroadcastStore = NewRebroadcastStore(m.config.RedisClient, 0) // 0 → default TTL

		recordClaimOutcome := func(ctx context.Context, e rebroadcastEntry, supplier string, _ int64, sessionID, outcome string, inclusionHeight int64) error {
			if outcome == inclusionFound {
				// The chain holds this claim. Whatever the broadcast reported,
				// the session must go back to `claimed` or its proof never goes
				// out — and a claim on-chain without a proof is a SLASH,
				// strictly worse than the lost reward the resend was meant to
				// avoid.
				//
				// This runs BEFORE the counter and the tracker on purpose. A
				// failure here keeps the pending entry, so the reconciler
				// re-delivers this same observation on the next block; counting
				// first would tally one claim once per retry.
				if err := m.reactivateClaimedSession(ctx, supplier, sessionID, e); err != nil {
					return err
				}
			}
			claimInclusionOutcomeTotal.WithLabelValues(supplier, e.ServiceID, outcome).Inc()
			// After the counter and after reactivation, for the same reason
			// reactivation runs first: a failure above keeps the entry and the
			// observation is re-delivered next block, and the ledger must not be
			// moved once per retry.
			m.settleLedgerOutcome(ctx, RebroadcastPhaseClaim, e, supplier, sessionID, outcome)
			if outcome == inclusionMissing {
				m.recordMissingCause(ctx, RebroadcastPhaseClaim, e, supplier, sessionID)
			}
			if m.sharedSubmissionTracker != nil {
				// Claim outcome is matched by the ORIGINAL submit tx hash (the one
				// stored on the submission record); a rebroadcast changes the
				// latest hash but the record key does not, so look up by OrigTxHash.
				origHash := e.OrigTxHash
				if origHash == "" {
					origHash = e.TxHash
				}
				// Logged HERE because the error does NOT rise: this closure returns
				// nil after this block, so a caller cannot see it and cannot decide
				// on it. Reporting is all that is left, and a submission outcome
				// that goes unrecorded with nothing said is a gap an operator can
				// never reconstruct -- the chain remembers the claim, not our
				// ledger's opinion of it.
				//
				// Warn, and the volume is bounded: the reconciler runs one pass per
				// block over the sessions THIS replica owns, so the ceiling is
				// pending-sessions-per-block, not per relay. A flood needs a
				// PARTIAL Redis failure -- reads working, tracker writes failing --
				// because a total outage fails ActiveGroups first and the pass
				// returns after a single line.
				//
				// If item 37 ever gives this closure an error path, look at the
				// caller before adding a second line: today there is none to
				// double up with.
				if err := m.sharedSubmissionTracker.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
					Supplier:        supplier,
					TxHash:          origHash,
					Outcome:         outcome,
					InclusionHeight: inclusionHeight,
					Rebroadcasts:    e.Rebroadcasts,
				}); err != nil {
					m.logger.Warn().Err(err).
						Str("supplier", supplier).
						Str(logging.FieldSessionID, sessionID).
						Str("outcome", outcome).
						Msg("claim on-chain outcome observed but not recorded in the submission tracker")
				}
			}
			return nil
		}
		recordProofOutcome := func(ctx context.Context, e rebroadcastEntry, supplier string, sessionEnd int64, sessionID, outcome string, inclusionHeight int64) error {
			proofInclusionOutcomeTotal.WithLabelValues(supplier, e.ServiceID, outcome).Inc()
			// Closes the `unresolved` balance this session's submission failure
			// opened, and names where the money went. on_chain_rejected settles
			// it as lost like on_chain_missing does: the chain executed the proof
			// and refused it, so nothing further will answer, and without this
			// branch a rejected proof's balance would never come down.
			m.settleLedgerOutcome(ctx, RebroadcastPhaseProof, e, supplier, sessionID, outcome)
			if outcome == inclusionMissing {
				m.recordMissingCause(ctx, RebroadcastPhaseProof, e, supplier, sessionID)
			}
			if m.sharedSubmissionTracker != nil {
				// Same as the claim side above: the error does not rise, so this
				// line is the only record of it. Bounded the same way, per block
				// and per owned session, and a flood needs a PARTIAL Redis failure
				// because a total one fails ActiveGroups first.
				if err := m.sharedSubmissionTracker.UpdateProofOnChainOutcome(ctx, ProofOnChainUpdate{
					Supplier:        supplier,
					SessionEnd:      sessionEnd,
					SessionID:       sessionID,
					Outcome:         outcome,
					InclusionHeight: inclusionHeight,
					NewProofTxHash:  e.TxHash,
					Rebroadcasts:    e.Rebroadcasts,
				}); err != nil {
					m.logger.Warn().Err(err).
						Str("supplier", supplier).
						Str(logging.FieldSessionID, sessionID).
						Str("outcome", outcome).
						Msg("proof on-chain outcome observed but not recorded in the submission tracker")
				}
			}
			// Metric + tracker only, and that is a GAP rather than a property:
			// proof_tx_error has the same anatomy as claim_tx_error — the
			// broadcast can report failure while the proof lands — and there IS
			// a state to restore, `proved`. It does not cost a slash (the proof
			// is on-chain), so it is not fixed here; it leaves the session in a
			// failed state and the ledger counting it lost. Queue item 37.
			return nil
		}

		claimPhase := reconcilePhase{
			phase:             RebroadcastPhaseClaim,
			windowCloseHeight: sharedtypes.GetClaimWindowCloseHeight,
			verdict:           claimPhaseVerdict,
			recordOutcome:     recordClaimOutcome,
			recordRebroadcast: func(supplier, serviceID, result string) {
				claimRebroadcastsTotal.WithLabelValues(supplier, serviceID, result).Inc()
			},
		}
		// diagnoseProofRejection answers the one question about a rejection that
		// can be answered from here, and says so when it cannot.
		//
		// The chain does not expose WHY it refused a proof: the reason lives in
		// the FailureReason of an EndBlocker event, the claim itself carries four
		// fields and none of them is the reason, and reading events would require
		// the transaction indexer this whole reconciler exists to work without.
		// What IS available is the root the claim committed to, which came back
		// from the same read that produced the verdict. Comparing it against the
		// root this miner stored separates one cause from the other six: a
		// mismatch means what we hold is not what we claimed and no proof built
		// from it can ever pass, while a match means the claim was right and the
		// proof failed on construction or a signature.
		//
		// Warn and not Debug: it fires once per rejected session -- the entry is
		// cleared immediately after -- so it is bounded by the defect existing,
		// and it is money.
		diagnoseProofRejection := func(ctx context.Context, e rebroadcastEntry, supplier string, sessionEnd int64, sessionID string, height int64, onChainRoot []byte) {
			cause := rejectionRootUnknown
			// The session store is per supplier, held on the SupplierState this
			// replica owns -- the same map the reconciler's ownership filter
			// reads, so a supplier whose rejection reaches this closure always
			// has one.
			st, owned := m.suppliers.Load(supplier)
			if owned && st.SessionStore != nil && len(onChainRoot) > 0 {
				// A missing snapshot is EXPECTED, not exceptional. Nothing
				// deletes a session record -- there is no production caller of
				// the store's Delete -- so it lives until SessionTTL expires it,
				// refreshed on every write. Two hours against a proof window of
				// minutes is comfortable with mainnet parameters, and both of
				// those numbers are configurable, so the margin is a bound and
				// never a guarantee. When the snapshot is gone the honest answer
				// is that the comparison could not be made, which is why
				// root_unknown is a first-class cause and not an error.
				snapshot, sErr := st.SessionStore.Get(ctx, sessionID)
				switch {
				case sErr != nil:
					m.logger.Debug().Err(sErr).Str("session_id", sessionID).
						Msg("proof rejected: could not read the session to compare roots")
				case snapshot == nil || len(snapshot.ClaimedRootHash) == 0:
					// Nothing to compare against; cause stays root_unknown.
				case bytes.Equal(snapshot.ClaimedRootHash, onChainRoot):
					cause = rejectionRootMatch
				default:
					cause = rejectionRootMismatch
				}
			}
			proofRejectionDiagnosisTotal.WithLabelValues(cause).Inc()

			m.logger.Warn().
				Str("supplier", supplier).
				Str("session_id", sessionID).
				Int64("session_end", sessionEnd).
				Int64("observed_at_height", height).
				Str("tx_hash", e.TxHash).
				Str("cause", cause).
				Msg("proof rejected on-chain: it reached the chain and the EndBlocker refused it; it will not be re-sent")

			if m.sharedSubmissionTracker == nil {
				return
			}
			if err := m.sharedSubmissionTracker.UpdateProofOnChainOutcome(ctx, ProofOnChainUpdate{
				Supplier:        supplier,
				SessionEnd:      sessionEnd,
				SessionID:       sessionID,
				Outcome:         inclusionRejected,
				InclusionHeight: height,
				RejectionCause:  cause,
			}); err != nil {
				// Same shape as its siblings: the error does not rise, so this
				// line is the only record of it.
				m.logger.Warn().Err(err).Str("session_id", sessionID).
					Msg("proof rejection diagnosis not recorded in the submission tracker")
			}
		}

		proofPhase := reconcilePhase{
			phase:             RebroadcastPhaseProof,
			windowCloseHeight: sharedtypes.GetProofWindowCloseHeight,
			// Proof inclusion is read from the claim's ProofValidationStatus
			// (VALIDATED), not from proofs: a submitted proof is validated and
			// deleted in the EndBlocker of its submission block, so it is not
			// queryable afterwards. See query.GetSupplierSessionStates.
			//
			verdict:           proofPhaseVerdict,
			diagnoseRejection: diagnoseProofRejection,
			recordOutcome:     recordProofOutcome,
			recordRebroadcast: func(supplier, serviceID, result string) {
				proofRebroadcastsTotal.WithLabelValues(supplier, serviceID, result).Inc()
			},
		}

		m.inclusionReconciler = NewInclusionReconciler(
			m.logger,
			m.config.SharedClient,
			m.rebroadcastStore,
			m, // SupplierManager implements MessageResubmitter
			claimPhase,
			proofPhase,
			inclusionQuery.GetSupplierSessionStates,
			m.config.InclusionReconcilerConfig,
		)
		// Per-supplier ownership: only reconcile suppliers this replica controls
		// (matches the SupplierClaimer SetNX submission-coordination model).
		m.inclusionReconciler.SetOwnershipFilter(func(supplier string) bool {
			_, owned := m.suppliers.Load(supplier)
			return owned
		})

		m.startReconcilerBlockLoop()
	})
}

// startConnPoolResizeLoop keeps the transaction connection pool sized for the
// suppliers this replica currently holds a lease on.
//
// It takes its OWN subscription rather than riding the reconciler's loop, which
// looks like the obvious place to hang it. That loop never starts when the
// reconciler was not built -- no proof query client, or no block client at all --
// so sharing it would silently tie the size of the connection pool to whether
// the inclusion reconciler happens to be configured, two things with no
// relationship to each other.
//
// The trigger is every block, and the resize is level-triggered, so a takeover
// that moves leases mid-window is picked up on the next block rather than
// whenever the previous holder's lease finally expires.
func (m *SupplierManager) startConnPoolResizeLoop() {
	if m.config.TxClient == nil {
		return
	}

	// Presence only: the field's type already guarantees Subscribe, so what is
	// left to check is whether a client was wired at all -- a nil one is a
	// supported configuration for tooling and tests.
	//
	// Warn and not Error: the pool keeps the floor it was built with, which is
	// what every replica ran with before it could grow at all. Claims still
	// submit; a replica holding many leases just runs with fewer connections
	// than it should.
	if m.config.BlockClient == nil {
		m.logger.Warn().Msg("no block client; transaction connection pool will stay at its startup size")
		return
	}

	m.mu.RLock()
	parent := m.ctx
	m.mu.RUnlock()
	if parent == nil {
		parent = context.Background()
	}
	loopCtx, cancel := context.WithCancel(parent)
	m.poolResizeCancel = cancel
	m.poolResizeWG.Add(1)

	resize := func(ctx context.Context) {
		defer m.poolResizeWG.Done()

		blockCh := m.config.BlockClient.Subscribe(ctx, blockEventSubscriberBuffer)
		runCoalescingBlockLoop(ctx, blockCh, func(int64) {
			claimer := m.claimer
			if claimer == nil {
				return
			}
			if err := m.config.TxClient.ResizeConnPool(ctx, claimer.ClaimedCount()); err != nil {
				// Warn, not Error: this fires once per block at worst and only
				// when a grow failed, and the pool keeps every connection it
				// already had, so the failure costs capacity rather than
				// correctness.
				m.logger.Warn().Err(err).Msg("could not resize the transaction connection pool")
			}
		})
	}

	go logging.RecoverGoRoutine(m.logger, "conn_pool_resize", resize)(loopCtx)
}

// startReconcilerBlockLoop drives reconciler.OnBlock once per new block from the
// block client's event stream (the same stream the lifecycle uses). Runs on
// every replica; the reconciler's per-supplier ownership filter ensures each
// replica only acts on its own suppliers.
func (m *SupplierManager) startReconcilerBlockLoop() {
	// Derive from the manager lifecycle ctx so the loop also stops if the parent
	// is canceled (not only via Close); fall back to Background if unset.
	m.mu.RLock()
	parent := m.ctx
	m.mu.RUnlock()
	if parent == nil {
		parent = context.Background()
	}
	loopCtx, cancel := context.WithCancel(parent)
	m.reconcilerCancel = cancel
	m.reconcilerWG.Add(1)
	go func() {
		defer m.reconcilerWG.Done()
		// Decouple ingestion from processing. OnBlock runs a full reconcile pass
		// and blocks on group.Wait(), so reading the channel inline would stall
		// block delivery and let the fan-out channel fill — the same failure that
		// stranded the session lifecycle at high supplier counts. The coalescing
		// loop's reader drains the channel instantly while a single processor runs
		// OnBlock against the latest height. Reconciliation is level-triggered (it
		// checks pending claims/proofs against the current height), so coalescing
		// redundant ticks never skips work, and the single processor matches
		// OnBlock's own single-flight guard.
		blockCh := m.config.BlockClient.Subscribe(loopCtx, blockEventSubscriberBuffer)
		runCoalescingBlockLoop(loopCtx, blockCh, m.inclusionReconciler.OnBlock)
		// A return with the loop context still live means the block channel closed
		// under us: the reconciler loses its per-block trigger, so the claim/proof
		// forfeits it exists to recover would go unrecovered. Make it loud; an
		// expected cancel is Debug.
		if loopCtx.Err() == nil {
			m.logger.Warn().Msg("inclusion reconciler block channel closed unexpectedly; reconciler block loop stopped")
		} else {
			m.logger.Debug().Msg("inclusion reconciler block loop stopped")
		}
	}()
}

// settleLedgerOutcome moves a session's money to its final door once the chain
// has answered for it. It is the only place the ledger CLOSES a balance, and the
// counterpart to the places that open one.
//
// THE DISCRIMINATOR IS e.OrigTxHash, AND IT IS A FACT THE ENTRY ALREADY CARRIES.
// persistRebroadcastEntry is its only writer and it is never rewritten, so it is
// the original submission's tx hash for as long as the entry lives. EMPTY means
// the submission was never confirmed -- the self-heal persists on the failure
// paths pass "" precisely to mark that -- which is exactly the case where
// RecordRevenueClaimed / RecordRevenueProved did NOT run at submission time.
// NON-EMPTY means the transaction was accepted and the revenue was already
// counted, so there is nothing to add here and adding it would count the session
// twice. Without this test, crediting every on_chain_found would double every
// normal claim and proof, because the success paths persist an entry too.
//
// OWNERSHIP IS NOT RE-DECIDED HERE. The weight comes from the session store on
// the SupplierState this replica holds, which is the same m.suppliers.Load that
// the reconciler's own ownership filter reads. Two predicates answering the same
// question is how they drift apart, and only the replica that owns a supplier
// may report its accounting -- otherwise two replicas report the same money.
//
// KNOWN GAP, deliberately not closed here: a CONFIRMED proof the chain turns out
// not to hold was already counted into upokt_proved_total at submission. This
// function cannot subtract it -- a counter does not go down -- so `proved`
// overstates by that amount and proof_inclusion_outcome_total is what reveals
// it. That is the success side of the ledger and it is a separate change.
func (m *SupplierManager) settleLedgerOutcome(
	ctx context.Context,
	phase RebroadcastPhase,
	e rebroadcastEntry,
	supplier, sessionID, outcome string,
) {
	// poll_error is not an answer: the entry is kept and asked again next block.
	if outcome == inclusionPollErr {
		return
	}
	if e.OrigTxHash != "" {
		return
	}

	st, owned := m.suppliers.Load(supplier)
	if !owned || st.SessionStore == nil {
		return
	}
	snapshot, err := st.SessionStore.Get(ctx, sessionID)
	if err != nil || snapshot == nil {
		// No snapshot, no weight, and inventing one would be worse than the
		// absence. The session's own failure is already in sessions_failed_total;
		// what is missing is the money, and an unresolved balance that never
		// closes is exactly the visible signal this family exists to give.
		m.logger.Debug().Err(err).
			Str("supplier", supplier).
			Str(logging.FieldSessionID, sessionID).
			Str("phase", string(phase)).
			Str("outcome", outcome).
			Msg("inclusion outcome not settled into the ledger: no session snapshot to weigh it with")
		return
	}

	relays := snapshot.RelayCount
	computeUnits := int64(snapshot.TotalComputeUnits)

	switch phase {
	case RebroadcastPhaseClaim:
		// Nothing was opened as unresolved on the claim side: this money was
		// never in the book, so there was no balance to hold. The chain's answer
		// decides whether it enters the book or is written off.
		if outcome == inclusionFound {
			// The chain HOLDS this claim, so "uPOKT claimed" is literally true.
			// This is the write the recovery path was missing: the submission
			// never confirmed, so nothing counted it, and the session goes on to
			// be proved -- crediting `proved` revenue that `claimed` never had.
			RecordRevenueClaimed(supplier, snapshot.ServiceID, snapshot.TotalComputeUnits, relays)
			return
		}
		RecordRevenueForgone(supplier, snapshot.ServiceID, outcome, relays, computeUnits)

	case RebroadcastPhaseProof:
		// This money IS in the book and is waiting in `unresolved`, opened by the
		// same failure path that wrote this entry. Close that balance first, then
		// name where it went.
		RecordSessionUnresolvedResolved(supplier, snapshot.ServiceID, string(phase), relays, computeUnits)
		if outcome == inclusionFound {
			RecordRevenueProved(supplier, snapshot.ServiceID, snapshot.TotalComputeUnits, relays)
			return
		}
		RecordRevenueLost(supplier, snapshot.ServiceID, outcome, relays, computeUnits)
	}
}

// recordMissingCause splits an on_chain_missing verdict by what the chain says
// about the transaction itself, and changes NOTHING else.
//
// It is deliberately an instrument and not a decision. The read cannot authorise
// acting differently, because its own negative answer -- the index does not hold
// this hash -- still covers a transaction sitting in the mempool, and resending
// on that would sign a second one while the first is alive. So the resend gate
// above is untouched: this only says WHY the session was missing.
//
// The chain's codespace and code go in the LOG, never in a label: they come from
// the chain, so their value set is unbounded.
func (m *SupplierManager) recordMissingCause(
	ctx context.Context,
	phase RebroadcastPhase,
	e rebroadcastEntry,
	supplier, sessionID string,
) {
	if m.config.TxClient == nil {
		return
	}

	cause, res := m.config.TxClient.ReadInclusionForEntry(ctx, e.OrigTxHash, e.TxHash)

	cause = resolveMissingCause(cause, e.Rebroadcasts)
	inclusionMissingCauseTotal.WithLabelValues(string(phase), cause.String()).Inc()

	event := m.logger.Debug()
	if res != nil {
		event = event.Uint32("tx_code", res.Code).Str("tx_codespace", res.Codespace).Str("raw_log", res.RawLog)
	}
	event.
		Str("phase", string(phase)).
		Str(logging.FieldSupplier, supplier).
		Str(logging.FieldSessionID, sessionID).
		Str("cause", cause.String()).
		Msg("inclusion reconcile: missing session classified by post-inclusion read")
}

// resolveMissingCause refuses to name a cause the entry's hashes cannot justify.
//
// The entry remembers two hashes -- the original and the LATEST resend -- so
// past one resend the hash of every middle attempt has been overwritten. The
// read is then answering about a strict subset of the transactions that were
// actually broadcast, and only one of its verdicts is unsafe under that: a
// NotInBlock derived from hashes that are all absent says nothing about the
// one it was never given, which may well have been included and failed. That
// would not be an incomplete label, it would be an INVERTED one, pointing the
// operator at "it never reached a block" when the truth is "it did and was
// rejected" -- the opposite diagnosis.
//
// Unknown already means exactly this ("did not answer in a way that can be
// decided"), and the cause only feeds a label and a Debug log, so declining
// to name it costs no behaviour. Preferring not to know over knowing wrong is
// the cheaper error here. The default sets no cap (MaxRebroadcasts nil), so any
// entry with two counted resends reaches it; only an explicit cap of 1 makes it
// unreachable.
func resolveMissingCause(cause tx.TxInclusion, rebroadcasts int) tx.TxInclusion {
	if cause == tx.TxInclusionNotInBlock && rebroadcasts > 1 {
		return tx.TxInclusionUnknown
	}
	return cause
}

// reactivateClaimedSession returns a session to `claimed` after the reconciler
// OBSERVED its claim on-chain, so the lifecycle resumes and submits the proof.
//
// The claimed root comes from the persisted MsgCreateClaim, not from the SMST:
// those bytes are the message that was signed and accepted, they are never
// mutated by a rebroadcast, and reading them costs no Redis round-trip and does
// not depend on the tree's TTL still being alive.
func (m *SupplierManager) reactivateClaimedSession(
	ctx context.Context,
	supplier, sessionID string,
	e rebroadcastEntry,
) error {
	state, ok := m.suppliers.Load(supplier)
	if !ok || state.SessionCoordinator == nil {
		// Ownership moved between the reconcile pass and here. Returning an
		// error keeps the pending entry so the new owner still sees the
		// observation instead of it being cleared away by a replica that can
		// no longer act on it.
		return fmt.Errorf("supplier %s no longer owned by this replica", supplier)
	}

	var msg prooftypes.MsgCreateClaim
	if err := msg.Unmarshal(e.MsgBytes); err != nil {
		// A corrupt payload cannot be fixed by retrying, so this returns nil
		// and lets the entry clear. Error level, not Warn: the session stays
		// terminal with its claim on-chain, which is the slash this whole path
		// exists to prevent, and it is bounded by the defect existing.
		m.logger.Error().Err(err).
			Str(logging.FieldSupplier, supplier).
			Str(logging.FieldSessionID, sessionID).
			Msg("claim observed on-chain but its stored message will not unmarshal; session stays terminal and will be slashed")
		return nil
	}

	// The last hash we broadcast. Both fields are written from the same value
	// (lifecycle_callback.go:312-313) and only a rebroadcast ever changes
	// TxHash, so there is nothing for OrigTxHash to fall back to. The
	// reconciler reads inclusion from module state, not from this hash, so
	// treat it as provenance rather than as a verified on-chain tx.
	err := state.SessionCoordinator.OnClaimObservedOnChain(ctx, sessionID, msg.RootHash, e.TxHash)
	if errors.Is(err, ErrClaimRootUnusable) {
		// Permanent, exactly like a payload that will not unmarshal: retrying
		// cannot make a malformed root well-formed, and returning an error here
		// would keep the entry and re-fire this every block until its TTL. Same
		// level and same reasoning as the unmarshal case above.
		m.logger.Error().Err(err).
			Str(logging.FieldSupplier, supplier).
			Str(logging.FieldSessionID, sessionID).
			Msg("claim observed on-chain but its root is unusable; session stays terminal and will be slashed")
		return nil
	}
	return err
}

// ResubmitMessage implements MessageResubmitter: it routes a re-broadcast to the
// owning supplier's tx client. Returns an error (not a panic) for suppliers this
// replica does not control — the reconciler's ownership filter normally prevents
// reaching here for non-owned suppliers.
func (m *SupplierManager) ResubmitMessage(ctx context.Context, phase RebroadcastPhase, supplier string, msgBytes []byte, cached tx.SignedTxPayload, timeoutHeight int64, timeout time.Duration, regime string) (string, tx.SignedTxPayload, error) {
	state, ok := m.suppliers.Load(supplier)
	if !ok || state.SupplierClient == nil {
		return "", tx.SignedTxPayload{}, fmt.Errorf("no tx client for supplier %s (not owned by this replica)", supplier)
	}

	// The resend INHERITS the budget its original submission was born with; it
	// does not derive one. Recomputing here meant "the blocks left in the
	// window", which shrank on every attempt -- so the transaction replacing a
	// lost one carried LESS time than the one it replaced, precisely when more
	// was wanted. It cannot be recomputed correctly on this side anyway: the
	// window length is a chain parameter and this type has no access to shared
	// params.
	//
	// A missing budget means an entry written before the field existed, or one
	// an older binary rewrote and stripped. The ceiling is used, with the regime
	// saying so out loud -- counted by tx where a transaction is SIGNED, so a
	// re-injection, which replays a derivation already counted, counts nothing.
	//
	// THAT FALLBACK IS ONLY SAFE BECAUSE THE TRANSACTION CARRIES A
	// timeout_height. A timestamp longer than the window is inert once the chain
	// enforces a height: the height cuts first, at the close. Without it this
	// same fallback would hand a resend a deadline that OUTLIVES its own window
	// -- precisely the defect the window budget exists to prevent -- so a change
	// that stops setting timeout_height must revisit this line, not just the one
	// that sets it. The ceiling also sits below the SDK's 600 s limit, so it
	// cannot be rejected as an over-long unordered TTL either.
	resendTimeout, resendRegime := timeout, regime
	if resendTimeout <= 0 {
		resendTimeout, resendRegime = tx.WindowTimeout(0, 0)
	}
	ctx = tx.WithTxWindowTimeout(ctx, resendTimeout, resendRegime)

	// A resend must not queue for a broadcast permit. Its budget is the
	// reconciler's per-group timeout, and spending that budget waiting means
	// leaving without reaching the chain -- in saturation, which is exactly
	// when the resend exists. Failing fast costs one block: the payload stays
	// in the store and the reconciler runs again next block.
	ctx = tx.WithoutPermitWait(ctx)

	// RE-INJECT before signing anything.
	//
	// The same bytes carry the same unordered nonce, so the node recognises the
	// duplicate and discards it by itself; signing again would put a SECOND live
	// transaction for one claim into the gossip, and behind a load balancer each
	// attempt can land on a different node that never saw the others. Signing is
	// the fallback, not the default.
	if reusable(cached, state.SupplierClient.LatestBlockTime()) {
		hash, err := state.SupplierClient.BroadcastRawReturningHash(ctx, string(phase), cached)
		// The payload goes back unchanged: nothing was signed, so the entry must
		// keep exactly the bytes it already had.
		return hash, cached, err
	}

	switch phase {
	case RebroadcastPhaseClaim:
		var msg prooftypes.MsgCreateClaim
		if err := msg.Unmarshal(msgBytes); err != nil {
			return "", tx.SignedTxPayload{}, fmt.Errorf("unmarshal MsgCreateClaim: %w", err)
		}
		hash, signed, err := state.SupplierClient.CreateClaimsReturningHash(ctx, timeoutHeight, &msg)
		return hash, signed, err
	case RebroadcastPhaseProof:
		var msg prooftypes.MsgSubmitProof
		if err := msg.Unmarshal(msgBytes); err != nil {
			return "", tx.SignedTxPayload{}, fmt.Errorf("unmarshal MsgSubmitProof: %w", err)
		}
		if m.proofAlreadyJudged(ctx, supplier, msg.GetSessionHeader().GetSessionId()) {
			return "", tx.SignedTxPayload{}, ErrProofAlreadyJudged
		}
		hash, signed, err := state.SupplierClient.SubmitProofsReturningHash(ctx, timeoutHeight, &msg)
		return hash, signed, err
	default:
		return "", tx.SignedTxPayload{}, fmt.Errorf("unknown rebroadcast phase %q", phase)
	}
}

// ErrProofAlreadyJudged is a resend that would sign a NEW proof transaction for
// a claim whose proof the chain already validated or rejected. poktroll deletes
// a proof once its EndBlocker judges it and SubmitProof does not read the
// claim's verdict, so a second proof is accepted and charged again: the fee is
// the whole cost and nothing is gained.
var ErrProofAlreadyJudged = errors.New("the chain already judged this claim's proof")

// proofAlreadyJudged asks the chain, uncached, whether the claim's proof was
// already validated or rejected. It is asked only where a resend would SIGN a
// new transaction -- a re-injection carries the original nonce and cannot be
// charged twice. An unanswered question is not a verdict: the proof is sent,
// because a missed proof costs the claim and a duplicate costs one fee.
// GetClaim is not used here: it is cached for liveEntityCacheTTL, long enough
// to still answer "pending" for a proof validated since.
func (m *SupplierManager) proofAlreadyJudged(ctx context.Context, supplier, sessionID string) bool {
	if m.config.ProofQueryClient == nil || sessionID == "" {
		return false
	}
	states, err := m.config.ProofQueryClient.GetSupplierSessionStates(ctx, supplier)
	if err != nil {
		m.logger.Debug().Err(err).Str("session_id", sessionID).
			Msg("resend: could not read the proof's verdict before signing; sending it")
		return false
	}
	claim, ok := states[sessionID]
	return ok && (claim.ProofState == query.SessionProofValidated || claim.ProofState == query.SessionProofRejected)
}

// reusable answers whether the transaction an entry already carries is still
// worth sending again.
//
// The deadline check is the precondition the whole re-injection rests on and it
// cannot be skipped: a signed transaction seals its own timeout_timestamp, so
// past that instant the ante handler refuses it no matter how healthy
// everything else is. On mainnet the derived budget sits just under the SDK
// ceiling while the window is barely longer, so bytes dying BEFORE their window
// closes is the ordinary end of a window rather than an edge case.
//
// It compares against the CHAIN's clock, the same one that produced the
// deadline when this was signed. The wall clock disagrees with it exactly when
// the chain runs behind, which is when the answer matters most. A zero clock is
// UNKNOWN and must answer no: re-injecting on a guess spends the attempt on
// bytes the chain may already refuse, and the cost of the opposite mistake is
// one signature.
func reusable(cached tx.SignedTxPayload, chainNow time.Time) bool {
	if len(cached.Bytes) == 0 || cached.TimeoutAt.IsZero() {
		return false
	}
	if chainNow.IsZero() {
		return false
	}
	return chainNow.Before(cached.TimeoutAt)
}

// claimWindowClosedAt reports whether the claim window for a session ending at
// sessionEndHeight has already closed at the height this miner last observed.
// It answers FALSE whenever it cannot tell; the caller uses the answer to
// discard a relay, so an unknown must never cost one.
func (m *SupplierManager) claimWindowClosedAt(ctx context.Context, sessionEndHeight int64) bool {
	if m.config.BlockClient == nil || m.config.SharedClient == nil {
		return false
	}
	block := m.config.BlockClient.LastBlock(ctx)
	if block == nil {
		return false // no observed height: cannot tell, do not drop
	}
	currentHeight := block.Height()

	// Answer BEFORE any params query, and not for speed: this runs for every
	// relay whose session is not already known-terminal, so on a live fleet it
	// runs constantly with an end height in the FUTURE. GetParamsAtHeight has no
	// future-height guard (query/query.go:492) -- it would query, get today's
	// live params back, and cache them under a future height that the query
	// layer treats as immutable for thirty minutes. The lifecycle sweep reads
	// that same cache to compute a session's windows, so a governance change
	// landing mid-session would be masked there. A session that has not ended
	// cannot have a closed claim window, so the answer is already known.
	if sessionEndHeight >= currentHeight {
		return false
	}

	// Past its end height, so the at-height read is the immutable one it is
	// meant to be -- and it is the ONLY one that may answer here. Live params
	// were a fallback until 2026-08-28: if governance shortened the claim window
	// after this session ended, they report a window closed that was open at the
	// height that governs it, and the caller DISCARDS the relay on that answer.
	// That is this function's contract inverted, three lines above its own
	// signature. An at-height read that fails is an unknown, and an unknown
	// answers false -- the relay is admitted and, if it really is late, the
	// lifecycle sweep records it as claim_window_closed with the right reason.
	params, err := m.config.SharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
	if err != nil || params == nil {
		return false
	}
	return currentHeight >= sharedtypes.GetClaimWindowCloseHeight(params, sessionEndHeight)
}

// Why the entry drops a relay its session's claim no longer waits for. Closed is
// kept apart because a closed claim window is also one past the flush cap, and
// the live gate excuses a relay dropped past the close by that name.
const (
	claimWindowReasonOpen   = "claim_window_open"
	claimWindowReasonClosed = "claim_window_closed"
)

// claimWindowReached reports whether a relay for a session ending at
// sessionEndHeight arrives, at the height this miner last observed, too late
// for that session's claim, and names why: claimWindowReasonClosed once the
// claim window has closed, claimWindowReasonOpen from claimFlushCapBlocks past
// its opening, when the claim flush stops waiting for relays. Before that the
// relay is admitted, because the flush may still take it.
//
// It answers "" whenever it cannot tell, for the reason claimWindowClosedAt
// does: the caller discards on the answer. A params read that fails is counted,
// because an entry cut that silently stopped cutting looks the same as one with
// nothing late to cut.
//
// It reads no Redis: the height is the observed block, and the params come from
// the query client's height cache or one RPC shared by every caller asking for
// the same height.
func (m *SupplierManager) claimWindowReached(ctx context.Context, sessionEndHeight int64) string {
	if m.config.BlockClient == nil || m.config.SharedClient == nil || sessionEndHeight <= 0 {
		return ""
	}
	block := m.config.BlockClient.LastBlock(ctx)
	if block == nil {
		return ""
	}
	currentHeight := block.Height()

	// A session that has not ended cannot be past its claim window, and asking
	// for the params at a future height would cache today's params under it; see
	// claimWindowClosedAt.
	if sessionEndHeight >= currentHeight {
		return ""
	}

	params, err := m.config.SharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
	if err != nil || params == nil {
		RecordClaimWindowOpenUnchecked()
		return ""
	}
	switch {
	case currentHeight >= sharedtypes.GetClaimWindowCloseHeight(params, sessionEndHeight):
		return claimWindowReasonClosed
	case currentHeight >= sharedtypes.GetClaimWindowOpenHeight(params, sessionEndHeight)+claimFlushCapBlocks:
		return claimWindowReasonOpen
	}
	return ""
}
