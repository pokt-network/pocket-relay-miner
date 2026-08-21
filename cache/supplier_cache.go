package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	// SupplierStatusActive indicates the supplier is active and accepting relays.
	SupplierStatusActive = "active"

	// SupplierStatusUnstaking indicates the supplier is unstaking.
	// Check UnstakeSessionEndHeight to determine if still in grace period.
	SupplierStatusUnstaking = "unstaking"

	// SupplierStatusNotStaked indicates the address is in keys but NOT staked on-chain.
	// This allows operators to see which keys are not yet staked as suppliers.
	SupplierStatusNotStaked = "not_staked"

	// Cache type for pub/sub and metrics
	supplierCacheType = "supplier"

	// Lock key prefix for distributed locking during L3 query
	// supplierLockPrefix = "ha:cache:lock:supplier:"

	// defaultSupplierCacheTTL is the fallback used when SupplierCacheConfig.TTL
	// is unset and SupplierCacheTTLFromParams could not compute one (chain
	// unreachable at startup). Same order of magnitude as proof_params.go's
	// own "10-minute safety net" convention, not a new arbitrary number.
	defaultSupplierCacheTTL = 10 * time.Minute

	// supplierCacheTTLMultiplier mirrors shared_params_singleton.go's
	// sessionTTLMultiplier: margin beyond the protocol deadline the TTL is
	// meant to outlive, so a missed reconcile tick does not race the expiry.
	supplierCacheTTLMultiplier = 2
)

// SupplierCacheTTLFromParams computes the Redis TTL for a SupplierCache entry
// from the chain's own session length, not an arbitrary constant.
//
// The miner rewrites an active supplier's entry every reconcile pass, so this
// TTL only matters once nothing writes it anymore — the decommissioned-
// supplier case (HIGH-1, review 2026-08-20:
// scripts/localonly/REVIEW-2026-08-20-r1-stream-lifecycle.md). Before this,
// SetSupplierState wrote with no TTL at all, so a supplier whose signing key
// left the miner's keyring mid-teardown froze at its last-written state —
// often still "staked, serving" — forever, with no cache layer left to
// notice.
//
// A supplier legitimately stays IsActive() while Status == unstaking: poktroll
// keeps it serving until the current session ends. Verified against
// x/supplier/keeper/msg_server_unstake_supplier.go (poktroll@v0.1.35):
// Supplier.UnstakeSessionEndHeight is set in the SAME block as
// MsgUnstakeSupplier (so IsUnbonding() is true immediately), but
// ServiceConfigHistory[].DeactivationHeight is deferred to the NEXT session
// boundary only — "the supplier MUST continue to provide service until the
// end of the current session". So the entry must outlive one session, not
// one full unbonding period.
//
// SupplierUnbondingPeriodSessions is the WRONG axis for this, and an earlier
// version of this function used it: that param answers "how long until the
// stake unlocks" (an economic-security window), not "how long until services
// deactivate" (always <= 1 session, regardless of the unbonding period's
// length). Verified live against mainnet (sauron-api.infra.pocket.network,
// x/shared/params, 2026-08-20): num_blocks_per_session=20 but
// supplier_unbonding_period_sessions=1429 — using the unbonding axis would
// have produced a ~40-day TTL, keeping a decommissioned supplier servable and
// its orphaned stream invisible for over a month. num_blocks_per_session is
// the parameter that actually bounds when services deactivate.
//
// The 2x multiplier (Jorge, 2026-08-20) is margin, not a second deadline: one
// session for the protocol's own boundary, one more so a delayed reconcile
// pass does not race the expiry — comfortably above the reconcile interval
// (DefaultSupplierReconcileInterval = 60s; see the margin check in
// TestSupplierCacheTTLFromParams). An operator who removes the signing key
// before that window closes loses their own rewards — this repo does not try
// to protect against that (there is no recovering a signature we no longer
// hold the key for); the TTL only bounds how long the resulting stale entry
// can mislead the relayer into serving it, instead of freezing that lie
// forever.
func SupplierCacheTTLFromParams(params *sharedtypes.Params, blockTimeSeconds int64) time.Duration {
	if params == nil || blockTimeSeconds <= 0 {
		return defaultSupplierCacheTTL
	}

	blocksPerSession := params.GetNumBlocksPerSession()
	if blocksPerSession == 0 {
		return defaultSupplierCacheTTL
	}

	ttlSeconds := int64(supplierCacheTTLMultiplier*blocksPerSession) * blockTimeSeconds
	return time.Duration(ttlSeconds) * time.Second
}

