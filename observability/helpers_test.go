//go:build test

package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

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
