package miner

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Supplier Claiming Default Timing Constants
//
// These defaults are used when no YAML config is provided. They are tuned for
// production reliability with high supplier counts (500+).
//
// Timing relationships:
//   - ClaimTTL = 90s: Time before a claim expires if not renewed
//   - RenewRate = 10s: Claims are renewed 9x before TTL expires (90s / 10s)
//   - InstanceTTL = 90s: Instance registration expires after 90s without heartbeat
//   - InstanceHeartbeatRate = 10s: Instance heartbeats every 10s
//   - RebalanceInterval = 30s: Check for rebalancing every 30s
//
// Failover timing:
//   - If a miner crashes, its claims expire after ClaimTTL (90s)
//   - Other miners detect orphaned claims in the next rebalance cycle (~30s)
//   - Maximum failover time: ClaimTTL + RebalanceInterval = ~120s
//
// These can be overridden via supplier_claiming config in miner YAML.
const (
	// ClaimTTL is how long a supplier claim is valid before it expires.
	// If a miner crashes, other miners can reclaim after this duration.
	// Set to 90s (up from 30s) to give the sequential renewal loop sufficient
	// headroom with high supplier counts (500+) where renewal can take 4-8s.
	ClaimTTL = 90 * time.Second

	// RenewRate is how often to renew claims.
	// Claims are renewed 9x before TTL expires (90s / 10s = 9x safety margin).
	RenewRate = 10 * time.Second

	// InstanceTTL is how long an instance registration is valid.
	// Matches ClaimTTL for consistency.
	InstanceTTL = 90 * time.Second

	// InstanceHeartbeatRate is how often to heartbeat instance registration.
	// Matches RenewRate for consistency.
	InstanceHeartbeatRate = 10 * time.Second

	// RebalanceInterval is how often to check for rebalancing.
	// When new miners join, suppliers are redistributed within this interval.
	RebalanceInterval = 30 * time.Second
)

