//go:build test

package miner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// coldRelay is one relay of a synthetic tree: a key, ~700 B of value sharing a
// template the way serialized relays share most of their bytes, and a weight.
type coldRelay struct {
	key    []byte
	value  []byte
	weight uint64
}

func coldRelays(seed uint64, n int) []coldRelay {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	template := bytes.Repeat([]byte("relay-request-and-response-template/"), 16) // 576 B
	relays := make([]coldRelay, n)
	for i := range relays {
		tail := make([]byte, 124)
		for j := range tail {
			tail[j] = byte(rng.Uint32())
		}
		key := sha256.Sum256([]byte(fmt.Sprintf("cold-%d-%d", seed, i)))
		relays[i] = coldRelay{key: key[:], value: append(bytes.Clone(template), tail...), weight: uint64(1 + i%3)}
	}
	return relays
}

// claimColdTree builds a session's tree through the manager and flushes it, as
// the claim path does, and returns the claimed root.
func claimColdTree(t testing.TB, ctx context.Context, mgr *RedisSMSTManager, sessionID string, relays []coldRelay) []byte {
	t.Helper()
	for _, r := range relays {
		require.NoError(t, mgr.UpdateTree(ctx, sessionID, bytes.Clone(r.key), bytes.Clone(r.value), r.weight))
	}
	root, err := mgr.FlushTree(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, root, SMSTRootLen)
	return root
}

// chainVerifies runs the proof through the chain's own sequence
// (x/proof/keeper/proof_validation.go in poktroll v0.1.35): unmarshal the
// compact proof, decompact it with the protocol spec, verify it against root.
// It returns the decompacted proof so a caller can compare what it proves.
func chainVerifies(t testing.TB, proofBz, root []byte) (bool, *smt.SparseMerkleClosestProof) {
	t.Helper()
	compact := &smt.SparseCompactMerkleClosestProof{}
	require.NoError(t, compact.Unmarshal(proofBz))
	proof, err := smt.DecompactClosestProof(compact, protocol.NewSMTSpec())
	require.NoError(t, err)
	ok, err := smt.VerifyClosestProof(proof, root, protocol.NewSMTSpec())
	require.NoError(t, err)
	return ok, proof
}

// sameNamespaceClient is a second client on the namespace newTestRedis gave a
// test, so a hook can be added to it without touching the first.
func sameNamespaceClient(t testing.TB, prefix string) *redisutil.Client {
	t.Helper()
	client, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL:       testredis.URL(),
		Namespace: config.RedisNamespaceConfig{BasePrefix: prefix},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func coldPaths(seed uint64, n int) [][]byte {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	paths := make([][]byte, n)
	for i := range paths {
		paths[i] = make([]byte, 32)
		for j := range paths[i] {
			paths[i][j] = byte(rng.Uint32())
		}
	}
	return paths
}

func TestColdCompaction_AFailoverMinerProvesACompactedTreeAgainstTheClaimedRoot(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_v1", "sess-cold-v1"
	kb := client.KB()
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})

	relays := coldRelays(1, 2500)
	root := claimColdTree(t, ctx, mgr, sessionID, relays)

	// What the uncompacted tree proves for each path, to compare against.
	paths := coldPaths(7, 120)
	before := make([]*smt.SparseMerkleClosestProof, len(paths))
	for i, path := range paths {
		proofBz, err := mgr.ProveClosest(ctx, sessionID, path)
		require.NoError(t, err)
		ok, proof := chainVerifies(t, proofBz, root)
		require.True(t, ok, "control: the uncompacted tree's proof for path %d verifies", i)
		before[i] = proof
	}

	result, err := mgr.CompactColdTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, coldCompacted, result)
	require.False(t, keyExists(t, client, kb.SMSTNodesKey(supplier, sessionID)), "the nodes hash is deleted")
	require.True(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, sessionID)), "the leaves blob stays")
	require.Zero(t, mgr.GetTreeCount(), "the compacted tree is no longer resident")

	badRoot := bytes.Clone(root)
	badRoot[5] ^= 0x01
	leafValues := make(map[string]bool, len(relays))
	for _, r := range relays {
		leafValues[string(r.value)] = true
	}

	// A miner that never held the tree: the failover path, loadTreeFromRedis.
	failover := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier, CacheTTL: time.Hour, RebuildAdmission: NewRebuildAdmission(zerolog.Nop()),
	})
	for i, path := range paths {
		proofBz, err := failover.ProveClosest(ctx, sessionID, path)
		require.NoError(t, err, "LINK V1: path %d proves from the compacted tree", i)
		ok, proof := chainVerifies(t, proofBz, root)
		require.True(t, ok, "LINK V1: the proof from the compacted tree for path %d verifies against the claimed root", i)
		badOK, _ := chainVerifies(t, proofBz, badRoot)
		require.False(t, badOK, "control: path %d does not verify against a root with one bit changed", i)
		require.Equal(t, before[i].ClosestPath, proof.ClosestPath, "path %d proves the same leaf", i)
		require.Equal(t, before[i].ClosestValueHash, proof.ClosestValueHash, "path %d proves the same leaf value", i)
		require.True(t, leafValues[string(proof.GetValueHash(protocol.NewSMTSpec()))], "path %d proves one of the tree's relays", i)
	}

	// The manager that compacted it proves too, now from Redis.
	proofBz, err := mgr.ProveClosest(ctx, sessionID, paths[0])
	require.NoError(t, err)
	ok, _ := chainVerifies(t, proofBz, root)
	require.True(t, ok, "the compacting manager proves from the blob")

	// A resume (GetOrCreateTree, then the claim path's FlushTree again) keeps
	// the claimed root and still proves.
	resumer := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	_, err = resumer.GetOrCreateTree(ctx, sessionID)
	require.NoError(t, err)
	again, err := resumer.FlushTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, root, again, "a resumed compacted tree flushes to the claimed root")
	proofBz, err = resumer.ProveClosest(ctx, sessionID, paths[1])
	require.NoError(t, err)
	ok, _ = chainVerifies(t, proofBz, root)
	require.True(t, ok, "a resumed compacted tree proves")
	require.False(t, keyExists(t, client, kb.SMSTNodesKey(supplier, sessionID)), "nothing wrote nodes back to Redis")
}

