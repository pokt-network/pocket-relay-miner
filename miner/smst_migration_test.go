//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestMigrateLegacySMSTKeys_RescuesOwnerAndEnablesProof is the full
// end-to-end regression: after migration, the supplier whose session
// metadata matched the legacy root must (a) see the tree under the new
// schema and (b) be able to generate a valid proof from it. This proves
// the migration is actually useful in production, not just cosmetic.
func TestMigrateLegacySMSTKeys_RescuesOwnerAndEnablesProof(t *testing.T) {
	h := newMigrationHarness(t)
	defer h.cleanup()

	sessionID := "session_rescue_owner"
	owner := "pokt1supplier_owner"
	loser1 := "pokt1supplier_loser_1"
	loser2 := "pokt1supplier_loser_2"

	// Build the OWNER's real SMST on-disk so we have a legitimate root + nodes
	// hash with content-addressed entries that the chain formula will accept.
	ownerRoot, relayPath := h.buildOwnerTree(t, owner, sessionID, []testRelayEntry{
		{key: []byte("relay-key-a"), value: []byte("relay-val-a"), weight: 100},
		{key: []byte("relay-key-b"), value: []byte("relay-val-b"), weight: 200},
		{key: []byte("relay-key-c"), value: []byte("relay-val-c"), weight: 300},
	})

	// Rename the owner's new-schema keys BACK to the legacy schema to simulate
	// pre-upgrade state: "last flusher won, only owner's data is in Redis".
	h.moveNewToLegacy(t, owner, sessionID)

	// Seed session metadata for 3 suppliers of the same session. Owner's
	// claimed_root_hash equals ownerRoot; the other two have different roots.
	h.writeSessionMetadata(t, owner, sessionID, ownerRoot)
	h.writeSessionMetadata(t, loser1, sessionID, []byte("different_root_loser_1____________"))
	h.writeSessionMetadata(t, loser2, sessionID, []byte("different_root_loser_2____________"))

	// Sanity: the legacy root key exists, the new-schema key does NOT.
	require.True(t, h.exists(h.legacyRootKey(sessionID)), "legacy root key should exist pre-migration")
	require.False(t, h.exists(h.newRootKey(owner, sessionID)), "new root key must not exist pre-migration")

	// Run the migration.
	stats, err := MigrateLegacySMSTKeys(h.ctx, zerolog.Nop(), h.redisClient)
	require.NoError(t, err)
	require.Equal(t, 1, stats.LegacyRootsScanned)
	require.Equal(t, 1, stats.SessionsMigrated)
	require.Equal(t, 0, stats.SessionsOrphaned)

	// Legacy keys gone, new-schema keys present for the owner.
	require.False(t, h.exists(h.legacyRootKey(sessionID)), "legacy root must be deleted")
	require.False(t, h.exists(h.legacyNodesKey(sessionID)), "legacy nodes must be deleted")
	require.False(t, h.exists(h.legacyStatsKey(sessionID)), "legacy stats must be deleted")
	require.True(t, h.exists(h.newRootKey(owner, sessionID)), "owner's new root must exist")
	require.True(t, h.exists(h.newNodesKey(owner, sessionID)), "owner's new nodes must exist")

	// End-to-end: owner's RedisSMSTManager must lazy-load the tree and
	// produce a root equal to ownerRoot + generate a valid proof.
	ownerMgr := h.newManager(owner)

	gotRoot, err := ownerMgr.GetTreeRoot(h.ctx, sessionID)
	require.NoError(t, err, "owner must lazy-load from migrated keys")
	require.Equal(t, ownerRoot, gotRoot, "owner root must match legacy root (end-to-end rescue)")

	proof, err := ownerMgr.ProveClosest(h.ctx, sessionID, relayPath)
	require.NoError(t, err, "owner must generate a proof from migrated tree")
	require.NotEmpty(t, proof, "proof bytes must be non-empty")
}

