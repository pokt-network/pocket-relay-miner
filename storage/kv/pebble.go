package kv

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Key layout in the embedded store. A key never contains a NUL byte, so NUL
// separates a set's key from its members.
const (
	stringPrefix  = "kv\x00"   // kv\x00<key> -> <expiry ms BE8><value>
	setPrefix     = "kvs\x00"  // kvs\x00<key>\x00<member> -> ""
	setMetaPrefix = "kvsx\x00" // kvsx\x00<key> -> <expiry ms BE8>
)

// janitorInterval is how often expired keys are deleted. Expiry is checked on
// every read too, so the janitor only frees space.
const janitorInterval = time.Minute

// Pebble is the Store over the embedded store, with pub/sub delivered inside
// the process.
//
// One mutex serializes every write, and every read that a write depends on
// (SetNX, CompareAndDelete, SetKeepTTL, Expire, the janitor), which is what
// Redis's single thread gave them: a write never lands between another's read
// and its write.
type Pebble struct {
	store  *pebblestore.Store
	kb     *redisutil.KeyBuilder
	logger logging.Logger
	bus    *Bus

	mu       sync.Mutex
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	now      func() time.Time
}

// NewPebble returns the Store over store, with its keys built by kb, and
// starts the janitor that deletes expired keys. Close stops it.
func NewPebble(logger logging.Logger, store *pebblestore.Store, kb *redisutil.KeyBuilder) *Pebble {
	p := &Pebble{
		store:  store,
		kb:     kb,
		logger: logging.ForComponent(logger, "kv_pebble"),
		bus:    NewBus(logger),
		stop:   make(chan struct{}),
		now:    time.Now,
	}
	p.wg.Add(1)
	go logging.RecoverGoRoutine(p.logger, "kv_pebble_janitor", func(context.Context) {
		defer p.wg.Done()
		ticker := time.NewTicker(janitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				if err := p.deleteExpired(); err != nil {
					p.logger.Warn().Err(err).Msg("deleting expired keys failed; retried on the next tick")
				}
			}
		}
	})(context.Background())
	return p
}

// Close stops the janitor. The store itself is closed by its owner.
func (p *Pebble) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	p.wg.Wait()
	return nil
}

func (p *Pebble) KB() *redisutil.KeyBuilder { return p.kb }

// WithLock runs fn holding the store's write lock, for a writer outside this
// type that reads and rewrites string keys (the relay queue's meter counters):
// a Del or the janitor cannot land between its read and its write.
func (p *Pebble) WithLock(fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fn()
}

// StringKey is where a string key is stored; the relay queue writes the meter
// counters there, in the same batch as its entries.
func StringKey(key string) []byte { return []byte(stringPrefix + key) }

// EncodeValue is a stored string: its expiry (unix ms, 0 for none) and value.
func EncodeValue(expiry time.Time, value []byte) []byte {
	out := make([]byte, 8+len(value))
	if !expiry.IsZero() {
		binary.BigEndian.PutUint64(out[:8], uint64(expiry.UnixMilli()))
	}
	copy(out[8:], value)
	return out
}

// DecodeValue splits a stored string; expired reports it past its expiry.
func DecodeValue(raw []byte, now time.Time) (value []byte, expiry time.Time, expired bool, err error) {
	if len(raw) < 8 {
		return nil, time.Time{}, false, fmt.Errorf("kv: stored value of %d bytes", len(raw))
	}
	if ms := binary.BigEndian.Uint64(raw[:8]); ms != 0 {
		expiry = time.UnixMilli(int64(ms))
		expired = !expiry.After(now)
	}
	return raw[8:], expiry, expired, nil
}

func setMemberPrefix(key string) []byte { return []byte(setPrefix + key + "\x00") }
func setMetaKey(key string) []byte      { return []byte(setMetaPrefix + key) }

