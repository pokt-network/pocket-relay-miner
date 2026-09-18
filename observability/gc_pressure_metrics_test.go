//go:build test

package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// The miner's memory brake is judged by whether the GC keeps up with the limit:
// the heap goal, the cycles it completes, whether its CPU limiter let the heap
// grow past the goal, and the CPU it takes.
func TestGoCollectorExportsTheGCPressureMetrics(t *testing.T) {
	for name, registry := range map[string]*prometheus.Registry{"miner": MinerRegistry, "relayer": RelayerRegistry} {
		families, err := registry.Gather()
		require.NoError(t, err)
		names := make([]string, 0, len(families))
		for _, family := range families {
			names = append(names, family.GetName())
		}
		for _, want := range []string{
			"go_gc_heap_goal_bytes",
			"go_gc_cycles_total_gc_cycles_total",
			"go_gc_limiter_last_enabled_gc_cycle",
			"go_cpu_classes_gc_total_cpu_seconds_total",
			"go_cpu_classes_gc_mark_assist_cpu_seconds_total",
		} {
			require.Contains(t, names, want, "LINK gc-pressure-exported: %s registry", name)
		}
		require.Contains(t, names, "go_memstats_heap_alloc_bytes", "the collector's defaults are kept")
	}
}
