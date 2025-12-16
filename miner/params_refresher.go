package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
)

const (
	// Default refresh interval for on-chain params
	defaultParamsRefreshInterval = 5 * time.Minute

	// Redis key suffix for meter cleanup channel (combined with RedisKeyPrefix config)
	meterCleanupSuffix = "meter:cleanup"
)

// ParamsRefresherConfig contains configuration for the params refresher.
type ParamsRefresherConfig struct {
	// RefreshInterval is how often to refresh params from chain.
	// Should be less than the session-wide TTL to ensure caches don't expire.
	RefreshInterval time.Duration

	// RedisKeyPrefix is the prefix for Redis keys.
	RedisKeyPrefix string
}

// DefaultParamsRefresherConfig returns sensible defaults.
func DefaultParamsRefresherConfig() ParamsRefresherConfig {
	return ParamsRefresherConfig{
		RefreshInterval: defaultParamsRefreshInterval,
		RedisKeyPrefix:  "ha",
	}
}

// ParamsRefresher monitors and refreshes on-chain params in Redis cache.
// This runs on miners to ensure relayers always have fresh cached data.
// Triggers refresh on new blocks (from BlockClient) rather than time-based polling.
type ParamsRefresher struct {
	logger        logging.Logger
	config        ParamsRefresherConfig
	redisClient   redis.UniversalClient
	sharedClient  client.SharedQueryClient
	sessionClient client.SessionQueryClient
	appClient     client.ApplicationQueryClient
	serviceClient ServiceQueryClient
	blockClient   client.BlockClient // NEW: Block watcher for block-based refresh triggers

	// Track known apps to refresh their stakes
	knownApps   map[string]struct{}
	knownAppsMu sync.RWMutex

	// Track known services to refresh compute units
	knownServices   map[string]struct{}
	knownServicesMu sync.RWMutex

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.RWMutex
	closed   bool
}

// ServiceQueryClient queries on-chain service data.
type ServiceQueryClient interface {
	// GetServiceComputeUnits returns the compute units per relay for a service.
	GetServiceComputeUnits(ctx context.Context, serviceID string) (uint64, error)
}

// NewParamsRefresher creates a new params refresher.
func NewParamsRefresher(
	logger logging.Logger,
	redisClient redis.UniversalClient,
	sharedClient client.SharedQueryClient,
	sessionClient client.SessionQueryClient,
	appClient client.ApplicationQueryClient,
	serviceClient ServiceQueryClient,
	blockClient client.BlockClient,
	config ParamsRefresherConfig,
) *ParamsRefresher {
	if config.RefreshInterval == 0 {
		config.RefreshInterval = defaultParamsRefreshInterval
	}
	if config.RedisKeyPrefix == "" {
		config.RedisKeyPrefix = "ha"
	}

	return &ParamsRefresher{
		logger:        logging.ForComponent(logger, logging.ComponentParamsRefresher),
		config:        config,
		redisClient:   redisClient,
		sharedClient:  sharedClient,
		sessionClient: sessionClient,
		appClient:     appClient,
		serviceClient: serviceClient,
		blockClient:   blockClient,
		knownApps:     make(map[string]struct{}),
		knownServices: make(map[string]struct{}),
	}
}

// Start begins the params refresher background process.
func (r *ParamsRefresher) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}

	r.ctx, r.cancelFn = context.WithCancel(ctx)
	r.mu.Unlock()

	// Do initial refresh immediately
	r.refreshAll(r.ctx)

	// Start background refresh worker
	r.wg.Add(1)
	go r.refreshWorker(r.ctx)

	r.logger.Info().
		Dur("refresh_interval", r.config.RefreshInterval).
		Msg("params refresher started")

	return nil
}

// AddKnownApp adds an application address to track for stake refreshes.
func (r *ParamsRefresher) AddKnownApp(appAddress string) {
	r.knownAppsMu.Lock()
	r.knownApps[appAddress] = struct{}{}
	r.knownAppsMu.Unlock()
}

// AddKnownService adds a service ID to track for compute units refreshes.
func (r *ParamsRefresher) AddKnownService(serviceID string) {
	r.knownServicesMu.Lock()
	r.knownServices[serviceID] = struct{}{}
	r.knownServicesMu.Unlock()
}

