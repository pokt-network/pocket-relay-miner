package miner

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// ErrRelayBatched is what the relay handler returns when it has handed the
// relay's stream entry to the supplier's relayBatch: a relay put in the SMST,
// whose duplicate mark, session counters and acknowledgement the batch does, or
// a rejected relay, which the batch only acknowledges. It is NOT a failure: the
// caller must neither acknowledge the entry (the batch does, when it flushes)
// nor hand it back.
var ErrRelayBatched = errors.New("relay handed to the batch: acknowledged when the batch flushes")

// relayBatchCap bounds how many relays one session holds before a flush is
// forced. It is also what bounds retention while flushes fail: at the cap, a
// relay the batch cannot take is finished on its own.
const relayBatchCap = 1000

// relaySession is what every relay of one session shares. It is kept once per
// session, not per relay, and only for the per-relay fallback, which calls the
// session coordinator with it.
type relaySession struct {
	sessionID   string
	supplier    string
	serviceID   string
	application string
	startHeight int64
	endHeight   int64
}

func relaySessionOf(m *transport.MinedRelayMessage) relaySession {
	return relaySession{
		sessionID:   m.SessionId,
		supplier:    m.SupplierOperatorAddress,
		serviceID:   m.ServiceId,
		application: m.ApplicationAddress,
		startHeight: m.SessionStartHeight,
		endHeight:   m.SessionEndHeight,
	}
}

// batchedRelay is all a relay leaves in memory once it is in the SMST: never the
// pooled message, which is returned to its pool as soon as the handler returns.
type batchedRelay struct {
	id           string // stream entry ID
	hash         []byte // relay hash; hashMember turns it into the dedup set's member
	computeUnits uint64
	gen          uint64 // generation of the tree UpdateTreeGen put the relay in
}

// takeOtherGenerations removes from the batch, and returns, the relays put in
// a tree of a generation other than gen.
func (sb *sessionBatch) takeOtherGenerations(gen uint64) (other []batchedRelay) {
	kept := sb.relays[:0]
	for _, r := range sb.relays {
		if r.gen == gen {
			kept = append(kept, r)
		} else {
			other = append(other, r)
		}
	}
	sb.relays = kept
	return other
}

// Why a batch hands relays back unacknowledged; the reason label of
// relay_batch_released_total.
const (
	// relayBatchReleasedNotResident: the session's tree is not resident here.
	relayBatchReleasedNotResident = "tree_not_resident"
	// relayBatchReleasedTreeReplaced: the relays went into a tree that was
	// evicted and replaced since.
	relayBatchReleasedTreeReplaced = "tree_replaced"
)

type sessionBatch struct {
	session relaySession
	relays  []batchedRelay
}

// flushPoint names the places a test can cut or panic a flush through
// relayBatch.hook.
type flushPoint int

const (
	// flushPointBeforeScript sits between the live_root checkpoint and the
	// script: whatever comes first has happened, whatever comes second has not.
	flushPointBeforeScript flushPoint = iota
	// flushPointFallbackRelay runs before each relay of the per-relay fallback.
	flushPointFallbackRelay
	// flushPointAfterScript follows a script that ran: an error there stands
	// for its answer lost on the way back, so the flush is retried.
	flushPointAfterScript
)

type flushOutcome int

const (
	flushDone     flushOutcome = iota // counted and acknowledged
	flushRetry                        // nothing known to be written: keep and retry
	flushRelease                      // hand the entries back unacknowledged
	flushPerRelay                     // finish each relay the way a lone relay is finished
)

