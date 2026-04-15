package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// ServiceFactorData is the data stored in Redis for a service factor.
type ServiceFactorData struct {
	Factor    float64 `json:"factor"`
	UpdatedAt int64   `json:"updated_at"`
}

// ServiceFactorRegistryConfig contains configuration for the ServiceFactorRegistry.
type ServiceFactorRegistryConfig struct {
	// DefaultServiceFactor is the global service factor for all services.
	// If set, effectiveLimit = appStake * DefaultServiceFactor
	// If not set (0), use baseLimit formula.
	DefaultServiceFactor float64

	// ServiceFactors is a map of per-service overrides.
	// Key: serviceID, Value: serviceFactor
	ServiceFactors map[string]float64

	// CacheTTL is the TTL for service factor data in Redis.
	// Prevents data leaks if a miner stops publishing.
	CacheTTL time.Duration
}

// ServiceFactorRegistry manages service factor configuration in Redis.
// It is created on the miner and publishes service factors to Redis
// so that relayers can read them for relay metering.
type ServiceFactorRegistry struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	keyBuilder  *redisutil.KeyBuilder
	config      ServiceFactorRegistryConfig
}

// NewServiceFactorRegistry creates a new service factor registry.
func NewServiceFactorRegistry(
	logger logging.Logger,
	redisClient *redisutil.Client,
	keyBuilder *redisutil.KeyBuilder,
	config ServiceFactorRegistryConfig,
) *ServiceFactorRegistry {
	return &ServiceFactorRegistry{
		logger:      logging.ForComponent(logger, logging.ComponentServiceFactorRegistry),
		redisClient: redisClient,
		keyBuilder:  keyBuilder,
		config:      config,
	}
}

// PublishServiceFactors publishes all service factor configuration to Redis.
// This should be called at miner startup after leader election setup.
//
// After writing each value, it also publishes a cache invalidation event on
// the service_factor pub/sub channel so that any live relayer instances
// invalidate their L1 cache and pick up the new value on the next call to
// GetServiceFactor. See cache/service_factor_events.go for the channel
// contract and payload format.
func (r *ServiceFactorRegistry) PublishServiceFactors(ctx context.Context) error {
	// Publish a default service factor if set
	if r.config.DefaultServiceFactor > 0 {
		key := r.keyBuilder.ServiceFactorDefaultKey()
		data := ServiceFactorData{
			Factor:    r.config.DefaultServiceFactor,
			UpdatedAt: nowUnix(),
		}
		jsonData, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal default service factor: %w", err)
		}

		if err = r.redisClient.Set(ctx, key, jsonData, r.config.CacheTTL).Err(); err != nil {
			return fmt.Errorf("failed to set default service factor: %w", err)
		}

		if err = r.publishInvalidation(ctx, ""); err != nil {
			// Non-fatal: relayers will still read the new value on next restart,
			// and the stale L1 cache has a finite blast radius.
			r.logger.Warn().
				Err(err).
				Str("service_id", "default").
				Msg("failed to publish service_factor invalidation event (non-fatal)")
		}

		r.logger.Info().
			Float64("factor", r.config.DefaultServiceFactor).
			Str("key", key).
			Msg("published default service factor to Redis")
	} else {
		r.logger.Info().Msg("no default_service_factor configured, will use baseLimit formula (most conservative)")
	}

	// Publish per-service overrides
	for serviceID, factor := range r.config.ServiceFactors {
		if factor <= 0 {
			r.logger.Warn().
				Str("service_id", serviceID).
				Float64("factor", factor).
				Msg("ignoring invalid service_factor <= 0")
			continue
		}

		key := r.keyBuilder.ServiceFactorServiceKey(serviceID)
		data := ServiceFactorData{
			Factor:    factor,
			UpdatedAt: nowUnix(),
		}
		jsonData, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal service factor for %s: %w", serviceID, err)
		}

		if err = r.redisClient.Set(ctx, key, jsonData, r.config.CacheTTL).Err(); err != nil {
			return fmt.Errorf("failed to set service factor for %s: %w", serviceID, err)
		}

		if err = r.publishInvalidation(ctx, serviceID); err != nil {
			r.logger.Warn().
				Err(err).
				Str("service_id", serviceID).
				Msg("failed to publish service_factor invalidation event (non-fatal)")
		}

		r.logger.Info().
			Str("service_id", serviceID).
			Float64("factor", factor).
			Str("key", key).
			Msg("published per-service factor to Redis")
	}

	return nil
}

// publishInvalidation sends a cache-invalidation event on the service_factor
// pub/sub channel. ServiceID == "" means "invalidate the default factor
// entry"; a non-empty serviceID invalidates that specific override.
func (r *ServiceFactorRegistry) publishInvalidation(ctx context.Context, serviceID string) error {
	payload := cache.ServiceFactorInvalidationPayload{ServiceID: serviceID}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal invalidation payload: %w", err)
	}
	if err = cache.PublishInvalidation(
		ctx,
		r.redisClient,
		r.logger,
		cache.ServiceFactorCacheType,
		string(payloadBytes),
	); err != nil {
		return err
	}
	scope := serviceID
	if scope == "" {
		scope = "default"
	}
	r.logger.Info().
		Str("scope", scope).
		Str("payload", string(payloadBytes)).
		Msg("published service_factor invalidation event to pub/sub")
	return nil
}

// GetServiceFactor returns the service factor for a given service ID.
// It checks per-service override first, then falls back to default.
// Returns (factor, true) if found, (0, false) if not configured.
func (r *ServiceFactorRegistry) GetServiceFactor(serviceID string) (float64, bool) {
	// Check per-service override
	if factor, exists := r.config.ServiceFactors[serviceID]; exists && factor > 0 {
		return factor, true
	}

	// Fall back to default
	if r.config.DefaultServiceFactor > 0 {
		return r.config.DefaultServiceFactor, true
	}

	return 0, false
}

// ClearAll removes all service factor data from Redis.
// Used primarily for testing.
func (r *ServiceFactorRegistry) ClearAll(ctx context.Context) error {
	// Clear default
	key := r.keyBuilder.ServiceFactorDefaultKey()
	r.redisClient.Del(ctx, key)

	// Clear per-service
	for serviceID := range r.config.ServiceFactors {
		sfKey := r.keyBuilder.ServiceFactorServiceKey(serviceID)
		r.redisClient.Del(ctx, sfKey)
	}

	return nil
}

// nowUnix returns the current Unix timestamp.
func nowUnix() int64 {
	return time.Now().Unix()
}