// PublishMeterCleanup publishes a cleanup signal for a session.
// Called when a claim is successfully submitted to free Redis space.
func (r *ParamsRefresher) PublishMeterCleanup(ctx context.Context, sessionID string) error {
	channel := fmt.Sprintf("%s:%s", r.config.RedisKeyPrefix, meterCleanupSuffix)
	return r.redisClient.Publish(ctx, channel, sessionID).Err()
}

// refreshWorker refreshes params on new blocks instead of time-based polling.
// This ensures params are fresh without unnecessary polling overhead.
func (r *ParamsRefresher) refreshWorker(ctx context.Context) {
	defer r.wg.Done()

	lastHeight := int64(0)
	ticker := time.NewTicker(1 * time.Second) // Poll for block height changes
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			block := r.blockClient.LastBlock(ctx)
			currentHeight := block.Height()

			// Only refresh when block height changes
			if currentHeight > lastHeight {
				r.refreshAll(ctx)
				lastHeight = currentHeight

				r.logger.Debug().
					Int64("block_height", currentHeight).
					Msg("refreshed params on new block")
			}
		}
	}
}

// refreshAll refreshes all cached params.
func (r *ParamsRefresher) refreshAll(ctx context.Context) {
	// Refresh shared params
	if err := r.refreshSharedParams(ctx); err != nil {
		r.logger.Warn().Err(err).Msg("failed to refresh shared params")
	}

	// Refresh session params
	if err := r.refreshSessionParams(ctx); err != nil {
		r.logger.Warn().Err(err).Msg("failed to refresh session params")
	}

	// Refresh known app stakes
	r.refreshAppStakes(ctx)

	// Refresh known service compute units
	r.refreshServiceComputeUnits(ctx)
}

// refreshSharedParams refreshes shared params cache.
func (r *ParamsRefresher) refreshSharedParams(ctx context.Context) error {
	params, err := r.sharedClient.GetParams(ctx)
	if err != nil {
		return err
	}

	// Store in Redis
	// Using the same format as RelayMeter expects
	type cachedSharedParams struct {
		NumBlocksPerSession                uint64 `json:"num_blocks_per_session"`
		ComputeUnitsToTokensMultiplier     uint64 `json:"compute_units_to_tokens_multiplier"`
		ComputeUnitCostGranularity         uint64 `json:"compute_unit_cost_granularity"`
		SessionEndToProofWindowCloseBlocks int64  `json:"session_end_to_proof_window_close_blocks"`
		UpdatedAt                          int64  `json:"updated_at"`
	}

	// Calculate session end to proof window close
	claimWindowOpen := params.GetClaimWindowOpenOffsetBlocks()
	claimWindowClose := params.GetClaimWindowCloseOffsetBlocks()
	proofWindowOpen := params.GetProofWindowOpenOffsetBlocks()
	proofWindowClose := params.GetProofWindowCloseOffsetBlocks()

	sessionEndToProofClose := int64(claimWindowOpen + claimWindowClose + proofWindowOpen + proofWindowClose)

	cached := cachedSharedParams{
		NumBlocksPerSession:                uint64(params.GetNumBlocksPerSession()),
		ComputeUnitsToTokensMultiplier:     params.GetComputeUnitsToTokensMultiplier(),
		ComputeUnitCostGranularity:         params.GetComputeUnitCostGranularity(),
		SessionEndToProofWindowCloseBlocks: sessionEndToProofClose,
		UpdatedAt:                          time.Now().Unix(),
	}

	key := fmt.Sprintf("%s:params:shared", r.config.RedisKeyPrefix)
	if err := r.redisClient.Set(ctx, key, mustMarshalJSON(cached), 10*time.Minute).Err(); err != nil {
		return err
	}

	r.logger.Debug().Msg("refreshed shared params cache")
	paramsRefreshed.WithLabelValues("shared").Inc()

	return nil
}

