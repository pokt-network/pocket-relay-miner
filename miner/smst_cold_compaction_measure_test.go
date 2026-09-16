//go:build test

package miner

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// These two do not run in the gates. The first builds trees of up to 100k
// leaves and reports sizes and times; the second lowers maxmemory, which on the
// shared test Redis would break every other package running against it. Both
// are meant for a disposable server: REDIS_TEST_URL pointing at one.

// TestColdCompaction_MeasureSizesAndTimes reports, per synthetic tree size, the
// nodes hash's MEMORY USAGE against the leaves blob's, the time to compact, and
// the time a miner that never held the tree takes to prove from the blob.
// Synthetic relays share a 576 B template, so the blob ratio is not a real
// relay's.
func TestColdCompaction_MeasureSizesAndTimes(t *testing.T) {
	if os.Getenv("PRM_COLD_MEASURE") != "1" {
		t.Skip("set PRM_COLD_MEASURE=1 (and REDIS_TEST_URL to a disposable server) to measure")
	}
	sizes := []int{5000, 50000, 100000}
	if s := os.Getenv("PRM_COLD_MEASURE_SIZES"); s != "" {
		sizes = nil
		for _, f := range strings.Split(s, ",") {
			n, err := strconv.Atoi(f)
			require.NoError(t, err)
			sizes = append(sizes, n)
		}
	}
	ctx := context.Background()
	client, _ := newTestRedis(t)
	kb := client.KB()
	for _, n := range sizes {
		supplier := "pokt1cold_measure_" + strconv.Itoa(n)
		const sessionID = "sess-cold-measure"
		mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
		root := claimColdTree(t, ctx, mgr, sessionID, coldRelays(uint64(n), n))
		hashLen, err := client.HLen(ctx, kb.SMSTNodesKey(supplier, sessionID)).Result()
		require.NoError(t, err)
		hashMU, err := client.MemoryUsage(ctx, kb.SMSTNodesKey(supplier, sessionID), 0).Result()
		require.NoError(t, err)

		start := time.Now()
		result, err := mgr.CompactColdTree(ctx, sessionID)
		compactTook := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, coldCompacted, result)
		blobLen, err := client.StrLen(ctx, kb.SMSTLeavesKey(supplier, sessionID)).Result()
		require.NoError(t, err)
		blobMU, err := client.MemoryUsage(ctx, kb.SMSTLeavesKey(supplier, sessionID), 0).Result()
		require.NoError(t, err)

		failover := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
		start = time.Now()
		proofBz, err := failover.ProveClosest(ctx, sessionID, coldPaths(uint64(n), 1)[0])
		proveTook := time.Since(start)
		require.NoError(t, err)
		ok, _ := chainVerifies(t, proofBz, root)
		require.True(t, ok)

		t.Logf("V5 SYNTHETIC leaves=%d hash_fields=%d hash_memory_usage=%d blob_bytes=%d blob_memory_usage=%d ratio=%.4f compact=%s prove_from_blob=%s",
			n, hashLen, hashMU, blobLen, blobMU, float64(blobMU)/float64(hashMU), compactTook, proveTook)
		require.NoError(t, failover.DeleteTree(ctx, sessionID))
	}
}

// TestColdCompaction_RealMaxmemoryRefusesTheBlobAndKeepsTheHash sets maxmemory
// just above what the server holds once the tree is stored, so the blob's SET
// is refused by Redis itself; then lifts it and compacts.
func TestColdCompaction_RealMaxmemoryRefusesTheBlobAndKeepsTheHash(t *testing.T) {
	if os.Getenv("PRM_COLD_REAL_MAXMEMORY") != "1" {
		t.Skip("set PRM_COLD_REAL_MAXMEMORY=1 and REDIS_TEST_URL to a disposable server: this changes maxmemory")
	}
	ctx := context.Background()
	client, _ := newTestRedis(t)
	kb := client.KB()
	const supplier, sessionID = "pokt1cold_maxmemory", "sess-cold-maxmemory"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	root := claimColdTree(t, ctx, mgr, sessionID, coldRelays(21, 3000))
	hashKey := kb.SMSTNodesKey(supplier, sessionID)
	lenBefore, err := client.HLen(ctx, hashKey).Result()
	require.NoError(t, err)

	info, err := client.InfoMap(ctx, "memory").Result()
	require.NoError(t, err)
	used, err := strconv.ParseInt(info["Memory"]["used_memory"], 10, 64)
	require.NoError(t, err)
	require.NoError(t, client.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err())
	require.NoError(t, client.ConfigSet(ctx, "maxmemory", strconv.FormatInt(used+64<<10, 10)).Err())
	t.Cleanup(func() { _ = client.ConfigSet(context.Background(), "maxmemory", "0").Err() })

	result, err := mgr.CompactColdTree(ctx, sessionID)
	t.Logf("under maxmemory: result=%s err=%v", result, err)
	require.Equal(t, coldSetFailed, result, "LINK V3-real: Redis refuses the blob under maxmemory")
	require.ErrorContains(t, err, "OOM command not allowed")
	lenAfter, err := client.HLen(ctx, hashKey).Result()
	require.NoError(t, err)
	require.Equal(t, lenBefore, lenAfter, "LINK V3-real: the nodes hash is whole")
	require.False(t, keyExists(t, client, kb.SMSTLeavesKey(supplier, sessionID)))

	require.NoError(t, client.ConfigSet(ctx, "maxmemory", "0").Err())
	result, err = mgr.CompactColdTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, coldCompacted, result, "with memory back the retry compacts")
	failover := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	proofBz, err := failover.ProveClosest(ctx, sessionID, coldPaths(22, 1)[0])
	require.NoError(t, err)
	ok, _ := chainVerifies(t, proofBz, root)
	require.True(t, ok)
}