// TestColdCompaction_ATreeClaimedBeforeCompactionExistedIsProvedFromItsHash is
// the upgrade: a binary without compaction claimed the tree, so Redis holds its
// nodes hash and claimed_root and no blob. The new binary must prove it from the
// hash, and must not create a blob or delete the hash on the way.
func TestColdCompaction_ATreeClaimedBeforeCompactionExistedIsProvedFromItsHash(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_upgrade", "sess-cold-upgrade"
	kb := client.KB()
	old := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	root := claimColdTree(t, ctx, old, sessionID, coldRelays(16, 400))
	require.True(t, keyExists(t, client, kb.SMSTRootKey(supplier, sessionID)), "control: the claimed_root is stored")
	hashKey := kb.SMSTNodesKey(supplier, sessionID)
	lenBefore, err := client.HLen(ctx, hashKey).Result()
	require.NoError(t, err)
	require.Positive(t, lenBefore, "control: the nodes hash is stored")

	rebuilds := testutil.ToFloat64(observability.SMSTColdRebuilds.WithLabelValues(supplier, "missing")) +
		testutil.ToFloat64(observability.SMSTColdRebuilds.WithLabelValues(supplier, "ok"))
	upgraded := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	badRoot := bytes.Clone(root)
	badRoot[7] ^= 0x01
	for i, path := range coldPaths(17, 20) {
		proofBz, err := upgraded.ProveClosest(ctx, sessionID, path)
		require.NoError(t, err, "LINK upgrade: path %d of a tree without a blob proves from its hash", i)
		ok, _ := chainVerifies(t, proofBz, root)
		require.True(t, ok, "LINK upgrade: path %d verifies against the claimed root", i)
		badOK, _ := chainVerifies(t, proofBz, badRoot)
		require.False(t, badOK, "control: path %d does not verify against a root with one bit changed", i)
	}

	require.False(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, sessionID)), "proving creates no blob")
	lenAfter, err := client.HLen(ctx, hashKey).Result()
	require.NoError(t, err)
	require.Equal(t, lenBefore, lenAfter, "proving leaves the nodes hash whole")
	require.Equal(t, rebuilds,
		testutil.ToFloat64(observability.SMSTColdRebuilds.WithLabelValues(supplier, "missing"))+
			testutil.ToFloat64(observability.SMSTColdRebuilds.WithLabelValues(supplier, "ok")),
		"no proof went through a rebuild")
}

