package miner

import (
	"context"
	"encoding/json"
	"errors"
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
// RebroadcastStorage is the persistence the inclusion reconciler needs, stated
// as a contract instead of as a Redis client.
//
// NOTHING IN THESE FIVE SIGNATURES IS REDIS-SHAPED, and that is the finding
// rather than the design: they already spoke only in domain types --
// RebroadcastPhase, supplier, session end, session id, payload bytes -- so the
// abstraction was there and merely undeclared. Declaring it is not a redesign
// and deliberately changes no semantics, no TTL and no error behaviour.
//
// Why it is declared over the WHOLE store and not over the part being added
// today: a store with one injectable half and one half nailed to Redis is worse
// than either extreme, because whoever writes a second backing can substitute
// one half and not the other and ends up running two backings for one object.
// Half an abstraction costs more than none.
//
// The contract that is easy to lose when reimplementing it: List returns an
// empty map and no error for a group that does not exist -- absence is a normal
// state here, not a failure -- and Delete of something absent is a no-op for the
// same reason. Both are relied on by the reconciler on its ordinary path.
type RebroadcastStorage interface {
	Put(ctx context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string, payload []byte) error
	List(ctx context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64) (map[string][]byte, error)
	Delete(ctx context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string) error
	CleanupIfEmpty(ctx context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64) error
	ActiveGroups(ctx context.Context, phase RebroadcastPhase) ([]RebroadcastGroup, error)

	// The signed-transaction cache. It is part of THIS interface and not a
	// second one beside it: a backing that implemented one and not the other
	// would leave the reconciler running two stores for one object, which is
	// the half-abstraction this contract exists to prevent.
	//
	// Three semantics are load-bearing and easy to lose when reimplementing:
	// a MISS is not an error (nothing stored and store-unreachable both end in
	// signing, but only the second is worth reporting); the deadline comes back
	// WITH the bytes, because a signed transaction seals its own expiry and
	// nothing outside it can reconstruct that moment; and discarding what was
	// never stored is a no-op, because the caller invalidates on every failure
	// without first asking whether there is anything to discard.
	//
	// An implementation may REFUSE a payload -- the Redis one refuses those over
	// a size cap -- and that is not an error either: the next resend finds
	// nothing and signs, exactly as it does today.
	PutSignedTx(ctx context.Context, txHash string, txBytes []byte, timeoutAt time.Time, timeoutHeight int64) error
	GetSignedTx(ctx context.Context, txHash string) (txBytes []byte, timeoutAt time.Time, timeoutHeight int64, err error)
	DeleteSignedTx(ctx context.Context, txHash string) error
}

// The Redis implementation. The assertion sits here so that changing either the
// interface or this type fails at compile time rather than at wiring.
var _ RebroadcastStorage = (*RebroadcastStore)(nil)

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
	return s.redisClient.KB().RebroadcastKey(string(phase), supplier, sessionEnd)
}

func (s *RebroadcastStore) indexKey(phase RebroadcastPhase) string {
	return s.redisClient.KB().RebroadcastIndexKey(string(phase))
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
		// A member this loop cannot parse means the GROUP it names is never
		// reconciled again -- every pending claim or proof under it stops being
		// checked, silently, for as long as the malformed member sits in the
		// index. Both skips were bare continues.
		//
		// The signal is a metric and not a log because this store carries no
		// logger, and because a metric is what an operator can alert on: the
		// condition is bounded by a producer defect existing, so it must be
		// visible without turning on debug logging.
		idx := strings.LastIndex(m, ":")
		if idx < 0 {
			inclusionGroupAbandonedTotal.WithLabelValues(string(phase), abandonCauseIndexMalformed).Inc()
			continue
		}
		sessionEnd, convErr := strconv.ParseInt(m[idx+1:], 10, 64)
		if convErr != nil {
			inclusionGroupAbandonedTotal.WithLabelValues(string(phase), abandonCauseIndexMalformed).Inc()
			continue
		}
		groups = append(groups, RebroadcastGroup{Supplier: m[:idx], SessionEnd: sessionEnd})
	}
	return groups, nil
}

// maxSignedTxCacheBytes is the largest payload cached for re-injection.
//
// WHY A CAP AT ALL. Caching doubles the payload: the entry already holds
// MsgBytes, and this holds the signed transaction built around them. A proof
// message carries one whole relay verbatim -- request, response and both
// signatures -- so its size is the size of that service's traffic, and the
// relayer's own configured ceiling for a body is 10 MB by default (200 MB as
// the unconfigured fallback). One relay per proof transaction and 94 proof
// transactions in one measured window make the worst case large enough that
// nobody should discover it in production.
//
// AND THE DAMAGE IS SHARED, WHICH IS THE REAL ARGUMENT. This cache is
// discardable; the SMST nodes and the rebroadcast entries beside it are not.
// docs/REDIS.md recommends `maxmemory-policy: noeviction`, so a full instance
// does not quietly evict the cheap thing -- it REFUSES WRITES, including the
// ones that cost a claim. Under any eviction policy instead, the cache competes
// with the authority for survival. Either way an unbounded discardable cache
// spends a budget it does not own.
//
// WHERE THE NUMBER COMES FROM, and what was NOT measured. The only figure in
// hand is a floor: proof payloads of 1254-1340 bytes on localnet (n≈40, from
// the `proof_len` the miner already logs). The production distribution was NOT
// measured, on any network. 1 MiB admits roughly 800x that floor and excludes
// anything approaching a tenth of the default body ceiling. It is deliberately
// a constant and not a setting: this repository prefers a commit that changes a
// number over a knob that can be turned until it means something else, and the
// counter beside it is what makes the number reviewable.
const maxSignedTxCacheBytes = 1 << 20 // 1 MiB

