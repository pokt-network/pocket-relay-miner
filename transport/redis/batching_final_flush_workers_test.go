//go:build test

package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestTheFinalFlushWritesWithEveryWorker: Close drains a queue several chunks deep
// with the dispatch workers, two chunks in flight at once, and lands every relay
// once. A second Close changes nothing.
func TestTheFinalFlushWritesWithEveryWorker(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	barrier := newPipelineBarrier(2)
	client.AddHook(barrier)
	// An interval long enough that the only write is the final flush.
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))

	suppliers := suppliersNamed("pokt1flush", 4)
	const perSupplier = 200
	publishInterleaved(t, p, suppliers, perSupplier)

	require.NoError(t, p.Close(), "the final flush must write everything it holds")

	barrier.requireReached(t)
	for _, s := range suppliers {
		requireEachRelayOnce(t, client, transport.SupplierStreamName(prefix, s), perSupplier)
	}
	require.NoError(t, p.Close(), "Close is idempotent")
	require.Zero(t, p.QueuedBytes())
}

// TestTheFinalFlushWithWorkersCountsWhatItAbandoned: when every write of the
// final flush fails, each worker still attempted its chunk, and every relay the
// flush could not write is counted as abandoned exactly once, a second Close
// included.
func TestTheFinalFlushWithWorkersCountsWhatItAbandoned(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	hook := &alwaysFails{err: errors.New("connection reset by peer")}
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithDispatchWorkers(2))

	suppliers := []string{"pokt1abandonA", "pokt1abandonB"}
	const perSupplier = 200
	before := map[string]float64{}
	for _, s := range suppliers {
		before[s] = testutil.ToFloat64(shutdownAbandonedRelays.WithLabelValues(s, "svc"))
		publishMined(t, p, s, perSupplier, func(int) string { return "s1" })
	}

	require.Error(t, p.Close(), "Close must report what the final flush did not write")
	require.Equal(t, int64(2), hook.fired.Load(),
		"both chunks must be attempted by the final flush's workers in one round")
	for _, s := range suppliers {
		require.Equal(t, float64(perSupplier), testutil.ToFloat64(shutdownAbandonedRelays.WithLabelValues(s, "svc"))-before[s],
			"%s: every relay the flush could not write is counted as abandoned", s)
	}

	require.NoError(t, p.Close(), "a second Close neither fails nor counts again")
	for _, s := range suppliers {
		require.Equal(t, float64(perSupplier), testutil.ToFloat64(shutdownAbandonedRelays.WithLabelValues(s, "svc"))-before[s])
		require.Zero(t, client.XLen(context.Background(), transport.SupplierStreamName(prefix, s)).Val())
	}
}
