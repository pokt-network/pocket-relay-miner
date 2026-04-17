//go:build test

package miner

// Tests for the live_root checkpoint mechanism: the Redis key written on
// every Nth UpdateTree that lets an HA follower resume a mid-session SMST
// after the previous leader dies without waiting for a flush.
//
// Regression context: before these tests, a mid-session leader kill lost
// ~50% of the relays the dead leader had processed (the survivor's
// GetOrCreateTree returned an empty tree even though Redis had the
// committed nodes) - see scripts/test-quantitative-failover.sh
// KILL_TARGET=leader run on 2026-04-16.
//
// All tests use the existing RedisSMSTTestSuite harness so they share a
// single miniredis instance and run sequentially.

import (
	"fmt"

	"github.com/rs/zerolog"
)

// TestLiveRoot_FirstUpdateCheckpoints verifies that the VERY FIRST
// UpdateTree call always writes live_root, even though the interval is
// larger than 1. Without this, low-traffic sessions (fewer relays than
// the interval) could lose the entire session on a mid-session kill.
func (s *RedisSMSTTestSuite) TestLiveRoot_FirstUpdateCheckpoints() {
	supplier := "pokt1live_first_update"
	sessionID := "session_live_first"

	mgr := s.createTestRedisSMSTManager(supplier)

	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID, []byte("k1"), []byte("v1"), 10))

	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)
	liveRoot, err := s.redisClient.Get(s.ctx, liveKey).Bytes()
	s.Require().NoError(err, "live_root MUST exist after the first update")
	s.Require().NotEmpty(liveRoot, "live_root bytes must not be empty")
}

// TestLiveRoot_CheckpointsAtInterval verifies the batching optimisation:
// between the first update and the next interval boundary, live_root is
// NOT re-written on every update, but IS re-written when the interval is
// reached.
func (s *RedisSMSTTestSuite) TestLiveRoot_CheckpointsAtInterval() {
	supplier := "pokt1live_interval"
	sessionID := "session_live_interval"
	const interval = 5

	mgr := s.createTestRedisSMSTManagerWithInterval(supplier, interval)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)

	// Update #1 - first update always checkpoints.
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID, []byte("k1"), []byte("v1"), 10))
	checkpointAfter1, _ := s.redisClient.Get(s.ctx, liveKey).Bytes()
	s.Require().NotEmpty(checkpointAfter1, "live_root must be set after update 1")

	// Updates #2, #3, #4 - no checkpoint, live_root must be byte-equal
	// to the one written at update 1 (still reflects the first relay's
	// root, not any subsequent updates).
	for i := 2; i <= 4; i++ {
		s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("k%d", i)), []byte(fmt.Sprintf("v%d", i)), uint64(10*i)))
	}
	checkpointAfter4, _ := s.redisClient.Get(s.ctx, liveKey).Bytes()
	s.Require().Equal(checkpointAfter1, checkpointAfter4,
		"live_root must NOT be re-written between interval boundaries")

	// Update #5 - interval boundary, checkpoint must refresh.
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID, []byte("k5"), []byte("v5"), 50))
	checkpointAfter5, _ := s.redisClient.Get(s.ctx, liveKey).Bytes()
	s.Require().NotEqual(checkpointAfter1, checkpointAfter5,
		"live_root must be refreshed at interval boundary (update 5)")
}

