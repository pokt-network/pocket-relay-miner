//go:build test

package miner

import (
	"context"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A failed FlushPipeline leaves nodes buffered in the store, already marked
// persisted in the trie. Every writer of a root has to send them before the
// root: a root stored while nodes under it are only in that buffer points a
// resumed tree at digests Redis does not have.

var errInjectedHSet = errors.New("injected: HSET failed")

// hsetFailSwitch fails only HSET, as a single command and inside a pipeline, so
// a test can make the nodes write fail while the root write still reaches
// Redis. Failing every command instead would also stop the root, and a writer
// that ignored the nodes error would pass for the wrong reason.
type hsetFailSwitch struct {
	on atomic.Bool
}

func newHSetFailSwitch(client redis.UniversalClient) *hsetFailSwitch {
	f := &hsetFailSwitch{}
	client.AddHook(f)
	return f
}

func (f *hsetFailSwitch) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *hsetFailSwitch) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if f.on.Load() && cmd.Name() == "hset" {
			return errInjectedHSet
		}
		return next(ctx, cmd)
	}
}

func (f *hsetFailSwitch) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if f.on.Load() {
			for _, cmd := range cmds {
				if cmd.Name() == "hset" {
					for _, c := range cmds {
						c.SetErr(errInjectedHSet)
					}
					return errInjectedHSet
				}
			}
		}
		return next(ctx, cmds)
	}
}

func (h *flushFailureHarness) liveRoot() []byte {
	h.t.Helper()
	return h.getRootKey(h.client.KB().SMSTLiveRootKey(h.supplier, h.sessionID))
}

func (h *flushFailureHarness) claimedRoot() []byte {
	h.t.Helper()
	return h.getRootKey(h.client.KB().SMSTRootKey(h.supplier, h.sessionID))
}

