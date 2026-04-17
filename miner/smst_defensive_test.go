//go:build test

package miner

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// These tests reproduce the Nodefleet-reported panic
//   panic: runtime error: slice bounds out of range [:1] with capacity 0
// and confirm every entry point that reads a serialized SMST root from Redis
// refuses to pass a malformed payload to the smt library, deletes the corrupt
// key so the issue self-heals, and falls back to a safe state (fresh tree,
// explicit error, or orphaned migration).

// corruptRootLengths is the set of lengths that trigger the Count()/Sum() or
// internal slice panic in the smt library: empty, 1-byte (Jonathan's stack
// trace), short-prefix, and off-by-one just below the expected 48 bytes.
var corruptRootLengths = []int{0, 1, 10, 32, 47}

// validRoot returns a byte slice of the expected SMST root length. It is only
// the right shape, not a cryptographically valid root — every test below only
// checks that the defensive layer rejects / accepts based on length.
func validRoot() []byte {
	r := make([]byte, SMSTRootLen)
	for i := range r {
		r[i] = byte(i + 1)
	}
	return r
}

// defensiveHarness wraps miniredis + a RedisSMSTManager for each test case so
// the corrupt-root scenarios are fully isolated from each other.
type defensiveHarness struct {
	t           *testing.T
	ctx         context.Context
	miniRedis   *miniredis.Miniredis
	redisClient *redisutil.Client
	manager     *RedisSMSTManager
	supplier    string
}

func newDefensiveHarness(t *testing.T) *defensiveHarness {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	supplier := "pokt1defensive_supplier_addr"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier,
		CacheTTL:        0,
	})
	return &defensiveHarness{
		t: t, ctx: ctx, miniRedis: mr,
		redisClient: client, manager: mgr, supplier: supplier,
	}
}

func (h *defensiveHarness) cleanup() {
	_ = h.redisClient.Close()
	h.miniRedis.Close()
}

func (h *defensiveHarness) claimedRootKey(sessionID string) string {
	return h.redisClient.KB().SMSTRootKey(h.supplier, sessionID)
}
func (h *defensiveHarness) liveRootKey(sessionID string) string {
	return h.redisClient.KB().SMSTLiveRootKey(h.supplier, sessionID)
}
func (h *defensiveHarness) exists(key string) bool {
	n, _ := h.redisClient.Exists(h.ctx, key).Result()
	return n > 0
}

// TestIsValidSMSTRoot pins the invariant used by every defensive check.
func TestIsValidSMSTRoot(t *testing.T) {
	for _, n := range corruptRootLengths {
		require.Falsef(t, isValidSMSTRoot(make([]byte, n)),
			"%d-byte slice must be rejected", n)
	}
	require.True(t, isValidSMSTRoot(validRoot()), "48-byte slice must be accepted")
	require.False(t, isValidSMSTRoot(nil), "nil must be rejected")
	require.False(t, isValidSMSTRoot(make([]byte, SMSTRootLen+1)), "49-byte slice must be rejected")
}

// TestGetOrCreateTree_CorruptClaimedRoot_DeletesAndStartsFresh exercises the
// failover path a follower hits when the leader left a corrupt claimed_root
// in Redis. Historically this was the direct panic trigger:
// smt.ImportSparseMerkleSumTrie with a sub-length root.
func TestGetOrCreateTree_CorruptClaimedRoot_DeletesAndStartsFresh(t *testing.T) {
	for _, n := range corruptRootLengths {
		n := n
		t.Run(fmt.Sprintf("root_len=%d", n), func(t *testing.T) {
			h := newDefensiveHarness(t)
			defer h.cleanup()
			sessionID := fmt.Sprintf("session_claimed_corrupt_%d", n)

			corrupt := make([]byte, n)
			require.NoError(t, h.redisClient.Set(h.ctx, h.claimedRootKey(sessionID), corrupt, 0).Err())

			// Must not panic — that is the explicit regression guard.
			require.NotPanics(t, func() {
				tree, err := h.manager.GetOrCreateTree(h.ctx, sessionID)
				require.NoError(t, err)
				require.NotNil(t, tree)
				require.Nil(t, tree.claimedRoot,
					"corrupt claimed_root must be discarded, fresh tree must be empty")
			})

			// len==0 never reaches the defensive branch (the existing `len > 0`
			// guard short-circuits first) and cannot trigger the panic anyway,
			// so we only require deletion for the sizes that *would* panic.
			if n > 0 {
				require.False(t, h.exists(h.claimedRootKey(sessionID)),
					"corrupt claimed_root key must be deleted")
			}
		})
	}
}

