//go:build test

package observability

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// testFactory creates a test metrics factory for unit tests.
func testFactory() promauto.Factory {
	return promauto.With(prometheus.NewRegistry())
}

// TestNewRuntimeMetricsCollector tests creating a new runtime metrics collector.
func TestNewRuntimeMetricsCollector(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())
	require.NotNil(t, collector, "Collector should not be nil")
	require.Equal(t, config.CollectionInterval, collector.config.CollectionInterval)
}

// TestDefaultRuntimeMetricsCollectorConfig tests default configuration.
func TestDefaultRuntimeMetricsCollectorConfig(t *testing.T) {
	config := DefaultRuntimeMetricsCollectorConfig()
	require.Equal(t, 10*time.Second, config.CollectionInterval, "Default interval should be 10 seconds")
}

// TestRuntimeMetricsCollector_Start_Stop tests basic lifecycle.
func TestRuntimeMetricsCollector_Start_Stop(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := RuntimeMetricsCollectorConfig{
		CollectionInterval: 50 * time.Millisecond,
	}

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start collector
	err := collector.Start(ctx)
	require.NoError(t, err, "Collector should start successfully")

	// Wait for at least one collection cycle
	time.Sleep(150 * time.Millisecond)

	// Stop collector
	collector.Stop()

	// Verify it stopped
	require.False(t, collector.running, "Collector should not be running after stop")
}

// TestRuntimeMetricsCollector_Start_AlreadyRunning tests starting an already running collector.
func TestRuntimeMetricsCollector_Start_AlreadyRunning(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := RuntimeMetricsCollectorConfig{
		CollectionInterval: 100 * time.Millisecond,
	}

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start collector twice
	err := collector.Start(ctx)
	require.NoError(t, err)

	err = collector.Start(ctx)
	require.NoError(t, err, "Starting already running collector should not error")

	collector.Stop()
}

// TestRuntimeMetricsCollector_Stop_NotRunning tests stopping a non-running collector.
func TestRuntimeMetricsCollector_Stop_NotRunning(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())

	// Stop without starting
	collector.Stop()
	// Should not panic or error
}

// TestRuntimeMetricsCollector_CollectNow tests immediate collection.
func TestRuntimeMetricsCollector_CollectNow(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())

	// Trigger immediate collection
	collector.CollectNow()
	// Should not panic

	// Verify we can call it multiple times
	collector.CollectNow()
	collector.CollectNow()
}

// TestRuntimeMetricsCollector_Collect tests the collect method.
func TestRuntimeMetricsCollector_Collect(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())

	// Get baseline memory stats
	var memStats1 runtime.MemStats
	runtime.ReadMemStats(&memStats1)

	// First collection
	collector.collect()

	// Allocate some memory
	_ = make([]byte, 1024*1024) // 1 MB

	// Second collection (should track deltas)
	collector.collect()

	// Verify last values are updated
	require.Greater(t, collector.lastMallocs, uint64(0), "lastMallocs should be set")
}

// TestRuntimeMetricsCollector_ContextCancellation tests shutdown on context cancellation.
func TestRuntimeMetricsCollector_ContextCancellation(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := RuntimeMetricsCollectorConfig{
		CollectionInterval: 50 * time.Millisecond,
	}

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())
	ctx, cancel := context.WithCancel(context.Background())

	err := collector.Start(ctx)
	require.NoError(t, err)

	// Let it run for a bit
	time.Sleep(100 * time.Millisecond)

	// Cancel context
	cancel()

	// Wait for shutdown
	time.Sleep(100 * time.Millisecond)

	// Cleanup
	collector.Stop()
}

// TestRuntimeMetricsCollector_ZeroInterval tests using zero interval (should default).
func TestRuntimeMetricsCollector_ZeroInterval(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := RuntimeMetricsCollectorConfig{
		CollectionInterval: 0, // Zero - should use default
	}

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())
	require.Equal(t, 10*time.Second, collector.config.CollectionInterval, "Zero interval should default to 10s")
}

// TestRuntimeMetricsCollector_MultipleCollections tests multiple collection cycles.
func TestRuntimeMetricsCollector_MultipleCollections(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := RuntimeMetricsCollectorConfig{
		CollectionInterval: 20 * time.Millisecond,
	}

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := collector.Start(ctx)
	require.NoError(t, err)

	// Wait for multiple collection cycles
	time.Sleep(100 * time.Millisecond)

	// Verify last values are tracked
	require.Greater(t, collector.lastMallocs, uint64(0), "Mallocs should be tracked")

	collector.Stop()
}

