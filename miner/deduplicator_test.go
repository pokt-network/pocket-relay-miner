//go:build test

package miner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

func setupTestDeduplicator(t *testing.T) (*RedisDeduplicator, *miniredis.Miniredis) {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)

	d := NewRedisDeduplicator(testLogger(), client, DeduplicatorConfig{
		KeyPrefix:        "ha:miner:dedup",
		TTLBlocks:        10,
		BlockTimeSeconds: 30,
	})
	require.NoError(t, d.Start(ctx))
	t.Cleanup(func() { _ = d.Close() })

	return d, mr
}

func hashOf(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// --- IsDuplicate / MarkProcessed / CleanupSession ---

func TestDeduplicator_EmptySession_IsNotDuplicate(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	ctx := context.Background()

	isDup, err := d.IsDuplicate(ctx, hashOf("relay-1"), "sess-1")
	require.NoError(t, err)
	assert.False(t, isDup)
}

func TestDeduplicator_MarkThenCheck_IsDuplicate(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	ctx := context.Background()

	h := hashOf("relay-1")
	require.NoError(t, d.MarkProcessed(ctx, h, "sess-1"))

	isDup, err := d.IsDuplicate(ctx, h, "sess-1")
	require.NoError(t, err)
	assert.True(t, isDup)
}

func TestDeduplicator_DifferentSessionsIsolated(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	ctx := context.Background()

	h := hashOf("relay-1")
	require.NoError(t, d.MarkProcessed(ctx, h, "sess-A"))

	isDupA, err := d.IsDuplicate(ctx, h, "sess-A")
	require.NoError(t, err)
	assert.True(t, isDupA, "same session: should be duplicate")

	isDupB, err := d.IsDuplicate(ctx, h, "sess-B")
	require.NoError(t, err)
	assert.False(t, isDupB, "different session: must not be duplicate")
}

func TestDeduplicator_DifferentHashesIsolated(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	ctx := context.Background()

	require.NoError(t, d.MarkProcessed(ctx, hashOf("relay-A"), "sess-1"))

	isDup, err := d.IsDuplicate(ctx, hashOf("relay-B"), "sess-1")
	require.NoError(t, err)
	assert.False(t, isDup)
}

func TestDeduplicator_MarkProcessedBatch(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	ctx := context.Background()

	batch := [][]byte{hashOf("r1"), hashOf("r2"), hashOf("r3")}
	require.NoError(t, d.MarkProcessedBatch(ctx, batch, "sess-1"))

	for i, h := range batch {
		isDup, err := d.IsDuplicate(ctx, h, "sess-1")
		require.NoError(t, err)
		assert.True(t, isDup, "batch entry %d should be duplicate", i)
	}

	// unrelated hash must not be flagged
	isDup, err := d.IsDuplicate(ctx, hashOf("r4"), "sess-1")
	require.NoError(t, err)
	assert.False(t, isDup)
}

func TestDeduplicator_MarkProcessedBatch_Empty(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	ctx := context.Background()

	// empty batch should be a no-op, no error
	err := d.MarkProcessedBatch(ctx, nil, "sess-1")
	require.NoError(t, err)

	err = d.MarkProcessedBatch(ctx, [][]byte{}, "sess-1")
	require.NoError(t, err)
}

func TestDeduplicator_CleanupSession(t *testing.T) {
	d, mr := setupTestDeduplicator(t)
	ctx := context.Background()

	h := hashOf("relay-1")
	require.NoError(t, d.MarkProcessed(ctx, h, "sess-1"))

	// sanity: the Redis set exists
	assert.True(t, mr.Exists("ha:miner:dedup:session:sess-1"))

	require.NoError(t, d.CleanupSession(ctx, "sess-1"))

	// after cleanup the set is gone
	assert.False(t, mr.Exists("ha:miner:dedup:session:sess-1"))

	// and IsDuplicate now returns false
	isDup, err := d.IsDuplicate(ctx, h, "sess-1")
	require.NoError(t, err)
	assert.False(t, isDup)
}

// --- TTL behavior ---

func TestDeduplicator_TTLAppliedOnMark(t *testing.T) {
	d, mr := setupTestDeduplicator(t)
	ctx := context.Background()

	require.NoError(t, d.MarkProcessed(ctx, hashOf("r1"), "sess-1"))

	ttl := mr.TTL("ha:miner:dedup:session:sess-1")
	expected := time.Duration(10*30) * time.Second // TTLBlocks * BlockTimeSeconds
	assert.Equal(t, expected, ttl)
}

func TestDeduplicator_TTLRefreshedOnSubsequentMark(t *testing.T) {
	d, mr := setupTestDeduplicator(t)
	ctx := context.Background()

	require.NoError(t, d.MarkProcessed(ctx, hashOf("r1"), "sess-1"))

	// fast-forward past half the TTL
	mr.FastForward(150 * time.Second)

	// second mark must refresh the expire to the full window
	require.NoError(t, d.MarkProcessed(ctx, hashOf("r2"), "sess-1"))

	ttl := mr.TTL("ha:miner:dedup:session:sess-1")
	expected := time.Duration(10*30) * time.Second
	assert.Equal(t, expected, ttl)
}

// --- Raw-byte storage (no hex encoding) ---

func TestDeduplicator_StoresRawBytesNotHex(t *testing.T) {
	d, mr := setupTestDeduplicator(t)
	ctx := context.Background()

	h := hashOf("relay-1") // 32 bytes
	require.NoError(t, d.MarkProcessed(ctx, h, "sess-1"))

	members, err := mr.SMembers("ha:miner:dedup:session:sess-1")
	require.NoError(t, err)
	require.Len(t, members, 1)

	// Must be the raw bytes (32 chars of arbitrary bytes), not hex (64 chars).
	assert.Equal(t, 32, len(members[0]), "member should be raw 32-byte sha256, not 64-char hex")
	assert.Equal(t, string(h), members[0])
}

// --- Concurrent access (race detector) ---

func TestDeduplicator_ConcurrentMarkAndCheck(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	ctx := context.Background()

	const (
		writers      = 8
		relaysPerOne = 100
		sessionID    = "sess-conc"
	)

	var wg sync.WaitGroup
	wg.Add(writers * 2)

	// concurrent writers
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < relaysPerOne; i++ {
				h := hashOf(fmt.Sprintf("w%d-r%d", w, i))
				if err := d.MarkProcessed(ctx, h, sessionID); err != nil {
					t.Errorf("mark failed: %v", err)
					return
				}
			}
		}(w)
	}

	// concurrent readers — must not race, may miss entries that are in-flight
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < relaysPerOne; i++ {
				h := hashOf(fmt.Sprintf("w%d-r%d", w, i))
				_, err := d.IsDuplicate(ctx, h, sessionID)
				if err != nil {
					t.Errorf("check failed: %v", err)
					return
				}
			}
		}(w)
	}

	wg.Wait()

	// Every written hash must be detected as duplicate after the writers finish.
	for w := 0; w < writers; w++ {
		for i := 0; i < relaysPerOne; i++ {
			h := hashOf(fmt.Sprintf("w%d-r%d", w, i))
			isDup, err := d.IsDuplicate(ctx, h, sessionID)
			require.NoError(t, err)
			assert.Truef(t, isDup, "relay w%d-r%d should be duplicate after concurrent writes", w, i)
		}
	}
}

