//go:build test

package redis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
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

	h.observe(maxmemory-1100*mib, maxmemory, storeEvictionPolicy)
	require.True(t, h.Operable(), "control: 1.07 GiB free is above the reserve")

	changed := h.Changed()
	h.observe(maxmemory-1000*mib, maxmemory, storeEvictionPolicy)
	require.False(t, h.Operable(), "LINK close: below 1 GiB free the store closes")
	select {
	case <-changed:
	default:
		t.Fatal("Changed must fire on the transition")
	}

	h.observe(maxmemory-2047*mib, maxmemory, storeEvictionPolicy)
	require.False(t, h.Operable(), "LINK hysteresis: below 2 GiB free the store stays closed")

	h.observe(maxmemory-900*mib, maxmemory, storeEvictionPolicy)
	require.False(t, h.Operable())
	require.Equal(t, closedBefore+1, transitions(component, "closed", StoreReasonMemoryReserve),
		"staying closed is not another transition")

	h.observe(maxmemory-2048*mib, maxmemory, storeEvictionPolicy)
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
	h.observe(maxmemory-150*mib, maxmemory, storeEvictionPolicy)
	require.False(t, h.Operable(), "a sample with less than twice the reserve free does not reopen after an OOM")
	h.observe(maxmemory-300*mib, maxmemory, storeEvictionPolicy)
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
	h.observe(maxmemory-150*mib, maxmemory, storeEvictionPolicy)
	require.True(t, h.Operable(), "a lost sample is not memory: one with more than the reserve free reopens")
}

// A store with no memory limit used to reopen every gate on every sample, the
// one closed by Redis's own refusal included: the only signal that cannot be
// wrong lasted a tick. Now it closes and stays closed.
func TestStoreHealth_WithoutMaxmemoryTheStoreCloses(t *testing.T) {
	const component = "test_no_max"
	h := NewStoreHealth(zerolog.Nop(), nil, component, StoreGateAdmission)
	h.observe(8*1024*mib, 0, storeEvictionPolicy)
	require.False(t, h.Operable(), "LINK no-maxmemory-closes: a store with no memory limit is closed")
	require.Equal(t, -1.0, testutil.ToFloat64(storeFreeBytes.WithLabelValues(component)))

	h.ReportOOM()
	require.False(t, h.Operable())
	h.observe(8*1024*mib, 0, storeEvictionPolicy)
	require.False(t, h.Operable(), "LINK no-maxmemory-closes: a sample with no limit does not reopen what Redis itself refused")

	const maxmemory = 1024 * mib
	h.observe(maxmemory-512*mib, maxmemory, storeEvictionPolicy)
	require.True(t, h.Operable(), "a sample with a usable configuration and room reopens it")
}

// Every policy other than noeviction drops keys to make room, and what this
// store holds is the preimage of a claim, not a cache.
func TestStoreHealth_WithAnEvictingPolicyTheStoreCloses(t *testing.T) {
	const component, maxmemory = "test_policy", 1024 * mib
	h := NewStoreHealth(zerolog.Nop(), nil, component, StoreGateAdmission)
	h.observe(maxmemory-512*mib, maxmemory, storeEvictionPolicy)
	require.True(t, h.Operable(), "premise: room and a usable policy is open")

	h.observe(maxmemory-512*mib, maxmemory, "allkeys-lru")
	require.False(t, h.Operable(), "LINK eviction-policy-closes: a policy that evicts closes the store, however much room it has")

	h.observe(maxmemory-512*mib, maxmemory, storeEvictionPolicy)
	require.True(t, h.Operable(), "the policy put back reopens it")
}

func TestStoreConfigRefusal_NamesTheValueItRead(t *testing.T) {
	require.NoError(t, storeConfigRefusal(1024*mib, storeEvictionPolicy))
	require.ErrorContains(t, storeConfigRefusal(0, storeEvictionPolicy), "maxmemory is 0",
		"LINK refusal-names-the-value: the operator is told which setting is wrong")
	require.ErrorContains(t, storeConfigRefusal(1024*mib, "volatile-lru"), "volatile-lru",
		"LINK refusal-names-the-value: the refusal quotes the policy it read")
}