// TestMigrateLegacySMSTKeys_NoOwnerDeletesKeys covers the hostile case:
// legacy keys exist but no supplier's session metadata claims the same
// root. The migration must delete the orphaned keys so they don't
// accumulate in Redis.
func TestMigrateLegacySMSTKeys_NoOwnerDeletesKeys(t *testing.T) {
	h := newMigrationHarness(t)
	defer h.cleanup()

	sessionID := "session_no_owner"

	// Write legacy keys with some root that no supplier will claim.
	legacyRoot := []byte("orphaned_root_bytes_____________")
	require.NoError(t, h.redisClient.Set(h.ctx, h.legacyRootKey(sessionID), legacyRoot, 0).Err())
	require.NoError(t, h.redisClient.HSet(h.ctx, h.legacyNodesKey(sessionID), "field1", "value1").Err())
	require.NoError(t, h.redisClient.Set(h.ctx, h.legacyStatsKey(sessionID), "1:100", 0).Err())

	// Session metadata for a supplier who did NOT flush last — root differs.
	h.writeSessionMetadata(t, "pokt1different_supplier", sessionID, []byte("different_root_bytes_____________"))

	stats, err := MigrateLegacySMSTKeys(h.ctx, zerolog.Nop(), h.redisClient)
	require.NoError(t, err)
	require.Equal(t, 1, stats.LegacyRootsScanned)
	require.Equal(t, 0, stats.SessionsMigrated)
	require.Equal(t, 1, stats.SessionsOrphaned)
	require.Greater(t, stats.LegacyKeysDeleted, 0, "orphaned keys must be deleted to free Redis memory")

	require.False(t, h.exists(h.legacyRootKey(sessionID)))
	require.False(t, h.exists(h.legacyNodesKey(sessionID)))
	require.False(t, h.exists(h.legacyStatsKey(sessionID)))
}

// TestMigrateLegacySMSTKeys_IgnoresNewSchema verifies the migration is a
// no-op when all keys are already in the new schema. Critical: this test
// guards against a mis-parse that would delete new-schema data.
func TestMigrateLegacySMSTKeys_IgnoresNewSchema(t *testing.T) {
	h := newMigrationHarness(t)
	defer h.cleanup()

	sessionID := "session_new_schema"
	supplier := "pokt1supplier_already_migrated"

	// Use the real manager to write new-schema keys.
	mgr := h.newManager(supplier)
	require.NoError(t, mgr.UpdateTree(h.ctx, sessionID, []byte("k"), []byte("v"), 100))
	_, err := mgr.FlushTree(h.ctx, sessionID)
	require.NoError(t, err)

	beforeRoot := h.redisClient.Get(h.ctx, h.newRootKey(supplier, sessionID)).Val()
	require.NotEmpty(t, beforeRoot)

	stats, err := MigrateLegacySMSTKeys(h.ctx, zerolog.Nop(), h.redisClient)
	require.NoError(t, err)
	require.Equal(t, 0, stats.LegacyRootsScanned, "new-schema keys must not be counted as legacy")
	require.Equal(t, 0, stats.SessionsMigrated)

	// New-schema keys untouched.
	afterRoot := h.redisClient.Get(h.ctx, h.newRootKey(supplier, sessionID)).Val()
	require.Equal(t, beforeRoot, afterRoot, "new-schema root must not be modified by migration")
}

// TestMigrateLegacySMSTKeys_MixedLegacyAndNewSchema verifies that legacy
// entries for session A are migrated while new-schema entries for session
// B remain untouched — the most realistic post-upgrade state.
func TestMigrateLegacySMSTKeys_MixedLegacyAndNewSchema(t *testing.T) {
	h := newMigrationHarness(t)
	defer h.cleanup()

	// Session A: legacy schema, needs migration.
	sessionA := "session_mixed_legacy"
	ownerA := "pokt1mixed_owner_a"
	rootA, _ := h.buildOwnerTree(t, ownerA, sessionA, []testRelayEntry{
		{key: []byte("ka"), value: []byte("va"), weight: 50},
	})
	h.moveNewToLegacy(t, ownerA, sessionA)
	h.writeSessionMetadata(t, ownerA, sessionA, rootA)

	// Session B: already on new schema.
	sessionB := "session_mixed_new"
	supplierB := "pokt1mixed_supplier_b"
	mgrB := h.newManager(supplierB)
	require.NoError(t, mgrB.UpdateTree(h.ctx, sessionB, []byte("kb"), []byte("vb"), 75))
	rootB, err := mgrB.FlushTree(h.ctx, sessionB)
	require.NoError(t, err)

	stats, err := MigrateLegacySMSTKeys(h.ctx, zerolog.Nop(), h.redisClient)
	require.NoError(t, err)
	require.Equal(t, 1, stats.LegacyRootsScanned, "only session A should be scanned as legacy")
	require.Equal(t, 1, stats.SessionsMigrated)

	// Session A migrated.
	require.False(t, h.exists(h.legacyRootKey(sessionA)))
	require.True(t, h.exists(h.newRootKey(ownerA, sessionA)))

	// Session B unaffected.
	gotB := h.redisClient.Get(h.ctx, h.newRootKey(supplierB, sessionB)).Val()
	require.Equal(t, string(rootB), gotB, "session B new-schema root must be unchanged")
}

// --- harness ---

type migrationHarness struct {
	t           *testing.T
	ctx         context.Context
	miniRedis   *miniredis.Miniredis
	redisClient *redisutil.Client
}

