//go:build test

package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// execRecorder records, per EXEC, how many XADD, INCRBY and EXPIRE commands it
// carried and which counters its INCRBYs and EXPIREs named. It counts by command
// name, so it does not depend on whether the hook sees MULTI and EXEC.
type execRecorder struct {
	mu    sync.Mutex
	execs []execShape
}

type execShape struct {
	xadds, incrs, expires int
	incrKeys, expireKeys  []string
}

func (r *execRecorder) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (r *execRecorder) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (r *execRecorder) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		var s execShape
		for _, c := range cmds {
			switch strings.ToLower(c.Name()) {
			case "xadd":
				s.xadds++
			case "incrby":
				s.incrs++
				s.incrKeys = append(s.incrKeys, fmt.Sprint(c.Args()[1]))
			case "expire":
				s.expires++
				s.expireKeys = append(s.expireKeys, fmt.Sprint(c.Args()[1]))
			}
		}
		// Only the dispatcher's own writes are shapes. go-redis initialises every
		// NEW connection with a pipeline of its own -- SELECT, CLIENT SETNAME,
		// CLIENT TRACKING (redis.go:839 of v9.22.0, and the comment at :725-730
		// calls it "this internal conn's init pipeline") -- and it runs through
		// the client's hooks like any other, so it reaches this recorder as an
		// EXEC with none of the commands measured here.
		//
		// Measured 2026-09-20: once the dispatcher started beating as it starts
		// (item 388), that PING opens a connection while the fixture is already
		// recording, and the extra empty shape made the chunk-border test fail
		// about one run in five.
		//
		// What this costs: an EXEC of the dispatcher's that carried none of these
		// three commands would go unrecorded. writeChunk never sends one -- a
		// chunk always carries an XADD or a charge's INCRBY -- so the exclusion
		// is on a shape the dispatcher does not produce.
		if s.xadds+s.incrs+s.expires > 0 {
			r.mu.Lock()
			r.execs = append(r.execs, s)
			r.mu.Unlock()
		}
		return next(ctx, cmds)
	}
}

func (r *execRecorder) shapes() []execShape {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]execShape(nil), r.execs...)
}

// chargeFixture is a publisher that never dispatches on its own, with a ledger
// and an EXEC recorder on a real Redis.
func chargeFixture(t *testing.T) (*BatchingPublisher, *ChargeLedger, goredis.UniversalClient, string, *execRecorder) {
	t.Helper()
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	rec := &execRecorder{}
	client.AddHook(rec)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)
	return p, ledger, client, prefix, rec
}

// requireChargesNeverSplit asserts no EXEC went over the command limit and every
// INCRBY travelled with its own EXPIRE NX.
func requireChargesNeverSplit(t *testing.T, shapes []execShape) {
	t.Helper()
	for i, s := range shapes {
		require.LessOrEqual(t, s.xadds+s.incrs+s.expires, maxChunkCommands, "EXEC %d is over the command limit", i)
		require.Equal(t, s.incrKeys, s.expireKeys, "EXEC %d split a charge from its EXPIRE NX", i)
	}
}

func publishMined(t *testing.T, p *BatchingPublisher, supplier string, n int, session func(int) string) {
	t.Helper()
	for i := 0; i < n; i++ {
		msg := mined(supplier, session(i), i)
		msg.RelayBytes = []byte(fmt.Sprintf("relay-%d", i))
		require.NoError(t, p.Publish(context.Background(), msg))
	}
}

// TestAChargeRidesWithItsSupplierUpToTheCommandLimit is the border of the chunk:
// 254 XADDs and one charge are exactly 256 commands and one EXEC; with 255 XADDs
// the charge does not fit and goes in another EXEC, written once.
func TestAChargeRidesWithItsSupplierUpToTheCommandLimit(t *testing.T) {
	for _, tc := range []struct {
		xadds, wantExecs int
	}{{254, 1}, {255, 2}} {
		t.Run(fmt.Sprint(tc.xadds), func(t *testing.T) {
			p, ledger, client, prefix, rec := chargeFixture(t)
			const supplier = "pokt1border"
			ctx := context.Background()
			publishMined(t, p, supplier, tc.xadds, func(int) string { return "s1" })
			key := prefix + ":consumed:border"
			ledger.Add(key, supplier, 7, time.Hour)

			p.dispatchAll(ctx)

			shapes := rec.shapes()
			require.Len(t, shapes, tc.wantExecs)
			requireChargesNeverSplit(t, shapes)
			require.Equal(t, int64(tc.xadds), client.XLen(ctx, transport.SupplierStreamName(prefix, supplier)).Val())
			got, err := client.Get(ctx, key).Int64()
			require.NoError(t, err)
			require.Equal(t, int64(7), got, "the charge is written exactly once")
			require.Positive(t, client.TTL(ctx, key).Val())
			require.Zero(t, ledger.Pending(key))
		})
	}
}

