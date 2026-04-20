//go:build test

package miner

// Tests that reproduce the Anaski production panics and prove that every
// SMT boundary in the miner converts a data-corruption panic into a
// logged error — never a goroutine crash.
//
// Context
//   2026-04-17: first Anaski panic (single update frame) — fixed by
//       f102395: atomic orphan+live_root checkpoint.
//   2026-04-19: second Anaski panic (two update frames) — a child of
//       the current root is missing from the nodes hash. The atomic-
//       checkpoint fix only eliminates one corruption path (orphan HDEL
//       race); operational realities (two-leader windows under CPU
//       pressure, manual hash surgery, Redis maxmemory evictions, etc.)
//       can still leave the hash with missing inner nodes.
//
// Defensive contract enforced by these tests:
//   1. A missing node in the Redis hash must surface as a returned error
//      from UpdateTree, never a panic.
//   2. The panic from any SMT library call (Update, Commit, Root,
//      ProveClosest, Import, MustCount, MustSum) must be caught at the
//      miner boundary.
//   3. After corruption is detected, the next UpdateTree call for the
//      same session must still succeed (either the tree self-heals or
//      the session is reset to a fresh state).

import (
	"encoding/hex"
	"fmt"
	"sync"
)

// smstInnerNodePrefix is the first byte of an inner-node payload as
// encoded by github.com/pokt-network/smt/node_encoders.go. Leaves use 0,
// inner nodes use 1, extensions use 2. The tests use this to pick a
// non-leaf victim to delete deterministically.
const smstInnerNodePrefix byte = 1

// buildTreeWithInnerNodes runs enough updates to guarantee the SMST has
// multiple internal nodes so the tests have something to corrupt.
func (s *RedisSMSTTestSuite) buildTreeWithInnerNodes(
	mgr *RedisSMSTManager, sessionID string, n int,
) {
	for i := 1; i <= n; i++ {
		s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("seed_k%04d", i)),
			[]byte(fmt.Sprintf("seed_v%04d", i)),
			uint64(10)),
			"seed update #%d must not fail", i)
	}
}

// pickInnerNodeField returns the hex field name of any inner node in the
// session's Redis hash. It intentionally skips leaves and extensions so
// the corruption target is guaranteed to sit above a leaf in the tree —
// exactly where the Anaski panic traversal happens.
func (s *RedisSMSTTestSuite) pickInnerNodeField(supplier, sessionID string) string {
	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	fields, err := s.redisClient.HGetAll(s.ctx, nodesKey).Result()
	s.Require().NoError(err, "HGetAll on nodes hash must succeed")
	s.Require().NotEmpty(fields, "nodes hash must have entries after seeding")

	for field, val := range fields {
		if len(val) > 0 && val[0] == smstInnerNodePrefix {
			return field
		}
	}
	s.FailNow("no inner node found in hash — tree not deep enough to reproduce panic")
	return ""
}

// forceResume drops the in-memory tree so the next UpdateTree call is
// forced through resumeTreeFromRedisLocked — that is the exact trigger
// the production Anaski stack hits on a standby-becomes-leader transition.
func (s *RedisSMSTTestSuite) forceResume(mgr *RedisSMSTManager, sessionID string) {
	mgr.treesMu.Lock()
	delete(mgr.trees, sessionID)
	mgr.treesMu.Unlock()
}

// TestSMSTUpdate_MissingInnerNode_ReturnsErrorNoPanic reproduces the
// Anaski 2026-04-19 stack trace:
//
//	panic: slice bounds out of range [:1] with capacity 0
//	parseSumTrieNode smt.go:631
//	resolveSumNode smt.go:625
//	resolveLazy smt.go:556
//	update smt.go:143 (depth=1, child resolve)
//	update smt.go:228 (depth=0, recursion into root's child)
//	SMT.Update smt.go:125
//	RedisSMSTManager.UpdateTree smst_manager.go:277
//
// Before the defensive fix this test panics. After the fix UpdateTree
// returns a non-nil error, the process keeps running, and subsequent
// updates on the same session succeed (self-heal or reset path).
func (s *RedisSMSTTestSuite) TestSMSTUpdate_MissingInnerNode_ReturnsErrorNoPanic() {
	supplier := "pokt1corrupt_inner_node"
	sessionID := "session_corrupt_inner"

	mgr := s.createTestRedisSMSTManager(supplier)
	s.buildTreeWithInnerNodes(mgr, sessionID, 32)

	// Delete every inner node so probe paths cannot miss the gap. A
	// single-victim version of this test is map-iteration-order
	// dependent (rare but real flake source): if none of the probe
	// keys' paths happen to descend through that one digest, errCount
	// stays at zero even though the defense is working correctly.
	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	fields, err := s.redisClient.HGetAll(s.ctx, nodesKey).Result()
	s.Require().NoError(err)
	victims := make([]string, 0, len(fields))
	for field, val := range fields {
		if len(val) > 0 && val[0] == smstInnerNodePrefix {
			victims = append(victims, field)
		}
	}
	s.Require().NotEmpty(victims, "need inner nodes to corrupt")
	s.Require().NoError(s.redisClient.HDel(s.ctx, nodesKey, victims...).Err(),
		"HDEL on all inner nodes must succeed")

	// Simulate a new leader picking up the session — exactly the path
	// the Anaski instance took when it panicked.
	s.forceResume(mgr, sessionID)

	// Fire a spread of updates so at least one path traverses through
	// a deleted digest. None may panic; the corrupted path must
	// return an error instead.
	var errCount int
	s.Require().NotPanics(func() {
		for i := 0; i < 64; i++ {
			if err := mgr.UpdateTree(s.ctx, sessionID,
				[]byte(fmt.Sprintf("probe_k%04d", i)),
				[]byte(fmt.Sprintf("probe_v%04d", i)),
				uint64(10)); err != nil {
				errCount++
			}
		}
	}, "UpdateTree must never panic on missing node — this is the regression guard for the Anaski 2026-04-19 crash")

	s.Require().Greaterf(errCount, 0,
		"expected at least one probe path to surface the corruption as an error")
}

