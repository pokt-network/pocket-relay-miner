//go:build test

package miner

import "fmt"

// Integration-level tests that exercise multiple RedisSMSTManager instances
// (one per supplier) sharing a single Redis (miniredis). They reproduce the
// production scenario where ≥2 suppliers participate in the same session
// and prove that the Redis key schema correctly isolates per-supplier state.
//
// Context (the bug they cover):
//   - Before the schema fix, SMSTNodesKey/RootKey/StatsKey were keyed only
//     by sessionID. When multiple suppliers in the same session flushed
//     their trees, all three Redis keys collided and the last write won.
//   - After supplier#0 successfully proved, DeleteTree wiped the shared
//     Redis keys that supplier#1..N still needed for lazy-load during HA
//     failover, producing `session not found in memory or Redis` and
//     PROOF_MISSING claim expirations on-chain (stake drain).
//
// Each test evicts the in-memory `m.trees` entry to FORCE the
// `loadTreeFromRedis` path — the exact path taken by a follower after a
// leader kill in production.

// TestSMST_MultiSupplier_SharedSession_IndependentRoots verifies that two
// suppliers participating in the same session have independent roots in
// Redis after flush. Each supplier must be able to lazy-load its OWN root,
// not the other supplier's.
//
// FAILS on old schema: both flushes write to `ha:smst:{sessionID}:root`
// — last-write-wins — so one supplier's GetTreeRoot returns the other's root.
func (s *RedisSMSTTestSuite) TestSMST_MultiSupplier_SharedSession_IndependentRoots() {
	mgr0 := s.createTestRedisSMSTManager("pokt1supplier0_indep_roots")
	mgr1 := s.createTestRedisSMSTManager("pokt1supplier1_indep_roots")
	sessionID := "session_shared_indep_roots"

	// Each supplier adds different relays to its own tree
	s.Require().NoError(mgr0.UpdateTree(s.ctx, sessionID, []byte("relay_s0_key"), []byte("s0_value"), 100))
	s.Require().NoError(mgr1.UpdateTree(s.ctx, sessionID, []byte("relay_s1_key"), []byte("s1_value"), 200))

	// Flush both — simulates claim window, leader writes roots to Redis
	root0, err := mgr0.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err, "supplier0 flush")
	root1, err := mgr1.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err, "supplier1 flush")

	// Sanity: trees are distinct, so roots must be distinct
	s.Require().NotEqual(root0, root1,
		"suppliers with different relay sets must have different roots")

	// Simulate leader failover: drop in-memory state from BOTH managers
	// so the next read has to lazy-load from Redis (the bug scenario).
	mgr0.treesMu.Lock()
	delete(mgr0.trees, sessionID)
	mgr0.treesMu.Unlock()
	mgr1.treesMu.Lock()
	delete(mgr1.trees, sessionID)
	mgr1.treesMu.Unlock()

	// Each supplier must lazy-load ITS OWN root — not the other supplier's.
	gotRoot0, err := mgr0.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err, "supplier0 GetTreeRoot after eviction")
	s.Require().Equal(root0, gotRoot0,
		"supplier0 must lazy-load its own root; got supplier1's root (shared-key collision)")

	gotRoot1, err := mgr1.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err, "supplier1 GetTreeRoot after eviction")
	s.Require().Equal(root1, gotRoot1,
		"supplier1 must lazy-load its own root; got supplier0's root (shared-key collision)")
}

