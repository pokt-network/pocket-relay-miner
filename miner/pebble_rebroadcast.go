package miner

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
)

// Key layout of the rebroadcast payloads in the embedded store. A phase and a
// supplier address never contain a NUL byte; the session end is big-endian so
// a phase's groups sort by supplier, then by session end.
const (
	pebbleRebroadcastPrefix      = "rb\x00"  // rb\x00<phase>\x00<supplier>\x00<end BE8><sessionID> -> payload
	pebbleRebroadcastGroupPrefix = "rbg\x00" // rbg\x00<phase>\x00<supplier>\x00<end BE8> -> expiry
)

// pebbleRebroadcastStore is RebroadcastStorage in the embedded store. A
// group's payloads share one expiry, refreshed by every Put, as the Redis
// group hash's TTL is; past it the group reads as empty. The groups ActiveGroups
// returns are the live ones, so the expired ones are deleted there: the index
// is the set of group records, not a separate set that could disagree.
type pebbleRebroadcastStore struct {
	store *pebblestore.Store
	ttl   time.Duration
	// mu makes Delete's "remove, then drop the group if empty" atomic against
	// a Put, as the Redis Lua scripts are.
	mu sync.Mutex
}

func newPebbleRebroadcastStore(store *pebblestore.Store, ttl time.Duration) *pebbleRebroadcastStore {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &pebbleRebroadcastStore{store: store, ttl: ttl}
}

func rebroadcastGroupPart(phase RebroadcastPhase, supplier string, sessionEnd int64) []byte {
	key := []byte(string(phase) + "\x00" + supplier + "\x00")
	return binary.BigEndian.AppendUint64(key, uint64(sessionEnd))
}

func (s *pebbleRebroadcastStore) payloadPrefix(phase RebroadcastPhase, supplier string, sessionEnd int64) []byte {
	return append([]byte(pebbleRebroadcastPrefix), rebroadcastGroupPart(phase, supplier, sessionEnd)...)
}

func (s *pebbleRebroadcastStore) groupKey(phase RebroadcastPhase, supplier string, sessionEnd int64) []byte {
	return append([]byte(pebbleRebroadcastGroupPrefix), rebroadcastGroupPart(phase, supplier, sessionEnd)...)
}

// liveLocked reports whether the group exists and is within its expiry.
func (s *pebbleRebroadcastStore) live(key []byte, now time.Time) (bool, error) {
	raw, closer, err := s.store.DB().Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = closer.Close() }()
	_, _, expired, err := kv.DecodeValue(raw, now)
	return err == nil && !expired, err
}

func (s *pebbleRebroadcastStore) dropGroup(batch *pebble.Batch, phase RebroadcastPhase, supplier string, sessionEnd int64) {
	prefix := s.payloadPrefix(phase, supplier, sessionEnd)
	_ = batch.DeleteRange(prefix, keyUpperBound(prefix), nil)
	_ = batch.Delete(s.groupKey(phase, supplier, sessionEnd), nil)
}

func (s *pebbleRebroadcastStore) Put(_ context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	batch := s.store.DB().NewBatch()
	// A group past its expiry is a new group: its old payloads go.
	if live, err := s.live(s.groupKey(phase, supplier, sessionEnd), now); err != nil {
		_ = batch.Close()
		return fmt.Errorf("failed to persist rebroadcast payload (%s/%s/%d/%s): %w", phase, supplier, sessionEnd, sessionID, err)
	} else if !live {
		s.dropGroup(batch, phase, supplier, sessionEnd)
	}
	_ = batch.Set(append(s.payloadPrefix(phase, supplier, sessionEnd), sessionID...), payload, nil)
	_ = batch.Set(s.groupKey(phase, supplier, sessionEnd), kv.EncodeValue(now.Add(s.ttl), nil), nil)
	if err := s.store.Commit(batch); err != nil {
		return fmt.Errorf("failed to persist rebroadcast payload (%s/%s/%d/%s): %w", phase, supplier, sessionEnd, sessionID, err)
	}
	return nil
}

