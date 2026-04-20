package miner

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alitto/pond/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/pocket-relay-miner/tx"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

// SupplierQueryClient queries supplier information from the blockchain.
type SupplierQueryClient interface {
	GetSupplier(ctx context.Context, supplierOperatorAddress string) (sharedtypes.Supplier, error)
	GetParams(ctx context.Context) (*suppliertypes.Params, error)
	// InvalidateSupplier removes a supplier from the query cache so
	// the next GetSupplier call fetches fresh data from the chain.
	InvalidateSupplier(operatorAddress string)
}

// ErrDrainAborted is returned by onSupplierReleased when the drain is vetoed
// because the supplier is still staked on-chain. This tells the claimer to
// keep the Redis claim key alive instead of releasing it.
var ErrDrainAborted = errors.New("drain aborted: supplier still staked on-chain")

// SupplierStatus represents the state of a supplier in the miner.
type SupplierStatus int

const (
	// SupplierStatusActive means the supplier is actively processing relays.
	SupplierStatusActive SupplierStatus = iota
	// SupplierStatusDraining means the supplier is being removed but waiting for pending work.
	SupplierStatusDraining
)

// SupplierState holds the state for a single supplier in the miner.
//
// Status is stored atomically (int32) because consumeForSupplier reads
// it from the relay hot path while removeSupplier writes it under
// suppliersMu. Taking suppliersMu in the relay path would serialize
// every relay through the map mutex; using an atomic gives lock-free
// reads/writes with sequential consistency. Callers must use
// LoadStatus / StoreStatus — do not access `status` directly.
type SupplierState struct {
	OperatorAddr string
	Services     []string
	status       atomic.Int32

	// Redis stream consumer for this supplier
	Consumer *redistransport.StreamsConsumer

	// Session management
	SessionStore       *RedisSessionStore
	SessionCoordinator *SessionCoordinator

	// SMST management (for building and managing session trees)
	SMSTManager *RedisSMSTManager

	// Lifecycle management (for claim/proof submission with timing spread)
	LifecycleManager  *SessionLifecycleManager
	LifecycleCallback *LifecycleCallback
	SupplierClient    *tx.HASupplierClient

	// Pending work tracking
	ActiveSessions int
	PendingClaims  int
	PendingProofs  int

	// Lifecycle
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
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
// a drain) should hold suppliersMu for the composite update but must
// still use StoreStatus so concurrent LoadStatus readers observe a
// well-defined transition.
func (s *SupplierState) StoreStatus(status SupplierStatus) {
	s.status.Store(int32(status))
}

// SupplierManagerConfig contains configuration for the SupplierManager.
type SupplierManagerConfig struct {
	// Redis connection
	RedisClient *redistransport.Client

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
	BatchSize    int64 // Number of messages to fetch per XREADGROUP
	AckBatchSize int64 // Number of messages to ACK in a batch

	// Redis stream configuration
	// Note: Stream consumption uses BLOCK 0 (TRUE PUSH) for live consumption - not configurable
	ClaimIdleTimeout time.Duration // How long a message can be pending before being claimed

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

	// BlockClient for monitoring block heights (claim/proof timing)
	BlockClient client.BlockClient

	// SharedClient for querying shared parameters (claim/proof windows)
	SharedClient client.SharedQueryClient

	// SessionClient for querying session information
	SessionClient SessionQueryClient

	// ProofChecker determines if a proof is required for a claimed session.
	// If nil, proofs are always submitted (legacy behavior).
	ProofChecker *ProofRequirementChecker

	// ServiceFactorProvider provides service factor configuration for claim ceiling warnings.
	// If nil, no ceiling warnings are logged.
	ServiceFactorProvider ServiceFactorProvider

	// AppClient queries application data for claim ceiling calculations.
	// If nil, ceiling warnings are skipped.
	AppClient ApplicationQueryClient

	// SessionLifecycleConfig contains configuration for session lifecycle management.
	SessionLifecycleConfig SessionLifecycleConfig

	// WorkerPool is the master worker pool shared across all concurrent operations.
	// MUST be set by caller. Should be limited to runtime.NumCPU().
	// Subpools will be created from this for different workloads.
	WorkerPool pond.Pool

	// ClaimerConfig contains configuration for the SupplierClaimer.
	// Used for distributed supplier claiming across multiple miners via Redis leases.
	ClaimerConfig SupplierClaimerConfig

	// DisableClaimBatching disables batching of claim submissions.
	// WORKAROUND: Set to true to avoid cross-contamination where one invalid claim
	// causes the entire batch to fail.
	DisableClaimBatching bool

	// DisableProofBatching disables batching of proof submissions.
	// WORKAROUND: Set to true to avoid cross-contamination where one invalid proof
	// (e.g., difficulty validation failure) causes the entire batch to fail.
	DisableProofBatching bool

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
}

// DefaultSupplierReconcileInterval is the default polling cadence for the
// on-chain stake reconciler.
const DefaultSupplierReconcileInterval = 60 * time.Second

// SupplierManager manages multiple suppliers in the HA Miner.
// It handles dynamic addition/removal of suppliers based on key changes.
type SupplierManager struct {
	logger     logging.Logger
	config     SupplierManagerConfig
	keyManager keys.KeyManager
	registry   *SupplierRegistry

	// Per-supplier state
	suppliers   map[string]*SupplierState
	suppliersMu sync.RWMutex

	// Message processing callback
	onRelay func(ctx context.Context, supplierAddr string, msg *transport.StreamMessage) error

	// Pond subpool for bounded supplier queries (prevents unbounded goroutine spawning)
	querySubpool pond.Pool

	// Distributed claiming (optional)
	claimer *SupplierClaimer

	// Deduplicator (shared across suppliers). Prevents counter drift when Redis
	// Streams redeliver a relay (consumer reclaim, transient ack failure).
	deduplicator Deduplicator

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
		suppliers:    make(map[string]*SupplierState),
		querySubpool: querySubpool,
	}

	// Construct a shared deduplicator if we have a Redis client. Falls back to
	// nil if Redis is absent (e.g. tests) — handleRelay treats nil as fail-open.
	if config.RedisClient != nil {
		// KeyPrefix empty → defaults to "ha:miner:dedup" (matches KeyBuilder.MinerDedupKey).
		mgr.deduplicator = NewRedisDeduplicator(
			componentLogger,
			config.RedisClient,
			DeduplicatorConfig{},
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
	m.claimer.UpdateSuppliers(m.filterStakedSuppliers(ctx, m.keyManager.ListSuppliers()))
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
// Each supplier holds 1 connection indefinitely for BLOCK 0 stream consumption.
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
			if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
				// Write NOT STAKED status to Redis cache for visibility
				m.writeSupplierStatusToCache(ctx, addr, false, nil)

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
			// Network/timeout error — fail-open: treat as staked to avoid false drains
			m.logger.Warn().
				Err(err).
				Str("address", addr).
				Msg("failed to query supplier status, treating as staked (fail-open)")
			stakedSuppliers = append(stakedSuppliers, addr)
			continue
		}

		// Supplier is staked. Resolve services using the ServiceConfigHistory-
		// aware helper so we respect activation_height / deactivation_height
		// scheduled by MsgStakeSupplier updates and MsgUnstakeSupplier. The
		// denormalized supplier.Services field cuts too fast: poktroll
		// schedules deactivations at the next session_end, not immediately,
		// so a service removed mid-session must keep serving relays until
		// its deactivation_height is reached. Same logic in reverse for
		// services with a future activation_height.
		var currentHeight int64
		if m.config.BlockClient != nil {
			if block := m.config.BlockClient.LastBlock(ctx); block != nil {
				currentHeight = block.Height()
			}
		}
		activeConfigs := supplier.GetActiveServiceConfigs(currentHeight)
		services := make([]string, 0, len(activeConfigs))
		for _, svc := range activeConfigs {
			if svc != nil {
				services = append(services, svc.ServiceId)
			}
		}
		m.writeSupplierStatusToCache(ctx, addr, true, services)
		stakedSuppliers = append(stakedSuppliers, addr)
	}

	m.logger.Debug().
		Int("staked", len(stakedSuppliers)).
		Int("not_staked", unstakedCount).
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
			KeyPrefix:       m.config.RedisClient.KB().MinerSessionsPrefix(),
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
func (m *SupplierManager) writeSupplierStatusToCache(ctx context.Context, addr string, staked bool, services []string) {
	if m.config.SupplierCache == nil {
		return
	}

	status := cache.SupplierStatusNotStaked
	if staked {
		status = cache.SupplierStatusActive
	}

	state := &cache.SupplierState{
		Status:          status,
		Staked:          staked,
		OperatorAddress: addr,
		Services:        services,
		UpdatedBy:       m.config.MinerID,
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

	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
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

	// Check if we already have this supplier
	m.suppliersMu.RLock()
	_, exists := m.suppliers[supplier]
	m.suppliersMu.RUnlock()
	if exists {
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
// It verifies on-chain staking status before draining to prevent false drains.
func (m *SupplierManager) onSupplierReleased(ctx context.Context, supplier string) error {
	shouldDrain, verifyResult := m.verifySupplierUnstaked(ctx, supplier, "rebalance_release")
	supplierDrainDecisionTotal.WithLabelValues("rebalance_release", verifyResult).Inc()

	m.logger.Info().
		Str("supplier", supplier).
		Str("drain_trigger", "rebalance_release").
		Str("on_chain_result", verifyResult).
		Bool("drain_aborted", !shouldDrain).
		Str("instance_id", m.config.MinerID).
		Msg("drain decision audit")

	if !shouldDrain {
		m.logger.Warn().
			Str("supplier", supplier).
			Str("on_chain_result", verifyResult).
			Str("instance_id", m.config.MinerID).
			Msg("DRAIN ABORTED: supplier is staked on-chain, keeping claim")
		return ErrDrainAborted
	}

	// Remove the supplier (with drain)
	m.removeSupplier(supplier)
	return nil
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

	services := make([]string, 0, len(chainSupplier.Services))
	for _, svc := range chainSupplier.Services {
		services = append(services, svc.ServiceId)
	}

	return &SupplierWarmupData{
		OwnerAddress: chainSupplier.OwnerAddress,
		Services:     services,
	}
}

// addSupplierWithHandoff adds a supplier with handoff validation.
// This logs the inherited sessions and validates SMST state.
func (m *SupplierManager) addSupplierWithHandoff(ctx context.Context, supplier string, warmupData *SupplierWarmupData) error {
	// First, load existing sessions from Redis to validate handoff
	sessionStore := NewRedisSessionStore(
		m.logger,
		m.config.RedisClient,
		SessionStoreConfig{
			KeyPrefix:       m.config.RedisClient.KB().MinerSessionsPrefix(),
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
			// Sessions that have already been claimed/proved don't need SMST anymore.
			// The SMST was deleted after the root hash was computed and submitted.
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
	OwnerAddress string
	Services     []string
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
				if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
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
		if m.claimer != nil {
			allSuppliers := m.keyManager.ListSuppliers()
			stakedSuppliers := m.filterStakedSuppliers(ctx, allSuppliers)
			m.claimer.UpdateSuppliers(stakedSuppliers)
		}

		go m.removeSupplier(operatorAddr)
	}
}

// addSupplierWithData adds a new supplier to the manager with optional pre-warmed data.
// If prewarmedData is nil, it will query fresh data from the chain.
func (m *SupplierManager) addSupplierWithData(ctx context.Context, operatorAddr string, prewarmedData *SupplierWarmupData) error {
	m.suppliersMu.Lock()
	defer m.suppliersMu.Unlock()

	// Check if already exists
	if _, exists := m.suppliers[operatorAddr]; exists {
		return nil // Already added
	}

	// Create supplier-specific context
	supplierCtx, cancelFn := context.WithCancel(ctx)

	// Create session store for this supplier
	sessionStore := NewRedisSessionStore(
		m.logger,
		m.config.RedisClient,
		SessionStoreConfig{
			KeyPrefix:       m.config.RedisClient.KB().MinerSessionsPrefix(),
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
			MaxRetries:              3,
			// Note: Uses BLOCK 0 (TRUE PUSH) for live consumption - hardcoded in consumer
		},
		0, // Discovery interval ignored with single stream architecture
	)
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
		},
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
		lifecycleCallbackConfig.DisableClaimBatching = m.config.DisableClaimBatching
		lifecycleCallbackConfig.DisableProofBatching = m.config.DisableProofBatching
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

		// Wire optional providers for claim ceiling warnings
		if m.config.ServiceFactorProvider != nil {
			lifecycleCallback.SetServiceFactorProvider(m.config.ServiceFactorProvider)
		}
		if m.config.AppClient != nil {
			lifecycleCallback.SetAppClient(m.config.AppClient)
		}

		// Wire the SMST as the claimed-root rehydration source for the
		// proof-requirement check. Under HA failover the snapshot's
		// ClaimedRootHash can be nil (OnSessionClaimed's Redis write
		// failed and was only logged at Warn); without this provider
		// IsProofRequired would fabricate a proof from a nil root.
		if m.config.ProofChecker != nil {
			m.config.ProofChecker.SetClaimedRootProvider(smstManager)
		}

		// Wire stream deleter for cleanup after session settlement
		// This stops the consumer from reading stale messages and frees Redis memory
		lifecycleCallback.SetStreamDeleter(consumer)

		// Wire submission tracker for debugging claim/proof submissions
		// Tracks tx hashes, success/failure, errors, and timing for post-mortem analysis
		submissionTracker := NewSubmissionTracker(m.logger, m.config.RedisClient, m.config.SubmissionTrackingTTL)
		lifecycleCallback.SetSubmissionTracker(submissionTracker)

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

		// Start lifecycle manager
		if startErr := lifecycleManager.Start(supplierCtx); startErr != nil {
			m.logger.Warn().
				Err(startErr).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to start lifecycle manager, continuing without lifecycle management")
			lifecycleManager = nil
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
		LifecycleManager:   lifecycleManager,
		LifecycleCallback:  lifecycleCallback,
		SupplierClient:     supplierClient,
		cancelFn:           cancelFn,
	}
	state.StoreStatus(SupplierStatusActive)

	m.suppliers[operatorAddr] = state

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

	// Get supplier's staked services - use pre-warmed data if available, otherwise query fresh
	var services []string
	var ownerAddr string

	if prewarmedData != nil {
		// Use pre-warmed data from parallel warmup (already fresh from chain)
		ownerAddr = prewarmedData.OwnerAddress
		services = prewarmedData.Services
		m.logger.Debug().
			Str(logging.FieldSupplier, operatorAddr).
			Int("services", len(services)).
			Msg("using pre-warmed supplier data")
	} else if m.config.SupplierQueryClient != nil {
		// No pre-warmed data, query fresh from chain (hot-reload case)
		supplier, queryErr := m.config.SupplierQueryClient.GetSupplier(ctx, operatorAddr)
		if queryErr != nil {
			// Distinguish between "not found" (unstaked, expected) vs actual errors
			if st, ok := status.FromError(queryErr); ok && st.Code() == codes.NotFound {
				// Expected: supplier key loaded but not yet staked on-chain
				m.logger.Debug().
					Str(logging.FieldSupplier, operatorAddr).
					Msg("supplier not staked on-chain yet (pre-loaded key)")
			} else {
				// Unexpected: network or other errors
				m.logger.Warn().
					Err(queryErr).
					Str(logging.FieldSupplier, operatorAddr).
					Msg("failed to query supplier from blockchain")
			}
		} else {
			ownerAddr = supplier.OwnerAddress
			for _, svc := range supplier.Services {
				if svc != nil {
					services = append(services, svc.ServiceId)
				}
			}
			m.logger.Debug().
				Str(logging.FieldSupplier, operatorAddr).
				Str("services", fmt.Sprintf("%v", services)).
				Msg("queried supplier services from blockchain")
		}
	}
	state.Services = services

	// Publish supplier state to cache for relayers to read
	if m.config.SupplierCache != nil {
		supplierState := &cache.SupplierState{
			Status:          cache.SupplierStatusActive,
			Staked:          true,
			OperatorAddress: operatorAddr,
			OwnerAddress:    ownerAddr,
			Services:        services,
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
	}

	supplierManagerSuppliersActive.Inc()

	m.logger.Debug().
		Str(logging.FieldSupplier, operatorAddr).
		Msg("supplier added and consuming")

	return nil
}

// consumeForSupplier runs the consume loop for a single supplier with immediate ACK.
// Each message is ACK'd immediately after successful processing to prevent race conditions
// with XAUTOCLAIM reclaiming messages that were already processed but not yet ACK'd.
//
// Belt-and-suspenders defense: even though every SMT boundary inside
// handleRelay is wrapped with runSMSTSafely, any panic from unrelated
// code paths (nil pointer, map corruption, pool misuse) must not kill
// this consumer goroutine — losing it stops every relay for the
// supplier until a restart. We cannot use logging.RecoverGoRoutine
// directly because this loop must keep running after a single relay
// panics, not exit. Instead we recover *per iteration* below.
func (m *SupplierManager) consumeForSupplier(ctx context.Context, state *SupplierState) {
	defer state.wg.Done()

	defer func() {
		if r := recover(); r != nil {
			logging.PanicRecoveriesTotal.WithLabelValues("supplier_consume_loop").Inc()
			m.logger.Error().
				Str(logging.FieldSupplier, state.OperatorAddr).
				Str("panic_value", fmt.Sprintf("%v", r)).
				Str("stack_trace", string(debug.Stack())).
				Msg("PANIC RECOVERED in consumeForSupplier — consumer goroutine would have died")
		}
	}()

	msgChan := state.Consumer.Consume(ctx)

	for {
		select {
		case msg, ok := <-msgChan:
			if !ok {
				// Channel closed, exit
				return
			}

			// Track relay consumed from Redis Stream (relayer → miner)
			RecordRelayConsumedFromStream(state.OperatorAddr, msg.Message.ServiceId)

			// When draining, we continue processing existing messages
			// but log that we're in drain mode for visibility.
			// LoadStatus is lock-free — removeSupplier publishes the
			// draining flag via StoreStatus under suppliersMu, and
			// this read stays off the map mutex on the hot path.
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
						processErr = fmt.Errorf("panic recovered in relay processing: %v", r)
					}
				}()
				if m.onRelay != nil {
					startTime := time.Now()
					processErr = m.onRelay(ctx, state.OperatorAddr, &msg)
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
				m.logger.Warn().
					Err(processErr).
					Str(logging.FieldSupplier, state.OperatorAddr).
					Str("session_id", sessionID).
					Msg("failed to process relay")
				// Don't ACK on processing failure - let XAUTOCLAIM retry
				continue
			}

			// ACK immediately after successful processing
			// This prevents race conditions where XAUTOCLAIM reclaims already-processed messages
			if err := state.Consumer.AckMessage(ctx, msg); err != nil {
				m.logger.Warn().
					Err(err).
					Str(logging.FieldSupplier, state.OperatorAddr).
					Str("message_id", msg.ID).
					Msg("failed to acknowledge message")
			}

		case <-ctx.Done():
			return
		}
	}
}

// removeSupplier gracefully removes a supplier (waits for pending work).
//
// Drain-window semantics (post commit 8eb604c):
//
//  1. We cancel the supplier's context (state.cancelFn) BEFORE removing
//     it from m.suppliers. This ordering is deliberate: the cancel
//     signal must reach every in-flight handleRelay/UpdateTree before
//     the map delete, otherwise a goroutine that still holds a pointer
//     to SupplierState could race with cleanup below (Consumer.Close,
//     SMSTManager.Close, etc.).
//
//  2. Between cancelFn() firing and the map delete, a concurrent
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
func (m *SupplierManager) removeSupplier(operatorAddr string) {
	m.suppliersMu.Lock()
	state, exists := m.suppliers[operatorAddr]
	if !exists {
		m.suppliersMu.Unlock()
		return
	}

	// Mark as draining and copy services while holding lock
	state.StoreStatus(SupplierStatusDraining)
	servicesCopy := make([]string, len(state.Services))
	copy(servicesCopy, state.Services)
	m.suppliersMu.Unlock()

	// Publish draining status to registry
	if m.registry != nil {
		if err := m.registry.PublishSupplierUpdate(m.ctx, SupplierUpdateActionDraining, operatorAddr, nil); err != nil {
			m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("failed to publish draining status")
		}
	}

	// Update cache to mark supplier as unstaking (use copied services to avoid race)
	if m.config.SupplierCache != nil {
		supplierState := &cache.SupplierState{
			Status:          cache.SupplierStatusUnstaking,
			Staked:          true, // Still staked, just unstaking
			OperatorAddress: operatorAddr,
			Services:        servicesCopy,
			UpdatedBy:       m.config.MinerID,
		}
		if cacheErr := m.config.SupplierCache.SetSupplierState(m.ctx, supplierState); cacheErr != nil {
			m.logger.Warn().
				Err(cacheErr).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to update supplier state to unstaking in cache")
		}
	}

	m.logger.Info().
		Str(logging.FieldSupplier, operatorAddr).
		Msg("supplier marked as draining, waiting for pending work...")

	// Wait for pending work (TODO: implement proper tracking)
	// For now, just wait for consumer to finish
	state.cancelFn()
	state.wg.Wait()

	// Cleanup
	m.suppliersMu.Lock()
	defer m.suppliersMu.Unlock()

	// Close lifecycle manager first to stop monitoring
	if state.LifecycleManager != nil {
		if err := state.LifecycleManager.Close(); err != nil {
			m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("error closing lifecycle manager")
		}
	}

	// Close SMST manager
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

	delete(m.suppliers, operatorAddr)

	// Only remove from registry and cache if no other miner has already claimed
	// this supplier. During rebalance, miner1 may release a supplier that miner2
	// has already claimed and registered — deleting here would clobber miner2's entries.
	claimKey := m.config.RedisClient.KB().MinerClaimKey(operatorAddr)
	claimOwner, claimErr := m.config.RedisClient.Get(m.ctx, claimKey).Result()
	reclaimedByOther := claimErr == nil && claimOwner != "" && claimOwner != m.config.MinerID

	if reclaimedByOther {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Str("new_owner", claimOwner).
			Msg("supplier already reclaimed by another miner, skipping registry/cache cleanup")
	} else {
		// Publish removal to registry
		if m.registry != nil {
			if err := m.registry.PublishSupplierUpdate(m.ctx, SupplierUpdateActionRemove, operatorAddr, nil); err != nil {
				m.logger.Warn().Err(err).Str(logging.FieldSupplier, operatorAddr).Msg("failed to publish removal status")
			}
		}

		// Delete supplier from cache
		if m.config.SupplierCache != nil {
			if cacheErr := m.config.SupplierCache.DeleteSupplierState(m.ctx, operatorAddr); cacheErr != nil {
				m.logger.Warn().
					Err(cacheErr).
					Str(logging.FieldSupplier, operatorAddr).
					Msg("failed to delete supplier state from cache")
			}
		}
	}

	supplierManagerSuppliersActive.Dec()

	m.logger.Info().
		Str(logging.FieldSupplier, operatorAddr).
		Msg("supplier gracefully removed")
}

// GetSupplierState returns the state for a specific supplier.
func (m *SupplierManager) GetSupplierState(operatorAddr string) (*SupplierState, bool) {
	m.suppliersMu.RLock()
	defer m.suppliersMu.RUnlock()

	state, ok := m.suppliers[operatorAddr]
	return state, ok
}

// ListSuppliers returns all active supplier addresses.
func (m *SupplierManager) ListSuppliers() []string {
	m.suppliersMu.RLock()
	defer m.suppliersMu.RUnlock()

	suppliers := make([]string, 0, len(m.suppliers))
	for addr := range m.suppliers {
		suppliers = append(suppliers, addr)
	}
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

	// Stop the claimer first (releases all claims)
	if m.claimer != nil {
		if err := m.claimer.Stop(context.Background()); err != nil {
			m.logger.Warn().Err(err).Msg("failed to stop supplier claimer")
		}
	}

	// Wait for all suppliers to finish
	m.suppliersMu.Lock()
	for _, state := range m.suppliers {
		state.cancelFn()
		state.wg.Wait()

		// Close lifecycle manager first
		if state.LifecycleManager != nil {
			_ = state.LifecycleManager.Close()
		}

		// Close SMST manager
		if state.SMSTManager != nil {
			_ = state.SMSTManager.Close()
		}

		_ = state.Consumer.Close()
		_ = state.SessionCoordinator.Close()
		_ = state.SessionStore.Close()
	}
	m.suppliers = make(map[string]*SupplierState)
	m.suppliersMu.Unlock()

	// Stop query subpool gracefully (drains queued tasks)
	if m.querySubpool != nil {
		m.querySubpool.StopAndWait()
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
	m.suppliersMu.RLock()
	suppliers := make([]*SupplierState, 0, len(m.suppliers))
	for _, state := range m.suppliers {
		suppliers = append(suppliers, state)
	}
	m.suppliersMu.RUnlock()

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
	// Every task submitted above absorbs its own error and returns nil
	// (failures are logged inline per-supplier and the outer loop must not
	// abort the remaining trims for one bad supplier). As a result
	// group.Wait() is invariant-nil, and the explicit `_ =` discards the
	// zero-value interface by design. If you change a submitted task to
	// propagate an error, replace this with a Warn/Debug log of the Wait
	// result rather than silently dropping it.
	_ = group.Wait()

	if totalTrimmed > 0 {
		m.logger.Info().
			Int64("total_trimmed", totalTrimmed).
			Int("suppliers_trimmed", trimmedSuppliers).
			Int("total_suppliers", len(suppliers)).
			Dur("max_age", maxAge).
			Msg("stream trimming completed")
	}
}
