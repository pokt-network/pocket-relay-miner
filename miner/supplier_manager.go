package miner

import (
	"context"
	"fmt"
	"sync"
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
}

// SupplierStatus represents the state of a supplier in the miner.
type SupplierStatus int

const (
	// SupplierStatusActive means the supplier is actively processing relays.
	SupplierStatusActive SupplierStatus = iota
	// SupplierStatusDraining means the supplier is being removed but waiting for pending work.
	SupplierStatusDraining
)

// SupplierState holds the state for a single supplier in the miner.
type SupplierState struct {
	OperatorAddr string
	Services     []string
	Status       SupplierStatus

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
}

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

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	closed   bool
	mu       sync.Mutex
}

// NewSupplierManager creates a new supplier manager.
func NewSupplierManager(
	logger logging.Logger,
	keyManager keys.KeyManager,
	registry *SupplierRegistry,
	config SupplierManagerConfig,
) *SupplierManager {
	// Create subpool for bounded supplier queries (prevents system overwhelm)
	// Limit to 20 concurrent queries to avoid spawning 100+ goroutines
	querySubpool := config.WorkerPool.NewSubpool(20)

	return &SupplierManager{
		logger:       logging.ForComponent(logger, logging.ComponentSupplierManager),
		config:       config,
		keyManager:   keyManager,
		registry:     registry,
		suppliers:    make(map[string]*SupplierState),
		querySubpool: querySubpool,
	}
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

	// Get all supplier addresses from key manager
	supplierAddrs := m.keyManager.ListSuppliers()
	if len(supplierAddrs) == 0 {
		m.logger.Info().Msg("no suppliers to initialize")
		return nil
	}

	// Start with distributed claiming (ONLY mode - each miner claims fair share via Redis leases)
	return m.startWithDistributedClaiming(ctx, supplierAddrs)
}

