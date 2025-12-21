package miner

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Supplier Claiming Timing Constants
//
// These values are carefully tuned for production reliability and are NOT user-configurable.
// Modifying these could cause claim conflicts, orphaned suppliers, or failover delays.
//
// Timing relationships:
//   - ClaimTTL = 30s: Time before a claim expires if not renewed
//   - RenewRate = 10s: Claims are renewed every 10s (3x before TTL expires)
//   - InstanceTTL = 30s: Instance registration expires after 30s without heartbeat
//   - InstanceHeartbeatRate = 10s: Instance heartbeats every 10s
//   - RebalanceInterval = 30s: Check for rebalancing every 30s
//
// Failover timing:
//   - If a miner crashes, its claims expire after ClaimTTL (30s)
//   - Other miners detect orphaned claims in the next rebalance cycle (~30s)
//   - Maximum failover time: ClaimTTL + RebalanceInterval = ~60s
//
// DO NOT MODIFY THESE VALUES without understanding the full implications.
const (
	// ClaimTTL is how long a supplier claim is valid before it expires.
	// If a miner crashes, other miners can reclaim after this duration.
	// Must be > 2 * RenewRate to allow for network delays.
	ClaimTTL = 30 * time.Second

	// RenewRate is how often to renew claims.
	// Claims are renewed 3 times before TTL expires (30s / 10s = 3x safety margin).
	RenewRate = 10 * time.Second

	// InstanceTTL is how long an instance registration is valid.
	// Matches ClaimTTL for consistency.
	InstanceTTL = 30 * time.Second

	// InstanceHeartbeatRate is how often to heartbeat instance registration.
	// Matches RenewRate for consistency.
	InstanceHeartbeatRate = 10 * time.Second

	// RebalanceInterval is how often to check for rebalancing.
	// When new miners join, suppliers are redistributed within this interval.
	RebalanceInterval = 30 * time.Second
)

// SupplierClaimerConfig contains configuration for the SupplierClaimer.
type SupplierClaimerConfig struct {
	// ClaimTTL is how long a supplier claim is valid before expiring.
	// Default: 30s
	ClaimTTL time.Duration

	// RenewRate is how often to renew claims.
	// Default: 10s (should be < ClaimTTL/2)
	RenewRate time.Duration

	// InstanceTTL is how long an instance registration is valid.
	// Default: 30s
	InstanceTTL time.Duration

	// InstanceHeartbeatRate is how often to heartbeat instance registration.
	// Default: 10s
	InstanceHeartbeatRate time.Duration

	// RebalanceInterval is how often to check for rebalancing.
	// Default: 30s
	RebalanceInterval time.Duration
}

// SupplierClaimer manages distributed supplier claiming using Redis-based leases.
// It provides fair distribution of suppliers across multiple miner instances.
//
// Key features:
// - Lease-based claiming with automatic renewal
// - Fair share rebalancing when miners join/leave
// - Automatic reclaim of orphaned suppliers (failed miners)
// - Instance registration with heartbeat
type SupplierClaimer struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	instanceID  string
	config      SupplierClaimerConfig

	// Claimed suppliers (this instance owns these)
	claimed   map[string]struct{}
	claimedMu sync.RWMutex

	// All configured suppliers (from KeyManager)
	allSuppliers   []string
	allSuppliersMu sync.RWMutex

	// Callbacks
	onClaimFn   func(ctx context.Context, supplier string) error
	onReleaseFn func(ctx context.Context, supplier string) error

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewSupplierClaimer creates a new supplier claimer.
// The claimer uses hardcoded timing constants (ClaimTTL, RenewRate, etc.)
// to ensure production reliability. Config values are ignored.
func NewSupplierClaimer(
	logger logging.Logger,
	redisClient *redisutil.Client,
	instanceID string,
	_ SupplierClaimerConfig, // Config parameter kept for API compatibility but ignored
) *SupplierClaimer {
	// Always use hardcoded constants for production reliability
	config := SupplierClaimerConfig{
		ClaimTTL:              ClaimTTL,
		RenewRate:             RenewRate,
		InstanceTTL:           InstanceTTL,
		InstanceHeartbeatRate: InstanceHeartbeatRate,
		RebalanceInterval:     RebalanceInterval,
	}

	return &SupplierClaimer{
		logger:      logging.ForComponent(logger, logging.ComponentSupplierClaimer),
		redisClient: redisClient,
		instanceID:  instanceID,
		config:      config,
		claimed:     make(map[string]struct{}),
	}
}