// Drain triggers. They are a BOUNDED set on purpose: the value reaches a
// Prometheus label, and it is the word an operator reads first during an
// incident. "claim_expiry" was already documented as a drain_reason in
// metrics.go and never had a writer -- these are its writers.
const (
	// triggerLeaseExpired: the lease key was gone and we could not get it back.
	triggerLeaseExpired = "lease_expired"
	// triggerLeaseStolen: the key holds another instance's id.
	triggerLeaseStolen = "lease_stolen"
	// triggerRenewStalled: no renewal has succeeded for longer than ClaimTTL.
	triggerRenewStalled = "renew_stalled"
	// triggerRebalanceRelease: we handed the supplier to a peer on purpose.
	triggerRebalanceRelease = "rebalance_release"
	// triggerKeyRemoval: the operator removed the signing key.
	triggerKeyRemoval = "key_removal"
	// triggerClaimCallbackFailed: we claimed it but could not start it.
	triggerClaimCallbackFailed = "claim_callback_failed"
	// triggerConsumeLoopPanicked: its consume loop used up its panic budget.
	triggerConsumeLoopPanicked = "consume_loop_panicked"
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
	// Maps supplier address to the time it was claimed, enabling newest-first
	// release ordering during rebalancing to minimize session handoff churn.
	claimed   map[string]time.Time
	claimedMu sync.RWMutex

	// Suppliers we released recently (rebalance handoff to peers). Used by
	// claimOrphaned to skip keys we just gave up, otherwise our own next
	// tick would race the peer's rebalance and re-grab everything we
	// released. Entries are lazily pruned after recentlyReleasedCooldown.
	recentlyReleased   map[string]time.Time
	recentlyReleasedMu sync.Mutex

	// draining holds the suppliers this instance gave up whose drain has not
	// finished. A released supplier's lease key stays OURS until FinishRelease,
	// so the old consume loop is the only writer while it winds down, and every
	// path that reads "the key is mine" -- TryClaim, releaseLost, claimMore,
	// claimOrphaned -- must know about it, or it takes back a supplier that is
	// still being torn down.
	draining   map[string]drainLease
	drainingMu sync.Mutex

	// All configured suppliers (from KeyManager)
	allSuppliers   []string
	allSuppliersMu sync.RWMutex

	// lastRenewedAt is when this instance last CONFIRMED it still owns each
	// lease. It is the only lease signal that does not come from Redis, and
	// that is the point: the branch below where the GET itself fails is the
	// one branch reachable when the broken thing IS Redis, so it cannot ask
	// Redis whether the lease survived. Once no renewal has succeeded for
	// longer than ClaimTTL the key cannot still be ours, whatever Redis says.
	lastRenewedAt   map[string]time.Time
	lastRenewedAtMu sync.Mutex

	// nowFn is a struct field rather than a direct time.Now call so a test can
	// move the clock. It is set ONCE, in the constructor, which runs on the
	// caller's goroutine.
	//
	// THE CONTRACT, stated because the obvious stronger claim is false: the
	// renewal loop DOES read it from the goroutine Start spawns. What makes
	// that safe is that nothing writes it after construction except a test,
	// and a test may only write it with the loop STOPPED. Mutating it while
	// the loop runs is a data race. (CLAUDE.md, wsFirstFrameWait: the field
	// capture is what removes the race a package var had; it does not make
	// later writes safe.)
	nowFn func() time.Time

	// Callbacks
	onClaimFn func(ctx context.Context, supplier string) error
	// onReleaseFn carries the trigger because the drain path labels a metric
	// and an audit log with it. Passing it means an operator reading
	// "lost a lease to a peer" is not sent to look at the rebalancer.
	onReleaseFn func(ctx context.Context, supplier, trigger string) error

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// drainLease is what the claimer knows about a supplier being drained: why it
// was given up, and until when ExtendDrainLease keeps its key (zero until then).
type drainLease struct {
	trigger  string
	deadline time.Time
}

// releaseLeaseScript deletes a lease only if this instance still holds it.
var releaseLeaseScript = redis.NewScript(`
	if redis.call("get", KEYS[1]) == ARGV[1] then
		return redis.call("del", KEYS[1])
	else
		return 0
	end
`)

// extendDrainLeaseScript sets a lease's TTL, in milliseconds, only if this
// instance still holds it.
var extendDrainLeaseScript = redis.NewScript(`
	if redis.call("get", KEYS[1]) == ARGV[1] then
		return redis.call("pexpire", KEYS[1], ARGV[2])
	else
		return 0
	end
`)

// NewSupplierClaimer creates a new supplier claimer.
// Uses the provided config values. Zero values fall back to the package-level
// constants (ClaimTTL=90s, RenewRate=10s, etc.).
func NewSupplierClaimer(
	logger logging.Logger,
	redisClient *redisutil.Client,
	instanceID string,
	cfg SupplierClaimerConfig,
) *SupplierClaimer {
	// Apply defaults for any zero-value fields
	if cfg.ClaimTTL <= 0 {
		cfg.ClaimTTL = ClaimTTL
	}
	if cfg.RenewRate <= 0 {
		cfg.RenewRate = RenewRate
	}
	if cfg.InstanceTTL <= 0 {
		cfg.InstanceTTL = cfg.ClaimTTL // Match ClaimTTL
	}
	if cfg.InstanceHeartbeatRate <= 0 {
		cfg.InstanceHeartbeatRate = cfg.RenewRate // Match RenewRate
	}
	if cfg.RebalanceInterval <= 0 {
		cfg.RebalanceInterval = RebalanceInterval
	}

	componentLogger := logging.ForComponent(logger, logging.ComponentSupplierClaimer)
	componentLogger.Info().
		Dur("claim_ttl", cfg.ClaimTTL).
		Dur("renew_rate", cfg.RenewRate).
		Dur("rebalance_interval", cfg.RebalanceInterval).
		Msg("supplier claimer timing configuration")

	return &SupplierClaimer{
		logger:           componentLogger,
		redisClient:      redisClient,
		instanceID:       instanceID,
		config:           cfg,
		claimed:          make(map[string]time.Time),
		recentlyReleased: make(map[string]time.Time),
		draining:         make(map[string]drainLease),
		lastRenewedAt:    make(map[string]time.Time),
		nowFn:            time.Now,
	}
}

// SetCallbacks sets the callbacks for claim and release events.
// onClaimFn is called when a supplier is successfully claimed (should start lifecycle).
// onReleaseFn is called when a supplier is released (should drain and stop lifecycle).
func (c *SupplierClaimer) SetCallbacks(
	onClaimFn func(ctx context.Context, supplier string) error,
	onReleaseFn func(ctx context.Context, supplier, trigger string) error,
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

// StopLoops stops the heartbeat, renewal and rebalance loops WITHOUT releasing
// anything: the leases stay this instance's while the manager tears its
// suppliers down, and FinishShutdown deletes them afterwards. Releasing first
// freed every key while the old consume loops could still write.
func (c *SupplierClaimer) StopLoops() {
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.wg.Wait()
}

// FinishShutdown deletes every lease this instance still holds, each only if it
// is still ours, and unregisters the instance. The manager calls it once its
// suppliers are torn down; a shutdown that never gets here leaves the keys to
// expire on their TTL.
func (c *SupplierClaimer) FinishShutdown(ctx context.Context) {
	for _, supplier := range c.ClaimedSuppliers() {
		c.claimedMu.Lock()
		delete(c.claimed, supplier)
		c.claimedMu.Unlock()

		if err := c.deleteLeaseIfOurs(ctx, supplier); err != nil {
			c.logger.Warn().Err(err).Str("supplier", supplier).Msg("failed to release claim on shutdown")
			continue
		}
		c.logger.Info().
			Str("supplier", supplier).
			Msg("released supplier claim")
		supplierReleasedTotal.WithLabelValues(supplier, c.instanceID).Inc()
	}

	if err := c.unregisterInstance(ctx); err != nil {
		c.logger.Warn().Err(err).Msg("failed to unregister instance on shutdown")
	}

	c.logger.Info().Msg("supplier claimer stopped")
}

// TryClaim attempts to claim a supplier using Redis SET NX with TTL.
// Returns true if the claim was successful, false if already claimed by another instance.
func (c *SupplierClaimer) TryClaim(ctx context.Context, supplier string) bool {
	if c.isDraining(supplier) {
		// Still being torn down. Taken back now, a new consume loop would start
		// while the old one still writes.
		return false
	}

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

	adopted := false
	if !success {
		// Already claimed - check if it's by us (renewal case) or another instance
		owner, err := c.redisClient.Get(ctx, claimKey).Result()
		if err == nil && owner == c.instanceID {
			if c.isDraining(supplier) {
				// Released since the check above: the key is ours only because
				// its drain still holds it. Renewing it here would count a
				// supplier claimed that nothing is going to start.
				return false
			}
			// We already own it, just renew - check result to ensure it worked
			renewed, expireErr := c.redisClient.Expire(ctx, claimKey, c.config.ClaimTTL).Result()
			if expireErr != nil {
				c.logger.Warn().
					Err(expireErr).
					Str("supplier", supplier).
					Msg("failed to renew existing claim")
				return false
			}
			if !renewed {
				c.logger.Warn().
					Str("supplier", supplier).
					Msg("claim key disappeared during renewal in TryClaim")
				return false
			}
			// Ours in Redis, and nothing here runs it -- a release whose final
			// delete failed leaves exactly this. Answering true here counted a
			// claim that nothing had started; it is taken the whole way instead,
			// as a SETNX that succeeded is. Checked and inserted under one lock:
			// two callers can both get this far, and only the one that inserts
			// may start the supplier.
			c.claimedMu.Lock()
			_, running := c.claimed[supplier]
			if !running {
				c.claimed[supplier] = time.Now()
			}
			c.claimedMu.Unlock()
			if running {
				return true // a renewal of a supplier this instance runs
			}
			adopted = true
			c.logger.Info().
				Str("supplier", supplier).
				Msg("lease already held by this instance with nothing running it; claiming it")
		} else {
			c.logger.Debug().
				Str("supplier", supplier).
				Str("owner", owner).
				Msg("supplier already claimed by another instance")
			return false
		}
	}

	if !adopted {
		// Successfully claimed — record timestamp for newest-first release ordering
		c.claimedMu.Lock()
		c.claimed[supplier] = time.Now()
		c.claimedMu.Unlock()
	}

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
			// Release the claim since we couldn't start lifecycle.
			//
			// The drain Release starts deletes the key when it ends. If Release
			// fails instead, the claim is kept and the key stays held -- no
			// replica takes this supplier until shutdown or its TTL, while this
			// one has already given up on it. Silent, it looks like a supplier
			// nobody wanted.
			if relErr := c.Release(ctx, supplier, triggerClaimCallbackFailed); relErr != nil {
				c.logger.Warn().
					Err(relErr).
					Str("supplier", supplier).
					Msg("could not release the claim after a failed callback; it stays held until its TTL expires")
			}
			return false
		}
	}

	return true
}

