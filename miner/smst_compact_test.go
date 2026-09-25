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
// so a test can exercise commitLocked's handling of a failed compaction
// without touching the real smt library. CompactPersistedLeaves returns no
// error, so the only way it fails is a panic, which runSMSTSafely recovers.
// calls counts the attempts. Every other method is promoted from the embedded
// interface unchanged.
type failingCompactor struct {
	smt.SparseMerkleSumTrie
	calls *int
}

func (f failingCompactor) CompactPersistedLeaves() int {
	*f.calls++
	panic("injected compaction failure")
}

// TestSMSTCompactsPersistedLeavesWithoutChangingTheRoot: after N relays, the
// compactor (wired in commitLocked right after Commit, before FlushPipeline) must
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
		"every persisted leaf should already be compacted after FlushTree -- commitLocked runs this right after Commit+FlushPipeline")
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

// TestARelayIsNotLostWhenCompactionFails: a compaction that panics must not
// fail the commit, must not evict the tree, and must not be tried again on it.
// The relays it ran after are already durable in the trie and in Redis. An
// error would hand them back as if the write had failed; an eviction would
// resume the tree from Redis and panic again, and after
// persistentCorruptionThreshold evictions purge the session's Redis state with
// its relays. So the tree keeps serving, uncompacted.
func TestARelayIsNotLostWhenCompactionFails(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier = "pokt1compact_fail_supplier"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        0,
	})
	const sessionID = "sess-compact-fail"

	key0 := sha256.Sum256([]byte("compact-fail-relay-0"))
	require.NoError(t, mgr.UpdateTree(ctx, sessionID, key0[:], []byte("relay-0-bytes"), 3))
	resident, err := mgr.CommitTree(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, resident)

	// Swap the tree's compactor for one that always panics, without touching
	// the real smt library -- every other trie method still delegates to the
	// real tree via the embedded interface.
	calls := 0
	mgr.treesMu.Lock()
	tree := mgr.trees[sessionID]
	tree.trie = failingCompactor{SparseMerkleSumTrie: tree.trie, calls: &calls}
	mgr.treesMu.Unlock()

	panics := observability.SMSTPanicsRecovered.WithLabelValues(supplier, "compact")
	panicsBefore := testutil.ToFloat64(panics)

	values := map[[32]byte][]byte{key0: []byte("relay-0-bytes")}
	for i := 1; i <= 3; i++ {
		key := sha256.Sum256([]byte(fmt.Sprintf("compact-fail-relay-%d", i)))
		values[key] = []byte(fmt.Sprintf("relay-%d-bytes", i))
		require.NoError(t, mgr.UpdateTree(ctx, sessionID, key[:], values[key], 5))
		resident, err := mgr.CommitTree(ctx, sessionID)
		require.NoError(t, err, "commit %d: a compaction panic must not surface as a commit error", i)
		require.True(t, resident, "commit %d: a compaction panic must not evict the tree", i)
	}

	require.Equal(t, 1, calls, "the compaction must be tried once and then not again on this tree")
	require.Equal(t, float64(1), testutil.ToFloat64(panics)-panicsBefore,
		"the one panic must be counted in ha_smst_panics_recovered_total{op=compact}")

	mgr.treesMu.RLock()
	stillResident := mgr.trees[sessionID] == tree
	mgr.treesMu.RUnlock()
	require.True(t, stillResident, "the same tree must still be resident: nothing evicted it")

	count, err := tree.trie.Count()
	require.NoError(t, err)
	require.Equal(t, uint64(len(values)), count, "every relay must be in the tree")
	for key, want := range values {
		got, _, err := tree.trie.Get(key[:])
		require.NoError(t, err)
		require.Equal(t, want, got, "relay %x must be readable, unmodified", key[:4])
	}
}