func expiryOf(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

func (p *Pebble) raw(key []byte) ([]byte, bool, error) {
	value, closer, err := p.store.DB().Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := append([]byte(nil), value...)
	_ = closer.Close()
	return out, true, nil
}

// getString returns a live string's value and expiry.
func (p *Pebble) getString(key string) ([]byte, time.Time, bool, error) {
	raw, ok, err := p.raw(StringKey(key))
	if err != nil || !ok {
		return nil, time.Time{}, false, err
	}
	value, expiry, expired, err := DecodeValue(raw, p.now())
	if err != nil || expired {
		return nil, time.Time{}, false, err
	}
	return value, expiry, true, nil
}

// setLive reports whether the set exists and has not expired.
func (p *Pebble) setLive(key string) (bool, error) {
	raw, ok, err := p.raw(setMetaKey(key))
	if err != nil || !ok {
		return false, err
	}
	_, _, expired, err := DecodeValue(raw, p.now())
	return err == nil && !expired, err
}

func (p *Pebble) commit(fill func(b *pebble.Batch)) error {
	b := p.store.DB().NewBatch()
	fill(b)
	return p.store.Commit(b)
}

func (p *Pebble) Get(_ context.Context, key string) ([]byte, error) {
	value, _, ok, err := p.getString(key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return value, nil
}

func (p *Pebble) MGet(_ context.Context, keys ...string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	for i, key := range keys {
		value, _, ok, err := p.getString(key)
		if err != nil {
			return nil, err
		}
		if ok {
			out[i] = value
		}
	}
	return out, nil
}

func (p *Pebble) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commit(func(b *pebble.Batch) {
		_ = b.Set(StringKey(key), EncodeValue(expiryOf(p.now(), ttl), value), nil)
	})
}

// SetAll is one batch: every entry or none.
func (p *Pebble) SetAll(_ context.Context, entries ...Entry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	return p.commit(func(b *pebble.Batch) {
		for _, e := range entries {
			_ = b.Set(StringKey(e.Key), EncodeValue(expiryOf(now, e.TTL), e.Value), nil)
		}
	})
}

func (p *Pebble) SetKeepTTL(_ context.Context, key string, value []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, expiry, _, err := p.getString(key)
	if err != nil {
		return err
	}
	return p.commit(func(b *pebble.Batch) {
		_ = b.Set(StringKey(key), EncodeValue(expiry, value), nil)
	})
}

func (p *Pebble) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _, exists, err := p.getString(key)
	if err != nil || exists {
		return false, err
	}
	err = p.commit(func(b *pebble.Batch) {
		_ = b.Set(StringKey(key), EncodeValue(expiryOf(p.now(), ttl), value), nil)
	})
	return err == nil, err
}

func (p *Pebble) CompareAndDelete(_ context.Context, key string, expected []byte) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	value, _, ok, err := p.getString(key)
	if err != nil || !ok || !bytes.Equal(value, expected) {
		return false, err
	}
	err = p.commit(func(b *pebble.Batch) { _ = b.Delete(StringKey(key), nil) })
	return err == nil, err
}

func (p *Pebble) Del(_ context.Context, keys ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commit(func(b *pebble.Batch) {
		for _, key := range keys {
			_ = b.Delete(StringKey(key), nil)
			prefix := setMemberPrefix(key)
			_ = b.DeleteRange(prefix, upperBound(prefix), nil)
			_ = b.Delete(setMetaKey(key), nil)
		}
	})
}

func (p *Pebble) Exists(_ context.Context, key string) (bool, error) {
	_, _, ok, err := p.getString(key)
	if err != nil || ok {
		return ok, err
	}
	return p.setLive(key)
}

func (p *Pebble) Expire(_ context.Context, key string, ttl time.Duration) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	expiry := expiryOf(p.now(), ttl)
	if ttl <= 0 {
		// Redis deletes a key given a non-positive TTL.
		expiry = p.now().Add(-time.Millisecond)
	}
	value, _, ok, err := p.getString(key)
	if err != nil {
		return false, err
	}
	if ok {
		return true, p.commit(func(b *pebble.Batch) {
			_ = b.Set(StringKey(key), EncodeValue(expiry, value), nil)
		})
	}
	live, err := p.setLive(key)
	if err != nil || !live {
		return false, err
	}
	return true, p.commit(func(b *pebble.Batch) {
		_ = b.Set(setMetaKey(key), EncodeValue(expiry, nil), nil)
	})
}