// TestSMST_MultiSupplier_DeleteTree_DoesNotAffectOtherSuppliers verifies that
// supplier0 cleaning up its SMST after proof submission does NOT wipe the
// Redis state that supplier1 still needs.
//
// FAILS on old schema: DeleteTree deletes `ha:smst:{sessionID}:nodes|root|stats`,
// which are shared across suppliers — so supplier1's lazy-load returns
// `redis: nil` ("session not found in memory or Redis") on the old code.
// This is the exact stake-drain bug observed in chaos testing on 2026-04-16.
func (s *RedisSMSTTestSuite) TestSMST_MultiSupplier_DeleteTree_DoesNotAffectOtherSuppliers() {
	mgr0 := s.createTestRedisSMSTManager("pokt1supplier0_delete_isol")
	mgr1 := s.createTestRedisSMSTManager("pokt1supplier1_delete_isol")
	sessionID := "session_shared_delete_isol"

	s.Require().NoError(mgr0.UpdateTree(s.ctx, sessionID, []byte("s0_key1"), []byte("s0_v1"), 100))
	s.Require().NoError(mgr0.UpdateTree(s.ctx, sessionID, []byte("s0_key2"), []byte("s0_v2"), 150))

	s.Require().NoError(mgr1.UpdateTree(s.ctx, sessionID, []byte("s1_key1"), []byte("s1_v1"), 200))
	s.Require().NoError(mgr1.UpdateTree(s.ctx, sessionID, []byte("s1_key2"), []byte("s1_v2"), 250))

	_, err := mgr0.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	root1, err := mgr1.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// supplier0 submits proof and cleans up (lifecycle_callback.go OnProofSubmitted).
	s.Require().NoError(mgr0.DeleteTree(s.ctx, sessionID), "DeleteTree should succeed")

	// Force supplier1 into the lazy-load path (follower scenario: no in-memory tree).
	mgr1.treesMu.Lock()
	delete(mgr1.trees, sessionID)
	mgr1.treesMu.Unlock()

	// supplier1 must still be able to lazy-load its tree and generate a proof.
	gotRoot1, err := mgr1.GetTreeRoot(s.ctx, sessionID)
	s.Require().NoError(err,
		"supplier1 lazy-load MUST NOT fail after supplier0's DeleteTree — "+
			"this is the stake-drain bug (PROOF_MISSING)")
	s.Require().Equal(root1, gotRoot1,
		"supplier1 root must be intact after supplier0 cleanup")

	// Proof generation must succeed against the lazy-loaded tree (end-to-end check).
	path := s.generateKnownBitPath("alternating")
	proof, err := mgr1.ProveClosest(s.ctx, sessionID, path)
	s.Require().NoError(err, "supplier1 ProveClosest after supplier0 DeleteTree")
	s.Require().NotEmpty(proof, "proof bytes must be non-empty")
}

// TestSMST_MultiSupplier_HAFailover_LazyLoadAllIndependently mirrors the
// production bug pattern end-to-end: N suppliers participate in the same
// session, the leader flushes all of their trees, then a follower (with an
// empty in-memory map for every supplier) must lazy-load and prove each one
// sequentially. After each successful proof, DeleteTree is called — which
// MUST NOT break the remaining suppliers' lazy-load.
//
// FAILS on old schema after the first DeleteTree: the shared Redis keys
// are wiped and every subsequent supplier gets `redis: nil`.
func (s *RedisSMSTTestSuite) TestSMST_MultiSupplier_HAFailover_LazyLoadAllIndependently() {
	const numSuppliers = 5
	sessionID := "session_shared_ha_multi"

	// Simulate the leader miner pod: one RedisSMSTManager per supplier,
	// each with its own tree for the same sessionID.
	leaderMgrs := make([]*RedisSMSTManager, numSuppliers)
	expectedRoots := make([][]byte, numSuppliers)
	for i := 0; i < numSuppliers; i++ {
		supplierAddr := "pokt1supplier_ha_" + byteToHex(byte(i))
		leaderMgrs[i] = s.createTestRedisSMSTManager(supplierAddr)

		// Unique relay set per supplier
		key := []byte("relay_" + supplierAddr)
		val := []byte("value_" + supplierAddr)
		s.Require().NoError(leaderMgrs[i].UpdateTree(s.ctx, sessionID, key, val, uint64(100*(i+1))))

		rootHash, err := leaderMgrs[i].FlushTree(s.ctx, sessionID)
		s.Require().NoError(err, "leader flush supplier #%d", i)
		expectedRoots[i] = rootHash
	}

	// Simulate follower failover: brand-new managers (empty in-memory state)
	// for the same suppliers, backed by the same Redis.
	followerMgrs := make([]*RedisSMSTManager, numSuppliers)
	for i := 0; i < numSuppliers; i++ {
		supplierAddr := "pokt1supplier_ha_" + byteToHex(byte(i))
		followerMgrs[i] = s.createTestRedisSMSTManager(supplierAddr)
	}

	// Each follower manager must lazy-load its supplier's tree, generate a
	// proof, then DeleteTree as cleanup (mirrors OnProofSubmitted in
	// lifecycle_callback). The cleanup must NOT break subsequent suppliers.
	path := s.generateKnownBitPath("alternating")
	for i := 0; i < numSuppliers; i++ {
		gotRoot, err := followerMgrs[i].GetTreeRoot(s.ctx, sessionID)
		s.Require().NoErrorf(err,
			"supplier #%d GetTreeRoot failed on follower (previous suppliers' DeleteTree wiped shared keys)",
			i)
		s.Require().Equalf(expectedRoots[i], gotRoot,
			"supplier #%d root mismatch — lazy-load returned another supplier's root", i)

		proof, err := followerMgrs[i].ProveClosest(s.ctx, sessionID, path)
		s.Require().NoErrorf(err, "supplier #%d ProveClosest on follower", i)
		s.Require().NotEmptyf(proof, "supplier #%d proof bytes empty", i)

		// Cleanup (mirrors OnProofSubmitted) — must not affect suppliers i+1..N.
		s.Require().NoErrorf(followerMgrs[i].DeleteTree(s.ctx, sessionID),
			"supplier #%d DeleteTree", i)
	}
}