// Release hands a supplier over.
//
// The lease key is NOT deleted here. It stays this instance's until the drain
// the release callback starts has torn the supplier down, and FinishRelease
// deletes it then: deleted first, a peer could claim the supplier and start
// writing its tree and acknowledging its relays while this instance's consume
// loop still did the same. With a callback -- the manager always sets one --
// nothing here touches Redis, so the serial renewal and rebalance loops that
// call it are not held up by a slow Redis or a slow drain.
//
// If the callback fails the release is undone and its error returned: the
// supplier is back in the map and renewed as before. With no callback there is
// no drain, and the key is deleted here. A supplier already draining is left to
// the drain under way.
func (c *SupplierClaimer) Release(ctx context.Context, supplier, trigger string) error {
	// draining goes up FIRST: between the map delete below and the mark, a
	// claimMore on another goroutine would find the supplier neither claimed nor
	// draining, and TryClaim's "already ours" branch would count it claimed with
	// nothing started, since the key is still ours.
	if !c.markDraining(supplier, trigger) {
		c.logger.Debug().Str("supplier", supplier).Msg("supplier already being released")
		return nil
	}

	// Give the supplier up LOCALLY before the key goes. renewAllClaims runs on
	// its own goroutine and reads a missing key as a lease that expired, which
	// it takes back -- unless the map says this instance let it go. With the
	// key deleted first, a renewal landing between the DEL and
	// these writes re-took the supplier and then had the re-take erased from
	// the map: a lease held in Redis that nobody renewed, blocking every peer
	// for its TTL (L3 of df5441c, 2026-09-11: one supplier re-taken and
	// released 31 times). Also the reason claimOrphaned must not race the peer
	// that is supposed to pick it up.
	c.claimedMu.Lock()
	claimedAt, wasClaimed := c.claimed[supplier]
	delete(c.claimed, supplier)
	c.claimedMu.Unlock()

	c.recentlyReleasedMu.Lock()
	c.recentlyReleased[supplier] = time.Now()
	c.recentlyReleasedMu.Unlock()

	if c.onReleaseFn != nil {
		if err := c.onReleaseFn(ctx, supplier, trigger); err != nil {
			if wasClaimed {
				c.claimedMu.Lock()
				c.claimed[supplier] = claimedAt
				c.claimedMu.Unlock()
			}
			c.unmarkDraining(supplier)
			c.logger.Info().
				Err(err).
				Str("supplier", supplier).
				Msg("release callback failed, keeping claim")
			return err
		}
	} else if err := c.FinishRelease(ctx, supplier); err != nil {
		return err
	}

	c.logger.Info().
		Str("supplier", supplier).
		Msg("released supplier claim")

	supplierReleasedTotal.WithLabelValues(supplier, c.instanceID).Inc()

	return nil
}

