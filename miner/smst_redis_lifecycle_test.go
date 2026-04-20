//go:build test

package miner

// Regression tests for three Redis-lifecycle bugs that can produce
// stake-drain or silent progress loss under specific timings:
//
//  1. claimed_root persisted with TTL=0 (forever) while the nodes hash
//     ages out via its sliding TTL. A crash between FlushTree and
//     proof submission leaves a dangling claimed_root that, on leader
//     resume, imports a tree whose inner nodes are already gone —
//     ProveClosest then fails with ErrSMSTNodeMissing and the session
//     goes to proof-missing (stake slashed).
//
//  2. WarmupFromRedis creates a FRESH empty tree even when a live_root
//     or claimed_root exists in Redis. Any caller (ops script, future
//     eager-warmup wiring, debug tool) silently resets in-progress
//     sessions to zero relays.
//
//  3. MigrateLegacySMSTKeys renames legacy nodes/root/stats but NOT
//     the legacy live_root key. Mid-session checkpoints at migration
//     time end up orphaned under the legacy key; the new code looks
//     for ha:smst:{supplier}:{sessionID}:live_root and finds nothing,
//     creating a fresh empty tree on the next relay (all pre-migration
//     relays for that session lost).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// --- BUG 1: claimed_root TTL ---

// TestFlushTree_ClaimedRootHonoursCacheTTL verifies that FlushTree
// persists the claimed_root (and stats) with the configured CacheTTL,
// not TTL=0 (persist forever). This is the key invariant that prevents
// claimed_root from outliving the nodes hash on a crash between claim
// and proof submission.
func TestFlushTree_ClaimedRootHonoursCacheTTL(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer client.Close()

	supplier := "pokt1claimed_root_ttl"
	sessionID := "session_claimed_root_ttl"
	const cacheTTL = 30 * time.Second

	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress:            supplier,
		CacheTTL:                   cacheTTL,
		LiveRootCheckpointInterval: 1,
	})

	// Build a real tree and flush it to write claimed_root + stats.
	require.NoError(t, mgr.UpdateTree(ctx, sessionID, []byte("k1"), []byte("v1"), 10))
	_, err = mgr.FlushTree(ctx, sessionID)
	require.NoError(t, err)

	rootKey := client.KB().SMSTRootKey(supplier, sessionID)
	statsKey := client.KB().SMSTStatsKey(supplier, sessionID)

	// Immediately after flush, TTL must be ~cacheTTL. miniredis TTL is exact.
	require.Equalf(t, cacheTTL, mr.TTL(rootKey),
		"FlushTree MUST persist claimed_root with CacheTTL so it cannot outlive the nodes hash (got %s)", mr.TTL(rootKey))
	require.Equalf(t, cacheTTL, mr.TTL(statsKey),
		"FlushTree MUST persist stats with CacheTTL for the same reason (got %s)", mr.TTL(statsKey))

	// Fast-forward past TTL and confirm claimed_root is gone. This is the
	// behavioural proof that the TTL is enforced; if the bug re-appears
	// (TTL=0) the key would still exist.
	mr.FastForward(cacheTTL + time.Second)
	require.Falsef(t, mr.Exists(rootKey),
		"claimed_root MUST expire after CacheTTL elapses (bug: TTL=0 would leave it persistent)")
}