// The gate's Redis runs without maxmemory, which is the configuration this
// process refuses: Start reads it from a real INFO memory reply and says so.
func TestStoreHealth_StartRefusesTheRealServerWithoutMaxmemory(t *testing.T) {
	const component = "test_start_real"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := testredis.Client(t)
	h := NewStoreHealth(zerolog.Nop(), client, component, StoreGateAdmission)

	err := h.Start(ctx)
	require.ErrorContains(t, err, "maxmemory is 0",
		"LINK preflight-refuses: a store that answered with no memory limit stops the process")
	require.Equal(t, -1.0, testutil.ToFloat64(storeFreeBytes.WithLabelValues(component)),
		"the sample was read from the real server")
}

// A store that does not answer is a Redis still coming up, not a misconfigured
// one: the process starts, with its gates closed until a sample arrives.
func TestStoreHealth_StartDoesNotRefuseAStoreThatDoesNotAnswer(t *testing.T) {
	const component = "test_start_silent"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A client pointed at a port nothing listens on: INFO returns an error.
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: time.Second, MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	h := NewStoreHealth(zerolog.Nop(), client, component, StoreGateAdmission)

	require.NoError(t, h.Start(ctx),
		"LINK preflight-silent-store: a store that did not answer does not stop the process")
	require.False(t, h.Operable(),
		"LINK preflight-silent-store: its gates are closed until a sample arrives")
}

func TestStoreHealth_ParsesInfoMemory(t *testing.T) {
	used, maxmemory, policy, ok := parseStoreMemory("# Memory\r\nused_memory:1234\r\nused_memory_human:1.2K\r\nmaxmemory:9663676416\r\nmaxmemory_policy:noeviction\r\n")
	require.True(t, ok, "LINK policy-parsed: INFO writes the policy with an underscore, and without it there is no sample")
	require.Equal(t, uint64(1234), used)
	require.Equal(t, uint64(9663676416), maxmemory)
	require.Equal(t, "noeviction", policy, "LINK policy-parsed: INFO writes the policy with an underscore")
	_, _, _, ok = parseStoreMemory("# Memory\r\nused_memory:1234\r\nmaxmemory_policy:noeviction\r\n")
	require.False(t, ok, "a reply without maxmemory is not a sample")
	_, _, _, ok = parseStoreMemory("# Memory\r\nused_memory:1234\r\nmaxmemory:9663676416\r\n")
	require.False(t, ok, "LINK policy-parsed: a reply without the policy is not a sample either")
}