// ExtendDrainLease sets the lease of a supplier being drained to budget, if this
// instance still holds it, and records the deadline FinishRelease checks. A
// released supplier has left the claimed map, so no renewal pass started after
// the release touches its key: the budget is how long this instance keeps it,
// and a drain that hangs gives the supplier up when the key expires.
func (c *SupplierClaimer) ExtendDrainLease(ctx context.Context, supplier string, budget time.Duration) error {
	c.drainingMu.Lock()
	if lease, ok := c.draining[supplier]; ok {
		lease.deadline = c.nowFn().Add(budget)
		c.draining[supplier] = lease
	}
	c.drainingMu.Unlock()

	claimKey := c.redisClient.KB().MinerClaimKey(supplier)
	if err := extendDrainLeaseScript.Run(ctx, c.redisClient, []string{claimKey}, c.instanceID, budget.Milliseconds()).Err(); err != nil {
		return fmt.Errorf("failed to extend drain lease: %w", err)
	}
	return nil
}

// FinishRelease ends a release: it deletes the lease, only if this instance
// still holds it, and then takes the supplier out of draining -- in that order,
// because a supplier out of draining whose key is still ours is exactly what
// TryClaim's "already ours" branch miscounts. The manager calls it at the end
// of every drain, whatever the drain found, so no supplier stays draining, and
// unclaimable by this instance, after its teardown. draining is cleared even
// when the delete fails; the key then expires on its drain budget.
//
// A drain that outran the budget is counted: its key may have expired, and a
// peer taken the supplier, while the old consume loop could still write.
func (c *SupplierClaimer) FinishRelease(ctx context.Context, supplier string) error {
	err := c.deleteLeaseIfOurs(ctx, supplier)
	lease := c.unmarkDraining(supplier)

	if !lease.deadline.IsZero() && c.nowFn().After(lease.deadline) {
		c.logger.Warn().
			Str("supplier", supplier).
			Str("trigger", lease.trigger).
			Msg("supplier drain outran its lease budget; a peer may have claimed it while this instance still wrote")
		supplierDrainLeaseOverrunTotal.WithLabelValues(lease.trigger, c.instanceID).Inc()
	}

	if err != nil {
		return fmt.Errorf("failed to release claim: %w", err)
	}
	return nil
}

