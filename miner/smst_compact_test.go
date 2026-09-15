//go:build test

package miner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/pokt-network/smt/kvstore/simplemap"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// failingCompactor wraps a real trie and makes CompactPersistedLeaves fail,
// so a test can exercise updateTree's error-swallowing path (item 269/C8)
// without touching the real .smt-c8 library. Every other method is promoted
// from the embedded interface unchanged.
type failingCompactor struct {
	smt.SparseMerkleSumTrie
}

func (failingCompactor) CompactPersistedLeaves() (int, error) {
	return 0, errors.New("injected compaction failure")
}

// TestSMSTCompactsPersistedLeavesWithoutChangingTheRoot: after N relays, the
// compactor (wired in updateTree right after FlushPipeline, item 269/C8) must
// have compacted every persisted leaf, and the sealed root the miner signs
// must be identical to a twin tree that never compacts anything.
func TestSMSTCompactsPersistedLeavesWithoutChangingTheRoot(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: "pokt1compact_supplier",
		CacheTTL:        0,
	})

	const sessionID = "sess-compact-root"
	const n = 40

	type leaf struct {
		key, value []byte
		weight     uint64
	}
	leaves := make([]leaf, n)
	for i := range leaves {
		key := sha256.Sum256([]byte(fmt.Sprintf("compact-relay-%d", i)))
		leaves[i] = leaf{
			key:    key[:],
			value:  []byte(fmt.Sprintf("compact-relay-bytes-%d", i)),
			weight: uint64(i%5 + 1),
		}
	}

	// Twin tree: never goes through the manager, so nothing ever calls
	// CompactPersistedLeaves on it. If compaction changed the root, this
	// would diverge from the manager's tree.
	twin := smt.NewSparseMerkleSumTrie(simplemap.NewSimpleMap(), protocol.NewTrieHasher(), protocol.SMTValueHasher())

	for _, l := range leaves {
		require.NoError(t, mgr.UpdateTree(ctx, sessionID, l.key, l.value, l.weight))
		require.NoError(t, twin.Update(l.key, l.value, l.weight))
	}
	require.NoError(t, twin.Commit())

	managerRoot, err := mgr.FlushTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, []byte(twin.Root()), managerRoot,
		"compaction must not change the root the miner signs")

	mgr.treesMu.RLock()
	tree, ok := mgr.trees[sessionID]
	mgr.treesMu.RUnlock()
	require.True(t, ok, "the sealed tree must still be resident right after FlushTree")

	compactor, ok := tree.trie.(interface {
		CompactPersistedLeaves() (int, error)
	})
	require.True(t, ok, "the tree must expose the .smt-c8 compactor")

	compactedAgain, err := compactor.CompactPersistedLeaves()
	require.NoError(t, err)
	require.Zero(t, compactedAgain,
		"every persisted leaf should already be compacted after n relays -- updateTree runs this once per relay, right after Commit+FlushPipeline")
}

// TestARelayIsNotLostWhenCompactionFails: a compaction failure must be logged
// and swallowed, never surfaced as an UpdateTree error -- the relay it would
// apply to is already durable in the trie and in Redis by the time
// compaction runs.
func TestARelayIsNotLostWhenCompactionFails(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: "pokt1compact_fail_supplier",
		CacheTTL:        0,
	})
	const sessionID = "sess-compact-fail"

	key0 := sha256.Sum256([]byte("compact-fail-relay-0"))
	require.NoError(t, mgr.UpdateTree(ctx, sessionID, key0[:], []byte("relay-0-bytes"), 3))

	// Swap the tree's compactor for one that always errors, without
	// touching the real .smt-c8 library -- every other trie method still
	// delegates to the real tree via the embedded interface.
	mgr.treesMu.Lock()
	tree := mgr.trees[sessionID]
	tree.trie = failingCompactor{tree.trie}
	mgr.treesMu.Unlock()

	key1 := sha256.Sum256([]byte("compact-fail-relay-1"))
	err := mgr.UpdateTree(ctx, sessionID, key1[:], []byte("relay-1-bytes"), 5)
	require.NoError(t, err, "a compaction failure must not surface as an UpdateTree error")

	count, err := tree.trie.Count()
	require.NoError(t, err)
	require.Equal(t, uint64(2), count, "both relays must be in the tree -- the failing compaction must not have dropped the second one")

	gotValue, gotWeight, err := tree.trie.Get(key1[:])
	require.NoError(t, err)
	require.Equal(t, []byte("relay-1-bytes"), gotValue, "the relay whose commit ran alongside the failing compaction must be readable, unmodified")
	require.Equal(t, uint64(5), gotWeight)
}