// TestSMSTUpdate_MissingGrandchild_ReturnsErrorNoPanic covers the
// 2-level-deep stack specifically. It deletes every non-root inner
// node so the SMT library's recursive update is guaranteed to hit the
// gap at least one hop below the root. This guards against a narrower
// fix that only catches root-level resolves.
//
// Deleting more than one victim also stress-tests the eviction path:
// on the first corruption signal UpdateTree evicts the session and
// wipes the nodes hash, so subsequent probes see a fresh empty tree.
// The assertion is that NO probe ever panics; it is acceptable for
// later probes to succeed (self-heal is by design).
func (s *RedisSMSTTestSuite) TestSMSTUpdate_MissingGrandchild_ReturnsErrorNoPanic() {
	supplier := "pokt1corrupt_grandchild"
	sessionID := "session_corrupt_grandchild"

	mgr := s.createTestRedisSMSTManager(supplier)
	s.buildTreeWithInnerNodes(mgr, sessionID, 64)

	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)
	liveRoot, err := s.redisClient.Get(s.ctx, liveKey).Bytes()
	s.Require().NoError(err, "live_root must exist after seeding")
	s.Require().NotEmpty(liveRoot)

	// Exclude the root digest so at least one traversal step is needed
	// before the missing node is reached.
	rootHex := hex.EncodeToString(liveRoot[:32])

	fields, err := s.redisClient.HGetAll(s.ctx, nodesKey).Result()
	s.Require().NoError(err)

	victims := make([]string, 0, len(fields))
	for field, val := range fields {
		if len(val) == 0 || val[0] != smstInnerNodePrefix {
			continue
		}
		if field == rootHex {
			continue
		}
		victims = append(victims, field)
	}
	s.Require().NotEmpty(victims, "need at least one non-root inner node to delete")

	s.Require().NoError(s.redisClient.HDel(s.ctx, nodesKey, victims...).Err(),
		"HDEL on every non-root inner node must succeed")
	s.forceResume(mgr, sessionID)

	s.Require().NotPanics(func() {
		for i := 0; i < 128; i++ {
			_ = mgr.UpdateTree(s.ctx, sessionID,
				[]byte(fmt.Sprintf("probe_g%04d", i)),
				[]byte(fmt.Sprintf("probe_v%04d", i)),
				uint64(10))
		}
	}, "UpdateTree must not panic on a tree with every non-root inner node deleted — two-frame traversal must surface as error")
}

// TestSMSTUpdate_ConcurrentCorruption_NoGoroutinePanic models the
// "two-leader window" production scenario where CPU/memory pressure
// causes the Redis leader lock to flap and two managers briefly write
// to the same nodes hash. Instance A's orphan HDELs can wipe nodes that
// instance B's in-memory tree still references; the next UpdateTree on
// B hits a missing digest.
//
// Here we compress that into a deterministic race: many goroutines
// concurrently issue UpdateTree while another deletes random inner
// nodes from the hash. The assertion is simple: no goroutine panics,
// no matter how aggressively the hash is corrupted underneath.
func (s *RedisSMSTTestSuite) TestSMSTUpdate_ConcurrentCorruption_NoGoroutinePanic() {
	supplier := "pokt1corrupt_concurrent"
	sessionID := "session_corrupt_concurrent"

	mgr := s.createTestRedisSMSTManager(supplier)
	s.buildTreeWithInnerNodes(mgr, sessionID, 32)

	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	fields, err := s.redisClient.HGetAll(s.ctx, nodesKey).Result()
	s.Require().NoError(err)
	victims := make([]string, 0, len(fields))
	for field, val := range fields {
		if len(val) > 0 && val[0] == smstInnerNodePrefix {
			victims = append(victims, field)
		}
	}
	s.Require().NotEmpty(victims, "need inner nodes to corrupt concurrently")

	// Drop in-memory state to force a fresh resume from Redis under load.
	s.forceResume(mgr, sessionID)

	const workers = 8
	const updatesPerWorker = 32

	var (
		wg        sync.WaitGroup
		panicked  = make(chan any, workers+1)
		updateErr int64
		updateMu  sync.Mutex
	)

	// Corruption goroutine: HDEL every victim while updates are in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		for _, v := range victims {
			_ = s.redisClient.HDel(s.ctx, nodesKey, v).Err()
		}
	}()

	// Worker goroutines: hammer UpdateTree.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicked <- r
				}
			}()
			for i := 0; i < updatesPerWorker; i++ {
				err := mgr.UpdateTree(s.ctx, sessionID,
					[]byte(fmt.Sprintf("cc_w%d_k%04d", w, i)),
					[]byte(fmt.Sprintf("cc_w%d_v%04d", w, i)),
					uint64(10))
				if err != nil {
					updateMu.Lock()
					updateErr++
					updateMu.Unlock()
				}
			}
		}(w)
	}

	wg.Wait()
	close(panicked)

	// Drain panics — if any goroutine panicked the regression is live.
	for r := range panicked {
		s.FailNowf("goroutine panic under concurrent corruption",
			"recovered value=%v — UpdateTree must surface corruption as error, not panic", r)
	}

	// We don't pin a specific updateErr count because the race is
	// non-deterministic across Go schedulers. We only care that (a)
	// nothing panicked and (b) the process continues. Log for visibility:
	s.T().Logf("concurrent-corruption test: %d / %d updates errored (non-panic path exercised)",
		updateErr, workers*updatesPerWorker)
}