// TestLiveRoot_MidSessionResumePreservesTree is the crown-jewel regression
// test: it simulates a leader dying mid-session after N updates, the
// follower taking over (new manager instance, empty in-memory map,
// backed by the same Redis), adding more updates, flushing, and verifying
// that the resulting claim reflects the TOTAL relay count across both
// leaders' contributions.
//
// Before the live_root fix, the follower's flushed tree only reflected
// its own updates (count M), silently dropping the dead leader's N relays
// that were already committed to the nodes hash.
func (s *RedisSMSTTestSuite) TestLiveRoot_MidSessionResumePreservesTree() {
	supplier := "pokt1live_resume"
	sessionID := "session_live_resume"
	const interval = 10
	const leaderUpdates = 20 // at interval boundary - no relays lost
	const followerUpdates = 15

	// Phase 1: "Leader" processes 20 relays. With interval=10, live_root
	// is checkpointed at updates 1, 10, and 20.
	leaderMgr := s.createTestRedisSMSTManagerWithInterval(supplier, interval)
	for i := 1; i <= leaderUpdates; i++ {
		s.Require().NoError(leaderMgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("leader_k%d", i)),
			[]byte(fmt.Sprintf("leader_v%d", i)),
			uint64(10)))
	}

	// Phase 2: leader "dies" - we drop its in-memory tree. Redis state is
	// unchanged (nodes hash + live_root at update 20).
	leaderMgr.treesMu.Lock()
	delete(leaderMgr.trees, sessionID)
	leaderMgr.treesMu.Unlock()

	// Phase 3: "Follower" takes over with a brand-new manager instance
	// (no shared in-memory state, only Redis). It adds 15 more updates.
	// The follower's GetOrCreateTree MUST resume from live_root - if it
	// creates a fresh empty tree, the leader's 20 relays are orphaned.
	followerMgr := s.createTestRedisSMSTManagerWithInterval(supplier, interval)
	for i := 1; i <= followerUpdates; i++ {
		s.Require().NoError(followerMgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("follower_k%d", i)),
			[]byte(fmt.Sprintf("follower_v%d", i)),
			uint64(10)))
	}

	// Phase 4: flush and verify the claimed tree has the FULL count.
	rootHash, err := followerMgr.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().NotEmpty(rootHash)

	count, sum, err := followerMgr.GetTreeStats(sessionID)
	s.Require().NoError(err)

	expected := uint64(leaderUpdates + followerUpdates)
	s.Require().Equalf(expected, count,
		"follower's flushed tree must preserve leader's %d + follower's %d = %d relays "+
			"(without live_root resume, count would be only %d)",
		leaderUpdates, followerUpdates, expected, followerUpdates)
	s.Require().Equalf(expected*10, sum, "sum must match %d * weight=10", expected)
}

// TestLiveRoot_LossBoundedByInterval verifies the worst-case guarantee:
// if the leader dies BETWEEN checkpoint boundaries, the follower's resume
// will be missing at most (interval - 1) relays. Losing exactly
// (interval - 1) is the expected trade-off for reducing write amplification.
func (s *RedisSMSTTestSuite) TestLiveRoot_LossBoundedByInterval() {
	supplier := "pokt1live_loss_bound"
	sessionID := "session_live_loss"
	const interval = 10
	const leaderUpdates = 19 // one before the next checkpoint (at 20)

	leaderMgr := s.createTestRedisSMSTManagerWithInterval(supplier, interval)
	for i := 1; i <= leaderUpdates; i++ {
		s.Require().NoError(leaderMgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("k%d", i)),
			[]byte(fmt.Sprintf("v%d", i)),
			uint64(10)))
	}

	// Drop in-memory state, simulate leader kill between checkpoints.
	leaderMgr.treesMu.Lock()
	delete(leaderMgr.trees, sessionID)
	leaderMgr.treesMu.Unlock()

	// Resume from Redis; flush without adding anything new.
	followerMgr := s.createTestRedisSMSTManagerWithInterval(supplier, interval)
	_, err := followerMgr.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	count, _, err := followerMgr.GetTreeStats(sessionID)
	s.Require().NoError(err)

	// Last checkpoint was at update 10. Updates 11-19 are "in-flight"
	// from Redis's perspective (nodes committed, root not checkpointed).
	// Expected count after resume is 10; loss = 9 = interval - 1.
	s.Require().Equalf(uint64(10), count,
		"follower must resume at last checkpoint (10), not latest update (19). "+
			"Loss bound = interval-1 = %d relays", interval-1)

	loss := leaderUpdates - int(count)
	s.Require().LessOrEqualf(loss, interval-1,
		"loss %d must not exceed interval-1 = %d", loss, interval-1)
}

