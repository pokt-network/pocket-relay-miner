//go:build test

package redis

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestApproxBytesTracksTheRetainedHeap measures the heap queued entries retain
// and requires the queue's own count to be within a quarter of it, at relay sizes
// from 64 B to 64 KiB.
func TestApproxBytesTracksTheRetainedHeap(t *testing.T) {
	for _, size := range []int{64, 512, 4 << 10, 64 << 10} {
		p := NewBatchingPublisher(zerolog.Nop(), nil, "measure", time.Hour)
		const n = 4000
		msgs := make([]*transport.MinedRelayMessage, n)
		for i := range msgs {
			msgs[i] = &transport.MinedRelayMessage{SessionId: "session-0123456789abcdef", SessionEndHeight: 10,
				SupplierOperatorAddress: "pokt1supplieroperatoraddress0000000000000", ServiceId: "develop-http",
				RelayBytes: make([]byte, size)}
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for _, m := range msgs {
			require.NoError(t, p.Publish(context.Background(), m))
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		retained := float64(after.HeapAlloc-before.HeapAlloc) / n
		counted := float64(p.QueuedBytes()) / n
		t.Logf("relay=%d retained_per_entry=%.0f counted_per_entry=%.0f ratio=%.2f", size, retained, counted, retained/counted)
		require.InDelta(t, 1.0, retained/counted, 0.25,
			"LINK approx-bytes: relay of %d B retains %.0f per entry, the queue counts %.0f", size, retained, counted)
		runtime.KeepAlive(msgs)
		// Nothing to write here: the client is nil and the queue is dropped
		// before the final flush could reach it.
		p.mu.Lock()
		p.queue, p.head, p.bytes = nil, 0, 0
		p.mu.Unlock()
		p.stop()
		<-p.done
	}
}
