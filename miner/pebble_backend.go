package miner

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
)

// Key layout of the miner's state in the embedded store. A supplier address
// and a session ID never contain a NUL byte, so NUL separates the parts.
const (
	pebbleSessionPrefix  = "msess\x00"   // msess\x00<supplier>\x00<session> -> JSON snapshot
	pebbleDedupPrefix    = "mdedup\x00"  // mdedup\x00<session>\x00<hash> -> ""
	pebbleDedupTTLPrefix = "mdedupx\x00" // mdedupx\x00<session> -> expiry, unix ms BE8
)

// PebbleStoreBackend keeps every supplier's relay queue, sessions and dedup
// marks in one embedded store: the standalone subcommand's backend.
//
// One mutex serializes every write that reads before it writes -- a session's
// counters, a dedup mark, a batch commit -- which is what Redis's single
// thread and Lua scripts gave the same operations: each is atomic against the
// others.
type PebbleStoreBackend struct {
	logger  logging.Logger
	store   *pebblestore.Store
	broker  *pebblequeue.Broker
	config  SupplierManagerConfig
	dedupCf DeduplicatorConfig

	mu    sync.Mutex
	dedup *pebbleDeduplicator
	// lastSweep is when sweepLocked last ran.
	lastSweep time.Time
}

// sweepInterval is how often a session scan also sweeps every supplier's
// expired sessions and every session's expired dedup marks.
const sweepInterval = time.Minute

// NewPebbleStoreBackend returns the backend over store, with the relay queues
// of broker. config supplies the session TTL, the block time and the consumer
// sizing, as it does to the Redis backend.
func NewPebbleStoreBackend(logger logging.Logger, store *pebblestore.Store, broker *pebblequeue.Broker, config SupplierManagerConfig) *PebbleStoreBackend {
	b := &PebbleStoreBackend{
		logger:  logger,
		store:   store,
		broker:  broker,
		config:  config,
		dedupCf: DeduplicatorConfig{BlockTimeSeconds: config.BlockTimeSeconds}.withDefaults(),
	}
	b.dedup = &pebbleDeduplicator{b: b}
	return b
}

func (b *PebbleStoreBackend) deduplicator() Deduplicator { return b.dedup }

func (b *PebbleStoreBackend) smstStore(supplier string) smstStore {
	return &pebbleSMSTStore{b: b, supplier: supplier}
}

// leaseStore holds the leases in the process: standalone has no peers to
// share suppliers with, so it takes every one, and a restart holds them at
// once.
func (b *PebbleStoreBackend) leaseStore() leaseStore {
	return newExclusiveLeaseStore()
}

func (b *PebbleStoreBackend) rebroadcastStore() RebroadcastStorage {
	return newPebbleRebroadcastStore(b.store, 0) // 0 → default TTL
}

func (b *PebbleStoreBackend) sessionStore(supplier string) SessionStore {
	return &pebbleSessionStore{b: b, supplier: supplier, ttl: sessionTTL(b.config.SessionTTL), syncWAL: b.store.Sync}
}

func (b *PebbleStoreBackend) forSupplier(supplier string, dedup Deduplicator) (supplierStores, error) {
	consumer, err := b.broker.Consumer(transport.ConsumerConfig{
		SupplierOperatorAddress: supplier,
		BatchSize:               int64(b.config.BatchSize),
		ClaimIdleTimeout:        b.config.ClaimIdleTimeout.Milliseconds(),
	})
	if err != nil {
		return supplierStores{}, fmt.Errorf("failed to create consumer for %s: %w", supplier, err)
	}
	// Delivery waits on the store health gate the process was given, as the
	// Redis consumer does.
	consumer.SetStoreHealth(b.config.StoreHealth)
	sessions := b.sessionStore(supplier).(*pebbleSessionStore)
	var commit relayCommitter
	// The committer marks the backend's own dedup set; a different
	// deduplicator (a test's) would not see those marks, so there is no batch.
	if dedup == Deduplicator(b.dedup) {
		commit = &pebbleRelayCommitter{b: b, sessions: sessions, consumer: consumer}
	}
	return supplierStores{
		sessions: sessions,
		consumer: consumer,
		commit:   commit,
		setPause: consumer.SetIngestionPause,
	}, nil
}

