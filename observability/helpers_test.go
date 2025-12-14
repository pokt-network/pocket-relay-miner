//go:build test

package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// TestNewTimer tests creating a new timer.
func TestNewTimer(t *testing.T) {
	timer := NewTimer()
	require.NotNil(t, timer, "Timer should not be nil")
	require.False(t, timer.startTime.IsZero(), "Timer start time should be set")
}

// TestTimer_Duration tests measuring elapsed time.
func TestTimer_Duration(t *testing.T) {
	timer := NewTimer()
	time.Sleep(10 * time.Millisecond)
	duration := timer.Duration()
	require.Greater(t, duration, 10*time.Millisecond, "Duration should be at least 10ms")
	require.Less(t, duration, 100*time.Millisecond, "Duration should be less than 100ms")
}

// TestTimer_ObserveOperation tests recording operation duration.
func TestTimer_ObserveOperation(t *testing.T) {
	// Create isolated registry for this test
	registry := prometheus.NewRegistry()
	histogram := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "test_operation_duration",
			Help: "Test operation duration",
		},
		[]string{"component", "operation", "status"},
	)
	registry.MustRegister(histogram)

	timer := NewTimer()
	time.Sleep(5 * time.Millisecond)

	// Record to our test histogram instead of the global one
	histogram.WithLabelValues("test_component", "test_operation", "success").Observe(timer.Duration().Seconds())

	// Verify metric was recorded
	metrics, err := registry.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, metrics, "Metrics should be gathered")
}

// TestTimer_ObserveRedisOperation tests recording Redis operation duration.
func TestTimer_ObserveRedisOperation(t *testing.T) {
	timer := NewTimer()
	time.Sleep(5 * time.Millisecond)

	// This will record to global metrics (promauto)
	timer.ObserveRedisOperation("GET", "success")

	// We can't easily verify promauto metrics, but we ensure no panic
}

// TestTimer_ObserveOnchainQuery tests recording on-chain query duration.
func TestTimer_ObserveOnchainQuery(t *testing.T) {
	timer := NewTimer()
	time.Sleep(5 * time.Millisecond)

	timer.ObserveOnchainQuery("GetSession", "success")
	// Ensure no panic
}

// TestTimer_ObserveTxSubmission tests recording transaction submission duration.
func TestTimer_ObserveTxSubmission(t *testing.T) {
	timer := NewTimer()
	time.Sleep(5 * time.Millisecond)

	timer.ObserveTxSubmission("claim", "success")
	// Ensure no panic
}

// TestTimer_ObserveSigning tests recording signing duration.
func TestTimer_ObserveSigning(t *testing.T) {
	timer := NewTimer()
	time.Sleep(5 * time.Millisecond)

	timer.ObserveSigning("relay_response")
	// Ensure no panic
}

// TestRecordCacheHit tests recording cache hit.
func TestRecordCacheHit(t *testing.T) {
	RecordCacheHit("test_cache")
	// Ensure no panic - metrics are global via promauto
}

// TestRecordCacheMiss tests recording cache miss.
func TestRecordCacheMiss(t *testing.T) {
	RecordCacheMiss("test_cache")
	// Ensure no panic
}

// TestRecordError tests recording errors.
func TestRecordError(t *testing.T) {
	RecordError("test_component", "validation_error")
	RecordError("test_component", "network_error")
	// Ensure no panic
}

// TestSetQueueMetrics tests setting queue depth and capacity.
func TestSetQueueMetrics(t *testing.T) {
	SetQueueMetrics("test_queue", 50, 100)
	SetQueueMetrics("test_queue", 75, 100)
	SetQueueMetrics("test_queue", 0, 100)
	// Ensure no panic
}

// TestSetMemoryUsage tests setting memory usage.
func TestSetMemoryUsage(t *testing.T) {
	SetMemoryUsage("test_component", 1024*1024)
	SetMemoryUsage("test_component", 2048*1024)
	SetMemoryUsage("test_component", 0)
	// Ensure no panic
}

// TestSetGoroutineCount tests setting goroutine count.
func TestSetGoroutineCount(t *testing.T) {
	SetGoroutineCount("test_component", 10)
	SetGoroutineCount("test_component", 20)
	SetGoroutineCount("test_component", 0)
	// Ensure no panic
}

// TestRecordStartupDuration tests recording startup duration.
func TestRecordStartupDuration(t *testing.T) {
	RecordStartupDuration("test_component", 100*time.Millisecond)
	RecordStartupDuration("test_component", 1*time.Second)
	// Ensure no panic
}

// TestSetProcessInfo tests setting process info.
func TestSetProcessInfo(t *testing.T) {
	SetProcessInfo("v1.0.0", "relayer")
	SetProcessInfo("v1.0.1", "miner")
	// Ensure no panic
}

