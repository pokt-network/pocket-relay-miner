package pebblequeue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// readBatch bounds how many entries one pass reads before it delivers them.
const readBatch = 256

// defaultReleaseDelay is the Redis consumer's default ClaimIdleTimeout.
const defaultReleaseDelay = 30 * time.Second

// Consumer is the miner's side of one supplier's queue. It implements
// transport.MinedRelayConsumer.
type Consumer struct {
	b        *Broker
	st       *stream
	supplier string
	prefix   []byte
	bufSize  int
	logger   logging.Logger

	// releaseDelay is how long a released entry waits before it is delivered
	// again: the idle timeout after which the Redis sweep takes back an entry
	// XNACK released.
	releaseDelay time.Duration

	mu sync.Mutex
	// cursor is the key of the last entry read; the next read starts after it.
	cursor   []byte
	pending  map[string]struct{}
	released []releasedEntry
	closed   bool

	// health and pause hold delivery while the store cannot take what the
	// miner writes for a relay, or the process admission says so; nil holds
	// nothing. Set before Consume.
	health redistransport.OperableSignal
	pause  redistransport.IngestionPause

	stopCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

// releasedEntry is an entry handed back, due for delivery again at due.
type releasedEntry struct {
	id  string
	due time.Time
}

// Consumer returns a consumer of the supplier's queue. One per supplier.
func (b *Broker) Consumer(cfg transport.ConsumerConfig) (*Consumer, error) {
	name := transport.SupplierStreamName(b.streamPrefix, cfg.SupplierOperatorAddress)
	st, err := b.stream(name)
	if err != nil {
		return nil, err
	}
	bufSize := int(cfg.ChannelBufferSize)
	if bufSize <= 0 {
		bufSize = 5000
	}
	releaseDelay := time.Duration(cfg.ClaimIdleTimeout) * time.Millisecond
	if releaseDelay <= 0 {
		releaseDelay = defaultReleaseDelay
	}
	stopCtx, stop := context.WithCancel(context.Background())
	return &Consumer{
		b:            b,
		st:           st,
		supplier:     cfg.SupplierOperatorAddress,
		prefix:       entryPrefixOf(name),
		bufSize:      bufSize,
		logger:       logging.ForSupplierComponent(b.logger, "pebble_queue_consumer", cfg.SupplierOperatorAddress),
		releaseDelay: releaseDelay,
		pending:      make(map[string]struct{}),
		stopCtx:      stopCtx,
		stop:         stop,
	}, nil
}

// Consume starts delivery. Called at most once.
func (c *Consumer) Consume(ctx context.Context) <-chan transport.StreamMessage {
	out := make(chan transport.StreamMessage, c.bufSize)
	ctx, cancel := context.WithCancel(ctx)
	stopWatch := context.AfterFunc(c.stopCtx, cancel)
	c.wg.Add(1)
	go logging.RecoverGoRoutine(c.logger, "pebble_queue_consume", func(ctx context.Context) {
		defer c.wg.Done()
		defer close(out)
		defer stopWatch()
		defer cancel()
		c.deliverLoop(ctx, out)
	})(ctx)
	return out
}

// SetStoreHealth holds delivery while health is not operable. Call it before
// Consume.
func (c *Consumer) SetStoreHealth(health redistransport.OperableSignal) {
	c.health = health
}

// SetIngestionPause holds delivery while pause is Paused. Call it before
// Consume.
func (c *Consumer) SetIngestionPause(pause redistransport.IngestionPause) {
	c.pause = pause
}

func (c *Consumer) deliverLoop(ctx context.Context, out chan<- transport.StreamMessage) {
	for {
		if redistransport.WaitOperable(ctx, c.health, c.pause) != nil {
			return
		}
		msgs, nextDue, err := c.nextBatch(time.Now())
		if err != nil {
			c.logger.Warn().Err(err).Msg("reading the relay queue failed; retrying on the next wake")
		}
		for i, msg := range msgs {
			select {
			case out <- msg:
			case <-ctx.Done():
				c.putBack(msgs[i:])
				return
			}
		}
		if len(msgs) > 0 {
			continue
		}
		var due <-chan time.Time
		var timer *time.Timer
		if !nextDue.IsZero() {
			timer = time.NewTimer(time.Until(nextDue))
			due = timer.C
		}
		select {
		case <-c.st.notify:
		case <-due:
		case <-ctx.Done():
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// putBack returns messages read but not delivered: they stay in the queue and
// are no longer pending, so the next read or a restart finds them.
func (c *Consumer) putBack(msgs []transport.StreamMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range msgs {
		delete(c.pending, m.ID)
		c.released = append(c.released, releasedEntry{id: m.ID})
		transport.ReleaseMinedRelayMessage(m.Message)
	}
}

// nextBatch reads what is due at now: released entries past their delay
// first, as reclaims, then entries after the cursor. Each is pending once
// returned. nextDue is when the next released entry falls due, zero for none.
func (c *Consumer) nextBatch(now time.Time) (out []transport.StreamMessage, nextDue time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	released := c.released
	c.released = nil
	for i, r := range released {
		if r.due.After(now) {
			c.released = append(c.released, r)
			if nextDue.IsZero() || r.due.Before(nextDue) {
				nextDue = r.due
			}
			continue
		}
		msg, ok, err := c.readLocked(r.id)
		if err != nil {
			c.released = append(c.released, released[i:]...)
			return out, nextDue, err
		}
		if !ok {
			continue // acknowledged since
		}
		msg.IsReclaim = true
		c.pending[r.id] = struct{}{}
		out = append(out, msg)
	}

	// An entry read here has never been delivered by this process. One a
	// previous run delivered and did not acknowledge comes back live too: its
	// dedup marks, counters and acknowledgement were committed together, so if
	// the acknowledgement is missing the marks are too, and it is processed as
	// new, which is what it is for the store.
	lower := c.prefix
	if c.cursor != nil {
		lower = append(append([]byte(nil), c.cursor...), 0)
	}
	iter, err := c.b.store.DB().NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: prefixEnd(c.prefix)})
	if err != nil {
		return out, nextDue, fmt.Errorf("pebblequeue: iterate: %w", err)
	}
	defer func() { _ = iter.Close() }()
	for valid := iter.First(); valid && len(out) < readBatch; valid = iter.Next() {
		key := iter.Key()
		c.cursor = append(c.cursor[:0], key...)
		id, ok := idOfKey(c.prefix, key)
		if !ok {
			continue
		}
		msg, err := c.decode(id, iter.Value())
		if err != nil {
			c.dropUndecodable(id, err)
			continue
		}
		c.pending[id] = struct{}{}
		out = append(out, msg)
	}
	return out, nextDue, iter.Error()
}

func (c *Consumer) decode(id string, value []byte) (transport.StreamMessage, error) {
	msg, err := redistransport.DecodeEntry(value)
	if err != nil {
		return transport.StreamMessage{}, err
	}
	redistransport.RecordConsumed(c.supplier, msg, len(value))
	return transport.StreamMessage{ID: id, StreamName: c.st.name, Message: msg}, nil
}

// readLocked reads one entry by ID; ok is false when it is no longer there.
func (c *Consumer) readLocked(id string) (transport.StreamMessage, bool, error) {
	key, err := c.key(id)
	if err != nil {
		return transport.StreamMessage{}, false, nil
	}
	value, closer, err := c.b.store.DB().Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return transport.StreamMessage{}, false, nil
	}
	if err != nil {
		return transport.StreamMessage{}, false, fmt.Errorf("pebblequeue: read %s: %w", id, err)
	}
	defer func() { _ = closer.Close() }()
	msg, err := c.decode(id, value)
	if err != nil {
		c.dropUndecodable(id, err)
		return transport.StreamMessage{}, false, nil
	}
	return msg, true, nil
}

// dropUndecodable deletes an entry no consumer can ever process, as the Redis
// consumer acknowledges and deletes one: kept, it would be read on every pass.
func (c *Consumer) dropUndecodable(id string, cause error) {
	c.logger.Warn().Err(cause).Str("message_id", id).Msg("undecodable relay queue entry deleted")
	key, err := c.key(id)
	if err != nil {
		return
	}
	b := c.b.store.DB().NewBatch()
	_ = b.Delete(key, nil)
	_ = c.b.store.Commit(b)
	delete(c.pending, id)
}

func (c *Consumer) key(id string) ([]byte, error) {
	ms, seq, err := parseID(id)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(nil), c.prefix...), encodeID(ms, seq)...), nil
}

