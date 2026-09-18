//go:build test

package miner

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlushPhase_SpreadsSuppliersStartedTogetherAcrossTheInterval(t *testing.T) {
	const interval = 15 * time.Second
	const suppliers = 30
	perSecond := map[time.Duration]int{}
	distinct := map[time.Duration]bool{}
	for i := range suppliers {
		phase := flushPhase(fmt.Sprintf("pokt1supplier%02d", i), interval)
		require.GreaterOrEqual(t, phase, time.Duration(0))
		require.Less(t, phase, interval, "the first flush comes within one interval")
		perSecond[phase.Truncate(time.Second)]++
		distinct[phase] = true
	}
	require.Len(t, distinct, suppliers, "LINK flush-phase: suppliers started together do not flush in phase")
	for second, n := range perSecond {
		require.LessOrEqual(t, n, suppliers/4, "LINK flush-phase: at most a quarter of the suppliers flush within the second at %s", second)
	}
	require.Equal(t, flushPhase("pokt1supplier00", interval), flushPhase("pokt1supplier00", interval), "a supplier's phase is the same on every start")
}

func TestRunConsumeLoop_TheFirstFlushWaitsTheSuppliersPhaseAndTheTickerFollows(t *testing.T) {
	const supplier = "pokt1flush_phase"
	const interval = 15 * time.Second
	f := newPanicLoopFixture(t, supplier, func(int32) {})
	f.w.mgr.config.RelayBatchFlushInterval = interval

	var mu sync.Mutex
	requested := make(chan time.Duration, 1)
	fire := make(chan time.Time, 1)
	stopped := false
	f.w.mgr.firstFlushTimer = func(d time.Duration) (<-chan time.Time, func() bool) {
		requested <- d
		return fire, func() bool { mu.Lock(); defer mu.Unlock(); stopped = true; return true }
	}
	f.start(t)

	select {
	case d := <-requested:
		require.Equal(t, flushPhase(supplier, interval), d, "LINK flush-phase-wired: the loop's first flush waits the supplier's phase")
	case <-time.After(30 * time.Second):
		t.Fatal("LINK flush-phase-wired: the consume loop never armed its first flush")
	}
	require.Zero(t, f.hookCalls.Load(), "nothing flushes before the phase elapses")

	fire <- time.Now()
	deadline := time.After(30 * time.Second)
	for f.hookCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the first flush did not run when its phase elapsed")
		case <-time.After(time.Millisecond):
		}
	}
	f.w.state.cancelFn()
	f.waitLoopDone(t, "the consume loop did not stop")
	mu.Lock()
	defer mu.Unlock()
	require.True(t, stopped, "the first flush's timer is stopped when the loop ends")
}
