//go:build test

package miner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// The tree is committed once per relay batch, not once per relay. Every writer
// of a root, and every acknowledgement outside the batch, has to find the nodes
// it depends on already in Redis.

// requireStoredTreeHolds resumes the session's tree in a manager that never held
// it -- from claimed_root, else live_root, as a restarted or promoted miner does
// -- and reads every relay back through it. A read walks the stored nodes from
// the root down, so a node the root references and Redis lacks fails it;
// counting leaves from the root's digest would not see the gap.
func requireStoredTreeHolds(t *testing.T, client *redisutil.Client, supplier, sessionID string, relays []flushFailureRelay, why string) {
	t.Helper()
	fresh := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	tree, err := fresh.GetOrCreateTree(context.Background(), sessionID)
	require.NoError(t, err)
	require.True(t, tree.claimedRoot != nil || tree.liveRoot != nil, "%s: there must be a stored root to resume from", why)
	for _, r := range relays {
		value, _, err := tree.trie.Get(r.key)
		require.NoError(t, err, "%s: relay %x must be readable from the stored root", why, r.key[:4])
		require.Equal(t, r.value, value, "%s: relay %x", why, r.key[:4])
	}
}

func updateRelays(t *testing.T, ctx context.Context, mgr *RedisSMSTManager, sessionID, label string, n int) []flushFailureRelay {
	t.Helper()
	relays := make([]flushFailureRelay, n)
	for i := range relays {
		relays[i] = newFlushFailureRelay(fmt.Sprintf("%s-%d", label, i), uint64(i+1))
		require.NoError(t, relays[i].update(ctx, mgr, sessionID))
	}
	return relays
}

func TestBatchCommit_UpdatesWriteNothingAndTheCheckpointStoresEveryNode(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_commit", "sess-batch-commit"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})

	// Twelve updates: past the first-update and every-ten checkpoints the tree
	// used to write on its own.
	relays := updateRelays(t, ctx, mgr, sessionID, "batch-commit", 12)
	written, err := client.Exists(ctx,
		client.KB().SMSTNodesKey(supplier, sessionID),
		client.KB().SMSTLiveRootKey(supplier, sessionID),
	).Result()
	require.NoError(t, err)
	require.Zero(t, written, "an update writes nothing: neither nodes nor live_root before the batch commits")

	resident, _, err := mgr.CheckpointLiveRoot(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, resident)
	requireStoredTreeHolds(t, client, supplier, sessionID, relays, "after CheckpointLiveRoot")
}

func TestBatchCommit_ACommitLeavesTheStoredLiveRootReadable(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_commit_orphans", "sess-batch-commit-orphans"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})

	relays := updateRelays(t, ctx, mgr, sessionID, "orphans", 8)
	resident, _, err := mgr.CheckpointLiveRoot(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, resident)

	// Replacing a leaf and adding others orphans nodes the stored live_root
	// still references. The commit that follows must not delete them before a
	// new live_root stops referencing them.
	replaced := relays[0]
	replaced.value = []byte("a replaced value for the same key")
	require.NoError(t, replaced.update(ctx, mgr, sessionID))
	updateRelays(t, ctx, mgr, sessionID, "orphans-more", 4)
	resident, err = mgr.CommitTree(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, resident)

	requireStoredTreeHolds(t, client, supplier, sessionID, relays, "a commit without a new live_root")
}

func TestBatchCommit_EveryRootWriterStoresTheNodesUnderIt(t *testing.T) {
	writers := []struct {
		name  string
		write func(t *testing.T, ctx context.Context, mgr *RedisSMSTManager, sessionID string)
	}{
		{"live_root", func(t *testing.T, ctx context.Context, mgr *RedisSMSTManager, sessionID string) {
			resident, _, err := mgr.CheckpointLiveRoot(ctx, sessionID)
			require.NoError(t, err)
			require.True(t, resident)
		}},
		{"exit_live_root", func(t *testing.T, ctx context.Context, mgr *RedisSMSTManager, sessionID string) {
			written, err := mgr.CheckpointLiveRootOnExit(ctx, sessionID)
			require.NoError(t, err)
			require.True(t, written)
		}},
		{"exit_all", func(t *testing.T, ctx context.Context, mgr *RedisSMSTManager, sessionID string) {
			written, failed, err := mgr.CheckpointAllOnExit(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, written)
			require.Zero(t, failed)
		}},
		{"claimed_root", func(t *testing.T, ctx context.Context, mgr *RedisSMSTManager, sessionID string) {
			_, err := mgr.FlushTree(ctx, sessionID)
			require.NoError(t, err)
		}},
	}
	for _, writer := range writers {
		t.Run(writer.name, func(t *testing.T) {
			ctx := context.Background()
			client, _ := newTestRedis(t)
			supplier := "pokt1root_writer_" + writer.name
			const sessionID = "sess-root-writer"
			mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})

			relays := updateRelays(t, ctx, mgr, sessionID, "root-writer", 6)
			writer.write(t, ctx, mgr, sessionID)
			requireStoredTreeHolds(t, client, supplier, sessionID, relays, writer.name)
		})
	}
}