func TestColdCompaction_ConcurrentProofsOfACompactedTreeAllVerify(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_concurrent", "sess-cold-concurrent"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	root := claimColdTree(t, ctx, mgr, sessionID, coldRelays(2, 600))
	result, err := mgr.CompactColdTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, coldCompacted, result)

	failover := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier, CacheTTL: time.Hour, RebuildAdmission: NewRebuildAdmission(zerolog.Nop()),
	})
	paths := coldPaths(3, 16)
	proofs := make([][]byte, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			proofs[i], errs[i] = failover.ProveClosest(ctx, sessionID, paths[i])
		}()
	}
	wg.Wait()
	for i := range paths {
		require.NoError(t, errs[i], "path %d", i)
		ok, _ := chainVerifies(t, proofs[i], root)
		require.True(t, ok, "path %d", i)
	}
}

// findLeafField returns the hash field and bytes of one leaf node of a stored tree.
// findLeafField returns one leaf of the hash, DECOMPRESSED.
//
// It reads the stored bytes directly, so it is one of the readers that knows
// how a node is stored -- and it has to decompress like the store does, or it
// matches nothing: a leaf is stored as a zstd frame whose first byte is 0x28,
// never 0x00 (item 398, smst_node_codec.go). This is the test scaffolding
// reading the format without declaring it, which is why the assertion above it
// went red the moment production started compressing.
func findLeafField(t *testing.T, client *redisutil.Client, hashKey string) (string, []byte) {
	t.Helper()
	all, err := client.HGetAll(context.Background(), hashKey).Result()
	require.NoError(t, err)
	for field, stored := range all {
		node, decErr := decompressNode([]byte(stored))
		require.NoError(t, decErr, "field %s of %s", field, hashKey)
		if len(node) > 1+coldLeafPathLen+coldLeafMetaLen && node[0] == 0 {
			return field, node
		}
	}
	t.Fatalf("no leaf node in %s", hashKey)
	return "", nil
}

func TestColdCompaction_LeavesThatDoNotRebuildTheClaimedRootKeepTheNodesHash(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, client *redisutil.Client, hashKey string)
	}{
		{
			name: "a leaf value changed",
			tamper: func(t *testing.T, client *redisutil.Client, hashKey string) {
				field, node := findLeafField(t, client, hashKey)
				node[1+coldLeafPathLen+3] ^= 0xff
				// Written the way production writes it, so the tampering is a
				// changed LEAF and not a changed encoding.
				require.NoError(t, client.HSet(context.Background(), hashKey, field, compressNode(node)).Err())
			},
		},
		{
			name: "a leaf missing",
			tamper: func(t *testing.T, client *redisutil.Client, hashKey string) {
				field, _ := findLeafField(t, client, hashKey)
				require.NoError(t, client.HDel(context.Background(), hashKey, field).Err())
			},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			client, _ := newTestRedis(t)
			supplier := fmt.Sprintf("pokt1cold_v2_%d", i)
			const sessionID = "sess-cold-v2"
			kb := client.KB()
			mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
			claimColdTree(t, ctx, mgr, sessionID, coldRelays(4, 300))

			hashKey := kb.SMSTNodesKey(supplier, sessionID)
			tc.tamper(t, client, hashKey)
			lenBefore, err := client.HLen(ctx, hashKey).Result()
			require.NoError(t, err)
			mismatches := testutil.ToFloat64(observability.SMSTColdCompactions.WithLabelValues(supplier, string(coldMismatch)))

			result, err := mgr.CompactColdTree(ctx, sessionID)
			lenAfter, lenErr := client.HLen(ctx, hashKey).Result()
			require.NoError(t, lenErr)
			require.Equal(t, lenBefore, lenAfter, "LINK V2: the nodes hash is kept whole")
			require.Error(t, err)
			require.Equal(t, coldMismatch, result, "LINK V2: leaves that do not rebuild the claimed root are a mismatch")
			require.False(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, sessionID)), "the mismatched blob is removed")
			require.Equal(t, mismatches+1,
				testutil.ToFloat64(observability.SMSTColdCompactions.WithLabelValues(supplier, string(coldMismatch))))
			require.False(t, result.retryable(), "a mismatch is not retried")
		})
	}
}