func TestStoreHealth_NilIsOperable(t *testing.T) {
	var h *StoreHealth
	require.True(t, h.Operable())
	require.Nil(t, h.Changed())
	h.ReportOOM()
	h.OnChange(func(bool) {})
	require.NoError(t, h.Start(context.Background()))
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

	h.observe(maxmemory-900*mib, maxmemory, storeEvictionPolicy)
	now = now.Add(90 * time.Second)
	h.observe(maxmemory-2100*mib, maxmemory, storeEvictionPolicy)

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
	// A counter is the process's, not the test's: it keeps what earlier runs of
	// this same test added under -count.
	reserveClosures := testutil.ToFloat64(storeTransitions.WithLabelValues(component, string(StoreGateIngestion), "open", StoreReasonMemoryReserve))
	const maxmemory = 9 * 1024 * mib // the cluster's maxmemory

	h.observe(maxmemory-1100*mib, maxmemory, storeEvictionPolicy)
	require.True(t, admission.Operable(), "control: above 1 GiB free both gates are open")
	require.True(t, ingestion.Operable())

	h.observe(maxmemory-1000*mib, maxmemory, storeEvictionPolicy)
	require.False(t, admission.Operable(), "LINK gates-close: below 1 GiB free the relayer's gate closes")
	require.False(t, ingestion.Operable(), "LINK gates-close: and the miner's with it")

	h.observe(maxmemory-1535*mib, maxmemory, storeEvictionPolicy)
	require.False(t, admission.Operable(), "(a) between 1 and 1.5 GiB free both stay closed")
	require.False(t, ingestion.Operable(), "LINK ingestion-band: (a) the miner still waits below 1.5 GiB")

	h.observe(maxmemory-1536*mib, maxmemory, storeEvictionPolicy)
	require.True(t, ingestion.Operable(), "LINK ingestion-reopen: (b) at 1.5 GiB free the miner consumes")
	require.False(t, admission.Operable(), "LINK admission-band: (b) while the relayer still refuses below 2 GiB")
	require.Equal(t, 0.0, testutil.ToFloat64(storeOperable.WithLabelValues(component, string(StoreGateAdmission))))
	require.Equal(t, 1.0, testutil.ToFloat64(storeOperable.WithLabelValues(component, string(StoreGateIngestion))))

	h.observe(maxmemory-2048*mib, maxmemory, storeEvictionPolicy)
	require.True(t, admission.Operable(), "(c) at 2 GiB free both are open")
	require.True(t, ingestion.Operable())

	h.observe(maxmemory-1023*mib, maxmemory, storeEvictionPolicy)
	require.False(t, admission.Operable(), "(d) below 1 GiB free both close again")
	require.False(t, ingestion.Operable())
	require.Equal(t, reserveClosures+1, testutil.ToFloat64(storeTransitions.WithLabelValues(component, string(StoreGateIngestion), "open", StoreReasonMemoryReserve)),
		"the miner's gate closed for the memory reserve once in this run")

	require.Equal(t, uint64(2<<30), storeReopenAt(StoreGateAdmission, maxmemory))
	require.Equal(t, uint64(1536*mib), storeReopenAt(StoreGateIngestion, maxmemory))
	require.Equal(t, 128*mib+64*mib, storeReopenAt(StoreGateIngestion, 1024*mib), "a small maxmemory keeps the proportions")
}

func TestStoreVersionRefusal_Table(t *testing.T) {
	cases := []struct {
		version string
		fork    string
		accept  bool
		// quote must appear in the refusal: the operator is told what was read.
		quote string
	}{
		{version: "8.10.0", accept: true},
		{version: "8.10.1", accept: true},
		{version: "8.10", accept: true},
		{version: "8.11.2", accept: true},
		{version: "9.0.0", accept: true},
		{version: "9.0", accept: true},
		{version: "10.1.0", accept: true},
		// "8.9.9" > "8.10.0" as strings: the row a string comparison gets wrong.
		{version: "8.9.9", quote: "8.9.9"},
		{version: "8.9", quote: "8.9"},
		{version: "8.2.1", quote: "8.2.1"},
		{version: "7.2.4", quote: "7.2.4"},
		{version: "7.9.227", quote: "7.9.227"},
		{version: "6.2.14", quote: "6.2.14"},
		{version: "8.10.0-rc1", quote: "8.10.0-rc1"},
		{version: "", quote: `""`},
		{version: "abc", quote: "abc"},
		{version: "v8.10", quote: "v8.10"},
		{version: "7.2.4", fork: "valkey_version 8.1.0", quote: "valkey_version 8.1.0"},
	}
	// One subtest per row, so every row runs and a wrong comparison reports
	// each row it gets wrong, not only the first.
	for _, tc := range cases {
		t.Run(tc.version+"|"+tc.fork, func(t *testing.T) {
			err := storeVersionRefusal(tc.version, tc.fork)
			if tc.accept {
				require.NoError(t, err, "LINK version-gate: redis_version %q is supported", tc.version)
				return
			}
			require.Error(t, err, "LINK version-gate: redis_version %q (fork %q) must be refused", tc.version, tc.fork)
			require.ErrorContains(t, err, tc.quote, "the refusal quotes what it read")
			require.ErrorContains(t, err, "8.10", "the refusal says which version to run")
		})
	}
}