// deleteLeaseIfOurs deletes the supplier's lease key only if this instance
// still holds it.
func (c *SupplierClaimer) deleteLeaseIfOurs(ctx context.Context, supplier string) error {
	claimKey := c.redisClient.KB().MinerClaimKey(supplier)
	result, err := releaseLeaseScript.Run(ctx, c.redisClient, []string{claimKey}, c.instanceID).Int64()
	if err != nil {
		return err
	}
	if result == 0 {
		c.logger.Debug().Str("supplier", supplier).Msg("claim was not owned by us")
	}
	return nil
}

// markDraining marks the supplier as being drained, and reports false if it
// already was.
func (c *SupplierClaimer) markDraining(supplier, trigger string) bool {
	c.drainingMu.Lock()
	defer c.drainingMu.Unlock()
	if _, ok := c.draining[supplier]; ok {
		return false
	}
	c.draining[supplier] = drainLease{trigger: trigger}
	return true
}

// unmarkDraining takes the supplier out of draining and returns what was known
// about its drain.
func (c *SupplierClaimer) unmarkDraining(supplier string) drainLease {
	c.drainingMu.Lock()
	defer c.drainingMu.Unlock()
	lease := c.draining[supplier]
	delete(c.draining, supplier)
	return lease
}

// isDraining reports whether the supplier is released but its drain has not
// finished.
func (c *SupplierClaimer) isDraining(supplier string) bool {
	c.drainingMu.Lock()
	defer c.drainingMu.Unlock()
	_, ok := c.draining[supplier]
	return ok
}

// releaseLost reports a lease this instance NO LONGER HOLDS.
//
// It is deliberately not Release. Release means "I am handing over something I
// have": its drain deletes the Redis key, and it aborts, keeping the claim, if
// the callback fails. Losing a lease means "I acknowledge something that is no
// longer mine" -- there is no key of ours to delete, and there is no claim to
// keep. Reusing Release here would make both of those sentences false.
//
// The three things it does belong together, and the reason this helper exists
// at all is that they were NOT together: the renewal loop grew three branches
// that each dropped the supplier from the local map and told nobody, and the
// divergence between them IS the defect. One place, one meaning.
//
// The supplier is marked draining, as a release marks it: until the drain this
// call starts has finished, TryClaim refuses it, so no re-take can start a new
// consume loop -- or be deleted by the teardown -- while the old one winds down.
// The cooldown write only keeps claimOrphaned off it for a while longer; it is
// a fairness window, not a correctness guard, since claimMore does not consult
// it. A supplier already draining was released first, and that drain owns the
// teardown: reporting it lost as well would count a hand-over as a lost lease.
//
// CALLER CONTRACT: only call this once the caller has established that recovery
// FAILED. Releasing first and re-claiming afterwards manufactures exactly the
// race described above.
func (c *SupplierClaimer) releaseLost(ctx context.Context, supplier, trigger string) {
	if !c.markDraining(supplier, trigger) {
		return
	}

	c.claimedMu.Lock()
	delete(c.claimed, supplier)
	c.claimedMu.Unlock()

	c.lastRenewedAtMu.Lock()
	delete(c.lastRenewedAt, supplier)
	c.lastRenewedAtMu.Unlock()

	c.recentlyReleasedMu.Lock()
	c.recentlyReleased[supplier] = c.nowFn()
	c.recentlyReleasedMu.Unlock()

	// A lease lost is a state change, not a per-request event: it fires once
	// per change and it is what an operator reads during an incident.
	c.logger.Warn().
		Str("supplier", supplier).
		Str("trigger", trigger).
		Str("instance_id", c.instanceID).
		Msg("lease lost; draining supplier")

	// NOT supplierReleasedTotal: that series means "handed a supplier over on
	// purpose", and a lease we lost is the opposite event. Sharing it would
	// make every existing release panel count more with no way to say why --
	// the exact confusion the trigger exists to prevent.
	supplierLeaseLostTotal.WithLabelValues(trigger, c.instanceID).Inc()

	if c.onReleaseFn == nil {
		c.unmarkDraining(supplier) // no drain, so nothing will finish it
		return
	}
	if err := c.onReleaseFn(ctx, supplier, trigger); err != nil {
		// Nothing to roll back: the lease is gone either way. No drain started,
		// so nothing will take it out of draining but this. Surface it, because
		// a failed drain leaves the supplier live in the manager, which is the
		// very condition this call exists to end.
		c.unmarkDraining(supplier)
		c.logger.Error().
			Err(err).
			Str("supplier", supplier).
			Str("trigger", trigger).
			Msg("drain callback failed after losing a lease; supplier may still be live")
	}
}

