package relayer

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/app/pocket"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// SharedParamCache defines the interface for accessing shared params with L1->L2->L3 caching.
type SharedParamCache interface {
	GetLatestSharedParams(ctx context.Context) (*sharedtypes.Params, error)
}

// ServiceCache defines the interface for accessing service data with L1->L2->L3 caching.
type ServiceCache interface {
	Get(ctx context.Context, serviceID string, force ...bool) (*sharedtypes.Service, error)
}

// FailBehavior determines how the relay meter behaves when Redis is unavailable.
type FailBehavior string

const (
	// FailOpen allows relays when Redis is unavailable (higher availability, risk of over-servicing).
	FailOpen FailBehavior = "open"

	// FailClosed rejects relays when Redis is unavailable (safer, lower availability).
	FailClosed FailBehavior = "closed"

	// Redis key suffixes (combined with RedisKeyPrefix config to form full keys)
	meterKeySuffix      = "meter"         // Session metering data
	paramsKeySuffix     = "params"        // Cached on-chain params
	appStakeKeySuffix   = "app_stake"     // Cached app stakes
	serviceKeySuffix    = "service"       // Cached service data
	meterCleanupChannel = "meter:cleanup" // Pub/sub channel for cleanup signals
)

// RelayMeterConfig contains configuration for the relay meter.
type RelayMeterConfig struct {
	// OverServicingEnabled allows suppliers to serve beyond app stake limits.
	OverServicingEnabled bool

	// RedisKeyPrefix is the prefix for Redis keys.
	RedisKeyPrefix string

	// FailBehavior determines behavior when Redis is unavailable.
	// "open" = allow relays (risk over-servicing)
	// "closed" = reject relays (safer)
	FailBehavior FailBehavior

	// SessionCleanupInterval is how often to clean up expired session meters.
	SessionCleanupInterval time.Duration

	// ParamsCacheTTL is the TTL for cached on-chain params.
	// Should be session-wide (e.g., session duration).
	// Miners will refresh this in background.
	ParamsCacheTTL time.Duration

	// AppStakeCacheTTL is the TTL for cached app stakes.
	// Should be session-wide.
	AppStakeCacheTTL time.Duration
}

// DefaultRelayMeterConfig returns sensible defaults.
func DefaultRelayMeterConfig() RelayMeterConfig {
	return RelayMeterConfig{
		OverServicingEnabled:   true,
		RedisKeyPrefix:         "ha",
		FailBehavior:           FailOpen, // Default to availability
		SessionCleanupInterval: 30 * time.Second,
		ParamsCacheTTL:         10 * time.Minute, // Session-wide, refreshed by miners
		AppStakeCacheTTL:       10 * time.Minute, // Session-wide, refreshed by miners
	}
}

// SessionMeterMeta contains metadata for a session meter stored in Redis.
type SessionMeterMeta struct {
	SessionID        string `json:"session_id"`
	AppAddress       string `json:"app_address"`
	ServiceID        string `json:"service_id"`
	SessionEndHeight int64  `json:"session_end_height"`
	MaxStakeUpokt    int64  `json:"max_stake_upokt"` // Max allowed stake in uPOKT
	CreatedAt        int64  `json:"created_at"`      // Unix timestamp
}

// CachedSharedParams contains cached shared parameters.
type CachedSharedParams struct {
	NumBlocksPerSession                uint64 `json:"num_blocks_per_session"`
	ComputeUnitsToTokensMultiplier     uint64 `json:"compute_units_to_tokens_multiplier"`
	ComputeUnitCostGranularity         uint64 `json:"compute_unit_cost_granularity"`
	SessionEndToProofWindowCloseBlocks int64  `json:"session_end_to_proof_window_close_blocks"`
	UpdatedAt                          int64  `json:"updated_at"`
}

// CachedSessionParams contains cached session parameters.
type CachedSessionParams struct {
	NumSuppliersPerSession uint64 `json:"num_suppliers_per_session"`
	UpdatedAt              int64  `json:"updated_at"`
}

