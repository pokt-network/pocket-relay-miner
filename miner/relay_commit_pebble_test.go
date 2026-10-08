//go:build test

package miner

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
)

type pebbleCommitHarness struct {
	t        testing.TB
	supplier string
	store    *pebblestore.Store
	broker   *pebblequeue.Broker
	backend  *PebbleStoreBackend
	stores   supplierStores
	n        int
}

func newPebbleCommitHarness(t testing.TB, supplier string) *pebbleCommitHarness {
	t.Helper()
	return newPebbleCommitHarnessSyncing(t, supplier, time.Hour)
}

// newPebbleCommitHarnessSyncing is the harness with the store's WAL fsynced
// every syncInterval: a benchmark passes the production default, so the cost of
// the fsync is in what it measures.
func newPebbleCommitHarnessSyncing(t testing.TB, supplier string, syncInterval time.Duration) *pebbleCommitHarness {
	t.Helper()
	store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: syncInterval})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	broker := pebblequeue.NewBroker(zerolog.Nop(), store, nil, "ha:relays")
	backend := NewPebbleStoreBackend(zerolog.Nop(), store, broker, SupplierManagerConfig{BlockTimeSeconds: 30})
	stores, err := backend.forSupplier(supplier, backend.deduplicator())
	require.NoError(t, err)
	require.NotNil(t, stores.commit, "premise: the store's own deduplicator gets a committer")
	return &pebbleCommitHarness{t: t, supplier: supplier, store: store, broker: broker, backend: backend, stores: stores}
}

func (h *pebbleCommitHarness) commit() relayCommitter { return h.stores.commit }

func (h *pebbleCommitHarness) createSession(id string, state SessionState) {
	created, err := h.stores.sessions.CreateIfAbsent(context.Background(), &SessionSnapshot{
		SessionID: id, SupplierOperatorAddress: h.supplier, ServiceID: "svc-1", State: state,
	})
	require.NoError(h.t, err)
	require.True(h.t, created, "premise: the session is new")
}

func (h *pebbleCommitHarness) publish(n int) []string {
	h.t.Helper()
	pub := h.broker.Publisher(0)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		h.n++
		require.NoError(h.t, pub.Publish(context.Background(), &transport.MinedRelayMessage{
			SupplierOperatorAddress: h.supplier, ServiceId: "svc-1", SessionId: "queued",
			SessionEndHeight: 100, RelayBytes: []byte(fmt.Sprintf("relay-%d", h.n)),
		}))
		id, err := h.stores.consumer.LastGeneratedID(context.Background())
		require.NoError(h.t, err)
		ids[i] = id
	}
	return ids
}

func (h *pebbleCommitHarness) count(prefix []byte) int64 {
	h.t.Helper()
	iter, err := h.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	require.NoError(h.t, err)
	var n int64
	for valid := iter.First(); valid; valid = iter.Next() {
		n++
	}
	require.NoError(h.t, iter.Close())
	return n
}

func (h *pebbleCommitHarness) marked(id string) int64 { return h.count(dedupSessionPrefix(id)) }

func (h *pebbleCommitHarness) markDone(id string, hash []byte) {
	added, err := h.backend.deduplicator().MarkProcessed(context.Background(), hash, id)
	require.NoError(h.t, err)
	require.True(h.t, added)
}

func (h *pebbleCommitHarness) queued() int64 {
	return h.count([]byte("q\x00" + transport.SupplierStreamName("ha:relays", h.supplier) + "\x00"))
}

func (h *pebbleCommitHarness) snapshot(id string) *SessionSnapshot {
	snap, err := h.stores.sessions.Get(context.Background(), id)
	require.NoError(h.t, err)
	require.NotNil(h.t, snap)
	return snap
}

// refusing corrupts the session's stored value: the committer refuses to
// count into a session it cannot read, before any write.
func (h *pebbleCommitHarness) refusing(id string) relayCommitter {
	sessions := h.stores.sessions.(*pebbleSessionStore)
	b := h.store.DB().NewBatch()
	require.NoError(h.t, b.Set(sessions.key(id), []byte("{not json"), nil))
	require.NoError(h.t, h.store.Commit(b))
	return h.stores.commit
}

// The commit reads the session's TTL once and writes it once: after new marks
// it expires one dedup TTL after the commit, and a commit that marks nothing
// leaves it as it was.
func TestPebbleCommitter_TheSessionTTLIsSlidOnlyByNewMarks(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1commit_ttl")
	const sessionID = "sess-commit-ttl"
	h.createSession(sessionID, SessionStateActive)
	ttl := h.backend.dedupCf.ttl()

	before := time.Now()
	_, err := h.commit().CommitSession(context.Background(), sessionID, relaysFor(h.publish(3), "h-0", "h-1", "h-2"))
	require.NoError(t, err)
	after := time.Now()
	stored, ok, err := h.backend.get(dedupTTLKey(sessionID))
	require.NoError(t, err)
	require.True(t, ok, "new marks write the session's TTL")
	require.Len(t, stored, 8)
	expiry := int64(binary.BigEndian.Uint64(stored))
	require.GreaterOrEqual(t, expiry, before.Add(ttl).UnixMilli())
	require.LessOrEqual(t, expiry, after.Add(ttl).UnixMilli())

	res, err := h.commit().CommitSession(context.Background(), sessionID, relaysFor(h.publish(3), "h-0", "h-1", "h-2"))
	require.NoError(t, err)
	require.Zero(t, res.newRelays, "premise: every relay was marked already")
	again, _, err := h.backend.get(dedupTTLKey(sessionID))
	require.NoError(t, err)
	require.Equal(t, stored, again, "a commit that marks nothing does not touch the TTL")
}

