//go:build test

package redis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	return testutil.ToFloat64(storeTransitions.WithLabelValues(component, string(StoreGateAdmission), state, reason))
}

func TestStoreHealth_ClosesBelowOneGiBAndReopensOnlyWithTwoGiB(t *testing.T) {
	const component = "test_hysteresis"
	h := NewStoreHealth(zerolog.Nop(), nil, component, StoreGateAdmission)
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
	require.Equal(t, 1.0, testutil.ToFloat64(storeOperable.WithLabelValues(component, string(StoreGateAdmission))))
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
	h := NewStoreHealth(zerolog.Nop(), client, component, StoreGateAdmission)
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
	h := NewStoreHealth(zerolog.Nop(), client, "test_oom_pipeline", StoreGateAdmission)
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
	h := NewStoreHealth(zerolog.Nop(), nil, component, StoreGateAdmission)
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
	h := NewStoreHealth(zerolog.Nop(), nil, component, StoreGateAdmission)
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
	h := NewStoreHealth(zerolog.Nop(), client, component, StoreGateAdmission)
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

// logLines decodes every JSON line written to buf.
func logLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		require.Equal(t, 1, strings.Count(line, `"component":`), "LINK log-key: one component key per line: %s", line)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		out = append(out, m)
	}
	return out
}

func TestStoreHealth_TransitionLogsCarryTheNumbersBehindTheDecision(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(2_000_000, 0)
	h := NewStoreHealth(zerolog.New(&buf), nil, "relayer", StoreGateAdmission)
	h.now = func() time.Time { return now }
	const maxmemory = 8 * 1024 * mib

	h.observe(maxmemory-900*mib, maxmemory)
	now = now.Add(90 * time.Second)
	h.observe(maxmemory-2100*mib, maxmemory)

	var lines []map[string]any
	for _, l := range logLines(t, buf.String()) {
		if l["gate"] == string(StoreGateAdmission) {
			lines = append(lines, l)
		}
	}
	require.Len(t, lines, 2, "one close and one reopen of the admission gate")
	closed, opened := lines[0], lines[1]
	require.Equal(t, "relayer", closed["process"])
	require.Equal(t, float64(maxmemory-900*mib), closed["used_memory"], "LINK log-close: the close says how full Redis was")
	require.Equal(t, float64(maxmemory), closed["maxmemory"])
	require.Equal(t, float64(900*mib), closed["free_bytes"])
	require.Equal(t, float64(1<<30), closed["close_below_bytes"])
	require.Equal(t, float64(2100*mib), opened["free_bytes"], "LINK log-open: the reopen says how much room there is")
	require.Equal(t, float64(2<<30), opened["reopen_at_bytes"])
	require.Equal(t, float64(90*time.Second/time.Millisecond), opened["closed_for"], "and how long it was closed (ms)")
}

func TestStoreHealth_BothGatesCloseTogetherAndTheMinerReopensFirst(t *testing.T) {
	const component = "test_two_gates"
	h := NewStoreHealth(zerolog.Nop(), nil, component, StoreGateAdmission)
	admission, ingestion := h.Gate(StoreGateAdmission), h.Gate(StoreGateIngestion)
	const maxmemory = 9 * 1024 * mib // the cluster's maxmemory

	h.observe(maxmemory-1100*mib, maxmemory)
	require.True(t, admission.Operable(), "control: above 1 GiB free both gates are open")
	require.True(t, ingestion.Operable())

	h.observe(maxmemory-1000*mib, maxmemory)
	require.False(t, admission.Operable(), "LINK gates-close: below 1 GiB free the relayer's gate closes")
	require.False(t, ingestion.Operable(), "LINK gates-close: and the miner's with it")

	h.observe(maxmemory-1535*mib, maxmemory)
	require.False(t, admission.Operable(), "(a) between 1 and 1.5 GiB free both stay closed")
	require.False(t, ingestion.Operable(), "LINK ingestion-band: (a) the miner still waits below 1.5 GiB")

	h.observe(maxmemory-1536*mib, maxmemory)
	require.True(t, ingestion.Operable(), "LINK ingestion-reopen: (b) at 1.5 GiB free the miner consumes")
	require.False(t, admission.Operable(), "LINK admission-band: (b) while the relayer still refuses below 2 GiB")
	require.Equal(t, 0.0, testutil.ToFloat64(storeOperable.WithLabelValues(component, string(StoreGateAdmission))))
	require.Equal(t, 1.0, testutil.ToFloat64(storeOperable.WithLabelValues(component, string(StoreGateIngestion))))

	h.observe(maxmemory-2048*mib, maxmemory)
	require.True(t, admission.Operable(), "(c) at 2 GiB free both are open")
	require.True(t, ingestion.Operable())

	h.observe(maxmemory-1023*mib, maxmemory)
	require.False(t, admission.Operable(), "(d) below 1 GiB free both close again")
	require.False(t, ingestion.Operable())
	require.Equal(t, 1.0, testutil.ToFloat64(storeTransitions.WithLabelValues(component, string(StoreGateIngestion), "open", StoreReasonMemoryReserve)))

	require.Equal(t, uint64(2<<30), storeReopenAt(StoreGateAdmission, maxmemory))
	require.Equal(t, uint64(1536*mib), storeReopenAt(StoreGateIngestion, maxmemory))
	require.Equal(t, 128*mib+64*mib, storeReopenAt(StoreGateIngestion, 1024*mib), "a small maxmemory keeps the proportions")
}
