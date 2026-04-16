//go:build test

package miner

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
)

// TestRedisSMSTManager_UpdateBasic tests basic Update/GetTreeStats/FlushTree operations.
// Ported from poktroll's TestSMST_TrieUpdateBasic.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_UpdateBasic() {
	manager := s.createTestRedisSMSTManager("pokt1test_update_basic")
	sessionID := "session_update_basic"

	// Initial state - no trees
	s.Require().Equal(0, manager.GetTreeCount())

	// Update tree with single relay
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	// Verify tree exists
	s.Require().Equal(1, manager.GetTreeCount())

	// Get stats
	count, sum, err := manager.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(uint64(1), count, "should have 1 relay")
	s.Require().Equal(uint64(100), sum, "sum should be 100")

	// Update with second relay
	err = manager.UpdateTree(s.ctx, sessionID, []byte("key2"), []byte("value2"), 200)
	s.Require().NoError(err)

	count, sum, err = manager.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(uint64(2), count, "should have 2 relays")
	s.Require().Equal(uint64(300), sum, "sum should be 300")

	// Flush tree (finalize root hash)
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().NotNil(rootHash)
	s.Require().NotEmpty(rootHash, "root hash should not be empty")

	// Verify root hash is retrievable
	gotRoot, err := manager.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().Equal(rootHash, gotRoot, "root hash should be consistent")
}

// TestRedisSMSTManager_DeleteAndReinsert tests deleting and re-inserting same key.
// Ported from poktroll's TestSMST_TrieDeleteBasic.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_DeleteAndReinsert() {
	manager := s.createTestRedisSMSTManager("pokt1test_delete_reinsert")
	sessionID := "session_delete_reinsert"

	// Insert key
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	root1, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// NOTE: SMST doesn't support Delete in our implementation yet
	// This test documents expected behavior when Delete is added
	// For now, skip the delete portion

	// Create new session and insert same key (simulates delete + reinsert)
	sessionID2 := "session_delete_reinsert_2"
	err = manager.UpdateTree(s.ctx, sessionID2, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	root2, err := manager.FlushTree(s.ctx, sessionID2)
	s.Require().NoError(err)

	// Roots should match (same key, same value, same weight)
	s.Require().Equal(root1, root2, "re-inserting same key should produce same root")
}

// TestRedisSMSTManager_KnownBitPath tests updates with known bit paths.
// Ported from poktroll's TestSMST_TrieKnownPath.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_KnownBitPath() {
	manager := s.createTestRedisSMSTManager("pokt1test_known_bit_path")
	sessionID := "session_known_bit_path"

	// Use known bit paths to create predictable tree structure
	allZeros := s.generateKnownBitPath("all_zeros")
	allOnes := s.generateKnownBitPath("all_ones")
	alternating := s.generateKnownBitPath("alternating")

	// Update with different path patterns
	err := manager.UpdateTree(s.ctx, sessionID, allZeros, []byte("value_zeros"), 100)
	s.Require().NoError(err)

	err = manager.UpdateTree(s.ctx, sessionID, allOnes, []byte("value_ones"), 200)
	s.Require().NoError(err)

	err = manager.UpdateTree(s.ctx, sessionID, alternating, []byte("value_alt"), 300)
	s.Require().NoError(err)

	// Verify stats
	count, sum, err := manager.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(uint64(3), count)
	s.Require().Equal(uint64(600), sum)

	// Flush and verify root
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().NotEmpty(rootHash)
}

// TestRedisSMSTManager_MaxHeight tests maximum tree depth (256 levels).
// Ported from poktroll's TestSMST_TrieMaxHeightCase.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_MaxHeight() {
	manager := s.createTestRedisSMSTManager("pokt1test_max_height")
	sessionID := "session_max_height"

	// Create two keys that differ only in the last bit
	// This forces the tree to maximum depth (256 levels for SHA-256)
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)

	// All zeros except last bit
	key1[31] = 0x00 // ...00000000
	key2[31] = 0x01 // ...00000001

	// Update both
	err := manager.UpdateTree(s.ctx, sessionID, key1, []byte("value1"), 100)
	s.Require().NoError(err)

	err = manager.UpdateTree(s.ctx, sessionID, key2, []byte("value2"), 200)
	s.Require().NoError(err)

	// Verify stats
	count, sum, err := manager.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(uint64(2), count)
	s.Require().Equal(uint64(300), sum)

	// Flush should succeed even at max depth
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().NotEmpty(rootHash)
}

