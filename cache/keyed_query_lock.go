package cache

import (
	"context"
	"fmt"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// keyedQueryLockSpec parameterizes queryKeyedChainWithLock for one keyed cache
// type (application, service, account). It captures the only things that differ
// between those caches: the metric/cache-type label, the chain-query metric
// label, the log field used for the key, the per-entry "another instance is
// querying X" message, and the callbacks that touch typed state (L2 read,
// L2-retry-hit handling, chain query).
//
// The control flow in queryKeyedChainWithLock is a byte-for-byte transcription
// of the previous per-cache queryChainWithLock implementations: same SetNX lock
// (5s TTL), same 5ms contention sleep, same try-lock → sleep → retry-L2 →
// chain-query order, same metric increments. Only the textual duplication is
// removed.
type keyedQueryLockSpec[V any] struct {
	// cacheType is the metric/cache-type label (e.g. applicationCacheType). It is
	// used for the lock key, the cache key, lockAcquisitions and cacheHits labels.
	cacheType string
	// chainLabel is the label for chain-query metrics (chainQueries /
	// chainQueryLatency / chainQueryErrors). Historically the keyed caches passed
	// the same string as cacheType, but it is kept separate to preserve the exact
	// literals from each call site.
	chainLabel string
	// logKeyField is the structured-log field name for the key (e.g.
	// logging.FieldAppAddress, logging.FieldServiceID, or "address").
	logKeyField string
	// waitingMsg is the debug message logged on lock contention (e.g.
	// "another instance is querying application, waiting").
	waitingMsg string

	// loadFromRedis reads and decodes the value from L2 (Redis) for the given
	// key. It returns ok=false on any miss or decode failure (the caller then
	// falls through to a chain query), mirroring the original
	// `if err == nil { if unmarshal == nil {...} }` nesting.
	loadFromRedis func(ctx context.Context, key string) (val V, ok bool)
	// onRetryHit is invoked with the value decoded from L2 during the post-sleep
	// retry, immediately before it is returned. It exists so the caches that warm
	// L1 on a retry hit (application, service) can do so, while account — which
	// historically did not store L1 on retry — passes a no-op.
	onRetryHit func(key string, val V)
	// queryChain performs the L3 chain query for the given key.
	queryChain func(ctx context.Context, key string) (V, error)
}

// queryKeyedChainWithLock queries the chain for a keyed cache entry with
// distributed locking to prevent duplicate queries from multiple instances.
//
// This is the shared implementation behind the application, service and account
// caches' queryChainWithLock methods. It preserves their exact runtime behavior:
//   - SetNX lock acquisition with a 5s TTL
//   - on contention: a 5ms sleep then a single L2 retry
//   - on still-miss: a chain query
//   - the lockAcquisitions / cacheHits(l2_retry) / chainQueries /
//     chainQueryLatency / chainQueryErrors metric increments
func queryKeyedChainWithLock[V any](
	ctx context.Context,
	redisClient *redisutil.Client,
	logger logging.Logger,
	key string,
	spec keyedQueryLockSpec[V],
) (V, error) {
	var zero V

	lockKey := redisClient.KB().CacheLockKey(spec.cacheType, key)

	// Try to acquire distributed lock
	lockToken := newLockToken()
	locked, err := redisClient.SetNX(ctx, lockKey, lockToken, 5*time.Second).Result()
	if err != nil {
		return zero, fmt.Errorf("failed to acquire lock: %w", err)
	}
	// Release only a lock we hold. A contended loser that falls through to its
	// own chain query must NOT delete the winner's still-held lock on the way
	// out -- that lets a third instance acquire immediately and fire another
	// duplicate query, defeating the dedup this lock exists for.
	if locked {
		defer releaseCacheLock(ctx, redisClient, lockKey, lockToken)
	}

	if !locked {
		// Another instance is querying, wait and retry L2
		lockAcquisitions.WithLabelValues(spec.cacheType, "contended").Inc()
		logger.Debug().
			Str(spec.logKeyField, key).
			Msg(spec.waitingMsg)
		time.Sleep(5 * time.Millisecond)

		// Retry L2 after waiting
		if val, ok := spec.loadFromRedis(ctx, key); ok {
			spec.onRetryHit(key, val)
			cacheHits.WithLabelValues(spec.cacheType, CacheLevelL2Retry).Inc()
			return val, nil
		}

		// If still not in Redis, query chain anyway
	} else {
		lockAcquisitions.WithLabelValues(spec.cacheType, "acquired").Inc()
	}

	// Query chain
	chainQueries.WithLabelValues(spec.chainLabel).Inc()
	chainStart := time.Now()

	val, err := spec.queryChain(ctx, key)
	chainQueryLatency.WithLabelValues(spec.chainLabel).Observe(time.Since(chainStart).Seconds())

	if err != nil {
		chainQueryErrors.WithLabelValues(spec.chainLabel).Inc()
		return zero, err
	}

	return val, nil
}

// warmupKeyedFromRedis loads a single keyed entry from Redis (L2) into L1.
//
// This is the shared implementation behind warmupSingleApp / warmupSingleService.
// It preserves their exact behavior: a Redis miss returns nil (skip, no error),
// a decode failure logs a warning and returns the decode error, and a success
// stores the decoded value in L1 via storeL1.
func warmupKeyedFromRedis[V any](
	ctx context.Context,
	redisClient *redisutil.Client,
	logger logging.Logger,
	cacheType string,
	logKeyField string,
	unmarshalFailMsg string,
	key string,
	unmarshal func([]byte) (V, error),
	storeL1 func(key string, val V),
) error {
	// Load from Redis (L2) into local cache (L1)
	redisKey := redisClient.KB().CacheKey(cacheType, key)
	data, err := redisClient.Get(ctx, redisKey).Bytes()
	if err != nil {
		// Key doesn't exist in Redis, skip
		return nil
	}

	val, err := unmarshal(data)
	if err != nil {
		logger.Warn().
			Err(err).
			Str(logKeyField, key).
			Msg(unmarshalFailMsg)
		return err
	}

	storeL1(key, val)
	return nil
}
