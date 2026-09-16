//go:build test

package redis

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// redisRefusesForMemory answers a MULTI the way Redis 8.10.0 did under maxmemory
// (measured): OOM on each command refused while queuing (XADD, INCRBY), EXECABORT
// on the rest, the whole transaction discarded, nothing written.
type redisRefusesForMemory struct {
	off   atomic.Bool
	fired atomic.Int64
}

func (f *redisRefusesForMemory) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (f *redisRefusesForMemory) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return next
}

func (f *redisRefusesForMemory) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if f.off.Load() {
			return next(ctx, cmds)
		}
		f.fired.Add(1)
		abort := errors.New("EXECABORT Transaction discarded because of previous errors.")
		for _, cmd := range cmds {
			switch cmd.Name() {
			case "xadd", "incrby":
				cmd.SetErr(errors.New(redisOOMReply))
			default:
				cmd.SetErr(abort)
			}
		}
		return abort
	}
}

func queuedAttempts(p *BatchingPublisher) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int
	for _, q := range p.queue[p.head:] {
		out = append(out, q.attempts)
	}
	return out
}

func TestBatchingPublisher_AChunkRefusedForMemoryIsNotSuccessAndSpendsNoAttempt(t *testing.T) {
	ctx := context.Background()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	refuse := &redisRefusesForMemory{}
	client.AddHook(refuse)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)

	const supplier = "pokt1oomchunk"
	for i := 0; i < 3; i++ {
		require.NoError(t, p.Publish(ctx, mined(supplier, "s1", i)))
	}
	ledger.Add(prefix+":consumed", supplier, 7, time.Minute)
	p.lastSuccess.Store(1)
	published := testutil.ToFloat64(publishedTotal.WithLabelValues(supplier, "svc"))
	exhausted := testutil.ToFloat64(chargeWriteFailures.WithLabelValues("attempts_exhausted"))

	for round := 0; round < maxPublishAttempts+2; round++ {
		p.dispatchAll(ctx)
	}

	require.Positive(t, refuse.fired.Load(), "control: the refusal reached a real dispatch")
	require.Equal(t, []int{0, 0, 0}, queuedAttempts(p),
		"LINK oom-attempts: a chunk refused for memory spends no entry's attempt")
	require.Equal(t, int64(1), p.lastSuccess.Load(), "LINK oom-success: a refusal is not Redis answering the dispatcher")
	require.Equal(t, published, testutil.ToFloat64(publishedTotal.WithLabelValues(supplier, "svc")),
		"nothing refused counts as published")
	require.Equal(t, exhausted, testutil.ToFloat64(chargeWriteFailures.WithLabelValues("attempts_exhausted")),
		"a charge refused for memory is not dropped")
	require.Equal(t, int64(7), ledger.Pending(prefix+":consumed"), "LINK oom-charge: the charge stays pending")

	refuse.off.Store(true)
	p.dispatchAll(ctx)
	stream := transport.SupplierStreamName(prefix, supplier)
	require.Equal(t, int64(3), client.XLen(ctx, stream).Val(), "once Redis has room every entry is written")
	require.Equal(t, "7", client.Get(ctx, prefix+":consumed").Val(), "and the charge with them, once")
}

func TestBatchingPublisher_HoldsTheQueueWhileTheStoreIsNotOperable(t *testing.T) {
	ctx := context.Background()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	health := NewStoreHealth(zerolog.Nop(), client, "test_publisher_hold", StoreGateAdmission)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithStoreHealth(health))
	t.Cleanup(func() { _ = p.Close() })

	const supplier = "pokt1oomhold"
	require.NoError(t, p.Publish(ctx, mined(supplier, "s1", 1)))
	stream := transport.SupplierStreamName(prefix, supplier)

	health.ReportOOM()
	p.dispatchAll(ctx)
	require.Equal(t, int64(0), client.XLen(ctx, stream).Val(),
		"LINK hold: nothing is written while the store is not operable")
	require.Equal(t, []int{0}, queuedAttempts(p), "and the entry keeps its place and its attempts")

	const maxmemory = 1024 * mib
	health.observe(maxmemory-512*mib, maxmemory)
	p.dispatchAll(ctx)
	require.Equal(t, int64(1), client.XLen(ctx, stream).Val(), "reopened, the entry is written")
}

// TestBatchingPublisher_RealMaxmemory drives a REAL Redis to maxmemory. It changes
// maxmemory, so it only runs against a disposable server:
// PRM_REAL_OOM=1 REDIS_TEST_URL=redis://127.0.0.1:<port>.
func TestBatchingPublisher_RealMaxmemory(t *testing.T) {
	if os.Getenv("PRM_REAL_OOM") != "1" {
		t.Skip("set PRM_REAL_OOM=1 and REDIS_TEST_URL to a disposable server: this changes maxmemory")
	}
	ctx := context.Background()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	health := NewStoreHealth(zerolog.Nop(), client, "test_real_oom", StoreGateAdmission)
	client.AddHook(health.Hook())
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour, WithStoreHealth(health))
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)

	const supplier = "pokt1realoom"
	for i := 0; i < 5; i++ {
		require.NoError(t, p.Publish(ctx, mined(supplier, "s1", i)))
	}
	ledger.Add(prefix+":consumed", supplier, 11, time.Minute)
	published := testutil.ToFloat64(publishedTotal.WithLabelValues(supplier, "svc"))

	require.NoError(t, client.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err())
	require.NoError(t, client.Set(ctx, prefix+":filler", make([]byte, 4<<20), 0).Err())
	info, err := client.InfoMap(ctx, "memory").Result()
	require.NoError(t, err)
	used, err := strconv.ParseUint(info["Memory"]["used_memory"], 10, 64)
	require.NoError(t, err)
	require.NoError(t, client.ConfigSet(ctx, "maxmemory", strconv.FormatUint(used, 10)).Err())
	t.Cleanup(func() { _ = client.ConfigSet(context.Background(), "maxmemory", "0").Err() })

	p.dispatchAll(ctx)
	stream := transport.SupplierStreamName(prefix, supplier)
	require.False(t, health.Operable(), "LINK real-oom: Redis's refusal closes the store")
	require.Equal(t, []int{0, 0, 0, 0, 0}, queuedAttempts(p), "no entry spent an attempt")
	require.Equal(t, published, testutil.ToFloat64(publishedTotal.WithLabelValues(supplier, "svc")))

	require.NoError(t, client.ConfigSet(ctx, "maxmemory", "0").Err())
	require.Equal(t, int64(0), client.XLen(ctx, stream).Val(), "the refused MULTI wrote nothing (EXECABORT)")
	health.poll(ctx)
	require.True(t, health.Operable(), "without the limit a sample reopens the store")
	p.dispatchAll(ctx)
	require.Equal(t, int64(5), client.XLen(ctx, stream).Val(), "every relay reaches the stream once")
	require.Equal(t, "11", client.Get(ctx, prefix+":consumed").Val(), "and the charge is written once")
	require.Equal(t, published+5, testutil.ToFloat64(publishedTotal.WithLabelValues(supplier, "svc")))
}
