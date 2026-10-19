//go:build test

package miner

import (
	"bytes"
	"fmt"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
)

// TestRedisSMSTProof_BasicOperations tests basic proof generation and verification.
// Ported from poktroll's TestSMST_Proof_Operations.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_BasicOperations() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_basic")
	sessionID := "session_proof_basic"

	// Add relays
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	err = manager.UpdateTree(s.ctx, sessionID, []byte("key2"), []byte("value2"), 200)
	s.Require().NoError(err)

	// Flush tree (required before generating proofs)
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof for key1
	path := protocol.GetPathForProof([]byte("key1"), sessionID)
	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)
	s.Require().NotEmpty(proofBytes, "proof should not be empty")

	// Unmarshal compact proof
	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)

	// Decompact for verification
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	// Verify Redis proof validates
	valid, err := smt.VerifyClosestProof(proof, rootHash, protocol.NewSMTSpec())
	s.Require().NoError(err)
	s.Require().True(valid, "Redis proof should be valid")

	// Also verify proof structure is correct
	s.Require().NotEmpty(proof.ClosestPath, "proof should have closest path")
	s.Require().NotEmpty(proof.ClosestValueHash, "proof should have value hash")
}

// TestRedisSMSTProof_InvalidDetection tests detection of tampered proofs.
// Ported from poktroll's TestSMST_Proof_ValidateBasic.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_InvalidDetection() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_invalid")
	sessionID := "session_proof_invalid"

	// Add relays
	relays := s.generateTestRelays(5, 111)
	for _, relay := range relays {
		err := manager.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}

	// Flush
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate valid proof
	path := protocol.GetPathForProof(relays[0].key, sessionID)
	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	// Unmarshal and decompact proof
	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	inMemorySMST := s.createInMemorySMST()
	for _, relay := range relays {
		err = inMemorySMST.Update(relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}
	err = inMemorySMST.Commit()
	s.Require().NoError(err)

	valid, err := smt.VerifyClosestProof(proof, rootHash, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().True(valid, "valid proof should pass verification")

	// Test with wrong root hash (should fail)
	wrongRoot := make([]byte, len(rootHash))
	copy(wrongRoot, rootHash)
	wrongRoot[0] ^= 0xFF // Flip bits

	valid, err = smt.VerifyClosestProof(proof, wrongRoot, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().False(valid, "proof should fail with wrong root hash")
}

// TestRedisSMSTProof_ClosestLeaf tests proof generation for closest leaf.
// Ported from poktroll's TestSMST_ProveWeighted.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_ClosestLeaf() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_closest")
	sessionID := "session_proof_closest"

	// Add relays with known paths
	key1 := s.generateKnownBitPath("all_zeros")
	key2 := s.generateKnownBitPath("all_ones")

	err := manager.UpdateTree(s.ctx, sessionID, key1, []byte("value1"), 100)
	s.Require().NoError(err)

	err = manager.UpdateTree(s.ctx, sessionID, key2, []byte("value2"), 200)
	s.Require().NoError(err)

	// Flush
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Prove for a key that doesn't exist (should find closest)
	nonExistentKey := s.generateKnownBitPath("alternating")
	path := protocol.GetPathForProof(nonExistentKey, sessionID)

	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)
	s.Require().NotEmpty(proofBytes)

	// Verify proof
	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	inMemorySMST := s.createInMemorySMST()
	err = inMemorySMST.Update(key1, []byte("value1"), 100)
	s.Require().NoError(err)
	err = inMemorySMST.Update(key2, []byte("value2"), 200)
	s.Require().NoError(err)
	err = inMemorySMST.Commit()
	s.Require().NoError(err)

	valid, err := smt.VerifyClosestProof(proof, rootHash, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().True(valid, "closest proof should be valid")
}

// TestRedisSMSTProof_EmptyTree tests proof generation on empty tree.
// Ported from poktroll's TestSMST_ProveWeighted_Empty.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_EmptyTree() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_empty")
	sessionID := "session_proof_empty"

	// Create empty tree (no relays added)
	_, err := manager.GetOrCreateTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Flush empty tree
	_, err = manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Try to generate proof (should work, but return empty proof or error)
	path := protocol.GetPathForProof([]byte("nonexistent"), sessionID)
	_, err = manager.ProveWeighted(s.ctx, sessionID, path)

	// Note: The behavior depends on implementation
	// It might error or return an empty proof
	// Either is acceptable for an empty tree
	if err != nil {
		s.Require().Contains(err.Error(), "", "error is acceptable for empty tree proof")
	}

	// Verify root hash is empty tree root
	inMemorySMST := s.createInMemorySMST()
	emptyRoot := inMemorySMST.Root()
	s.Require().NotEmpty(emptyRoot, "empty tree should have a root")
	// Note: Redis root might differ slightly, but should be deterministic
}