// supplierCacheL1TTL bounds how long an L1 (in-process) supplier entry — its
// stake status and service list — is served before it is treated as a miss and
// re-read from L2. Supplier stake/services are latest-semantics: they change
// on-chain (stake, unstake, service add/remove) and the miner rewrites L2.
// Without a TTL the L1 xsync map freezes the first value for the process
// lifetime; it is only ever cleared by pub/sub invalidation, which the miner
// fires on Set/Delete but which a relayer can miss (restart, dropped event),
// stranding a stale stake/services view forever. Latest-semantics caches use a
// 60s floor (mirrors application/service). var (not const) so tests can shrink
// it without sleeping.
var supplierCacheL1TTL = 60 * time.Second

// supplierCacheL1Entry is an L1-cached supplier state plus the time it was
// fetched, so the entry can be aged out by supplierCacheL1TTL.
type supplierCacheL1Entry struct {
	supplier *SupplierState
	cachedAt time.Time
}

// SupplierState represents the cached state of a supplier.
// This is stored in Redis and shared between miner (writer) and relayer (reader).
type SupplierState struct {
	// Status is the supplier's current status: "active", "unstaking", or "not_staked"
	Status string `json:"status"`

	// Staked indicates if this address is staked as a supplier on-chain.
	// false means the address is in the keys file but NOT staked on-chain.
	// This allows the miner to track all configured keys and report staking status.
	Staked bool `json:"staked"`

	// Services is the list of service IDs this supplier is staked for.
	// Empty if not staked (Staked=false).
	Services []string `json:"services"`

	// StakedEndpoints is the per-transport view of what the supplier declared
	// on-chain: one entry per (service, rpc_type) endpoint. Services above is the
	// service-level projection of this; both are kept so an old relayer that does
	// not know this field still gets Services.
	//
	// MIXED-FLEET BACK-COMPAT: an old miner does not publish this field, so a new
	// relayer reading old state sees it empty. Consumers MUST treat empty as
	// "unknown, do not act" (fail-open) — see TransportDeclared — never as "no
	// endpoints declared". Adding a field is safe: old relayers ignore it.
	StakedEndpoints []StakedEndpoint `json:"staked_endpoints,omitempty"`

	// UnstakeSessionEndHeight is the session end height when unstaking takes effect.
	// 0 means the supplier is active (not unstaking).
	// >0 means the supplier is unstaking and will be fully unstaked at this session end height.
	//
	// It is INFORMATIONAL here and must not be turned into a serve gate. The grace
	// period it describes is already enforced, one level down and more precisely, by
	// Services: MsgUnstakeSupplier sets DeactivationHeight = next session start on
	// every one of the supplier's service configs
	// (x/supplier/keeper/msg_server_unstake_supplier.go:92-97), and Services is
	// derived from GetActiveServiceConfigs at the observed height, so it empties at
	// that boundary on its own. A second height comparison here would be redundant
	// at best and would disagree with the per-service schedule at worst -- services
	// can be deactivated individually, without any unstake.
	UnstakeSessionEndHeight uint64 `json:"unstake_session_end_height"`

	// OperatorAddress is the supplier's operator address.
	OperatorAddress string `json:"operator_address"`

	// OwnerAddress is the supplier's owner address.
	OwnerAddress string `json:"owner_address"`

	// LastUpdated is the Unix timestamp when this state was last updated.
	LastUpdated int64 `json:"last_updated"`

	// LastRefreshSessionStart is the session start height when staking status was last checked.
	// Staking status is only refreshed at session boundaries to avoid unnecessary queries.
	LastRefreshSessionStart int64 `json:"last_refresh_session_start,omitempty"`

	// UpdatedBy identifies which miner instance updated this state (for debugging).
	UpdatedBy string `json:"updated_by,omitempty"`
}