// TestSMST_MultiSupplier_ConcurrentDeleteAndLazyLoad stress-tests the race
// between supplier0 issuing DeleteTree while supplier1 is mid lazy-load.
// With per-supplier keys, these operations hit disjoint Redis keys and
// never interfere. Before the fix, this would intermittently produce
// `redis: nil` on supplier1's lazy-load depending on Redis ordering.
//
// The test runs many iterations with race detector enabled to prove
// there is no shared state between suppliers in the SMST layer.
func (s *RedisSMSTTestSuite) TestSMST_MultiSupplier_ConcurrentDeleteAndLazyLoad() {
	const iterations = 50
	for iter := 0; iter < iterations; iter++ {
		// Fresh suppliers per iteration keeps each sub-run independent of
		// any carry-over state, so a failure at iteration N is always
		// reproducible from iteration N.
		supplier0 := fmt.Sprintf("pokt1supplier0_concurrent_%d", iter)
		supplier1 := fmt.Sprintf("pokt1supplier1_concurrent_%d", iter)
		sessionID := fmt.Sprintf("session_concurrent_%d", iter)

		mgr0 := s.createTestRedisSMSTManager(supplier0)
		mgr1 := s.createTestRedisSMSTManager(supplier1)

		s.Require().NoError(mgr0.UpdateTree(s.ctx, sessionID, []byte("s0_relay"), []byte("v0"), 100))
		s.Require().NoError(mgr1.UpdateTree(s.ctx, sessionID, []byte("s1_relay"), []byte("v1"), 200))

		_, err := mgr0.FlushTree(s.ctx, sessionID)
		s.Require().NoError(err)
		expectedRoot1, err := mgr1.FlushTree(s.ctx, sessionID)
		s.Require().NoError(err)

		// Evict supplier1 in-memory to force the lazy-load path.
		mgr1.treesMu.Lock()
		delete(mgr1.trees, sessionID)
		mgr1.treesMu.Unlock()

		// Fire DeleteTree (supplier0) and GetTreeRoot (supplier1) concurrently.
		start := make(chan struct{})
		done := make(chan error, 2)

		go func() {
			<-start
			done <- mgr0.DeleteTree(s.ctx, sessionID)
		}()
		go func() {
			<-start
			_, err := mgr1.GetTreeRoot(s.ctx, sessionID)
			done <- err
		}()

		close(start)

		err0 := <-done
		err1 := <-done
		s.Require().NoErrorf(err0, "iter %d: DeleteTree on supplier0 must not fail", iter)
		s.Require().NoErrorf(err1, "iter %d: supplier1 GetTreeRoot must not observe supplier0's delete (key isolation)", iter)

		// supplier1's tree must be intact — verify root still matches.
		root1After, err := mgr1.GetTreeRoot(s.ctx, sessionID)
		s.Require().NoErrorf(err, "iter %d: supplier1 GetTreeRoot after race", iter)
		s.Require().Equalf(expectedRoot1, root1After,
			"iter %d: supplier1 root changed after supplier0 delete (key leak)", iter)
	}
}

// byteToHex returns a two-character hex string for a byte.
// Used to produce unique, stable supplier address suffixes in tests.
func byteToHex(b byte) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[b>>4], hex[b&0x0f]})
}