// sessionTTL is the session TTL the stores apply: the configured one, or the
// Redis session store's default.
func sessionTTL(configured time.Duration) time.Duration {
	if configured == 0 {
		return 2 * time.Hour
	}
	return configured
}

func (b *PebbleStoreBackend) get(key []byte) ([]byte, bool, error) {
	value, closer, err := b.store.DB().Get(key)
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

// has reports whether the key is stored, without copying its value.
func (b *PebbleStoreBackend) has(key []byte) (bool, error) {
	_, closer, err := b.store.DB().Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, closer.Close()
}

// --- sessions ---------------------------------------------------------------

type pebbleSessionStore struct {
	b        *PebbleStoreBackend
	supplier string
	ttl      time.Duration
	closed   bool
	// syncWAL fsyncs the store; a test replaces it before the store is used.
	syncWAL func() error
}

func (s *pebbleSessionStore) prefix() []byte {
	return []byte(pebbleSessionPrefix + s.supplier + "\x00")
}

func (s *pebbleSessionStore) key(sessionID string) []byte {
	return append(s.prefix(), sessionID...)
}

// expired reports a session nothing has written to for its TTL: Redis would
// have expired its key.
func (s *pebbleSessionStore) expired(snap *SessionSnapshot, now time.Time) bool {
	return !snap.LastUpdatedAt.IsZero() && snap.LastUpdatedAt.Add(s.ttl).Before(now)
}

// getLocked reads a session; nil when it is absent or expired.
func (s *pebbleSessionStore) getLocked(sessionID string) (*SessionSnapshot, error) {
	value, ok, err := s.b.get(s.key(sessionID))
	if err != nil {
		return nil, fmt.Errorf("failed to read session %s: %w", sessionID, err)
	}
	if !ok {
		return nil, nil
	}
	snap := &SessionSnapshot{}
	if err := json.Unmarshal(value, snap); err != nil {
		return nil, fmt.Errorf("failed to decode session %s: %w", sessionID, err)
	}
	if s.expired(snap, time.Now()) {
		return nil, nil
	}
	return snap, nil
}

func (s *pebbleSessionStore) putLocked(batch *pebble.Batch, snap *SessionSnapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("failed to encode session %s: %w", snap.SessionID, err)
	}
	return batch.Set(s.key(snap.SessionID), data, nil)
}

// writeLocked writes snap over prev, the stored session (nil when there is
// none), and reports whether the write records a claim or proof tx hash. Such a
// write must be fsynced before the caller returns (syncTx): losing it to an OS
// crash after the broadcast would leave the session as if nothing had been
// sent. The fsync runs after b.mu is released, as the SMST store's does, so the
// relay commits of every supplier do not wait on it; a reader may meanwhile see
// the hash before it is durable, which nothing acts on before the caller returns.
func (s *pebbleSessionStore) writeLocked(prev, snap *SessionSnapshot) (needSync bool, err error) {
	batch := s.b.store.DB().NewBatch()
	if err := s.putLocked(batch, snap); err != nil {
		_ = batch.Close()
		return false, err
	}
	if err := s.b.store.Commit(batch); err != nil {
		return false, err
	}
	return recordsTx(prev, snap), nil
}

// syncTx fsyncs a write writeLocked reported as recording a tx hash. Called
// without b.mu.
func (s *pebbleSessionStore) syncTx(needSync bool, sessionID string) error {
	if !needSync {
		return nil
	}
	if err := s.syncWAL(); err != nil {
		return fmt.Errorf("failed to sync session %s: %w", sessionID, err)
	}
	return nil
}

func recordsTx(prev, snap *SessionSnapshot) bool {
	if prev == nil {
		return snap.ClaimTxHash != "" || snap.ProofTxHash != ""
	}
	return snap.ClaimTxHash != prev.ClaimTxHash || snap.ProofTxHash != prev.ProofTxHash
}

// Save writes the session. On a session that exists it keeps the stored relay
// counters, as Redis does: they belong to IncrementRelayCount and the relay
// commit, which may have counted relays since the caller read its snapshot.
func (s *pebbleSessionStore) Save(_ context.Context, snapshot *SessionSnapshot) error {
	needSync, err := s.saveLocked(snapshot)
	if err != nil {
		return err
	}
	return s.syncTx(needSync, snapshot.SessionID)
}