// StakedEndpoint is one (service, transport) endpoint a supplier declared
// on-chain. RpcType holds the canonical relayer backend-type string
// (jsonrpc|rest|websocket|grpc|cometbft), i.e. the same key space as the
// service config's backend keys, so the relayer can compare directly.
type StakedEndpoint struct {
	ServiceID string `json:"service_id"`
	RpcType   string `json:"rpc_type"`
}

// TransportDeclared reports whether the supplier declared an on-chain endpoint
// for (serviceID, backendType).
//
// FAIL-OPEN: when StakedEndpoints is empty the transport view is unknown (old
// miner, or not yet published) — this returns true so callers never act on
// missing data. It answers "false" only when StakedEndpoints is populated AND
// the pair is genuinely absent.
func (s *SupplierState) TransportDeclared(serviceID, backendType string) bool {
	if len(s.StakedEndpoints) == 0 {
		return true
	}
	for _, e := range s.StakedEndpoints {
		if e.ServiceID == serviceID && e.RpcType == backendType {
			return true
		}
	}
	return false
}

// IsContaminated returns true when the cached state looks like a failed-write
// artifact: staked+active with an empty services list. This anti-pattern is
// produced by a separate bug in the write path (miner/supplier_manager.go).
// Legitimate states that look similar are NOT contaminated:
//   - Unstaked entries have Staked=false.
//   - Draining entries have Status=unstaking and preserve their services.
//
// Only the exact tuple (Staked && Status==active && len(Services)==0) is
// treated as contamination.
func (s *SupplierState) IsContaminated() bool {
	return s.Staked && s.Status == SupplierStatusActive && len(s.Services) == 0
}

// IsActive returns true if the supplier is active and should accept relays.
//
// An unstaking supplier (Status == SupplierStatusUnstaking) is still considered
// active: poktroll deactivates services at the next session-start boundary, not
// immediately. The Services list is the precise gate — it empties at the
// deactivation boundary (x/shared schedules it via activation_height /
// deactivation_height on each ServiceConfig). proxy.go checks
// len(Services)==0 and IsActiveForService, so the per-service timing is
// enforced there. Only SupplierStatusNotStaked — meaning the address is not
// staked on-chain at all — warrants rejecting relays here.
func (s *SupplierState) IsActive() bool {
	return s.Staked && s.Status != SupplierStatusNotStaked
}

// IsActiveForService returns true if the supplier is active for the given service.
func (s *SupplierState) IsActiveForService(serviceID string) bool {
	if !s.IsActive() {
		return false
	}
	for _, svc := range s.Services {
		if svc == serviceID {
			return true
		}
	}
	return false
}

// SupplierCache provides read/write access to the shared supplier state cache.
//
// Cache levels:
// - L1: In-memory cache using xsync.MapOf for lock-free concurrent access
// - L2: Redis cache with JSON marshaling
//
// The cache subscribes to pub/sub invalidation events to stay synchronized
// across all instances.
type SupplierCache struct {
	logger   logging.Logger
	redis    *redisutil.Client
	failOpen bool

	// ttl is nanoseconds (time.Duration), atomic so SetTTL can update it after
	// construction without a lock on the hot SetSupplierState write path. A
	// caller that cannot afford to block startup on a chain query (see
	// SetTTL's own doc comment) constructs with a safe default and upgrades
	// it once the real value is available.
	ttl atomic.Int64

	// L1: In-memory cache (xsync for lock-free performance), TTL-bounded by
	// supplierCacheL1TTL so an on-chain stake/services change is not frozen forever.
	localCache *xsync.Map[string, supplierCacheL1Entry]

	// Pub/sub
	pubsub *redis.PubSub

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// SupplierCacheConfig contains configuration for SupplierCache.
type SupplierCacheConfig struct {
	// FailOpen determines behavior when Redis is unavailable.
	// If true, treat supplier as active when cache unavailable (safer for traffic).
	// If false, treat supplier as inactive when cache unavailable (safer for validation).
	FailOpen bool

	// TTL bounds how long a written entry survives in Redis without being
	// refreshed. See SupplierCacheTTLFromParams for how to derive it from
	// live chain params. <= 0 falls back to defaultSupplierCacheTTL — never
	// to no TTL at all; see SetSupplierState.
	TTL time.Duration
}

// NewSupplierCache creates a new SupplierCache.
//
// The cache must be started with Start() before use and should be closed
// with Close() when no longer needed.
func NewSupplierCache(
	logger logging.Logger,
	redisClient *redisutil.Client,
	config SupplierCacheConfig,
) *SupplierCache {
	ttl := config.TTL
	if ttl <= 0 {
		ttl = defaultSupplierCacheTTL
	}

	c := &SupplierCache{
		logger:     logging.ForComponent(logger, logging.ComponentQuerySupplier),
		redis:      redisClient,
		failOpen:   config.FailOpen,
		localCache: xsync.NewMap[string, supplierCacheL1Entry](),
	}
	c.ttl.Store(int64(ttl))
	return c
}

// SetTTL updates the TTL applied to every future SetSupplierState write.
// Safe to call concurrently with writes (atomic) and at any time after
// construction — in particular, from a caller that could not afford to
// block on a chain query at construction time and constructed with the
// safe default, then computed the real value asynchronously (see
// miner/supplier_worker.go's use of this alongside its shared-params
// advisory, both dispatched off the master pool so an unreachable node
// does not lengthen startup).
//
// Does NOT retroactively touch entries already written with the old TTL;
// only the transient mismatch on entries written in that window, at most
// one reconcile cycle's worth, since the next write refreshes them anyway.
func (c *SupplierCache) SetTTL(ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultSupplierCacheTTL
	}
	c.ttl.Store(int64(ttl))
}