func (s *pebbleRebroadcastStore) List(_ context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64) (map[string][]byte, error) {
	out := make(map[string][]byte)
	live, err := s.live(s.groupKey(phase, supplier, sessionEnd), time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to list rebroadcast payloads (%s/%s/%d): %w", phase, supplier, sessionEnd, err)
	}
	if !live {
		return out, nil
	}
	prefix := s.payloadPrefix(phase, supplier, sessionEnd)
	iter, err := s.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	if err != nil {
		return nil, fmt.Errorf("failed to list rebroadcast payloads (%s/%s/%d): %w", phase, supplier, sessionEnd, err)
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		out[string(iter.Key()[len(prefix):])] = append([]byte(nil), iter.Value()...)
	}
	if err := errors.Join(iter.Error(), iter.Close()); err != nil {
		return nil, fmt.Errorf("failed to list rebroadcast payloads (%s/%s/%d): %w", phase, supplier, sessionEnd, err)
	}
	return out, nil
}

// empty reports whether the group holds no payload.
func (s *pebbleRebroadcastStore) empty(phase RebroadcastPhase, supplier string, sessionEnd int64) (bool, error) {
	prefix := s.payloadPrefix(phase, supplier, sessionEnd)
	iter, err := s.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	if err != nil {
		return false, err
	}
	found := iter.First()
	return !found, errors.Join(iter.Error(), iter.Close())
}

func (s *pebbleRebroadcastStore) Delete(_ context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(err error) error {
		return fmt.Errorf("failed to delete rebroadcast payload (%s/%s/%d/%s): %w", phase, supplier, sessionEnd, sessionID, err)
	}
	batch := s.store.DB().NewBatch()
	_ = batch.Delete(append(s.payloadPrefix(phase, supplier, sessionEnd), sessionID...), nil)
	if err := s.store.Commit(batch); err != nil {
		return fail(err)
	}
	empty, err := s.empty(phase, supplier, sessionEnd)
	if err != nil || !empty {
		return err
	}
	batch = s.store.DB().NewBatch()
	s.dropGroup(batch, phase, supplier, sessionEnd)
	if err := s.store.Commit(batch); err != nil {
		return fail(err)
	}
	return nil
}

func (s *pebbleRebroadcastStore) CleanupIfEmpty(_ context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	live, err := s.live(s.groupKey(phase, supplier, sessionEnd), time.Now())
	if err == nil && live {
		var empty bool
		empty, err = s.empty(phase, supplier, sessionEnd)
		if err == nil && !empty {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("failed to cleanup empty rebroadcast group (%s/%s/%d): %w", phase, supplier, sessionEnd, err)
	}
	batch := s.store.DB().NewBatch()
	s.dropGroup(batch, phase, supplier, sessionEnd)
	if err := s.store.Commit(batch); err != nil {
		return fmt.Errorf("failed to cleanup empty rebroadcast group (%s/%s/%d): %w", phase, supplier, sessionEnd, err)
	}
	return nil
}

func (s *pebbleRebroadcastStore) ActiveGroups(_ context.Context, phase RebroadcastPhase) ([]RebroadcastGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := []byte(pebbleRebroadcastGroupPrefix + string(phase) + "\x00")
	iter, err := s.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	if err != nil {
		return nil, fmt.Errorf("failed to list rebroadcast groups (%s): %w", phase, err)
	}
	now := time.Now()
	var groups, expired []RebroadcastGroup
	for valid := iter.First(); valid; valid = iter.Next() {
		rest := iter.Key()[len(prefix):]
		if len(rest) < 9 || rest[len(rest)-9] != 0 {
			inclusionGroupAbandonedTotal.WithLabelValues(string(phase), abandonCauseIndexMalformed).Inc()
			continue
		}
		group := RebroadcastGroup{
			Supplier:   string(rest[:len(rest)-9]),
			SessionEnd: int64(binary.BigEndian.Uint64(rest[len(rest)-8:])),
		}
		if _, _, isExpired, decErr := kv.DecodeValue(iter.Value(), now); decErr == nil && isExpired {
			expired = append(expired, group)
			continue
		}
		groups = append(groups, group)
	}
	if err := errors.Join(iter.Error(), iter.Close()); err != nil {
		return nil, fmt.Errorf("failed to list rebroadcast groups (%s): %w", phase, err)
	}
	if len(expired) > 0 {
		batch := s.store.DB().NewBatch()
		for _, g := range expired {
			s.dropGroup(batch, phase, g.Supplier, g.SessionEnd)
		}
		if err := s.store.Commit(batch); err != nil {
			return nil, fmt.Errorf("failed to drop expired rebroadcast groups (%s): %w", phase, err)
		}
	}
	return groups, nil
}

var _ RebroadcastStorage = (*pebbleRebroadcastStore)(nil)