// CachedAppStake contains cached application stake.
type CachedAppStake struct {
	StakeUpokt int64 `json:"stake_upokt"`
	UpdatedAt  int64 `json:"updated_at"`
}

// CachedServiceData contains cached service configuration.
type CachedServiceData struct {
	ComputeUnitsPerRelay uint64 `json:"compute_units_per_relay"`
	UpdatedAt            int64  `json:"updated_at"`
}

// SessionMeterState represents the metering state for a session.
// Used for local caching and API responses.
type SessionMeterState struct {
	SessionID          string
	AppAddress         string
	ServiceID          string
	MaxStake           cosmostypes.Coin
	ConsumedStake      cosmostypes.Coin
	OverServicedRelays uint64
	SessionEndHeight   int64
	LastUpdated        time.Time
}

// RelayMeter manages rate limiting based on application stake.
// Uses Redis for distributed state sharing across replicas.
type RelayMeter struct {
	logger        logging.Logger
	config        RelayMeterConfig
	redisClient   redis.UniversalClient
	appClient     client.ApplicationQueryClient
	sharedClient  client.SharedQueryClient
	sessionClient client.SessionQueryClient
	blockClient   client.BlockClient

	// Caches (L1 -> L2 -> L3 with pub/sub invalidation)
	sharedParamCache SharedParamCache
	serviceCache     ServiceCache

	// Local L1 cache for hot path performance
	// This is a read-through cache; writes go to Redis first
	localCache   map[string]*SessionMeterMeta
	localCacheMu sync.RWMutex

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.RWMutex
	closed   bool
}

// NewRelayMeter creates a new relay meter.
func NewRelayMeter(
	logger logging.Logger,
	redisClient redis.UniversalClient,
	appClient client.ApplicationQueryClient,
	sharedClient client.SharedQueryClient,
	sessionClient client.SessionQueryClient,
	blockClient client.BlockClient,
	sharedParamCache SharedParamCache,
	serviceCache ServiceCache,
	config RelayMeterConfig,
) *RelayMeter {
	if config.RedisKeyPrefix == "" {
		config.RedisKeyPrefix = "ha"
	}
	if config.FailBehavior == "" {
		config.FailBehavior = FailOpen
	}
	if config.SessionCleanupInterval == 0 {
		config.SessionCleanupInterval = 30 * time.Second
	}
	if config.ParamsCacheTTL == 0 {
		config.ParamsCacheTTL = 10 * time.Minute
	}
	if config.AppStakeCacheTTL == 0 {
		config.AppStakeCacheTTL = 10 * time.Minute
	}

	return &RelayMeter{
		logger:           logging.ForComponent(logger, logging.ComponentRelayMeter),
		config:           config,
		redisClient:      redisClient,
		appClient:        appClient,
		sharedClient:     sharedClient,
		sessionClient:    sessionClient,
		blockClient:      blockClient,
		sharedParamCache: sharedParamCache,
		serviceCache:     serviceCache,
		localCache:       make(map[string]*SessionMeterMeta),
	}
}

// Start begins the relay meter background processes.
func (m *RelayMeter) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("relay meter is closed")
	}

	m.ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	// Start cleanup subscription worker
	m.wg.Add(1)
	go m.cleanupSubscriber(m.ctx)

	// Start periodic local cache cleanup
	m.wg.Add(1)
	go m.localCacheCleanupWorker(m.ctx)

	m.logger.Info().
		Bool("over_servicing_enabled", m.config.OverServicingEnabled).
		Str("fail_behavior", string(m.config.FailBehavior)).
		Msg("relay meter started")

	return nil
}