// truncateSet stores a shortened copy of the value SET on one key: what a write
// that did not land whole leaves behind.
type truncateSet struct{ key string }

func (f *truncateSet) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *truncateSet) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); cmd.Name() == "set" && len(args) > 2 && args[1] == f.key {
			if value, ok := args[2].([]byte); ok && len(value) > coldLeavesHeaderLen+8 {
				args[2] = bytes.Clone(value[:len(value)-8])
			}
		}
		return next(ctx, cmd)
	}
}

func (f *truncateSet) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestColdCompaction_TheBlobIsVerifiedAsStoredNotAsSent(t *testing.T) {
	ctx := context.Background()
	client, prefix := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_readback", "sess-cold-readback"
	kb := client.KB()
	builder := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	claimColdTree(t, ctx, builder, sessionID, coldRelays(15, 300))

	truncating := sameNamespaceClient(t, prefix)
	truncating.AddHook(&truncateSet{key: kb.SMSTLeavesKey(supplier, sessionID)})
	mgr := NewRedisSMSTManager(zerolog.Nop(), truncating, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})

	hashKey := kb.SMSTNodesKey(supplier, sessionID)
	lenBefore, err := client.HLen(ctx, hashKey).Result()
	require.NoError(t, err)
	result, err := mgr.CompactColdTree(ctx, sessionID)
	lenAfter, lenErr := client.HLen(ctx, hashKey).Result()
	require.NoError(t, lenErr)
	require.Equal(t, lenBefore, lenAfter, "LINK V2-readback: a blob stored damaged does not let the nodes hash go")
	require.Error(t, err)
	require.Equal(t, coldMismatch, result)
	require.False(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, sessionID)), "the damaged blob is removed")
}

func TestColdCompaction_AProofFromABlobThatDoesNotRebuildTheClaimedRootFails(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_badblob", "sess-cold-badblob"
	kb := client.KB()
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	root := claimColdTree(t, ctx, mgr, sessionID, coldRelays(5, 300))
	result, err := mgr.CompactColdTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, coldCompacted, result)

	// Same header and root, one leaf value changed.
	leavesKey := kb.SMSTLeavesKey(supplier, sessionID)
	blob, err := client.Get(ctx, leavesKey).Bytes()
	require.NoError(t, err)
	_, leaves, err := decodeColdLeaves(blob)
	require.NoError(t, err)
	leaves[len(leaves)/2].value = bytes.Clone(leaves[len(leaves)/2].value)
	leaves[len(leaves)/2].value[0] ^= 0xff
	tampered, err := encodeColdLeaves(root, leaves)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, leavesKey, tampered, time.Hour).Err())

	mismatches := testutil.ToFloat64(observability.SMSTColdRebuilds.WithLabelValues(supplier, "mismatch"))
	failover := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	proofBz, err := failover.ProveClosest(ctx, sessionID, coldPaths(9, 1)[0])
	require.ErrorIs(t, err, errColdRootMismatch, "LINK V2-prove: a rebuild that is not the claimed root yields no proof")
	require.Nil(t, proofBz)
	require.Equal(t, mismatches+1, testutil.ToFloat64(observability.SMSTColdRebuilds.WithLabelValues(supplier, "mismatch")))
}

// setOOM rejects SET of one key with Redis's own maxmemory reply.
type setOOM struct {
	key string
	on  atomic.Bool
}

const redisOOMReply = "OOM command not allowed when used memory > 'maxmemory'."

