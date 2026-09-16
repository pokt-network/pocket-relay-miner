//go:build test

package redis

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// redisOOMReply is Redis's own refusal of a write under maxmemory.
const redisOOMReply = "OOM command not allowed when used memory > 'maxmemory'."

const mib = uint64(1 << 20)

func transitions(component, state, reason string) float64 {
	return testutil.ToFloat64(storeTransitions.WithLabelValues(component, state, reason))
}

func TestStoreHealth_ClosesBelowOneGiBAndReopensOnlyWithTwoGiB(t *testing.T) {
	const component = "test_hysteresis"
	h := NewStoreHealth(zerolog.Nop(), nil, component)
	const maxmemory = 8 * 1024 * mib // the cluster's maxmemory
	closedBefore := transitions(component, "closed", StoreReasonMemoryReserve)
	openBefore := transitions(component, "open", StoreReasonMemoryReserve)
	var calls []bool
	h.OnChange(func(operable bool) { calls = append(calls, operable) })

	h.observe(maxmemory-1100*mib, maxmemory)
	require.True(t, h.Operable(), "control: 1.07 GiB free is above the reserve")

	changed := h.Changed()
	h.observe(maxmemory-1000*mib, maxmemory)
	require.False(t, h.Operable(), "LINK close: below 1 GiB free the store closes")
	select {
	case <-changed:
	default:
		t.Fatal("Changed must fire on the transition")
	}

	h.observe(maxmemory-2047*mib, maxmemory)
	require.False(t, h.Operable(), "LINK hysteresis: below 2 GiB free the store stays closed")

	h.observe(maxmemory-900*mib, maxmemory)
	require.False(t, h.Operable())
	require.Equal(t, closedBefore+1, transitions(component, "closed", StoreReasonMemoryReserve),
		"staying closed is not another transition")

	h.observe(maxmemory-2048*mib, maxmemory)
	require.True(t, h.Operable(), "LINK reopen: with 2 GiB free the store reopens")
	require.Equal(t, openBefore+1, transitions(component, "open", StoreReasonMemoryReserve))
	require.Equal(t, []bool{false, true}, calls)
	require.Equal(t, 1.0, testutil.ToFloat64(storeOperable.WithLabelValues(component)))
	require.Equal(t, float64(2048*mib), testutil.ToFloat64(storeFreeBytes.WithLabelValues(component)))
}

func TestStoreHealth_TheReserveIsOneGiBOrAnEighthOfASmallMaxmemory(t *testing.T) {
	require.Equal(t, uint64(1<<30), storeCloseBelow(8*1024*mib), "LINK reserve: 1 GiB with the cluster's 8 GiB")
	require.Equal(t, uint64(1<<30), storeCloseBelow(64*1024*mib))
	require.Equal(t, 128*mib, storeCloseBelow(1024*mib), "an eighth of a small maxmemory")
	require.Equal(t, uint64(0), storeCloseBelow(0))
}

// oomOnWrite answers SET with Redis's maxmemory refusal while on.
type oomOnWrite struct{ on atomic.Bool }

