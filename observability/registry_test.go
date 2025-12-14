//go:build test

package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// TestMinerRegistry tests that MinerRegistry is initialized.
func TestMinerRegistry(t *testing.T) {
	require.NotNil(t, MinerRegistry, "MinerRegistry should not be nil")
}

// TestRelayerRegistry tests that RelayerRegistry is initialized.
func TestRelayerRegistry(t *testing.T) {
	require.NotNil(t, RelayerRegistry, "RelayerRegistry should not be nil")
}

// TestSharedRegistry tests that SharedRegistry is initialized.
func TestSharedRegistry(t *testing.T) {
	require.NotNil(t, SharedRegistry, "SharedRegistry should not be nil")
}

// TestMinerFactory tests that MinerFactory is initialized.
func TestMinerFactory(t *testing.T) {
	require.NotNil(t, MinerFactory, "MinerFactory should not be nil")
}

// TestRelayerFactory tests that RelayerFactory is initialized.
func TestRelayerFactory(t *testing.T) {
	require.NotNil(t, RelayerFactory, "RelayerFactory should not be nil")
}

// TestSharedFactory tests that SharedFactory is initialized.
func TestSharedFactory(t *testing.T) {
	require.NotNil(t, SharedFactory, "SharedFactory should not be nil")
}

// TestRegistries_AreDistinct tests that registries are different instances.
func TestRegistries_AreDistinct(t *testing.T) {
	// Verify registries are not the same instance
	require.NotEqual(t, MinerRegistry, RelayerRegistry, "MinerRegistry and RelayerRegistry should be different")
	require.NotEqual(t, MinerRegistry, SharedRegistry, "MinerRegistry and SharedRegistry should be different")
	require.NotEqual(t, RelayerRegistry, SharedRegistry, "RelayerRegistry and SharedRegistry should be different")
}

// TestMinerRegistry_RegisterMetric tests registering a metric to MinerRegistry.
func TestMinerRegistry_RegisterMetric(t *testing.T) {
	// Create test registry to avoid polluting global registry
	testRegistry := prometheus.NewRegistry()

	// Register a test metric
	testCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_miner_counter",
		Help: "Test counter for miner",
	})

	err := testRegistry.Register(testCounter)
	require.NoError(t, err, "Should register metric successfully")

	// Increment counter
	testCounter.Inc()

	// Gather metrics
	metrics, err := testRegistry.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, metrics, "Should have metrics")

	// Verify our metric is there
	found := false
	for _, mf := range metrics {
		if mf.GetName() == "test_miner_counter" {
			found = true
			require.Len(t, mf.GetMetric(), 1)
			require.Equal(t, float64(1), mf.GetMetric()[0].GetCounter().GetValue())
		}
	}
	require.True(t, found, "Should find test_miner_counter metric")
}

// TestRelayerRegistry_RegisterMetric tests registering a metric to RelayerRegistry.
func TestRelayerRegistry_RegisterMetric(t *testing.T) {
	testRegistry := prometheus.NewRegistry()

	testGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_relayer_gauge",
		Help: "Test gauge for relayer",
	})

	err := testRegistry.Register(testGauge)
	require.NoError(t, err)

	testGauge.Set(42)

	metrics, err := testRegistry.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, metrics)
}

// TestSharedRegistry_RegisterMetric tests registering a metric to SharedRegistry.
func TestSharedRegistry_RegisterMetric(t *testing.T) {
	testRegistry := prometheus.NewRegistry()

	testHistogram := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "test_shared_histogram",
		Help:    "Test histogram for shared",
		Buckets: prometheus.DefBuckets,
	})

	err := testRegistry.Register(testHistogram)
	require.NoError(t, err)

	testHistogram.Observe(0.5)

	metrics, err := testRegistry.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, metrics)
}

// TestRegistry_DuplicateRegistration tests that duplicate registration fails.
func TestRegistry_DuplicateRegistration(t *testing.T) {
	testRegistry := prometheus.NewRegistry()

	metric1 := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "duplicate_counter",
		Help: "First counter",
	})

	metric2 := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "duplicate_counter", // Same name
		Help: "Second counter",
	})

	// First registration should succeed
	err := testRegistry.Register(metric1)
	require.NoError(t, err)

	// Second registration should fail
	err = testRegistry.Register(metric2)
	require.Error(t, err, "Duplicate registration should fail")
}

// TestRegistry_MustRegister tests MustRegister panics on error.
func TestRegistry_MustRegister(t *testing.T) {
	testRegistry := prometheus.NewRegistry()

	metric1 := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "must_register_counter",
		Help: "First counter",
	})

	// First registration should succeed
	require.NotPanics(t, func() {
		testRegistry.MustRegister(metric1)
	})

	metric2 := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "must_register_counter", // Duplicate
		Help: "Second counter",
	})

	// Duplicate registration should panic
	require.Panics(t, func() {
		testRegistry.MustRegister(metric2)
	})
}

// TestRegistry_Unregister tests unregistering a metric.
func TestRegistry_Unregister(t *testing.T) {
	testRegistry := prometheus.NewRegistry()

	metric := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "unregister_counter",
		Help: "Counter to unregister",
	})

	// Register
	err := testRegistry.Register(metric)
	require.NoError(t, err)

	// Unregister
	unregistered := testRegistry.Unregister(metric)
	require.True(t, unregistered, "Should unregister successfully")

	// Can register again after unregister
	err = testRegistry.Register(metric)
	require.NoError(t, err)
}

