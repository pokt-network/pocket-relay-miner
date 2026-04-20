package miner

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// LegacySMSTMigrationStats summarizes a legacy-to-new SMST key migration.
type LegacySMSTMigrationStats struct {
	LegacyRootsScanned int
	SessionsMigrated   int
	SessionsOrphaned   int // legacy keys with no matching session owner - deleted
	LegacyKeysDeleted  int

	// LegacyLiveRootOrphans counts legacy sessions that had ONLY a
	// live_root (no claimed_root) — these cannot be rescued because the
	// owner-lookup needs a flushed claimed_root_hash to match. They are
	// deleted so Redis memory does not accumulate across upgrades.
	LegacyLiveRootOrphans int
}

// MigrateLegacySMSTKeys scans Redis for SMST keys written under the pre-fix
// schema (keyed only by sessionID, without supplier segment) and tries to
// rescue each session by assigning the legacy root to the supplier whose
// session metadata `claimed_root_hash` matches.
//
// Context: before the per-supplier key fix, multiple suppliers in the same
// session shared `ha:smst:{sessionID}:{root|nodes|stats}`. Only the last
// supplier to flush landed in Redis — the other suppliers' roots were
// overwritten. After the upgrade, the new code reads
// `ha:smst:{supplier}:{sessionID}:root` which does not exist, so claims
// expire with PROOF_MISSING (stake drain).
//
// This migration rescues the one supplier we CAN identify (the last flusher):
//
//  1. For each legacy `:root` key, read the stored root R.
//  2. Scan session metadata `ha:miner:sessions:{supplier}:{sessionID}` across
//     all known suppliers and compare their `claimed_root_hash` to R.
//  3. If exactly one supplier matches — the last flusher — RENAME the legacy
//     keys under the new schema so that supplier's lazy-load finds them and
//     the proof completes.
//  4. If zero or more than one match, delete the legacy keys. The other
//     suppliers in the session are NOT rescued (their tree was never written
//     to Redis intact) and their claims will expire — this is the known
//     one-time cost of the upgrade.
//
// The function is idempotent: keys already in the new schema are ignored.
// Running it again after a completed migration is a no-op.
func MigrateLegacySMSTKeys(
	ctx context.Context,
	logger logging.Logger,
	client *redisutil.Client,
) (LegacySMSTMigrationStats, error) {
	var stats LegacySMSTMigrationStats

	kb := client.KB()
	prefix := kb.SMSTNodesPrefix() // "ha:smst:"

	// Scan only the per-session :root markers — cheaper than scanning the
	// big :nodes hashes. For every legacy root found, we handle its matching
	// :nodes and :stats in migrateOneLegacySession.
	pattern := prefix + "*:root"
	var cursor uint64

	for {
		keys, next, err := client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return stats, fmt.Errorf("scan legacy smst root keys: %w", err)
		}
		for _, rootKey := range keys {
			// Parse: "ha:smst:{a}:{b}:root"
			//   old schema: {a}={sessionID}, no second segment → rest = "sessionID"
			//   new schema: {a}={supplier}, {b}={sessionID}          → rest = "supplier:sessionID"
			rest := strings.TrimPrefix(rootKey, prefix)
			rest = strings.TrimSuffix(rest, ":root")
			if strings.ContainsRune(rest, ':') {
				continue // already on new schema, skip
			}
			sessionID := rest
			stats.LegacyRootsScanned++

			if err := migrateOneLegacySession(ctx, logger, client, sessionID, &stats); err != nil {
				logger.Warn().
					Err(err).
					Str("session_id", sessionID).
					Msg("failed to migrate legacy SMST key (continuing)")
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	// Second pass: clean up legacy sessions with ONLY a live_root (no
	// claimed_root). These are mid-session checkpoints that were never
	// flushed. The :root-keyed scan above cannot see them, but leaving
	// the orphans in Redis leaks memory across upgrades. They cannot be
	// rescued because findSessionOwnerByRoot needs a flushed
	// claimed_root_hash to match — a mid-session live_root has no such
	// counterpart in session metadata. Delete the triplet (live_root +
	// nodes + stats if present) and count them for operator visibility.
	cursor = 0
	livePattern := prefix + "*:live_root"
	for {
		keys, next, err := client.Scan(ctx, cursor, livePattern, 500).Result()
		if err != nil {
			return stats, fmt.Errorf("scan legacy smst live_root keys: %w", err)
		}
		for _, liveKey := range keys {
			rest := strings.TrimPrefix(liveKey, prefix)
			rest = strings.TrimSuffix(rest, ":live_root")
			if strings.ContainsRune(rest, ':') {
				continue // already on new schema, skip
			}
			sessionID := rest
			// If the :root counterpart exists it was already handled in
			// the first pass (its live_root got renamed alongside the
			// other three keys). Skip.
			if ok, _ := client.Exists(ctx, prefix+sessionID+":root").Result(); ok > 0 {
				continue
			}
			stats.LegacyLiveRootOrphans++
			legacyNodesKey := prefix + sessionID + ":nodes"
			legacyStatsKey := prefix + sessionID + ":stats"
			deleted, err := client.Del(ctx, liveKey, legacyNodesKey, legacyStatsKey).Result()
			if err != nil {
				logger.Warn().
					Err(err).
					Str("session_id", sessionID).
					Msg("failed to delete orphaned legacy live_root (continuing)")
				continue
			}
			stats.LegacyKeysDeleted += int(deleted)
			logger.Warn().
				Str("session_id", sessionID).
				Msg("deleted orphaned legacy live_root (mid-session checkpoint cannot be rescued - claim will start empty)")
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	logger.Info().
		Int("legacy_roots_scanned", stats.LegacyRootsScanned).
		Int("sessions_migrated", stats.SessionsMigrated).
		Int("sessions_orphaned", stats.SessionsOrphaned).
		Int("legacy_live_root_orphans", stats.LegacyLiveRootOrphans).
		Int("legacy_keys_deleted", stats.LegacyKeysDeleted).
		Msg("legacy SMST key migration complete")

	return stats, nil
}

// migrateOneLegacySession processes a single legacy sessionID. It either
// renames the three legacy keys under the new schema (if an owner is
// identified) or deletes them (if not).
func migrateOneLegacySession(
	ctx context.Context,
	logger logging.Logger,
	client *redisutil.Client,
	sessionID string,
	stats *LegacySMSTMigrationStats,
) error {
	kb := client.KB()
	legacyPrefix := kb.SMSTNodesPrefix()
	legacyRootKey := legacyPrefix + sessionID + ":root"
	legacyNodesKey := legacyPrefix + sessionID + ":nodes"
	legacyStatsKey := legacyPrefix + sessionID + ":stats"
	legacyLiveRootKey := legacyPrefix + sessionID + ":live_root"

	rootBytes, err := client.Get(ctx, legacyRootKey).Bytes()
	if err == redis.Nil || len(rootBytes) == 0 {
		// Root already gone (e.g. a previous migration pass). Clean any
		// orphaned nodes/stats/live_root so they don't accumulate Redis memory.
		deleteLegacyTriplet(ctx, client,
			legacyRootKey, legacyNodesKey, legacyStatsKey, legacyLiveRootKey, stats)
		stats.SessionsOrphaned++
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy root: %w", err)
	}

	// A root with the wrong length cannot represent a valid SMST payload: the
	// smt library would panic on import (slice bounds out of range). Treat it
	// as if the session had no rescuable state — delete the triplet and move
	// on. The one-time loss is the same as any other orphan.
	if len(rootBytes) != SMSTRootLen {
		logger.Warn().
			Str("session_id", sessionID).
			Int("got_len", len(rootBytes)).
			Int("want_len", SMSTRootLen).
			Str("root_hex", fmt.Sprintf("%x", rootBytes)).
			Msg("legacy SMST root has invalid length - deleting (claim will expire)")
		deleteLegacyTriplet(ctx, client,
			legacyRootKey, legacyNodesKey, legacyStatsKey, legacyLiveRootKey, stats)
		stats.SessionsOrphaned++
		return nil
	}

	owner, err := findSessionOwnerByRoot(ctx, client, sessionID, rootBytes)
	if err != nil {
		return fmt.Errorf("scan session metadata for %s: %w", sessionID, err)
	}
	if owner == "" {
		// No supplier's session metadata matches the legacy root. Either:
		//  - the owner's session was already cleaned up (terminal state), or
		//  - the miner's supplier_registry has not yet indexed the owner.
		// In either case we can't safely rescue; delete and log.
		logger.Warn().
			Str("session_id", sessionID).
			Str("root_hex", fmt.Sprintf("%x", rootBytes)).
			Msg("legacy SMST root has no matching session owner - deleting (claim will expire)")
		deleteLegacyTriplet(ctx, client,
			legacyRootKey, legacyNodesKey, legacyStatsKey, legacyLiveRootKey, stats)
		stats.SessionsOrphaned++
		return nil
	}

	newRootKey := kb.SMSTRootKey(owner, sessionID)
	newNodesKey := kb.SMSTNodesKey(owner, sessionID)
	newStatsKey := kb.SMSTStatsKey(owner, sessionID)
	newLiveRootKey := kb.SMSTLiveRootKey(owner, sessionID)

	if err := client.Rename(ctx, legacyRootKey, newRootKey).Err(); err != nil {
		return fmt.Errorf("rename root: %w", err)
	}
	if exists, _ := client.Exists(ctx, legacyStatsKey).Result(); exists > 0 {
		if err := client.Rename(ctx, legacyStatsKey, newStatsKey).Err(); err != nil {
			logger.Warn().Err(err).Str("session_id", sessionID).
				Msg("failed to rename legacy stats key (continuing)")
		}
	}
	if exists, _ := client.Exists(ctx, legacyNodesKey).Result(); exists > 0 {
		if err := client.Rename(ctx, legacyNodesKey, newNodesKey).Err(); err != nil {
			logger.Warn().Err(err).Str("session_id", sessionID).
				Msg("failed to rename legacy nodes key (continuing)")
		}
	}
	// live_root: written on every UpdateTree, can be present even when
	// the session was flushed (up until DeleteTree clears it). Without
	// this rename, mid-session checkpoints from before the upgrade were
	// orphaned under the legacy key — the new code would look for
	// ha:smst:{supplier}:{sessionID}:live_root, find nothing, and start a
	// fresh tree on the next relay, dropping all pre-migration relays.
	if exists, _ := client.Exists(ctx, legacyLiveRootKey).Result(); exists > 0 {
		if err := client.Rename(ctx, legacyLiveRootKey, newLiveRootKey).Err(); err != nil {
			logger.Warn().Err(err).Str("session_id", sessionID).
				Msg("failed to rename legacy live_root key (continuing)")
		}
	}

	stats.SessionsMigrated++
	logger.Info().
		Str("session_id", sessionID).
		Str("supplier", owner).
		Str("root_hex", fmt.Sprintf("%x", rootBytes)).
		Msg("migrated legacy SMST session to per-supplier schema")
	return nil
}

// findSessionOwnerByRoot scans `ha:miner:sessions:*:{sessionID}` entries and
// returns the supplier address whose `claimed_root_hash` equals wantRoot.
// Returns "" if there is no unambiguous match.
func findSessionOwnerByRoot(
	ctx context.Context,
	client *redisutil.Client,
	sessionID string,
	wantRoot []byte,
) (string, error) {
	sessionsPrefix := client.KB().MinerSessionsPrefix() // "ha:miner:sessions"
	scanPattern := sessionsPrefix + ":*:" + sessionID

	var cursor uint64
	var matches []string

	for {
		keys, next, err := client.Scan(ctx, cursor, scanPattern, 500).Result()
		if err != nil {
			return "", err
		}
		for _, skey := range keys {
			// Expected: "ha:miner:sessions:{supplier}:{sessionID}" (exactly 2 segments after prefix).
			// Reject state keys like "ha:miner:sessions:{supplier}:state:{state}"
			// which would appear as 3 segments.
			after := strings.TrimPrefix(skey, sessionsPrefix+":")
			parts := strings.Split(after, ":")
			if len(parts) != 2 || parts[1] != sessionID {
				continue
			}
			got, err := client.HGet(ctx, skey, hfClaimedRootHash).Result()
			if err == redis.Nil || err != nil {
				continue
			}
			if bytes.Equal([]byte(got), wantRoot) {
				matches = append(matches, parts[0])
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	default:
		// 0 → no owner found. >1 → ambiguous (should not happen with cryptographic roots).
		return "", nil
	}
}

// deleteLegacyTriplet deletes the four legacy SMST keys (root, nodes,
// stats, live_root) for a session that cannot be rescued. Named
// "triplet" for historical reasons; the live_root was added in the
// 2026-04-19 fix to prevent orphaned mid-session checkpoints from
// accumulating in Redis across upgrades.
func deleteLegacyTriplet(
	ctx context.Context,
	client *redisutil.Client,
	rootKey, nodesKey, statsKey, liveRootKey string,
	stats *LegacySMSTMigrationStats,
) {
	deleted, err := client.Del(ctx, rootKey, nodesKey, statsKey, liveRootKey).Result()
	if err == nil {
		stats.LegacyKeysDeleted += int(deleted)
	}
}