// TestLiveRoot_ClaimedRootTakesPriority verifies the resume order: if
// BOTH a claimed_root (post-flush, sealed) and a live_root exist for a
// session, the follower must resume from claimed_root. Using live_root
// after a flush would allow stray UpdateTree calls on a tree that was
// supposed to be sealed.
func (s *RedisSMSTTestSuite) TestLiveRoot_ClaimedRootTakesPriority() {
	supplier := "pokt1live_vs_claimed"
	sessionID := "session_live_vs_claimed"

	mgr := s.createTestRedisSMSTManager(supplier)

	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID, []byte("k1"), []byte("v1"), 100))
	claimedRoot, err := mgr.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// At this point Redis may still have a live_root from the update
	// (FlushTree does not delete it — claimed_root supersedes). Simulate
	// failover: drop memory, resume on a new manager.
	mgr.treesMu.Lock()
	delete(mgr.trees, sessionID)
	mgr.treesMu.Unlock()

	resumeMgr := s.createTestRedisSMSTManager(supplier)
	gotRoot, err := resumeMgr.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err, "resume must succeed via claimed_root")
	s.Require().Equal(claimedRoot, gotRoot,
		"resume must pick claimed_root, not live_root - the tree is sealed")

	// And a late UpdateTree must be rejected as already claimed.
	err = resumeMgr.UpdateTree(s.ctx, sessionID, []byte("late"), []byte("v"), 10)
	s.Require().ErrorIs(err, ErrSessionClaimed,
		"late UpdateTree on a sealed tree must return ErrSessionClaimed, "+
			"confirming the tree was imported from claimed_root")
}

// TestLiveRoot_DeleteTreeCleansUp verifies that DeleteTree removes the
// live_root key along with the others, so cleanup after settlement leaves
// no stray keys in Redis.
func (s *RedisSMSTTestSuite) TestLiveRoot_DeleteTreeCleansUp() {
	supplier := "pokt1live_delete"
	sessionID := "session_live_delete"

	mgr := s.createTestRedisSMSTManager(supplier)

	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID, []byte("k"), []byte("v"), 10))
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)

	exists, _ := s.redisClient.Exists(s.ctx, liveKey).Result()
	s.Require().Equal(int64(1), exists, "live_root must exist before DeleteTree")

	s.Require().NoError(mgr.DeleteTree(s.ctx, sessionID))

	exists, _ = s.redisClient.Exists(s.ctx, liveKey).Result()
	s.Require().Equal(int64(0), exists, "live_root must be cleaned up by DeleteTree")
}

// TestLiveRoot_CustomIntervalRespected verifies that the config's
// LiveRootCheckpointInterval overrides the default. interval=1 should
// produce a fresh live_root on every update (zero-loss mode).
func (s *RedisSMSTTestSuite) TestLiveRoot_CustomIntervalRespected() {
	supplier := "pokt1live_custom"
	sessionID := "session_live_custom"

	mgr := s.createTestRedisSMSTManagerWithInterval(supplier, 1)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)

	// Every update must change live_root when interval=1.
	var prev []byte
	for i := 1; i <= 5; i++ {
		s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("k%d", i)),
			[]byte(fmt.Sprintf("v%d", i)),
			uint64(10)))
		cur, _ := s.redisClient.Get(s.ctx, liveKey).Bytes()
		s.Require().NotEmpty(cur, "live_root present after update %d", i)
		if i > 1 {
			s.Require().NotEqualf(prev, cur,
				"live_root must change on every update when interval=1 (step %d)", i)
		}
		prev = cur
	}
}