// CheckAndConsumeRelay checks if a relay can be served and consumes stake if so.
// Uses atomic Redis INCRBY for distributed state.
// Returns:
// - allowed: true if the relay should be served
// - overServiced: true if this relay exceeds the app's stake limit
// - err: any error that occurred
func (m *RelayMeter) CheckAndConsumeRelay(
	ctx context.Context,
	sessionID string,
	appAddress string,
	serviceID string,
	sessionEndHeight int64,
) (allowed bool, overServiced bool, err error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return false, false, fmt.Errorf("relay meter is closed")
	}
	m.mu.RUnlock()

	// Get relay cost first
	relayCostUpokt, err := m.getRelayCost(ctx, serviceID)
	if err != nil {
		m.logger.Warn().Err(err).Str(logging.FieldServiceID, serviceID).
			Msg("failed to get relay cost")
		return m.handleRedisError("get relay cost")
	}

	// Get or create session meter
	meta, maxStakeUpokt, err := m.getOrCreateSessionMeter(ctx, sessionID, appAddress, serviceID, sessionEndHeight)
	if err != nil {
		m.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to get session meter")
		return m.handleRedisError("get session meter")
	}

	// Atomically increment consumed stake in Redis
	consumedKey := m.consumedKey(sessionID)
	newConsumed, err := m.redisClient.IncrBy(ctx, consumedKey, relayCostUpokt).Result()
	if err != nil {
		m.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to increment consumed stake")
		return m.handleRedisError("increment consumed")
	}

	// Check if within limits
	if newConsumed <= maxStakeUpokt {
		// Within limits
		relayMeterConsumptions.WithLabelValues(serviceID, "within_limit").Inc()
		return true, false, nil
	}

	// Over the limit - atomically increment over-serviced counter
	overServicedKey := m.overServicedKey(sessionID)
	overServicedCount, _ := m.redisClient.Incr(ctx, overServicedKey).Result()

	relayMeterConsumptions.WithLabelValues(serviceID, "over_limit").Inc()

	if m.config.OverServicingEnabled {
		// Track over-servicing metrics
		overServicedUpokt := newConsumed - maxStakeUpokt
		relayMeterOverServicedUpokt.WithLabelValues(serviceID, sessionID).Add(float64(overServicedUpokt))
		relayMeterOverServicedRelays.WithLabelValues(serviceID, sessionID).Inc()

		// Log at power-of-2 intervals
		if shouldLogOverServicing(uint64(overServicedCount)) {
			m.logger.Warn().
				Str("application", meta.AppAddress).
				Str(logging.FieldServiceID, serviceID).
				Str(logging.FieldSessionID, sessionID).
				Int64("over_serviced_count", overServicedCount).
				Int64("over_serviced_upokt", overServicedUpokt).
				Int64("consumed_upokt", newConsumed).
				Int64("max_stake_upokt", maxStakeUpokt).
				Msg("application over-serviced (over-servicing enabled)")
		}
		return true, true, nil
	}

	m.logger.Debug().
		Str("application", appAddress).
		Str(logging.FieldSessionID, sessionID).
		Int64("consumed_upokt", newConsumed).
		Int64("max_stake_upokt", maxStakeUpokt).
		Msg("relay rejected due to stake limit")

	// Revert the increment since we're rejecting
	m.redisClient.DecrBy(ctx, consumedKey, relayCostUpokt)

	return false, true, nil
}

// RevertRelayConsumption reverts the stake consumption for a relay that wasn't mined.
func (m *RelayMeter) RevertRelayConsumption(
	ctx context.Context,
	sessionID string,
	serviceID string,
) error {
	relayCostUpokt, err := m.getRelayCost(ctx, serviceID)
	if err != nil {
		return nil // Can't calculate, skip revert
	}

	consumedKey := m.consumedKey(sessionID)
	newVal, err := m.redisClient.DecrBy(ctx, consumedKey, relayCostUpokt).Result()
	if err != nil {
		return fmt.Errorf("failed to revert consumption: %w", err)
	}

	// Ensure we don't go negative
	if newVal < 0 {
		m.redisClient.Set(ctx, consumedKey, 0, 0)
	}

	return nil
}

// AllowOverServicing returns whether over-servicing is enabled.
func (m *RelayMeter) AllowOverServicing() bool {
	return m.config.OverServicingEnabled
}

