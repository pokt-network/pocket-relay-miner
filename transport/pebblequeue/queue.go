// Package pebblequeue is the relay queue of a standalone process: the relayer
// publishes mined relays into it and the miner consumes them, through one
// Pebble store instead of Redis Streams.
//
// It keeps the contract the miner reads (transport.MinedRelayConsumer):
// "<ms>-<seq>" IDs that only increase, at-least-once delivery, an entry pending
// from its delivery until it is acknowledged or released, and every entry found
// at startup delivered live, as XREADGROUP ">" delivers an entry no consumer
// holds, and a released entry delivered again once the release delay has
// passed, as the Redis sweep takes back an entry XNACK released.
//
// Entries reach the OS before Publish returns and are fsynced on the store's
// timer (see pebblestore): a crash of the process loses none, an OS crash at
// most the store's sync interval.
package pebblequeue

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Key layout. A stream name never contains a NUL byte (it is a prefix and a
// bech32 address), so NUL separates the parts.
const (
	entryPrefix  = "q\x00"     // q\x00<stream>\x00<ms BE8><seq BE8> -> encoded relay
	lastIDPrefix = "qlast\x00" // qlast\x00<stream> -> <ms BE8><seq BE8>
	idLen        = 16
)

// ErrClosed is returned by a closed publisher or consumer.
var ErrClosed = errors.New("pebblequeue: closed")

// Broker owns the queues of one store.
type Broker struct {
	store        *pebblestore.Store
	streamPrefix string
	logger       logging.Logger

	// mu serializes every publish: an ID is minted and its entry committed
	// under it, so entries are committed in ID order, and the meter counters a
	// publish rewrites are read and written by one writer at a time.
	mu      sync.Mutex
	streams map[string]*stream
	ledger  *redistransport.ChargeLedger
	// counters is where the meter counters live; its lock is held while a
	// counter is read and rewritten, so the meter's Del of a counter cannot
	// land in between. Nil when no charges are written.
	counters *kv.Pebble
	// afterCountersRead, when set, runs between the counters' read and their
	// write, with b.mu held. Tests only.
	afterCountersRead func()
}

// stream is one supplier's queue.
type stream struct {
	name    string
	lastMS  uint64
	lastSeq uint64
	// notify wakes the stream's consumer after a commit or a release.
	notify chan struct{}
}

// NewBroker returns the broker for the queues stored in store. counters is the
// kv store the relay meter reads its counters from (nil writes no charges).
// streamPrefix names the queues the way the Redis transport names its streams.
func NewBroker(logger logging.Logger, store *pebblestore.Store, counters *kv.Pebble, streamPrefix string) *Broker {
	return &Broker{
		store:        store,
		counters:     counters,
		streamPrefix: streamPrefix,
		logger:       logging.ForComponent(logger, "pebble_queue"),
		streams:      make(map[string]*stream),
	}
}

// streamLocked returns the named stream, loading its last ID on first use.
func (b *Broker) streamLocked(name string) (*stream, error) {
	if st, ok := b.streams[name]; ok {
		return st, nil
	}
	st := &stream{name: name, notify: make(chan struct{}, 1)}
	value, closer, err := b.store.DB().Get(lastIDKey(name))
	switch {
	case errors.Is(err, pebble.ErrNotFound):
	case err != nil:
		return nil, fmt.Errorf("pebblequeue: read last id of %s: %w", name, err)
	default:
		if len(value) == idLen {
			st.lastMS = binary.BigEndian.Uint64(value[:8])
			st.lastSeq = binary.BigEndian.Uint64(value[8:])
		}
		_ = closer.Close()
	}
	b.streams[name] = st
	return st, nil
}

func (b *Broker) stream(name string) (*stream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.streamLocked(name)
}

// next mints the stream's next ID: the current millisecond, or the last one
// when the clock has not moved past it, with the sequence counting up.
func (st *stream) next(now time.Time) (ms, seq uint64) {
	ms = uint64(now.UnixMilli())
	if ms <= st.lastMS {
		ms, seq = st.lastMS, st.lastSeq+1
	}
	st.lastMS, st.lastSeq = ms, seq
	return ms, seq
}

func (st *stream) wake() {
	select {
	case st.notify <- struct{}{}:
	default:
	}
}

func (st *stream) lastID() string {
	if st.lastMS == 0 && st.lastSeq == 0 {
		return ""
	}
	return formatID(st.lastMS, st.lastSeq)
}

// Publisher is the relayer's side: it implements transport.MinedRelayPublisher
// and the parts of the batching publisher the relayer reads.
type Publisher struct {
	b      *Broker
	mu     sync.Mutex
	closed bool
	// lastErr is the last commit's failure, nil once a commit succeeds again.
	lastErr error

	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// Publisher returns the broker's publisher. Every chargeInterval (none when
// zero) and on Close it writes the meter charges no publish has carried, as
// the Redis batcher does on every tick and in its final flush: a relay served
// and not mined still spends its budget, and a restart must read it.
func (b *Broker) Publisher(chargeInterval time.Duration) *Publisher {
	p := &Publisher{b: b, stop: make(chan struct{})}
	if chargeInterval > 0 {
		p.wg.Add(1)
		go logging.RecoverGoRoutine(b.logger, "pebble_queue_charges", func(context.Context) {
			defer p.wg.Done()
			ticker := time.NewTicker(chargeInterval)
			defer ticker.Stop()
			for {
				select {
				case <-p.stop:
					return
				case <-ticker.C:
					if err := b.flushCharges(); err != nil {
						b.logger.Warn().Err(err).Msg("writing meter charges failed; retried on the next tick")
					}
				}
			}
		})(context.Background())
	}
	return p
}

// SetChargeLedger makes every publish write the meter charges served so far in
// the same batch as its entry, as the Redis publisher writes them in the same
// MULTI: a crash cannot keep a relay and lose its charge, or the reverse.
func (p *Publisher) SetChargeLedger(ledger *redistransport.ChargeLedger) {
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	p.b.ledger = ledger
}

// Publish validates the relay and commits it to its supplier's queue.
func (p *Publisher) Publish(_ context.Context, msg *transport.MinedRelayMessage) error {
	name, data, reason, err := redistransport.PrepareEntry(p.b.streamPrefix, msg)
	if err != nil {
		redistransport.RecordPublishReject(p.b.logger, reason, msg, err.Error())
		return err
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		redistransport.RecordPublishReject(p.b.logger, "publisher_closed", msg, "publisher already closed")
		return ErrClosed
	}

	st, err := p.b.publish(name, data)
	p.mu.Lock()
	p.lastErr = err
	p.mu.Unlock()
	if err != nil {
		return err
	}
	redistransport.RecordPublished(msg.SupplierOperatorAddress, msg.ServiceId)
	st.wake()
	return nil
}

func (b *Broker) publish(name string, data []byte) (*stream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.streamLocked(name)
	if err != nil {
		return nil, err
	}
	prevMS, prevSeq := st.lastMS, st.lastSeq
	ms, seq := st.next(time.Now())

	err = b.commitWithCharges(func(batch *pebble.Batch) {
		_ = batch.Set(entryKey(name, ms, seq), data, nil)
		_ = batch.Set(lastIDKey(name), encodeID(ms, seq), nil)
	})
	if err != nil {
		st.lastMS, st.lastSeq = prevMS, prevSeq
		return nil, err
	}
	return st, nil
}

// flushCharges writes the charges no publish has carried yet.
func (b *Broker) flushCharges() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ledger == nil {
		return nil
	}
	return b.commitWithCharges(nil)
}

// commitWithCharges commits one batch holding what fill writes and the meter
// charges served so far, read and rewritten under the counters' lock. With
// nothing to write it commits nothing. Called with b.mu held.
func (b *Broker) commitWithCharges(fill func(*pebble.Batch)) error {
	write := func() error {
		var charges []redistransport.Charge
		if b.ledger != nil {
			charges = b.ledger.TakeAll()
		}
		if fill == nil && len(charges) == 0 {
			return nil
		}
		batch := b.store.DB().NewBatch()
		if fill != nil {
			fill(batch)
		}
		consumed := make([]int64, len(charges))
		now := time.Now()
		for i, c := range charges {
			current, expiry, err := b.counterLocked(c.Key, now)
			if err != nil {
				_ = batch.Close()
				b.untake(charges)
				return err
			}
			// EXPIRE NX: the TTL is set when the counter has none.
			if expiry.IsZero() && c.TTL > 0 {
				expiry = now.Add(c.TTL)
			}
			consumed[i] = current + c.Amount
			_ = batch.Set(kv.StringKey(c.Key), kv.EncodeValue(expiry, []byte(strconv.FormatInt(consumed[i], 10))), nil)
		}
		if b.afterCountersRead != nil {
			b.afterCountersRead()
		}
		if err := b.store.Commit(batch); err != nil {
			b.untake(charges)
			return err
		}
		for i, c := range charges {
			b.ledger.Committed(c, consumed[i])
		}
		return nil
	}
	if b.counters == nil {
		return write()
	}
	return b.counters.WithLock(write)
}

func (b *Broker) untake(charges []redistransport.Charge) {
	for _, c := range charges {
		b.ledger.Untake(c)
	}
}

// counterLocked reads a meter counter where the kv store keeps strings, so the
// relay meter reads it with Get as it reads the Redis counter.
func (b *Broker) counterLocked(key string, now time.Time) (int64, time.Time, error) {
	raw, closer, err := b.store.DB().Get(kv.StringKey(key))
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("pebblequeue: read counter: %w", err)
	}
	defer func() { _ = closer.Close() }()
	value, expiry, expired, err := kv.DecodeValue(raw, now)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("pebblequeue: counter %q: %w", key, err)
	}
	if expired {
		return 0, time.Time{}, nil
	}
	n, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("pebblequeue: counter %q is not an integer: %w", key, err)
	}
	return n, expiry, nil
}