// TestRedisSMSTManager_OrphanCleanup tests node cleanup after operations.
// Ported from poktroll's TestSMST_OrphanRemoval.
// Note: This is more of a verification that operations don't leak memory/Redis keys.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_OrphanCleanup() {
	supplierAddr := "pokt1test_orphan_cleanup"
	manager := s.createTestRedisSMSTManager(supplierAddr)
	sessionID := "session_orphan_cleanup"

	// Insert multiple keys
	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("key_%d", i))
		value := []byte(fmt.Sprintf("value_%d", i))
		err := manager.UpdateTree(s.ctx, sessionID, key, value, uint64((i+1)*100))
		s.Require().NoError(err)
	}

	// Flush tree
	_, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Delete tree (cleanup)
	err = manager.DeleteTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Verify tree is gone from memory
	s.Require().Equal(0, manager.GetTreeCount())

	// Verify Redis hash is deleted
	hashKey := s.redisClient.KB().SMSTNodesKey(supplierAddr, sessionID)
	exists, err := s.redisClient.Exists(s.ctx, hashKey).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(0), exists, "Redis hash should be deleted")
}

// TestRedisSMSTManager_SumAggregation tests sum/count aggregation across relays.
// Ported from poktroll's TestSMST_TotalSum.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_SumAggregation() {
	manager := s.createTestRedisSMSTManager("pokt1test_sum_aggregation")
	sessionID := "session_sum_aggregation"

	// Add relays with increasing weights
	relays := s.generateTestRelays(20, 42) // 20 relays with seed 42

	totalSum := uint64(0)
	for _, relay := range relays {
		err := manager.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
		totalSum += relay.weight
	}

	// Verify stats
	count, sum, err := manager.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(uint64(20), count, "should have 20 relays")
	s.Require().Equal(totalSum, sum, "sum should match total")

	// Flush
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().NotEmpty(rootHash)
}

// TestRedisSMSTManager_ValueRetrieval tests retrieving values after commit.
// Ported from poktroll's TestSMST_Retrieval.
// Note: Our SMST implementation doesn't expose Get after flush, but we can verify via stats.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_ValueRetrieval() {
	manager := s.createTestRedisSMSTManager("pokt1test_value_retrieval")
	sessionID := "session_value_retrieval"

	// Add relays
	relays := s.generateTestRelays(5, 99)
	for _, relay := range relays {
		err := manager.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}

	// Verify stats before flush
	countBefore, sumBefore, err := manager.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(uint64(5), countBefore)

	// Flush
	_, err = manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Verify stats after flush (should be same)
	countAfter, sumAfter, err := manager.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(countBefore, countAfter, "count should not change after flush")
	s.Require().Equal(sumBefore, sumAfter, "sum should not change after flush")
}

// Redis-specific tests below

// TestRedisSMSTManager_MultipleSessions tests concurrent session isolation.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_MultipleSessions() {
	manager := s.createTestRedisSMSTManager("pokt1test_multiple_sessions")

	// Create 5 different sessions
	sessions := []string{"session_1", "session_2", "session_3", "session_4", "session_5"}

	// Add different data to each session
	for i, sessionID := range sessions {
		relays := s.generateTestRelays(10, byte(i)) // Different seed per session
		for _, relay := range relays {
			err := manager.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
			s.Require().NoError(err)
		}
	}

	// Verify tree count
	s.Require().Equal(5, manager.GetTreeCount(), "should have 5 separate trees")

	// Flush all sessions and verify different root hashes
	roots := make(map[string][]byte)
	for _, sessionID := range sessions {
		root, err := manager.FlushTree(s.ctx, sessionID)
		s.Require().NoError(err)
		roots[sessionID] = root
	}

	// Verify all roots are different (different data per session)
	for i, sessionID1 := range sessions {
		for j, sessionID2 := range sessions {
			if i != j {
				s.Require().NotEqual(roots[sessionID1], roots[sessionID2],
					"different sessions should have different roots")
			}
		}
	}
}

