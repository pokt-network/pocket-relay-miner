//go:build test

package miner

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/suite"

	"github.com/pokt-network/pocket-relay-miner/testutil"
)

// DeduplicatorTestSuite tests the RedisDeduplicator implementation.
// This characterizes how relay deduplication works across distributed miner instances.
type DeduplicatorTestSuite struct {
	testutil.RedisTestSuite
	deduplicator *RedisDeduplicator
}

// SetupTest creates a fresh deduplicator for each test.
func (s *DeduplicatorTestSuite) SetupTest() {
	// Call parent to flush Redis
	s.RedisTestSuite.SetupTest()

	// Create deduplicator with test config
	config := DeduplicatorConfig{
		KeyPrefix:              "ha:miner:dedup",
		TTLBlocks:              10,
		BlockTimeSeconds:       1, // Short TTL for test speed
		LocalCacheSize:         100,
		CleanupIntervalSeconds: 60,
	}

	logger := zerolog.Nop()
	s.deduplicator = NewRedisDeduplicator(logger, s.RedisClient, config)
}

// TearDownTest cleans up the deduplicator.
func (s *DeduplicatorTestSuite) TearDownTest() {
	if s.deduplicator != nil {
		_ = s.deduplicator.Close()
	}
}

// TestDeduplicator_FirstRelayNotDuplicate verifies that the first relay is not marked as duplicate.
func (s *DeduplicatorTestSuite) TestDeduplicator_FirstRelayNotDuplicate() {
	ctx := context.Background()
	sessionID := "session_001"
	relayHash := []byte("relay_hash_001")

	// First check should return false (not a duplicate)
	isDup, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID)
	s.Require().NoError(err)
	s.Require().False(isDup, "first relay should not be marked as duplicate")

	// Verify key doesn't exist yet in Redis (IsDuplicate doesn't add to Redis)
	key := s.deduplicator.sessionKey(sessionID)
	exists, err := s.RedisClient.Exists(ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(0), exists, "key should not exist until MarkProcessed")
}

// TestDeduplicator_DuplicateRelayDetected verifies that duplicate relays are detected.
func (s *DeduplicatorTestSuite) TestDeduplicator_DuplicateRelayDetected() {
	ctx := context.Background()
	sessionID := "session_002"
	relayHash := []byte("relay_hash_002")

	// Mark as processed
	err := s.deduplicator.MarkProcessed(ctx, relayHash, sessionID)
	s.Require().NoError(err)

	// Check again - should be duplicate now
	isDup, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID)
	s.Require().NoError(err)
	s.Require().True(isDup, "relay should be marked as duplicate after MarkProcessed")

	// Verify it's in Redis
	key := s.deduplicator.sessionKey(sessionID)
	hashKey := hex.EncodeToString(relayHash)
	isMember, err := s.RedisClient.SIsMember(ctx, key, hashKey).Result()
	s.Require().NoError(err)
	s.Require().True(isMember, "relay hash should be in Redis set")
}

// TestDeduplicator_DifferentSessionsIndependent verifies that sessions have independent dedup sets.
func (s *DeduplicatorTestSuite) TestDeduplicator_DifferentSessionsIndependent() {
	ctx := context.Background()
	sessionID1 := "session_003"
	sessionID2 := "session_004"
	relayHash := []byte("relay_hash_003") // Same hash for both sessions

	// Mark as processed in session 1
	err := s.deduplicator.MarkProcessed(ctx, relayHash, sessionID1)
	s.Require().NoError(err)

	// Should be duplicate in session 1
	isDup1, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID1)
	s.Require().NoError(err)
	s.Require().True(isDup1, "relay should be duplicate in session 1")

	// Should NOT be duplicate in session 2 (independent sessions)
	isDup2, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID2)
	s.Require().NoError(err)
	s.Require().False(isDup2, "relay should not be duplicate in session 2")
}

// TestDeduplicator_MultipleRelaysTracked verifies that multiple relays are tracked correctly.
func (s *DeduplicatorTestSuite) TestDeduplicator_MultipleRelaysTracked() {
	ctx := context.Background()
	sessionID := "session_005"

	// Mark 5 different relays as processed
	for i := 0; i < 5; i++ {
		relayHash := []byte(fmt.Sprintf("relay_hash_%d", i))
		err := s.deduplicator.MarkProcessed(ctx, relayHash, sessionID)
		s.Require().NoError(err)
	}

	// Verify all are marked as duplicates
	for i := 0; i < 5; i++ {
		relayHash := []byte(fmt.Sprintf("relay_hash_%d", i))
		isDup, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID)
		s.Require().NoError(err)
		s.Require().True(isDup, "relay %d should be duplicate", i)
	}

	// Verify count in Redis
	key := s.deduplicator.sessionKey(sessionID)
	count, err := s.RedisClient.SCard(ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(5), count, "should have 5 relay hashes")
}

