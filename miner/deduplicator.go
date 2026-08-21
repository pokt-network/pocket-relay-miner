package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Deduplicator ensures that redelivered relays (reclaims from a consumer that
// crashed without acking, or the original copy still buffered in a
// slow-but-alive consumer after another consumer reclaimed it) are not
// counted twice. The SMST tree is idempotent on insertions of the same
// (key, value, weight) tuple, but the side counter
// `snapshot.TotalComputeUnits` is incremented unconditionally by
// IncrementRelayCount and must be protected against double-count:
// over-counting there would inflate the economic-viability prediction and
// cause unprofitable sessions to be claimed.
//
// The relay worker calls MarkProcessed on EVERY relay and uses its return
// value as the gate for the counter: the reclaim skips entries owned by this
// consumer and takes only those idle past the timeout, but idleness cannot
// distinguish a dead consumer from a slow-but-alive one — so the duplicate can
// arrive with IsReclaim=false (the original copy, processed after the reclaimed
// one) and a reclaim-only check would miss it. IsDuplicate remains as a cheap early exit on the reclaim
// path before the SMST work.
type Deduplicator interface {
	// IsDuplicate returns true if the relay hash has already been marked as
	// processed for the given session.
	IsDuplicate(ctx context.Context, relayHash []byte, sessionID string) (bool, error)

	// MarkProcessed records that a relay hash has been processed. Called
	// unconditionally by the relay worker after a successful SMST update.
	// Returns whether the hash was newly added: false means another
	// processing of the same relay already marked it, and the caller must
	// not increment the per-session counters again.
	MarkProcessed(ctx context.Context, relayHash []byte, sessionID string) (bool, error)

	// MarkProcessedBatch records multiple relay hashes in a single pipeline.
	MarkProcessedBatch(ctx context.Context, relayHashes [][]byte, sessionID string) error

	// CleanupSession removes the deduplication set for a session. Called when
	// a session reaches a terminal state so Redis memory is reclaimed.
	CleanupSession(ctx context.Context, sessionID string) error

	// Start is a no-op kept for interface symmetry. The deduplicator holds no
	// background goroutines; all state lives in Redis.
	Start(ctx context.Context) error

	// Close is a no-op kept for interface symmetry.
	Close() error
}

// RedisDeduplicator stores relay hashes in per-session Redis sets. Set
// members are the raw relay hash bytes (stored as strings via Go's
// bytes-as-string conversion), which avoids the ~2× memory overhead of hex
// encoding both in the client heap and in Redis storage.
type RedisDeduplicator struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	config      DeduplicatorConfig

	mu     sync.Mutex
	closed bool
}

// DeduplicatorConfig configures TTL behavior.
type DeduplicatorConfig struct {
	// TTLBlocks is how many blocks to keep entries (converted to time).
	TTLBlocks int64

	// BlockTimeSeconds is the assumed block time for TTL calculation.
	BlockTimeSeconds int64
}

// NewRedisDeduplicator constructs a Redis-backed deduplicator.
func NewRedisDeduplicator(
	logger logging.Logger,
	redisClient *redisutil.Client,
	config DeduplicatorConfig,
) *RedisDeduplicator {
	if config.TTLBlocks == 0 {
		config.TTLBlocks = 10 // session length + grace period + buffer
	}
	if config.BlockTimeSeconds == 0 {
		config.BlockTimeSeconds = cache.DefaultBlockTimeSeconds
	}

	return &RedisDeduplicator{
		logger:      logging.ForComponent(logger, logging.ComponentDeduplicator),
		redisClient: redisClient,
		config:      config,
	}
}

// Start is a no-op. The deduplicator has no background goroutines.
func (d *RedisDeduplicator) Start(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return fmt.Errorf("deduplicator is closed")
	}
	d.logger.Info().Msg("deduplicator started")
	return nil
}

// Close is a no-op kept for interface symmetry.
func (d *RedisDeduplicator) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	d.logger.Info().Msg("deduplicator closed")
	return nil
}