// relayBatch holds, per session, the relays of one supplier that are already in
// the SMST but not yet marked, counted and acknowledged, and finishes them
// together: one live_root checkpoint and one script per session, instead of
// three round trips per relay.
//
// What is held is SAFE to lose to a crash: the entries are still pending in the
// stream, the dedup set does not have them yet, so whoever takes them next
// counts them exactly once. That is the whole reason the duplicate mark moves
// into the batch with the counter and the acknowledgement -- marked early, a
// redelivery after a crash would skip the counter forever.
//
// Age budget, because a pending entry older than claim_idle_timeout can be
// taken by another miner's reclaim even though this one is alive
// (StreamsConsumer.claimIdleFromOtherConsumers): the idle time counts from
// DELIVERY, so it is the time in the delivery channel (5000 slots; ~18 s under
// backlog was an estimate, not measured) plus up to one flush interval (15 s by
// default, validated to be at most a quarter of the idle timeout): about 33 s
// against 60 s. On the way out the batch is released, not flushed, so the exit
// adds nothing to it. Past the budget, the dedup set still keeps the count
// exact; what two miners can then both do is write the same tree, the same
// hazard a slow consumer has always had.
//
// Every flush and the release happen under mu, which the consume loop, its tick
// and the lifecycle's claim transition all take. That is what makes a session
// flush at most once at a time, and why entries never leave the map before a
// flush decided their fate: a retained batch is simply still there.
type relayBatch struct {
	logger       logging.Logger
	redisClient  *redisutil.Client
	supplierAddr string
	store        *RedisSessionStore
	dedup        *RedisDeduplicator
	smst         *RedisSMSTManager
	coordinator  *SessionCoordinator
	consumer     *redisutil.StreamsConsumer

	// hook is nil in production. A test sets it before the batch is used, to
	// cut a flush at a point (return an error) or to panic there.
	hook func(point flushPoint, id string) error

	mu       sync.Mutex
	sessions map[string]*sessionBatch

	// acks are the stream entries of rejected relays -- dropped before the tree
	// or refused by it -- waiting to be acknowledged together, in one XACKDEL
	// (flushAcksLocked), instead of one each. Nothing about them is counted or
	// marked, so a crash before that XACKDEL only redelivers them, and the
	// redelivery is rejected again. Protected by mu.
	acks []string
}

// newRelayBatch returns nil when the deduplicator is not the Redis one whose set
// the script writes; the supplier then finishes every relay on its own, as
// before the batch existed.
//
// The script touches the session hash, the dedup set and the stream, three keys
// with no hash tag, which a Redis Cluster refuses as CROSSSLOT. That is not
// handled on purpose: no deployment of this miner runs a cluster and none is
// planned (Jorge, 2026-09-10). Session creation has the same limit already --
// CreateIfAbsent's index pipeline spans two slots -- which was read in
// go-redis's source, not run against a cluster.
func newRelayBatch(
	logger logging.Logger,
	redisClient *redisutil.Client,
	supplierAddr string,
	store *RedisSessionStore,
	dedup Deduplicator,
	smst *RedisSMSTManager,
	coordinator *SessionCoordinator,
	consumer *redisutil.StreamsConsumer,
) *relayBatch {
	redisDedup, ok := dedup.(*RedisDeduplicator)
	if !ok || redisDedup == nil {
		return nil
	}
	return &relayBatch{
		logger:       logging.ForSupplierComponent(logger, "relay_batch", supplierAddr),
		redisClient:  redisClient,
		supplierAddr: supplierAddr,
		store:        store,
		dedup:        redisDedup,
		smst:         smst,
		coordinator:  coordinator,
		consumer:     consumer,
		sessions:     make(map[string]*sessionBatch),
	}
}

// Add takes one relay that is already in the SMST. It reports false when it
// will not -- the session already holds relayBatchCap relays and flushing them
// failed -- and the caller must then finish that relay itself.
func (b *relayBatch) Add(ctx context.Context, s relaySession, r batchedRelay) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	sb := b.sessions[s.sessionID]
	if sb != nil && len(sb.relays) >= relayBatchCap {
		if b.flushLocked(ctx, sb) {
			return false
		}
		sb = nil
	}
	if sb == nil {
		sb = &sessionBatch{session: s}
		b.sessions[s.sessionID] = sb
	}
	sb.relays = append(sb.relays, r)
	return true
}

// AddAck takes the stream entry of a rejected relay, to acknowledge with the
// next flush. It reports false when it will not -- relayBatchCap entries are
// already waiting and acknowledging them failed -- and the caller must then
// acknowledge that entry itself.
func (b *relayBatch) AddAck(ctx context.Context, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.acks) >= relayBatchCap && b.flushAcksLocked(ctx) {
		return false
	}
	b.acks = append(b.acks, id)
	return true
}

// flushAcksLocked acknowledges every waiting rejected entry in one XACKDEL and
// reports whether they are still waiting: an error keeps them for the next
// flush. At most relayBatchCap ids go in one call, the size the flush script
// already sends XACKDEL.
func (b *relayBatch) flushAcksLocked(ctx context.Context) (retained bool) {
	if len(b.acks) == 0 {
		return false
	}
	if err := b.redisClient.XAckDel(ctx, b.consumer.StreamName(), b.consumer.ConsumerGroup(), "DELREF", b.acks...).Err(); err != nil {
		b.logger.Debug().Err(err).Int("entries", len(b.acks)).
			Msg("relay batch: acknowledging rejected relays failed, keeping them for the next flush")
		return true
	}
	b.consumer.RecordAcked(len(b.acks))
	b.acks = b.acks[:0]
	return false
}

