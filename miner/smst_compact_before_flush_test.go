//go:build test

package miner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// A big relay is held once while its batch is written, not twice: the leaf is
// compacted as soon as Commit hands its node to the store, and the store keeps
// the node smt gave it instead of a copy. A 1 MiB pulse OOM-killed the miner
// with the leaf and a copy of its node both resident until the HSET returned.

func bufferedNode(t *testing.T, store *RedisMapStore, key []byte) ([]byte, bool) {
	t.Helper()
	store.pipelineMu.Lock()
	defer store.pipelineMu.Unlock()
	v, ok := store.pipelineBuffer[hex.EncodeToString(key)]
	return v, ok
}

func TestRedisMapStore_SetKeepsTheSliceItIsGiven(t *testing.T) {
	h := newFlushFailureHarness(t, true)
	store := h.tree.store.(*RedisMapStore)
	key, value := []byte("node-digest"), transport.ChainedHashBytes("set-owns", 1<<20)

	store.BeginPipeline()
	require.NoError(t, store.Set(key, value))

	buffered, ok := bufferedNode(t, store, key)
	require.True(t, ok, "premise: in pipeline mode Set buffers the node")
	require.True(t, unsafe.SliceData(buffered) == unsafe.SliceData(value),
		"the buffer must hold the node smt handed over, not a second 1 MiB copy of it")
}

func TestRedisMapStore_GetServesABufferedNodeWithoutRedis(t *testing.T) {
	h := newFlushFailureHarness(t, true)
	store := h.tree.store.(*RedisMapStore)
	key, value := []byte("pending-digest"), transport.ChainedHashBytes("pending", 4096)
	want := bytes.Clone(value)

	store.BeginPipeline()
	require.NoError(t, store.Set(key, value))

	h.fail.Fail("redis unreachable")
	got, err := store.Get(key)
	h.fail.Clear()
	require.NoError(t, err, "a node no flush has written yet exists only in the buffer, and must be readable from it")
	require.Equal(t, want, got)

	got[0] ^= 0xff
	buffered, _ := bufferedNode(t, store, key)
	require.Equal(t, want, buffered, "Get hands out a copy: smt may append to what it reads, and the buffer is what the flush writes")

	// Control: once written, the node is read from Redis, so the same probe fails.
	require.NoError(t, store.FlushPipeline())
	h.fail.Fail("redis unreachable")
	_, err = store.Get(key)
	h.fail.Clear()
	require.Error(t, err, "CONTROL: a flushed node is not served from the buffer; if this passes, the probe above proves nothing")
}

// digestField is the Redis field a sum-trie leaf or inner node is stored under:
// sha256 of the node followed by its last 16 bytes, the sum and count (smt
// digestSumNode). Not for an extension node, whose digest smt takes from the
// inner node it expands to, not from its own encoding.
func digestField(node []byte) string {
	d := sha256.Sum256(node)
	return hex.EncodeToString(append(d[:], node[len(node)-16:]...))
}