// startWithDistributedClaiming starts the manager with distributed supplier claiming.
// Suppliers are claimed via Redis leases and distributed fairly across miners.
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

	if len(stakedSuppliers) == 0 {
		m.logger.Warn().
			Int("total_keys", len(supplierAddrs)).
			Msg("no staked suppliers found - nothing to claim")
		return nil
	}

	// Create the claimer
	m.claimer = NewSupplierClaimer(
		m.logger,
		m.config.RedisClient,
		m.config.MinerID,
		m.config.ClaimerConfig,
	)

	// Set callbacks for claim/release events
	m.claimer.SetCallbacks(
		m.onSupplierClaimed,
		m.onSupplierReleased,
	)

	// Start the claimer with only staked suppliers
	if err := m.claimer.Start(ctx, stakedSuppliers); err != nil {
		return fmt.Errorf("failed to start supplier claimer: %w", err)
	}

	m.logger.Info().
		Int("claimed", m.claimer.ClaimedCount()).
		Int("staked_suppliers", len(stakedSuppliers)).
		Bool("distributed_claiming", true).
		Msg("supplier manager started with distributed claiming")

	// Check if pool size is sufficient for the number of suppliers
	m.checkPoolSize(len(stakedSuppliers))

	return nil
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
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		supplier, err := m.config.SupplierQueryClient.GetSupplier(queryCtx, addr)
		cancel()

		if err != nil {
			// Check if it's a NotFound error (not staked)
			if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
				m.logger.Debug().
					Str("address", addr).
					Msg("skipping non-staked address (not a supplier on-chain)")

				// Write NOT STAKED status to Redis cache for visibility
				m.writeSupplierStatusToCache(ctx, addr, false, nil)
				unstakedCount++
				continue
			}
			// Other errors (network, timeout) - log warning but skip to be safe
			m.logger.Warn().
				Err(err).
				Str("address", addr).
				Msg("failed to query supplier status, skipping")
			continue
		}

		// Supplier is staked - write to cache and include in result
		services := make([]string, 0, len(supplier.Services))
		for _, svc := range supplier.Services {
			services = append(services, svc.ServiceId)
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
// It drains the supplier and stops its lifecycle.
func (m *SupplierManager) onSupplierReleased(ctx context.Context, supplier string) error {
	m.logger.Info().
		Str("supplier", supplier).
		Msg("released supplier, starting drain")

	// Remove the supplier (with drain)
	m.removeSupplier(supplier)
	return nil
}

// warmupSingleSupplier queries chain data for a single supplier.
func (m *SupplierManager) warmupSingleSupplier(ctx context.Context, supplier string) *SupplierWarmupData {
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

		// Validate each session's SMST exists
		for _, session := range sessions {
			smstKey := m.config.RedisClient.KB().SMSTNodesKey(session.SessionID)
			exists, _ := m.config.RedisClient.Exists(ctx, smstKey).Result()

			m.logger.Debug().
				Str("supplier", supplier).
				Str("session_id", session.SessionID).
				Bool("smst_exists", exists > 0).
				Int64("relay_count", session.RelayCount).
				Str("state", string(session.State)).
				Msg("validating session SMST during handoff")

			if exists == 0 && session.RelayCount > 0 {
				m.logger.Error().
					Str("supplier", supplier).
					Str("session_id", session.SessionID).
					Int64("relay_count", session.RelayCount).
					Msg("HANDOFF WARNING: SMST missing but relay count > 0")
			}
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

// onKeyChange handles key addition/removal notifications.
func (m *SupplierManager) onKeyChange(operatorAddr string, added bool) {
	if added {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("key added via hot-reload")

		// Check if supplier is staked on-chain before processing
		if m.config.SupplierQueryClient != nil {
			queryCtx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			_, err := m.config.SupplierQueryClient.GetSupplier(queryCtx, operatorAddr)
			cancel()

			if err != nil {
				// Check if it's a NotFound error (not staked)
				if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
					m.logger.Debug().
						Str("address", operatorAddr).
						Msg("skipping hot-reloaded key - not staked as supplier on-chain")
					return
				}
				// Other errors - log warning but skip to be safe
				m.logger.Warn().
					Err(err).
					Str("address", operatorAddr).
					Msg("failed to query supplier status for hot-reloaded key, skipping")
				return
			}
		}

		// Supplier is staked - proceed with adding
		if m.claimer != nil {
			// Distributed claiming mode: update claimer's supplier list
			// The claimer will handle claiming via rebalance
			allSuppliers := m.keyManager.ListSuppliers()
			stakedSuppliers := m.filterStakedSuppliers(m.ctx, allSuppliers)
			m.claimer.UpdateSuppliers(stakedSuppliers)
			m.logger.Debug().
				Str(logging.FieldSupplier, operatorAddr).
				Int("total_staked", len(stakedSuppliers)).
				Msg("updated claimer with hot-reloaded staked supplier")
		} else {
			// Single-miner mode: add directly
			if err := m.addSupplierWithData(m.ctx, operatorAddr, nil); err != nil {
				m.logger.Error().
					Err(err).
					Str(logging.FieldSupplier, operatorAddr).
					Msg("failed to add supplier")
			}
		}
	} else {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("key removed, draining supplier")

		// Update claimer if in distributed mode
		if m.claimer != nil {
			allSuppliers := m.keyManager.ListSuppliers()
			stakedSuppliers := m.filterStakedSuppliers(m.ctx, allSuppliers)
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
			SupplierAddress: operatorAddr,
			CacheTTL:        m.config.CacheTTL,
		},
	)

	// NOTE: SMST warmup removed - trees are lazy-loaded on first relay via GetOrCreateTree()
	// The SMT library lazy-loads nodes from Redis, so warmup only created empty shell objects.
	// Lazy loading is instant (no SCAN overhead) and reduces startup time by 10-20 minutes.
	// HA failover works identically: new instance reads SMST data from Redis on first relay.
	m.logger.Debug().
		Str(logging.FieldSupplier, operatorAddr).
		Msg("SMST manager ready (will lazy-load trees on first relay)")

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

		// Wire stream deleter for cleanup after session settlement
		// This stops the consumer from reading stale messages and frees Redis memory
		lifecycleCallback.SetStreamDeleter(consumer)

		// Wire submission tracker for debugging claim/proof submissions
		// Tracks tx hashes, success/failure, errors, and timing for post-mortem analysis
		submissionTracker := NewSubmissionTracker(m.logger, m.config.RedisClient)
		lifecycleCallback.SetSubmissionTracker(submissionTracker)

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
			consumer,            // Pass consumer as PendingRelayChecker for late relay detection
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
			m.logger.Debug().
				Str(logging.FieldSupplier, operatorAddr).
				Msg("wired session creation callback to lifecycle manager")
		}
	} else {
		m.logger.Warn().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("lifecycle management disabled - TxClient, BlockClient, or SharedClient not configured")
	}

	state := &SupplierState{
		OperatorAddr:       operatorAddr,
		Status:             SupplierStatusActive,
		Consumer:           consumer,
		SessionStore:       sessionStore,
		SessionCoordinator: sessionCoordinator,
		SMSTManager:        smstManager,
		LifecycleManager:   lifecycleManager,
		LifecycleCallback:  lifecycleCallback,
		SupplierClient:     supplierClient,
		cancelFn:           cancelFn,
	}

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
func (m *SupplierManager) consumeForSupplier(ctx context.Context, state *SupplierState) {
	defer state.wg.Done()

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
			// but log that we're in drain mode for visibility
			if state.Status == SupplierStatusDraining {
				m.logger.Debug().
					Str(logging.FieldSupplier, state.OperatorAddr).
					Msg("processing relay during drain")
			}

			// Process the relay (adds to SMST tree)
			if m.onRelay != nil {
				startTime := time.Now()
				if err := m.onRelay(ctx, state.OperatorAddr, &msg); err != nil {
					m.logger.Warn().
						Err(err).
						Str(logging.FieldSupplier, state.OperatorAddr).
						Str("session_id", msg.Message.SessionId).
						Msg("failed to process relay")
					// Record processing latency even on failure
					RecordRelayProcessingLatency(state.OperatorAddr, msg.Message.ServiceId, "error", time.Since(startTime).Seconds())
					// Don't ACK on processing failure - let XAUTOCLAIM retry
					continue
				}
				// Record processing latency on success
				RecordRelayProcessingLatency(state.OperatorAddr, msg.Message.ServiceId, "success", time.Since(startTime).Seconds())
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
func (m *SupplierManager) removeSupplier(operatorAddr string) {
	m.suppliersMu.Lock()
	state, exists := m.suppliers[operatorAddr]
	if !exists {
		m.suppliersMu.Unlock()
		return
	}

	// Mark as draining and copy services while holding lock
	state.Status = SupplierStatusDraining
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
		_ = state.LifecycleManager.Close()
	}

	// Close SMST manager
	if state.SMSTManager != nil {
		_ = state.SMSTManager.Close()
	}

	_ = state.Consumer.Close()
	_ = state.SessionCoordinator.Close()
	_ = state.SessionStore.Close()

	delete(m.suppliers, operatorAddr)

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
