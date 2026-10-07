package miner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/pokt-network/pocket-relay-miner/storage/kv"
)

// Key layout of the session trees in the embedded store. A supplier address
// and a session ID never contain a NUL byte, so NUL separates the parts.
const (
	pebbleSMSTNodesPrefix  = "smstn\x00" // smstn\x00<supplier>\x00<session>\x00<field> -> encoded node
	pebbleSMSTRecordPrefix = "smstr\x00" // smstr\x00<supplier>\x00<session>\x00<record> -> expiry + value
)

// pebbleSMSTStore keeps one supplier's session trees in the embedded store.
//
// A record is stored with its expiry and reads as absent once past it, as a
// Redis key whose TTL ran out. The nodes' TTL lives in their own record
// (smstNodes, empty value), as a hash's TTL does: past it the nodes read as
// absent to exists, and the backend's sweep deletes them. A single node read
// does not look at that TTL; Redis would answer it missing, and a tree read
// past its TTL is one nothing has written for that long.
//
// Writes that read first take the backend's lock, as Redis's scripts and
// MULTIs made them atomic; node writes need no read and take none. So a sweep
// of a tree whose TTL ran out can delete nodes written just before it, as
// Redis drops an expired hash and an HSET then recreates it with only the
// nodes it carries: a tree nothing checkpointed for its TTL is not kept.
type pebbleSMSTStore struct {
	b        *PebbleStoreBackend
	supplier string
}

func (s *pebbleSMSTStore) sessionPart(sessionID string) string {
	return s.supplier + "\x00" + sessionID + "\x00"
}

func (s *pebbleSMSTStore) nodesPrefix(sessionID string) []byte {
	return []byte(pebbleSMSTNodesPrefix + s.sessionPart(sessionID))
}

func (s *pebbleSMSTStore) recordKey(rec smstRecord, sessionID string) []byte {
	return append([]byte(pebbleSMSTRecordPrefix+s.sessionPart(sessionID)), byte('0'+rec))
}

// readRecord reads a record; ok is false when it is absent or expired.
func (s *pebbleSMSTStore) readRecord(rec smstRecord, sessionID string, now time.Time) (value []byte, ok bool, err error) {
	raw, found, err := s.b.get(s.recordKey(rec, sessionID))
	if err != nil || !found {
		return nil, false, err
	}
	value, _, expired, err := kv.DecodeValue(raw, now)
	if err != nil {
		return nil, false, fmt.Errorf("smst record %d of %s: %w", rec, sessionID, err)
	}
	return value, !expired, nil
}

func (s *pebbleSMSTStore) writeRecord(batch *pebble.Batch, rec smstRecord, sessionID string, value []byte, expiry time.Time) {
	_ = batch.Set(s.recordKey(rec, sessionID), kv.EncodeValue(expiry, value), nil)
}

// nodesLive reports whether the session has nodes stored and within their TTL.
func (s *pebbleSMSTStore) nodesLive(sessionID string, now time.Time) (bool, error) {
	raw, found, err := s.b.get(s.recordKey(smstNodes, sessionID))
	if err != nil {
		return false, err
	}
	if found {
		if _, _, expired, decErr := kv.DecodeValue(raw, now); decErr == nil && expired {
			return false, nil
		}
	}
	prefix := s.nodesPrefix(sessionID)
	iter, err := s.b.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	if err != nil {
		return false, err
	}
	found = iter.First()
	return found, errors.Join(iter.Error(), iter.Close())
}

func (s *pebbleSMSTStore) deleteNodes(batch *pebble.Batch, sessionID string) {
	prefix := s.nodesPrefix(sessionID)
	_ = batch.DeleteRange(prefix, keyUpperBound(prefix), nil)
	_ = batch.Delete(s.recordKey(smstNodes, sessionID), nil)
}