// TestRedisSMSTManager_ClaimedTreeImmutable tests that no updates are allowed after FlushTree().
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_ClaimedTreeImmutable() {
	manager := s.createTestRedisSMSTManager("pokt1test_claimed_immutable")
	sessionID := "session_claimed_immutable"

	// Add relay
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	// Flush tree (marks as claimed)
	_, err = manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Attempt to update after flush (should fail)
	err = manager.UpdateTree(s.ctx, sessionID, []byte("key2"), []byte("value2"), 200)
	s.Require().Error(err, "should not allow updates after flush")
	// Error message can be either "already been claimed" or "is sealing for claim"
	errMsg := err.Error()
	s.Require().True(
		strings.Contains(errMsg, "already been claimed") || strings.Contains(errMsg, "is sealing"),
		"error should mention claim/sealing status, got: %s", errMsg,
	)
}

// CRITICAL TEST: Root Hash Equivalence
// TestRedisSMSTManager_RootHashEquivalence verifies that Redis-backed SMST
// produces identical root hashes as in-memory SMST for the same operations.
//
// This is the MOST IMPORTANT test - if this fails, our Redis implementation is broken.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_RootHashEquivalence() {
	manager := s.createTestRedisSMSTManager("pokt1test_root_hash_equivalence")
	sessionID := "session_root_hash_equivalence"

	// Create in-memory SMST for comparison
	inMemorySMST := s.createInMemorySMST()

	// Generate test relays
	relays := s.generateTestRelays(50, 77) // 50 relays with seed 77

	// Apply identical operations to both trees
	for _, relay := range relays {
		// In-memory SMST
		err := inMemorySMST.Update(relay.key, relay.value, relay.weight)
		s.Require().NoError(err, "in-memory Update should not error")
		err = inMemorySMST.Commit()
		s.Require().NoError(err, "in-memory Commit should not error")

		// Redis-backed SMST
		err = manager.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err, "Redis UpdateTree should not error")
	}

	// Get root hashes
	inMemoryRoot := inMemorySMST.Root()
	redisRoot, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// CRITICAL ASSERTION: Root hashes MUST match
	s.assertRootHashEqual(inMemoryRoot, redisRoot,
		"Redis-backed SMST root hash MUST match in-memory SMST root hash for identical operations")

	// Also verify via GetTreeRoot
	gotRedisRoot, err := manager.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err)
	s.assertRootHashEqual(inMemoryRoot, gotRedisRoot,
		"GetTreeRoot should return same root as FlushTree")
}

// TestRedisSMSTManager_WarmupFromRedis tests restoring state after restart/failover.
// This is critical for HA - a new instance must be able to resume from Redis.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_WarmupFromRedis() {
	manager1 := s.createTestRedisSMSTManager("pokt1test_warmup")
	sessionID := "session_warmup"

	// Manager 1: Create and populate tree
	relays := s.generateTestRelays(10, 88)
	for _, relay := range relays {
		err := manager1.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}

	// Flush to get root hash
	root1, err := manager1.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Create second manager instance (simulates restart/failover)
	manager2 := s.createTestRedisSMSTManager("pokt1test_warmup")

	// Warmup from Redis (loads existing trees)
	loadedCount, err := manager2.WarmupFromRedis(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(1, loadedCount, "should load 1 tree from Redis")

	// Verify tree exists in manager2
	s.Require().Equal(1, manager2.GetTreeCount())

	// Get root from manager2 (should match manager1)
	root2, err := manager2.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err)
	s.assertRootHashEqual(root1, root2, "warmed-up tree should have same root hash")

	// Verify stats match
	count1, sum1, err := manager1.GetTreeStats(sessionID)
	s.Require().NoError(err)

	count2, sum2, err := manager2.GetTreeStats(sessionID)
	s.Require().NoError(err)

	s.Require().Equal(count1, count2, "count should match after warmup")
	s.Require().Equal(sum1, sum2, "sum should match after warmup")
}