func (f *setOOM) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *setOOM) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); f.on.Load() && cmd.Name() == "set" && len(args) > 1 && args[1] == f.key {
			err := errors.New(redisOOMReply)
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

func (f *setOOM) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestColdCompaction_ARejectedSetKeepsTheNodesHashAndIsRetried(t *testing.T) {
	ctx := context.Background()
	client, prefix := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_oom", "sess-cold-oom"
	kb := client.KB()
	builder := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	root := claimColdTree(t, ctx, builder, sessionID, coldRelays(6, 400))

	oomClient := sameNamespaceClient(t, prefix)
	oom := &setOOM{key: kb.SMSTLeavesKey(supplier, sessionID)}
	oom.on.Store(true)
	oomClient.AddHook(oom)

	mgr := NewRedisSMSTManager(zerolog.Nop(), oomClient, RedisSMSTManagerConfig{
		SupplierAddress: supplier, CacheTTL: time.Hour,
	})
	var retries []func()
	mgr.coldAfterFunc = func(d time.Duration, f func()) {
		require.Equal(t, coldCompactionRetryDelay, d)
		retries = append(retries, f)
	}

	hashKey := kb.SMSTNodesKey(supplier, sessionID)
	lenBefore, err := client.HLen(ctx, hashKey).Result()
	require.NoError(t, err)
	setFailed := testutil.ToFloat64(observability.SMSTColdCompactions.WithLabelValues(supplier, string(coldSetFailed)))

	mgr.ScheduleColdCompaction(ctx, sessionID)

	lenAfter, err := client.HLen(ctx, hashKey).Result()
	require.NoError(t, err)
	require.Equal(t, lenBefore, lenAfter, "LINK V3: a rejected SET leaves the nodes hash whole")
	require.False(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, sessionID)))
	require.Equal(t, setFailed+1, testutil.ToFloat64(observability.SMSTColdCompactions.WithLabelValues(supplier, string(coldSetFailed))))
	require.Len(t, retries, 1, "LINK V3-retry: a rejected SET schedules another attempt")

	oom.on.Store(false)
	retries[0]()
	require.False(t, keyExists(t, client, hashKey), "LINK V3-retry: the retry compacts once the SET goes through")
	require.True(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, sessionID)))
	require.Len(t, retries, 1, "a compaction that succeeded schedules nothing more")

	proofBz, err := mgr.ProveClosest(ctx, sessionID, coldPaths(10, 1)[0])
	require.NoError(t, err)
	ok, _ := chainVerifies(t, proofBz, root)
	require.True(t, ok)
}

func TestColdCompaction_RetriesStopAtTheAttemptLimit(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_limit", "sess-cold-limit"
	// No claimed_root: every attempt is not_ready, which is retryable.
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	var pending []func()
	mgr.coldAfterFunc = func(_ time.Duration, f func()) { pending = append(pending, f) }
	before := testutil.ToFloat64(observability.SMSTColdCompactions.WithLabelValues(supplier, string(coldNotReady)))

	mgr.ScheduleColdCompaction(ctx, sessionID)
	for attempts := 1; len(pending) > 0; attempts++ {
		require.Less(t, attempts, coldCompactionAttempts, "no retry is scheduled past the attempt limit")
		next := pending[0]
		pending = pending[1:]
		next()
	}
	require.Equal(t, before+coldCompactionAttempts,
		testutil.ToFloat64(observability.SMSTColdCompactions.WithLabelValues(supplier, string(coldNotReady))))
}

func TestColdCompaction_AManagerWithNoSettingsCompactsEveryClaimedTree(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_always", "sess-cold-always"
	kb := client.KB()
	// Only what identifies the supplier: nothing in the config can turn it off.
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	claimColdTree(t, ctx, mgr, sessionID, coldRelays(11, 50))

	mgr.ScheduleColdCompaction(ctx, sessionID)
	require.False(t, keyExists(t, client, kb.SMSTNodesKey(supplier, sessionID)),
		"LINK always: a scheduled compaction deletes the nodes hash with no setting")
	require.True(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, sessionID)), "and leaves the blob")
}

// unlinkAfterExists deletes a nodes hash right after the pipeline that checked
// for it, the window in which a compaction elsewhere can delete it.
type unlinkAfterExists struct {
	nodesKey string
	plain    *redis.Client
	armed    atomic.Bool
}

func (f *unlinkAfterExists) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *unlinkAfterExists) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (f *unlinkAfterExists) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			if args := cmd.Args(); cmd.Name() == "exists" && len(args) > 1 && args[1] == f.nodesKey && f.armed.CompareAndSwap(true, false) {
				if unlinkErr := f.plain.Unlink(ctx, f.nodesKey).Err(); unlinkErr != nil {
					return unlinkErr
				}
			}
		}
		return err
	}
}