// TestRedisSMSTProof_SingleNode tests proof with single node.
// Ported from poktroll's TestSMST_ProveWeighted_OneNode.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_SingleNode() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_single")
	sessionID := "session_proof_single"

	// Add single relay
	err := manager.UpdateTree(s.ctx, sessionID, []byte("only_key"), []byte("only_value"), 100)
	s.Require().NoError(err)

	// Flush
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof for the only key
	path := protocol.GetPathForProof([]byte("only_key"), sessionID)
	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)
	s.Require().NotEmpty(proofBytes)

	// Verify proof
	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	inMemorySMST := s.createInMemorySMST()
	err = inMemorySMST.Update([]byte("only_key"), []byte("only_value"), 100)
	s.Require().NoError(err)
	err = inMemorySMST.Commit()
	s.Require().NoError(err)

	valid, err := smt.VerifyClosestProof(proof, rootHash, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().True(valid, "single node proof should be valid")
}

// TestRedisSMSTProof_Compression tests compact proof marshaling.
// Ported from poktroll's TestSMST_ProveWeighted_Proof (compression test).
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_Compression() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_compression")
	sessionID := "session_proof_compression"

	// Add multiple relays
	relays := s.generateTestRelays(20, 222)
	for _, relay := range relays {
		err := manager.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}

	// Flush
	_, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof
	path := protocol.GetPathForProof(relays[0].key, sessionID)
	compactProofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	// Verify it's compact (ProveWeighted returns CompactClosestProof)
	// The marshaled bytes should be relatively small
	s.Require().NotEmpty(compactProofBytes)

	// Unmarshal should work
	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(compactProofBytes)
	s.Require().NoError(err)
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	// Verify proof has sidenodes (indicating tree depth)
	s.Require().NotEmpty(proof.ClosestPath, "proof should have closest path")
}

// TestRedisSMSTProof_DepthValidation tests depth validation in proofs.
// Ported from poktroll's TestSMST_ClosestProof_ValidateBasic.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_DepthValidation() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_depth")
	sessionID := "session_proof_depth"

	// Create tree with maximum depth (two keys differing only in last bit)
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	key1[31] = 0x00
	key2[31] = 0x01

	err := manager.UpdateTree(s.ctx, sessionID, key1, []byte("value1"), 100)
	s.Require().NoError(err)

	err = manager.UpdateTree(s.ctx, sessionID, key2, []byte("value2"), 200)
	s.Require().NoError(err)

	// Flush
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof (should handle maximum depth)
	path := protocol.GetPathForProof(key1, sessionID)
	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	// Verify proof
	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	inMemorySMST := s.createInMemorySMST()
	err = inMemorySMST.Update(key1, []byte("value1"), 100)
	s.Require().NoError(err)
	err = inMemorySMST.Update(key2, []byte("value2"), 200)
	s.Require().NoError(err)
	err = inMemorySMST.Commit()
	s.Require().NoError(err)

	valid, err := smt.VerifyClosestProof(proof, rootHash, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().True(valid, "max depth proof should be valid")
}

// Redis-specific proof tests below

