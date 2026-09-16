//go:build test

package miner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/pokt-network/smt/kvstore/simplemap"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// failingCompactor wraps a real trie and makes CompactPersistedLeaves fail,
// so a test can exercise updateTree's failure-swallowing path without
// touching the real smt library. CompactPersistedLeaves returns no error, so
// the only way it fails is a panic, which runSMSTSafely recovers. Every other
// method is promoted from the embedded interface unchanged.
type failingCompactor struct {
	smt.SparseMerkleSumTrie
}

func (failingCompactor) CompactPersistedLeaves() int {
	panic("injected compaction failure")
}

// TestSMSTCompactsPersistedLeavesWithoutChangingTheRoot: after N relays, the
// compactor (wired in updateTree right after FlushPipeline) must
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

	compactor, ok := tree.trie.(leafCompactor)
	require.True(t, ok, "the tree must expose the smt compactor")

	compactedAgain := compactor.CompactPersistedLeaves()
	require.Zero(t, compactedAgain,
		"every persisted leaf should already be compacted after n relays -- updateTree runs this once per relay, right after Commit+FlushPipeline")
}

// TestCommitTreeCountsEveryCompactedLeaf: a commit must really compact, and
// ha_smst_leaves_compacted_total is how an operator sees that it does. Were the
// runtime assertion in commitLocked to stop matching the smt trie, CommitTree
// would still succeed and the root would still be right -- only this counter
// stays flat. The commit persists the n leaves the updates added and the
// compaction right after drops every one of them, so it counts exactly n.
func TestCommitTreeCountsEveryCompactedLeaf(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier = "pokt1compact_counter_supplier"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        0,
	})
	const sessionID = "sess-compact-counter"
	const n = 25

	counter := observability.SMSTLeavesCompacted.WithLabelValues(supplier)
	before := testutil.ToFloat64(counter)
	for i := 0; i < n; i++ {
		key := sha256.Sum256([]byte(fmt.Sprintf("compact-counter-relay-%d", i)))
		require.NoError(t, mgr.UpdateTree(ctx, sessionID, key[:], []byte(fmt.Sprintf("relay-%d", i)), 1))
	}
	require.Zero(t, testutil.ToFloat64(counter)-before, "an update commits nothing, so it must compact nothing")

	resident, err := mgr.CommitTree(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, resident, "the tree the updates built must still be resident")

	require.Equal(t, float64(n), testutil.ToFloat64(counter)-before,
		"committing %d relays must compact %d leaves; a flat counter means the commit never reached CompactPersistedLeaves", n, n)
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
	// touching the real smt library -- every other trie method still
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