// SetCallbacks sets the callbacks for claim and release events.
// onClaimFn is called when a supplier is successfully claimed (should start lifecycle).
// onReleaseFn is called when a supplier is released (should drain and stop lifecycle).
func (c *SupplierClaimer) SetCallbacks(
	onClaimFn func(ctx context.Context, supplier string) error,
	onReleaseFn func(ctx context.Context, supplier string) error,
) {
	c.onClaimFn = onClaimFn
	c.onReleaseFn = onReleaseFn
}

// Start initializes the claimer and begins the claim/renew/rebalance loops.
func (c *SupplierClaimer) Start(ctx context.Context, suppliers []string) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	c.allSuppliersMu.Lock()
	c.allSuppliers = suppliers
	c.allSuppliersMu.Unlock()

	// Log configuration
	c.logger.Info().
		Dur("claim_ttl", c.config.ClaimTTL).
		Dur("renew_rate", c.config.RenewRate).
		Dur("instance_ttl", c.config.InstanceTTL).
		Dur("rebalance_interval", c.config.RebalanceInterval).
		Msg("supplier claimer configuration")

	// Register this instance
	if err := c.registerInstance(ctx); err != nil {
		return fmt.Errorf("failed to register instance: %w", err)
	}

	// Initial claim of suppliers
	if err := c.initialClaim(ctx); err != nil {
		c.logger.Warn().Err(err).Msg("initial claim had errors (will retry in background)")
	}

	// Start background goroutines
	c.wg.Add(3)
	go c.instanceHeartbeatLoop()
	go c.renewLoop()
	go c.rebalanceLoop()

	c.logger.Info().
		Int("suppliers", len(suppliers)).
		Int("claimed", c.ClaimedCount()).
		Msg("supplier claimer started")

	return nil
}

// Stop gracefully shuts down the claimer and releases all claims.
func (c *SupplierClaimer) Stop(ctx context.Context) error {
	if c.cancelFn != nil {
		c.cancelFn()
	}

	// Wait for goroutines to finish
	c.wg.Wait()

	// Release all claims
	c.claimedMu.Lock()
	claimed := make([]string, 0, len(c.claimed))
	for supplier := range c.claimed {
		claimed = append(claimed, supplier)
	}
	c.claimedMu.Unlock()

	for _, supplier := range claimed {
		if err := c.Release(ctx, supplier); err != nil {
			c.logger.Warn().Err(err).Str("supplier", supplier).Msg("failed to release claim on shutdown")
		}
	}

	// Unregister this instance
	if err := c.unregisterInstance(ctx); err != nil {
		c.logger.Warn().Err(err).Msg("failed to unregister instance on shutdown")
	}

	c.logger.Info().Msg("supplier claimer stopped")

	return nil
}

// TryClaim attempts to claim a supplier using Redis SET NX with TTL.
// Returns true if the claim was successful, false if already claimed by another instance.
func (c *SupplierClaimer) TryClaim(ctx context.Context, supplier string) bool {
	claimKey := c.redisClient.KB().MinerClaimKey(supplier)

	// Use SET NX (only set if not exists) with TTL
	success, err := c.redisClient.SetNX(ctx, claimKey, c.instanceID, c.config.ClaimTTL).Result()
	if err != nil {
		c.logger.Error().
			Err(err).
			Str("supplier", supplier).
			Str("claim_key", claimKey).
			Msg("failed to claim supplier")
		return false
	}

	if !success {
		// Already claimed - check if it's by us (renewal case) or another instance
		owner, err := c.redisClient.Get(ctx, claimKey).Result()
		if err == nil && owner == c.instanceID {
			// We already own it, just renew
			if err := c.redisClient.Expire(ctx, claimKey, c.config.ClaimTTL).Err(); err != nil {
				c.logger.Warn().
					Err(err).
					Str("supplier", supplier).
					Msg("failed to renew existing claim")
			}
			return true
		}
		c.logger.Debug().
			Str("supplier", supplier).
			Str("owner", owner).
			Msg("supplier already claimed by another instance")
		return false
	}

	// Successfully claimed
	c.claimedMu.Lock()
	c.claimed[supplier] = struct{}{}
	c.claimedMu.Unlock()

	c.logger.Info().
		Str("supplier", supplier).
		Str("claim_key", claimKey).
		Dur("ttl", c.config.ClaimTTL).
		Msg("claimed supplier")

	supplierClaimedTotal.WithLabelValues(supplier, c.instanceID).Inc()

	// Invoke claim callback
	if c.onClaimFn != nil {
		if err := c.onClaimFn(ctx, supplier); err != nil {
			c.logger.Error().
				Err(err).
				Str("supplier", supplier).
				Msg("claim callback failed")
			// Release the claim since we couldn't start lifecycle
			_ = c.Release(ctx, supplier)
			return false
		}
	}

	return true
}