// TestRedisSMSTManager_WarmupMultipleTrees tests warming up multiple sessions.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_WarmupMultipleTrees() {
	manager1 := s.createTestRedisSMSTManager("pokt1test_warmup_multiple")

	// Create 10 sessions
	sessionIDs := make([]string, 10)
	roots := make(map[string][]byte)

	for i := 0; i < 10; i++ {
		sessionID := fmt.Sprintf("session_warmup_%d", i)
		sessionIDs[i] = sessionID

		// Add data to each session
		relays := s.generateTestRelays(5, byte(i))
		for _, relay := range relays {
			err := manager1.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
			s.Require().NoError(err)
		}

		// Flush
		root, err := manager1.FlushTree(s.ctx, sessionID)
		s.Require().NoError(err)
		roots[sessionID] = root
	}

	// Create second manager and warmup
	manager2 := s.createTestRedisSMSTManager("pokt1test_warmup_multiple")

	loadedCount, err := manager2.WarmupFromRedis(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(10, loadedCount, "should load all 10 trees")

	// Verify all roots match
	for _, sessionID := range sessionIDs {
		root2, err := manager2.GetTreeRoot(s.ctx, sessionID)
		s.Require().NoError(err)
		s.assertRootHashEqual(roots[sessionID], root2,
			fmt.Sprintf("warmed-up tree %s should have matching root", sessionID))
	}
}

// TestRedisSMSTManager_DeleteTree tests deleting from memory and Redis.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_DeleteTree() {
	supplierAddr := "pokt1test_delete_tree"
	manager := s.createTestRedisSMSTManager(supplierAddr)
	sessionID := "session_delete_tree"

	// Create tree
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	// Verify exists
	s.Require().Equal(1, manager.GetTreeCount())

	hashKey := s.redisClient.KB().SMSTNodesKey(supplierAddr, sessionID)
	exists, err := s.redisClient.Exists(s.ctx, hashKey).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(1), exists, "Redis hash should exist")

	// Delete tree
	err = manager.DeleteTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Verify gone from memory
	s.Require().Equal(0, manager.GetTreeCount())

	// Verify gone from Redis
	exists, err = s.redisClient.Exists(s.ctx, hashKey).Result()
	s.Require().NoError(err)
	s.Require().Equal(int64(0), exists, "Redis hash should be deleted")
}

// TestRedisSMSTManager_SetTreeTTL tests TTL expiration logic.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_SetTreeTTL() {
	supplierAddr := "pokt1test_set_ttl"
	manager := s.createTestRedisSMSTManager(supplierAddr)
	sessionID := "session_set_ttl"

	// Create tree
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	// Set TTL (use miniredis which supports TTL commands)
	ttl := 60 * time.Second // 60 seconds (won't actually expire in test, just verify command works)
	err = manager.SetTreeTTL(s.ctx, sessionID, ttl)
	s.Require().NoError(err)

	// Verify TTL was set in Redis
	hashKey := s.redisClient.KB().SMSTNodesKey(supplierAddr, sessionID)
	ttlResult, err := s.redisClient.TTL(s.ctx, hashKey).Result()
	s.Require().NoError(err)
	s.Require().Greater(ttlResult.Seconds(), float64(0), "TTL should be set")
}

// TestRedisSMSTManager_GetTreeCount tests tracking active trees.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_GetTreeCount() {
	manager := s.createTestRedisSMSTManager("pokt1test_tree_count")

	// Initially 0
	s.Require().Equal(0, manager.GetTreeCount())

	// Create 3 trees
	for i := 0; i < 3; i++ {
		sessionID := fmt.Sprintf("session_%d", i)
		err := manager.UpdateTree(s.ctx, sessionID, []byte("key"), []byte("value"), 100)
		s.Require().NoError(err)
	}

	s.Require().Equal(3, manager.GetTreeCount())

	// Delete one
	err := manager.DeleteTree(s.ctx, "session_1")
	s.Require().NoError(err)

	s.Require().Equal(2, manager.GetTreeCount())
}

// ============================================================================
// SEALING MECHANISM TESTS - Verifying Two-Phase Seal Race Condition Prevention
// ============================================================================

