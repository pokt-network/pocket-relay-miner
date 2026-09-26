package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// ServiceFactorData is the data stored in Redis for a service factor.
//
// DEPRECATED as the relayer's source of truth: it is the per-key format, kept
// only so a relayer built before the manifest keeps reading factors from a
// miner built after it. New relayers read ServiceFactorManifest.
type ServiceFactorData struct {
	Factor    float64 `json:"factor"`
	UpdatedAt int64   `json:"updated_at"`
}

// ServiceFactorManifest is the COMPLETE service factor state, published as one
// document so that every case is a value and none is an absence.
//
// The per-key format cannot express "the operator configured no factor": with
// nothing configured the miner writes no key at all, so a relayer cannot tell
// that from "the miner has not published yet". Both are the same missing byte,
// and one of them means the relayer is pricing relays it should refuse.
//
// It is also the only shape that can retire an override. The miner never
// deletes: an override removed from the config used to stand in Redis until its
// key expired, and a new leader could not clear it because it did not know the
// key existed. One document replaces the whole set in a single write.
type ServiceFactorManifest struct {
	// HasDefault distinguishes "no default configured" from a default of zero.
	// DefaultFactor is meaningless when this is false.
	HasDefault bool `json:"has_default"`

	// DefaultFactor applies to every service without an entry in Overrides.
	DefaultFactor float64 `json:"default_factor"`

	// Overrides holds the per-service factors. A service absent from this map
	// has no override -- that is a statement, not a gap.
	Overrides map[string]float64 `json:"overrides"`

	UpdatedAt int64 `json:"updated_at"`
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

	// RepublishInterval is how often Start rewrites the manifest.
	RepublishInterval time.Duration
}

// ServiceFactorRegistry manages service factor configuration in Redis.
// It is created on the miner and publishes service factors to Redis
// so that relayers can read them for relay metering.
type ServiceFactorRegistry struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	keyBuilder  *redisutil.KeyBuilder
	config      ServiceFactorRegistryConfig

	// Lifecycle. The republish loop hangs off a context this registry owns, NOT
	// off the one Start receives: that context belongs to the leader elector and
	// is not cancelled when leadership is lost (LeaderController.Close cleans up
	// components without cancelling anything). A loop on the elector's context
	// would keep rewriting the manifest from a miner that no longer leads --
	// the exact stale writer this republishing exists to bound.
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	started  bool
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

// Start publishes the manifest once and then keeps rewriting it on
// RepublishInterval until Close.
//
// The manifest has no TTL, so nothing else would ever correct it. A miner that
// wrote it while it still believed itself leader leaves its own config standing
// forever; the current leader rewriting on a period bounds that to one interval.
// That is the auto-healing an expiry used to give, without the expiry -- which
// is what made an absent key ambiguous in the first place.
func (r *ServiceFactorRegistry) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started {
		return nil
	}

	if err := r.PublishServiceFactors(ctx); err != nil {
		return err
	}

	loopCtx, cancelFn := context.WithCancel(ctx)
	r.cancelFn = cancelFn
	r.started = true

	interval := r.config.RepublishInterval
	r.wg.Add(1)
	go logging.RecoverGoRoutine(r.logger, logging.ComponentServiceFactorRegistry, func(c context.Context) {
		defer r.wg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-c.Done():
				return
			case <-ticker.C:
				if err := r.PublishServiceFactors(c); err != nil {
					// Non-fatal: the manifest already in Redis stays valid, and
					// the next tick retries. Losing a rewrite only widens the
					// window a stale writer could hold, it does not unprice
					// anything.
					r.logger.Warn().
						Err(err).
						Msg("failed to republish service factor manifest, will retry on the next tick")
				}
			}
		}
	})(loopCtx)

	r.logger.Info().
		Dur("republish_interval", interval).
		Msg("service factor registry started")

	return nil
}

// Close stops the republish loop. It is idempotent.
func (r *ServiceFactorRegistry) Close() error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = false
	cancelFn := r.cancelFn
	r.cancelFn = nil
	r.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}
	r.wg.Wait()

	r.logger.Info().Msg("service factor registry stopped")
	return nil
}