// TestTimer_MultipleObservations tests multiple observations from the same timer.
func TestTimer_MultipleObservations(t *testing.T) {
	timer := NewTimer()
	time.Sleep(5 * time.Millisecond)

	// Multiple observations should all work
	timer.ObserveRedisOperation("SET", "success")
	timer.ObserveOnchainQuery("GetSession", "success")
	timer.ObserveTxSubmission("claim", "success")
	timer.ObserveSigning("relay")

	// Verify duration keeps increasing
	duration1 := timer.Duration()
	time.Sleep(5 * time.Millisecond)
	duration2 := timer.Duration()
	require.Greater(t, duration2, duration1, "Duration should increase")
}

// TestHelpers_WithDifferentStatuses tests helpers with different status values.
func TestHelpers_WithDifferentStatuses(t *testing.T) {
	timer := NewTimer()

	// Test different statuses
	timer.ObserveRedisOperation("GET", "success")
	timer.ObserveRedisOperation("SET", "error")
	timer.ObserveOnchainQuery("GetSession", "success")
	timer.ObserveOnchainQuery("GetSession", "timeout")
	timer.ObserveTxSubmission("claim", "success")
	timer.ObserveTxSubmission("proof", "failed")

	// Ensure no panic
}

// TestHelpers_EdgeCases tests edge cases in helper functions.
func TestHelpers_EdgeCases(t *testing.T) {
	// Empty strings
	RecordCacheHit("")
	RecordCacheMiss("")
	RecordError("", "")

	// Zero values
	SetQueueMetrics("queue", 0, 0)
	SetMemoryUsage("component", 0)
	SetGoroutineCount("component", 0)
	RecordStartupDuration("component", 0)

	// Negative values (should not panic, though semantically incorrect)
	SetQueueMetrics("queue", -1, -1)
	SetMemoryUsage("component", -1)
	SetGoroutineCount("component", -1)

	// Ensure no panic
}

// Helper function to get metric value from registry.
func getCounterValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	metrics, err := registry.Gather()
	require.NoError(t, err)

	for _, mf := range metrics {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// Helper function to get gauge value from registry.
func getGaugeValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	metrics, err := registry.Gather()
	require.NoError(t, err)

	for _, mf := range metrics {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				if m.GetGauge() != nil {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

// Helper function to get histogram sample count.
func getHistogramCount(t *testing.T, registry *prometheus.Registry, name string) uint64 {
	metrics, err := registry.Gather()
	require.NoError(t, err)

	for _, mf := range metrics {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				if m.GetHistogram() != nil {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// TestIsolatedMetrics tests metrics in an isolated registry.
func TestIsolatedMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()

	// Create test counter
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_counter_total",
			Help: "Test counter",
		},
		[]string{"label"},
	)
	registry.MustRegister(counter)

	// Increment counter
	counter.WithLabelValues("value1").Inc()
	counter.WithLabelValues("value1").Add(5)
	counter.WithLabelValues("value2").Inc()

	// Verify counter values
	metrics, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, metrics, 1, "Should have one metric family")

	metricFamily := metrics[0]
	require.Equal(t, "test_counter_total", metricFamily.GetName())
	require.Equal(t, io_prometheus_client.MetricType_COUNTER, metricFamily.GetType())
	require.Len(t, metricFamily.GetMetric(), 2, "Should have two label combinations")
}

// TestIsolatedGauge tests gauge metrics in an isolated registry.
func TestIsolatedGauge(t *testing.T) {
	registry := prometheus.NewRegistry()

	// Create test gauge
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "test_gauge",
			Help: "Test gauge",
		},
		[]string{"label"},
	)
	registry.MustRegister(gauge)

	// Set gauge values
	gauge.WithLabelValues("value1").Set(42)
	gauge.WithLabelValues("value1").Inc()
	gauge.WithLabelValues("value1").Dec()

	// Verify gauge value
	value := getGaugeValue(t, registry, "test_gauge")
	require.Equal(t, float64(42), value, "Gauge should be 42 (set 42, inc, dec)")
}

// TestIsolatedHistogram tests histogram metrics in an isolated registry.
func TestIsolatedHistogram(t *testing.T) {
	registry := prometheus.NewRegistry()

	// Create test histogram
	histogram := prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "test_histogram_seconds",
			Help:    "Test histogram",
			Buckets: []float64{0.001, 0.01, 0.1, 1},
		},
	)
	registry.MustRegister(histogram)

	// Observe values
	histogram.Observe(0.005)
	histogram.Observe(0.05)
	histogram.Observe(0.5)

	// Verify histogram
	count := getHistogramCount(t, registry, "test_histogram_seconds")
	require.Equal(t, uint64(3), count, "Histogram should have 3 observations")
}