func (s *pebbleSessionStore) saveLocked(snapshot *SessionSnapshot) (bool, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.closed {
		return false, fmt.Errorf("session store is closed")
	}
	prev, err := s.getLocked(snapshot.SessionID)
	if err != nil {
		return false, err
	}
	snapshot.LastUpdatedAt = time.Now()
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = snapshot.LastUpdatedAt
	}
	write := snapshot
	if prev != nil {
		merged := *snapshot
		merged.RelayCount = prev.RelayCount
		merged.TotalComputeUnits = prev.TotalComputeUnits
		write = &merged
	}
	return s.writeLocked(prev, write)
}

func (s *pebbleSessionStore) CreateIfAbsent(_ context.Context, snapshot *SessionSnapshot) (bool, error) {
	created, needSync, err := s.createIfAbsentLocked(snapshot)
	if err != nil || !created {
		return false, err
	}
	return true, s.syncTx(needSync, snapshot.SessionID)
}

func (s *pebbleSessionStore) createIfAbsentLocked(snapshot *SessionSnapshot) (created, needSync bool, err error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.closed {
		return false, false, fmt.Errorf("session store is closed")
	}
	existing, err := s.getLocked(snapshot.SessionID)
	if err != nil {
		return false, false, err
	}
	if existing != nil {
		return false, false, nil
	}
	snapshot.LastUpdatedAt = time.Now()
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = snapshot.LastUpdatedAt
	}
	needSync, err = s.writeLocked(nil, snapshot)
	return err == nil, needSync, err
}

func (s *pebbleSessionStore) Get(_ context.Context, sessionID string) (*SessionSnapshot, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	return s.getLocked(sessionID)
}

func (s *pebbleSessionStore) GetBySupplier(context.Context) ([]*SessionSnapshot, error) {
	return s.scan("")
}

func (s *pebbleSessionStore) GetByState(_ context.Context, state SessionState) ([]*SessionSnapshot, error) {
	return s.scan(state)
}

// scan returns the supplier's sessions, in state when it is not empty. An
// expired session is deleted on the way, with its dedup marks: the sweep Redis
// TTLs did.
func (s *pebbleSessionStore) scan(state SessionState) ([]*SessionSnapshot, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	prefix := s.prefix()
	iter, err := s.b.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	if err != nil {
		return nil, fmt.Errorf("failed to scan sessions: %w", err)
	}
	now := time.Now()
	var out []*SessionSnapshot
	var expired []string
	for valid := iter.First(); valid; valid = iter.Next() {
		snap := &SessionSnapshot{}
		if err := json.Unmarshal(iter.Value(), snap); err != nil {
			continue
		}
		if s.expired(snap, now) {
			expired = append(expired, snap.SessionID)
			continue
		}
		if state == "" || snap.State == state {
			out = append(out, snap)
		}
	}
	if err := errors.Join(iter.Error(), iter.Close()); err != nil {
		return nil, fmt.Errorf("failed to scan sessions: %w", err)
	}
	if now.Sub(s.b.lastSweep) >= sweepInterval {
		if err := s.b.sweepLocked(now, s.ttl); err != nil {
			s.b.logger.Warn().Err(err).Msg("failed to sweep expired sessions and dedup marks; retried on a later scan")
		} else {
			s.b.lastSweep = now
		}
	}
	if len(expired) > 0 {
		batch := s.b.store.DB().NewBatch()
		for _, id := range expired {
			_ = batch.Delete(s.key(id), nil)
			s.b.dedup.deleteSessionLocked(batch, id)
		}
		if err := s.b.store.Commit(batch); err != nil {
			s.b.logger.Warn().Err(err).Int("sessions", len(expired)).Msg("failed to delete expired sessions; retried on the next scan")
		}
	}
	return out, nil
}

func (s *pebbleSessionStore) Delete(_ context.Context, sessionID string) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	batch := s.b.store.DB().NewBatch()
	_ = batch.Delete(s.key(sessionID), nil)
	if err := s.b.store.Commit(batch); err != nil {
		return fmt.Errorf("failed to delete session snapshot: %w", err)
	}
	return nil
}

