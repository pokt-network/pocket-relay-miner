//go:build test

package miner

import (
	"bytes"
	"sync/atomic"
)

// TestFlushTree_SealMismatch_AcceptsPostWaitRoot verifies that FlushTree
// does not abort the claim when the root captured before the wait window
// differs from the root captured after it. Because tree.sealing is set to
// true under the tree lock BEFORE rootAfterSeal is captured, no legitimate
// UpdateTree can land during the wait — a mismatch therefore indicates a
// subtle library/sealing-flag anomaly rather than an in-flight relay. The
// correct response is to trust rootAfterWait (stable now that the lock is
// held again) and proceed with the claim, logging a warning so the
// anomaly remains observable.
//
// The pre-fix behavior was to return an error and abandon the session,
// which silently dropped the entire claim and caused on-chain ComputeUnits
// to undercount relative to network RPS.
//
// The test installs a FlushTree test hook that fires inside the unlock
// window and directly mutates tree.trie via the session's RedisMapStore
// (the same backing store the tree uses). The sealing flag intentionally
// does NOT guard this direct path — it only guards the UpdateTree entry
// point — so the hook can simulate the anomaly the old code failed on.
func (s *RedisSMSTTestSuite) TestFlushTree_SealMismatch_AcceptsPostWaitRoot() {
	manager := s.createTestRedisSMSTManager("pokt1flush_seal_mismatch")
	sessionID := "session_seal_mismatch"

	// Seed the tree with one relay so rootAfterSeal is well-defined.
	err := manager.UpdateTree(s.ctx, sessionID, []byte("k_initial"), []byte("v_initial"), 100)
	s.Require().NoError(err)

	// Grab the tree so the hook can modify it directly. The manager keeps
	// it cached under treesMu; once we have the pointer the hook closure
	// uses it without re-traversing the map.
	manager.treesMu.RLock()
	tree, ok := manager.trees[sessionID]
	manager.treesMu.RUnlock()
	s.Require().True(ok, "tree must be populated after initial update")

	// Install a hook that fires inside Phase 3 (after rootAfterSeal is
	// captured, before rootAfterWait is captured). The hook commits a new
	// leaf directly through the trie, bypassing UpdateTree's sealing
	// check, so the next Root() read under the post-wait lock returns a
	// different value.
	var hookFired atomic.Int32
	installFlushSealWaitHook(func(sid string) {
		if sid != sessionID {
			return
		}
		if hookFired.Add(1) > 1 {
			return // only fire once per FlushTree
		}
		tree.mu.Lock()
		defer tree.mu.Unlock()

		// Directly mutate the trie. This simulates the anomaly described
		// in the bug report — a root-changing operation that bypasses the
		// sealing flag. Errors here are reported via suite assertions so
		// the hook completion doesn't mask a setup failure.
		if upErr := tree.trie.Update([]byte("k_injected"), []byte("v_injected"), 200); upErr != nil {
			s.T().Errorf("hook: direct Update failed: %v", upErr)
			return
		}
		if cErr := tree.trie.Commit(); cErr != nil {
			s.T().Errorf("hook: direct Commit failed: %v", cErr)
			return
		}
	})
	defer installFlushSealWaitHook(nil)

	rootAfterFlush, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err, "FlushTree must accept post-wait root and proceed, not error out")
	s.Require().Equal(1, int(hookFired.Load()), "hook must have fired exactly once")
	s.Require().Len(rootAfterFlush, SMSTRootLen, "flushed root must have canonical length")

	// The claimed root must equal the post-wait root. We re-read it via
	// GetTreeRoot; any cached value inside the manager would have been
	// assigned from rootAfterWait by the fix.
	gotRoot, err := manager.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().True(bytes.Equal(gotRoot, rootAfterFlush),
		"post-flush GetTreeRoot must match the value returned by FlushTree")
}

// TestFlushTree_SealMismatch_UsesWaitRootNotSealRoot asserts the stronger
// property that the ACCEPTED root specifically equals the post-wait
// reading (i.e. the trie state after the hook-injected Update landed) and
// is DIFFERENT from the pre-wait reading. This confirms the fix doesn't
// just "swallow errors" but correctly treats rootAfterWait as the
// authoritative sealed state.
func (s *RedisSMSTTestSuite) TestFlushTree_SealMismatch_UsesWaitRootNotSealRoot() {
	manager := s.createTestRedisSMSTManager("pokt1flush_seal_wait_root")
	sessionID := "session_wait_root_auth"

	err := manager.UpdateTree(s.ctx, sessionID, []byte("k0"), []byte("v0"), 100)
	s.Require().NoError(err)

	manager.treesMu.RLock()
	tree, ok := manager.trees[sessionID]
	manager.treesMu.RUnlock()
	s.Require().True(ok)

	// Capture the pre-mutation root (will be rootAfterSeal inside FlushTree).
	tree.mu.Lock()
	preSealRoot := bytes.Clone(tree.trie.Root())
	tree.mu.Unlock()

	// Second manager instance used for Redis key probing if needed —
	// here we just use the same manager, but we want a distinct value for
	// the comparison. Install the same injection hook.
	var hookFired atomic.Int32
	installFlushSealWaitHook(func(sid string) {
		if sid != sessionID || hookFired.Add(1) > 1 {
			return
		}
		tree.mu.Lock()
		defer tree.mu.Unlock()
		if upErr := tree.trie.Update([]byte("k_wait_mutation"), []byte("v_wait_mutation"), 500); upErr != nil {
			s.T().Errorf("hook: direct Update failed: %v", upErr)
			return
		}
		if cErr := tree.trie.Commit(); cErr != nil {
			s.T().Errorf("hook: direct Commit failed: %v", cErr)
			return
		}
	})
	defer installFlushSealWaitHook(nil)

	acceptedRoot, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	s.Require().Equal(1, int(hookFired.Load()))

	// The accepted root must NOT equal the pre-seal reading — that's the
	// value the old buggy code either returned an error on or (worse)
	// would have used. It must equal the stable post-wait reading, which
	// we re-derive below directly from the trie.
	s.Require().False(bytes.Equal(acceptedRoot, preSealRoot),
		"accepted root must not equal the pre-wait reading (defeats the purpose of the fix)")

	tree.mu.Lock()
	postWaitRoot := bytes.Clone(tree.trie.Root())
	tree.mu.Unlock()

	s.Require().True(bytes.Equal(acceptedRoot, postWaitRoot),
		"accepted root must equal the post-wait reading (the authoritative sealed state)")
}

// installFlushSealWaitHook is a test-only helper to set/clear the package
// hook without exporting it. Centralised so all tests that use it go
// through the same guarded path and failures to clean up are visible.
func installFlushSealWaitHook(hook func(sessionID string)) {
	flushTreeSealWaitHook = hook
}