func TestHandleRelay_ARelayOutsideTheBatchIsStoredBeforeItIsAcknowledged(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, "pokt1outside_batch", "consumer-a")
	// A supplier whose relays are finished one at a time.
	w.state.relayBatch = nil
	const sessionID, payload = "sess-outside-batch", "outside-batch-0"
	ids := w.publish(1)

	require.True(t, w.deliver(w.msg(ids[0], sessionID, payload, 100)), "CONTROL: without a batch the delivery acknowledges")

	hash := sha256.Sum256([]byte(payload))
	leaf := flushFailureRelay{key: hash[:], value: []byte(payload), weight: 100}
	stored, err := client.HExists(w.ctx, client.KB().SMSTNodesKey(w.supplier, sessionID), leaf.leafField()).Result()
	require.NoError(t, err)
	require.True(t, stored, "the acknowledged relay's leaf must already be in Redis")
}

func TestRedisMapStore_ALargeNodesWriteIsSplitAndFullyWritten(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	counter := newNamedCommandCounter(client)
	const supplier, sessionID = "pokt1nodes_chunks", "sess-nodes-chunks"
	store, ok := NewRedisMapStore(ctx, client, supplier, sessionID).(*RedisMapStore)
	require.True(t, ok)

	// About 600 KiB of values: more than two nodesWriteChunkBytes.
	const nodes = 600
	value := bytes.Repeat([]byte("v"), 1024)
	store.BeginPipeline()
	for i := 0; i < nodes; i++ {
		require.NoError(t, store.Set([]byte(fmt.Sprintf("node-%d", i)), value))
	}
	counter.take()
	require.NoError(t, store.FlushPipeline())
	sent := counter.take()
	require.Greater(t, sent["hset"], 1, "CONTROL: a write this large goes as more than one HSET, sent %v", sent)

	written, err := client.HLen(ctx, client.KB().SMSTNodesKey(supplier, sessionID)).Result()
	require.NoError(t, err)
	require.EqualValues(t, nodes, written, "every node of every piece must be written")
}

// hsetBytesRecorder records how many field and value bytes each HSET a client
// sends carries, piped or not.
type hsetBytesRecorder struct {
	mu    sync.Mutex
	sizes []int
}

func newHSetBytesRecorder(client redis.UniversalClient) *hsetBytesRecorder {
	r := &hsetBytesRecorder{}
	client.AddHook(r)
	return r
}

func (r *hsetBytesRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *hsetBytesRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.record(cmd)
		return next(ctx, cmd)
	}
}

func (r *hsetBytesRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			r.record(cmd)
		}
		return next(ctx, cmds)
	}
}

func (r *hsetBytesRecorder) record(cmd redis.Cmder) {
	if cmd.Name() != "hset" {
		return
	}
	size := 0
	for _, arg := range cmd.Args()[2:] {
		switch v := arg.(type) {
		case string:
			size += len(v)
		case []byte:
			size += len(v)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sizes = append(r.sizes, size)
}

// take returns the sizes recorded since the last take, and forgets them.
func (r *hsetBytesRecorder) take() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.sizes
	r.sizes = nil
	return out
}

// A full relay batch of nodes must not reach Redis as a few large HSETs: each
// one runs start to end on Redis's single thread while the relayer waits. The
// bound is the 32 KiB piece chosen against the relayer's measured latency.
func TestRedisMapStore_AFullRelayBatchOfNodesGoesInPiecesOfAtMost32KiB(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	recorder := newHSetBytesRecorder(client)
	const supplier, sessionID = "pokt1nodes_32k_pieces", "sess-nodes-32k-pieces"
	store, ok := NewRedisMapStore(ctx, client, supplier, sessionID).(*RedisMapStore)
	require.True(t, ok)

	// relayBatchCap leaves of about 1 KiB each.
	const leaves = relayBatchCap
	value := bytes.Repeat([]byte("v"), 1024)
	store.BeginPipeline()
	for i := 0; i < leaves; i++ {
		require.NoError(t, store.Set([]byte(fmt.Sprintf("node-%04d", i)), value))
	}
	recorder.take()
	require.NoError(t, store.FlushPipeline())
	sizes := recorder.take()

	for i, size := range sizes {
		require.LessOrEqual(t, size, 32<<10, "HSET %d of %d carries %d bytes, more than a 32 KiB piece", i, len(sizes), size)
	}
	require.Greater(t, len(sizes), 20, "a full batch of ~1 KiB nodes must go as many HSETs, sent %d", len(sizes))

	written, err := client.HLen(ctx, client.KB().SMSTNodesKey(supplier, sessionID)).Result()
	require.NoError(t, err)
	require.EqualValues(t, leaves, written, "every node of every piece must be written")
}

func TestRelayBatch_AnAcknowledgedBatchIsReadableFromItsLiveRoot(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, "pokt1batch_readable", "consumer-a")
	const sessionID = "sess-batch-readable"
	ids := w.publish(3)

	relays := make([]flushFailureRelay, len(ids))
	for i, id := range ids {
		payload := fmt.Sprintf("batch-readable-%d", i)
		w.deliver(w.msg(id, sessionID, payload, 100))
		hash := sha256.Sum256([]byte(payload))
		relays[i] = flushFailureRelay{key: hash[:], value: []byte(payload), weight: 100}
	}
	w.batch.FlushAll(w.ctx)
	require.Zero(t, w.pending(), "CONTROL: the batch is acknowledged")

	requireStoredTreeHolds(t, client, w.supplier, sessionID, relays, "after the batch flush")
}
