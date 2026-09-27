//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestNewSupplierManager_ForwardsBlockTimeSecondsToDeduplicator is the
// test-teeth for the dedup TTL wiring bug (found 2026-08-21 while verifying
// mainnet's real block time): NewSupplierManager used to construct the
// deduplicator with an empty DeduplicatorConfig{}, so the operator's
// configured BlockTimeSeconds never reached it and NewRedisDeduplicator's
// own fallback (30) took over regardless -- a well-configured operator
// (block_time_seconds: 64, mainnet's verified value) still got a dedup TTL
// computed with 30. On mainnet that produced TTLBlocks(10)*30s=5min instead
// of the intended ~10.7min, so a relay redelivered between 5 and 10.7
// minutes after the first copy would no longer be recognised as a duplicate
// and would be counted twice -- the exact over-count the deduplicator's own
// doc comment warns inflates the economic-viability prediction.
//
// Asserts the CONSEQUENCE (the Redis key's actual TTL after a real
// MarkProcessed call), not the wiring mechanism, so it cannot be satisfied
// by a config value that is merely stored somewhere but never used.
func TestNewSupplierManager_ForwardsBlockTimeSecondsToDeduplicator(t *testing.T) {
	ctx := context.Background()
	redisClient, _ := newTestRedis(t)

	pool := pond.NewPool(4)
	defer pool.StopAndWait()

	const configuredBlockTimeSeconds = 64 // mainnet, verified live 2026-08-21

	mgr := NewSupplierManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		nil, // keyManager: unused before Start()
		nil, // registry: unused before Start()
		SupplierManagerConfig{
			RedisClient:      redisClient,
			MinerID:          "test-miner",
			WorkerPool:       pool,
			BlockTimeSeconds: configuredBlockTimeSeconds,
		},
	)
	require.NotNil(t, mgr.deduplicator, "a Redis client was supplied, so a deduplicator must exist")

	const sessionID = "sess-dedup-wiring"
	_, err := mgr.deduplicator.MarkProcessed(ctx, []byte("relay-hash-1"), sessionID)
	require.NoError(t, err)

	key := redisClient.KB().MinerDedupSessionKey(sessionID)
	ttl, err := redisClient.TTL(ctx, key).Result()
	require.NoError(t, err)

	// TTLBlocks defaults to 10 (NewRedisDeduplicator). With the fix,
	// BlockTimeSeconds=64 flows through: 10*64s = 640s. The pre-fix
	// behaviour would have produced 10*30s = 300s regardless of what was
	// configured -- assert well above that wrong value, not just "positive".
	wantTTL := 10 * configuredBlockTimeSeconds * time.Second
	require.InDelta(t, wantTTL.Seconds(), ttl.Seconds(), 5,
		"dedup TTL must be computed from the configured BlockTimeSeconds (64s), not silently "+
			"fall back to NewRedisDeduplicator's own 30s default")
}