// supplierKey returns the Redis key for a supplier's state.
func (c *SupplierCache) supplierKey(operatorAddress string) string {
	return c.redis.KB().SupplierStateKey(operatorAddress)
}

// GetSupplierState retrieves a supplier's state from the cache using L1 → L2 fallback.
// Returns nil if the supplier is not in the cache.
// If Redis is unavailable and FailOpen is true, returns a synthetic "active" state.
func (c *SupplierCache) GetSupplierState(ctx context.Context, operatorAddress string) (*SupplierState, error) {
	start := time.Now()

	// L1: Check local cache (xsync). Only a fresh entry (within
	// supplierCacheL1TTL) is a hit; a stale one falls through to L2 so an
	// on-chain stake/services change propagates instead of being frozen.
	if entry, ok := c.localCache.Load(operatorAddress); ok && time.Since(entry.cachedAt) < supplierCacheL1TTL {
		cached := entry.supplier

		// Defensive guard: a pre-fix binary (or a warmup before this guard
		// existed) may have populated L1 with a contaminated entry. Evict
		// it and treat as a miss so the caller re-hydrates via L3.
		if cached != nil && cached.IsContaminated() {
			supplierContaminated.WithLabelValues("l1_read").Inc()
			c.localCache.Delete(operatorAddress)
			c.logger.Warn().
				Str(logging.FieldSupplierOperator, operatorAddress).
				Msg("supplier cache entry contaminated (staked+active but services empty); ignoring and reporting as cache miss")
			cacheMisses.WithLabelValues(supplierCacheType, CacheLevelL1).Inc()
			cacheGetLatency.WithLabelValues(supplierCacheType, CacheLevelL1).Observe(time.Since(start).Seconds())
			return nil, nil
		}

		cacheHits.WithLabelValues(supplierCacheType, CacheLevelL1).Inc()
		cacheGetLatency.WithLabelValues(supplierCacheType, CacheLevelL1).Observe(time.Since(start).Seconds())
		c.logger.Debug().
			Str(logging.FieldSupplierOperator, operatorAddress).
			Msg("supplier cache hit (L1)")
		return cached, nil
	}

	// L2: Check Redis cache
	key := c.supplierKey(operatorAddress)
	data, err := c.redis.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			// Supplier not in cache
			cacheMisses.WithLabelValues(supplierCacheType, "l2_not_found").Inc()
			cacheGetLatency.WithLabelValues(supplierCacheType, "l2_not_found").Observe(time.Since(start).Seconds())
			return nil, nil
		}

		// Redis error — per-relay under an outage; the cache error rate is
		// in the cache metrics and the outage state in the reconnect logs.
		c.logger.Debug().
			Err(err).
			Str(logging.FieldSupplierOperator, operatorAddress).
			Bool("fail_open", c.failOpen).
			Msg("failed to get supplier state from cache")

		if c.failOpen {
			// Return synthetic active state to avoid blocking traffic
			c.logger.Debug().
				Str(logging.FieldSupplierOperator, operatorAddress).
				Msg("fail-open: treating supplier as active due to cache error")
			cacheGetLatency.WithLabelValues(supplierCacheType, "l2_error").Observe(time.Since(start).Seconds())
			return &SupplierState{
				Status:          SupplierStatusActive,
				OperatorAddress: operatorAddress,
			}, nil
		}

		cacheGetLatency.WithLabelValues(supplierCacheType, "l2_error").Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("failed to get supplier state: %w", err)
	}

	var state SupplierState
	if err := json.Unmarshal(data, &state); err != nil {
		cacheGetLatency.WithLabelValues(supplierCacheType, "l2_error").Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("failed to unmarshal supplier state: %w", err)
	}

	// Defensive guard: reject contaminated entries (staked+active with empty
	// services) produced by a separate write-path bug. Treat as a cache miss
	// so the miner re-hydrates a clean entry from L3 on the next refresh,
	// and do NOT populate L1 (avoid re-infecting followers).
	if state.IsContaminated() {
		supplierContaminated.WithLabelValues("l2_read").Inc()
		c.logger.Warn().
			Str(logging.FieldSupplierOperator, operatorAddress).
			Msg("supplier cache entry contaminated (staked+active but services empty); ignoring and reporting as cache miss")
		cacheMisses.WithLabelValues(supplierCacheType, CacheLevelL2).Inc()
		cacheGetLatency.WithLabelValues(supplierCacheType, CacheLevelL2).Observe(time.Since(start).Seconds())
		return nil, nil
	}

	// Store in L1 for next time
	c.localCache.Store(operatorAddress, supplierCacheL1Entry{supplier: &state, cachedAt: time.Now()})
	cacheHits.WithLabelValues(supplierCacheType, CacheLevelL2).Inc()
	cacheGetLatency.WithLabelValues(supplierCacheType, CacheLevelL2).Observe(time.Since(start).Seconds())

	c.logger.Debug().
		Str(logging.FieldSupplierOperator, operatorAddress).
		Msg("supplier cache hit (L2) → stored in L1")

	return &state, nil
}