func expiryAfter(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

func (s *pebbleSMSTStore) nodes(ctx context.Context, sessionID string) smstNodeStore {
	return newNodeStore(ctx, &pebbleNodes{s: s, sessionID: sessionID, prefix: s.nodesPrefix(sessionID)})
}

func (s *pebbleSMSTStore) get(_ context.Context, rec smstRecord, sessionID string) ([]byte, error) {
	value, ok, err := s.readRecord(rec, sessionID, time.Now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errSMSTRecordAbsent
	}
	return value, nil
}

func (s *pebbleSMSTStore) head(ctx context.Context, rec smstRecord, sessionID string, n int) ([]byte, error) {
	value, err := s.get(ctx, rec, sessionID)
	if errors.Is(err, errSMSTRecordAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(value) > n {
		value = value[:n]
	}
	return value, nil
}

// set writes a record. The claimed root is fsynced before set returns: the
// claim is built from it, and the proof is imported at it, so an OS crash after
// the claim must not take it back. The fsync runs after the lock is released:
// it makes the whole log durable, and the relay commits wait on that lock.
func (s *pebbleSMSTStore) set(_ context.Context, rec smstRecord, sessionID string, value []byte, ttl time.Duration) error {
	s.b.mu.Lock()
	batch := s.b.store.DB().NewBatch()
	s.writeRecord(batch, rec, sessionID, value, expiryAfter(time.Now(), ttl))
	err := s.b.store.Commit(batch)
	s.b.mu.Unlock()
	if err != nil {
		return err
	}
	if rec == smstClaimedRoot {
		return s.b.store.Sync()
	}
	return nil
}

func (s *pebbleSMSTStore) exists(_ context.Context, rec smstRecord, sessionID string) (bool, error) {
	now := time.Now()
	if rec == smstNodes {
		return s.nodesLive(sessionID, now)
	}
	_, ok, err := s.readRecord(rec, sessionID, now)
	return ok, err
}

// expire sets a record's TTL, as EXPIRE does: nothing for a record that is not
// stored, and a TTL of zero or less deletes it.
func (s *pebbleSMSTStore) expire(_ context.Context, rec smstRecord, sessionID string, ttl time.Duration) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	now := time.Now()
	batch := s.b.store.DB().NewBatch()
	if rec == smstNodes {
		live, err := s.nodesLive(sessionID, now)
		if err != nil || !live {
			_ = batch.Close()
			return err
		}
		if ttl <= 0 {
			s.deleteNodes(batch, sessionID)
		} else {
			s.writeRecord(batch, smstNodes, sessionID, nil, now.Add(ttl))
		}
		return s.b.store.Commit(batch)
	}
	value, ok, err := s.readRecord(rec, sessionID, now)
	if err != nil || !ok {
		_ = batch.Close()
		return err
	}
	if ttl <= 0 {
		_ = batch.Delete(s.recordKey(rec, sessionID), nil)
	} else {
		s.writeRecord(batch, rec, sessionID, value, now.Add(ttl))
	}
	return s.b.store.Commit(batch)
}

func (s *pebbleSMSTStore) del(_ context.Context, sessionID string, recs ...smstRecord) (int64, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	now := time.Now()
	batch := s.b.store.DB().NewBatch()
	var existed int64
	for _, rec := range recs {
		var ok bool
		var err error
		if rec == smstNodes {
			ok, err = s.nodesLive(sessionID, now)
			s.deleteNodes(batch, sessionID)
		} else {
			_, ok, err = s.readRecord(rec, sessionID, now)
			_ = batch.Delete(s.recordKey(rec, sessionID), nil)
		}
		if err != nil {
			_ = batch.Close()
			return 0, err
		}
		if ok {
			existed++
		}
	}
	return existed, s.b.store.Commit(batch)
}

func (s *pebbleSMSTStore) compacted(ctx context.Context, sessionID string) (bool, error) {
	leaves, err := s.exists(ctx, smstLeaves, sessionID)
	if err != nil || !leaves {
		return false, err
	}
	nodes, err := s.exists(ctx, smstNodes, sessionID)
	return !nodes, err
}

func (s *pebbleSMSTStore) setLiveRootIfUnchanged(_ context.Context, sessionID string, root, expected []byte, ttl time.Duration) (bool, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	now := time.Now()
	current, ok, err := s.readRecord(smstLiveRoot, sessionID, now)
	if err != nil {
		return false, err
	}
	if !ok {
		current = nil // expired reads as absent, as an expired Redis key does
	}
	if !bytes.Equal(current, expected) {
		return false, nil
	}
	batch := s.b.store.DB().NewBatch()
	s.writeRecord(batch, smstLiveRoot, sessionID, root, expiryAfter(now, ttl))
	if ttl > 0 {
		live, err := s.nodesLive(sessionID, now)
		if err != nil {
			_ = batch.Close()
			return false, err
		}
		if live {
			s.writeRecord(batch, smstNodes, sessionID, nil, now.Add(ttl))
		}
	}
	if err := s.b.store.Commit(batch); err != nil {
		return false, err
	}
	return true, nil
}

func (s *pebbleSMSTStore) deleteNodesIfLeaves(_ context.Context, sessionID string, leaves []byte) (bool, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	current, ok, err := s.readRecord(smstLeaves, sessionID, time.Now())
	if err != nil || !ok || !bytes.Equal(current, leaves) {
		return false, err
	}
	batch := s.b.store.DB().NewBatch()
	s.deleteNodes(batch, sessionID)
	if err := s.b.store.Commit(batch); err != nil {
		return false, err
	}
	return true, nil
}

func (s *pebbleSMSTStore) sessionsWithNodes(_ context.Context) ([]string, error) {
	prefix := []byte(pebbleSMSTNodesPrefix + s.supplier + "\x00")
	iter, err := s.b.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var sessions []string
	for valid := iter.First(); valid; {
		rest := iter.Key()[len(prefix):]
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			valid = iter.Next()
			continue
		}
		sessionID := string(rest[:end])
		if live, liveErr := s.nodesLive(sessionID, now); liveErr == nil && live {
			sessions = append(sessions, sessionID)
		}
		// The next session starts after every key of this one.
		valid = iter.SeekGE(keyUpperBound(s.nodesPrefix(sessionID)))
	}
	return sessions, errors.Join(iter.Error(), iter.Close())
}