// GetSessionMeterState returns the current meter state for a session.
func (m *RelayMeter) GetSessionMeterState(ctx context.Context, sessionID string) *SessionMeterState {
	meta, err := m.getSessionMeta(ctx, sessionID)
	if err != nil || meta == nil {
		return nil
	}

	consumed, _ := m.redisClient.Get(ctx, m.consumedKey(sessionID)).Int64()
	overServiced, _ := m.redisClient.Get(ctx, m.overServicedKey(sessionID)).Uint64()

	return &SessionMeterState{
		SessionID:          meta.SessionID,
		AppAddress:         meta.AppAddress,
		ServiceID:          meta.ServiceID,
		MaxStake:           cosmostypes.NewInt64Coin(pocket.DenomuPOKT, meta.MaxStakeUpokt),
		ConsumedStake:      cosmostypes.NewInt64Coin(pocket.DenomuPOKT, consumed),
		OverServicedRelays: overServiced,
		SessionEndHeight:   meta.SessionEndHeight,
		LastUpdated:        time.Unix(meta.CreatedAt, 0),
	}
}

// ClearSessionMeter clears all metering data for a session.
// Called by miners when claims are processed to free Redis space.
func (m *RelayMeter) ClearSessionMeter(ctx context.Context, sessionID string) error {
	keys := []string{
		m.metaKey(sessionID),
		m.consumedKey(sessionID),
		m.overServicedKey(sessionID),
	}

	if err := m.redisClient.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("failed to clear session meter: %w", err)
	}

	// Clear local cache
	m.localCacheMu.Lock()
	delete(m.localCache, sessionID)
	m.localCacheMu.Unlock()

	relayMeterSessionsActive.Dec()

	m.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Msg("cleared session meter")

	return nil
}

// PublishCleanupSignal publishes a cleanup signal for a session.
// Miners call this after processing claims to notify all relayers.
func (m *RelayMeter) PublishCleanupSignal(ctx context.Context, sessionID string) error {
	channel := fmt.Sprintf("%s:%s", m.config.RedisKeyPrefix, meterCleanupChannel)
	return m.redisClient.Publish(ctx, channel, sessionID).Err()
}

// getOrCreateSessionMeter gets or creates a session meter in Redis.
// Returns the metadata and max stake in uPOKT.
func (m *RelayMeter) getOrCreateSessionMeter(
	ctx context.Context,
	sessionID string,
	appAddress string,
	serviceID string,
	sessionEndHeight int64,
) (*SessionMeterMeta, int64, error) {
	// Check local cache first (L1)
	m.localCacheMu.RLock()
	if meta, exists := m.localCache[sessionID]; exists {
		m.localCacheMu.RUnlock()
		return meta, meta.MaxStakeUpokt, nil
	}
	m.localCacheMu.RUnlock()

	// Check Redis (L2)
	meta, err := m.getSessionMeta(ctx, sessionID)
	if err == nil && meta != nil {
		// Cache locally
		m.localCacheMu.Lock()
		m.localCache[sessionID] = meta
		m.localCacheMu.Unlock()
		return meta, meta.MaxStakeUpokt, nil
	}

	// Create new session meter
	maxStakeUpokt, err := m.calculateMaxStake(ctx, appAddress)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to calculate max stake: %w", err)
	}

	meta = &SessionMeterMeta{
		SessionID:        sessionID,
		AppAddress:       appAddress,
		ServiceID:        serviceID,
		SessionEndHeight: sessionEndHeight,
		MaxStakeUpokt:    maxStakeUpokt,
		CreatedAt:        time.Now().Unix(),
	}

	// Store in Redis with session-wide TTL
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal meta: %w", err)
	}

	// Use SETNX to handle race conditions
	metaKey := m.metaKey(sessionID)
	set, err := m.redisClient.SetNX(ctx, metaKey, metaBytes, m.config.ParamsCacheTTL).Result()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create session meter: %w", err)
	}

	if !set {
		// Another replica created it first, fetch their version
		return m.getOrCreateSessionMeter(ctx, sessionID, appAddress, serviceID, sessionEndHeight)
	}

	// Initialize consumed counter
	consumedKey := m.consumedKey(sessionID)
	m.redisClient.Set(ctx, consumedKey, 0, m.config.ParamsCacheTTL)

	// Cache locally
	m.localCacheMu.Lock()
	m.localCache[sessionID] = meta
	m.localCacheMu.Unlock()

	relayMeterSessionsActive.Inc()

	return meta, maxStakeUpokt, nil
}