func (s *pebbleSessionStore) UpdateState(_ context.Context, sessionID string, newState SessionState) error {
	return s.updateState(sessionID, newState, "")
}

func (s *pebbleSessionStore) MarkClaimMissing(_ context.Context, sessionID string, verdict string) error {
	return s.updateState(sessionID, SessionStateClaimMissing, verdict)
}

func (s *pebbleSessionStore) updateState(sessionID string, newState SessionState, verdict string) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	snap, err := s.getLocked(sessionID)
	if err != nil {
		return err
	}
	if snap == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	if err := checkStateWrite(snap, newState); err != nil {
		return err
	}
	if snap.State == newState {
		return nil
	}
	snap.State = newState
	if verdict != "" {
		snap.ClaimMissingVerdict = verdict
	}
	snap.LastUpdatedAt = time.Now()
	_, err = s.writeLocked(snap, snap) // the state only: no tx hash changes
	return err
}

func (s *pebbleSessionStore) ReactivateClaimed(_ context.Context, sessionID string, claimedRootHash []byte, claimTxHash string) (Reactivation, error) {
	from, needSync, err := s.reactivateClaimedLocked(sessionID, claimedRootHash, claimTxHash)
	if err != nil || from.From == "" {
		return Reactivation{}, err
	}
	if err := s.syncTx(needSync, sessionID); err != nil {
		return Reactivation{}, err
	}
	return from, nil
}

func (s *pebbleSessionStore) reactivateClaimedLocked(sessionID string, claimedRootHash []byte, claimTxHash string) (from Reactivation, needSync bool, err error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	snap, err := s.getLocked(sessionID)
	if err != nil {
		return Reactivation{}, false, err
	}
	if snap == nil {
		return Reactivation{}, false, fmt.Errorf("session not found: %s", sessionID)
	}
	if !canReactivateClaimed(snap.State) {
		return Reactivation{}, false, nil
	}
	prev := *snap
	snap.State = SessionStateClaimed
	snap.ClaimMissingVerdict = ""
	snap.ClaimedRootHash = claimedRootHash
	if claimTxHash != "" {
		snap.ClaimTxHash = claimTxHash
	}
	snap.LastUpdatedAt = time.Now()
	needSync, err = s.writeLocked(&prev, snap)
	if err != nil {
		return Reactivation{}, false, err
	}
	return Reactivation{From: prev.State, ClaimMissingVerdict: prev.ClaimMissingVerdict, ClaimTxHash: prev.ClaimTxHash}, needSync, nil
}

func (s *pebbleSessionStore) ReactivateProved(_ context.Context, sessionID string, proofTxHash string) (Reactivation, error) {
	from, needSync, err := s.reactivateProvedLocked(sessionID, proofTxHash)
	if err != nil || from.From == "" {
		return Reactivation{}, err
	}
	if err := s.syncTx(needSync, sessionID); err != nil {
		return Reactivation{}, err
	}
	return from, nil
}

func (s *pebbleSessionStore) reactivateProvedLocked(sessionID string, proofTxHash string) (Reactivation, bool, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	snap, err := s.getLocked(sessionID)
	if err != nil {
		return Reactivation{}, false, err
	}
	if snap == nil {
		return Reactivation{}, false, fmt.Errorf("session not found: %s", sessionID)
	}
	if !canReactivateProved(snap.State, snap.ProofTxHash) {
		return Reactivation{}, false, nil
	}
	prev := *snap
	snap.State = SessionStateProved
	if snap.ProofTxHash == "" {
		snap.ProofTxHash = proofTxHash
	}
	snap.LastUpdatedAt = time.Now()
	needSync, err := s.writeLocked(&prev, snap)
	if err != nil {
		return Reactivation{}, false, err
	}
	return Reactivation{From: prev.State, ClaimTxHash: prev.ClaimTxHash, ProofTxHash: prev.ProofTxHash}, needSync, nil
}