// TestThreeHundredSessionsAreEachChargedOnce is the case a chunk cannot hold whole:
// one supplier, 300 sessions, one mined relay each. Every XADD and every charge
// lands exactly once, asserted against Redis, and no EXEC goes over the limit.
func TestThreeHundredSessionsAreEachChargedOnce(t *testing.T) {
	p, ledger, client, prefix, rec := chargeFixture(t)
	const supplier = "pokt1many"
	const sessions = 300
	ctx := context.Background()
	publishMined(t, p, supplier, sessions, func(i int) string { return fmt.Sprintf("s%d", i) })
	keys := make([]string, sessions)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s:consumed:s%d", prefix, i)
		ledger.Add(keys[i], supplier, 3, time.Hour)
	}

	p.dispatchAll(ctx)

	requireChargesNeverSplit(t, rec.shapes())
	require.Equal(t, int64(sessions), client.XLen(ctx, transport.SupplierStreamName(prefix, supplier)).Val())
	for _, key := range keys {
		got, err := client.Get(ctx, key).Int64()
		require.NoError(t, err, key)
		require.Equal(t, int64(3), got, key)
		require.Zero(t, ledger.Pending(key))
	}
}

// TestAPairThatMinedNothingIsChargedAlone: a supplier with no XADD this tick still
// has its charge written.
func TestAPairThatMinedNothingIsChargedAlone(t *testing.T) {
	p, ledger, client, prefix, _ := chargeFixture(t)
	ctx := context.Background()
	key := prefix + ":consumed:alone"
	ledger.Add(key, "pokt1idle", 9, time.Hour)

	p.dispatchAll(ctx)

	got, err := client.Get(ctx, key).Int64()
	require.NoError(t, err)
	require.Equal(t, int64(9), got)
	require.Positive(t, client.TTL(ctx, key).Val())
	require.Zero(t, client.Exists(ctx, transport.SupplierStreamName(prefix, "pokt1idle")).Val())
}

// TestACounterRedisRefusesIsDroppedAfterTheLimit: a consumed counter of the wrong
// type refuses every INCRBY. The charge is retried up to the limit and then
// dropped with its reason counted, instead of staying pending forever.
func TestACounterRedisRefusesIsDroppedAfterTheLimit(t *testing.T) {
	p, ledger, client, prefix, _ := chargeFixture(t)
	ctx := context.Background()
	key := prefix + ":consumed:wrongtype"
	require.NoError(t, client.HSet(ctx, key, "f", "v").Err())
	ledger.Add(key, "pokt1wrong", 5, time.Hour)
	exhausted := chargeWriteFailures.WithLabelValues("attempts_exhausted")
	before := testutil.ToFloat64(exhausted)

	for i := 1; i < maxPublishAttempts; i++ {
		p.dispatchAll(ctx)
		require.Equal(t, int64(5), ledger.Pending(key), "attempt %d must keep the charge pending", i)
	}
	p.dispatchAll(ctx)

	require.Zero(t, ledger.Pending(key), "the charge is dropped once the attempts are exhausted")
	require.Equal(t, before+1, testutil.ToFloat64(exhausted))
	p.dispatchAll(ctx)
	require.Equal(t, before+1, testutil.ToFloat64(exhausted), "a dropped charge is not counted again")
}

// TestAnUnknownExecDoesNotChargeTwice: when the EXEC's outcome never comes back,
// the XADDs are written again and the charge is not: it may already be in Redis.
func TestAnUnknownExecDoesNotChargeTwice(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	hook := &exhaustedPoolTimeout{}
	hook.failNext.Store(1)
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	t.Cleanup(func() { _ = p.Close() })
	ledger := NewChargeLedger()
	p.SetChargeLedger(ledger)
	ctx := context.Background()
	const supplier = "pokt1unknown"
	publishMined(t, p, supplier, 3, func(int) string { return "s1" })
	key := prefix + ":consumed:unknown"
	ledger.Add(key, supplier, 4, time.Hour)
	unknown := chargeWriteFailures.WithLabelValues("exec_unknown")
	before := testutil.ToFloat64(unknown)

	p.dispatchAll(ctx)
	require.Equal(t, int64(1), hook.fired.Load(), "precondition: the EXEC failed at transport level")
	require.Zero(t, ledger.Pending(key), "an unknown EXEC does not give the charge back")
	require.Equal(t, before+1, testutil.ToFloat64(unknown))

	p.dispatchAll(ctx)
	require.Equal(t, int64(3), client.XLen(ctx, transport.SupplierStreamName(prefix, supplier)).Val(), "the XADDs are written again")
	require.ErrorIs(t, client.Get(ctx, key).Err(), goredis.Nil, "and the charge is not")
}