// getSessionMeta retrieves session metadata from Redis.
func (m *RelayMeter) getSessionMeta(ctx context.Context, sessionID string) (*SessionMeterMeta, error) {
	data, err := m.redisClient.Get(ctx, m.metaKey(sessionID)).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}

	var meta SessionMeterMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// calculateMaxStake calculates the maximum stake an app can consume per session/supplier.
// Uses cached params from Redis when available.
func (m *RelayMeter) calculateMaxStake(ctx context.Context, appAddress string) (int64, error) {
	// Get app stake (from Redis cache or chain)
	appStakeUpokt, err := m.getAppStake(ctx, appAddress)
	if err != nil {
		return 0, fmt.Errorf("failed to get app stake: %w", err)
	}

	// Get shared params (from Redis cache or chain)
	sharedParams, err := m.getSharedParams(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get shared params: %w", err)
	}

	// Get session params (from Redis cache or chain)
	sessionParams, err := m.getSessionParams(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get session params: %w", err)
	}

	// Calculate: stake / numSuppliers / pendingSessions
	numSuppliers := int64(sessionParams.NumSuppliersPerSession)
	if numSuppliers == 0 {
		numSuppliers = 1
	}

	appStakePerSupplier := appStakeUpokt / numSuppliers

	// Account for pending sessions
	numBlocksPerSession := int64(sharedParams.NumBlocksPerSession)
	if numBlocksPerSession == 0 {
		numBlocksPerSession = 1
	}

	numBlocksUntilProofWindowCloses := sharedParams.SessionEndToProofWindowCloseBlocks
	numClosedSessionsAwaitingSettlement := int64(math.Ceil(
		float64(numBlocksUntilProofWindowCloses) / float64(numBlocksPerSession),
	))

	// Add 1 for current session
	pendingSessions := numClosedSessionsAwaitingSettlement + 1

	maxStakePerSession := appStakePerSupplier / pendingSessions

	return maxStakePerSession, nil
}

// getRelayCost calculates the cost of a single relay in uPOKT.
// Uses cached params from Redis when available.
func (m *RelayMeter) getRelayCost(ctx context.Context, serviceID string) (int64, error) {
	// Get shared params
	sharedParams, err := m.getSharedParams(ctx)
	if err != nil {
		return 0, err
	}

	// Get compute units per relay for this service
	computeUnitsPerRelay, err := m.getServiceComputeUnits(ctx, serviceID)
	if err != nil {
		// Default to 1 if service not found
		computeUnitsPerRelay = 1
	}

	// Calculate cost: computeUnits * (multiplier / granularity)
	if sharedParams.ComputeUnitCostGranularity == 0 {
		return 0, fmt.Errorf("compute unit cost granularity is 0")
	}

	computeUnitCostUpokt := new(big.Rat).SetFrac64(
		int64(sharedParams.ComputeUnitsToTokensMultiplier),
		int64(sharedParams.ComputeUnitCostGranularity),
	)

	relayCostRat := new(big.Rat).Mul(
		new(big.Rat).SetUint64(computeUnitsPerRelay),
		computeUnitCostUpokt,
	)

	estimatedRelayCost := big.NewInt(0).Quo(relayCostRat.Num(), relayCostRat.Denom())
	return estimatedRelayCost.Int64(), nil
}