// recentlyReleasedCooldown is how long this instance must wait before
// re-claiming a supplier it just released. Must be greater than one
// rebalance interval so peer miners have time to observe the freed claim key
// and pick it up; otherwise this instance's own next tick would race them via
// claimOrphaned. 2x gives a full peer cycle of headroom plus jitter buffer. It
// is the CONFIGURED interval's: derived from the default, a miner configured
// with a longer interval re-claimed before its peers had a tick to take the
// supplier.
func (c *SupplierClaimer) recentlyReleasedCooldown() time.Duration {
	return 2 * c.config.RebalanceInterval
}

// inRecentReleaseCooldown reports whether this instance released the
// supplier within the last recentlyReleasedCooldown window. Side effect:
// prunes the entry if it has aged out, so the map stays bounded by the
// active supplier count.
func (c *SupplierClaimer) inRecentReleaseCooldown(supplier string) bool {
	c.recentlyReleasedMu.Lock()
	defer c.recentlyReleasedMu.Unlock()
	releasedAt, ok := c.recentlyReleased[supplier]
	if !ok {
		return false
	}
	if time.Since(releasedAt) >= c.recentlyReleasedCooldown() {
		delete(c.recentlyReleased, supplier)
		return false
	}
	return true
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

	// now is read here, on this goroutine, and passed down. See nowFn.
	now := c.nowFn()

	for _, supplier := range claimed {
		claimKey := c.redisClient.KB().MinerClaimKey(supplier)

		// Renew only if we still own it (check-and-renew)
		owner, err := c.redisClient.Get(c.ctx, claimKey).Result()
		if err != nil {
			if err == redis.Nil {
				// The lease expired. Try to take it back FIRST: the bool
				// decides. Recovering means the supplier is still ours and
				// draining it would destroy a live one; failing means a peer
				// has it and the manager has to be told, which is the whole
				// point of this pass.
				if c.releasedSinceSnapshot(supplier) {
					continue // this instance gave it up during the pass: the key is gone on purpose
				}
				c.logger.Warn().
					Str("supplier", supplier).
					Str("claim_key", claimKey).
					Msg("claim expired, attempting reclaim")
				if !c.TryClaim(c.ctx, supplier) {
					c.releaseLost(c.ctx, supplier, triggerLeaseExpired)
				}
				continue
			}

			// The GET itself failed. This is the ONLY branch reachable when
			// the broken thing is Redis, so it must not ask Redis whether the
			// lease survived. Left alone it used to log and continue, and the
			// supplier stayed live in the manager -- signing -- while the key
			// quietly expired and a peer took it. The local clock settles it:
			// once no renewal has succeeded for longer than the lease could
			// have lived, it is not ours regardless of what Redis would say.
			c.logger.Warn().
				Err(err).
				Str("supplier", supplier).
				Msg("failed to get claim owner")
			if c.renewStalledFor(supplier, now) >= c.config.ClaimTTL {
				c.releaseLost(c.ctx, supplier, triggerRenewStalled)
			}
			continue
		}

		if owner != c.instanceID {
			// A peer holds the key. There is nothing to recover, so unlike the
			// expiry branch this one reports the loss immediately.
			c.logger.Warn().
				Str("supplier", supplier).
				Str("owner", owner).
				Str("expected", c.instanceID).
				Msg("claim stolen by another instance")
			c.releaseLost(c.ctx, supplier, triggerLeaseStolen)
			continue
		}

		if c.releasedSinceSnapshot(supplier) {
			// Released after the snapshot, and its drain holds the key on a
			// budget of its own: renewing it to ClaimTTL would stretch the lease
			// of a supplier nothing renews any more past that budget.
			continue
		}

		// Renew the lease - check BOTH error AND result
		// Expire returns (bool, error) - bool is false if key doesn't exist
		renewed, err := c.redisClient.Expire(c.ctx, claimKey, c.config.ClaimTTL).Result()
		switch {
		case err != nil:
			c.logger.Warn().Err(err).Str("supplier", supplier).Msg("failed to renew claim")
			if c.renewStalledFor(supplier, now) >= c.config.ClaimTTL {
				c.releaseLost(c.ctx, supplier, triggerRenewStalled)
			}
		case !renewed:
			// The key died between GET and EXPIRE. Same shape as the expiry
			// branch: recover first, and only report the loss if we cannot.
			if c.releasedSinceSnapshot(supplier) {
				continue // released by this instance between the GET and the EXPIRE
			}
			c.logger.Warn().
				Str("supplier", supplier).
				Str("claim_key", claimKey).
				Msg("claim key disappeared during renewal (race condition)")
			if !c.TryClaim(c.ctx, supplier) {
				c.releaseLost(c.ctx, supplier, triggerLeaseExpired)
			}
		default:
			c.markRenewed(supplier, now)
		}
	}
}