// failingPing refuses every PING.
type failingPing struct{}

func (failingPing) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (failingPing) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if strings.EqualFold(cmd.Name(), "ping") {
			err := errors.New("ping refused by test")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}
func (failingPing) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

// TestTheHeartbeatMarksOnlyWhatRedisAnswered pins the mark admission reads: a PING
// Redis answers moves it, a refused PING does not, and an EXEC Redis answers moves
// it too.
func TestTheHeartbeatMarksOnlyWhatRedisAnswered(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)
	ctx := context.Background()

	// The dispatcher beats as soon as it starts, so a publisher on a Redis that
	// answers may be marked before any check here runs. On a Redis that refuses
	// PING that first beat earns nothing, and what is left is construction.
	refusing := testredis.Client(t)
	refusing.AddHook(failingPing{})
	unreached := NewBatchingPublisher(zerolog.Nop(), refusing, prefix, time.Hour)
	require.NoError(t, unreached.Close())
	alive, err := unreached.DispatcherHealthy()
	require.False(t, alive, "a publisher that has not reached Redis yet must refuse admission")
	require.ErrorIs(t, err, errDispatcherNeverReachedRedis,
		"construction must not hand admission a mark nobody earned")

	p := NewBatchingPublisher(zerolog.Nop(), client, prefix, time.Hour)
	// Stopped first: the checks below drive the heartbeat by hand, and the
	// dispatcher's own ticker must not race the injected clock.
	require.NoError(t, p.Close())

	t1 := time.Now()
	p.now = func() time.Time { return t1 }
	p.heartbeat(ctx)
	require.True(t, lastMark(p).Equal(t1), "an answered PING marks")

	client.AddHook(failingPing{})
	p.now = func() time.Time { return t1.Add(10 * time.Second) }
	p.heartbeat(ctx)
	require.True(t, lastMark(p).Equal(t1), "a refused PING does not mark")

	t3 := t1.Add(20 * time.Second)
	p.now = func() time.Time { return t3 }
	ledger := NewChargeLedger()
	_, _, err = p.writeChunk(ctx, nil, []Charge{{Key: prefix + ":consumed:beat", Supplier: "pokt1beat", Amount: 1, TTL: time.Hour}}, ledger)
	require.NoError(t, err)
	require.True(t, lastMark(p).Equal(t3), "an answered EXEC marks")
}

// pingsAnswered signals every PING Redis answered.
type pingsAnswered struct{ answered chan struct{} }

func (h *pingsAnswered) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (h *pingsAnswered) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil && strings.EqualFold(cmd.Name(), "ping") {
			h.answered <- struct{}{}
		}
		return err
	}
}
func (h *pingsAnswered) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

// TestTheHeartbeatRunsOnItsOwnTickerWhateverTheBatchInterval: with an hour
// between dispatches, the dispatcher still reaches Redis about once a second, so
// admission does not close on a quiet relayer.
func TestTheHeartbeatRunsOnItsOwnTickerWhateverTheBatchInterval(t *testing.T) {
	client := testredis.Client(t)
	hook := &pingsAnswered{answered: make(chan struct{}, 4)}
	client.AddHook(hook)
	p := NewBatchingPublisher(zerolog.Nop(), client, testredis.Prefix(t), time.Hour)
	t.Cleanup(func() { _ = p.Close() })
	built := lastMark(p)

	// The SECOND answered PING: the dispatcher runs one heartbeat at a time, so by
	// then the first one has stored its mark.
	for i := 0; i < 2; i++ {
		select {
		case <-hook.answered:
		case <-time.After(5 * heartbeatInterval):
			t.Fatalf("PING %d did not come within five heartbeat intervals: the heartbeat is not on its own ticker", i+1)
		}
	}
	require.True(t, lastMark(p).After(built), "an answered heartbeat moves the mark admission reads")
}
