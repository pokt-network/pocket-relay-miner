//go:build test

package redis

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	transportredis "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// newNamespacedDebugClient builds a real debug client whose namespace base
// prefix is this test's own key prefix.
//
// That does double duty: it isolates the test on the shared Redis (no FLUSH,
// ever), and it means every key the command touches is reached through the
// KeyBuilder under a NON-default namespace. A command that hand-built
// "ha:relays:*" would find nothing here and the test would fail — which is the
// point, since the convention check cannot see a pattern built by concatenation.
func newNamespacedDebugClient(t *testing.T) (*DebugRedisClient, string) {
	t.Helper()
	prefix := testredis.Prefix(t)

	client, err := transportredis.NewClient(context.Background(), transportredis.ClientConfig{
		URL:       testredis.URL(),
		Namespace: config.RedisNamespaceConfig{BasePrefix: prefix},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return &DebugRedisClient{
		Client: client,
		Logger: logging.NewLoggerFromConfig(logging.DefaultConfig()),
	}, prefix
}

const (
	orphanKnownAddr = "pokt1known"
	orphanEmptyAddr = "pokt1orphan_empty"
	orphanFullAddr  = "pokt1orphan_with_relays"
)

// seedOrphanFixture creates three streams: one belonging to a supplier the
// registry still lists, one orphan holding relays, and one orphan holding
// nothing.
func seedOrphanFixture(t *testing.T, c *DebugRedisClient) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, c.SAdd(ctx, c.KB().SuppliersRegistryIndexKey(), orphanKnownAddr).Err())

	for _, addr := range []string{orphanKnownAddr, orphanFullAddr} {
		require.NoError(t, c.XAdd(ctx, &redis.XAddArgs{
			Stream: c.KB().StreamKey(addr), Values: map[string]any{"data": []byte("x")},
		}).Err())
	}

	// The empty orphan: created, then emptied, which is what a stream looks like
	// once every relay in it has been acknowledged and deleted.
	emptyKey := c.KB().StreamKey(orphanEmptyAddr)
	res, err := c.XAdd(ctx, &redis.XAddArgs{Stream: emptyKey, Values: map[string]any{"data": []byte("x")}}).Result()
	require.NoError(t, err)
	require.NoError(t, c.XDel(ctx, emptyKey, res).Err())

	length, err := c.XLen(ctx, emptyKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), length, "premise: the empty orphan holds nothing")
}

func streamExists(t *testing.T, c *DebugRedisClient, addr string) bool {
	t.Helper()
	n, err := c.Exists(context.Background(), c.KB().StreamKey(addr)).Result()
	require.NoError(t, err)
	return n == 1
}

// TestOrphanedStreamsNeverDeletesAStreamHoldingRelays is the safety property.
//
// Deleting a stream key deletes its consumer group and that group's pending
// entries list with it, and the consumer recreates the group empty on its next
// connect — so a delete here is not recoverable and not even noticeable. Entries
// in a stream are relays nobody has been paid for yet, so a stream holding any
// is never a candidate, orphaned or not.
func TestOrphanedStreamsNeverDeletesAStreamHoldingRelays(t *testing.T) {
	c, _ := newNamespacedDebugClient(t)
	seedOrphanFixture(t, c)

	require.NoError(t, orphanedStreams(context.Background(), c, true, true))

	require.True(t, streamExists(t, c, orphanFullAddr),
		"an orphaned stream that still holds relays must survive: those entries are unpaid work, "+
			"and deleting the key takes the consumer group and its pending list with it")
	require.True(t, streamExists(t, c, orphanKnownAddr),
		"a stream whose supplier is still known is not an orphan at all")
	require.False(t, streamExists(t, c, orphanEmptyAddr),
		"an orphaned stream holding nothing is exactly what --delete-empty is for")
}

