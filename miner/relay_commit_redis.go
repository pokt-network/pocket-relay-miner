package miner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// redisRelayCommitter is the relayCommitter of a Redis-backed miner: one Lua
// script per session batch over the session hash, the dedup set and the
// supplier's stream, and one XACKDEL for rejected entries.
type redisRelayCommitter struct {
	client     *redisutil.Client
	store      *RedisSessionStore
	dedup      *RedisDeduplicator
	streamName string
	group      string
}

// newRedisRelayCommitter returns nil -- an untyped nil, so a caller's nil check
// sees it -- when the deduplicator is not the Redis one whose set the script
// writes; the supplier then finishes every relay on its own, as before the
// batch existed.
//
// The script touches the session hash, the dedup set and the stream, three keys
// with no hash tag, which a Redis Cluster refuses as CROSSSLOT. That is not
// handled on purpose: no deployment of this miner runs a cluster and none is
// planned (Jorge, 2026-09-10). Session creation has the same limit already --
// CreateIfAbsent's index pipeline spans two slots -- which was read in
// go-redis's source, not run against a cluster.
func newRedisRelayCommitter(
	client *redisutil.Client,
	store *RedisSessionStore,
	dedup Deduplicator,
	consumer *redisutil.StreamsConsumer,
) relayCommitter {
	redisDedup, ok := dedup.(*RedisDeduplicator)
	if !ok || redisDedup == nil {
		return nil
	}
	return &redisRelayCommitter{
		client:     client,
		store:      store,
		dedup:      redisDedup,
		streamName: consumer.StreamName(),
		group:      consumer.ConsumerGroup(),
	}
}

// CommitSession runs relayBatchScript, with the rescue IncrementRelayCount has
// for a pre-hash session: rewrite it and run again. The script refused before
// writing, so nothing is doubled.
//
// Two errors it reports as errCommitRefused are not script refusals, and are
// mapped that way because the batch has always finished them one at a time: a
// failed migration (which may be transient; whether it can leave part of the
// rewritten session written is not verified), and an answer without four values (unreachable with this
// script, which always returns four; were it reached, the script's writes
// would have happened).
func (c *redisRelayCommitter) CommitSession(ctx context.Context, sessionID string, relays []batchedRelay) (relayBatchResult, error) {
	result, err := c.runScript(ctx, sessionID, relays)
	if isLegacyKeyErr(err) {
		if migrateErr := c.store.migrateLegacyKey(ctx, sessionID); migrateErr != nil {
			return relayBatchResult{}, fmt.Errorf("%w: legacy session key could not be migrated: %w", errCommitRefused, migrateErr)
		}
		result, err = c.runScript(ctx, sessionID, relays)
	}
	if isLegacyKeyErr(err) || isRelayBatchRefusal(err) {
		return relayBatchResult{}, fmt.Errorf("%w: %w", errCommitRefused, err)
	}
	return result, err
}

// AckRejected acknowledges the entries in one XACKDEL. It goes to the client,
// not through the consumer, so it does not depend on the consumer being open.
func (c *redisRelayCommitter) AckRejected(ctx context.Context, ids []string) error {
	return c.client.XAckDel(ctx, c.streamName, c.group, "DELREF", ids...).Err()
}

func (c *redisRelayCommitter) runScript(ctx context.Context, sessionID string, relays []batchedRelay) (relayBatchResult, error) {
	keys := []string{
		c.store.sessionKey(sessionID),
		c.dedup.sessionKey(sessionID),
		c.streamName,
	}
	args := make([]any, 0, 5+3*len(relays))
	args = append(args,
		c.group,
		time.Now().Format(time.RFC3339Nano),
		int64(c.store.config.SessionTTL.Seconds()),
		int64(c.dedup.getTTL().Seconds()),
		len(relays),
	)
	for _, r := range relays {
		args = append(args, r.id, hashMember(r.hash), r.computeUnits)
	}

	vals, err := relayBatchScript.Run(ctx, c.client, keys, args...).Int64Slice()
	if err != nil {
		return relayBatchResult{}, err
	}
	if len(vals) != 4 {
		return relayBatchResult{}, fmt.Errorf("relay batch: script returned %d values, expected 4", len(vals))
	}
	return relayBatchResult{status: vals[0], newRelays: vals[1], newComputeUnits: vals[2], freshDups: vals[3]}, nil
}

func isLegacyKeyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "legacy key")
}