// TestDeduplicator_CleanupRemovesEntries verifies that CleanupSession removes all entries.
func (s *DeduplicatorTestSuite) TestDeduplicator_CleanupRemovesEntries() {
	ctx := context.Background()
	sessionID := "session_006"

	// Mark 3 relays as processed
	for i := 0; i < 3; i++ {
		relayHash := []byte(fmt.Sprintf("relay_hash_cleanup_%d", i))
		err := s.deduplicator.MarkProcessed(ctx, relayHash, sessionID)
		s.Require().NoError(err)
	}

	// Verify they exist
	key := s.deduplicator.sessionKey(sessionID)
	count, err := s.RedisClient.SCard(ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(3), count)

	// Cleanup session
	err = s.deduplicator.CleanupSession(ctx, sessionID)
	s.Require().NoError(err)

	// Verify Redis key is deleted
	exists, err := s.RedisClient.Exists(ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(0), exists, "key should be deleted after cleanup")

	// Verify relays are not duplicates after cleanup
	for i := 0; i < 3; i++ {
		relayHash := []byte(fmt.Sprintf("relay_hash_cleanup_%d", i))
		isDup, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID)
		s.Require().NoError(err)
		s.Require().False(isDup, "relay should not be duplicate after cleanup")
	}
}

// TestDeduplicator_ConcurrentDedup verifies concurrent deduplication is safe.
// This test characterizes behavior under concurrent access from multiple goroutines.
func (s *DeduplicatorTestSuite) TestDeduplicator_ConcurrentDedup() {
	ctx := context.Background()
	sessionID := "session_007"
	relayHash := []byte("concurrent_relay_hash")

	var wg sync.WaitGroup
	goroutines := 10
	wg.Add(goroutines)

	// Multiple goroutines try to mark the same relay
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = s.deduplicator.MarkProcessed(ctx, relayHash, sessionID)
		}()
	}

	wg.Wait()

	// Verify relay is marked as duplicate (idempotent)
	isDup, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID)
	s.Require().NoError(err)
	s.Require().True(isDup, "relay should be duplicate after concurrent marks")

	// Verify only one entry in Redis (no duplicates in set)
	key := s.deduplicator.sessionKey(sessionID)
	count, err := s.RedisClient.SCard(ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(1), count, "should have exactly 1 entry despite concurrent marks")
}

// TestDeduplicator_MarkProcessedBatch verifies batch marking works correctly.
func (s *DeduplicatorTestSuite) TestDeduplicator_MarkProcessedBatch() {
	ctx := context.Background()
	sessionID := "session_008"

	// Create batch of relay hashes
	relayHashes := make([][]byte, 10)
	for i := 0; i < 10; i++ {
		relayHashes[i] = []byte(fmt.Sprintf("batch_relay_%d", i))
	}

	// Mark batch
	err := s.deduplicator.MarkProcessedBatch(ctx, relayHashes, sessionID)
	s.Require().NoError(err)

	// Verify all are marked as duplicates
	for i := 0; i < 10; i++ {
		isDup, err := s.deduplicator.IsDuplicate(ctx, relayHashes[i], sessionID)
		s.Require().NoError(err)
		s.Require().True(isDup, "batch relay %d should be duplicate", i)
	}

	// Verify count in Redis
	key := s.deduplicator.sessionKey(sessionID)
	count, err := s.RedisClient.SCard(ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(10), count, "should have 10 relay hashes from batch")
}

// TestDeduplicator_LocalCacheHit verifies local cache improves performance.
func (s *DeduplicatorTestSuite) TestDeduplicator_LocalCacheHit() {
	ctx := context.Background()
	sessionID := "session_009"
	relayHash := []byte("local_cache_relay")

	// Mark as processed (adds to Redis and local cache)
	err := s.deduplicator.MarkProcessed(ctx, relayHash, sessionID)
	s.Require().NoError(err)

	// First IsDuplicate call hits Redis, adds to local cache
	isDup1, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID)
	s.Require().NoError(err)
	s.Require().True(isDup1)

	// Delete from Redis to verify local cache is used
	key := s.deduplicator.sessionKey(sessionID)
	err = s.RedisClient.Del(ctx, key).Err()
	s.Require().NoError(err)

	// Should still be duplicate (from local cache)
	isDup2, err := s.deduplicator.IsDuplicate(ctx, relayHash, sessionID)
	s.Require().NoError(err)
	s.Require().True(isDup2, "should hit local cache even after Redis deletion")
}

// TestDeduplicator_TTLExpires verifies TTL is set on Redis keys.
func (s *DeduplicatorTestSuite) TestDeduplicator_TTLExpires() {
	ctx := context.Background()
	sessionID := "session_010"
	relayHash := []byte("ttl_relay")

	// Mark as processed
	err := s.deduplicator.MarkProcessed(ctx, relayHash, sessionID)
	s.Require().NoError(err)

	// Verify TTL is set (config: 10 blocks * 1 second = 10 seconds)
	key := s.deduplicator.sessionKey(sessionID)
	ttl, err := s.RedisClient.TTL(ctx, key).Result()
	s.Require().NoError(err)
	s.Require().True(ttl > 0, "TTL should be set")
	s.Require().True(ttl <= 10*time.Second, "TTL should be <= 10 seconds")
}

// TestDeduplicatorTestSuite runs the test suite.
func TestDeduplicatorTestSuite(t *testing.T) {
	suite.Run(t, new(DeduplicatorTestSuite))
}