// sweepSMSTLocked deletes the session-tree records past their expiry, and the
// nodes of every tree whose nodes' TTL ran out. Part of the backend's sweep;
// the caller holds b.mu.
func (b *PebbleStoreBackend) sweepSMSTLocked(batch *pebble.Batch, now time.Time) (int, error) {
	prefix := []byte(pebbleSMSTRecordPrefix)
	iter, err := b.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	if err != nil {
		return 0, err
	}
	found := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		if _, _, expired, decErr := kv.DecodeValue(iter.Value(), now); decErr != nil || !expired {
			continue
		}
		key := iter.Key()
		_ = batch.Delete(append([]byte(nil), key...), nil)
		found++
		if key[len(key)-1] == byte('0'+smstNodes) {
			// key is smstr\x00<supplier>\x00<session>\x00<record>.
			session := key[len(prefix) : len(key)-1]
			nodes := append([]byte(pebbleSMSTNodesPrefix), session...)
			_ = batch.DeleteRange(nodes, keyUpperBound(nodes), nil)
		}
	}
	return found, errors.Join(iter.Error(), iter.Close())
}

// pebbleNodes is one session tree's nodes in the embedded store.
type pebbleNodes struct {
	s         *pebbleSMSTStore
	sessionID string
	prefix    []byte
}

func (n *pebbleNodes) name() string {
	return "smst nodes " + n.s.supplier + "/" + n.sessionID
}

func (n *pebbleNodes) key(field string) []byte {
	return append(append([]byte(nil), n.prefix...), field...)
}

func (n *pebbleNodes) get(_ context.Context, field string) ([]byte, bool, error) {
	return n.s.b.get(n.key(field))
}

func (n *pebbleNodes) put(_ context.Context, fields []string, stored [][]byte) error {
	batch := n.s.b.store.DB().NewBatch()
	for i, field := range fields {
		_ = batch.Set(n.key(field), stored[i], nil)
	}
	return n.s.b.store.Commit(batch)
}

func (n *pebbleNodes) del(_ context.Context, field string) error {
	batch := n.s.b.store.DB().NewBatch()
	_ = batch.Delete(n.key(field), nil)
	return n.s.b.store.Commit(batch)
}

func (n *pebbleNodes) count(_ context.Context) (int, error) {
	iter, err := n.s.b.store.DB().NewIter(&pebble.IterOptions{LowerBound: n.prefix, UpperBound: keyUpperBound(n.prefix)})
	if err != nil {
		return 0, err
	}
	count := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		count++
	}
	return count, errors.Join(iter.Error(), iter.Close())
}

func (n *pebbleNodes) clear(_ context.Context) error {
	n.s.b.mu.Lock()
	defer n.s.b.mu.Unlock()
	batch := n.s.b.store.DB().NewBatch()
	n.s.deleteNodes(batch, n.sessionID)
	return n.s.b.store.Commit(batch)
}

func (n *pebbleNodes) scan(_ context.Context, fn func(field string, stored []byte) error) error {
	iter, err := n.s.b.store.DB().NewIter(&pebble.IterOptions{LowerBound: n.prefix, UpperBound: keyUpperBound(n.prefix)})
	if err != nil {
		return err
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		stored := append([]byte(nil), iter.Value()...)
		if err := fn(string(iter.Key()[len(n.prefix):]), stored); err != nil {
			_ = iter.Close()
			return err
		}
	}
	return errors.Join(iter.Error(), iter.Close())
}

// checkpoint is one batch: the orphans' deletes, the live root, and the
// sliding TTL of the live root and of the nodes.
func (n *pebbleNodes) checkpoint(_ context.Context, orphans []string, liveRoot []byte, ttl time.Duration) error {
	n.s.b.mu.Lock()
	defer n.s.b.mu.Unlock()
	now := time.Now()
	batch := n.s.b.store.DB().NewBatch()
	for _, field := range orphans {
		_ = batch.Delete(n.key(field), nil)
	}
	n.s.writeRecord(batch, smstLiveRoot, n.sessionID, liveRoot, expiryAfter(now, ttl))
	if ttl > 0 {
		n.s.writeRecord(batch, smstNodes, n.sessionID, nil, now.Add(ttl))
	}
	return n.s.b.store.Commit(batch)
}

var _ smstStore = (*pebbleSMSTStore)(nil)
var _ nodeBackend = (*pebbleNodes)(nil)
