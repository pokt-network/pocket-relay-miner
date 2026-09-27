//go:build test

package miner

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// captureAdvisory runs LogStartupCapacityAdvisory against an in-memory logger
// and returns everything it wrote, so a test can assert which warnings fired.
func captureAdvisory(t *testing.T, mutate func(c *Config), numSuppliers int) string {
	t.Helper()
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.TraceLevel)

	c := DefaultConfig()
	// Healthy baseline: no master-pool override.
	c.WorkerPools.MasterPoolSize = 0
	if mutate != nil {
		mutate(c)
	}
	c.LogStartupCapacityAdvisory(logger, numSuppliers)
	return buf.String()
}

const (
	advisoryCPUMarker  = "LIKELY UNDER-PROVISIONED"
	advisoryPoolMarker = "master_pool_size is set BELOW"
)

// A healthy config (batching on, auto pool, few keys per CPU) is silent — the
// advisory must not cry wolf, or operators learn to ignore it.
func TestCapacityAdvisory_HealthyConfig_Silent(t *testing.T) {
	out := captureAdvisory(t, nil, 5)
	require.NotContains(t, out, advisoryCPUMarker)
	require.NotContains(t, out, advisoryPoolMarker)
}

func TestCapacityAdvisory_MasterPoolBelowAuto_Warns(t *testing.T) {
	out := captureAdvisory(t, func(c *Config) {
		c.WorkerPools.MasterPoolSize = 1 // far below any auto-calc for >0 suppliers
	}, 10)
	require.Contains(t, out, advisoryPoolMarker, "below-auto master pool must warn")
}

// Too many supplier keys per CPU fires the under-provisioned warning. The
// threshold is suppliersPerCPUWarnThreshold per effective CPU.
func TestCapacityAdvisory_SuppliersPerCPUOverThreshold_Warns(t *testing.T) {
	cpu := getEffectiveCPUCount()
	require.Positive(t, cpu, "effective CPU must be detectable")
	over := cpu*suppliersPerCPUWarnThreshold + 1

	out := captureAdvisory(t, nil, over)
	require.Contains(t, out, advisoryCPUMarker, "over-threshold suppliers/CPU must warn")

	// One under the threshold stays silent (boundary check).
	under := captureAdvisory(t, nil, cpu*suppliersPerCPUWarnThreshold)
	require.NotContains(t, under, advisoryCPUMarker, "at-threshold must NOT warn")
}

// strings import kept meaningful: guard that the JSON the logger emits is
// non-empty when a warning fires (sanity that capture works at all).
func TestCapacityAdvisory_CaptureSanity(t *testing.T) {
	out := captureAdvisory(t, func(c *Config) { c.WorkerPools.MasterPoolSize = 1 }, 5)
	require.True(t, strings.Contains(out, "\"level\":\"warn\""), "warning must be emitted at warn level")
}