// FlushAll flushes every session held, and acknowledges the rejected relays
// waiting. A session whose flush fails with nothing known to be written stays
// held for the next one, as do rejected entries whose acknowledgement failed.
func (b *relayBatch) FlushAll(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sb := range b.sessions {
		b.flushLocked(ctx, sb)
	}
	b.flushAcksLocked(ctx)
}

// FlushSessions flushes the named sessions, for a caller about to read their
// counters -- the claim transition refreshes relay_count right after.
func (b *relayBatch) FlushSessions(ctx context.Context, sessionIDs []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range sessionIDs {
		if sb := b.sessions[id]; sb != nil {
			b.flushLocked(ctx, sb)
		}
	}
}

// ReleaseAll hands every held entry back to the group unacknowledged, without
// flushing, as the supplier's consume loop ends (Jorge, 2026-09-10: on the way
// out it only lets go of what it holds; flushing N sessions does not fit the
// exit window). Nothing is lost: the relays are in the tree, the dedup set never
// saw them, so the next consumer redelivers them into the tree and counts them
// once.
//
// Before letting go, it writes the live_root that covers them
// (CheckpointLiveRootOnExit): the next owner resumes from live_root, and a
// relay missing there comes back only if its entry is redelivered before the
// session is sealed. A checkpoint that fails does not hold the release.
//
// Rejected relays waiting are acknowledged, not released: released, they would
// only come back to be rejected again.
func (b *relayBatch) ReleaseAll(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushAcksLocked(ctx)
	for id, sb := range b.sessions {
		if _, err := b.smst.CheckpointLiveRootOnExit(ctx, id); err != nil {
			b.logger.Debug().Err(err).Str(logging.FieldSessionID, id).
				Msg("relay batch: exit live_root checkpoint failed, releasing anyway")
		}
		b.release(ctx, sb)
		delete(b.sessions, id)
	}
}

// AckAllAsLost acknowledges every held entry without counting it, for a
// supplier whose signing key was removed: nobody in this fleet can claim these
// relays, so releasing them would only leave them pending for a consumer that
// never comes. It mirrors drainDeliveryBuffer's key-removal branch -- acked,
// counted as dropped for want of a key, dedup and counters untouched -- and an
// entry whose acknowledgement fails stays pending, as it does there.
//
// Rejected relays waiting are acknowledged too, without being counted as
// dropped for want of a key: they were rejected, and counted as such, already.
func (b *relayBatch) AckAllAsLost(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushAcksLocked(ctx)
	for id, sb := range b.sessions {
		for _, r := range sb.relays {
			msg := transport.StreamMessage{ID: r.id, StreamName: b.consumer.StreamName()}
			if err := b.consumer.AckMessage(ctx, msg); err == nil {
				RecordRelayDroppedNoKey(b.supplierAddr, sb.session.serviceID)
			}
		}
		delete(b.sessions, id)
	}
}

// flushLocked settles one session and reports whether it is still held.
func (b *relayBatch) flushLocked(ctx context.Context, sb *sessionBatch) (retained bool) {
	switch b.flushSession(ctx, sb) {
	case flushRetry:
		return true
	case flushRelease:
		RecordRelayBatchReleased(b.supplierAddr, relayBatchReleasedNotResident, len(sb.relays))
		b.release(ctx, sb)
	case flushPerRelay:
		b.finishOneByOne(ctx, sb)
	}
	delete(b.sessions, sb.session.sessionID)
	return false
}