// releasedSinceSnapshot reports whether this instance gave the supplier up after
// renewAllClaims took its snapshot of the claimed map: Release (rebalance, key
// removal) runs on another goroutine, and its drain deletes the key on purpose.
// A missing key is then not a lease to take back -- re-taking it undoes the release, and
// the next rebalance releases it again. A lease that really expired (a slow
// Redis) is still in the map and is recovered.
//
// The map alone decides, because Release takes the supplier out of it before
// the key goes. The release cooldown must NOT be consulted: nothing clears it
// when claimMore legitimately takes the supplier back, so a lease that then
// really expired inside the cooldown would be neither recovered nor reported --
// the supplier left signing without a lease until the cooldown ran out.
func (c *SupplierClaimer) releasedSinceSnapshot(supplier string) bool {
	return !c.IsClaimed(supplier)
}

// markRenewed records a CONFIRMED renewal.
func (c *SupplierClaimer) markRenewed(supplier string, now time.Time) {
	c.lastRenewedAtMu.Lock()
	c.lastRenewedAt[supplier] = now
	c.lastRenewedAtMu.Unlock()
}

// renewStalledFor reports how long it has been since this instance last
// confirmed it still owns supplier's lease. A supplier with no record yet is
// treated as renewed now, so a single failure never reports a loss -- only a
// stall that outlives the lease does.
func (c *SupplierClaimer) renewStalledFor(supplier string, now time.Time) time.Duration {
	c.lastRenewedAtMu.Lock()
	defer c.lastRenewedAtMu.Unlock()
	last, ok := c.lastRenewedAt[supplier]
	if !ok {
		c.lastRenewedAt[supplier] = now
		return 0
	}
	return now.Sub(last)
}

// rebalanceLoop periodically checks and rebalances supplier distribution.
func (c *SupplierClaimer) rebalanceLoop() {
	defer c.wg.Done()

	// Half a renewal period out of step with the renewal loop, which starts
	// with this one: with the default rebalance interval a multiple of the
	// renewal rate, every rebalance tick would otherwise land on a renewal
	// pass over the same claims.
	select {
	case <-c.ctx.Done():
		return
	case <-time.After(c.config.RenewRate / 2):
	}

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

	releasedThisTick := false
	if currentCount > fairShare {
		// Release excess suppliers
		excess := currentCount - fairShare
		releasedThisTick = c.releaseExcess(excess) > 0
	} else if currentCount < fairShare {
		// Try to claim more suppliers
		needed := fairShare - currentCount
		c.claimMore(needed)
	}

	// Check for orphaned suppliers that no miner instance has claimed —
	// keys that expired without being renewed (crashed miner). Skip this
	// step if we just released suppliers in this tick: those keys are
	// freshly empty BY DESIGN so peer miners can claim them, and
	// claimOrphaned would otherwise race the peers and re-grab everything
	// we just gave up, defeating the rebalance. A genuinely orphaned key
	// (from a dead miner) will still be caught on the next tick.
	if !releasedThisTick {
		c.claimOrphaned()
	}
}

