package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	pond "github.com/alitto/pond/v2"
	"github.com/redis/go-redis/v9"
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
)

// SupplierQueryClient queries supplier information from the blockchain.
type SupplierQueryClient interface {
	GetSupplier(ctx context.Context, supplierOperatorAddress string) (sharedtypes.Supplier, error)
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
	SessionStore    *RedisSessionStore
	WAL             *RedisWAL
	SnapshotManager *SMSTSnapshotManager

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
	RedisClient redis.UniversalClient

	// Stream configuration
	StreamPrefix  string
	ConsumerGroup string
	ConsumerName  string

	// Session configuration
	SessionTTL time.Duration

	// WAL configuration
	WALMaxLen int64

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

	// SessionLifecycleConfig contains configuration for session lifecycle management.
	SessionLifecycleConfig SessionLifecycleConfig

	// WorkerPool is the master worker pool shared across all concurrent operations.
	// MUST be set by caller. Should be limited to runtime.NumCPU().
	// Subpools will be created from this for different workloads.
	WorkerPool pond.Pool
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

	// Phase 1: Parallel warmup - query all supplier data from chain concurrently
	startTime := time.Now()
	supplierData := m.warmupSuppliersParallel(ctx, supplierAddrs)
	warmupDuration := time.Since(startTime)

	m.logger.Info().
		Int("total_suppliers", len(supplierAddrs)).
		Int("warmed_suppliers", len(supplierData)).
		Int64("warmup_ms", warmupDuration.Milliseconds()).
		Msg("parallel supplier warmup complete")

	// Phase 2: Initialize suppliers with pre-fetched data
	for _, operatorAddr := range supplierAddrs {
		// Pass pre-fetched data (nil if warmup failed for this supplier)
		prewarmedData := supplierData[operatorAddr]
		if err := m.addSupplierWithData(m.ctx, operatorAddr, prewarmedData); err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to add supplier on startup")
		}
	}

	m.logger.Info().
		Int("suppliers", len(m.suppliers)).
		Msg("supplier manager started")

	return nil
}

// SupplierWarmupData holds pre-fetched supplier data from the chain.
type SupplierWarmupData struct {
	OwnerAddress string
	Services     []string
}

// warmupSuppliersParallel queries all supplier data from chain in parallel using a worker pool.
// This avoids sequential startup delays when many suppliers are configured while respecting CPU limits.
// Uses a subpool from the master worker pool to ensure we don't exceed system capacity.
func (m *SupplierManager) warmupSuppliersParallel(ctx context.Context, supplierAddrs []string) map[string]*SupplierWarmupData {
	if m.config.SupplierQueryClient == nil {
		m.logger.Debug().Msg("no supplier query client configured, skipping warmup")
		return nil
	}

	results := make(map[string]*SupplierWarmupData)
	var mu sync.Mutex

	m.logger.Info().
		Int("suppliers", len(supplierAddrs)).
		Int("max_concurrent", 20).
		Msg("starting parallel supplier warmup with bounded pond subpool")

	// Use pond Group for coordinated task submission and waiting
	// This allows us to wait for all warmup tasks to complete without blocking the persistent subpool
	group := m.querySubpool.NewGroup()

	// Submit all warmup tasks to the persistent querySubpool (20 workers max)
	// This prevents unbounded goroutine spawning when warming up 100+ suppliers
	for _, addr := range supplierAddrs {
		operatorAddr := addr // capture for closure
		group.Submit(func() {
			// Create timeout context for this query
			queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			supplier, err := m.config.SupplierQueryClient.GetSupplier(queryCtx, operatorAddr)
			if err != nil {
				// Check if it's a NotFound error (supplier not staked)
				if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
					m.logger.Debug().
						Str(logging.FieldSupplier, operatorAddr).
						Msg("supplier not staked (pre-loaded key or unstaked)")
				} else {
					// Other errors (network, timeout, etc.)
					m.logger.Warn().
						Err(err).
						Str(logging.FieldSupplier, operatorAddr).
						Msg("failed to query supplier during warmup")
				}
				return
			}

			// Extract services from supplier
			var services []string
			for _, svc := range supplier.Services {
				if svc != nil {
					services = append(services, svc.ServiceId)
				}
			}

			data := &SupplierWarmupData{
				OwnerAddress: supplier.OwnerAddress,
				Services:     services,
			}

			mu.Lock()
			results[operatorAddr] = data
			mu.Unlock()

			m.logger.Debug().
				Str(logging.FieldSupplier, operatorAddr).
				Int("services", len(services)).
				Msg("warmed supplier data")
		})
	}

	// Wait for all warmup tasks to complete
	if err := group.Wait(); err != nil {
		m.logger.Warn().Err(err).Msg("some supplier warmup tasks failed")
	}
	return results
}

