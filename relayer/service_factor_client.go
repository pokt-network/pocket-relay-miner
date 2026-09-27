package relayer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// ServiceFactorManifest is the complete service factor state published by the
// miner. This struct must match the one in miner/service_factor_registry.go.
type ServiceFactorManifest struct {
	HasDefault    bool               `json:"has_default"`
	DefaultFactor float64            `json:"default_factor"`
	Overrides     map[string]float64 `json:"overrides"`
	UpdatedAt     int64              `json:"updated_at"`
}

// serviceFactorReloadInterval is how often a relayer without a manifest retries.
// It also bounds how long this relayer refuses relays after the miner finally
// publishes one.
const serviceFactorReloadInterval = 2 * time.Second

// ServiceFactorClient holds the miner's service factor manifest.
//
// The whole state arrives in ONE document, loaded at startup and replaced on
// pub/sub, so resolving a factor never reaches Redis: with nothing configured
// the previous per-key lookup issued one GET per relay, measured at 1.858 GET/s
// against 1.855 relays/s on a live run. A service the manifest does not list has
// no override -- that is data, not a failed lookup.
type ServiceFactorClient struct {
	logger      logging.Logger
	redisClient *redisutil.Client

	// manifest is replaced whole, never mutated: readers take the pointer and
	// the map inside is only ever written before that pointer is published, so
	// the hot path needs no lock and no concurrent map.
	//
	// IT IS ALSO MONOTONIC, and that is load-bearing, not a convenience: once a
	// manifest is stored this pointer is never set back to nil, because a failed
	// reload keeps the last good manifest (see loadManifest). Priced() therefore
	// only ever goes from false to true.
	//
	// The WebSocket FRAME path depends on this and has no gate of its own: a
	// connection that was upgraded proves this relayer was priced at the time,
	// and monotonicity is what keeps that true for every frame that follows.
	// Making a reload failure clear this pointer would silently uncover that
	// path -- and would also stop admission across the whole fleet at once on a
	// single blink of Redis.
	manifest atomic.Pointer[ServiceFactorManifest]

	// Lifecycle
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
}

// NewServiceFactorClient creates a new service factor client.
func NewServiceFactorClient(
	logger logging.Logger,
	redisClient *redisutil.Client,
) *ServiceFactorClient {
	return &ServiceFactorClient{
		logger:      logging.ForComponent(logger, logging.ComponentServiceFactorClient),
		redisClient: redisClient,
	}
}

// Start loads the manifest and keeps retrying until it has one.
//
// It does NOT fail the process when the manifest is absent. On any restart of
// the cluster a relayer can come up before the miner has published, and dying
// in a loop would be worse than waiting: instead this relayer reports itself
// unpriced, every transport refuses relays with a counted 503, and admission
// opens by itself as soon as the manifest appears.
func (c *ServiceFactorClient) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	loopCtx, cancelFn := context.WithCancel(ctx)
	c.cancelFn = cancelFn
	c.mu.Unlock()

	if err := c.loadManifest(loopCtx); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("no service factor manifest yet -- relays are refused until the miner publishes one")

		c.wg.Add(1)
		go logging.RecoverGoRoutine(c.logger, logging.ComponentServiceFactorClient, func(rc context.Context) {
			defer c.wg.Done()
			c.retryUntilLoaded(rc)
		})(loopCtx)
	}

	// Subscribe to miner-published invalidation events so a factor change is
	// picked up without restarting the relayer. The subscription reconnects on
	// its own if Redis goes down and comes back.
	if err := cache.SubscribeToInvalidations(
		loopCtx,
		c.redisClient,
		c.logger,
		cache.ServiceFactorCacheType,
		c.handleInvalidation,
	); err != nil {
		c.logger.Warn().
			Err(err).
			Msg("failed to subscribe to service_factor invalidation events — the manifest will not hot-reload")
	}

	c.logger.Info().Bool("priced", c.Priced()).Msg("service factor client started")

	return nil
}