// TestOrphanedStreamsListingDeletesNothing pins that the default is read-only.
// An operator running the command to look must not change anything.
func TestOrphanedStreamsListingDeletesNothing(t *testing.T) {
	c, _ := newNamespacedDebugClient(t)
	seedOrphanFixture(t, c)

	require.NoError(t, orphanedStreams(context.Background(), c, false, false))

	for _, addr := range []string{orphanKnownAddr, orphanFullAddr, orphanEmptyAddr} {
		require.True(t, streamExists(t, c, addr),
			"listing orphans must not delete %s: without --delete-empty the command only reports", addr)
	}
}

// TestOrphanedStreamsCountsTheSupplierCacheAsKnown covers the second half of the
// "known" definition.
//
// A supplier torn down by this fleet keeps its cache entry on purpose — that
// entry is the chain's answer, and the relayer needs it to keep refusing relays.
// Treating only the registry index as authoritative would call such a supplier's
// stream an orphan and offer it for deletion while its relays are still being
// claimed.
func TestOrphanedStreamsCountsTheSupplierCacheAsKnown(t *testing.T) {
	c, _ := newNamespacedDebugClient(t)
	ctx := context.Background()

	const addr = "pokt1torn_down_but_cached"
	require.NoError(t, c.Set(ctx, c.KB().SupplierStateKey(addr), `{"status":"unstaking"}`, 0).Err())

	// An empty stream, so it WOULD be deleted if it were classified as an orphan.
	key := c.KB().StreamKey(addr)
	id, err := c.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"data": []byte("x")}}).Result()
	require.NoError(t, err)
	require.NoError(t, c.XDel(ctx, key, id).Err())

	require.NoError(t, orphanedStreams(ctx, c, true, true))

	require.True(t, streamExists(t, c, addr),
		"a supplier present in the supplier cache is known, even with no registry index entry: "+
			"its stream is not an orphan")
}

// TestKnownSupplierAddressesUnionsBothSources states the union directly, so a
// future change that drops one source fails here rather than in a delete path.
func TestKnownSupplierAddressesUnionsBothSources(t *testing.T) {
	c, _ := newNamespacedDebugClient(t)
	ctx := context.Background()

	require.NoError(t, c.SAdd(ctx, c.KB().SuppliersRegistryIndexKey(), "pokt1from_index").Err())
	require.NoError(t, c.Set(ctx, c.KB().SupplierStateKey("pokt1from_cache"), "{}", 0).Err())

	known, err := miner.KnownSupplierAddresses(ctx, c.Client, func(ctx context.Context, pattern string) ([]string, error) {
		return clusterAwareScanAllKeys(ctx, c, pattern)
	})
	require.NoError(t, err)
	require.Contains(t, known, "pokt1from_index")
	require.Contains(t, known, "pokt1from_cache")
	require.NotContains(t, known, "pokt1never_seen")
}

// TestKnownSupplierAddressesOnAnEmptyDeployment covers the cold-start shape: no
// registry index key exists at all, which Redis reports as a missing key rather
// than an empty set.
func TestKnownSupplierAddressesOnAnEmptyDeployment(t *testing.T) {
	c, _ := newNamespacedDebugClient(t)

	ctx := context.Background()
	known, err := miner.KnownSupplierAddresses(ctx, c.Client, func(ctx context.Context, pattern string) ([]string, error) {
		return clusterAwareScanAllKeys(ctx, c, pattern)
	})
	require.NoError(t, err, "a deployment that has never registered a supplier is not an error")
	require.Empty(t, known)
}

// TestStreamStatsReportsUnknownOnAReadError is MEDIUM-1's test-teeth (review
// 2026-08-20): a stream key that fails to read must come back ok=false, never
// a silent zero. Before the fix, orphanedStreams left length/pending at their
// zero value on an XLen/XInfoGroups error and printed "SAFE TO DELETE true"
// for a stream it could not actually inspect.
//
// WRONGTYPE, not a mock: setting a plain string at the stream's key is a
// real, deterministic way to make XLen/XInfoGroups fail against real Redis,
// without needing an error-injecting client.
func TestStreamStatsReportsUnknownOnAReadError(t *testing.T) {
	c, _ := newNamespacedDebugClient(t)
	ctx := context.Background()

	const addr = "pokt1wrongtype"
	key := c.KB().StreamKey(addr)
	require.NoError(t, c.Set(ctx, key, "not a stream", 0).Err())

	length, pending, ok := streamStats(ctx, c, key)
	require.False(t, ok, "a read error must report ok=false, never a silent zero")
	require.Zero(t, length)
	require.Zero(t, pending)
}