func (s *pebbleSessionStore) IncrementRelayCount(_ context.Context, sessionID string, computeUnits uint64) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	snap, err := s.getLocked(sessionID)
	if err != nil {
		return err
	}
	if snap == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	if snap.State.IsTerminal() {
		return ErrSessionTerminal
	}
	snap.RelayCount++
	snap.TotalComputeUnits += computeUnits
	snap.LastUpdatedAt = time.Now()
	_, err = s.writeLocked(snap, snap) // the counters only: no tx hash changes
	return err
}

func (s *pebbleSessionStore) Close() error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.closed = true
	return nil
}

var _ SessionStore = (*pebbleSessionStore)(nil)

// sweepLocked deletes what Redis TTLs would have expired and no supplier's
// scan reaches: the sessions of every supplier, including one no longer
// served, the dedup marks of every session, including one with no snapshot,
// and the session trees' expired records and nodes.
func (b *PebbleStoreBackend) sweepLocked(now time.Time, ttl time.Duration) error {
	batch := b.store.DB().NewBatch()
	found := 0
	sessions := []byte(pebbleSessionPrefix)
	iter, err := b.store.DB().NewIter(&pebble.IterOptions{LowerBound: sessions, UpperBound: keyUpperBound(sessions)})
	if err != nil {
		_ = batch.Close()
		return err
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		snap := &SessionSnapshot{}
		if json.Unmarshal(iter.Value(), snap) != nil || snap.LastUpdatedAt.IsZero() || !snap.LastUpdatedAt.Add(ttl).Before(now) {
			continue
		}
		_ = batch.Delete(append([]byte(nil), iter.Key()...), nil)
		b.dedup.deleteSessionLocked(batch, snap.SessionID)
		found++
	}
	if err := errors.Join(iter.Error(), iter.Close()); err != nil {
		_ = batch.Close()
		return err
	}
	smst, err := b.sweepSMSTLocked(batch, now)
	if err != nil {
		_ = batch.Close()
		return err
	}
	found += smst
	ttls := []byte(pebbleDedupTTLPrefix)
	iter, err = b.store.DB().NewIter(&pebble.IterOptions{LowerBound: ttls, UpperBound: keyUpperBound(ttls)})
	if err != nil {
		_ = batch.Close()
		return err
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		if v := iter.Value(); len(v) == 8 && int64(binary.BigEndian.Uint64(v)) > now.UnixMilli() {
			continue
		}
		b.dedup.deleteSessionLocked(batch, string(iter.Key()[len(ttls):]))
		found++
	}
	if err := errors.Join(iter.Error(), iter.Close()); err != nil {
		_ = batch.Close()
		return err
	}
	if found == 0 {
		return batch.Close()
	}
	return b.store.Commit(batch)
}

// --- dedup ------------------------------------------------------------------

// pebbleDeduplicator keeps one mark per (session, relay hash). Like the Redis
// set, a session's marks live for the dedup TTL after its last mark.
type pebbleDeduplicator struct{ b *PebbleStoreBackend }

func dedupSessionPrefix(sessionID string) []byte {
	return []byte(pebbleDedupPrefix + sessionID + "\x00")
}

func dedupKey(sessionID string, relayHash []byte) []byte {
	return append(dedupSessionPrefix(sessionID), relayHash...)
}

func dedupTTLKey(sessionID string) []byte { return []byte(pebbleDedupTTLPrefix + sessionID) }

// appendDedupKey writes the mark key of relayHash into dst, reusing its array:
// the session's mark prefix (dedupSessionPrefix) followed by the hash.
func appendDedupKey(dst, sessionPrefix, relayHash []byte) []byte {
	return append(append(dst[:0], sessionPrefix...), relayHash...)
}

// liveLocked reports whether the session's marks are still within their TTL.
func (d *pebbleDeduplicator) liveLocked(sessionID string, now time.Time) (bool, error) {
	live, _, err := d.ttlStateLocked(sessionID, now)
	return live, err
}

// ttlStateLocked reports whether the session's marks are live, and whether they
// expired: marks whose TTL ran out may still be on disk until the sweep, and a
// new mark must not bring them back, as a Redis set that expired is gone. A
// session with no TTL at all has no marks to drop.
func (d *pebbleDeduplicator) ttlStateLocked(sessionID string, now time.Time) (live, expired bool, err error) {
	value, ok, err := d.b.get(dedupTTLKey(sessionID))
	if err != nil || !ok || len(value) != 8 {
		return false, false, err
	}
	live = int64(binary.BigEndian.Uint64(value)) > now.UnixMilli()
	return live, !live, nil
}