// IsDuplicate checks whether relayHash has already been marked as processed
// for sessionID. Returns false on Redis errors so the caller fails open
// (better to risk a rare double-count than to drop a valid relay).
func (d *RedisDeduplicator) IsDuplicate(ctx context.Context, relayHash []byte, sessionID string) (bool, error) {
	key := d.sessionKey(sessionID)
	exists, err := d.redisClient.SIsMember(ctx, key, hashMember(relayHash)).Result()
	if err != nil {
		dedupErrors.WithLabelValues("redis_check").Inc()
		return false, fmt.Errorf("failed to check Redis: %w", err)
	}
	if exists {
		dedupRedisCacheHits.Inc()
		return true, nil
	}
	dedupMisses.Inc()
	return false, nil
}

// MarkProcessed records relayHash in the session's dedup set and refreshes
// the TTL. Called after a successful SMST update. The SADD result doubles as
// the duplicate signal: 0 added members means the hash was already marked by
// an earlier processing of the same relay, and the caller must skip the
// per-session counter increment. If the caller crashes between SMST update
// and this call, the next redelivery will not detect the duplicate and
// IncrementRelayCount may run again — but that window is far smaller than
// skipping the SMST update itself, and the SMST is idempotent.
func (d *RedisDeduplicator) MarkProcessed(ctx context.Context, relayHash []byte, sessionID string) (bool, error) {
	key := d.sessionKey(sessionID)
	ttl := d.getTTL()

	pipe := d.redisClient.Pipeline()
	addCmd := pipe.SAdd(ctx, key, hashMember(relayHash))
	pipe.Expire(ctx, key, ttl)

	if _, err := pipe.Exec(ctx); err != nil {
		dedupErrors.WithLabelValues("redis_mark").Inc()
		return false, fmt.Errorf("failed to mark processed: %w", err)
	}

	dedupMarked.Inc()
	return addCmd.Val() == 1, nil
}

// MarkProcessedBatch records multiple relay hashes in a single pipeline.
func (d *RedisDeduplicator) MarkProcessedBatch(ctx context.Context, relayHashes [][]byte, sessionID string) error {
	if len(relayHashes) == 0 {
		return nil
	}

	key := d.sessionKey(sessionID)
	ttl := d.getTTL()

	members := make([]interface{}, len(relayHashes))
	for i, h := range relayHashes {
		members[i] = hashMember(h)
	}

	pipe := d.redisClient.Pipeline()
	pipe.SAdd(ctx, key, members...)
	pipe.Expire(ctx, key, ttl)

	if _, err := pipe.Exec(ctx); err != nil {
		dedupErrors.WithLabelValues("redis_batch_mark").Inc()
		return fmt.Errorf("failed to mark batch processed: %w", err)
	}

	dedupMarked.Add(float64(len(relayHashes)))
	return nil
}

// CleanupSession removes the deduplication set for a terminated session.
func (d *RedisDeduplicator) CleanupSession(ctx context.Context, sessionID string) error {
	key := d.sessionKey(sessionID)
	if err := d.redisClient.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to cleanup session: %w", err)
	}

	d.logger.Debug().
		Str("session_id", sessionID).
		Msg("cleaned up session deduplication entries")

	return nil
}

// sessionKey returns the Redis key for a session's deduplication set.
// Built through the KeyBuilder so the writer and the CLI reader can never
// drift apart under a custom namespace.
func (d *RedisDeduplicator) sessionKey(sessionID string) string {
	return d.redisClient.KB().MinerDedupSessionKey(sessionID)
}

// getTTL returns the TTL for deduplication entries.
func (d *RedisDeduplicator) getTTL() time.Duration {
	return time.Duration(d.config.TTLBlocks*d.config.BlockTimeSeconds) * time.Second
}

// hashMember converts raw relay hash bytes into the string form go-redis
// transmits to Redis. Go strings are arbitrary byte sequences and RESP3 SADD
// / SIsMember treat set members as binary-safe, so no encoding is needed.
// Passing the raw bytes avoids both client heap overhead (~64 B per hex
// string vs ~32 B raw) and Redis storage overhead.
func hashMember(relayHash []byte) string {
	return string(relayHash)
}

// Verify interface compliance.
var _ Deduplicator = (*RedisDeduplicator)(nil)
