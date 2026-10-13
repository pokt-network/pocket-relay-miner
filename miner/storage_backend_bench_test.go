//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/rs/zerolog"

	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
)

// The same bodies against both stores: Redis (high-availability mode) and the
// embedded Pebble store (standalone mode). Redis is the real one from
// scripts/gates/redis.sh, over loopback -- its best case, with no network hop.
// Pebble runs at the production sync interval, fsyncing its WAL every second
// on the disk b.TempDir is on.
//
//	go test -tags test -run '^$' -bench 'StoreBackend' -benchmem ./miner/

func eachSMSTBackend(b *testing.B, supplier string, body func(b *testing.B, m *RedisSMSTManager)) {
	cfg := RedisSMSTManagerConfig{SupplierAddress: supplier}
	b.Run("redis", func(b *testing.B) {
		client, _ := newTestRedis(b)
		body(b, newSMSTManager(zerolog.Nop(), newRedisSMSTStore(client, supplier), cfg))
	})
	b.Run("pebble", func(b *testing.B) {
		store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: b.TempDir(), SyncInterval: pebblestore.DefaultSyncInterval})
		if err != nil {
			b.Fatalf("open pebble: %v", err)
		}
		b.Cleanup(func() { _ = store.Close() })
		backend := NewPebbleStoreBackend(zerolog.Nop(), store, pebblequeue.NewBroker(zerolog.Nop(), store, nil, "ha:relays"), SupplierManagerConfig{})
		body(b, newSMSTManager(zerolog.Nop(), backend.smstStore(supplier), cfg))
	})
}

// A whole session's tree: create, 1000 relays, flush, prove.
func BenchmarkStoreBackend_SMSTSession1000Relays(b *testing.B) {
	eachSMSTBackend(b, "pokt1bench_backend_session", func(b *testing.B, m *RedisSMSTManager) {
		ctx := context.Background()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sessionID := fmt.Sprintf("bench-backend-1000-%d", i)
			if _, err := m.GetOrCreateTree(ctx, sessionID); err != nil {
				b.Fatalf("GetOrCreateTree: %v", err)
			}
			for j := 0; j < 1000; j++ {
				key := []byte(fmt.Sprintf("relay-key-%d", j))
				value := []byte(fmt.Sprintf("relay-value-%d", j))
				if err := m.UpdateTree(ctx, sessionID, key, value, uint64(j+1)); err != nil {
					b.Fatalf("UpdateTree: %v", err)
				}
			}
			if _, err := m.FlushTree(ctx, sessionID); err != nil {
				b.Fatalf("FlushTree: %v", err)
			}
			path := protocol.GetPathForProof([]byte("relay-key-0"), sessionID)
			if _, err := m.ProveClosest(ctx, sessionID, path); err != nil {
				b.Fatalf("ProveClosest: %v", err)
			}
		}
	})
}

// One relay into a live session's tree: the miner's per-relay SMST cost. It
// reaches neither store: node writes are buffered in the nodeStore until the
// tree is flushed, and reads hit that buffer first, so the two sub-benchmarks
// measure the same in-memory work. It is here as that control.
func BenchmarkStoreBackend_SMSTUpdate(b *testing.B) {
	eachSMSTBackend(b, "pokt1bench_backend_update", func(b *testing.B, m *RedisSMSTManager) {
		ctx := context.Background()
		const sessionID = "bench-backend-update"
		if _, err := m.GetOrCreateTree(ctx, sessionID); err != nil {
			b.Fatalf("GetOrCreateTree: %v", err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			key := []byte(fmt.Sprintf("relay-key-%d", i))
			if err := m.UpdateTree(ctx, sessionID, key, []byte("relay-value"), 1); err != nil {
				b.Fatalf("UpdateTree: %v", err)
			}
		}
	})
}

// One commit of 100 delivered relays: dedup marks, session counters and the
// acknowledgement of every entry, as one unit. Publishing them is not timed.
func BenchmarkStoreBackend_CommitBatch100(b *testing.B) {
	const batch = 100
	run := func(b *testing.B, h commitHarness) {
		ctx := context.Background()
		const sessionID = "bench-backend-commit"
		h.createSession(sessionID, SessionStateActive)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			ids := h.publish(batch)
			relays := make([]batchedRelay, batch)
			for j, id := range ids {
				relays[j] = batchedRelay{id: id, hash: []byte(fmt.Sprintf("h-%d-%d", i, j)), computeUnits: 100}
			}
			b.StartTimer()
			res, err := h.commit().CommitSession(ctx, sessionID, relays)
			if err != nil {
				b.Fatalf("CommitSession: %v", err)
			}
			if res.newRelays != batch {
				b.Fatalf("committed %d new relays, want %d", res.newRelays, batch)
			}
		}
		b.ReportMetric(float64(batch*b.N)/b.Elapsed().Seconds(), "relays/s")
	}
	b.Run("redis", func(b *testing.B) { run(b, newRedisCommitHarness(b, "pokt1bench_backend_commit")) })
	b.Run("pebble", func(b *testing.B) {
		run(b, newPebbleCommitHarnessSyncing(b, "pokt1bench_backend_commit", pebblestore.DefaultSyncInterval))
	})
}