// TestGetOrCreateTree_CorruptLiveRoot_DeletesAndStartsFresh is the same
// guarantee for the second Redis key read by resumeTreeFromRedisLocked.
// Corrupt live_root used to import into an empty-capacity byte slice.
func TestGetOrCreateTree_CorruptLiveRoot_DeletesAndStartsFresh(t *testing.T) {
	for _, n := range corruptRootLengths {
		n := n
		t.Run(fmt.Sprintf("root_len=%d", n), func(t *testing.T) {
			h := newDefensiveHarness(t)
			defer h.cleanup()
			sessionID := fmt.Sprintf("session_live_corrupt_%d", n)

			corrupt := make([]byte, n)
			require.NoError(t, h.redisClient.Set(h.ctx, h.liveRootKey(sessionID), corrupt, 0).Err())

			require.NotPanics(t, func() {
				tree, err := h.manager.GetOrCreateTree(h.ctx, sessionID)
				require.NoError(t, err)
				require.NotNil(t, tree)
			})

			if n > 0 {
				require.False(t, h.exists(h.liveRootKey(sessionID)),
					"corrupt live_root key must be deleted so the next leader does not trip on it either")
			}
		})
	}
}

// TestGetOrCreateTree_ValidClaimedRoot_Imports confirms the defensive layer
// does not regress the happy path: a 48-byte root still imports.
func TestGetOrCreateTree_ValidClaimedRoot_Imports(t *testing.T) {
	h := newDefensiveHarness(t)
	defer h.cleanup()
	sessionID := "session_valid_claimed"
	root := validRoot()
	require.NoError(t, h.redisClient.Set(h.ctx, h.claimedRootKey(sessionID), root, 0).Err())

	// ImportSparseMerkleSumTrie does not itself validate the cryptographic
	// contents, so a 48-byte "shape-valid" payload imports successfully and
	// is cached as the tree's claimedRoot. That is exactly what we want the
	// defensive length check to allow through unchanged.
	tree, err := h.manager.GetOrCreateTree(h.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, tree)
	require.True(t, bytes.Equal(root, tree.claimedRoot), "claimedRoot must be preserved byte-for-byte")
	require.True(t, h.exists(h.claimedRootKey(sessionID)), "valid root must not be deleted")
}

// TestLoadTreeFromRedis_CorruptRoot_ErrorsInsteadOfPanicking covers the
// lazy-load path used by ProveClosest after HA failover.
func TestLoadTreeFromRedis_CorruptRoot_ErrorsInsteadOfPanicking(t *testing.T) {
	for _, n := range corruptRootLengths {
		if n == 0 {
			// len==0 hits the existing "not found" branch, not the new guard.
			continue
		}
		n := n
		t.Run(fmt.Sprintf("root_len=%d", n), func(t *testing.T) {
			h := newDefensiveHarness(t)
			defer h.cleanup()
			sessionID := fmt.Sprintf("session_load_corrupt_%d", n)
			corrupt := make([]byte, n)
			require.NoError(t, h.redisClient.Set(h.ctx, h.claimedRootKey(sessionID), corrupt, 0).Err())

			var (
				tree *redisSMST
				err  error
			)
			require.NotPanics(t, func() {
				tree, err = h.manager.loadTreeFromRedis(h.ctx, sessionID)
			})
			require.Error(t, err, "corrupt root must surface an error, not a panic or a broken tree")
			require.Nil(t, tree)
			require.False(t, h.exists(h.claimedRootKey(sessionID)),
				"corrupt key must be deleted so a subsequent attempt does not keep tripping on it")
		})
	}
}