// --- Error propagation ---

func TestDeduplicator_RedisErrorOnCheck_FailsOpen(t *testing.T) {
	d, mr := setupTestDeduplicator(t)
	ctx := context.Background()

	// Close miniredis to simulate connection failure
	mr.Close()

	isDup, err := d.IsDuplicate(ctx, hashOf("r1"), "sess-1")
	require.Error(t, err, "expected redis error")
	assert.False(t, isDup, "must return false on error (fail-open)")
}

func TestDeduplicator_RedisErrorOnMark_Propagates(t *testing.T) {
	d, mr := setupTestDeduplicator(t)
	ctx := context.Background()

	mr.Close()

	err := d.MarkProcessed(ctx, hashOf("r1"), "sess-1")
	require.Error(t, err)
}

// --- Start/Close lifecycle ---

func TestDeduplicator_StartIsIdempotent(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	ctx := context.Background()

	// Start already called in setup; calling again should succeed.
	require.NoError(t, d.Start(ctx))
}

func TestDeduplicator_CloseIsIdempotent(t *testing.T) {
	d, _ := setupTestDeduplicator(t)

	require.NoError(t, d.Close())
	require.NoError(t, d.Close(), "second Close must be a no-op")
}

func TestDeduplicator_StartAfterCloseErrors(t *testing.T) {
	d, _ := setupTestDeduplicator(t)
	require.NoError(t, d.Close())

	err := d.Start(context.Background())
	require.Error(t, err, "Start on a closed deduplicator must error")
}

// --- Config defaults ---

func TestDeduplicator_EmptyConfigGetsDefaults(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)

	d := NewRedisDeduplicator(testLogger(), client, DeduplicatorConfig{})
	require.NoError(t, d.Start(ctx))
	t.Cleanup(func() { _ = d.Close() })

	// Mark a relay to trigger the TTL application.
	require.NoError(t, d.MarkProcessed(ctx, hashOf("r1"), "sess-1"))

	// Default key prefix.
	assert.True(t, mr.Exists("ha:miner:dedup:session:sess-1"))

	// Default TTL: 10 blocks × 30 s = 300 s
	ttl := mr.TTL("ha:miner:dedup:session:sess-1")
	assert.Equal(t, 300*time.Second, ttl)
}

// --- Interface compliance ---

func TestDeduplicator_ImplementsInterface(t *testing.T) {
	var _ Deduplicator = (*RedisDeduplicator)(nil)
}