// releaseExcess releases excess suppliers to allow other miners to claim them.
// Suppliers are released newest-first (most recently claimed = least established)
// to minimize session handoff churn during rebalancing. Returns the count of
// suppliers actually released so the caller can suppress same-tick reclaims.
func (c *SupplierClaimer) releaseExcess(count int) int {
	type claimEntry struct {
		supplier  string
		claimedAt time.Time
	}

	c.claimedMu.RLock()
	entries := make([]claimEntry, 0, len(c.claimed))
	for supplier, claimedAt := range c.claimed {
		entries = append(entries, claimEntry{supplier: supplier, claimedAt: claimedAt})
	}
	c.claimedMu.RUnlock()

	// Sort newest-first (most recently claimed = least established)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].claimedAt.After(entries[j].claimedAt)
	})

	released := 0
	for _, entry := range entries {
		if released >= count {
			break
		}

		if err := c.Release(c.ctx, entry.supplier, triggerRebalanceRelease); err != nil {
			c.logger.Warn().Err(err).Str("supplier", entry.supplier).Msg("failed to release excess supplier")
			continue
		}

		released++
		c.logger.Info().
			Str("supplier", entry.supplier).
			Int("released", released).
			Int("target", count).
			Time("claimed_at", entry.claimedAt).
			Msg("released supplier for rebalancing (newest-first)")
	}
	return released
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

		if c.IsClaimed(supplier) || c.isDraining(supplier) {
			continue // Already claimed by us, or still being handed over
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

// claimOrphaned scans all configured suppliers and claims any that have no active
// claim key in Redis. This catches suppliers that fell through the cracks — e.g.,
// when both miners are at fair share but one supplier's claim expired, neither
// miner's rebalance logic would detect it since both check only their own count.
func (c *SupplierClaimer) claimOrphaned() {
	c.allSuppliersMu.RLock()
	suppliers := make([]string, len(c.allSuppliers))
	copy(suppliers, c.allSuppliers)
	c.allSuppliersMu.RUnlock()

	if len(suppliers) == 0 {
		return
	}

	orphaned := 0
	claimed := 0
	skippedCooldown := 0
	for _, supplier := range suppliers {
		if c.IsClaimed(supplier) || c.isDraining(supplier) {
			continue
		}

		// Don't reclaim a supplier we just released — the peer miner
		// the release was meant for needs time to observe the empty
		// claim key. A genuine orphan from a crashed miner will fall
		// outside this window and be picked up normally.
		if c.inRecentReleaseCooldown(supplier) {
			skippedCooldown++
			continue
		}

		claimKey := c.redisClient.KB().MinerClaimKey(supplier)
		exists, err := c.redisClient.Exists(c.ctx, claimKey).Result()
		if err != nil {
			c.logger.Warn().Err(err).Str("supplier", supplier).
				Msg("failed to check claim key for orphan detection")
			continue
		}

		if exists == 0 {
			orphaned++
			c.logger.Warn().Str("supplier", supplier).
				Msg("detected orphaned supplier (no claim key), attempting to claim")
			if c.TryClaim(c.ctx, supplier) {
				claimed++
			}
		}
	}

	if orphaned > 0 {
		c.logger.Info().
			Int("orphaned_detected", orphaned).
			Int("orphaned_claimed", claimed).
			Int("total_suppliers", len(suppliers)).
			Msg("orphaned supplier scan complete")
	}
}

// UpdateSuppliers updates the list of configured suppliers.
// Called when KeyManager detects a config change.
//
// Issue #7 follow-up: previously this spawned `go c.rebalance()` on every
// call. Reconcile fires UpdateSuppliers every ~13s and rebalance itself can
// block on a slow supplier teardown — under that combination each tick
// stacks a new rebalance goroutine, all racing on the same suppliers and
// fanning out concurrent Release calls that deadlock on the supplier
// manager's coordination. The periodic rebalanceLoop ticker already covers
// fair-share convergence; UpdateSuppliers just records the new list.
func (c *SupplierClaimer) UpdateSuppliers(suppliers []string) {
	c.allSuppliersMu.Lock()
	c.allSuppliers = suppliers
	c.allSuppliersMu.Unlock()

	c.logger.Info().
		Int("suppliers", len(suppliers)).
		Msg("updated supplier list")
}