// getAppStake gets the app stake from Redis cache or queries the chain.
func (m *RelayMeter) getAppStake(ctx context.Context, appAddress string) (int64, error) {
	cacheKey := m.appStakeKey(appAddress)

	// Check Redis cache
	data, err := m.redisClient.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var cached CachedAppStake
		if json.Unmarshal(data, &cached) == nil {
			return cached.StakeUpokt, nil
		}
	}

	// Query chain
	app, err := m.appClient.GetApplication(ctx, appAddress)
	if err != nil {
		return 0, fmt.Errorf("failed to get application: %w", err)
	}

	stakeUpokt := app.GetStake().Amount.Int64()

	// Cache in Redis with session-wide TTL
	cached := CachedAppStake{
		StakeUpokt: stakeUpokt,
		UpdatedAt:  time.Now().Unix(),
	}
	if cacheBytes, err := json.Marshal(cached); err == nil {
		m.redisClient.Set(ctx, cacheKey, cacheBytes, m.config.AppStakeCacheTTL)
	}

	return stakeUpokt, nil
}

// getSharedParams gets shared params using L1 -> L2 -> L3 cache.
func (m *RelayMeter) getSharedParams(ctx context.Context) (*CachedSharedParams, error) {
	// Use shared param cache (L1 -> L2 -> L3)
	params, err := m.sharedParamCache.GetLatestSharedParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared params: %w", err)
	}

	// Convert to CachedSharedParams format
	cached := &CachedSharedParams{
		NumBlocksPerSession:                uint64(params.GetNumBlocksPerSession()),
		ComputeUnitsToTokensMultiplier:     params.GetComputeUnitsToTokensMultiplier(),
		ComputeUnitCostGranularity:         params.GetComputeUnitCostGranularity(),
		SessionEndToProofWindowCloseBlocks: sharedtypes.GetSessionEndToProofWindowCloseBlocks(params),
		UpdatedAt:                          time.Now().Unix(),
	}

	return cached, nil
}

// getSessionParams gets session params from Redis cache or queries the chain.
func (m *RelayMeter) getSessionParams(ctx context.Context) (*CachedSessionParams, error) {
	cacheKey := m.sessionParamsKey()

	// Check Redis cache
	data, err := m.redisClient.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var cached CachedSessionParams
		if json.Unmarshal(data, &cached) == nil {
			return &cached, nil
		}
	}

	// Query chain
	params, err := m.sessionClient.GetParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get session params: %w", err)
	}

	cached := &CachedSessionParams{
		NumSuppliersPerSession: params.GetNumSuppliersPerSession(),
		UpdatedAt:              time.Now().Unix(),
	}

	// Cache in Redis with session-wide TTL
	if cacheBytes, err := json.Marshal(cached); err == nil {
		m.redisClient.Set(ctx, cacheKey, cacheBytes, m.config.ParamsCacheTTL)
	}

	return cached, nil
}

// getServiceComputeUnits gets compute units per relay for a service using L1 -> L2 -> L3 cache.
func (m *RelayMeter) getServiceComputeUnits(ctx context.Context, serviceID string) (uint64, error) {
	// Use service cache (L1 -> L2 -> L3)
	service, err := m.serviceCache.Get(ctx, serviceID)
	if err != nil {
		// Service not found - default to 1 compute unit
		// This is safe because miners will populate the cache with actual values
		return 1, nil
	}

	computeUnits := service.GetComputeUnitsPerRelay()
	if computeUnits == 0 {
		// Ensure we never return 0 (would break cost calculations)
		return 1, nil
	}

	return computeUnits, nil
}

// RefreshSharedParams refreshes shared params cache from chain.
// Called by miners in background process.
func (m *RelayMeter) RefreshSharedParams(ctx context.Context) error {
	params, err := m.sharedClient.GetParams(ctx)
	if err != nil {
		return fmt.Errorf("failed to get shared params: %w", err)
	}

	cached := &CachedSharedParams{
		NumBlocksPerSession:                uint64(params.GetNumBlocksPerSession()),
		ComputeUnitsToTokensMultiplier:     params.GetComputeUnitsToTokensMultiplier(),
		ComputeUnitCostGranularity:         params.GetComputeUnitCostGranularity(),
		SessionEndToProofWindowCloseBlocks: sharedtypes.GetSessionEndToProofWindowCloseBlocks(params),
		UpdatedAt:                          time.Now().Unix(),
	}

	cacheBytes, err := json.Marshal(cached)
	if err != nil {
		return err
	}

	return m.redisClient.Set(ctx, m.sharedParamsKey(), cacheBytes, m.config.ParamsCacheTTL).Err()
}