// TestRedisSMSTManager_Sealing_ConcurrentUpdateDuringFlush tests that UpdateTree
// is properly rejected when called concurrently with FlushTree.
// This verifies the sealing flag blocks new updates during the flush process.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_Sealing_ConcurrentUpdateDuringFlush() {
	manager := s.createTestRedisSMSTManager("pokt1test_sealing_concurrent")
	sessionID := "session_sealing_concurrent"

	// Add initial relay
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	// Launch concurrent operations
	var wg sync.WaitGroup
	wg.Add(2)

	// Channel to signal when FlushTree starts sealing
	flushStarted := make(chan struct{})
	updateDuringFlush := make(chan error, 1)

	// Goroutine 1: FlushTree (will set sealing=true)
	go func() {
		defer wg.Done()
		close(flushStarted) // Signal that flush is about to start
		_, err := manager.FlushTree(s.ctx, sessionID)
		s.Require().NoError(err, "FlushTree should succeed")
	}()

	// Goroutine 2: UpdateTree (should be rejected during flush)
	go func() {
		defer wg.Done()
		<-flushStarted // Wait for flush to start
		// Small delay to ensure we hit the sealing window
		time.Sleep(5 * time.Millisecond)
		err := manager.UpdateTree(s.ctx, sessionID, []byte("key2"), []byte("value2"), 200)
		updateDuringFlush <- err
	}()

	wg.Wait()

	// Verify that UpdateTree was rejected
	updateErr := <-updateDuringFlush
	s.Require().Error(updateErr, "UpdateTree during FlushTree should be rejected")
	errMsg := updateErr.Error()
	s.Require().True(
		strings.Contains(errMsg, "is sealing") || strings.Contains(errMsg, "already been claimed"),
		"error should mention sealing or claimed status, got: %s", errMsg,
	)
}

// TestRedisSMSTManager_Sealing_MultipleFlushCalls tests that multiple concurrent
// FlushTree calls on the same session don't cause race conditions.
// After the first flush, subsequent calls should use the cached claimedRoot.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_Sealing_MultipleFlushCalls() {
	manager := s.createTestRedisSMSTManager("pokt1test_sealing_multiple_flush")
	sessionID := "session_multiple_flush"

	// Add relay
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	// Concurrent FlushTree calls
	numFlushes := 5
	roots := make([][]byte, numFlushes)
	errors := make([]error, numFlushes)

	var wg sync.WaitGroup
	wg.Add(numFlushes)

	for i := 0; i < numFlushes; i++ {
		idx := i
		go func() {
			defer wg.Done()
			root, err := manager.FlushTree(s.ctx, sessionID)
			roots[idx] = root
			errors[idx] = err
		}()
	}

	wg.Wait()

	// All flushes should succeed
	for i := 0; i < numFlushes; i++ {
		s.Require().NoError(errors[i], "FlushTree call %d should succeed", i)
		s.Require().NotNil(roots[i], "FlushTree call %d should return root", i)
	}

	// All roots should be identical (same tree, same root)
	for i := 1; i < numFlushes; i++ {
		s.Require().Equal(roots[0], roots[i], "all FlushTree calls should return same root")
	}
}

// TestRedisSMSTManager_Sealing_UpdateAfterFlush verifies that updates are
// rejected after FlushTree completes (sealing + claimed state).
// This is a more comprehensive version of TestRedisSMSTManager_ClaimedTreeImmutable.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_Sealing_UpdateAfterFlush() {
	manager := s.createTestRedisSMSTManager("pokt1test_sealing_after_flush")
	sessionID := "session_after_flush"

	// Add initial relay
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	// Flush tree (marks as claimed)
	rootBefore, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().NotNil(rootBefore)

	// Attempt multiple updates after flush (all should fail)
	for i := 0; i < 5; i++ {
		err = manager.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("key%d", i+2)),
			[]byte(fmt.Sprintf("value%d", i+2)),
			200+uint64(i)*100)
		s.Require().Error(err, "update %d should be rejected", i)

		errMsg := err.Error()
		s.Require().True(
			strings.Contains(errMsg, "already been claimed") || strings.Contains(errMsg, "is sealing"),
			"error should mention claim/sealing status, got: %s", errMsg,
		)
	}

	// Verify root hasn't changed
	rootAfter, err := manager.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().Equal(rootBefore, rootAfter, "root should be unchanged after rejected updates")

	// Verify count/sum haven't changed
	count, sum, err := manager.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(uint64(1), count, "count should still be 1")
	s.Require().Equal(uint64(100), sum, "sum should still be 100")
}