// refreshSessionParams refreshes session params cache.
func (r *ParamsRefresher) refreshSessionParams(ctx context.Context) error {
	params, err := r.sessionClient.GetParams(ctx)
	if err != nil {
		return err
	}

	type cachedSessionParams struct {
		NumSuppliersPerSession uint64 `json:"num_suppliers_per_session"`
		UpdatedAt              int64  `json:"updated_at"`
	}

	cached := cachedSessionParams{
		NumSuppliersPerSession: params.GetNumSuppliersPerSession(),
		UpdatedAt:              time.Now().Unix(),
	}

	key := fmt.Sprintf("%s:params:session", r.config.RedisKeyPrefix)
	if err := r.redisClient.Set(ctx, key, mustMarshalJSON(cached), 10*time.Minute).Err(); err != nil {
		return err
	}

	r.logger.Debug().Msg("refreshed session params cache")
	paramsRefreshed.WithLabelValues("session").Inc()

	return nil
}

// refreshAppStakes refreshes all known app stake caches.
func (r *ParamsRefresher) refreshAppStakes(ctx context.Context) {
	r.knownAppsMu.RLock()
	apps := make([]string, 0, len(r.knownApps))
	for app := range r.knownApps {
		apps = append(apps, app)
	}
	r.knownAppsMu.RUnlock()

	for _, appAddr := range apps {
		if err := r.refreshAppStake(ctx, appAddr); err != nil {
			r.logger.Debug().
				Err(err).
				Str("app_address", appAddr).
				Msg("failed to refresh app stake")
		}
	}
}

// refreshAppStake refreshes a single app's stake cache.
func (r *ParamsRefresher) refreshAppStake(ctx context.Context, appAddress string) error {
	app, err := r.appClient.GetApplication(ctx, appAddress)
	if err != nil {
		return err
	}

	type cachedAppStake struct {
		StakeUpokt int64 `json:"stake_upokt"`
		UpdatedAt  int64 `json:"updated_at"`
	}

	cached := cachedAppStake{
		StakeUpokt: app.GetStake().Amount.Int64(),
		UpdatedAt:  time.Now().Unix(),
	}

	key := fmt.Sprintf("%s:app_stake:%s", r.config.RedisKeyPrefix, appAddress)
	if err := r.redisClient.Set(ctx, key, mustMarshalJSON(cached), 10*time.Minute).Err(); err != nil {
		return err
	}

	paramsRefreshed.WithLabelValues("app_stake").Inc()
	return nil
}

// refreshServiceComputeUnits refreshes all known service compute units.
func (r *ParamsRefresher) refreshServiceComputeUnits(ctx context.Context) {
	if r.serviceClient == nil {
		return
	}

	r.knownServicesMu.RLock()
	services := make([]string, 0, len(r.knownServices))
	for svc := range r.knownServices {
		services = append(services, svc)
	}
	r.knownServicesMu.RUnlock()

	for _, serviceID := range services {
		if err := r.refreshServiceComputeUnitsForService(ctx, serviceID); err != nil {
			r.logger.Debug().
				Err(err).
				Str("service_id", serviceID).
				Msg("failed to refresh service compute units")
		}
	}
}

// refreshServiceComputeUnitsForService refreshes a single service's compute units cache.
func (r *ParamsRefresher) refreshServiceComputeUnitsForService(ctx context.Context, serviceID string) error {
	computeUnits, err := r.serviceClient.GetServiceComputeUnits(ctx, serviceID)
	if err != nil {
		return err
	}

	type cachedServiceData struct {
		ComputeUnitsPerRelay uint64 `json:"compute_units_per_relay"`
		UpdatedAt            int64  `json:"updated_at"`
	}

	cached := cachedServiceData{
		ComputeUnitsPerRelay: computeUnits,
		UpdatedAt:            time.Now().Unix(),
	}

	key := fmt.Sprintf("%s:service:%s:compute_units", r.config.RedisKeyPrefix, serviceID)
	if err := r.redisClient.Set(ctx, key, mustMarshalJSON(cached), 10*time.Minute).Err(); err != nil {
		return err
	}

	paramsRefreshed.WithLabelValues("service").Inc()
	return nil
}

// Close gracefully shuts down the params refresher.
func (r *ParamsRefresher) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}
	r.closed = true

	if r.cancelFn != nil {
		r.cancelFn()
	}

	r.wg.Wait()

	r.logger.Info().Msg("params refresher closed")
	return nil
}

// mustMarshalJSON marshals to JSON, panicking on error (should never fail for simple structs).
func mustMarshalJSON(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