// MarkDelivered is a no-op: the delivery channel is bounded by count, which
// is what bounds what this consumer holds in memory.
func (c *Consumer) MarkDelivered(transport.StreamMessage) {}

func (c *Consumer) checkOpen(msg transport.StreamMessage) error {
	if c.closed {
		return fmt.Errorf("consumer is closed")
	}
	if msg.StreamName == "" {
		return fmt.Errorf("message missing stream name")
	}
	return nil
}

// AckMessage deletes the entry: it is never delivered again.
func (c *Consumer) AckMessage(_ context.Context, msg transport.StreamMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(msg); err != nil {
		return err
	}
	key, err := c.key(msg.ID)
	if err != nil {
		return err
	}
	b := c.b.store.DB().NewBatch()
	_ = b.Delete(key, nil)
	if err := c.b.store.Commit(b); err != nil {
		return fmt.Errorf("failed to ack+delete message %s: %w", msg.ID, err)
	}
	delete(c.pending, msg.ID)
	redistransport.RecordAcked(c.supplier, 1)
	return nil
}

// ReleaseMessage hands a pending entry back: it is delivered again, as a
// reclaim, once the release delay has passed, as the Redis sweep takes back an
// entry XNACK released once it has been idle for the claim timeout. A relay
// released because the store refused it is not retried at once.
func (c *Consumer) ReleaseMessage(_ context.Context, msg transport.StreamMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkOpen(msg); err != nil {
		return err
	}
	delete(c.pending, msg.ID)
	c.released = append(c.released, releasedEntry{id: msg.ID, due: time.Now().Add(c.releaseDelay)})
	return nil
}

