//go:build test

package redis

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// commandCounter counts the stream reads and reclaims a consumer issues.
type commandCounter struct{ reads, reclaims atomic.Int64 }

func (c *commandCounter) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (c *commandCounter) count(name string) {
	switch name {
	case "xreadgroup":
		c.reads.Add(1)
	case "xpending", "xclaim", "xautoclaim":
		c.reclaims.Add(1)
	}
}

func (c *commandCounter) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		c.count(cmd.Name())
		return next(ctx, cmd)
	}
}

func (c *commandCounter) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		for _, cmd := range cmds {
			c.count(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

// pausedConsumer is a consumer over a stream holding two published relays, with
// its store closed.
func pausedConsumer(t *testing.T) (*StreamsConsumer, *StoreHealth, *commandCounter, goredis.UniversalClient, string) {
	t.Helper()
	ctx := context.Background()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	const supplier = "pokt1paused"
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	t.Cleanup(func() { _ = p.Close() })
	require.NoError(t, p.Publish(ctx, mined(supplier, "s1", 1)))
	require.NoError(t, p.Publish(ctx, mined(supplier, "s1", 2)))
	p.dispatchAll(ctx)

	c, err := NewStreamsConsumer(zerolog.Nop(), client, transport.ConsumerConfig{
		StreamPrefix: prefix, SupplierOperatorAddress: supplier,
		ConsumerGroup: prefix + ":group", ConsumerName: "paused", BatchSize: 10, ClaimIdleTimeout: 1000,
	})
	require.NoError(t, err)
	c.msgCh = make(chan transport.StreamMessage, 10)
	require.NoError(t, c.ensureConsumerGroup(ctx))
	health := NewStoreHealth(zerolog.Nop(), client, "test_consumer_pause", StoreGateAdmission)
	c.SetStoreHealth(health)
	health.ReportOOM()
	counter := &commandCounter{}
	client.AddHook(counter)
	return c, health, counter, client, transport.SupplierStreamName(prefix, supplier)
}

func TestStreamsConsumer_ReadsNothingWhileTheStoreIsClosedAndResumesWhenItReopens(t *testing.T) {
	c, health, counter, client, stream := pausedConsumer(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- c.consumeMessagesUntilError(ctx) }()

	// Reopen only after the loop has had its chance to read while closed: the
	// wait below is the transition, not a duration.
	changed := health.Changed()
	const maxmemory = 1024 * mib
	health.observe(maxmemory-512*mib, maxmemory, storeEvictionPolicy)
	<-changed

	select {
	case msg := <-c.msgCh:
		require.NotEmpty(t, msg.ID)
	case <-time.After(10 * time.Second):
		t.Fatal("reopened, the consumer must read")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("consumer did not stop")
	}
	require.Positive(t, counter.reads.Load(), "control: an open store reads")
	require.Equal(t, int64(2), client.XLen(context.Background(), stream).Val(), "reading never deletes")
}

func TestStreamsConsumer_AClosedStoreIssuesNoReadNoOwnPendingAndNoReclaim(t *testing.T) {
	c, _, counter, client, stream := pausedConsumer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, c.consumeMessagesUntilError(ctx), context.Canceled)
	require.Zero(t, counter.reads.Load(), "LINK pause-read: no XREADGROUP while the store is closed")

	require.ErrorIs(t, c.deliverOwnPending(ctx), context.Canceled)
	require.Zero(t, counter.reads.Load(), "LINK pause-own: own pending is not delivered while the store is closed")

	c.reclaimLoop(ctx)
	require.Zero(t, counter.reclaims.Load(), "LINK pause-reclaim: nothing is reclaimed while the store is closed")

	pending, err := client.XPending(context.Background(), stream, c.config.ConsumerGroup).Result()
	require.NoError(t, err)
	require.Zero(t, pending.Count, "nothing moved into the PEL")
	require.Equal(t, int64(2), client.XLen(context.Background(), stream).Val())
}
