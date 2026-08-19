package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/client"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

var _ SessionCache = (*RedisSessionCache)(nil)

// sessionCacheL1TTL bounds how long an L1 (in-process) session entry is served
// before it is treated as a miss and re-read from L2/L3. A session is
// height-keyed and immutable once observed, so this is a safety floor — not a
// correctness lever like the latest-semantics caches — but the L1 sync.Map
// otherwise freezes the first value for the process lifetime (it is never
// cleared by any invalidation path). Without a floor a process-lifetime entry
// can mask an L2/L3 correction (e.g. a non-deterministic session re-fetch). The
// floor is long (30m) because the key already pins the height/session. var (not
// const) so tests can shrink it without sleeping.
var sessionCacheL1TTL = 30 * time.Minute

// sessionCacheL1Entry is an L1-cached session plus the height it was keyed at (for
// window-bounded eviction) and the time it was fetched (for the TTL floor).
type sessionCacheL1Entry struct {
	session  *sessiontypes.Session
	height   int64
	cachedAt time.Time
}

// sessionCacheL1KeepHeights bounds the L1 height window. The sync.Map otherwise
// grows one entry per distinct session served for the process lifetime; storing
// prunes entries whose height is more than this many blocks below the newest.
const sessionCacheL1KeepHeights = 600

// storeSession caches a session in L1 and prunes entries far below the newest
// height, keeping the sync.Map bounded.
func (c *RedisSessionCache) storeSession(key string, height int64, session *sessiontypes.Session) {
	c.sessionCache.Store(key, sessionCacheL1Entry{session: session, height: height, cachedAt: time.Now()})

	cutoff := height - sessionCacheL1KeepHeights
	if cutoff <= 0 {
		return
	}
	c.sessionCache.Range(func(k, v any) bool {
		if e, ok := v.(sessionCacheL1Entry); ok && e.height < cutoff {
			c.sessionCache.Delete(k)
		}
		return true
	})
}

// RedisSessionCache implements SessionCache using Redis as L2 cache.
type RedisSessionCache struct {
	logger        logging.Logger
	redisClient   *redisutil.Client
	sessionClient client.SessionQueryClient
	sharedClient  client.SharedQueryClient
	blockClient   client.BlockClient
	config        CacheConfig

	// L1 local cache for sessions, TTL-bounded by sessionCacheL1TTL so an entry
	// is never frozen for the process lifetime.
	sessionCache sync.Map // map[string]sessionCacheL1Entry

	// Cache keys helper
	keys CacheKeys

	// Lifecycle
	mu       sync.RWMutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewRedisSessionCache creates a new SessionCache backed by Redis.
func NewRedisSessionCache(
	logger logging.Logger,
	redisClient *redisutil.Client,
	sessionClient client.SessionQueryClient,
	sharedClient client.SharedQueryClient,
	blockClient client.BlockClient,
	config CacheConfig,
) *RedisSessionCache {
	if config.CachePrefix == "" {
		config.CachePrefix = redisClient.KB().CachePrefix()
	}
	if config.BlockTimeSeconds == 0 {
		config.BlockTimeSeconds = 6
	}
	if config.ExtraGracePeriodBlocks == 0 {
		config.ExtraGracePeriodBlocks = 2
	}

	return &RedisSessionCache{
		logger:        logging.ForComponent(logger, logging.ComponentSessionCache),
		redisClient:   redisClient,
		sessionClient: sessionClient,
		sharedClient:  sharedClient,
		blockClient:   blockClient,
		config:        config,
		keys:          CacheKeys{Prefix: config.CachePrefix},
	}
}

// Start begins the cache's background processes.
func (c *RedisSessionCache) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("cache is closed")
	}

	_, c.cancelFn = context.WithCancel(ctx)
	c.mu.Unlock()

	c.logger.Info().Msg("session cache started")
	return nil
}