// flushSession runs the two round trips of a flush: the live_root checkpoint,
// then the script. The order is the money invariant: the script acknowledges
// the entries, an acknowledged entry is never delivered again, so the tree has
// to be reachable from a stored root that contains those relays BEFORE.
func (b *relayBatch) flushSession(ctx context.Context, sb *sessionBatch) (outcome flushOutcome) {
	sessionID := sb.session.sessionID

	// Jorge, 2026-09-10: one relay lost to a panic is acceptable, a batch is
	// not. A panic here costs the batch its shortcut, not its relays: they are
	// finished one at a time, where a panic loses only the relay that causes it.
	defer func() {
		if r := recover(); r != nil {
			logging.PanicRecoveriesTotal.WithLabelValues("relay_batch_flush").Inc()
			RecordRelayBatchPanic(b.supplierAddr)
			b.logger.Error().
				Str(logging.FieldSessionID, sessionID).
				Int("relays", len(sb.relays)).
				Str("panic_value", fmt.Sprintf("%v", r)).
				Str("stack_trace", string(debug.Stack())).
				Msg("PANIC RECOVERED flushing a relay batch — finishing its relays one at a time")
			outcome = flushPerRelay
		}
	}()

	resident, gen, err := b.smst.CheckpointLiveRoot(ctx, sessionID)
	if err != nil {
		b.logger.Debug().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("relay batch: live_root checkpoint failed, keeping the batch for the next flush")
		return flushRetry
	}
	if !resident {
		// No tree here to cover these relays: the session was deleted after it
		// ended, or its tree was evicted after corruption. Acknowledging would
		// lose them in the second case. Handed back, a redelivery is dropped as a
		// late relay in the first case and put back in the tree in the second.
		b.logger.Debug().Str(logging.FieldSessionID, sessionID).Int("relays", len(sb.relays)).
			Msg("relay batch: session tree not resident, handing the batch back unacknowledged")
		return flushRelease
	}

	// A relay that went into an earlier tree of this session -- evicted after
	// corruption, then replaced by one resumed from a live_root that may not
	// cover it -- is not in the tree just checkpointed. Acknowledged, it would
	// be gone; handed back, its redelivery puts it in this tree.
	if stale := sb.takeOtherGenerations(gen); len(stale) > 0 {
		b.logger.Debug().Str(logging.FieldSessionID, sessionID).Int("relays", len(stale)).
			Msg("relay batch: relays of a replaced session tree, handing them back unacknowledged")
		b.releaseRelays(ctx, sessionID, stale)
		RecordRelayBatchReleased(b.supplierAddr, relayBatchReleasedTreeReplaced, len(stale))
		if len(sb.relays) == 0 {
			return flushDone
		}
	}

	if b.hook != nil {
		if err := b.hook(flushPointBeforeScript, sessionID); err != nil {
			return flushRetry
		}
	}

	result, err := b.runScript(ctx, sb)
	if isLegacyKeyErr(err) {
		// Same rescue IncrementRelayCount has: rewrite the pre-hash session and
		// run again. The script refused before writing, so nothing is doubled.
		if migrateErr := b.store.migrateLegacyKey(ctx, sessionID); migrateErr != nil {
			b.logger.Debug().Err(migrateErr).Str(logging.FieldSessionID, sessionID).
				Msg("relay batch: legacy session key could not be migrated, finishing relays one at a time")
			return flushPerRelay
		}
		result, err = b.runScript(ctx, sb)
	}
	if err == nil && b.hook != nil {
		err = b.hook(flushPointAfterScript, sessionID)
	}
	switch {
	case err == nil:
		b.recordFlushed(sb, result)
		return flushDone
	case isLegacyKeyErr(err) || isRelayBatchRefusal(err):
		// Refused before the first write, and it would be refused again.
		b.logger.Debug().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("relay batch: script refused the batch, finishing relays one at a time")
		return flushPerRelay
	default:
		b.logger.Debug().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("relay batch: flush failed, keeping the batch for the next flush")
		return flushRetry
	}
}

// relayBatchResult is what relayBatchScript reports.
type relayBatchResult struct {
	status          int64 // 0 counted, 1 session not found, 2 session terminal
	newRelays       int64 // members the SADD added
	newComputeUnits int64 // their compute units
	// freshDups are the members the SADD already had whose entry this run
	// acknowledged: duplicates delivered to it. A run retried after its answer
	// was lost finds its own members added and its entries gone, and counts
	// none.
	freshDups int64
}

func (b *relayBatch) runScript(ctx context.Context, sb *sessionBatch) (relayBatchResult, error) {
	keys := []string{
		b.store.sessionKey(sb.session.sessionID),
		b.dedup.sessionKey(sb.session.sessionID),
		b.consumer.StreamName(),
	}
	args := make([]any, 0, 5+3*len(sb.relays))
	args = append(args,
		b.consumer.ConsumerGroup(),
		time.Now().Format(time.RFC3339Nano),
		int64(b.store.config.SessionTTL.Seconds()),
		int64(b.dedup.getTTL().Seconds()),
		len(sb.relays),
	)
	for _, r := range sb.relays {
		args = append(args, r.id, hashMember(r.hash), r.computeUnits)
	}

	vals, err := relayBatchScript.Run(ctx, b.redisClient, keys, args...).Int64Slice()
	if err != nil {
		return relayBatchResult{}, err
	}
	if len(vals) != 4 {
		return relayBatchResult{}, fmt.Errorf("relay batch: script returned %d values, expected 4", len(vals))
	}
	return relayBatchResult{status: vals[0], newRelays: vals[1], newComputeUnits: vals[2], freshDups: vals[3]}, nil
}