// TestRedisSMSTManager_Sealing_ProofGenerationSafety tests that ProveClosest
// validates root integrity and detects if the tree was somehow modified after sealing.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_Sealing_ProofGenerationSafety() {
	manager := s.createTestRedisSMSTManager("pokt1test_sealing_proof_safety")
	sessionID := "session_proof_safety"

	// Add relay
	key := []byte("relay_key_1")
	err := manager.UpdateTree(s.ctx, sessionID, key, []byte("value1"), 100)
	s.Require().NoError(err)

	// Flush tree
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof (should succeed - tree is sealed and root is stable)
	path := protocol.GetPathForProof(key, sessionID)
	proofBytes, err := manager.ProveClosest(s.ctx, sessionID, path)
	s.Require().NoError(err)
	s.Require().NotNil(proofBytes)

	// Verify proof is valid
	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)

	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	valid, err := smt.VerifyClosestProof(proof, rootHash, protocol.NewSMTSpec())
	s.Require().NoError(err)
	s.Require().True(valid, "proof should be valid")

	// Attempt to generate another proof (should still work with cached root)
	proofBytes2, err := manager.ProveClosest(s.ctx, sessionID, path)
	s.Require().NoError(err)
	s.Require().Equal(proofBytes, proofBytes2, "repeated proof generation should be idempotent")
}

// TestRedisSMSTManager_Sealing_ProofBeforeFlush tests that ProveClosest
// correctly rejects proof generation before FlushTree is called.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_Sealing_ProofBeforeFlush() {
	manager := s.createTestRedisSMSTManager("pokt1test_sealing_proof_before_flush")
	sessionID := "session_proof_before_flush"

	// Add relay but don't flush
	key := []byte("relay_key_1")
	err := manager.UpdateTree(s.ctx, sessionID, key, []byte("value1"), 100)
	s.Require().NoError(err)

	// Attempt proof generation before flush (should fail)
	path := protocol.GetPathForProof(key, sessionID)
	proofBytes, err := manager.ProveClosest(s.ctx, sessionID, path)
	s.Require().Error(err, "ProveClosest before FlushTree should fail")
	s.Require().Nil(proofBytes)
	s.Require().Contains(err.Error(), "has not been claimed yet")

	// Now flush
	_, err = manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Proof generation should now succeed
	proofBytes, err = manager.ProveClosest(s.ctx, sessionID, path)
	s.Require().NoError(err)
	s.Require().NotNil(proofBytes)
}

// TestRedisSMSTManager_Sealing_HighConcurrency is a stress test that verifies
// the sealing mechanism under high concurrent load.
// This tests the production scenario where multiple relays arrive while FlushTree is called.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_Sealing_HighConcurrency() {
	manager := s.createTestRedisSMSTManager("pokt1test_sealing_high_concurrency")
	sessionID := "session_high_concurrency"

	// Add initial relay
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key0"), []byte("value0"), 100)
	s.Require().NoError(err)

	// Launch many concurrent UpdateTree operations
	numUpdates := 50
	var wg sync.WaitGroup
	wg.Add(numUpdates + 1) // +1 for FlushTree

	updateErrors := make([]error, numUpdates)
	flushComplete := make(chan struct{})

	// Goroutine for FlushTree (starts after small delay to let some updates queue)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond) // Let some updates start
		_, err := manager.FlushTree(s.ctx, sessionID)
		s.Require().NoError(err)
		close(flushComplete)
	}()

	// Goroutines for concurrent updates
	for i := 0; i < numUpdates; i++ {
		idx := i
		go func() {
			defer wg.Done()
			// Stagger updates over time
			time.Sleep(time.Duration(idx) * time.Millisecond)
			err := manager.UpdateTree(s.ctx, sessionID,
				[]byte(fmt.Sprintf("key%d", idx+1)),
				[]byte(fmt.Sprintf("value%d", idx+1)),
				uint64((idx+1)*100))
			updateErrors[idx] = err
		}()
	}

	wg.Wait()
	<-flushComplete

	// Count successful vs rejected updates
	successCount := 0
	rejectedCount := 0
	for _, err := range updateErrors {
		if err == nil {
			successCount++
		} else {
			rejectedCount++
			// All rejections should be due to sealing/claimed status
			errMsg := err.Error()
			s.Require().True(
				strings.Contains(errMsg, "is sealing") || strings.Contains(errMsg, "already been claimed"),
				"rejected update should have sealing/claimed error, got: %s", errMsg,
			)
		}
	}

	// Some updates should have succeeded (before flush), others rejected (after flush started)
	s.Require().Greater(successCount, 0, "some updates should have succeeded before flush")
	s.Require().Greater(rejectedCount, 0, "some updates should have been rejected during/after flush")

	// Verify tree is sealed (no more updates allowed)
	err = manager.UpdateTree(s.ctx, sessionID, []byte("late_key"), []byte("late_value"), 9999)
	s.Require().Error(err)
	s.Require().True(
		strings.Contains(err.Error(), "already been claimed") || strings.Contains(err.Error(), "is sealing"),
	)
}