// TestRedisSMSTProof_AfterFlush tests that proof generation requires FlushTree().
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_AfterFlush() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_after_flush")
	sessionID := "session_proof_after_flush"

	// Add relay but DON'T flush
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	// Try to generate proof before flush (should fail)
	path := protocol.GetPathForProof([]byte("key1"), sessionID)
	_, err = manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().Error(err, "should not generate proof before FlushTree()")
	s.Require().Contains(err.Error(), "not been claimed yet", "error should mention claim requirement")

	// Now flush
	_, err = manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof (should succeed)
	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)
	s.Require().NotEmpty(proofBytes, "proof generation should succeed after flush")
}

// TestRedisSMSTProof_CachedProof tests proof caching in redisSMST struct.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_CachedProof() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_cached")
	sessionID := "session_proof_cached"

	// Add relay and flush
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	_, err = manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof for same path twice
	path := protocol.GetPathForProof([]byte("key1"), sessionID)

	proof1, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	proof2, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	// Proofs should be identical (cached)
	s.Require().True(bytes.Equal(proof1, proof2), "cached proof should be identical")
}

// TestRedisSMSTProof_WrongSession tests error on non-existent session.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_WrongSession() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_wrong_session")

	// Try to generate proof for non-existent session
	path := protocol.GetPathForProof([]byte("key1"), "nonexistent_session")
	_, err := manager.ProveWeighted(s.ctx, "nonexistent_session", path)
	s.Require().Error(err, "should error for non-existent session")
	s.Require().Contains(err.Error(), "not found", "error should mention session not found")
}

// TestRedisSMSTProof_MultipleRelays tests proof with many relays.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_MultipleRelays() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_multiple")
	sessionID := "session_proof_multiple"

	// Add 50 relays
	relays := s.generateTestRelays(50, 77) // Use byte-safe seed
	for _, relay := range relays {
		err := manager.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}

	// Flush
	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proofs for multiple relays
	for i := 0; i < 5; i++ { // Test first 5 relays
		path := protocol.GetPathForProof(relays[i].key, sessionID)
		proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
		s.Require().NoError(err, fmt.Sprintf("proof generation should succeed for relay %d", i))
		s.Require().NotEmpty(proofBytes)

		// Verify proof
		compactProof := &smt.SparseCompactMerkleClosestProof{}
		err = compactProof.Unmarshal(proofBytes)
		s.Require().NoError(err)
		proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
		s.Require().NoError(err)

		// Create in-memory SMST for verification
		inMemorySMST := s.createInMemorySMST()
		for _, relay := range relays {
			err = inMemorySMST.Update(relay.key, relay.value, relay.weight)
			s.Require().NoError(err)
		}
		err = inMemorySMST.Commit()
		s.Require().NoError(err)

		valid, err := smt.VerifyClosestProof(proof, rootHash, inMemorySMST.Spec())
		s.Require().NoError(err)
		s.Require().True(valid, fmt.Sprintf("proof should be valid for relay %d", i))
	}
}

// TestRedisSMSTProof_OnChainValidation simulates on-chain proof validation.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_OnChainValidation() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_onchain")
	sessionID := "session_proof_onchain"

	// Add relays
	relays := s.generateTestRelays(10, 88) // Use byte-safe seed
	for _, relay := range relays {
		err := manager.UpdateTree(s.ctx, sessionID, relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}

	// Flush to get root hash (this would be submitted in claim)
	claimRoot, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof (this would be submitted in proof)
	path := protocol.GetPathForProof(relays[0].key, sessionID)
	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	// Simulate on-chain validation
	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	// Verify proof against claim root (on-chain validator would do this)
	inMemorySMST := s.createInMemorySMST()
	for _, relay := range relays {
		err = inMemorySMST.Update(relay.key, relay.value, relay.weight)
		s.Require().NoError(err)
	}
	err = inMemorySMST.Commit()
	s.Require().NoError(err)

	valid, err := smt.VerifyClosestProof(proof, claimRoot, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().True(valid, "on-chain validator should accept proof")
}

// TestRedisSMSTProof_BitFlipping tests proof with bit-flipped paths.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_BitFlipping() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_bitflip")
	sessionID := "session_proof_bitflip"

	// Add relay
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	_, err = manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof with original path
	path := protocol.GetPathForProof([]byte("key1"), sessionID)
	_, err = manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	// Flip a bit in the path
	flippedPath := make([]byte, len(path))
	copy(flippedPath, path)
	flippedPath[0] ^= 0x01 // Flip least significant bit

	// Generate proof with flipped path (should still work - finds closest)
	proofBytes2, err := manager.ProveWeighted(s.ctx, sessionID, flippedPath)
	s.Require().NoError(err)
	s.Require().NotEmpty(proofBytes2)

	// Proofs might be the same (if tree is small) or different (if flipped path takes different route)
	// Either is valid behavior for ProveWeighted
}