// retryUntilLoaded reloads until a manifest lands or the client closes.
func (c *ServiceFactorClient) retryUntilLoaded(ctx context.Context) {
	ticker := time.NewTicker(serviceFactorReloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.loadManifest(ctx); err == nil {
				c.logger.Info().Msg("service factor manifest loaded -- admitting relays")
				return
			}
		}
	}
}

// loadManifest replaces the held manifest with what Redis holds.
//
// A FAILED RELOAD NEVER DROPS A GOOD MANIFEST, and the distinction between the
// two failures is the whole point. redis.Nil means the miner has not published:
// there is no price, and this relayer must not serve. Any other error -- a
// timeout, a lost connection -- says nothing about what Redis holds, and
// treating it as "no price" would stop admission across the entire fleet on one
// blink of Redis: a worse failure than the one this design fixes.
func (c *ServiceFactorClient) loadManifest(ctx context.Context) error {
	raw, err := c.redisClient.Get(ctx, c.redisClient.KB().ServiceFactorManifestKey()).Bytes()
	switch {
	case err == nil:
		var manifest ServiceFactorManifest
		if unmarshalErr := json.Unmarshal(raw, &manifest); unmarshalErr != nil {
			// A document that does not parse is a defect in the producer. The
			// manifest already held stays: it was valid when it was read.
			c.logger.Warn().Err(unmarshalErr).Msg("service factor manifest could not be parsed -- keeping the last good one")
			return unmarshalErr
		}
		c.manifest.Store(&manifest)
		return nil

	case errors.Is(err, redis.Nil):
		return err

	default:
		c.logger.Debug().Err(err).Msg("failed to read the service factor manifest from Redis")
		return err
	}
}

// Priced reports whether this relayer knows what to charge.
//
// Admission is gated on it: a relay served without a manifest is priced against
// state nobody published, and a price charged wrong is not recoverable. This is
// the opposite call from the boot-window optimistic serve, which is about the
// EXISTENCE of a supplier and is arbitrated afterwards by the miner -- nothing
// arbitrates a wrong price.
//
// It only ever goes from false to true: a failed reload keeps the last good
// manifest, so a relayer that was pricing does not stop.
func (c *ServiceFactorClient) Priced() bool {
	return c.manifest.Load() != nil
}

// handleInvalidation reloads the whole manifest. The payload names a scope the
// per-key format needed; one document is replaced entire, so any event means
// "read it again".
func (c *ServiceFactorClient) handleInvalidation(ctx context.Context, _ string) error {
	if err := c.loadManifest(ctx); err != nil {
		c.logger.Warn().Err(err).Msg("service_factor invalidation arrived but the manifest could not be reloaded")
		return nil
	}
	c.logger.Info().Msg("service factor manifest reloaded via pub/sub")
	return nil
}

// GetServiceFactor returns the service factor for a given service ID.
// Returns (factor, true) if one is configured, (0, false) if none is.
//
// This never reaches Redis: the manifest carries the whole state, so a service
// with no entry is answered from memory instead of costing a GET per relay.
// (0, false) means the protocol formula applies -- the caller must not read it
// as "unknown", which is what Priced answers.
func (c *ServiceFactorClient) GetServiceFactor(_ context.Context, serviceID string) (float64, bool) {
	manifest := c.manifest.Load()
	if manifest == nil {
		return 0, false
	}
	if factor, ok := manifest.Overrides[serviceID]; ok {
		return factor, true
	}
	if manifest.HasDefault {
		return manifest.DefaultFactor, true
	}
	return 0, false
}

// Close gracefully shuts down the service factor client.
func (c *ServiceFactorClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cancelFn := c.cancelFn
	c.cancelFn = nil
	c.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}
	c.wg.Wait()

	c.logger.Info().Msg("service factor client closed")
	return nil
}
