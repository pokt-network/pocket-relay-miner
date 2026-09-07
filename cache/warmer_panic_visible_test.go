//go:build test

package cache

import (
	"bytes"
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
)

// panickingAppClient embeds the interface as nil on purpose: only the method the
// warm path calls is defined, so any OTHER method the path reached would panic
// with its own name instead of silently doing nothing.
type panickingAppClient struct {
	client.ApplicationQueryClient
}

func (panickingAppClient) GetApplication(_ context.Context, _ string) (apptypes.Application, error) {
	panic("injected: application query blew up")
}

// A panic in a warm task is recovered by pond and handed back as the error of
// group.Wait(). Discarding it lost the panic twice over: nothing was logged or
// counted, AND the task updated neither WarmedApps nor FailedApps, so the
// summary silently stopped adding up to TotalApps.
func TestCacheWarmer_PanicInWarmTaskIsCountedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.TraceLevel)

	w := NewCacheWarmer(logger, CacheWarmerConfig{WarmupConcurrency: 2}, panickingAppClient{}, nil, nil)
	t.Cleanup(w.Stop)

	before := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("cache_warmup_app"))
	result := w.warmAppsParallel(context.Background(), []string{"app-1"})
	after := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("cache_warmup_app"))
	require.Greater(t, after, before,
		"the repo's convention is that a recovered panic is COUNTED and logged; the log alone leaves it out of alerting")

	require.Equal(t, 1, result.TotalApps)
	require.Equal(t, 0, result.WarmedApps+result.FailedApps,
		"a panicking task updates neither counter -- this is the arithmetic the log must explain")

	out := buf.String()
	require.Contains(t, out, "a warm task panicked",
		"a panic inside a warm task must be logged; discarding Wait's error left it with no trace at all")
	require.Contains(t, out, `"level":"error"`,
		"a recovered panic needs immediate attention, so it must not be logged below Error")
}

// The shutdown half of the same handling. pond answers a Submit made after the
// pool stopped with ErrPoolStopped, which arrives through the SAME channel as a
// recovered panic. Raising that at Error would spend, on every rollout that
// lands in this window, exactly the signal the panic branch exists to create.
func TestCacheWarmer_PoolStoppedIsNotReportedAsAPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.TraceLevel)

	w := NewCacheWarmer(logger, CacheWarmerConfig{WarmupConcurrency: 2}, panickingAppClient{}, nil, nil)
	w.Stop() // shutdown happens first; the warm pass below arrives too late

	before := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("cache_warmup_app"))
	w.warmAppsParallel(context.Background(), []string{"app-1"})
	after := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("cache_warmup_app"))

	require.Equal(t, before, after,
		"a stopped pool is not a panic; counting it would make the panic counter unusable as an alert")

	out := buf.String()
	require.NotContains(t, out, `"level":"error"`,
		"a pool stopped during shutdown must not raise Error -- that is the rollout noise this branch exists to avoid")
	require.Contains(t, out, "shutting down",
		"the abandonment must still be visible at Debug: silence and success would look identical")
}