func TestCommit_CompactsBigLeavesBeforeTheFlushAndLosesNothingWhenItFails(t *testing.T) {
	h := newFlushFailureHarness(t, true)
	h.seed(4)

	// Sizes around smt's own thresholds (256 B, 1 KiB) and a 1 MiB relay, all
	// incompressible so node compression cannot hide a size.
	var relays []flushFailureRelay
	for i, n := range []int{100, 600, 1 << 20} {
		key := sha256.Sum256([]byte(fmt.Sprintf("before-flush-%d", i)))
		relays = append(relays, flushFailureRelay{key: key[:], value: transport.ChainedHashBytes(fmt.Sprintf("v%d", i), n), weight: uint64(10 + i)})
	}
	for _, r := range relays {
		require.NoError(t, r.update(h.ctx, h.mgr, h.sessionID))
	}

	compacted := observability.SMSTLeavesCompacted.WithLabelValues(h.supplier)
	before := testutil.ToFloat64(compacted)
	pending := observability.SMSTPendingLeafBytes.WithLabelValues(h.supplier)
	pendingBefore := testutil.ToFloat64(pending)
	require.Greater(t, pendingBefore, float64(1<<20), "premise: the uncommitted leaves are counted as pending")
	h.fail.Fail("injected: flush pipeline failed")
	_, err := h.mgr.CommitTree(h.ctx, h.sessionID)
	h.fail.Clear()
	require.ErrorIs(t, err, ErrSMSTCommitFailed, "premise: the flush failed")
	require.GreaterOrEqual(t, testutil.ToFloat64(compacted)-before, float64(len(relays)),
		"every new leaf is compacted before the flush, so even a failed flush leaves no relay held twice")
	require.Equal(t, pendingBefore, testutil.ToFloat64(pending),
		"a failed flush leaves the compacted leaves' bytes in the store's buffer, so they stay counted as pending")
	for _, r := range relays {
		require.False(t, h.inRedis(r), "premise: the failed flush wrote nothing")
		require.True(t, h.inMemory(r), "a compacted leaf whose flush failed is still reachable, from the buffer")
	}

	h.commit()
	require.LessOrEqual(t, testutil.ToFloat64(pending), pendingBefore-float64(1<<20),
		"once the flush wrote them, the bytes are released, the 1 MiB leaf's included")
	root, err := h.mgr.FlushTree(h.ctx, h.sessionID)
	require.NoError(t, err)
	for _, r := range relays {
		require.True(t, h.inRedis(r), "the next flush writes the buffered node")
		require.NoError(t, h.proveAndVerify(r, root), "a %d B relay proves against the root", len(r.value))
	}

	fields, err := h.client.HGetAll(h.ctx, h.client.KB().SMSTNodesKey(h.supplier, h.sessionID)).Result()
	require.NoError(t, err)
	const extensionNodePrefix = 0x02
	var checked, biggest int
	for field, stored := range fields {
		node, err := decompressNode([]byte(stored))
		require.NoError(t, err)
		if node[0] == extensionNodePrefix {
			continue
		}
		require.Equal(t, field, digestField(node), "a stored node must hash to its own field: an aliased buffer would not")
		checked++
		biggest = max(biggest, len(node))
	}
	require.GreaterOrEqual(t, checked, len(relays), "CONTROL: every new leaf is among the nodes checked")
	require.Greater(t, biggest, 1<<20, "CONTROL: the 1 MiB leaf's node is among the stored nodes checked")
}

// FlushTree does not fail on a failed node write: the claim goes out with the
// root the tree holds, and the proof is built later from leaves already
// compacted. Until a flush succeeds, those nodes exist only in the store's
// buffer, and the proof has to be served from there.
func TestProveClosest_ServesACompactedLeafFromTheBufferAfterAFailedFlush(t *testing.T) {
	h := newFlushFailureHarness(t, true)
	h.seed(4)
	key := sha256.Sum256([]byte("proof-from-buffer"))
	big := flushFailureRelay{key: key[:], value: transport.ChainedHashBytes("proof-from-buffer", 1<<20), weight: 9}
	require.NoError(t, big.update(h.ctx, h.mgr, h.sessionID))

	h.fail.Fail("injected: redis unreachable for the seal")
	root, err := h.mgr.FlushTree(h.ctx, h.sessionID)
	require.NoError(t, err, "premise: a failed node write does not fail the seal")
	require.NotEmpty(t, root)

	// Redis answers again, but nothing has retried the flush: the leaf's node is
	// still only in the buffer when the proof is built.
	h.fail.Clear()
	require.False(t, h.inRedis(big), "premise: the leaf's node never reached Redis")
	require.NoError(t, h.proveAndVerify(big, root),
		"the 1 MiB relay's proof must be built from the buffered node and verify against the sealed root")
}
