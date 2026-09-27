//go:build test

package relayer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// batchQueueBytesGauge is what a scrape of ha_relayer_batch_queue_bytes reads now.
func batchQueueBytesGauge(t *testing.T) float64 {
	t.Helper()
	families, err := observability.RelayerRegistry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() == "ha_relayer_batch_queue_bytes" {
			require.Len(t, f.GetMetric(), 1)
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("ha_relayer_batch_queue_bytes is not registered")
	return 0
}

// TestBatchQueueBytesFollowsTheQueueWithoutAdmissions: the gauge says what the
// batch queue holds at every scrape, including after the queue drained with no
// admission asking. A gauge written only when admission asks stays at the last
// size it saw -- measured on a live relayer, 3.1 MB three hours after the load
// ended, and 128 MiB after a 1 MiB load. cmd/cmd_relayer.go makes the call this
// test makes; internal/conventions freezes that it does.
func TestBatchQueueBytesFollowsTheQueueWithoutAdmissions(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	batcher := redisutil.NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	for i := 0; i < 10; i++ {
		require.NoError(t, batcher.Publish(context.Background(), &transport.MinedRelayMessage{
			SessionId: "s1", SessionEndHeight: 10, SupplierOperatorAddress: "pokt1gauge", ServiceId: "svc",
			RelayBytes: []byte(fmt.Sprintf("relay-%d", i)),
		}))
	}
	SetBatchQueueBytesSource(batcher.QueuedBytes)

	queued := batcher.QueuedBytes()
	require.Positive(t, queued, "premise: the relays are queued")
	require.Equal(t, float64(queued), batchQueueBytesGauge(t), "the gauge reads the queue while it holds relays")

	require.NoError(t, batcher.Close(), "the final flush drains the queue; no admission asks meanwhile")
	require.Zero(t, batcher.QueuedBytes(), "premise: the queue drained")
	require.Zero(t, batchQueueBytesGauge(t),
		"the queue drained with no admission asking, and the gauge must follow it to 0")
}