func TestColdCompaction_AProofThatLosesTheNodesHashMidWalkIsProvedFromTheBlob(t *testing.T) {
	ctx := context.Background()
	client, prefix := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_race", "sess-cold-race"
	kb := client.KB()
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	root := claimColdTree(t, ctx, mgr, sessionID, coldRelays(12, 300))

	// The blob a compaction stores, while the hash is still there.
	leaves, _, err := mgr.readColdLeaves(ctx, kb.SMSTNodesKey(supplier, sessionID))
	require.NoError(t, err)
	blob, err := encodeColdLeaves(root, leaves)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, kb.SMSTLeavesKey(supplier, sessionID), blob, time.Hour).Err())

	hooked := sameNamespaceClient(t, prefix)
	race := &unlinkAfterExists{nodesKey: kb.SMSTNodesKey(supplier, sessionID), plain: testredis.Client(t)}
	race.armed.Store(true)
	hooked.AddHook(race)

	prover := NewRedisSMSTManager(zerolog.Nop(), hooked, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	rebuilds := testutil.ToFloat64(observability.SMSTColdRebuilds.WithLabelValues(supplier, "ok"))
	proofBz, err := prover.ProveClosest(ctx, sessionID, coldPaths(13, 1)[0])
	require.False(t, race.armed.Load(), "control: the hash was deleted between the check and the walk")
	require.NoError(t, err, "LINK race: a hash deleted mid-proof falls back to the blob")
	ok, _ := chainVerifies(t, proofBz, root)
	require.True(t, ok)
	require.Equal(t, rebuilds+1, testutil.ToFloat64(observability.SMSTColdRebuilds.WithLabelValues(supplier, "ok")))
}

func TestColdCompaction_TheBlobLivesAndDiesWithTheClaimedRoot(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1cold_ttl", "sess-cold-ttl"
	kb := client.KB()
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	claimColdTree(t, ctx, mgr, sessionID, coldRelays(14, 100))
	result, err := mgr.CompactColdTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, coldCompacted, result)
	leavesKey := kb.SMSTLeavesKey(supplier, sessionID)
	requireTTLNear(t, client, leavesKey, time.Hour)

	// A failover load refreshes it with the claimed_root.
	require.NoError(t, client.Expire(ctx, leavesKey, time.Minute).Err())
	failover := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	_, err = failover.GetTreeRoot(ctx, sessionID)
	require.NoError(t, err)
	ttl, err := client.PTTL(ctx, leavesKey).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 59*time.Minute, "LINK ttl: a failover load refreshes the leaves blob's TTL with the claimed_root's")

	require.NoError(t, failover.SetTreeTTL(ctx, sessionID, 2*time.Minute))
	requireTTLNear(t, client, leavesKey, 2*time.Minute)

	require.NoError(t, failover.DeleteTree(ctx, sessionID))
	require.False(t, keyExists(t, client, leavesKey), "LINK delete: DeleteTree removes the leaves blob")
}

func TestColdLeavesBlob_RejectsWhatItDidNotWrite(t *testing.T) {
	root := make([]byte, SMSTRootLen)
	binary.BigEndian.PutUint64(root[32:40], 3)
	binary.BigEndian.PutUint64(root[40:48], 2)
	leaves := []coldLeaf{
		{path: bytes.Repeat([]byte{2}, 32), value: []byte("second"), weight: 2},
		{path: bytes.Repeat([]byte{1}, 32), value: []byte("first"), weight: 1},
	}
	blob, err := encodeColdLeaves(root, leaves)
	require.NoError(t, err)

	gotRoot, got, err := decodeColdLeaves(blob)
	require.NoError(t, err)
	require.Equal(t, root, gotRoot)
	require.Len(t, got, 2)
	require.Equal(t, bytes.Repeat([]byte{1}, 32), got[0].path, "leaves are stored sorted by path")
	require.Equal(t, []byte("first"), got[0].value)
	require.Equal(t, uint64(1), got[0].weight)
	require.Equal(t, bytes.Repeat([]byte{2}, 32), got[1].path)
	require.Equal(t, []byte("second"), got[1].value)
	require.Equal(t, uint64(2), got[1].weight)
	require.Equal(t, len(got[0].value), cap(got[0].value), "a decoded value cannot be appended into its neighbour")

	for name, mutate := range map[string]func([]byte) []byte{
		"short":          func(b []byte) []byte { return b[:coldLeavesHeaderLen-1] },
		"version":        func(b []byte) []byte { b[0] = 9; return b },
		"codec":          func(b []byte) []byte { b[1] = 9; return b },
		"count":          func(b []byte) []byte { b[9]++; return b },
		"sum":            func(b []byte) []byte { b[17]++; return b },
		"frame":          func(b []byte) []byte { return b[:len(b)-3] },
		"root count":     func(b []byte) []byte { b[18+47]++; return b },
		"no frame bytes": func(b []byte) []byte { return b[:coldLeavesHeaderLen] },
	} {
		_, _, err := decodeColdLeaves(mutate(bytes.Clone(blob)))
		require.Error(t, err, name)
	}
	_, err = encodeColdLeaves(root[:10], leaves)
	require.Error(t, err, "a root of the wrong length is refused")
}