// TestRedisSMSTManager_Sealing_FlushStability tests the core guarantee of the
// two-phase seal: that the root hash captured during FlushTree is stable and
// won't change due to in-flight updates.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_Sealing_FlushStability() {
	manager := s.createTestRedisSMSTManager("pokt1test_sealing_stability")
	sessionID := "session_stability"

	// Add multiple relays sequentially
	numRelays := 10
	for i := 0; i < numRelays; i++ {
		err := manager.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("key%d", i)),
			[]byte(fmt.Sprintf("value%d", i)),
			uint64((i+1)*100))
		s.Require().NoError(err)
	}

	// Flush multiple times and verify root is stable
	roots := make([][]byte, 3)
	for i := 0; i < 3; i++ {
		root, err := manager.FlushTree(s.ctx, sessionID)
		s.Require().NoError(err)
		roots[i] = root
	}

	// All flushes should return the same root (tree is sealed after first flush)
	for i := 1; i < 3; i++ {
		s.Require().Equal(roots[0], roots[i], "flush %d should return same root", i)
	}

	// Verify stats are also stable
	for i := 0; i < 3; i++ {
		count, sum, err := manager.GetTreeStats(sessionID)
		s.Require().NoError(err)
		s.Require().Equal(uint64(numRelays), count, "count should be stable")
		s.Require().Equal(uint64((numRelays*(numRelays+1)/2)*100), sum, "sum should be stable")
	}
}

// TestRedisSMSTManager_ProveClosest_LazyLoadAfterFailover simulates the HA
// failover scenario: original leader flushes the tree and crashes, new leader
// takes over and must generate a proof. With lazy-loading in ProveClosest,
// the new leader reconstructs the tree from Redis on-demand.
//
// This reproduces the bug found during chaos testing where proofs failed with
// "session X not found" because the new leader did not call WarmupFromRedis.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_ProveClosest_LazyLoadAfterFailover() {
	supplierAddr := "pokt1test_ha_failover"
	sessionID := "session_ha_failover"

	// Leader 1: populate tree and flush (simulates claim submission)
	leader1 := s.createTestRedisSMSTManager(supplierAddr)
	relays := s.generateTestRelays(20, 42)
	for _, relay := range relays {
		err := leader1.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}
	root1, err := leader1.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().NotEmpty(root1, "flush should produce a root hash")

	// Simulate crash: leader1 disappears. Create a fresh manager (leader2)
	// WITHOUT calling WarmupFromRedis — this mimics the real production code
	// path where warmup is not invoked on leader change.
	leader2 := s.createTestRedisSMSTManager(supplierAddr)
	s.Require().Equal(0, leader2.GetTreeCount(), "leader2 starts empty")

	// Leader2 tries to generate a proof for a session it never saw.
	// Before fix: fails with "session not found".
	// After fix: lazy-loads from Redis and succeeds.
	proofPath := s.generateKnownBitPath("all_zeros") // 32-byte path
	proof, err := leader2.ProveClosest(s.ctx, sessionID, proofPath)
	s.Require().NoError(err, "ProveClosest should lazy-load tree from Redis")
	s.Require().NotEmpty(proof, "proof should be generated")

	// After lazy-load, tree is now in leader2's memory
	s.Require().Equal(1, leader2.GetTreeCount(), "tree should be loaded into memory")

	// Verify the root hash matches what leader1 had
	root2, err := leader2.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err)
	s.assertRootHashEqual(root1, root2, "lazy-loaded tree should have same root as original")
}