// TestStreamStatsCountsPendingEvenWhenLengthIsZero is MEDIUM-2's test-teeth.
// It reproduces the exact shape the review named: entries acknowledged-and-
// deleted (XACKDEL/DELREF) or plain XDEL can leave a stream at XLEN==0 while
// a consumer group's pending list still references those entry IDs. The
// pre-delete re-check used to run only XLen and would have called this
// stream safe; it now calls the same streamStats the listing uses, so this
// proves the shared primitive reports the case correctly for both call sites
// at once.
func TestStreamStatsCountsPendingEvenWhenLengthIsZero(t *testing.T) {
	c, _ := newNamespacedDebugClient(t)
	ctx := context.Background()

	const addr = "pokt1acked_but_pending"
	key := c.KB().StreamKey(addr)

	id, err := c.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"data": []byte("x")}}).Result()
	require.NoError(t, err)
	require.NoError(t, c.XGroupCreate(ctx, key, "g1", "0").Err())

	// Deliver the entry to a consumer without acking it: it lands in the
	// group's pending list.
	_, err = c.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: "g1", Consumer: "c1", Streams: []string{key, ">"}, Count: 1,
	}).Result()
	require.NoError(t, err)

	// The entry is gone from the stream itself, but the group never acked it,
	// so it stays in the PEL -- exactly what XACKDEL/DELREF and XDEL both can
	// leave behind.
	require.NoError(t, c.XDel(ctx, key, id).Err())

	length, err := c.XLen(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), length, "premise: XLEN reports empty despite the pending entry")

	gotLength, gotPending, ok := streamStats(ctx, c, key)
	require.True(t, ok)
	require.Equal(t, int64(0), gotLength)
	require.Equal(t, int64(1), gotPending,
		"a stream at XLEN==0 with a group's PEL still non-empty must NOT be reported as empty -- "+
			"deleting it would take that pending entry, and the relay it represents, with it")
}

// TestOrphanedStreamsRefusesToDeleteAStreamWithOnlyPendingEntries pins the
// safety property end to end: an orphan stream at XLEN==0 with a nonzero PEL
// must survive --delete-empty.
//
// It does NOT discriminate MEDIUM-2's specific fix (the pre-delete re-check
// re-running only XLen): the state here is already this shape at scan time,
// so the classification pass -- which always summed pending, before and
// after the fix -- excludes it from `deletable` before the re-check ever
// runs. Verified by injecting the pre-fix re-check (XLen-only) and confirming
// this test still passes. The actual test-teeth for the re-check's own code
// path is TestStreamStatsCountsPendingEvenWhenLengthIsZero above: the
// re-check now calls that exact function, so proving it correct there proves
// the re-check correct without needing to win a scan-vs-delete race, which
// orphanedStreams has no seam to inject deterministically.
func TestOrphanedStreamsRefusesToDeleteAStreamWithOnlyPendingEntries(t *testing.T) {
	c, _ := newNamespacedDebugClient(t)
	ctx := context.Background()

	const addr = "pokt1orphan_acked_but_pending"
	key := c.KB().StreamKey(addr)

	id, err := c.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"data": []byte("x")}}).Result()
	require.NoError(t, err)
	require.NoError(t, c.XGroupCreate(ctx, key, "g1", "0").Err())
	_, err = c.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: "g1", Consumer: "c1", Streams: []string{key, ">"}, Count: 1,
	}).Result()
	require.NoError(t, err)
	require.NoError(t, c.XDel(ctx, key, id).Err())

	require.NoError(t, orphanedStreams(ctx, c, true, true))

	require.True(t, streamExists(t, c, addr),
		"XLEN==0 is not enough to call a stream empty -- a group's pending entry must block the delete")
}