// onKeyChange handles key addition/removal notifications.
func (m *SupplierManager) onKeyChange(operatorAddr string, added bool) {
	if added {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("key added, initializing supplier")

		// Hot-reload case: no pre-warmed data, will query fresh
		if err := m.addSupplierWithData(m.ctx, operatorAddr, nil); err != nil {
			m.logger.Error().
				Err(err).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to add supplier")
		}
	} else {
		m.logger.Info().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("key removed, draining supplier")

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
			KeyPrefix:       "ha:miner:sessions",
			SupplierAddress: operatorAddr,
			SessionTTL:      m.config.SessionTTL,
		},
	)

	// Create WAL for this supplier
	wal := NewRedisWAL(
		m.logger,
		m.config.RedisClient,
		WALConfig{
			SupplierAddress: operatorAddr,
			KeyPrefix:       "ha:miner:wal",
			MaxLen:          m.config.WALMaxLen,
		},
	)

	// Create SMST snapshot manager
	snapshotManager := NewSMSTSnapshotManager(
		m.logger,
		sessionStore,
		wal,
		SMSTRecoveryConfig{
			SupplierAddress: operatorAddr,
			RecoveryTimeout: 5 * time.Minute,
		},
	)

	// Use default stream discovery interval (10s)
	// TODO: Make this configurable in the future via SessionLifecycleConfig
	streamDiscoveryInterval := time.Duration(10) * time.Second

	// Create consumer for this supplier
	consumer, err := redistransport.NewStreamsConsumer(
		m.logger,
		m.config.RedisClient,
		transport.ConsumerConfig{
			StreamPrefix:            m.config.StreamPrefix,
			SupplierOperatorAddress: operatorAddr,
			ConsumerGroup:           m.config.ConsumerGroup,
			ConsumerName:            m.config.ConsumerName,
			BatchSize:               100,
			BlockTimeout:            5000,
			ClaimIdleTimeout:        30000,
			MaxRetries:              3,
		},
		streamDiscoveryInterval, // Stream discovery interval from config
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
		},
	)

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
		lifecycleCallback = NewLifecycleCallback(
			m.logger,
			supplierClient,
			m.config.SharedClient,
			m.config.BlockClient,
			m.config.SessionClient,
			smstManager,
			snapshotManager,
			m.config.ProofChecker, // May be nil - if so, proofs are always submitted (legacy)
			LifecycleCallbackConfig{
				SupplierAddress:    operatorAddr,
				ClaimRetryAttempts: 3,
				ClaimRetryDelay:    2 * time.Second,
				ProofRetryAttempts: 3,
				ProofRetryDelay:    2 * time.Second,
			},
		)

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

		// Start lifecycle manager
		if startErr := lifecycleManager.Start(supplierCtx); startErr != nil {
			m.logger.Warn().
				Err(startErr).
				Str(logging.FieldSupplier, operatorAddr).
				Msg("failed to start lifecycle manager, continuing without lifecycle management")
			lifecycleManager = nil
		} else {
			// Wire up callback so snapshot manager notifies lifecycle manager of new sessions
			// This is critical for tracking sessions created after startup
			lm := lifecycleManager // capture for closure
			snapshotManager.SetOnSessionCreatedCallback(func(ctx context.Context, snapshot *SessionSnapshot) error {
				return lm.TrackSession(ctx, snapshot)
			})
			m.logger.Info().
				Str(logging.FieldSupplier, operatorAddr).
				Msg("wired session creation callback to lifecycle manager")
		}
	} else {
		m.logger.Warn().
			Str(logging.FieldSupplier, operatorAddr).
			Msg("lifecycle management disabled - TxClient, BlockClient, or SharedClient not configured")
	}

	state := &SupplierState{
		OperatorAddr:      operatorAddr,
		Status:            SupplierStatusActive,
		Consumer:          consumer,
		SessionStore:      sessionStore,
		WAL:               wal,
		SnapshotManager:   snapshotManager,
		SMSTManager:       smstManager,
		LifecycleManager:  lifecycleManager,
		LifecycleCallback: lifecycleCallback,
		SupplierClient:    supplierClient,
		cancelFn:          cancelFn,
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
			m.logger.Info().
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
			m.logger.Info().
				Str(logging.FieldSupplier, operatorAddr).
				Str("services", fmt.Sprintf("%v", services)).
				Msg("published supplier state to cache")
		}
	}

	supplierManagerSuppliersActive.Inc()

	m.logger.Info().
		Str(logging.FieldSupplier, operatorAddr).
		Msg("supplier added and consuming")

	return nil
}

// consumeForSupplier runs the consume loop for a single supplier.
func (m *SupplierManager) consumeForSupplier(ctx context.Context, state *SupplierState) {
	defer state.wg.Done()

	msgChan := state.Consumer.Consume(ctx)

	for msg := range msgChan {
		// When draining, we continue processing existing messages
		// but log that we're in drain mode for visibility
		if state.Status == SupplierStatusDraining {
			m.logger.Debug().
				Str(logging.FieldSupplier, state.OperatorAddr).
				Msg("processing relay during drain")
		}

		// Process the relay
		if m.onRelay != nil {
			if err := m.onRelay(ctx, state.OperatorAddr, &msg); err != nil {
				m.logger.Warn().
					Err(err).
					Str(logging.FieldSupplier, state.OperatorAddr).
					Str("session_id", msg.Message.SessionId).
					Msg("failed to process relay")
				continue
			}
		}

		// Acknowledge using AckMessage (multi-stream compatible)
		if err := state.Consumer.AckMessage(ctx, msg); err != nil {
			m.logger.Warn().
				Err(err).
				Str(logging.FieldSupplier, state.OperatorAddr).
				Msg("failed to acknowledge message")
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
	_ = state.SnapshotManager.Close()
	_ = state.WAL.Close()
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
		_ = state.SnapshotManager.Close()
		_ = state.WAL.Close()
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
