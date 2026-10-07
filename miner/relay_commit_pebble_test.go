//go:build test

package miner

import (
	"context"
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
	t        *testing.T
	supplier string
	store    *pebblestore.Store
	broker   *pebblequeue.Broker
	backend  *PebbleStoreBackend
	stores   supplierStores
	n        int
}

func newPebbleCommitHarness(t *testing.T, supplier string) *pebbleCommitHarness {
	t.Helper()
	store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
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