// TestLoadTreeFromRedis_RefreshesClaimedRootTTL verifies that a
// successful lazy-load of the tree (proof-submission path) refreshes
// the claimed_root's TTL. A long-running proof-retry loop must not
// allow the key to age out mid-flight.
func TestLoadTreeFromRedis_RefreshesClaimedRootTTL(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer client.Close()

	supplier := "pokt1claimed_root_refresh"
	sessionID := "session_claimed_root_refresh"
	const cacheTTL = 30 * time.Second

	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress:            supplier,
		CacheTTL:                   cacheTTL,
		LiveRootCheckpointInterval: 1,
	})

	require.NoError(t, mgr.UpdateTree(ctx, sessionID, []byte("k1"), []byte("v1"), 10))
	_, err = mgr.FlushTree(ctx, sessionID)
	require.NoError(t, err)

	rootKey := client.KB().SMSTRootKey(supplier, sessionID)
	statsKey := client.KB().SMSTStatsKey(supplier, sessionID)

	// Age the key part-way to expiry without crossing it.
	mr.FastForward(cacheTTL / 2)
	ttlBefore := mr.TTL(rootKey)
	require.Lessf(t, ttlBefore, cacheTTL-time.Second,
		"TTL should have decayed after fast-forward (got %s)", ttlBefore)

	// Simulate a proof-submission retry path: drop the in-memory tree,
	// then lazy-load from Redis. loadTreeFromRedis must refresh the TTL.
	mgr.treesMu.Lock()
	delete(mgr.trees, sessionID)
	mgr.treesMu.Unlock()

	loaded, err := mgr.loadTreeFromRedis(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, loaded)

	ttlAfter := mr.TTL(rootKey)
	require.Equalf(t, cacheTTL, ttlAfter,
		"loadTreeFromRedis MUST refresh claimed_root TTL on resume (got %s)", ttlAfter)
	require.Equalf(t, cacheTTL, mr.TTL(statsKey),
		"loadTreeFromRedis MUST refresh stats TTL on resume (got %s)", mr.TTL(statsKey))

	// After a further aging pass that would have expired the pre-refresh
	// TTL, the key must still be alive — proving the refresh actually
	// extended lifetime, not just touched it.
	mr.FastForward(cacheTTL - time.Second)
	require.Truef(t, mr.Exists(rootKey),
		"claimed_root MUST survive post-refresh aging (bug: no refresh would let it expire)")
}

// --- BUG 2: WarmupFromRedis resume paths ---

// TestWarmupFromRedis_ResumesFromLiveRoot verifies that when Redis
// contains a live_root and populated nodes hash for a session (mid-
// session state from a prior leader), WarmupFromRedis imports the
// tree at the live_root rather than creating a fresh empty one. The
// bug: NewSparseMerkleSumTrie was called unconditionally, silently
// discarding the in-progress session's relays on any warmup call.
func TestWarmupFromRedis_ResumesFromLiveRoot(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer client.Close()

	supplier := "pokt1warmup_live"
	sessionID := "session_warmup_live"

	// Seed a mid-session tree via a first manager that checkpoints on every
	// update. Do NOT flush — we want only live_root, no claimed_root.
	seedMgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress:            supplier,
		CacheTTL:                   0,
		LiveRootCheckpointInterval: 1,
	})
	const preWarmupUpdates = 5
	for i := 0; i < preWarmupUpdates; i++ {
		require.NoError(t, seedMgr.UpdateTree(ctx, sessionID,
			[]byte{byte(i)}, []byte{byte(i + 100)}, 10))
	}

	// Sanity: live_root exists, claimed_root does NOT.
	require.True(t, mr.Exists(client.KB().SMSTLiveRootKey(supplier, sessionID)))
	require.False(t, mr.Exists(client.KB().SMSTRootKey(supplier, sessionID)))

	// Fresh manager performs warmup. The bug: this used to build an empty
	// tree, so the next UpdateTree would see 1 relay instead of 6.
	warmMgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress:            supplier,
		CacheTTL:                   0,
		LiveRootCheckpointInterval: 1,
	})
	loaded, err := warmMgr.WarmupFromRedis(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, loaded)

	// Pushing one more relay must carry forward the 5 pre-warmup relays.
	require.NoError(t, warmMgr.UpdateTree(ctx, sessionID,
		[]byte{byte(100)}, []byte{byte(200)}, 10))
	_, err = warmMgr.FlushTree(ctx, sessionID)
	require.NoError(t, err)
	count, sum, err := warmMgr.GetTreeStats(sessionID)
	require.NoError(t, err)
	require.Equalf(t, uint64(preWarmupUpdates+1), count,
		"WarmupFromRedis MUST resume from live_root (expected %d, got %d — bug: empty tree makes it 1)",
		preWarmupUpdates+1, count)
	require.Equal(t, uint64((preWarmupUpdates+1)*10), sum)
}