// QueuedBytes is what Publish holds unwritten: nothing, it writes before it
// returns. The relayer's publish-queue gate reads it.
func (p *Publisher) QueuedBytes() int { return 0 }

// DispatcherHealthy reports whether the last publish reached the store.
func (p *Publisher) DispatcherHealthy() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastErr != nil {
		return false, p.lastErr
	}
	return true, nil
}

// Close refuses later publishes and writes the charges no publish carried.
// Idempotent.
func (p *Publisher) Close() error {
	var err error
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.stop)
		p.wg.Wait()
		err = p.b.flushCharges()
	})
	return err
}

var _ transport.MinedRelayPublisher = (*Publisher)(nil)

// --- key encoding ---------------------------------------------------------

func entryPrefixOf(stream string) []byte {
	return []byte(entryPrefix + stream + "\x00")
}

func entryKey(stream string, ms, seq uint64) []byte {
	k := entryPrefixOf(stream)
	return append(k, encodeID(ms, seq)...)
}

func lastIDKey(stream string) []byte { return []byte(lastIDPrefix + stream) }

func encodeID(ms, seq uint64) []byte {
	out := make([]byte, idLen)
	binary.BigEndian.PutUint64(out[:8], ms)
	binary.BigEndian.PutUint64(out[8:], seq)
	return out
}

func formatID(ms, seq uint64) string {
	return strconv.FormatUint(ms, 10) + "-" + strconv.FormatUint(seq, 10)
}

// parseID parses "<ms>-<seq>".
func parseID(id string) (ms, seq uint64, err error) {
	msPart, seqPart, ok := strings.Cut(id, "-")
	if !ok {
		return 0, 0, fmt.Errorf("pebblequeue: malformed id %q", id)
	}
	if ms, err = strconv.ParseUint(msPart, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("pebblequeue: malformed id %q: %w", id, err)
	}
	if seq, err = strconv.ParseUint(seqPart, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("pebblequeue: malformed id %q: %w", id, err)
	}
	return ms, seq, nil
}

// idOfKey returns the "<ms>-<seq>" ID of an entry key of the given stream.
func idOfKey(prefix, key []byte) (string, bool) {
	if !bytes.HasPrefix(key, prefix) || len(key) != len(prefix)+idLen {
		return "", false
	}
	raw := key[len(prefix):]
	return formatID(binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:])), true
}

// prefixEnd is the first key after every key that starts with prefix.
func prefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		end[i]++
		if end[i] != 0 {
			return end[:i+1]
		}
	}
	return nil
}