// markedLocked reports whether the relay is marked for the session.
func (d *pebbleDeduplicator) markedLocked(sessionID string, relayHash []byte, now time.Time) (bool, error) {
	live, err := d.liveLocked(sessionID, now)
	if err != nil || !live {
		return false, err
	}
	_, ok, err := d.b.get(dedupKey(sessionID, relayHash))
	return ok, err
}

// markLocked adds the mark and slides the session's TTL, in batch.
func (d *pebbleDeduplicator) markLocked(batch *pebble.Batch, sessionID string, relayHash []byte, now time.Time) {
	_ = batch.Set(dedupKey(sessionID, relayHash), nil, nil)
	d.slideTTLLocked(batch, sessionID, now)
}

// slideTTLLocked sets the session's marks to expire one dedup TTL after now,
// in batch.
func (d *pebbleDeduplicator) slideTTLLocked(batch *pebble.Batch, sessionID string, now time.Time) {
	expiry := make([]byte, 8)
	binary.BigEndian.PutUint64(expiry, uint64(now.Add(d.b.dedupCf.ttl()).UnixMilli()))
	_ = batch.Set(dedupTTLKey(sessionID), expiry, nil)
}

// deleteSessionLocked removes the session's marks, in batch.
func (d *pebbleDeduplicator) deleteSessionLocked(batch *pebble.Batch, sessionID string) {
	prefix := dedupSessionPrefix(sessionID)
	_ = batch.DeleteRange(prefix, keyUpperBound(prefix), nil)
	_ = batch.Delete(dedupTTLKey(sessionID), nil)
}

func (d *pebbleDeduplicator) IsDuplicate(_ context.Context, relayHash []byte, sessionID string) (bool, error) {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	marked, err := d.markedLocked(sessionID, relayHash, time.Now())
	if err != nil {
		dedupErrors.WithLabelValues("store_check").Inc()
		return false, fmt.Errorf("failed to check the store: %w", err)
	}
	if marked {
		dedupCacheHits.Inc()
		return true, nil
	}
	dedupMisses.Inc()
	return false, nil
}

func (d *pebbleDeduplicator) MarkProcessed(_ context.Context, relayHash []byte, sessionID string) (bool, error) {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	now := time.Now()
	_, expired, err := d.ttlStateLocked(sessionID, now)
	if err != nil {
		dedupErrors.WithLabelValues("store_mark").Inc()
		return false, fmt.Errorf("failed to mark processed: %w", err)
	}
	marked, err := d.markedLocked(sessionID, relayHash, now)
	if err != nil {
		dedupErrors.WithLabelValues("store_mark").Inc()
		return false, fmt.Errorf("failed to mark processed: %w", err)
	}
	batch := d.b.store.DB().NewBatch()
	if expired {
		// Before the new mark: a batch applies in order, so it survives.
		d.deleteSessionLocked(batch, sessionID)
	}
	d.markLocked(batch, sessionID, relayHash, now)
	if err := d.b.store.Commit(batch); err != nil {
		dedupErrors.WithLabelValues("store_mark").Inc()
		return false, fmt.Errorf("failed to mark processed: %w", err)
	}
	dedupMarked.Inc()
	return !marked, nil
}

func (d *pebbleDeduplicator) CleanupSession(_ context.Context, sessionID string) error {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	batch := d.b.store.DB().NewBatch()
	d.deleteSessionLocked(batch, sessionID)
	if err := d.b.store.Commit(batch); err != nil {
		return fmt.Errorf("failed to cleanup session: %w", err)
	}
	return nil
}

func (d *pebbleDeduplicator) Start(context.Context) error { return nil }
func (d *pebbleDeduplicator) Close() error                { return nil }

var _ Deduplicator = (*pebbleDeduplicator)(nil)

// --- batch commit -----------------------------------------------------------

