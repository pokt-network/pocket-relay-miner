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