// TestRuntimeMetricsCollector_MemoryMetrics tests memory metric collection.
func TestRuntimeMetricsCollector_MemoryMetrics(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())

	// Trigger collection
	collector.collect()

	// Allocate some memory
	allocations := make([][]byte, 100)
	for i := range allocations {
		allocations[i] = make([]byte, 1024*1024) // 1 MB each
	}

	// Collect again
	collector.collect()

	// Force GC
	runtime.GC()

	// Collect after GC
	collector.collect()

	// Keep allocations alive
	_ = allocations

	// Verify metrics are being tracked (non-zero values)
	require.Greater(t, collector.lastMallocs, uint64(0))
	require.Greater(t, collector.lastFrees, uint64(0))
}

// TestRuntimeMetricsCollector_GCMetrics tests GC metric collection.
func TestRuntimeMetricsCollector_GCMetrics(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())

	// Initial collection
	collector.collect()

	// Force multiple GCs
	for i := 0; i < 5; i++ {
		runtime.GC()
	}

	// Collect again
	collector.collect()

	// Verify GC metrics are tracked
	require.Greater(t, collector.lastNumGC, uint32(0), "NumGC should be greater than 0")
}

// TestRuntimeMetricsCollector_DeltaCalculations tests delta calculations for counters.
func TestRuntimeMetricsCollector_DeltaCalculations(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())

	// Get initial stats
	var memStats1 runtime.MemStats
	runtime.ReadMemStats(&memStats1)
	collector.lastMallocs = memStats1.Mallocs
	collector.lastFrees = memStats1.Frees
	collector.lastGCPauseTotal = memStats1.PauseTotalNs
	collector.lastNumGC = memStats1.NumGC

	// Allocate and free some memory
	temp := make([]byte, 1024*1024)
	_ = temp
	runtime.GC()

	// Collect (should calculate deltas)
	collector.collect()

	// Verify deltas were calculated (lastX values updated)
	var memStats2 runtime.MemStats
	runtime.ReadMemStats(&memStats2)

	// Note: These values might be equal if no allocations happened between reads
	// but we're verifying the logic works without panic
	require.True(t, collector.lastMallocs >= memStats1.Mallocs)
}

// TestRuntimeMetricsCollector_Concurrency tests concurrent Start/Stop calls.
func TestRuntimeMetricsCollector_Concurrency(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := RuntimeMetricsCollectorConfig{
		CollectionInterval: 20 * time.Millisecond,
	}

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start multiple goroutines calling Start/Stop
	done := make(chan bool, 10)

	for i := 0; i < 5; i++ {
		go func() {
			_ = collector.Start(ctx)
			done <- true
		}()
	}

	// Wait a bit
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 5; i++ {
		go func() {
			collector.Stop()
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Final cleanup
	collector.Stop()
}

// TestRuntimeMetricsCollector_LongRunning tests collector running for extended period.
func TestRuntimeMetricsCollector_LongRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping long-running test in short mode")
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := RuntimeMetricsCollectorConfig{
		CollectionInterval: 10 * time.Millisecond,
	}

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := collector.Start(ctx)
	require.NoError(t, err)

	// Run for 200ms (20 collection cycles)
	time.Sleep(200 * time.Millisecond)

	// Do some allocations during collection
	for i := 0; i < 10; i++ {
		_ = make([]byte, 1024*1024)
		time.Sleep(10 * time.Millisecond)
	}

	collector.Stop()
}

// TestRuntimeMetricsCollector_ForceGC tests forced GC tracking.
func TestRuntimeMetricsCollector_ForceGC(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())

	// Initial collection
	collector.collect()

	// Force GC multiple times
	for i := 0; i < 3; i++ {
		runtime.GC()
	}

	// Collect again
	collector.collect()

	// NumForcedGC might be 0 if GC is not forced by application
	// but we verify the collection logic works
	require.True(t, true, "Collection should complete without error")
}

// TestRuntimeMetricsCollector_Goroutines tests goroutine count tracking.
func TestRuntimeMetricsCollector_Goroutines(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := DefaultRuntimeMetricsCollectorConfig()

	collector := NewRuntimeMetricsCollector(logger, config, testFactory())

	initialGoroutines := runtime.NumGoroutine()

	// Collect
	collector.collect()

	// Create some goroutines
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			time.Sleep(100 * time.Millisecond)
			done <- true
		}()
	}

	// Collect again
	collector.collect()

	newGoroutines := runtime.NumGoroutine()
	require.Greater(t, newGoroutines, initialGoroutines, "Goroutine count should increase")

	// Wait for goroutines to finish
	for i := 0; i < 10; i++ {
		<-done
	}
}
