package miner

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
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

// relayBatchFlushBytes is how many relay bytes one supplier puts in its leaves
// before the consume loop flushes its batch without waiting for the tick. A leaf
// holds its relay until the commit a flush runs compacts it, so between two
// ticks a supplier of big relays held the whole interval's bytes: 1 MiB relays
// at ~450/s kept 3.0-3.6 GiB in leaves. Relays of a few KiB never reach it
// before the tick. Per supplier, so N busy suppliers hold about N times this.
const relayBatchFlushBytes = 16 << 20

// Why a batch flushed; the trigger label of relay_batch_flushes_total.
const (
	relayBatchFlushTime         = "time"  // the flush interval ticked
	relayBatchFlushCount        = "count" // a session reached relayBatchCap
	relayBatchFlushBytesTrigger = "bytes" // relayBatchFlushBytes were put in leaves
)

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
	// commit (relayCommitter.CommitSession; the Redis one runs a script):
	// whatever comes first has happened, whatever comes second has not.
	flushPointBeforeScript flushPoint = iota
	// flushPointFallbackRelay runs before each relay of the per-relay fallback.
	flushPointFallbackRelay
	// flushPointAfterScript follows a commit that ran: an error there stands
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
// together: one live_root checkpoint and one commit per session, instead of
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
	supplierAddr string
	dedup        Deduplicator
	smst         *RedisSMSTManager
	coordinator  *SessionCoordinator
	consumer     transport.MinedRelayConsumer
	commit       relayCommitter

	// hook is nil in production. A test sets it before the batch is used, to
	// cut a flush at a point (return an error) or to panic there.
	hook func(point flushPoint, id string) error

	mu       sync.Mutex
	sessions map[string]*sessionBatch

	// acks are the stream entries of rejected relays -- dropped before the tree
	// or refused by it -- waiting to be acknowledged together, in one
	// AckRejected (flushAcksLocked), instead of one each. Nothing about them is
	// counted or marked, so a crash before that only redelivers them, and the
	// redelivery is rejected again. Protected by mu.
	acks []string
}

// newRelayBatch returns nil when there is no committer (newRedisRelayCommitter
// returns none for a deduplicator that is not the Redis one): the supplier then
// finishes every relay on its own, as before the batch existed.
func newRelayBatch(
	logger logging.Logger,
	supplierAddr string,
	dedup Deduplicator,
	smst *RedisSMSTManager,
	coordinator *SessionCoordinator,
	consumer transport.MinedRelayConsumer,
	commit relayCommitter,
) *relayBatch {
	if commit == nil {
		return nil
	}
	return &relayBatch{
		logger:       logging.ForSupplierComponent(logger, "relay_batch", supplierAddr),
		supplierAddr: supplierAddr,
		dedup:        dedup,
		smst:         smst,
		coordinator:  coordinator,
		consumer:     consumer,
		commit:       commit,
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
		RecordRelayBatchFlush(b.supplierAddr, relayBatchFlushCount)
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

// flushAcksLocked acknowledges every waiting rejected entry in one AckRejected
// and reports whether they are still waiting: an error keeps them for the next
// flush. At most relayBatchCap ids go in one call, the size a session's commit
// already acknowledges.
func (b *relayBatch) flushAcksLocked(ctx context.Context) (retained bool) {
	if len(b.acks) == 0 {
		return false
	}
	if err := b.commit.AckRejected(ctx, b.acks); err != nil {
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
	if b.smst != nil {
		b.smst.ResetLeafBytesSinceFlush()
	}
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
// then the commit. The order is the money invariant: the commit acknowledges
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

	result, err := b.commit.CommitSession(ctx, sessionID, sb.relays)
	if err == nil && b.hook != nil {
		err = b.hook(flushPointAfterScript, sessionID)
	}
	switch {
	case err == nil:
		b.recordFlushed(sb, result)
		return flushDone
	case errors.Is(err, errCommitRefused):
		// Refused before the first write, and it would be refused again.
		b.logger.Debug().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("relay batch: commit refused the batch, finishing relays one at a time")
		return flushPerRelay
	default:
		b.logger.Debug().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("relay batch: flush failed, keeping the batch for the next flush")
		return flushRetry
	}
}

// relayBatchResult is what a relayCommitter reports for one session batch.
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