// Release releases a supplier claim.
func (c *SupplierClaimer) Release(ctx context.Context, supplier string) error {
	claimKey := c.redisClient.KB().MinerClaimKey(supplier)

	// Only delete if we own it (atomic check-and-delete)
	// Use Lua script to ensure atomicity
	script := redis.NewScript(`
		if redis.call("get", KEYS[1]) == ARGV[1] then
			return redis.call("del", KEYS[1])
		else
			return 0
		end
	`)

	result, err := script.Run(ctx, c.redisClient, []string{claimKey}, c.instanceID).Int64()
	if err != nil {
		return fmt.Errorf("failed to release claim: %w", err)
	}

	if result == 0 {
		// We didn't own it, but still update local state
		c.logger.Debug().Str("supplier", supplier).Msg("claim was not owned by us")
	}

	c.claimedMu.Lock()
	delete(c.claimed, supplier)
	c.claimedMu.Unlock()

	c.logger.Info().
		Str("supplier", supplier).
		Msg("released supplier claim")

	supplierReleasedTotal.WithLabelValues(supplier, c.instanceID).Inc()

	// Invoke release callback
	if c.onReleaseFn != nil {
		if err := c.onReleaseFn(ctx, supplier); err != nil {
			c.logger.Error().
				Err(err).
				Str("supplier", supplier).
				Msg("release callback failed")
			// Continue anyway - claim is released
		}
	}

	return nil
}

// IsClaimed returns true if the supplier is claimed by this instance.
func (c *SupplierClaimer) IsClaimed(supplier string) bool {
	c.claimedMu.RLock()
	defer c.claimedMu.RUnlock()
	_, ok := c.claimed[supplier]
	return ok
}

// ClaimedCount returns the number of suppliers claimed by this instance.
func (c *SupplierClaimer) ClaimedCount() int {
	c.claimedMu.RLock()
	defer c.claimedMu.RUnlock()
	return len(c.claimed)
}

// ClaimedSuppliers returns a copy of the claimed suppliers set.
func (c *SupplierClaimer) ClaimedSuppliers() []string {
	c.claimedMu.RLock()
	defer c.claimedMu.RUnlock()
	suppliers := make([]string, 0, len(c.claimed))
	for supplier := range c.claimed {
		suppliers = append(suppliers, supplier)
	}
	return suppliers
}

// registerInstance registers this miner instance in Redis.
func (c *SupplierClaimer) registerInstance(ctx context.Context) error {
	instanceKey := c.redisClient.KB().MinerInstanceKey(c.instanceID)
	activeSetKey := c.redisClient.KB().MinerActiveSetKey()

	// Set instance key with TTL
	if err := c.redisClient.Set(ctx, instanceKey, time.Now().UnixNano(), c.config.InstanceTTL).Err(); err != nil {
		return fmt.Errorf("failed to set instance key: %w", err)
	}

	// Add to active set
	if err := c.redisClient.SAdd(ctx, activeSetKey, c.instanceID).Err(); err != nil {
		return fmt.Errorf("failed to add to active set: %w", err)
	}

	c.logger.Debug().
		Msg("registered miner instance")

	return nil
}

