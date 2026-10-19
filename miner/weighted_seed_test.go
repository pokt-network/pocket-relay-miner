//go:build test

package miner

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/stretchr/testify/require"
)

// seedForLeaf returns a seed whose weighted proof is for the leaf at leafPath,
// by trying each leaf index of the trie under root. Tests that need the proof
// of one particular relay use it: a weighted proof's leaf is chosen by the
// seed, not by a path.
func seedForLeaf(root, leafPath []byte, spec *smt.TrieSpec, prove func(seed []byte) ([]byte, error)) ([]byte, error) {
	count, err := smt.MerkleSumRoot(root).Count()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < count; i++ {
		seed := make([]byte, 32)
		binary.BigEndian.PutUint64(seed, i)
		proofBz, err := prove(seed)
		if err != nil {
			return nil, err
		}
		proof, err := decompactWeightedProof(proofBz, spec)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(proof.ClosestPath, leafPath) {
			return seed, nil
		}
	}
	return nil, fmt.Errorf("no seed selects leaf %x among %d leaves", leafPath, count)
}

func decompactWeightedProof(proofBz []byte, spec *smt.TrieSpec) (*smt.SparseMerkleClosestProof, error) {
	var compact smt.SparseCompactMerkleClosestProof
	if err := compact.Unmarshal(proofBz); err != nil {
		return nil, fmt.Errorf("unmarshal proof: %w", err)
	}
	return smt.DecompactClosestProof(&compact, spec)
}

// weightedVerifies checks proofBz the way the chain checks a submitted proof:
// a closest proof against root, for the leaf seed selects by relay count.
func weightedVerifies(t testing.TB, proofBz, root, seed []byte) bool {
	t.Helper()
	proof, err := decompactWeightedProof(proofBz, protocol.NewSMTSpec())
	require.NoError(t, err)
	ok, err := smt.VerifyWeightedProof(proof, root, seed, protocol.NewSMTSpec())
	require.NoError(t, err)
	return ok
}