// pebbleRelayCommitter is relayBatchScript as one Pebble batch, under the
// backend's mutex: the marks, the counters for the new marks and the deletes
// of the acknowledged entries are written together or not at all.
type pebbleRelayCommitter struct {
	b        *PebbleStoreBackend
	sessions *pebbleSessionStore
	consumer *pebblequeue.Consumer
}

func (c *pebbleRelayCommitter) CommitSession(_ context.Context, sessionID string, relays []batchedRelay) (relayBatchResult, error) {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	now := time.Now()

	// Everything that can refuse is read before the first write.
	snap, err := c.sessions.getLocked(sessionID)
	if err != nil {
		return relayBatchResult{}, fmt.Errorf("%w: %w", errCommitRefused, err)
	}
	var res relayBatchResult
	switch {
	case snap == nil:
		res.status = 1
	case snap.State.IsTerminal():
		res.status = 2
	}

	// One read of the session's TTL for the whole commit: every read below is
	// of the store, which the batch does not reach until it commits, and `now`
	// is fixed, so it is the answer each relay would read.
	live, expired, err := c.b.dedup.ttlStateLocked(sessionID, now)
	if err != nil {
		return relayBatchResult{}, err
	}
	// One key buffer for every mark: the batch copies the key it is given, and
	// a read does not keep it.
	prefix := dedupSessionPrefix(sessionID)
	var key []byte

	batch := c.b.store.DB().NewBatch()
	if expired && len(relays) > 0 {
		// The marks the TTL let go, dropped before this commit's marks and
		// slide: a batch applies in order, so those survive.
		c.b.dedup.deleteSessionLocked(batch, sessionID)
	}
	seen := make(map[string]struct{}, len(relays))
	ids := make([]string, 0, len(relays))
	for _, r := range relays {
		ids = append(ids, r.id)
		_, inBatch := seen[string(r.hash)]
		marked := inBatch
		if !marked && live {
			key = appendDedupKey(key, prefix, r.hash)
			if marked, err = c.b.has(key); err != nil {
				_ = batch.Close()
				return relayBatchResult{}, err
			}
		}
		if marked {
			// A fresh duplicate: marked before, and its entry still here for
			// this commit to acknowledge.
			exists, err := c.consumer.Exists(r.id)
			if err != nil {
				_ = batch.Close()
				return relayBatchResult{}, err
			}
			if exists {
				res.freshDups++
			}
			continue
		}
		seen[string(r.hash)] = struct{}{}
		key = appendDedupKey(key, prefix, r.hash)
		_ = batch.Set(key, nil, nil)
		res.newRelays++
		res.newComputeUnits += int64(r.computeUnits)
	}
	// Every commit slides the session's TTL once, as the Redis script's EXPIRE
	// does: also one whose relays were all marked already, which is what a
	// redelivery after a lost answer is, so its marks outlive the copies still
	// to come. With the TTL run out every relay is new, so a non-empty batch
	// always has live marks to slide; an empty one writes nothing, as Redis
	// refuses it.
	if len(relays) > 0 {
		c.b.dedup.slideTTLLocked(batch, sessionID, now)
	}

	if res.status == 0 && res.newRelays > 0 {
		snap.RelayCount += res.newRelays
		snap.TotalComputeUnits += uint64(res.newComputeUnits)
		snap.LastUpdatedAt = now
		if err := c.sessions.putLocked(batch, snap); err != nil {
			_ = batch.Close()
			return relayBatchResult{}, err
		}
	}
	c.consumer.AckInBatch(batch, ids)
	if err := c.b.store.Commit(batch); err != nil {
		return relayBatchResult{}, err
	}
	c.consumer.Acked(ids)
	return res, nil
}

func (c *pebbleRelayCommitter) AckRejected(_ context.Context, ids []string) error {
	batch := c.b.store.DB().NewBatch()
	c.consumer.AckInBatch(batch, ids)
	if err := c.b.store.Commit(batch); err != nil {
		return err
	}
	c.consumer.Acked(ids)
	return nil
}

var _ relayCommitter = (*pebbleRelayCommitter)(nil)

// keyUpperBound is the first key after every key that starts with prefix.
func keyUpperBound(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		end[i]++
		if end[i] != 0 {
			return end[:i+1]
		}
	}
	return nil
}
