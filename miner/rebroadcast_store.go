package miner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// RebroadcastPhase distinguishes the claim and proof phases sharing the same
// store + reconciler machinery.
type RebroadcastPhase string

const (
	RebroadcastPhaseClaim RebroadcastPhase = "claim"
	RebroadcastPhaseProof RebroadcastPhase = "proof"
)

// RebroadcastGroup identifies one supplier's batch for a given session_end.
// All of an operator's sessions for an epoch share a session_end (global 60-block
// grid), so a group is the natural unit the inclusion reconciler verifies.
type RebroadcastGroup struct {
	Supplier   string
	SessionEnd int64
}

// RebroadcastStore persists the already-built MsgCreateClaim / MsgSubmitProof
// bytes so the inclusion reconciler can re-broadcast an accepted-but-not-yet
// included claim/proof while its window is open — without rebuilding from the
// SMST (which is deleted at submit) and surviving a leader failover (the bytes
// live in Redis, not an in-memory closure).
//
// Layout (per phase):
//   - payload hash:  ha:miner:rebroadcast:{phase}:{supplier}:{sessionEnd}
//     field = sessionID, value = marshaled proto message. TTL = ttl.
//   - group index:   ha:miner:rebroadcast:{phase}:index
//     set of "{supplier}:{sessionEnd}" members, for leader-failover recovery.
//
// All entries carry a TTL (≈ one window plus margin) so a missed terminal
// cleanup cannot leak Redis memory.
type RebroadcastStore struct {
	redisClient *redistransport.Client
	ttl         time.Duration
}

// NewRebroadcastStore creates a rebroadcast payload store. ttl should comfortably
// exceed the claim/proof window (a few minutes); 1h is a safe default.
func NewRebroadcastStore(redisClient *redistransport.Client, ttl time.Duration) *RebroadcastStore {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &RebroadcastStore{redisClient: redisClient, ttl: ttl}
}

// Keys embed the phase in a Redis Cluster hash-tag ({phase}) so a phase's group
// hashes and its index set always resolve to the same slot. This is required for
// the multi-key MULTI/EXEC (Put) and the multi-key Lua (Delete/CleanupIfEmpty)
// to be valid on a clustered deployment; on standalone Redis the braces are
// inert. See finding: cross-slot MULTI/EXEC.
func (s *RebroadcastStore) groupKey(phase RebroadcastPhase, supplier string, sessionEnd int64) string {
	return fmt.Sprintf("ha:miner:rebroadcast:{%s}:%s:%d", phase, supplier, sessionEnd)
}

func (s *RebroadcastStore) indexKey(phase RebroadcastPhase) string {
	return fmt.Sprintf("ha:miner:rebroadcast:{%s}:index", phase)
}

func (s *RebroadcastStore) indexMember(supplier string, sessionEnd int64) string {
	return fmt.Sprintf("%s:%d", supplier, sessionEnd)
}

// Put stores one session's built message and registers the group in the index.
// Idempotent: re-putting the same session overwrites the payload (e.g. after a
// rebroadcast produced a fresh tx — the message bytes are unchanged anyway).
func (s *RebroadcastStore) Put(ctx context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string, payload []byte) error {
	if s == nil || s.redisClient == nil {
		return nil
	}
	gk := s.groupKey(phase, supplier, sessionEnd)
	ik := s.indexKey(phase)

	pipe := s.redisClient.TxPipeline()
	pipe.HSet(ctx, gk, sessionID, payload)
	pipe.Expire(ctx, gk, s.ttl)
	pipe.SAdd(ctx, ik, s.indexMember(supplier, sessionEnd))
	pipe.Expire(ctx, ik, s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to persist rebroadcast payload (%s/%s/%d/%s): %w", phase, supplier, sessionEnd, sessionID, err)
	}
	return nil
}

// List returns the still-pending (not yet confirmed/cleaned) sessions for a
// group, mapping sessionID -> built message bytes.
func (s *RebroadcastStore) List(ctx context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64) (map[string][]byte, error) {
	if s == nil || s.redisClient == nil {
		return nil, nil
	}
	raw, err := s.redisClient.HGetAll(ctx, s.groupKey(phase, supplier, sessionEnd)).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list rebroadcast payloads (%s/%s/%d): %w", phase, supplier, sessionEnd, err)
	}
	out := make(map[string][]byte, len(raw))
	for sessionID, v := range raw {
		out[sessionID] = []byte(v)
	}
	return out, nil
}