// AckInBatch adds to b the deletes that acknowledge ids, for a commit that
// writes them together with other state. Acked must follow the commit.
func (c *Consumer) AckInBatch(b *pebble.Batch, ids []string) {
	for _, id := range ids {
		if key, err := c.key(id); err == nil {
			_ = b.Delete(key, nil)
		}
	}
}

// Acked forgets ids a committed batch acknowledged (AckInBatch).
func (c *Consumer) Acked(ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.pending, id)
	}
}

// Exists reports whether an entry is still in the queue.
func (c *Consumer) Exists(id string) (bool, error) {
	key, err := c.key(id)
	if err != nil {
		return false, err
	}
	_, closer, err := c.b.store.DB().Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = closer.Close()
	return true, nil
}

// EachOwnPending calls fn with every pending entry, oldest first, as a
// reclaim. One no longer in the queue is forgotten instead. A released entry is
// no longer pending, as an XNACK'd one is no longer in the consumer's PEL.
func (c *Consumer) EachOwnPending(_ context.Context, fn func(transport.StreamMessage)) error {
	c.mu.Lock()
	ids := make([]string, 0, len(c.pending))
	for id := range c.pending {
		ids = append(ids, id)
	}
	sortIDs(ids)
	var msgs []transport.StreamMessage
	var firstErr error
	for _, id := range ids {
		msg, ok, err := c.readLocked(id)
		if err != nil {
			firstErr = err
			continue
		}
		if !ok {
			delete(c.pending, id)
			continue
		}
		msg.IsReclaim = true
		c.pending[id] = struct{}{}
		msgs = append(msgs, msg)
	}
	c.mu.Unlock()
	for _, m := range msgs {
		fn(m)
	}
	return firstErr
}

// LastGeneratedID is the highest ID ever appended to this queue.
func (c *Consumer) LastGeneratedID(context.Context) (string, error) {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return c.st.lastID(), nil
}

// RecordAcked counts n entries acknowledged outside AckMessage.
func (c *Consumer) RecordAcked(n int) { redistransport.RecordAcked(c.supplier, n) }

// TrimStream deletes entries older than maxAge, delivered or not, the way the
// Redis consumer's XTRIM MINID does.
func (c *Consumer) TrimStream(_ context.Context, maxAge time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, nil
	}
	upper := append(append([]byte(nil), c.prefix...), encodeID(uint64(time.Now().Add(-maxAge).UnixMilli()), 0)...)
	iter, err := c.b.store.DB().NewIter(&pebble.IterOptions{LowerBound: c.prefix, UpperBound: upper})
	if err != nil {
		return 0, err
	}
	var trimmed int64
	for valid := iter.First(); valid; valid = iter.Next() {
		if id, ok := idOfKey(c.prefix, iter.Key()); ok {
			delete(c.pending, id)
			trimmed++
		}
	}
	if err := errors.Join(iter.Error(), iter.Close()); err != nil {
		return 0, err
	}
	if trimmed == 0 {
		return 0, nil
	}
	b := c.b.store.DB().NewBatch()
	_ = b.DeleteRange(c.prefix, upper, nil)
	if err := c.b.store.Commit(b); err != nil {
		return 0, err
	}
	c.logger.Info().Int64("trimmed_entries", trimmed).Dur("max_age", maxAge).Msg("trimmed old entries from the relay queue")
	return trimmed, nil
}

// StreamName is the queue's name, carried by every message it delivers.
func (c *Consumer) StreamName() string { return c.st.name }

// Stop ends delivery and waits for it. Ack and Release keep working.
// Idempotent.
func (c *Consumer) Stop() {
	c.stop()
	c.wg.Wait()
}

// Close stops the consumer. Pending entries stay in the queue. Idempotent.
func (c *Consumer) Close() error {
	c.Stop()
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

var _ transport.MinedRelayConsumer = (*Consumer)(nil)

// sortIDs orders "<ms>-<seq>" IDs numerically.
func sortIDs(ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		mi, si, _ := parseID(ids[i])
		mj, sj, _ := parseID(ids[j])
		return mi < mj || (mi == mj && si < sj)
	})
}