// SetSupplierState stores a supplier's state in both L1 and L2 caches.
// The miner typically calls this to update supplier state.
// Publishes invalidation event so other instances refresh their L1 cache.
func (c *SupplierCache) SetSupplierState(ctx context.Context, state *SupplierState) error {
	if state.OperatorAddress == "" {
		return fmt.Errorf("operator_address is required")
	}

	// Update timestamp
	state.LastUpdated = time.Now().Unix()

	// L1: Store in local cache
	c.localCache.Store(state.OperatorAddress, supplierCacheL1Entry{supplier: state, cachedAt: time.Now()})

	// L2: Store in Redis
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal supplier state: %w", err)
	}

	key := c.supplierKey(state.OperatorAddress)

	// Bounded TTL (see SupplierCacheTTLFromParams), refreshed on every write.
	// An actively-tracked supplier is rewritten every reconcile pass, long
	// before the TTL matters; it only fires once nothing writes this entry
	// anymore, which is exactly the decommissioned-supplier case this TTL
	// exists to bound (HIGH-1, review 2026-08-20). A TTL=0 write here is
	// what let that entry freeze "still active" forever.
	if err := c.redis.Set(ctx, key, data, time.Duration(c.ttl.Load())).Err(); err != nil {
		return fmt.Errorf("failed to set supplier state: %w", err)
	}

	// Publish invalidation event so other instances clear their L1 cache
	// and reload fresh data from L2 on next access.
	// This fixes the bug where relayers show "supplier not staked for service"
	// after services are updated but L1 cache still has stale data.
	payload := fmt.Sprintf(`{"operator_address": "%s"}`, state.OperatorAddress)
	if err := PublishInvalidation(ctx, c.redis, c.logger, supplierCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSupplierOperator, state.OperatorAddress).
			Msg("failed to publish supplier cache invalidation (other instances may have stale data)")
	}

	c.logger.Debug().
		Str(logging.FieldSupplierOperator, state.OperatorAddress).
		Str("status", state.Status).
		Uint64("unstake_session_end_height", state.UnstakeSessionEndHeight).
		Msg("updated supplier state in cache")

	return nil
}