// unregisterInstance removes this miner instance from Redis.
func (c *SupplierClaimer) unregisterInstance(ctx context.Context) error {
	instanceKey := c.redisClient.KB().MinerInstanceKey(c.instanceID)
	activeSetKey := c.redisClient.KB().MinerActiveSetKey()

	// Remove from active set
	c.redisClient.SRem(ctx, activeSetKey, c.instanceID)

	// Delete instance key
	c.redisClient.Del(ctx, instanceKey)

	c.logger.Debug().
		Msg("unregistered miner instance")

	return nil
}

// initialClaim attempts to claim suppliers on startup.
func (c *SupplierClaimer) initialClaim(ctx context.Context) error {
	c.allSuppliersMu.RLock()
	suppliers := make([]string, len(c.allSuppliers))
	copy(suppliers, c.allSuppliers)
	c.allSuppliersMu.RUnlock()

	// Calculate fair share
	fairShare := c.calculateFairShare(ctx)

	var errors int
	for _, supplier := range suppliers {
		if c.ClaimedCount() >= fairShare {
			break // Already at fair share
		}

		if !c.TryClaim(ctx, supplier) {
			errors++
		}
	}

	c.logger.Info().
		Int("fair_share", fairShare).
		Int("claimed", c.ClaimedCount()).
		Int("errors", errors).
		Msg("initial claim complete")

	return nil
}

// calculateFairShare calculates the fair share of suppliers for this instance.
func (c *SupplierClaimer) calculateFairShare(ctx context.Context) int {
	activeSetKey := c.redisClient.KB().MinerActiveSetKey()

	// Get active miner count
	activeMiners, err := c.redisClient.SCard(ctx, activeSetKey).Result()
	if err != nil || activeMiners == 0 {
		activeMiners = 1 // At least this instance
	}

	c.allSuppliersMu.RLock()
	totalSuppliers := len(c.allSuppliers)
	c.allSuppliersMu.RUnlock()

	if totalSuppliers == 0 {
		return 0
	}

	// Fair share = ceil(totalSuppliers / activeMiners)
	fairShare := int(math.Ceil(float64(totalSuppliers) / float64(activeMiners)))

	return fairShare
}

// instanceHeartbeatLoop periodically renews instance registration.
func (c *SupplierClaimer) instanceHeartbeatLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.InstanceHeartbeatRate)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := c.registerInstance(c.ctx); err != nil {
				c.logger.Warn().Err(err).Msg("failed to heartbeat instance")
			}

			// Clean up stale instances from active set
			c.cleanupStaleInstances()
		}
	}
}

// cleanupStaleInstances removes instances whose keys have expired.
func (c *SupplierClaimer) cleanupStaleInstances() {
	activeSetKey := c.redisClient.KB().MinerActiveSetKey()

	// Get all instances in the set
	instances, err := c.redisClient.SMembers(c.ctx, activeSetKey).Result()
	if err != nil {
		return
	}

	for _, instanceID := range instances {
		instanceKey := c.redisClient.KB().MinerInstanceKey(instanceID)

		// Check if instance key exists
		exists, err := c.redisClient.Exists(c.ctx, instanceKey).Result()
		if err != nil {
			continue
		}

		if exists == 0 {
			// Instance key expired, remove from set
			c.redisClient.SRem(c.ctx, activeSetKey, instanceID)
			c.logger.Debug().
				Str("stale_instance", instanceID).
				Msg("removed stale instance from active set")
		}
	}
}

// renewLoop periodically renews all claimed supplier leases.
func (c *SupplierClaimer) renewLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.RenewRate)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.renewAllClaims()
		}
	}
}