type testRelayEntry struct {
	key    []byte
	value  []byte
	weight uint64
}

func newMigrationHarness(t *testing.T) *migrationHarness {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	return &migrationHarness{
		t:           t,
		ctx:         ctx,
		miniRedis:   mr,
		redisClient: client,
	}
}

func (h *migrationHarness) cleanup() {
	_ = h.redisClient.Close()
	h.miniRedis.Close()
}

func (h *migrationHarness) newManager(supplierAddr string) *RedisSMSTManager {
	return NewRedisSMSTManager(zerolog.Nop(), h.redisClient, RedisSMSTManagerConfig{
		SupplierAddress: supplierAddr,
		CacheTTL:        0,
	})
}

// buildOwnerTree uses a real RedisSMSTManager to build and flush a tree,
// then returns the root and a valid path for ProveClosest. The path points
// at the first inserted relay's key (hashed by the SMT).
func (h *migrationHarness) buildOwnerTree(
	t *testing.T,
	supplier, sessionID string,
	relays []testRelayEntry,
) (root []byte, pathForProof []byte) {
	t.Helper()
	mgr := h.newManager(supplier)
	for _, r := range relays {
		require.NoError(t, mgr.UpdateTree(h.ctx, sessionID, r.key, r.value, r.weight))
	}
	root, err := mgr.FlushTree(h.ctx, sessionID)
	require.NoError(t, err)

	// Use the canonical helper to build a ProveClosest path — the same one
	// used in production proof generation. Any 32-byte hash works, but using
	// GetPathForProof keeps the test realistic end-to-end.
	return root, protocol.GetPathForProof(relays[0].key, sessionID)
}

// moveNewToLegacy takes new-schema keys for (supplier, sessionID) and
// renames them to the legacy schema (sessionID only). Use this to
// simulate the pre-fix state after building a real tree above.
func (h *migrationHarness) moveNewToLegacy(t *testing.T, supplier, sessionID string) {
	t.Helper()
	// Root
	require.NoError(t, h.redisClient.Rename(h.ctx,
		h.newRootKey(supplier, sessionID), h.legacyRootKey(sessionID)).Err())
	// Nodes — may or may not exist depending on tree size.
	if ok, _ := h.redisClient.Exists(h.ctx, h.newNodesKey(supplier, sessionID)).Result(); ok > 0 {
		require.NoError(t, h.redisClient.Rename(h.ctx,
			h.newNodesKey(supplier, sessionID), h.legacyNodesKey(sessionID)).Err())
	}
	// Stats
	if ok, _ := h.redisClient.Exists(h.ctx, h.newStatsKey(supplier, sessionID)).Result(); ok > 0 {
		require.NoError(t, h.redisClient.Rename(h.ctx,
			h.newStatsKey(supplier, sessionID), h.legacyStatsKey(sessionID)).Err())
	}
}

// writeSessionMetadata seeds `ha:miner:sessions:{supplier}:{sessionID}` with
// just the claimed_root_hash field — enough for the owner-lookup to match.
func (h *migrationHarness) writeSessionMetadata(
	t *testing.T, supplier, sessionID string, rootHash []byte,
) {
	t.Helper()
	key := h.redisClient.KB().MinerSessionKey(supplier, sessionID)
	require.NoError(t, h.redisClient.HSet(h.ctx, key, hfClaimedRootHash, string(rootHash)).Err())
}

func (h *migrationHarness) legacyRootKey(sessionID string) string {
	return h.redisClient.KB().SMSTNodesPrefix() + sessionID + ":root"
}
func (h *migrationHarness) legacyNodesKey(sessionID string) string {
	return h.redisClient.KB().SMSTNodesPrefix() + sessionID + ":nodes"
}
func (h *migrationHarness) legacyStatsKey(sessionID string) string {
	return h.redisClient.KB().SMSTNodesPrefix() + sessionID + ":stats"
}
func (h *migrationHarness) newRootKey(supplier, sessionID string) string {
	return h.redisClient.KB().SMSTRootKey(supplier, sessionID)
}
func (h *migrationHarness) newNodesKey(supplier, sessionID string) string {
	return h.redisClient.KB().SMSTNodesKey(supplier, sessionID)
}
func (h *migrationHarness) newStatsKey(supplier, sessionID string) string {
	return h.redisClient.KB().SMSTStatsKey(supplier, sessionID)
}
func (h *migrationHarness) exists(key string) bool {
	n, _ := h.redisClient.Exists(h.ctx, key).Result()
	return n > 0
}

// Ensure the SMT library import is actually used (the harness uses smt via
// the RedisSMSTManager); this blank-assign avoids a spurious "imported and
// not used" if the file is refactored.
var _ = smt.NewSparseMerkleSumTrie