// RefreshSessionParams refreshes session params cache from chain.
// Called by miners in background process.
func (m *RelayMeter) RefreshSessionParams(ctx context.Context) error {
	params, err := m.sessionClient.GetParams(ctx)
	if err != nil {
		return fmt.Errorf("failed to get session params: %w", err)
	}

	cached := &CachedSessionParams{
		NumSuppliersPerSession: params.GetNumSuppliersPerSession(),
		UpdatedAt:              time.Now().Unix(),
	}

	cacheBytes, err := json.Marshal(cached)
	if err != nil {
		return err
	}

	return m.redisClient.Set(ctx, m.sessionParamsKey(), cacheBytes, m.config.ParamsCacheTTL).Err()
}

// RefreshAppStake refreshes app stake cache from chain.
// Called by miners in background process.
func (m *RelayMeter) RefreshAppStake(ctx context.Context, appAddress string) error {
	app, err := m.appClient.GetApplication(ctx, appAddress)
	if err != nil {
		return fmt.Errorf("failed to get application: %w", err)
	}

	cached := &CachedAppStake{
		StakeUpokt: app.GetStake().Amount.Int64(),
		UpdatedAt:  time.Now().Unix(),
	}

	cacheBytes, err := json.Marshal(cached)
	if err != nil {
		return err
	}

	return m.redisClient.Set(ctx, m.appStakeKey(appAddress), cacheBytes, m.config.AppStakeCacheTTL).Err()
}

// RefreshServiceComputeUnits refreshes service compute units cache.
// Called by miners in background process.
func (m *RelayMeter) RefreshServiceComputeUnits(ctx context.Context, serviceID string, computeUnits uint64) error {
	cached := &CachedServiceData{
		ComputeUnitsPerRelay: computeUnits,
		UpdatedAt:            time.Now().Unix(),
	}

	cacheBytes, err := json.Marshal(cached)
	if err != nil {
		return err
	}

	return m.redisClient.Set(ctx, m.serviceComputeUnitsKey(serviceID), cacheBytes, m.config.ParamsCacheTTL).Err()
}

// handleRedisError handles Redis errors based on fail behavior.
func (m *RelayMeter) handleRedisError(operation string) (allowed bool, overServiced bool, err error) {
	relayMeterRedisErrors.WithLabelValues(operation).Inc()

	if m.config.FailBehavior == FailOpen {
		m.logger.Warn().
			Str("operation", operation).
			Msg("Redis error, fail-open: allowing relay")
		return true, false, nil
	}

	m.logger.Warn().
		Str("operation", operation).
		Msg("Redis error, fail-closed: rejecting relay")
	return false, false, fmt.Errorf("redis unavailable and fail-closed configured")
}

// cleanupSubscriber subscribes to cleanup signals from miners.
func (m *RelayMeter) cleanupSubscriber(ctx context.Context) {
	defer m.wg.Done()

	channel := fmt.Sprintf("%s:%s", m.config.RedisKeyPrefix, meterCleanupChannel)
	pubsub := m.redisClient.Subscribe(ctx, channel)
	defer func() { _ = pubsub.Close() }()

	ch := pubsub.Channel()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Received cleanup signal for a session
			sessionID := msg.Payload
			if err := m.ClearSessionMeter(ctx, sessionID); err != nil {
				m.logger.Warn().
					Err(err).
					Str(logging.FieldSessionID, sessionID).
					Msg("failed to clear session meter on cleanup signal")
			}
		}
	}
}

