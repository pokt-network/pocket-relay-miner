package logging

import (
	"context"
	"io"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestRecoverGoRoutineCountsThePanicAndDoesNotPropagate is the behavioural
// half of the panic signal. observability/registry_test.go already pins that
// ha_panic_recoveries_total is SERVED by the registry both binaries scrape;
// nothing proved that a panic actually reaches it, so the counter could have
// been registered and never written and every gate would still be green.
//
// The wrapped function is invoked synchronously rather than with `go`: the
// recover() is the same mechanism either way, and a goroutine would add a
// synchronisation point for no extra coverage.
func TestRecoverGoRoutineCountsThePanicAndDoesNotPropagate(t *testing.T) {
	const component = "recovery_test_panicking"
	logger := zerolog.New(io.Discard)

	seriesBefore := testutil.CollectAndCount(PanicRecoveriesTotal, "ha_panic_recoveries_total")

	ran := false
	wrapped := RecoverGoRoutine(logger, component, func(context.Context) {
		ran = true
		panic("deliberate panic from the recovery test")
	})

	require.NotPanics(t, func() { wrapped(context.Background()) },
		"RecoverGoRoutine must swallow the panic -- an escaping panic in a goroutine takes the process down")
	require.True(t, ran, "the wrapped function never ran, so the panic under test never happened")

	require.Equal(t, float64(1), testutil.ToFloat64(PanicRecoveriesTotal.WithLabelValues(component)),
		"the panic did not reach ha_panic_recoveries_total{component=%q}", component)

	// The series did not exist until the panic created it. This is why the live
	// gate cannot assert "panic_recoveries_total == 0": a CounterVec with no
	// child emits no family at all, so on a healthy fleet the metric is ABSENT,
	// not zero. The gate therefore asserts "no series above zero" and leans on
	// this test plus the registry test for the rename risk.
	require.Equal(t, seriesBefore+1,
		testutil.CollectAndCount(PanicRecoveriesTotal, "ha_panic_recoveries_total"),
		"the panic should have created exactly one new component series")
}

// TestRecoverGoRoutineLeavesTheCounterAloneWhenNothingPanics pins the other
// direction: a wrapper that counted every call would make the panic signal
// unalertable.
func TestRecoverGoRoutineLeavesTheCounterAloneWhenNothingPanics(t *testing.T) {
	const component = "recovery_test_clean"
	logger := zerolog.New(io.Discard)

	gotCtx := false
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "v")

	wrapped := RecoverGoRoutine(logger, component, func(inner context.Context) {
		gotCtx = inner.Value(ctxKey{}) == "v"
	})
	wrapped(ctx)

	require.True(t, gotCtx, "the context passed at call time must reach the wrapped function")
	require.Equal(t, float64(0), testutil.ToFloat64(PanicRecoveriesTotal.WithLabelValues(component)),
		"a clean run must not increment the panic counter")
}