// TestWarmupFromRedis_ResumesFromClaimedRoot verifies that when the
// session has already been flushed (claimed_root present), warmup
// imports the tree with claimedRoot set so late updates are correctly
// rejected with ErrSessionClaimed.
func TestWarmupFromRedis_ResumesFromClaimedRoot(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer client.Close()

	supplier := "pokt1warmup_claimed"
	sessionID := "session_warmup_claimed"

	seedMgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        0,
	})
	require.NoError(t, seedMgr.UpdateTree(ctx, sessionID,
		[]byte("k1"), []byte("v1"), 10))
	expectedRoot, err := seedMgr.FlushTree(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, mr.Exists(client.KB().SMSTRootKey(supplier, sessionID)))

	warmMgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        0,
	})
	loaded, err := warmMgr.WarmupFromRedis(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, loaded)

	// The restored tree must have claimedRoot set and reject updates.
	gotRoot, err := warmMgr.GetTreeRoot(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, expectedRoot, gotRoot,
		"WarmupFromRedis MUST import at claimed_root (not reset to empty)")

	err = warmMgr.UpdateTree(ctx, sessionID, []byte("late"), []byte("late"), 10)
	require.ErrorIs(t, err, ErrSessionClaimed,
		"warmed-up claimed tree MUST reject late updates (claimedRoot set)")
}

// TestWarmupFromRedis_HandlesCorruptLiveRoot verifies that a live_root
// with invalid length does NOT panic WarmupFromRedis. The bug surface
// is ImportSparseMerkleSumTrie which panics on malformed roots; the
// fix must wrap it in runSMSTSafely and fall through on failure.
func TestWarmupFromRedis_HandlesCorruptLiveRoot(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer client.Close()

	supplier := "pokt1warmup_corrupt"
	sessionID := "session_warmup_corrupt"

	// Seed nodes so the key exists and is visible to the :nodes scan.
	nodesKey := client.KB().SMSTNodesKey(supplier, sessionID)
	require.NoError(t, client.HSet(ctx, nodesKey, "field", "value").Err())
	// Write a corrupt live_root (short bytes).
	liveKey := client.KB().SMSTLiveRootKey(supplier, sessionID)
	require.NoError(t, client.Set(ctx, liveKey, []byte("too_short"), 0).Err())

	warmMgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        0,
	})

	// Must not panic and must return cleanly. The tree may or may not be
	// loaded; what matters is the process survives the malformed root.
	require.NotPanics(t, func() {
		_, werr := warmMgr.WarmupFromRedis(ctx)
		require.NoError(t, werr)
	}, "WarmupFromRedis MUST NOT propagate ImportSparseMerkleSumTrie panics")

	// The corrupt live_root should have been deleted by the fall-through.
	require.False(t, mr.Exists(liveKey),
		"corrupt live_root MUST be deleted so subsequent resumes don't re-trip")
}

// --- BUG 3: migration live_root rename ---