// recordFlushed moves the metrics the per-relay path moves once per relay, by
// the batch's size, on the same series.
func (b *relayBatch) recordFlushed(sb *sessionBatch, res relayBatchResult) {
	n := len(sb.relays)
	dedupMarked.Add(float64(n))
	b.consumer.RecordAcked(n)
	if res.freshDups > 0 {
		RecordRelaysRejected(b.supplierAddr, "duplicate", sb.session.serviceID, int(res.freshDups))
	}
	if res.status != 0 {
		// The per-relay path lands in the same place: marked and acknowledged,
		// not counted (supplier_worker.go, the OnRelayProcessed error branch).
		b.logger.Debug().
			Str(logging.FieldSessionID, sb.session.sessionID).
			Int64("status", res.status).
			Int("relays", n).
			Msg("relay batch: session missing (1) or terminal (2), relays marked and acknowledged without counting")
	}
}

// release hands every entry of the batch back to the group unacknowledged,
// the way drainDeliveryBuffer does.
func (b *relayBatch) release(ctx context.Context, sb *sessionBatch) {
	b.releaseRelays(ctx, sb.session.sessionID, sb.relays)
}

// releaseRelays hands the given entries of a session's batch back to the group
// unacknowledged.
func (b *relayBatch) releaseRelays(ctx context.Context, sessionID string, relays []batchedRelay) {
	failed := 0
	for _, r := range relays {
		msg := transport.StreamMessage{ID: r.id, StreamName: b.consumer.StreamName()}
		if err := b.consumer.ReleaseMessage(ctx, msg); err != nil {
			failed++
		}
	}
	if failed > 0 {
		b.logger.Debug().Str(logging.FieldSessionID, sessionID).Int("failed", failed).
			Msg("relay batch: some entries could not be released; they stay pending until this process restarts")
	}
}

// finishOneByOne finishes each relay of the batch the way a relay is finished
// without a batch: dedup mark, counters on the first processing, acknowledgement.
// A panic here loses only the relay that caused it, counted as lost and
// acknowledged -- the same decision handleStreamMessage applies to one relay.
func (b *relayBatch) finishOneByOne(ctx context.Context, sb *sessionBatch) {
	s := sb.session
	// Every relay below is acknowledged, so the tree's nodes go to Redis first.
	// The flush that fell back here has normally committed already, which makes
	// this send nothing; a panic before that commit is what it covers. Not
	// written, the relays are handed back instead of acknowledged.
	if resident, err := b.smst.CommitTree(ctx, s.sessionID); err != nil || !resident {
		b.logger.Debug().Err(err).Str(logging.FieldSessionID, s.sessionID).Bool("resident", resident).
			Msg("relay batch: the session tree could not be committed, handing the relays back unacknowledged")
		b.release(ctx, sb)
		return
	}
	for _, r := range sb.relays {
		msg := transport.StreamMessage{ID: r.id, StreamName: b.consumer.StreamName()}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					logging.PanicRecoveriesTotal.WithLabelValues("supplier_consume_relay").Inc()
					b.logger.Error().
						Str(logging.FieldSessionID, s.sessionID).
						Str("panic_value", fmt.Sprintf("%v", rec)).
						Str("stack_trace", string(debug.Stack())).
						Msg("PANIC RECOVERED finishing a batched relay — relay dropped, batch continuing")
					RecordRelayLostToPanic(b.supplierAddr, s.serviceID)
					if ackErr := b.consumer.AckMessage(ctx, msg); ackErr != nil {
						b.logger.Warn().Err(ackErr).Str(logging.FieldSessionID, s.sessionID).
							Msg("failed to ack a relay lost to a panic; it will be redelivered and panic again")
					}
				}
			}()
			if b.hook != nil {
				// This point only injects panics; its error has no meaning here.
				_ = b.hook(flushPointFallbackRelay, r.id)
			}
			countRelayOnce(ctx, b.logger, b.dedup, b.coordinator, b.supplierAddr, s, r.hash, r.computeUnits)
			if ackErr := b.consumer.AckMessage(ctx, msg); ackErr != nil {
				b.logger.Debug().Err(ackErr).Str("message_id", r.id).Msg("failed to acknowledge message")
			}
		}()
	}
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
