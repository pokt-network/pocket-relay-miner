//go:build test

package miner

// Tests for the sliding-TTL refresh inside FlushOrphansWithLiveRoot and
// the memory-only behavior of evictCorruptSession. Both are defensive
// fixes against the production corruption shape where:
//
//   1. A session lives longer than cache_ttl. The nodes hash was given
//      a TTL at creation and never refreshed, so it expires in Redis
//      while live_root (persistent) still points into the now-empty
//      hash. Next UpdateTree / failover resume reads live_root, tries
//      to traverse, hits a missing digest, panics.
//   2. Corruption is detected and the session is evicted. The prior
//      implementation wiped every Redis key for that session, which
//      guaranteed full data loss even when the corruption was
//      recoverable. The new implementation drops only the in-memory
//      cached tree; the next UpdateTree lets resumeTreeFromRedisLocked
//      decide whether the Redis state is usable.

import (
	"time"

	"github.com/rs/zerolog"
)

// TestFlushOrphansWithLiveRoot_RefreshesTTL verifies that every
// checkpoint pushes the TTL of the nodes hash AND the live_root key to
// cache_ttl. A session that keeps receiving relays must not have its
// nodes hash expire underneath it — that is the corruption shape
// producing the parseSumTrieNode panic in Anaski's stack.
func (s *RedisSMSTTestSuite) TestFlushOrphansWithLiveRoot_RefreshesTTL() {
	supplier := "pokt1sliding_ttl"
	sessionID := "session_sliding_ttl"
	// Short cache TTL so the refresh is observable without real-time
	// sleeps. miniredis.FastForward lets us age keys deterministically.
	const cacheTTL = 30 * time.Second

	config := RedisSMSTManagerConfig{
		SupplierAddress:            supplier,
		CacheTTL:                   cacheTTL,
		LiveRootCheckpointInterval: 1, // checkpoint every update
	}
	mgr := NewRedisSMSTManager(zerolog.Nop(), s.redisClient, config)

	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)

	// First update creates the tree AND hits the checkpoint branch
	// (updateCount == 1). Both keys should have TTL ≈ cacheTTL.
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID, []byte("k1"), []byte("v1"), 10))

	ttlNodes := s.miniRedis.TTL(nodesKey)
	ttlLive := s.miniRedis.TTL(liveKey)
	s.Require().Equalf(cacheTTL, ttlNodes,
		"first checkpoint must refresh nodes-hash TTL to cache_ttl (got %s)", ttlNodes)
	s.Require().Equalf(cacheTTL, ttlLive,
		"first checkpoint must refresh live_root TTL to cache_ttl (got %s)", ttlLive)

	// Age both keys most of the way to expiry without crossing it.
	s.miniRedis.FastForward(cacheTTL - 5*time.Second)
	ttlNodesAged := s.miniRedis.TTL(nodesKey)
	s.Require().Lessf(ttlNodesAged, cacheTTL-time.Second,
		"after fast-forward the TTL must have decayed (got %s)", ttlNodesAged)

	// Another checkpoint MUST push the TTL back out to cache_ttl.
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID, []byte("k2"), []byte("v2"), 10))

	ttlNodesRefreshed := s.miniRedis.TTL(nodesKey)
	ttlLiveRefreshed := s.miniRedis.TTL(liveKey)
	s.Require().Equalf(cacheTTL, ttlNodesRefreshed,
		"subsequent checkpoint must refresh nodes-hash TTL (got %s)", ttlNodesRefreshed)
	s.Require().Equalf(cacheTTL, ttlLiveRefreshed,
		"subsequent checkpoint must refresh live_root TTL (got %s)", ttlLiveRefreshed)
}