// renewAllClaims renews all claimed supplier leases.
func (c *SupplierClaimer) renewAllClaims() {
	c.claimedMu.RLock()
	claimed := make([]string, 0, len(c.claimed))
	for supplier := range c.claimed {
		claimed = append(claimed, supplier)
	}
	c.claimedMu.RUnlock()

	if len(claimed) == 0 {
		return
	}

	c.logger.Debug().
		Int("claim_count", len(claimed)).
		Dur("ttl", c.config.ClaimTTL).
		Msg("renewing claims")

	for _, supplier := range claimed {
		claimKey := c.redisClient.KB().MinerClaimKey(supplier)

		// Renew only if we still own it (check-and-renew)
		owner, err := c.redisClient.Get(c.ctx, claimKey).Result()
		if err != nil {
			if err == redis.Nil {
				// Claim expired, try to reclaim
				c.logger.Warn().
					Str("supplier", supplier).
					Str("claim_key", claimKey).
					Msg("claim expired, attempting reclaim")
				c.claimedMu.Lock()
				delete(c.claimed, supplier)
				c.claimedMu.Unlock()
				c.TryClaim(c.ctx, supplier)
			} else {
				c.logger.Warn().
					Err(err).
					Str("supplier", supplier).
					Msg("failed to get claim owner")
			}
			continue
		}

		if owner != c.instanceID {
			// Someone else claimed it
			c.logger.Warn().
				Str("supplier", supplier).
				Str("owner", owner).
				Str("expected", c.instanceID).
				Msg("claim stolen by another instance")
			c.claimedMu.Lock()
			delete(c.claimed, supplier)
			c.claimedMu.Unlock()
			continue
		}

		// Renew the lease
		if err := c.redisClient.Expire(c.ctx, claimKey, c.config.ClaimTTL).Err(); err != nil {
			c.logger.Warn().Err(err).Str("supplier", supplier).Msg("failed to renew claim")
		} else {
			c.logger.Debug().
				Str("supplier", supplier).
				Dur("ttl", c.config.ClaimTTL).
				Msg("renewed claim")
		}
	}
}

// rebalanceLoop periodically checks and rebalances supplier distribution.
func (c *SupplierClaimer) rebalanceLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.RebalanceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.rebalance()
		}
	}
}

// rebalance adjusts supplier claims to achieve fair distribution.
func (c *SupplierClaimer) rebalance() {
	fairShare := c.calculateFairShare(c.ctx)
	currentCount := c.ClaimedCount()

	supplierClaimedGauge.WithLabelValues(c.instanceID).Set(float64(currentCount))
	supplierFairShareGauge.WithLabelValues(c.instanceID).Set(float64(fairShare))

	c.logger.Debug().
		Int("fair_share", fairShare).
		Int("current", currentCount).
		Msg("rebalance check")

	if currentCount > fairShare {
		// Release excess suppliers
		excess := currentCount - fairShare
		c.releaseExcess(excess)
	} else if currentCount < fairShare {
		// Try to claim more suppliers
		needed := fairShare - currentCount
		c.claimMore(needed)
	}
}

// releaseExcess releases excess suppliers to allow other miners to claim them.
func (c *SupplierClaimer) releaseExcess(count int) {
	c.claimedMu.RLock()
	claimed := make([]string, 0, len(c.claimed))
	for supplier := range c.claimed {
		claimed = append(claimed, supplier)
	}
	c.claimedMu.RUnlock()

	released := 0
	for _, supplier := range claimed {
		if released >= count {
			break
		}

		if err := c.Release(c.ctx, supplier); err != nil {
			c.logger.Warn().Err(err).Str("supplier", supplier).Msg("failed to release excess supplier")
			continue
		}

		released++
		c.logger.Info().
			Str("supplier", supplier).
			Int("released", released).
			Int("target", count).
			Msg("released supplier for rebalancing")
	}
}

// claimMore attempts to claim unclaimed suppliers.
func (c *SupplierClaimer) claimMore(count int) {
	c.allSuppliersMu.RLock()
	suppliers := make([]string, len(c.allSuppliers))
	copy(suppliers, c.allSuppliers)
	c.allSuppliersMu.RUnlock()

	claimed := 0
	for _, supplier := range suppliers {
		if claimed >= count {
			break
		}

		if c.IsClaimed(supplier) {
			continue // Already claimed by us
		}

		if c.TryClaim(c.ctx, supplier) {
			claimed++
			c.logger.Info().
				Str("supplier", supplier).
				Int("claimed", claimed).
				Int("target", count).
				Msg("claimed additional supplier for rebalancing")
		}
	}
}

// UpdateSuppliers updates the list of configured suppliers.
// Called when KeyManager detects a config change.
func (c *SupplierClaimer) UpdateSuppliers(suppliers []string) {
	c.allSuppliersMu.Lock()
	c.allSuppliers = suppliers
	c.allSuppliersMu.Unlock()

	c.logger.Info().
		Int("suppliers", len(suppliers)).
		Msg("updated supplier list")

	// Trigger rebalance
	go c.rebalance()
}