// Marks whose session TTL has run out are not marks: the relays count as new.
func TestPebbleCommitter_MarksPastTheirTTLDoNotCount(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1commit_expired")
	const sessionID = "sess-commit-expired"
	h.createSession(sessionID, SessionStateActive)
	h.markDone(sessionID, []byte("h-0"))
	h.markDone(sessionID, []byte("h-1"))
	past := make([]byte, 8)
	binary.BigEndian.PutUint64(past, uint64(time.Now().Add(-time.Minute).UnixMilli()))
	b := h.store.DB().NewBatch()
	require.NoError(t, b.Set(dedupTTLKey(sessionID), past, nil))
	require.NoError(t, h.store.Commit(b))

	res, err := h.commit().CommitSession(context.Background(), sessionID, relaysFor(h.publish(2), "h-0", "h-1"))

	require.NoError(t, err)
	require.Equal(t, int64(2), res.newRelays, "expired marks count as absent")
	require.Zero(t, res.freshDups)
}

// A hundred relays in one commit: every mark is stored under its own key and
// every queue entry is gone, so the key buffers the commit reuses are copied
// by the batch, never shared by two writes.
func TestPebbleCommitter_AHundredRelaysKeepAHundredDistinctKeys(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1commit_hundred")
	const sessionID = "sess-commit-hundred"
	h.createSession(sessionID, SessionStateActive)
	ids := h.publish(100)
	hashes := make([]string, len(ids))
	for i := range hashes {
		hashes[i] = fmt.Sprintf("hash-%03d", i)
	}

	res, err := h.commit().CommitSession(context.Background(), sessionID, relaysFor(ids, hashes...))

	require.NoError(t, err)
	require.Equal(t, int64(100), res.newRelays)
	require.Equal(t, int64(100), h.marked(sessionID))
	for _, hash := range hashes {
		ok, err := h.backend.has(dedupKey(sessionID, []byte(hash)))
		require.NoError(t, err)
		require.True(t, ok, "mark of %s", hash)
	}
	require.Zero(t, h.queued(), "every entry acknowledged and removed")
}

// A rejected-entry ack runs outside the committer's lock; it must not share a
// key buffer with a commit running at the same time.
func TestPebbleCommitter_AckRejectedAndCommitRunConcurrently(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1commit_concurrent")
	const sessionID = "sess-commit-concurrent"
	h.createSession(sessionID, SessionStateActive)
	committed := h.publish(50)
	rejected := h.publish(50)
	hashes := make([]string, len(committed))
	for i := range hashes {
		hashes[i] = fmt.Sprintf("c-%02d", i)
	}

	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		errs <- h.commit().AckRejected(context.Background(), rejected)
	}()
	_, err := h.commit().CommitSession(context.Background(), sessionID, relaysFor(committed, hashes...))
	<-done

	require.NoError(t, err)
	require.NoError(t, <-errs)
	require.Zero(t, h.queued(), "both acknowledged every entry they were given")
	require.Equal(t, int64(50), h.marked(sessionID))
}

// The allocation budget of one 100-relay commit. Measured 2026-10-08: 987
// allocations before the TTL was read and written once per commit and the key
// buffers were reused; 127 after (219 under -race, which the gates also run).
// The ceiling has headroom over the latter and none for the former.
func TestPebbleCommitter_ACommitStaysWithinItsAllocationBudget(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1commit_allocs")
	const sessionID = "sess-commit-allocs"
	h.createSession(sessionID, SessionStateActive)
	const runs = 20
	batches := make([][]batchedRelay, runs+1)
	for i := range batches {
		ids := h.publish(100)
		hashes := make([]string, len(ids))
		for j := range hashes {
			hashes[j] = fmt.Sprintf("alloc-%d-%d", i, j)
		}
		batches[i] = relaysFor(ids, hashes...)
	}
	next := 0
	allocs := testing.AllocsPerRun(runs, func() {
		if _, err := h.commit().CommitSession(context.Background(), sessionID, batches[next]); err != nil {
			t.Fatalf("CommitSession: %v", err)
		}
		next++
	})
	t.Logf("allocations per 100-relay commit: %.0f", allocs)
	require.LessOrEqual(t, allocs, float64(pebbleCommitAllocCeiling))
}

// pebbleCommitAllocCeiling: see TestPebbleCommitter_ACommitStaysWithinItsAllocationBudget.
const pebbleCommitAllocCeiling = 300