// coldSchedulingSMST records the sessions the claim path asks to compact.
type coldSchedulingSMST struct {
	smstStub
	mu        sync.Mutex
	scheduled []string
}

func (s *coldSchedulingSMST) ScheduleColdCompaction(_ context.Context, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduled = append(s.scheduled, sessionID)
}

// TestOnSessionsNeedClaim_ASentClaimCompactsItsRealTree runs the claim path
// against a real RedisSMSTManager, so the claim's own FlushTree root is what
// the compaction checks, and nothing but the claim asks for it.
func TestOnSessionsNeedClaim_ASentClaimCompactsItsRealTree(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier = "pokt1coldrealclaim"
	kb := client.KB()
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})

	blocks := &heightedBlocks{}
	blocks.currentHeight = 103
	lc := &LifecycleCallback{
		logger:         logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:   &defaultParamsShared{},
		blockClient:    blocks,
		smstManager:    mgr,
		supplierClient: acceptingSupplier{},
		serviceClient:  erroringService{},
		config:         LifecycleCallbackConfig{ClaimRetryAttempts: 1},
	}
	sessions := []string{"session-coldreal-a-0000", "session-coldreal-b-0000"}
	snapshots := make([]*SessionSnapshot, 0, len(sessions))
	for i, id := range sessions {
		relays := coldRelays(uint64(30+i), 40)
		for _, r := range relays {
			require.NoError(t, mgr.UpdateTree(ctx, id, bytes.Clone(r.key), bytes.Clone(r.value), r.weight))
		}
		snapshots = append(snapshots, &SessionSnapshot{
			SessionID: id, SessionEndHeight: 100, SessionStartHeight: 81,
			SupplierOperatorAddress: supplier, ServiceID: "svc",
			RelayCount: int64(len(relays)), TotalComputeUnits: 80, State: SessionStateClaiming,
		})
	}

	result, err := lc.OnSessionsNeedClaim(ctx, snapshots)
	require.NoError(t, err)
	for _, id := range sessions {
		require.True(t, result.IsClaimed(id), "control: session %s was claimed", id)
		require.False(t, keyExists(t, client, kb.SMSTNodesKey(supplier, id)),
			"LINK claim-compacts: the claim of %s compacts its tree", id)
		require.True(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, id)))
	}
}

func TestOnSessionsNeedClaim_ASentClaimSchedulesItsTreeForCompaction(t *testing.T) {
	blocks := &heightedBlocks{}
	blocks.currentHeight = 103
	smst := &coldSchedulingSMST{}
	lc := &LifecycleCallback{
		logger:         logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:   &defaultParamsShared{},
		blockClient:    blocks,
		smstManager:    smst,
		supplierClient: acceptingSupplier{},
		serviceClient:  erroringService{},
		config:         LifecycleCallbackConfig{ClaimRetryAttempts: 1},
	}
	want := []string{"session-cold-a-0000", "session-cold-b-0000"}
	snapshots := make([]*SessionSnapshot, 0, len(want))
	for _, id := range want {
		snapshots = append(snapshots, &SessionSnapshot{
			SessionID: id, SessionEndHeight: 100, SessionStartHeight: 81,
			SupplierOperatorAddress: "pokt1coldclaim", ServiceID: "svc",
			RelayCount: 10, TotalComputeUnits: 100, State: SessionStateClaiming,
		})
	}

	_, err := lc.OnSessionsNeedClaim(context.Background(), snapshots)
	require.NoError(t, err)
	smst.mu.Lock()
	defer smst.mu.Unlock()
	require.ElementsMatch(t, want, smst.scheduled, "LINK schedule: every sent claim schedules its tree")
}