func (h *flushFailureHarness) getRootKey(key string) []byte {
	h.t.Helper()
	val, err := h.client.Get(h.ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	require.NoError(h.t, err)
	return val
}

func TestRedisMapStore_FailedFlushKeepsBufferedNodesForTheNextFlush(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	fail := newHSetFailSwitch(client)

	const supplier, sessionID = "pokt1pending_nodes_supplier", "sess-pending-nodes-store"
	store, ok := NewRedisMapStore(ctx, client, supplier, sessionID).(*RedisMapStore)
	require.True(t, ok)
	hashKey := client.KB().SMSTNodesKey(supplier, sessionID)

	store.BeginPipeline()
	require.NoError(t, store.Set([]byte("node-a"), []byte("value-a")))
	fail.on.Store(true)
	err := store.FlushPipeline()
	fail.on.Store(false)
	require.ErrorIs(t, err, errInjectedHSet, "the flush must fail on the injected HSET")

	exists, err := client.HExists(ctx, hashKey, hex.EncodeToString([]byte("node-a"))).Result()
	require.NoError(t, err)
	require.False(t, exists, "CONTROL: the failed flush wrote nothing")

	store.BeginPipeline()
	require.NoError(t, store.Set([]byte("node-b"), []byte("value-b")))
	require.NoError(t, store.FlushPipeline())

	for field, want := range map[string]string{"node-a": "value-a", "node-b": "value-b"} {
		stored, err := client.HGet(ctx, hashKey, hex.EncodeToString([]byte(field))).Bytes()
		require.NoError(t, err, "%s must be in Redis after the next successful flush", field)
		// Read the way the store reads: a node is stored as itself or as one
		// zstd frame of it (item 398). Comparing the stored bytes directly
		// passes here only because these values are too small to compress —
		// a property of the fixture, not of the store.
		got, decErr := decompressNode(stored)
		require.NoError(t, decErr)
		require.Equal(t, want, string(got))
	}
}

func TestFlushFailure_CheckpointLiveRootWritesBufferedNodesFirst(t *testing.T) {
	h := newFlushFailureHarness(t, true)
	h.seed(8)
	a := newFlushFailureRelay("relay-a", 7)
	h.failFlush(a)
	require.False(t, h.inRedis(a), "CONTROL: A's leaf has not reached Redis")

	resident, _, err := h.mgr.CheckpointLiveRoot(h.ctx, h.sessionID)
	require.NoError(t, err)
	require.True(t, resident)
	require.True(t, h.inRedis(a), "the checkpoint must write the buffered leaf before live_root")
	require.Equal(t, []byte(h.tree.trie.Root()), h.liveRoot(), "live_root must be the root that covers A")
}

func TestFlushFailure_CheckpointLiveRootKeepsTheOldRootWhileBufferedNodesCannotBeWritten(t *testing.T) {
	h := newFlushFailureHarness(t, true)
	hsetFail := newHSetFailSwitch(h.client)
	h.seed(8)
	before := h.liveRoot()
	require.Len(t, before, SMSTRootLen, "CONTROL: the first seed update checkpointed live_root")
	a := newFlushFailureRelay("relay-a", 7)
	h.failFlush(a)

	hsetFail.on.Store(true)
	_, _, err := h.mgr.CheckpointLiveRoot(h.ctx, h.sessionID)
	hsetFail.on.Store(false)
	require.ErrorIs(t, err, errInjectedHSet, "a checkpoint that cannot write the buffered nodes must fail, so the batch is not acknowledged")
	require.Equal(t, before, h.liveRoot(), "live_root must not move to a root whose nodes are not in Redis")
	require.False(t, h.inRedis(a))

	resident, _, err := h.mgr.CheckpointLiveRoot(h.ctx, h.sessionID)
	require.NoError(t, err)
	require.True(t, resident)
	require.True(t, h.inRedis(a), "the next checkpoint must write the nodes still buffered")
	require.Equal(t, []byte(h.tree.trie.Root()), h.liveRoot())
}

func TestFlushFailure_ExitCheckpointWritesBufferedNodesFirst(t *testing.T) {
	h := newFlushFailureHarness(t, true)
	h.seed(8)
	a := newFlushFailureRelay("relay-a", 7)
	h.failFlush(a)
	require.False(t, h.inRedis(a), "CONTROL: A's leaf has not reached Redis")

	written, err := h.mgr.CheckpointLiveRootOnExit(h.ctx, h.sessionID)
	require.NoError(t, err)
	require.True(t, written, "CONTROL: live_root still holds what this manager wrote, so the exit checkpoint writes")
	require.True(t, h.inRedis(a), "the exit checkpoint must write the buffered leaf before live_root")
	require.Equal(t, []byte(h.tree.trie.Root()), h.liveRoot())
}

func TestFlushFailure_FlushTreeWritesBufferedNodesBeforeClaimedRoot(t *testing.T) {
	for _, compact := range []bool{true, false} {
		t.Run(compactionLabel(compact), func(t *testing.T) {
			h := newFlushFailureHarness(t, compact)
			h.seed(8)
			a := newFlushFailureRelay("relay-a", 7)
			h.failFlush(a)
			require.False(t, h.inRedis(a), "CONTROL: A's leaf has not reached Redis")

			root, err := h.mgr.FlushTree(h.ctx, h.sessionID)
			require.NoError(t, err)
			require.True(t, h.inRedis(a), "FlushTree must write the buffered leaf before claimed_root")
			require.Equal(t, root, h.claimedRoot())

			// A manager that never held the tree proves A from Redis alone, so
			// the proof walks the stored nodes rather than the resident trie.
			resumed := *h
			resumed.mgr = NewRedisSMSTManager(zerolog.Nop(), h.client, RedisSMSTManagerConfig{
				SupplierAddress: h.supplier,
			})
			require.NoError(t, resumed.proveAndVerify(a, root), "A must be provable from a tree resumed at claimed_root")
		})
	}
}

func TestFlushFailure_FlushTreeDoesNotStoreClaimedRootWhileBufferedNodesCannotBeWritten(t *testing.T) {
	h := newFlushFailureHarness(t, true)
	hsetFail := newHSetFailSwitch(h.client)
	h.seed(8)
	a := newFlushFailureRelay("relay-a", 7)
	h.failFlush(a)

	hsetFail.on.Store(true)
	root, err := h.mgr.FlushTree(h.ctx, h.sessionID)
	hsetFail.on.Store(false)
	require.NoError(t, err, "the claim keeps its root from memory, as for a failed SET of the root")
	require.Len(t, root, SMSTRootLen)
	require.Nil(t, h.claimedRoot(), "claimed_root must not be stored while nodes under it are not in Redis")
	require.False(t, h.inRedis(a))
}