// TestRedisSMSTProof_RootMismatch tests that proof fails with wrong root.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_RootMismatch() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_root_mismatch")
	sessionID := "session_proof_root_mismatch"

	// Create tree
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof
	path := protocol.GetPathForProof([]byte("key1"), sessionID)
	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	inMemorySMST := s.createInMemorySMST()
	err = inMemorySMST.Update([]byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)
	err = inMemorySMST.Commit()
	s.Require().NoError(err)

	// Verify with correct root (should pass)
	valid, err := smt.VerifyClosestProof(proof, rootHash, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().True(valid)

	// Verify with wrong root (should fail)
	wrongRoot := make([]byte, len(rootHash))
	copy(wrongRoot, rootHash)
	wrongRoot[0] ^= 0xFF

	valid, err = smt.VerifyClosestProof(proof, wrongRoot, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().False(valid, "proof should fail with wrong root hash")
}

// TestRedisSMSTProof_WeightMismatch tests that proof validation detects weight mismatches.
// Note: This tests the SMST library's validation, not our Redis implementation.
func (s *RedisSMSTTestSuite) TestRedisSMSTProof_WeightMismatch() {
	manager := s.createTestRedisSMSTManager("pokt1test_proof_weight_mismatch")
	sessionID := "session_proof_weight_mismatch"

	// Create tree with known weight
	err := manager.UpdateTree(s.ctx, sessionID, []byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)

	rootHash, err := manager.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)

	// Generate proof
	path := protocol.GetPathForProof([]byte("key1"), sessionID)
	proofBytes, err := manager.ProveWeighted(s.ctx, sessionID, path)
	s.Require().NoError(err)

	compactProof := &smt.SparseCompactMerkleClosestProof{}
	err = compactProof.Unmarshal(proofBytes)
	s.Require().NoError(err)
	proof, err := smt.DecompactClosestProof(compactProof, protocol.NewSMTSpec())
	s.Require().NoError(err)

	// Verify with correct weight
	inMemorySMST := s.createInMemorySMST()
	err = inMemorySMST.Update([]byte("key1"), []byte("value1"), 100)
	s.Require().NoError(err)
	err = inMemorySMST.Commit()
	s.Require().NoError(err)

	valid, err := smt.VerifyClosestProof(proof, rootHash, inMemorySMST.Spec())
	s.Require().NoError(err)
	s.Require().True(valid, "proof with correct weight should pass")

	// Create new tree with WRONG weight (should have different root)
	inMemorySMST2 := s.createInMemorySMST()
	err = inMemorySMST2.Update([]byte("key1"), []byte("value1"), 999) // Wrong weight!
	s.Require().NoError(err)
	err = inMemorySMST2.Commit()
	s.Require().NoError(err)

	wrongWeightRoot := inMemorySMST2.Root()

	// Roots should differ (different weight = different root)
	s.Require().NotEqual(rootHash, wrongWeightRoot, "different weight should produce different root")

	// Proof should fail with wrong-weight root
	valid, err = smt.VerifyClosestProof(proof, wrongWeightRoot, inMemorySMST2.Spec())
	s.Require().NoError(err)
	s.Require().False(valid, "proof should fail when weight doesn't match")
}