// TestFlushOrphansWithLiveRoot_NoTTLWhenCacheTTLZero verifies that
// cache_ttl == 0 (the tests + operators who want persistent keys)
// disables the sliding TTL refresh. The nodes hash and live_root stay
// persistent until explicit DeleteTree.
func (s *RedisSMSTTestSuite) TestFlushOrphansWithLiveRoot_NoTTLWhenCacheTTLZero() {
	supplier := "pokt1no_ttl"
	sessionID := "session_no_ttl"

	mgr := s.createTestRedisSMSTManager(supplier) // CacheTTL=0 by default
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID, []byte("k1"), []byte("v1"), 10))

	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)

	s.Require().Equalf(time.Duration(0), s.miniRedis.TTL(nodesKey),
		"cache_ttl=0 must leave nodes-hash without a TTL")
	s.Require().Equalf(time.Duration(0), s.miniRedis.TTL(liveKey),
		"cache_ttl=0 must leave live_root without a TTL")
}

// TestEvictCorruptSession_PreservesRedisState verifies that the
// defensive eviction drops ONLY the in-memory cached tree. Redis-backed
// state is kept so resumeTreeFromRedisLocked can try to recover on the
// next UpdateTree. This is the softer alternative to the previous
// full-wipe behavior that guaranteed data loss on any corruption signal.
func (s *RedisSMSTTestSuite) TestEvictCorruptSession_PreservesRedisState() {
	supplier := "pokt1evict_memory_only"
	sessionID := "session_evict_memory_only"

	mgr := s.createTestRedisSMSTManager(supplier)
	// Seed with enough relays to have a meaningful tree in Redis.
	for i := 0; i < 8; i++ {
		s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
			[]byte{byte(i)}, []byte{byte(i + 100)}, 10))
	}

	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)

	s.Require().True(s.miniRedis.Exists(nodesKey), "nodes hash must exist after seeding")
	s.Require().True(s.miniRedis.Exists(liveKey), "live_root must exist after seeding")
	s.Require().Equal(1, mgr.GetTreeCount(), "one in-memory tree before eviction")

	// Trigger eviction directly (bypassing the corruption path).
	mgr.evictCorruptSession(s.ctx, sessionID, "test_manual")

	s.Require().Equal(0, mgr.GetTreeCount(),
		"eviction must drop the in-memory tree")
	s.Require().Truef(s.miniRedis.Exists(nodesKey),
		"eviction MUST preserve the nodes hash so resume can recover (got missing)")
	s.Require().Truef(s.miniRedis.Exists(liveKey),
		"eviction MUST preserve live_root so resume can recover (got missing)")
}

// TestEvictCorruptSession_ResumePathRecoversIntactState closes the loop:
// after a memory-only eviction on a non-corrupt Redis state, the next
// UpdateTree must resume the tree via resumeTreeFromRedisLocked and
// preserve the pre-eviction relay count, modulo the expected
// interval-1 checkpoint-window loss. Interval=1 isolates the eviction
// behavior from the checkpoint behavior so the count is exact.
func (s *RedisSMSTTestSuite) TestEvictCorruptSession_ResumePathRecoversIntactState() {
	supplier := "pokt1evict_resume"
	sessionID := "session_evict_resume"

	mgr := s.createTestRedisSMSTManagerWithInterval(supplier, 1)
	const preEvictionUpdates = 5
	for i := 0; i < preEvictionUpdates; i++ {
		s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
			[]byte{byte(i)}, []byte{byte(i + 100)}, 10))
	}

	mgr.evictCorruptSession(s.ctx, sessionID, "test_manual")

	// After eviction, an incoming relay must not create an empty tree —
	// GetOrCreateTree should resume from live_root and carry forward
	// the 5 pre-eviction relays, then add the new one.
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
		[]byte{byte(100)}, []byte{byte(200)}, 10))

	_, err := mgr.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	count, sum, err := mgr.GetTreeStats(sessionID)
	s.Require().NoError(err)
	// With interval=1 every update checkpoints, so live_root =
	// R_{preEvictionUpdates} at eviction time. Resume picks up there
	// and adds the post-eviction relay, total = 6.
	s.Require().Equal(uint64(preEvictionUpdates+1), count,
		"memory-only eviction must let the next UpdateTree resume and preserve pre-eviction count")
	s.Require().Equal(uint64((preEvictionUpdates+1)*10), sum)
}