// isRelayBatchRefusal matches the refusals relayBatchScript makes before its
// first write, all of which it would make again.
func isRelayBatchRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "relay batch:")
}

// relayBatchScript marks, counts and acknowledges one session's batch in one
// call.
//
// KEYS[1] = session hash, KEYS[2] = dedup set, KEYS[3] = stream
// ARGV[1] = consumer group, ARGV[2] = RFC3339Nano now, ARGV[3] = session TTL s,
// ARGV[4] = dedup TTL s, ARGV[5] = n, then n triples (id, relay hash, compute
// units): triple j is ARGV[3j+3], ARGV[3j+4], ARGV[3j+5].
//
// Returns {status, new relays, their compute units, fresh duplicates}; status
// is incrementRelayCountScript's: 0 counted, 1 session not found, 2 terminal.
// A fresh duplicate is a relay the SADD already had whose entry XACKDEL
// acknowledged in this run (it answers 1 per id acknowledged and deleted, -1
// per id not there -- measured on 8.10.0): a copy delivered here after another
// consumer finished the relay, not an entry a lost-answer run already took.
//
// Everything that can refuse is checked BEFORE the first write, because a
// script that fails halfway is not rolled back (measured on Redis 8.10.0: a
// SADD before a WRONGTYPE stayed written). XACKDEL does not fail on a missing
// stream, group or id -- it answers -1 (measured on 8.10.0, 2026-09-10) -- so
// once the writes start, nothing after them refuses.
//
// The branches copy the per-relay path: there the dedup mark comes first and the
// counter second, so a missing or terminal session gets its relays marked and
// acknowledged but not counted. Counting only what the SADD added is what keeps
// a redelivery from counting twice, in either order of two consumers; and
// adding each new member's OWN compute units matters because one session can
// hold relays mined at different CUPRs.
//
// Re-running it is harmless: the SADD adds nothing, so nothing is counted, and
// the ids are already gone, so nothing is a duplicate either. An error whose
// outcome is unknown can be retried.
var relayBatchScript = redis.NewScript(luaIsTerminal + `
local n = tonumber(ARGV[5])
if n == nil or n < 1 or #ARGV ~= 5 + 3 * n then
	return redis.error_reply('relay batch: malformed arguments')
end
local total = 0
for j = 1, n do
	local cu = tonumber(ARGV[3 * j + 5])
	if cu == nil or cu < 0 then
		return redis.error_reply('relay batch: bad compute units')
	end
	total = total + cu
end
if total > 9007199254740991 then
	return redis.error_reply('relay batch: compute units exceed exact integer range')
end
local ktype = redis.call('TYPE', KEYS[1])['ok']
if ktype ~= 'hash' and ktype ~= 'none' then
	return redis.error_reply('legacy key')
end
local stype = redis.call('TYPE', KEYS[3])['ok']
if stype ~= 'stream' and stype ~= 'none' then
	return redis.error_reply('relay batch: stream key is not a stream')
end

local new_relays, new_cu = 0, 0
local marked_before = {}
for j = 1, n do
	if redis.call('SADD', KEYS[2], ARGV[3 * j + 4]) == 1 then
		new_relays = new_relays + 1
		new_cu = new_cu + tonumber(ARGV[3 * j + 5])
	else
		marked_before[j] = true
	end
end
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[4]))

local status = 0
if ktype == 'none' then
	status = 1
elseif is_terminal(redis.call('HGET', KEYS[1], 'state')) then
	status = 2
elseif new_relays > 0 then
	redis.call('HINCRBY', KEYS[1], 'relay_count', new_relays)
	redis.call('HINCRBY', KEYS[1], 'total_compute_units', new_cu)
	redis.call('HSET', KEYS[1], 'last_updated_at', ARGV[2])
	redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
end

local fresh_dups = 0
local first = 1
while first <= n do
	local last = math.min(first + 999, n)
	local cmd = {'XACKDEL', KEYS[3], ARGV[1], 'DELREF', 'IDS', last - first + 1}
	for j = first, last do
		cmd[#cmd + 1] = ARGV[3 * j + 3]
	end
	local acked = redis.call(unpack(cmd))
	for k = 1, #acked do
		if acked[k] == 1 and marked_before[first + k - 1] then
			fresh_dups = fresh_dups + 1
		end
	end
	first = last + 1
end

return {status, new_relays, new_cu, fresh_dups}
`)