// TestMigrateLegacySMSTKeys_RenamesLiveRoot verifies that a legacy
// session with a live_root key (mid-session at migration time) also
// has its live_root migrated to the new per-supplier schema. Without
// this, the new code cannot find the checkpoint and silently creates
// a fresh tree on the next relay — losing all pre-migration relays.
func TestMigrateLegacySMSTKeys_RenamesLiveRoot(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer client.Close()

	sessionID := "session_live_root_migration"
	owner := "pokt1live_root_owner"

	// Build a real tree under the new schema, then flush — we need a
	// real claimed_root so the owner-lookup resolves to this supplier.
	seedMgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress:            owner,
		CacheTTL:                   0,
		LiveRootCheckpointInterval: 1,
	})
	require.NoError(t, seedMgr.UpdateTree(ctx, sessionID,
		[]byte("ka"), []byte("va"), 100))
	require.NoError(t, seedMgr.UpdateTree(ctx, sessionID,
		[]byte("kb"), []byte("vb"), 200))
	ownerRoot, err := seedMgr.FlushTree(ctx, sessionID)
	require.NoError(t, err)

	// Move new-schema keys (root, nodes, stats) to legacy schema.
	newRoot := client.KB().SMSTRootKey(owner, sessionID)
	newNodes := client.KB().SMSTNodesKey(owner, sessionID)
	newStats := client.KB().SMSTStatsKey(owner, sessionID)
	newLive := client.KB().SMSTLiveRootKey(owner, sessionID)
	legacyPrefix := client.KB().SMSTNodesPrefix()
	legacyRoot := legacyPrefix + sessionID + ":root"
	legacyNodes := legacyPrefix + sessionID + ":nodes"
	legacyStats := legacyPrefix + sessionID + ":stats"
	legacyLive := legacyPrefix + sessionID + ":live_root"

	require.NoError(t, client.Rename(ctx, newRoot, legacyRoot).Err())
	if ok, _ := client.Exists(ctx, newNodes).Result(); ok > 0 {
		require.NoError(t, client.Rename(ctx, newNodes, legacyNodes).Err())
	}
	if ok, _ := client.Exists(ctx, newStats).Result(); ok > 0 {
		require.NoError(t, client.Rename(ctx, newStats, legacyStats).Err())
	}
	// The seed manager used interval=1 so live_root was checkpointed. Move it too.
	if ok, _ := client.Exists(ctx, newLive).Result(); ok > 0 {
		require.NoError(t, client.Rename(ctx, newLive, legacyLive).Err())
	}
	require.True(t, mr.Exists(legacyLive),
		"legacy live_root must exist pre-migration (seed checkpointed it)")

	// Seed session metadata so owner-lookup finds the supplier.
	require.NoError(t, client.HSet(ctx,
		client.KB().MinerSessionKey(owner, sessionID),
		hfClaimedRootHash, string(ownerRoot)).Err())

	stats, err := MigrateLegacySMSTKeys(ctx, zerolog.Nop(), client)
	require.NoError(t, err)
	require.Equal(t, 1, stats.SessionsMigrated)

	// All four keys must be renamed to per-supplier form.
	require.False(t, mr.Exists(legacyRoot),
		"legacy root must be renamed")
	require.False(t, mr.Exists(legacyNodes),
		"legacy nodes must be renamed")
	require.False(t, mr.Exists(legacyStats),
		"legacy stats must be renamed")
	require.Falsef(t, mr.Exists(legacyLive),
		"legacy live_root MUST be renamed (bug: it was left orphaned)")

	require.True(t, mr.Exists(newRoot), "new root must exist")
	require.True(t, mr.Exists(newNodes), "new nodes must exist")
	require.Truef(t, mr.Exists(newLive),
		"new live_root MUST exist after migration (bug: previously lost)")
}

// TestMigrateLegacySMSTKeys_DeletesOrphanedLiveRootOnly covers the
// second scan-pattern case: a legacy session has ONLY a live_root
// (no claimed_root). The :root-key scan cannot see it. A clean
// implementation either migrates it or deletes it to avoid orphan
// accumulation; either way the session cannot be safely rescued
// (owner lookup relies on claimed_root_hash which does not exist
// mid-session). The test asserts the bytes do NOT accumulate
// indefinitely: after migration, the legacy live_root is gone.
func TestMigrateLegacySMSTKeys_DeletesOrphanedLiveRootOnly(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer client.Close()

	sessionID := "session_orphan_live_root"
	legacyPrefix := client.KB().SMSTNodesPrefix()
	legacyLive := legacyPrefix + sessionID + ":live_root"
	legacyNodes := legacyPrefix + sessionID + ":nodes"

	// Seed a mid-session legacy state: nodes hash + live_root, NO root.
	validRoot := make([]byte, SMSTRootLen)
	for i := range validRoot {
		validRoot[i] = 0xAB
	}
	require.NoError(t, client.Set(ctx, legacyLive, validRoot, 0).Err())
	require.NoError(t, client.HSet(ctx, legacyNodes, "field1", "value1").Err())

	stats, err := MigrateLegacySMSTKeys(ctx, zerolog.Nop(), client)
	require.NoError(t, err)
	// No :root key → the primary scan sees nothing. The secondary scan
	// must still clean up the orphaned live_root so Redis memory does
	// not accumulate across upgrades.
	require.Greaterf(t, stats.LegacyKeysDeleted+stats.SessionsMigrated, 0,
		"mid-session-only legacy state must be handled (migrated or deleted), got deleted=%d migrated=%d",
		stats.LegacyKeysDeleted, stats.SessionsMigrated)
	require.Falsef(t, mr.Exists(legacyLive),
		"legacy live_root must be removed (migrated or deleted) to prevent Redis memory leak across upgrades")
}

// Keep SMT types reachable from this file — the manager uses them
// transitively. Silences "imported and not used" if a future refactor
// drops the transitive uses.
var _ = smt.NewSparseMerkleSumTrie
var _ = protocol.NewTrieHasher