// DeleteSupplierState removes a supplier's state from both L1 and L2 caches.
// This should be called when a supplier is fully unstaked.
//
// UNUSED as of 2026-08-20 (review, "agujero 6" / LOW #4): removeSupplier no
// longer calls this (see its own comment on why an immediate delete was
// wrong — HIGH-1). SetSupplierState's TTL (SupplierCacheTTLFromParams) is
// now the ONLY thing that ever clears this entry, and it is passive: it
// bounds the blast radius but does not act the moment we KNOW a supplier is
// gone. The still-open agujero-6 teardown design likely wants both — this
// method for the confirmed-terminal moment, the TTL as the backstop for
// crash/partition, where nothing ever reaches that moment. Kept unused
// rather than deleted (against the repo's own "delete unused declarations"
// rule) until that design lands and either calls it or removes it for real.
func (c *SupplierCache) DeleteSupplierState(ctx context.Context, operatorAddress string) error {
	// Remove from L1 (local cache)
	c.localCache.Delete(operatorAddress)

	// Remove from L2 (Redis)
	key := c.supplierKey(operatorAddress)
	if err := c.redis.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete supplier state: %w", err)
	}

	// Publish invalidation event to other instances
	payload := fmt.Sprintf(`{"operator_address": "%s"}`, operatorAddress)
	if err := PublishInvalidation(ctx, c.redis, c.logger, supplierCacheType, payload); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSupplierOperator, operatorAddress).
			Msg("failed to publish invalidation event")
	}

	cacheInvalidations.WithLabelValues(supplierCacheType, SourceManual).Inc()

	c.logger.Info().
		Str(logging.FieldSupplierOperator, operatorAddress).
		Msg("deleted supplier state from cache")

	return nil
}

// IsSupplierActiveForService checks if a supplier is active for a service.
// This is a convenience method that combines GetSupplierState and IsActiveForService.
// Returns (true, nil) if supplier is active for the service.
// Returns (false, nil) if supplier is not active or not in cache.
// Returns (false, error) if there was a cache error and FailOpen is false.
func (c *SupplierCache) IsSupplierActiveForService(
	ctx context.Context,
	operatorAddress string,
	serviceID string,
) (bool, error) {
	state, err := c.GetSupplierState(ctx, operatorAddress)
	if err != nil {
		return false, err
	}

	if state == nil {
		// Supplier not in cache
		if c.failOpen {
			c.logger.Debug().
				Str("operator_address", operatorAddress).
				Str("service_id", serviceID).
				Msg("fail-open: supplier not in cache, treating as active")
			return true, nil
		}
		return false, nil
	}

	return state.IsActiveForService(serviceID), nil
}

// GetAllSupplierStates returns all supplier states from the cache.
// This is useful for debugging and monitoring.
func (c *SupplierCache) GetAllSupplierStates(ctx context.Context) (map[string]*SupplierState, error) {
	pattern := c.redis.KB().SupplierStatePattern()
	keys, err := c.redis.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list supplier keys: %w", err)
	}

	states := make(map[string]*SupplierState)
	for _, key := range keys {
		data, err := c.redis.Get(ctx, key).Bytes()
		if err != nil {
			if err == redis.Nil {
				continue
			}
			return nil, fmt.Errorf("failed to get supplier state for key %s: %w", key, err)
		}

		var state SupplierState
		if err := json.Unmarshal(data, &state); err != nil {
			c.logger.Warn().
				Err(err).
				Str("key", key).
				Msg("failed to unmarshal supplier state, skipping")
			continue
		}

		states[state.OperatorAddress] = &state
	}

	return states, nil
}

// Start initializes the cache and subscribes to pub/sub invalidation events.
func (c *SupplierCache) Start(ctx context.Context) error {
	c.ctx, c.cancelFn = context.WithCancel(ctx)

	// Subscribe to invalidation events
	if err := SubscribeToInvalidations(
		c.ctx,
		c.redis,
		c.logger,
		supplierCacheType,
		c.handleInvalidation,
	); err != nil {
		return fmt.Errorf("failed to subscribe to invalidations: %w", err)
	}

	c.logger.Info().Msg("supplier cache started")

	return nil
}