// TestRedisSMSTManager_ProveClosest_MissingInRedis verifies that lazy-load
// returns a clear error when the session was never flushed (no claimed root
// in Redis) — don't silently succeed with a bogus tree.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_ProveClosest_MissingInRedis() {
	manager := s.createTestRedisSMSTManager("pokt1test_missing")

	// Try to prove a session that doesn't exist anywhere
	_, err := manager.ProveClosest(s.ctx, "nonexistent_session", []byte("any_key"))
	s.Require().Error(err, "should fail when session not in memory or Redis")
	s.Require().Contains(err.Error(), "not found", "error should clearly indicate missing session")
}

// TestRedisSMSTManager_GetTreeRoot_LazyLoadAfterFailover verifies that
// GetTreeRoot also lazy-loads from Redis for the same HA failover scenario.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_GetTreeRoot_LazyLoadAfterFailover() {
	supplierAddr := "pokt1test_gettreeroot_ha"
	sessionID := "session_gettreeroot_ha"

	// Leader 1: populate and flush
	leader1 := s.createTestRedisSMSTManager(supplierAddr)
	relays := s.generateTestRelays(15, 77)
	for _, relay := range relays {
		err := leader1.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}
	root1, err := leader1.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Leader 2: fresh instance, no warmup
	leader2 := s.createTestRedisSMSTManager(supplierAddr)

	// GetTreeRoot should lazy-load from Redis
	root2, err := leader2.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err, "GetTreeRoot should lazy-load on HA failover")
	s.assertRootHashEqual(root1, root2, "lazy-loaded root should match")

	// Tree should now be in memory
	s.Require().Equal(1, leader2.GetTreeCount())
}

// TestRedisSMSTManager_LazyLoad_ConcurrentSafe verifies that concurrent
// lazy-load calls from multiple goroutines don't create duplicate trees or
// race. This matters because multiple proofs may be submitted in parallel
// via the transitionSubpool.
func (s *RedisSMSTTestSuite) TestRedisSMSTManager_LazyLoad_ConcurrentSafe() {
	supplierAddr := "pokt1test_concurrent_load"
	sessionID := "session_concurrent_load"

	// Populate and flush with leader1
	leader1 := s.createTestRedisSMSTManager(supplierAddr)
	relays := s.generateTestRelays(10, 55)
	for _, relay := range relays {
		err := leader1.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}
	root1, err := leader1.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Fresh leader2, no warmup
	leader2 := s.createTestRedisSMSTManager(supplierAddr)

	// Spawn N concurrent ProveClosest calls for the same session
	const numGoroutines = 20
	type result struct {
		proof []byte
		err   error
	}
	results := make(chan result, numGoroutines)

	proofPath := s.generateKnownBitPath("alternating") // 32-byte path
	for i := 0; i < numGoroutines; i++ {
		go func() {
			proof, err := leader2.ProveClosest(s.ctx, sessionID, proofPath)
			results <- result{proof: proof, err: err}
		}()
	}

	// Collect all results
	for i := 0; i < numGoroutines; i++ {
		r := <-results
		s.Require().NoError(r.err, "concurrent ProveClosest should all succeed")
		s.Require().NotEmpty(r.proof)
	}

	// Exactly ONE tree in memory despite N concurrent loads
	s.Require().Equal(1, leader2.GetTreeCount(), "concurrent lazy-loads must not create duplicate trees")

	// Root still matches leader1's
	root2, err := leader2.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err)
	s.assertRootHashEqual(root1, root2)
}