// TestMigrateLegacySMSTKeys_CorruptLegacyRoot_DeletesAsOrphan covers the
// migration path. Jonathan's deployment jumped schemas: legacy roots from
// the pre-refactor code could exist at unexpected sizes. A corrupt legacy
// root must not panic the migration; it must be counted as orphaned.
func TestMigrateLegacySMSTKeys_CorruptLegacyRoot_DeletesAsOrphan(t *testing.T) {
	for _, n := range corruptRootLengths {
		if n == 0 {
			// Zero-length already deleted by the existing "already gone" branch.
			continue
		}
		n := n
		t.Run(fmt.Sprintf("root_len=%d", n), func(t *testing.T) {
			h := newDefensiveHarness(t)
			defer h.cleanup()
			sessionID := fmt.Sprintf("session_legacy_corrupt_%d", n)

			legacyRootKey := h.redisClient.KB().SMSTNodesPrefix() + sessionID + ":root"
			legacyNodesKey := h.redisClient.KB().SMSTNodesPrefix() + sessionID + ":nodes"
			legacyStatsKey := h.redisClient.KB().SMSTNodesPrefix() + sessionID + ":stats"

			corrupt := make([]byte, n)
			require.NoError(t, h.redisClient.Set(h.ctx, legacyRootKey, corrupt, 0).Err())
			require.NoError(t, h.redisClient.HSet(h.ctx, legacyNodesKey, "node1", "x").Err())
			require.NoError(t, h.redisClient.Set(h.ctx, legacyStatsKey, "0:0", 0).Err())

			var (
				stats LegacySMSTMigrationStats
				err   error
			)
			require.NotPanics(t, func() {
				stats, err = MigrateLegacySMSTKeys(h.ctx, zerolog.Nop(), h.redisClient)
			})
			require.NoError(t, err)
			require.Equal(t, 1, stats.LegacyRootsScanned)
			require.Equal(t, 0, stats.SessionsMigrated)
			require.Equal(t, 1, stats.SessionsOrphaned,
				"corrupt legacy root must count as orphan, not migrate, not panic")

			for _, k := range []string{legacyRootKey, legacyNodesKey, legacyStatsKey} {
				require.Falsef(t, h.exists(k),
					"corrupt legacy key %s must be deleted so migration is idempotent", k)
			}
		})
	}
}

// TestSessionSnapshot_DecodeCorruptClaimedRootHash covers the persistence
// layer: even if a malformed root somehow lands in the session snapshot
// hash (manual intervention, pre-upgrade bug, etc.), the decoder drops it
// so it never reaches the claim-submit path.
func TestSessionSnapshot_DecodeCorruptClaimedRootHash(t *testing.T) {
	for _, n := range []int{1, 10, 47, 49} {
		n := n
		t.Run(fmt.Sprintf("len=%d", n), func(t *testing.T) {
			fields := map[string]string{
				hfSessionID:          "session_xyz",
				hfSupplierOperator:   "pokt1whatever",
				hfServiceID:          "svc",
				hfApplicationAddress: "pokt1app",
				hfSessionStartHeight: "100",
				hfSessionEndHeight:   "160",
				hfState:              "active",
				hfRelayCount:         "0",
				hfTotalComputeUnits:  "0",
				hfCreatedAt:          "2026-04-16T00:00:00.000000000Z",
				hfLastUpdatedAt:      "2026-04-16T00:00:00.000000000Z",
				hfClaimedRootHash:    string(make([]byte, n)),
			}
			snap, err := decodeSnapshot(fields)
			require.NoError(t, err)
			require.Nil(t, snap.ClaimedRootHash,
				"malformed ClaimedRootHash must be dropped at decode time")
		})
	}

	// Happy path: a 48-byte blob is preserved byte-for-byte.
	validFields := map[string]string{
		hfSessionID:          "session_xyz",
		hfSupplierOperator:   "pokt1whatever",
		hfServiceID:          "svc",
		hfApplicationAddress: "pokt1app",
		hfSessionStartHeight: "100",
		hfSessionEndHeight:   "160",
		hfState:              "active",
		hfRelayCount:         "0",
		hfTotalComputeUnits:  "0",
		hfCreatedAt:          "2026-04-16T00:00:00.000000000Z",
		hfLastUpdatedAt:      "2026-04-16T00:00:00.000000000Z",
		hfClaimedRootHash:    string(validRoot()),
	}
	snap, err := decodeSnapshot(validFields)
	require.NoError(t, err)
	require.True(t, bytes.Equal(validRoot(), snap.ClaimedRootHash),
		"well-formed root must pass through unchanged")
}