// localCacheCleanupWorker periodically cleans up the local L1 cache.
func (m *RelayMeter) localCacheCleanupWorker(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(m.config.SessionCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.cleanupLocalCache(ctx)
		}
	}
}

// cleanupLocalCache removes expired entries from local cache.
func (m *RelayMeter) cleanupLocalCache(ctx context.Context) {
	sharedParams, err := m.getSharedParams(ctx)
	if err != nil {
		return
	}

	block := m.blockClient.LastBlock(ctx)
	currentHeight := block.Height()

	// Find sessions to delete
	m.localCacheMu.RLock()
	var toDelete []string
	for sessionID, meta := range m.localCache {
		// Convert shared params to check claim window
		params := &sharedtypes.Params{
			NumBlocksPerSession: sharedParams.NumBlocksPerSession,
		}
		claimWindowOpen := sharedtypes.GetClaimWindowOpenHeight(params, meta.SessionEndHeight)
		if currentHeight >= claimWindowOpen {
			toDelete = append(toDelete, sessionID)
		}
	}
	m.localCacheMu.RUnlock()

	if len(toDelete) == 0 {
		return
	}

	// Delete expired sessions from local cache
	m.localCacheMu.Lock()
	for _, sessionID := range toDelete {
		delete(m.localCache, sessionID)
	}
	m.localCacheMu.Unlock()

	m.logger.Debug().
		Int("cleaned_up", len(toDelete)).
		Msg("cleaned up local cache entries")
}

// Close gracefully shuts down the relay meter.
func (m *RelayMeter) Close() error {
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

	m.logger.Info().Msg("relay meter closed")
	return nil
}

// Redis key helpers
func (m *RelayMeter) metaKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:%s:meta", m.config.RedisKeyPrefix, meterKeySuffix, sessionID)
}

func (m *RelayMeter) consumedKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:%s:consumed", m.config.RedisKeyPrefix, meterKeySuffix, sessionID)
}

func (m *RelayMeter) overServicedKey(sessionID string) string {
	return fmt.Sprintf("%s:%s:%s:over_serviced", m.config.RedisKeyPrefix, meterKeySuffix, sessionID)
}

func (m *RelayMeter) appStakeKey(appAddress string) string {
	return fmt.Sprintf("%s:%s:%s", m.config.RedisKeyPrefix, appStakeKeySuffix, appAddress)
}

func (m *RelayMeter) sharedParamsKey() string {
	return fmt.Sprintf("%s:%s:shared", m.config.RedisKeyPrefix, paramsKeySuffix)
}

func (m *RelayMeter) sessionParamsKey() string {
	return fmt.Sprintf("%s:%s:session", m.config.RedisKeyPrefix, paramsKeySuffix)
}

func (m *RelayMeter) serviceComputeUnitsKey(serviceID string) string {
	return fmt.Sprintf("%s:%s:%s:compute_units", m.config.RedisKeyPrefix, serviceKeySuffix, serviceID)
}

// shouldLogOverServicing returns true if the occurrence count is a power of 2.
// This provides exponential backoff for logging.
func shouldLogOverServicing(occurrence uint64) bool {
	return (occurrence & (occurrence - 1)) == 0
}

// RelayMeterSnapshot captures the current state for monitoring/debugging.
type RelayMeterSnapshot struct {
	ActiveSessions       int
	TotalOverServiced    uint64
	OverServicingEnabled bool
	FailBehavior         FailBehavior
}

// GetSnapshot returns a snapshot of the relay meter state.
func (m *RelayMeter) GetSnapshot(ctx context.Context) RelayMeterSnapshot {
	m.localCacheMu.RLock()
	activeLocal := len(m.localCache)
	m.localCacheMu.RUnlock()

	return RelayMeterSnapshot{
		ActiveSessions:       activeLocal,
		OverServicingEnabled: m.config.OverServicingEnabled,
		FailBehavior:         m.config.FailBehavior,
	}
}

// calculateAppStakePerSessionSupplier calculates the portion of app stake
// available to a single supplier in a single session.
// Kept for backwards compatibility with existing callers.