// GetSession returns the session for the given application, service, and block height.
func (c *RedisSessionCache) GetSession(ctx context.Context, appAddress, serviceId string, height int64) (*sessiontypes.Session, error) {
	start := time.Now()

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, fmt.Errorf("cache is closed")
	}
	c.mu.RUnlock()

	key := c.keys.Session(appAddress, serviceId, height)

	// L1: Check local cache. Only a fresh entry (within sessionCacheL1TTL) is a
	// hit; a stale one falls through to L2/L3 so the entry can never be frozen
	// for the process lifetime.
	if cached, ok := c.sessionCache.Load(key); ok {
		if entry := cached.(sessionCacheL1Entry); time.Since(entry.cachedAt) < sessionCacheL1TTL {
			cacheHits.WithLabelValues("session", CacheLevelL1).Inc()
			cacheGetLatency.WithLabelValues("session", CacheLevelL1).Observe(time.Since(start).Seconds())
			c.logger.Debug().
				Str("app_address", appAddress).
				Str("service_id", serviceId).
				Int64("height", height).
				Msg("session cache hit (L1)")
			return entry.session, nil
		}
	}
	cacheMisses.WithLabelValues("session", "l1").Inc()

	// L2: Check Redis cache
	data, err := c.redisClient.Get(ctx, key).Bytes()
	if err == nil {
		session := &sessiontypes.Session{}
		if unmarshalErr := json.Unmarshal(data, session); unmarshalErr != nil {
			c.logger.Warn().Err(unmarshalErr).Msg("failed to unmarshal cached session")
		} else {
			cacheHits.WithLabelValues("session", CacheLevelL2).Inc()
			cacheGetLatency.WithLabelValues("session", CacheLevelL2).Observe(time.Since(start).Seconds())
			c.storeSession(key, height, session)
			c.logger.Debug().
				Str("app_address", appAddress).
				Str("service_id", serviceId).
				Int64("height", height).
				Msg("session cache hit (L2) → stored in L1")
			return session, nil
		}
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		c.logger.Warn().Err(err).Msg("error fetching session from Redis")
	}
	cacheMisses.WithLabelValues("session", "l2").Inc()

	// L3: Query chain
	chainStart := time.Now()
	session, err := c.sessionClient.GetSession(ctx, appAddress, serviceId, height)
	chainQueryLatency.WithLabelValues("session").Observe(time.Since(chainStart).Seconds())

	if err != nil {
		chainQueryErrors.WithLabelValues("session").Inc()
		cacheGetLatency.WithLabelValues("session", "l3_error").Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("failed to query session: %w", err)
	}

	chainQueries.WithLabelValues("session").Inc()
	cacheGetLatency.WithLabelValues("session", CacheLevelL3).Observe(time.Since(start).Seconds())

	// Calculate TTL based on session end height
	ttl := c.calculateSessionTTL(ctx, session.Header.SessionEndBlockHeight)

	// Cache in Redis (L2). The sha256 stays at Debug as a cheap determinism
	// probe (two instances fetching the same session must log the same hash);
	// the raw session dump this block used to emit at Info on EVERY L3 miss
	// predates that investigation closing and violated the per-request
	// logging policy.
	data, err = json.Marshal(session)
	if err == nil {
		hash := sha256.Sum256(data)
		c.logger.Debug().
			Str("session_id", session.SessionId).
			Str("service_id", serviceId).
			Str("app_address", appAddress).
			Int64("height", height).
			Str("sha256", hex.EncodeToString(hash[:])).
			Msg("session fetched from chain")
		if err = c.redisClient.Set(ctx, key, data, ttl).Err(); err != nil {
			c.logger.Debug().Err(err).Msg("failed to cache session in Redis")
		}
	} else {
		// Per-L3-miss; the session is still served and cached in L1.
		c.logger.Warn().Err(err).Msg("failed to marshal session for caching in Redis (L2)")
	}

	// Cache in L1
	c.storeSession(key, height, session)
	c.logger.Debug().
		Str("app_address", appAddress).
		Str("service_id", serviceId).
		Int64("height", height).
		Msg("session cache miss (L3) -> stored in L1 and L2")

	return session, nil
}

// calculateSessionTTL calculates how long to cache a session.
func (c *RedisSessionCache) calculateSessionTTL(ctx context.Context, sessionEndHeight int64) time.Duration {
	currentBlock := c.blockClient.LastBlock(ctx)
	currentHeight := currentBlock.Height()

	// Get shared params for grace period
	sharedParams, err := c.sharedClient.GetParams(ctx)
	if err != nil {
		// Fall back to default TTL on error
		return time.Duration(c.config.BlockTimeSeconds*10) * time.Second
	}

	// Calculate when session is no longer valid (end + grace + extra grace)
	onChainGrace := int64(sharedParams.GetGracePeriodEndOffsetBlocks())
	extraGrace := c.config.ExtraGracePeriodBlocks
	sessionValidUntil := sessionEndHeight + onChainGrace + extraGrace

	// If session is already past validity, short TTL
	if currentHeight >= sessionValidUntil {
		return time.Duration(c.config.BlockTimeSeconds) * time.Second
	}

	// TTL = blocks remaining * block time + buffer
	blocksRemaining := sessionValidUntil - currentHeight
	return time.Duration(blocksRemaining*c.config.BlockTimeSeconds+10) * time.Second
}

// Close gracefully shuts down the cache.
func (c *RedisSessionCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true

	if c.cancelFn != nil {
		c.cancelFn()
	}

	c.wg.Wait()

	c.logger.Info().Msg("session cache closed")
	return nil
}