// signedTxRecord is what the cache holds: the bytes, and the deadline that is
// already sealed inside them.
//
// The timestamp travels WITH the bytes and not on the rebroadcast entry, and
// that is not a filing preference. A signed transaction carries its own
// timeout_timestamp, which cannot be moved without signing a new one, so those
// bytes stop being usable at a moment fixed when they were built. Nothing on
// the entry can reconstruct it: the entry knows the budget it was given and the
// height it was submitted at, but the deadline is anchored to the chain's block
// time AT SIGNING plus a per-transaction nonce offset, and neither is recorded
// anywhere else. Reading it back out of the bytes would mean decoding a
// transaction on every resend to answer a question we already knew the answer
// to when we wrote them.
//
// Storing them together also makes the pair impossible to half-lose: one key,
// one TTL, so a reader never gets bytes whose expiry it cannot check.
type signedTxRecord struct {
	Bytes           []byte `json:"b"`
	TimeoutUnixNano int64  `json:"t"`
	// TimeoutHeight is the height the chain enforces, sealed into the same
	// bytes. It is stored rather than recomputed for the same reason as the
	// timestamp: a re-injection cannot change it, so a later pass deriving its
	// own would describe a transaction that does not exist.
	TimeoutHeight int64 `json:"h,omitempty"`
}

// PutSignedTx stores the signed, encoded bytes of one broadcast transaction so
// a later resend can re-inject them instead of signing a new transaction.
//
// Keyed by the transaction hash and NOT by the entry, which is the whole reason
// this lives beside the entries rather than inside them: one claim transaction
// carries a batch and produces one rebroadcast entry per session, all sharing
// that hash, so storing the blob per entry would keep N copies of one payload
// and turn discarding it into N writes that can disagree. One write settles it
// here.
//
// These bytes are a CACHE and never the authority. The entry's own MsgBytes
// remain the source of truth: losing this key -- to the TTL, to an eviction, to
// an older binary that never wrote it -- costs a signature, not a claim, and
// the resend falls back to signing exactly as it does today. That is why there
// is no error path here that a caller must handle differently from any other
// Redis failure.
func (s *RebroadcastStore) PutSignedTx(ctx context.Context, txHash string, txBytes []byte, timeoutAt time.Time, timeoutHeight int64) error {
	if s == nil || s.redisClient == nil || txHash == "" || len(txBytes) == 0 {
		return nil
	}
	// Above the cap the payload is simply not cached, and that is a decision
	// rather than a failure: the resend signs a fresh transaction, which is
	// exactly what it does today, so the caller has nothing different to do.
	if len(txBytes) > maxSignedTxCacheBytes {
		signedTxCacheSkippedTotal.Inc()
		return nil
	}
	blob, err := json.Marshal(signedTxRecord{Bytes: txBytes, TimeoutUnixNano: timeoutAt.UnixNano(), TimeoutHeight: timeoutHeight})
	if err != nil {
		return fmt.Errorf("failed to encode signed tx record (%s): %w", txHash, err)
	}
	k := s.redisClient.KB().TxSignedBytesKey(txHash)
	if err := s.redisClient.Set(ctx, k, blob, s.ttl).Err(); err != nil {
		return fmt.Errorf("failed to persist signed tx bytes (%s): %w", txHash, err)
	}
	return nil
}

// GetSignedTx returns the stored bytes for a transaction hash, or nil when
// there are none.
//
// A miss is NOT an error and the two are deliberately not conflated: "nobody
// stored these" and "Redis is unreachable" lead to the same action -- sign a
// fresh transaction -- but only the second is worth reporting, so the error is
// returned separately rather than folded into an empty result.
func (s *RebroadcastStore) GetSignedTx(ctx context.Context, txHash string) ([]byte, time.Time, int64, error) {
	if s == nil || s.redisClient == nil || txHash == "" {
		return nil, time.Time{}, 0, nil
	}
	k := s.redisClient.KB().TxSignedBytesKey(txHash)
	blob, err := s.redisClient.Get(ctx, k).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, time.Time{}, 0, nil
		}
		return nil, time.Time{}, 0, fmt.Errorf("failed to read signed tx bytes (%s): %w", txHash, err)
	}
	var rec signedTxRecord
	if uErr := json.Unmarshal(blob, &rec); uErr != nil {
		// A record that will not decode is treated as absent rather than as an
		// outage: the bytes are a cache, so the resend signs and moves on. It is
		// still reported, because the only way to write one is a defect on this
		// side -- nothing else writes this key.
		return nil, time.Time{}, 0, fmt.Errorf("failed to decode signed tx record (%s): %w", txHash, uErr)
	}
	return rec.Bytes, time.Unix(0, rec.TimeoutUnixNano), rec.TimeoutHeight, nil
}

// DeleteSignedTx discards the stored bytes for a transaction hash, so the next
// resend signs a fresh transaction instead of re-injecting bytes that cannot
// land.
//
// Deleting bytes that were never stored is a no-op, which matters because the
// caller invalidates on every failure without first asking whether anything is
// there to invalidate.
func (s *RebroadcastStore) DeleteSignedTx(ctx context.Context, txHash string) error {
	if s == nil || s.redisClient == nil || txHash == "" {
		return nil
	}
	k := s.redisClient.KB().TxSignedBytesKey(txHash)
	if err := s.redisClient.Del(ctx, k).Err(); err != nil {
		return fmt.Errorf("failed to discard signed tx bytes (%s): %w", txHash, err)
	}
	return nil
}