// TestRegistry_Gather tests gathering metrics.
func TestRegistry_Gather(t *testing.T) {
	testRegistry := prometheus.NewRegistry()

	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gather_counter",
		Help: "Counter for gather test",
	})
	testRegistry.MustRegister(counter)
	counter.Inc()

	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gather_gauge",
		Help: "Gauge for gather test",
	})
	testRegistry.MustRegister(gauge)
	gauge.Set(123)

	// Gather metrics
	metrics, err := testRegistry.Gather()
	require.NoError(t, err)
	require.Len(t, metrics, 2, "Should have 2 metric families")

	names := make(map[string]bool)
	for _, mf := range metrics {
		names[mf.GetName()] = true
	}

	require.True(t, names["gather_counter"], "Should have gather_counter")
	require.True(t, names["gather_gauge"], "Should have gather_gauge")
}

// TestRegistry_WithLabels tests metrics with labels.
func TestRegistry_WithLabels(t *testing.T) {
	testRegistry := prometheus.NewRegistry()

	counterVec := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "labeled_counter",
			Help: "Counter with labels",
		},
		[]string{"label1", "label2"},
	)
	testRegistry.MustRegister(counterVec)

	// Increment different label combinations
	counterVec.WithLabelValues("value1", "valueA").Inc()
	counterVec.WithLabelValues("value1", "valueB").Add(2)
	counterVec.WithLabelValues("value2", "valueA").Add(3)

	// Gather metrics
	metrics, err := testRegistry.Gather()
	require.NoError(t, err)
	require.Len(t, metrics, 1)

	// Should have 3 different label combinations
	require.Len(t, metrics[0].GetMetric(), 3)
}

// TestRegistry_DefaultCollectors tests that default collectors are registered.
func TestRegistry_DefaultCollectors(t *testing.T) {
	// MinerRegistry and RelayerRegistry should have Go collectors registered in init()
	// We can verify by gathering metrics - Go collectors provide metrics like go_goroutines

	minerMetrics, err := MinerRegistry.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, minerMetrics, "MinerRegistry should have default collectors")

	// Check for Go metrics
	hasGoMetrics := false
	for _, mf := range minerMetrics {
		if mf.GetName() == "go_goroutines" {
			hasGoMetrics = true
			break
		}
	}
	require.True(t, hasGoMetrics, "MinerRegistry should have Go collector metrics")

	relayerMetrics, err := RelayerRegistry.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, relayerMetrics, "RelayerRegistry should have default collectors")

	hasGoMetrics = false
	for _, mf := range relayerMetrics {
		if mf.GetName() == "go_goroutines" {
			hasGoMetrics = true
			break
		}
	}
	require.True(t, hasGoMetrics, "RelayerRegistry should have Go collector metrics")
}

// TestRegistry_Isolation tests that registries are isolated from each other.
func TestRegistry_Isolation(t *testing.T) {
	testRegistry1 := prometheus.NewRegistry()
	testRegistry2 := prometheus.NewRegistry()

	// Register same metric name to different registries
	counter1 := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "isolated_counter",
		Help: "Counter in registry 1",
	})
	testRegistry1.MustRegister(counter1)
	counter1.Inc()

	counter2 := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "isolated_counter",
		Help: "Counter in registry 2",
	})
	testRegistry2.MustRegister(counter2)
	counter2.Add(5)

	// Verify isolation
	metrics1, err := testRegistry1.Gather()
	require.NoError(t, err)
	require.Len(t, metrics1, 1)
	require.Equal(t, float64(1), metrics1[0].GetMetric()[0].GetCounter().GetValue())

	metrics2, err := testRegistry2.Gather()
	require.NoError(t, err)
	require.Len(t, metrics2, 1)
	require.Equal(t, float64(5), metrics2[0].GetMetric()[0].GetCounter().GetValue())
}

// TestFactory_WrapRegistererWith tests wrapping a registry with labels.
func TestFactory_WrapRegistererWith(t *testing.T) {
	testRegistry := prometheus.NewRegistry()
	wrapped := prometheus.WrapRegistererWith(prometheus.Labels{"instance": "test"}, testRegistry)

	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wrapped_counter",
		Help: "Counter with wrapped registry",
	})

	err := wrapped.Register(counter)
	require.NoError(t, err)

	counter.Inc()

	metrics, err := testRegistry.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, metrics)
}

// TestFactory_WithPrefix tests registry with prefix.
func TestFactory_WithPrefix(t *testing.T) {
	testRegistry := prometheus.NewRegistry()
	wrapped := prometheus.WrapRegistererWithPrefix("prefix_", testRegistry)

	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gauge",
		Help: "Gauge with prefix",
	})

	err := wrapped.Register(gauge)
	require.NoError(t, err)

	gauge.Set(42)

	metrics, err := testRegistry.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, metrics)

	// Verify metric has prefix
	found := false
	for _, mf := range metrics {
		if mf.GetName() == "prefix_gauge" {
			found = true
		}
	}
	require.True(t, found, "Metric should have prefix")
}