// PublishServiceFactors writes the complete service factor state to Redis.
//
// Everything is written in ONE transaction, and the manifest goes LAST, so a
// relayer can never read a manifest promising what the per-key entries do not
// yet say. Nothing carries a TTL: these keys are replaced, never expired.
//
// After writing, it publishes cache invalidation events on the service_factor
// pub/sub channel so live relayers reload instead of serving their L1 copy.
// See cache/service_factor_events.go for the channel contract.
func (r *ServiceFactorRegistry) PublishServiceFactors(ctx context.Context) error {
	manifest := ServiceFactorManifest{
		HasDefault:    r.config.DefaultServiceFactor > 0,
		DefaultFactor: r.config.DefaultServiceFactor,
		Overrides:     make(map[string]float64, len(r.config.ServiceFactors)),
		UpdatedAt:     nowUnix(),
	}
	for serviceID, factor := range r.config.ServiceFactors {
		if factor <= 0 {
			r.logger.Warn().
				Str("service_id", serviceID).
				Float64("factor", factor).
				Msg("ignoring invalid service_factor <= 0")
			continue
		}
		manifest.Overrides[serviceID] = factor
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal service factor manifest: %w", err)
	}

	pipe := r.redisClient.TxPipeline()

	// The per-key entries come first and are the DEPRECATED format: a relayer
	// built before the manifest reads only these, so dropping them here would
	// stop pricing on every such relayer the moment this miner deploys.
	if manifest.HasDefault {
		defaultJSON, marshalErr := json.Marshal(ServiceFactorData{
			Factor:    manifest.DefaultFactor,
			UpdatedAt: manifest.UpdatedAt,
		})
		if marshalErr != nil {
			return fmt.Errorf("failed to marshal default service factor: %w", marshalErr)
		}
		pipe.Set(ctx, r.keyBuilder.ServiceFactorDefaultKey(), defaultJSON, 0)
	}
	for serviceID, factor := range manifest.Overrides {
		serviceJSON, marshalErr := json.Marshal(ServiceFactorData{
			Factor:    factor,
			UpdatedAt: manifest.UpdatedAt,
		})
		if marshalErr != nil {
			return fmt.Errorf("failed to marshal service factor for %s: %w", serviceID, marshalErr)
		}
		pipe.Set(ctx, r.keyBuilder.ServiceFactorServiceKey(serviceID), serviceJSON, 0)
	}

	pipe.Set(ctx, r.keyBuilder.ServiceFactorManifestKey(), manifestJSON, 0)

	if _, err = pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to publish service factor manifest: %w", err)
	}

	// One event per entry, because a relayer on the deprecated format
	// invalidates per key. A relayer on the manifest reloads the whole document
	// on any of them.
	if manifest.HasDefault {
		r.logInvalidationFailure(r.publishInvalidation(ctx, ""), "default")
	}
	for serviceID := range manifest.Overrides {
		r.logInvalidationFailure(r.publishInvalidation(ctx, serviceID), serviceID)
	}
	if !manifest.HasDefault && len(manifest.Overrides) == 0 {
		// Nothing above fired, and a relayer holding a previous manifest must
		// still learn that the operator cleared everything.
		r.logInvalidationFailure(r.publishInvalidation(ctx, ""), "default")
	}

	r.logger.Info().
		Bool("has_default", manifest.HasDefault).
		Float64("default_factor", manifest.DefaultFactor).
		Int("override_count", len(manifest.Overrides)).
		Msg("published service factor manifest to Redis")

	return nil
}

// logInvalidationFailure reports a failed invalidation without failing the
// publish: the values are already in Redis, so a relayer picks them up on its
// next reload, and the stale L1 window is finite.
func (r *ServiceFactorRegistry) logInvalidationFailure(err error, scope string) {
	if err == nil {
		return
	}
	r.logger.Warn().
		Err(err).
		Str("service_id", scope).
		Msg("failed to publish service_factor invalidation event (non-fatal)")
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
	r.logger.Debug().
		Str("scope", scope).
		Str("payload", string(payloadBytes)).
		Msg("published service_factor invalidation event to pub/sub")
	return nil
}

// nowUnix returns the current Unix timestamp.
func nowUnix() int64 {
	return time.Now().Unix()
}
