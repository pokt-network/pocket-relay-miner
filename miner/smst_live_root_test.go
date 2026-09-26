//go:build test

package miner

// Tests for the live_root checkpoint mechanism: the Redis key that lets an HA
// follower resume a mid-session SMST after the previous leader dies without
// waiting for a flush. CheckpointLiveRoot writes it, which the relay batch runs
// before it acknowledges; an update alone writes nothing.
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
)

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
	const leaderUpdates = 20
	const followerUpdates = 15

	// Phase 1: "Leader" processes 20 relays, and the checkpoint its relay batch
	// runs before acknowledging them writes live_root.
	leaderMgr := s.createTestRedisSMSTManager(supplier)
	for i := 1; i <= leaderUpdates; i++ {
		s.Require().NoError(leaderMgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("leader_k%d", i)),
			[]byte(fmt.Sprintf("leader_v%d", i)),
			uint64(10)))
	}
	s.checkpoint(leaderMgr, sessionID)

	// Phase 2: leader "dies" - we drop its in-memory tree. Redis state is
	// unchanged (nodes hash + live_root at update 20).
	leaderMgr.treesMu.Lock()
	delete(leaderMgr.trees, sessionID)
	leaderMgr.treesMu.Unlock()

	// Phase 3: "Follower" takes over with a brand-new manager instance
	// (no shared in-memory state, only Redis). It adds 15 more updates.
	// The follower's GetOrCreateTree MUST resume from live_root - if it
	// creates a fresh empty tree, the leader's 20 relays are orphaned.
	followerMgr := s.createTestRedisSMSTManager(supplier)
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
	s.checkpoint(mgr, sessionID)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)

	exists, _ := s.redisClient.Exists(s.ctx, liveKey).Result()
	s.Require().Equal(int64(1), exists, "live_root must exist before DeleteTree")

	s.Require().NoError(mgr.DeleteTree(s.ctx, sessionID))

	exists, _ = s.redisClient.Exists(s.ctx, liveKey).Result()
	s.Require().Equal(int64(0), exists, "live_root must be cleaned up by DeleteTree")
}

// TestLiveRoot_FollowerUpdateAfterStaleResume reproduces the Anaski
// production panic (2026-04-17):
//
//	panic: runtime error: slice bounds out of range [:1] with capacity 0
//	github.com/pokt-network/smt.isLeafNode(...) node_encoders.go:48
//	github.com/pokt-network/smt.(*SMT).parseSumTrieNode(...) smt.go:631
//
// Scenario:
//  1. Leader processes N updates past its last checkpoint
//     (e.g. checkpoint at 10, updates 1..19). live_root is frozen at R_10.
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
// this bug because it kills the leader right after a checkpoint, so
// live_root points to the current root with no deleted orphans.
//
// This test must PANIC before the atomic-checkpoint fix and PASS after.
// The fix: orphan HDELs must be deferred and flushed atomically with the
// next live_root SET, so live_root always references nodes present in Redis.
func (s *RedisSMSTTestSuite) TestLiveRoot_FollowerUpdateAfterStaleResume() {
	supplier := "pokt1live_stale_resume"
	sessionID := "session_live_stale"
	const leaderUpdates = 19 // checkpointed at 10, not at 19
	const followerUpdates = 5

	leaderMgr := s.createTestRedisSMSTManager(supplier)
	for i := 1; i <= leaderUpdates; i++ {
		s.Require().NoError(leaderMgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("leader_k%d", i)),
			[]byte(fmt.Sprintf("leader_v%d", i)),
			uint64(10)))
		if i == 10 {
			// The relay batch's checkpoint: live_root = R_10.
			s.checkpoint(leaderMgr, sessionID)
		}
	}
	// A commit with no new live_root: updates 11..19 reach the nodes hash and
	// orphan nodes R_10 still references.
	resident, commitErr := leaderMgr.CommitTree(s.ctx, sessionID)
	s.Require().NoError(commitErr)
	s.Require().True(resident)

	// Leader dies: drop in-memory state. Redis has the nodes hash (with
	// orphans from updates 11..19 already deleted) plus live_root = R_10.
	leaderMgr.treesMu.Lock()
	delete(leaderMgr.trees, sessionID)
	leaderMgr.treesMu.Unlock()

	// Follower takes over. UpdateTree must NOT panic: it will traverse
	// from R_10 to insert the new relay, and every child digest it
	// resolves must still exist in the nodes hash.
	followerMgr := s.createTestRedisSMSTManager(supplier)
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
	// The 9 in-flight relays (updates 11..19) are the expected loss here:
	// their entries were never acknowledged, so a redelivery puts them back.
	_, err := followerMgr.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	count, sum, err := followerMgr.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equalf(uint64(10+followerUpdates), count,
		"expected 10 (last checkpoint) + %d (follower) = %d relays after stale resume",
		followerUpdates, 10+followerUpdates)
	s.Require().Equal(uint64(10+followerUpdates)*10, sum)
}