// TestLiveRoot_FollowerUpdateAfterStaleResume reproduces the Anaski
// production panic (2026-04-17):
//
//	panic: runtime error: slice bounds out of range [:1] with capacity 0
//	github.com/pokt-network/smt.isLeafNode(...) node_encoders.go:48
//	github.com/pokt-network/smt.(*SMT).parseSumTrieNode(...) smt.go:631
//
// Scenario:
//  1. Leader processes N updates past the last checkpoint boundary
//     (e.g. interval=10, updates 1..19). live_root is frozen at R_10.
//  2. Updates 11..19 each call trie.Commit(), which internally DELETES
//     the orphaned inner nodes of the previous tree version. After
//     update 19, many nodes that R_10 transitively references have
//     been purged from the shared nodes hash.
//  3. Leader dies. Follower takes over, reads live_root = R_10, imports
//     the tree, then calls UpdateTree with a new relay.
//  4. Tree traversal hits a child digest whose node was deleted in step 2.
//     store.Get() returns (nil, nil) per MapStore contract; the SMT
//     library passes the zero-length slice to parseSumTrieNode which
//     panics on data[:1].
//
// The existing TestLiveRoot_MidSessionResumePreservesTree test hides
// this bug because it kills the leader AT an interval boundary, so
// live_root points to the current root with no deleted orphans.
// TestLiveRoot_LossBoundedByInterval kills between boundaries but
// only flushes (no traversal-triggering UpdateTree), also hiding it.
//
// This test must PANIC before the atomic-checkpoint fix and PASS after.
// The fix: orphan HDELs must be deferred and flushed atomically with the
// next live_root SET, so live_root always references nodes present in Redis.
func (s *RedisSMSTTestSuite) TestLiveRoot_FollowerUpdateAfterStaleResume() {
	supplier := "pokt1live_stale_resume"
	sessionID := "session_live_stale"
	const interval = 10
	const leaderUpdates = 19 // past boundary 10, no checkpoint at 19
	const followerUpdates = 5

	leaderMgr := s.createTestRedisSMSTManagerWithInterval(supplier, interval)
	for i := 1; i <= leaderUpdates; i++ {
		s.Require().NoError(leaderMgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("leader_k%d", i)),
			[]byte(fmt.Sprintf("leader_v%d", i)),
			uint64(10)))
	}

	// Leader dies: drop in-memory state. Redis has the nodes hash (with
	// orphans from updates 11..19 already deleted) plus live_root = R_10.
	leaderMgr.treesMu.Lock()
	delete(leaderMgr.trees, sessionID)
	leaderMgr.treesMu.Unlock()

	// Follower takes over. UpdateTree must NOT panic: it will traverse
	// from R_10 to insert the new relay, and every child digest it
	// resolves must still exist in the nodes hash.
	followerMgr := s.createTestRedisSMSTManagerWithInterval(supplier, interval)
	for i := 1; i <= followerUpdates; i++ {
		err := followerMgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("follower_k%d", i)),
			[]byte(fmt.Sprintf("follower_v%d", i)),
			uint64(10))
		s.Require().NoErrorf(err,
			"follower UpdateTree #%d must not panic after resume from stale live_root (R_10)", i)
	}

	// Verify the follower's tree reflects the last checkpoint + its own
	// contribution: 10 relays from the leader (at R_10) + followerUpdates.
	// The 9 in-flight relays (updates 11..19) are the expected loss,
	// bounded by (interval - 1) as the live_root design promises.
	_, err := followerMgr.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	count, sum, err := followerMgr.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equalf(uint64(10+followerUpdates), count,
		"expected 10 (last checkpoint) + %d (follower) = %d relays after stale resume",
		followerUpdates, 10+followerUpdates)
	s.Require().Equal(uint64(10+followerUpdates)*10, sum)
}

// createTestRedisSMSTManagerWithInterval is a helper that lets tests set
// the checkpoint interval explicitly.
func (s *RedisSMSTTestSuite) createTestRedisSMSTManagerWithInterval(
	supplierAddr string, interval int,
) *RedisSMSTManager {
	config := RedisSMSTManagerConfig{
		SupplierAddress:            supplierAddr,
		CacheTTL:                   0,
		LiveRootCheckpointInterval: interval,
	}
	return NewRedisSMSTManager(zerolog.Nop(), s.redisClient, config)
}