// A real INFO reply carries both sections, and lines such as redis_build_id or
// executable that hold colons of their own.
func TestStoreHealth_ParsesInfoServerAndMemory(t *testing.T) {
	const reply = "# Server\r\nredis_version:8.10.1\r\nredis_git_sha1:00000000\r\nredis_build_id:9a1b2c3d4e5f\r\n" +
		"redis_mode:standalone\r\nexecutable:/usr/local/bin/redis-server\r\ngcc_version:14.2.0\r\n\r\n" +
		"# Memory\r\nused_memory:1234\r\nmaxmemory:9663676416\r\nmaxmemory_policy:noeviction\r\n"
	s, ok := parseStoreInfo(reply)
	require.True(t, ok)
	require.Equal(t, storeSample{used: 1234, maxmemory: 9663676416, policy: "noeviction", version: "8.10.1"}, s)

	s, ok = parseStoreInfo("# Server\r\nredis_version:7.2.4\r\nvalkey_version:8.1.0\r\n# Memory\r\nused_memory:1\r\nmaxmemory:2\r\nmaxmemory_policy:noeviction\r\n")
	require.True(t, ok)
	require.Equal(t, "7.2.4", s.version)
	require.Equal(t, "valkey_version 8.1.0", s.fork, "a fork is named, so the refusal does not tell a Valkey operator to upgrade Redis 7.2.4")

	s, ok = parseStoreInfo("# Memory\r\nused_memory:1\r\nmaxmemory:2\r\nmaxmemory_policy:noeviction\r\n")
	require.True(t, ok, "a reply without redis_version is still a sample: the server answered")
	require.Empty(t, s.version, "and apply refuses it, because the version is empty")
}

// A sample from an unsupported server closes the store as misconfigured even
// after startup -- a Redis that was not answering when the process started, or
// one replaced behind the same address -- and only a supported one reopens it.
func TestStoreHealth_AnOldServerClosesTheStoreAfterStartup(t *testing.T) {
	const component, maxmemory = "test_old_version", 4096 * mib
	h := NewStoreHealth(zerolog.Nop(), nil, component, StoreGateAdmission)
	good := storeSample{used: 512 * mib, maxmemory: maxmemory, policy: storeEvictionPolicy, version: "8.10.1"}
	require.NoError(t, h.apply(good))
	require.True(t, h.Operable(), "premise: a supported server with room is open")

	old := good
	old.version = "7.2.4"
	before := transitions(component, "closed", StoreReasonMisconfigured)
	require.ErrorContains(t, h.apply(old), "7.2.4")
	require.False(t, h.Operable(), "LINK version-gate-observe: an old server closes the store")
	require.Equal(t, before+1, transitions(component, "closed", StoreReasonMisconfigured),
		"LINK version-gate-observe: closed as misconfigured, not as full")
	require.Equal(t, -1.0, testutil.ToFloat64(storeFreeBytes.WithLabelValues(component)))

	require.NoError(t, h.apply(good))
	require.True(t, h.Operable(), "a supported server with room reopens it")
}

// infoVersionRewriter rewrites redis_version in every INFO reply, and nothing
// else, so the real server can play an old one.
type infoVersionRewriter struct {
	to        string
	rewritten atomic.Int64
}

var redisVersionLine = regexp.MustCompile(`redis_version:[^\r\n]*`)

func (f *infoVersionRewriter) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (f *infoVersionRewriter) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		if sc, ok := cmd.(*goredis.StringCmd); ok && cmd.Name() == "info" && err == nil {
			sc.SetVal(redisVersionLine.ReplaceAllString(sc.Val(), "redis_version:"+f.to))
			f.rewritten.Add(1)
		}
		return err
	}
}

func (f *infoVersionRewriter) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

// The real path: Start reads the version from a real INFO reply and refuses an
// old server BEFORE its memory configuration. The shared test server has no
// maxmemory, so with the version check gone this test reads "maxmemory is 0".
func TestStoreHealth_StartRefusesAnOldServer(t *testing.T) {
	const component = "test_start_old"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := testredis.Client(t)
	rw := &infoVersionRewriter{to: "7.2.4"}
	client.AddHook(rw)
	h := NewStoreHealth(zerolog.Nop(), client, component, StoreGateAdmission)

	err := h.Start(ctx)
	require.Positive(t, rw.rewritten.Load(), "premise: Start's INFO went through the rewriter")
	require.ErrorContains(t, err, "redis_version is 7.2.4",
		"LINK version-gate-start: an old server stops the process, and the refusal names the version")
	require.NotContains(t, err.Error(), "maxmemory", "the version is the root cause and is reported first")
}