// Close gracefully shuts down the cache.
func (c *SupplierCache) Close() error {
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.wg.Wait()

	if c.pubsub != nil {
		_ = c.pubsub.Close()
	}

	c.logger.Info().Msg("supplier cache stopped")

	return nil
}

// WarmupFromRedis populates L1 cache from Redis on startup.
// If knownSupplierAddresses is nil or empty, automatically discovers all suppliers in Redis.
func (c *SupplierCache) WarmupFromRedis(ctx context.Context, knownSupplierAddresses []string) error {
	// If no addresses provided, discover all suppliers in Redis
	if len(knownSupplierAddresses) == 0 {
		c.logger.Info().Msg("discovering all suppliers in Redis for warmup")
		pattern := c.redis.KB().SupplierStatePattern()
		keys, err := c.redis.Keys(ctx, pattern).Result()
		if err != nil {
			return fmt.Errorf("failed to discover suppliers: %w", err)
		}

		// Extract operator addresses from keys (ha:supplier:{operator} -> {operator})
		for _, key := range keys {
			// Remove prefix to get operator address
			addr := strings.TrimPrefix(key, c.redis.KB().SupplierKeyPrefix()+":")
			if addr != key { // Ensure prefix was found and removed
				knownSupplierAddresses = append(knownSupplierAddresses, addr)
			}
		}
		c.logger.Info().Int("discovered_suppliers", len(knownSupplierAddresses)).Msg("discovered suppliers in Redis")
	} else {
		c.logger.Info().Int("count", len(knownSupplierAddresses)).Msg("warming up supplier cache from Redis")
	}

	if len(knownSupplierAddresses) == 0 {
		c.logger.Info().Msg("no suppliers found in Redis, skipping warmup")
		return nil
	}

	var wg sync.WaitGroup
	for _, operatorAddr := range knownSupplierAddresses {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()

			// Load from Redis (L2) into local cache (L1)
			key := c.supplierKey(addr)
			data, err := c.redis.Get(ctx, key).Bytes()
			if err != nil {
				// Key doesn't exist in Redis, skip
				return
			}

			var state SupplierState
			if err := json.Unmarshal(data, &state); err != nil {
				c.logger.Warn().
					Err(err).
					Str(logging.FieldSupplierOperator, addr).
					Msg("failed to unmarshal supplier state during warmup")
				return
			}

			// Defensive guard: skip contaminated entries during warmup so a
			// fleet restart doesn't re-infect L1 from stale L2 data.
			if state.IsContaminated() {
				supplierContaminated.WithLabelValues("warmup_skip").Inc()
				c.logger.Debug().
					Str(logging.FieldSupplierOperator, addr).
					Msg("skipping contaminated entry during warmup")
				return
			}

			c.localCache.Store(addr, supplierCacheL1Entry{supplier: &state, cachedAt: time.Now()})
			c.logger.Debug().
				Str(logging.FieldSupplierOperator, addr).
				Int("services", len(state.Services)).
				Msg("loaded supplier into cache during warmup")
		}(operatorAddr)
	}

	wg.Wait()
	c.logger.Info().Int("loaded_count", len(knownSupplierAddresses)).Msg("supplier cache warmup complete")

	return nil
}

// handleInvalidation handles cache invalidation events from pub/sub.
func (c *SupplierCache) handleInvalidation(ctx context.Context, payload string) error {
	c.logger.Debug().
		Str("payload", payload).
		Msg("received supplier invalidation event")

	// Empty-object payload means invalidate ALL entries (bulk clear-all).
	// Must be checked before unmarshal: "{}" parses successfully into the
	// event struct, so a check inside the error branch is unreachable.
	if payload == "{}" {
		c.localCache.Clear()
		cacheInvalidations.WithLabelValues(supplierCacheType, SourcePubSub).Inc()
		return nil
	}

	// Parse payload to get operator_address
	var event struct {
		OperatorAddress string `json:"operator_address"`
	}

	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		c.logger.Warn().
			Err(err).
			Str("payload", payload).
			Msg("failed to parse invalidation event")
		return err
	}

	// Remove from L1 (local cache)
	if event.OperatorAddress != "" {
		c.localCache.Delete(event.OperatorAddress)
	}

	cacheInvalidations.WithLabelValues(supplierCacheType, SourcePubSub).Inc()

	return nil
}