func (f *oomOnWrite) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (f *oomOnWrite) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if f.on.Load() && cmd.Name() == "set" {
			err := errors.New(redisOOMReply)
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

func (f *oomOnWrite) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func TestStoreHealth_AnOOMReplyClosesAtOnceAndOnlyASampleWithRoomReopens(t *testing.T) {
	const component = "test_oom_reply"
	ctx := context.Background()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	h := NewStoreHealth(zerolog.Nop(), client, component)
	client.AddHook(h.Hook())
	oom := &oomOnWrite{}
	client.AddHook(oom)
	closedBefore := transitions(component, "closed", StoreReasonOOMReply)

	require.NoError(t, client.Set(ctx, prefix+":k", "v", time.Minute).Err())
	require.True(t, h.Operable(), "control: an accepted write leaves the store operable")

	oom.on.Store(true)
	require.Error(t, client.Set(ctx, prefix+":k", "v", time.Minute).Err())
	require.False(t, h.Operable(), "LINK oom: a write refused for memory closes the store at once")
	require.Error(t, client.Set(ctx, prefix+":k", "v", time.Minute).Err())
	require.Equal(t, closedBefore+1, transitions(component, "closed", StoreReasonOOMReply),
		"a second refusal is not a second transition")

	oom.on.Store(false)
	const maxmemory = 1024 * mib
	h.observe(maxmemory-150*mib, maxmemory)
	require.False(t, h.Operable(), "a sample with less than twice the reserve free does not reopen after an OOM")
	h.observe(maxmemory-300*mib, maxmemory)
	require.True(t, h.Operable(), "a sample with room reopens it")
}

func TestStoreHealth_APipelineWithAnOOMReplyCloses(t *testing.T) {
	ctx := context.Background()
	client := testredis.Client(t)
	h := NewStoreHealth(zerolog.Nop(), client, "test_oom_pipeline")
	client.AddHook(h.Hook())
	client.AddHook(&redisRefusesForMemory{})

	_, _ = client.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.IncrBy(ctx, testredis.Prefix(t)+":c", 1)
		return nil
	})
	require.False(t, h.Operable(), "LINK oom-pipeline: an OOM inside a MULTI closes the store")
}

func TestStoreHealth_ALostSampleClosesAndASampleWithRoomReopens(t *testing.T) {
	const component = "test_stale"
	now := time.Unix(1_000_000, 0)
	h := NewStoreHealth(zerolog.Nop(), nil, component)
	h.now = func() time.Time { return now }
	h.started = true
	h.lastSample = now

	now = now.Add(storeHealthSampleMaxAge)
	h.observeFailure()
	require.True(t, h.Operable(), "control: a sample exactly at the limit is not stale")

	now = now.Add(time.Millisecond)
	h.observeFailure()
	require.False(t, h.Operable(), "LINK stale: no sample for longer than the limit closes the store")

	const maxmemory = 1024 * mib
	h.observe(maxmemory-150*mib, maxmemory)
	require.True(t, h.Operable(), "a lost sample is not memory: one with more than the reserve free reopens")
}

func TestStoreHealth_WithoutMaxmemoryOnlyRefusalsOrLostSamplesClose(t *testing.T) {
	const component = "test_no_max"
	h := NewStoreHealth(zerolog.Nop(), nil, component)
	h.observe(8*1024*mib, 0)
	require.True(t, h.Operable(), "no maxmemory is not a reason to close")
	require.Equal(t, -1.0, testutil.ToFloat64(storeFreeBytes.WithLabelValues(component)))
	h.ReportOOM()
	require.False(t, h.Operable())
	h.observe(8*1024*mib, 0)
	require.True(t, h.Operable(), "without maxmemory a good sample reopens")
}

func TestStoreHealth_StartSamplesTheRealServer(t *testing.T) {
	const component = "test_start_real"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := testredis.Client(t)
	h := NewStoreHealth(zerolog.Nop(), client, component)
	h.Start(ctx)
	require.True(t, h.Operable())
	// The gate's Redis runs without maxmemory, so a real sample reports -1; a
	// value the gauge never had before Start proves INFO was parsed.
	require.Equal(t, -1.0, testutil.ToFloat64(storeFreeBytes.WithLabelValues(component)))
}

func TestStoreHealth_ParsesInfoMemory(t *testing.T) {
	used, maxmemory, ok := parseStoreMemory("# Memory\r\nused_memory:1234\r\nused_memory_human:1.2K\r\nmaxmemory:9663676416\r\n")
	require.True(t, ok)
	require.Equal(t, uint64(1234), used)
	require.Equal(t, uint64(9663676416), maxmemory)
	_, _, ok = parseStoreMemory("# Memory\r\nused_memory:1234\r\n")
	require.False(t, ok, "a reply without maxmemory is not a sample")
}

func TestStoreHealth_NilIsOperable(t *testing.T) {
	var h *StoreHealth
	require.True(t, h.Operable())
	require.Nil(t, h.Changed())
	h.ReportOOM()
	h.OnChange(func(bool) {})
	h.Start(context.Background())
	require.True(t, h.Operable())
}