func (p *Pebble) ScanPrefix(_ context.Context, prefix string) ([]string, error) {
	now := p.now()
	var keys []string
	scan := func(base string, decode bool) error {
		lower := []byte(base + prefix)
		iter, err := p.store.DB().NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upperBound([]byte(base + prefix))})
		if err != nil {
			return err
		}
		for valid := iter.First(); valid; valid = iter.Next() {
			if decode {
				if _, _, expired, err := DecodeValue(iter.Value(), now); err != nil || expired {
					continue
				}
			}
			keys = append(keys, string(iter.Key()[len(base):]))
		}
		return errors.Join(iter.Error(), iter.Close())
	}
	if err := scan(stringPrefix, true); err != nil {
		return nil, err
	}
	if err := scan(setMetaPrefix, true); err != nil {
		return nil, err
	}
	return keys, nil
}

func (p *Pebble) SAdd(_ context.Context, key string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	live, err := p.setLive(key)
	if err != nil {
		return err
	}
	return p.commit(func(b *pebble.Batch) {
		if !live {
			// A set that expired is a new set: drop its old members.
			prefix := setMemberPrefix(key)
			_ = b.DeleteRange(prefix, upperBound(prefix), nil)
			_ = b.Set(setMetaKey(key), EncodeValue(time.Time{}, nil), nil)
		}
		for _, m := range members {
			_ = b.Set(append(setMemberPrefix(key), m...), nil, nil)
		}
	})
}

// SRem removes members; a set left empty is deleted, as Redis deletes it.
func (p *Pebble) SRem(_ context.Context, key string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.commit(func(b *pebble.Batch) {
		for _, m := range members {
			_ = b.Delete(append(setMemberPrefix(key), m...), nil)
		}
	})
	if err != nil {
		return err
	}
	left, err := p.members(key)
	if err != nil || len(left) > 0 {
		return err
	}
	return p.commit(func(b *pebble.Batch) { _ = b.Delete(setMetaKey(key), nil) })
}

func (p *Pebble) members(key string) ([]string, error) {
	live, err := p.setLive(key)
	if err != nil || !live {
		return nil, err
	}
	prefix := setMemberPrefix(key)
	iter, err := p.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upperBound(prefix)})
	if err != nil {
		return nil, err
	}
	var out []string
	for valid := iter.First(); valid; valid = iter.Next() {
		out = append(out, string(iter.Key()[len(prefix):]))
	}
	return out, errors.Join(iter.Error(), iter.Close())
}

func (p *Pebble) SMembers(_ context.Context, key string) ([]string, error) {
	members, err := p.members(key)
	if members == nil && err == nil {
		members = []string{}
	}
	return members, err
}

func (p *Pebble) SCard(_ context.Context, key string) (int64, error) {
	members, err := p.members(key)
	return int64(len(members)), err
}

func (p *Pebble) SIsMember(_ context.Context, key string, member string) (bool, error) {
	live, err := p.setLive(key)
	if err != nil || !live {
		return false, err
	}
	_, ok, err := p.raw(append(setMemberPrefix(key), member...))
	return ok, err
}

func (p *Pebble) Publish(_ context.Context, channel string, payload []byte) error {
	p.bus.Publish(channel, string(payload))
	return nil
}

func (p *Pebble) Subscribe(_ context.Context, channels ...string) (Subscription, error) {
	return p.bus.Subscribe(channels...), nil
}

func (p *Pebble) Ping(context.Context) error { return nil }

// deleteExpired deletes the strings and sets past their expiry.
func (p *Pebble) deleteExpired() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	b := p.store.DB().NewBatch()
	found := 0
	for _, base := range []string{stringPrefix, setMetaPrefix} {
		lower := []byte(base)
		iter, err := p.store.DB().NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upperBound(lower)})
		if err != nil {
			_ = b.Close()
			return err
		}
		for valid := iter.First(); valid; valid = iter.Next() {
			if _, _, expired, err := DecodeValue(iter.Value(), now); err != nil || !expired {
				continue
			}
			found++
			_ = b.Delete(append([]byte(nil), iter.Key()...), nil)
			if base == setMetaPrefix {
				prefix := setMemberPrefix(string(iter.Key()[len(base):]))
				_ = b.DeleteRange(prefix, upperBound(prefix), nil)
			}
		}
		if err := errors.Join(iter.Error(), iter.Close()); err != nil {
			_ = b.Close()
			return err
		}
	}
	if found == 0 {
		return b.Close()
	}
	return p.store.Commit(b)
}

// upperBound is the first key after every key that starts with prefix.
func upperBound(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		end[i]++
		if end[i] != 0 {
			return end[:i+1]
		}
	}
	return nil
}

var _ Store = (*Pebble)(nil)