// deleteScript atomically removes one field and, only if the hash is then empty,
// de-registers the group from the index. Running HDEL + the emptiness check +
// SREM inside one Lua eval closes the check-then-act TOCTOU: a concurrent Put
// (HSet + SAdd) cannot interleave between the HLEN check and the SREM, so a
// just-added live payload can never be orphaned. HDEL of the last field removes
// the hash key automatically, so no explicit DEL is needed.
//
//	KEYS[1] = group hash, KEYS[2] = index set
//	ARGV[1] = sessionID (field), ARGV[2] = index member
var deleteScript = redis.NewScript(`
redis.call('HDEL', KEYS[1], ARGV[1])
if redis.call('HLEN', KEYS[1]) == 0 then
  redis.call('SREM', KEYS[2], ARGV[2])
end
return 1
`)

// cleanupIfEmptyScript de-registers a group from the index iff its hash no longer
// exists (e.g. it was garbage-collected by its TTL backstop without Delete ever
// draining it). Reaps index members that would otherwise linger as ghost groups.
//
//	KEYS[1] = group hash, KEYS[2] = index set, ARGV[1] = index member
var cleanupIfEmptyScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('SREM', KEYS[2], ARGV[1])
end
return 1
`)

// Delete removes one session's payload after a terminal outcome (on-chain found
// or window closed), atomically de-registering the group from the index when it
// becomes empty.
func (s *RebroadcastStore) Delete(ctx context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string) error {
	if s == nil || s.redisClient == nil {
		return nil
	}
	gk := s.groupKey(phase, supplier, sessionEnd)
	ik := s.indexKey(phase)
	if err := deleteScript.Run(ctx, s.redisClient, []string{gk, ik}, sessionID, s.indexMember(supplier, sessionEnd)).Err(); err != nil {
		return fmt.Errorf("failed to delete rebroadcast payload (%s/%s/%d/%s): %w", phase, supplier, sessionEnd, sessionID, err)
	}
	return nil
}

// CleanupIfEmpty de-registers a (supplier, sessionEnd) group from the phase index
// when its payload hash no longer exists. Called by the reconciler after a pass
// finds no pending payloads for a group, so TTL-expired groups don't linger as
// ghost entries in ActiveGroups.
func (s *RebroadcastStore) CleanupIfEmpty(ctx context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64) error {
	if s == nil || s.redisClient == nil {
		return nil
	}
	gk := s.groupKey(phase, supplier, sessionEnd)
	ik := s.indexKey(phase)
	if err := cleanupIfEmptyScript.Run(ctx, s.redisClient, []string{gk, ik}, s.indexMember(supplier, sessionEnd)).Err(); err != nil {
		return fmt.Errorf("failed to cleanup empty rebroadcast group (%s/%s/%d): %w", phase, supplier, sessionEnd, err)
	}
	return nil
}

// ActiveGroups returns all registered (supplier, sessionEnd) groups for a phase.
// Used on leader failover to resume verification of in-flight batches the
// previous leader was tracking only in memory.
func (s *RebroadcastStore) ActiveGroups(ctx context.Context, phase RebroadcastPhase) ([]RebroadcastGroup, error) {
	if s == nil || s.redisClient == nil {
		return nil, nil
	}
	members, err := s.redisClient.SMembers(ctx, s.indexKey(phase)).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list rebroadcast groups (%s): %w", phase, err)
	}
	groups := make([]RebroadcastGroup, 0, len(members))
	for _, m := range members {
		// member = "{supplier}:{sessionEnd}". sessionEnd is the suffix after the
		// last ':'; supplier (bech32) contains no ':'.
		idx := strings.LastIndex(m, ":")
		if idx < 0 {
			continue
		}
		sessionEnd, convErr := strconv.ParseInt(m[idx+1:], 10, 64)
		if convErr != nil {
			continue
		}
		groups = append(groups, RebroadcastGroup{Supplier: m[:idx], SessionEnd: sessionEnd})
	}
	return groups, nil
}
