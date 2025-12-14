package observability

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// runtimeMetrics holds all runtime metric collectors
type runtimeMetrics struct {
	goroutines    prometheus.Gauge
	threads       prometheus.Gauge
	heapAlloc     prometheus.Gauge
	heapSys       prometheus.Gauge
	heapIdle      prometheus.Gauge
	heapInuse     prometheus.Gauge
	heapReleased  prometheus.Gauge
	heapObjects   prometheus.Gauge
	stackInuse    prometheus.Gauge
	stackSys      prometheus.Gauge
	mallocs       prometheus.Counter
	frees         prometheus.Counter
	gcPauseTotal  prometheus.Counter
	numGC         prometheus.Counter
	numForcedGC   prometheus.Counter
	lastGC        prometheus.Gauge
	nextGC        prometheus.Gauge
	gcCPUFraction prometheus.Gauge
}

// newRuntimeMetrics creates runtime metrics using the given factory
func newRuntimeMetrics(factory promauto.Factory) *runtimeMetrics {
	return &runtimeMetrics{
		goroutines: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "goroutines",
				Help:      "Number of goroutines",
			},
		),
		threads: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "threads",
				Help:      "Number of OS threads",
			},
		),
		heapAlloc: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "heap_alloc_bytes",
				Help:      "Bytes of allocated heap objects",
			},
		),
		heapSys: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "heap_sys_bytes",
				Help:      "Bytes of heap memory obtained from the OS",
			},
		),
		heapIdle: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "heap_idle_bytes",
				Help:      "Bytes in idle (unused) spans",
			},
		),
		heapInuse: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "heap_inuse_bytes",
				Help:      "Bytes in in-use spans",
			},
		),
		heapReleased: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "heap_released_bytes",
				Help:      "Bytes of physical memory returned to the OS",
			},
		),
		heapObjects: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "heap_objects",
				Help:      "Number of allocated heap objects",
			},
		),
		stackInuse: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "stack_inuse_bytes",
				Help:      "Bytes in stack spans",
			},
		),
		stackSys: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "stack_sys_bytes",
				Help:      "Bytes of stack memory obtained from the OS",
			},
		),
		mallocs: factory.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "mallocs_total",
				Help:      "Cumulative count of heap objects allocated",
			},
		),
		frees: factory.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "frees_total",
				Help:      "Cumulative count of heap objects freed",
			},
		),
		gcPauseTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "gc_pause_total_nanoseconds",
				Help:      "Cumulative nanoseconds in GC stop-the-world pauses",
			},
		),
		numGC: factory.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "gc_completed_total",
				Help:      "Number of completed GC cycles",
			},
		),
		numForcedGC: factory.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "gc_forced_total",
				Help:      "Number of GC cycles forced by the application",
			},
		),
		lastGC: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "last_gc_timestamp_seconds",
				Help:      "Timestamp of last GC",
			},
		),
		nextGC: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "next_gc_heap_size_bytes",
				Help:      "Target heap size of the next GC cycle",
			},
		),
		gcCPUFraction: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: "runtime",
				Name:      "gc_cpu_fraction",
				Help:      "Fraction of CPU time used by GC",
			},
		),
	}
}

// RuntimeMetricsCollectorConfig configures the runtime metrics collector.
type RuntimeMetricsCollectorConfig struct {
	// CollectionInterval is how often to collect runtime metrics.
	CollectionInterval time.Duration
}

// DefaultRuntimeMetricsCollectorConfig returns sensible defaults.
func DefaultRuntimeMetricsCollectorConfig() RuntimeMetricsCollectorConfig {
	return RuntimeMetricsCollectorConfig{
		CollectionInterval: 10 * time.Second,
	}
}

// RuntimeMetricsCollector periodically collects Go runtime metrics.
type RuntimeMetricsCollector struct {
	logger  logging.Logger
	config  RuntimeMetricsCollectorConfig
	metrics *runtimeMetrics

	// Previous values for delta calculations
	lastMallocs      uint64
	lastFrees        uint64
	lastGCPauseTotal uint64
	lastNumGC        uint32
	lastNumForcedGC  uint32

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	running  bool
}

// NewRuntimeMetricsCollector creates a new runtime metrics collector.
func NewRuntimeMetricsCollector(
	logger logging.Logger,
	config RuntimeMetricsCollectorConfig,
	factory promauto.Factory,
) *RuntimeMetricsCollector {
	if config.CollectionInterval == 0 {
		config.CollectionInterval = 10 * time.Second
	}

	return &RuntimeMetricsCollector{
		logger:  logging.ForComponent(logger, logging.ComponentRuntimeMetrics),
		config:  config,
		metrics: newRuntimeMetrics(factory),
	}
}

// Start begins collecting runtime metrics.
func (c *RuntimeMetricsCollector) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return nil
	}

	c.ctx, c.cancelFn = context.WithCancel(ctx)
	c.running = true

	// Collect initial values
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	c.lastMallocs = memStats.Mallocs
	c.lastFrees = memStats.Frees
	c.lastGCPauseTotal = memStats.PauseTotalNs
	c.lastNumGC = memStats.NumGC
	c.lastNumForcedGC = memStats.NumForcedGC

	c.wg.Add(1)
	go c.collectLoop()

	c.logger.Info().
		Dur("collection_interval", c.config.CollectionInterval).
		Msg("runtime metrics collector started")

	return nil
}

// Stop stops collecting runtime metrics.
func (c *RuntimeMetricsCollector) Stop() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	c.running = false
	c.cancelFn()
	c.mu.Unlock()

	c.wg.Wait()
	c.logger.Info().Msg("runtime metrics collector stopped")
}

// collectLoop periodically collects runtime metrics.
func (c *RuntimeMetricsCollector) collectLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.CollectionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.collect()
		}
	}
}

// collect reads runtime metrics and updates Prometheus gauges.
func (c *RuntimeMetricsCollector) collect() {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// Goroutines and threads
	c.metrics.goroutines.Set(float64(runtime.NumGoroutine()))
	c.metrics.threads.Set(float64(runtime.GOMAXPROCS(0)))

	// Heap metrics
	c.metrics.heapAlloc.Set(float64(memStats.HeapAlloc))
	c.metrics.heapSys.Set(float64(memStats.HeapSys))
	c.metrics.heapIdle.Set(float64(memStats.HeapIdle))
	c.metrics.heapInuse.Set(float64(memStats.HeapInuse))
	c.metrics.heapReleased.Set(float64(memStats.HeapReleased))
	c.metrics.heapObjects.Set(float64(memStats.HeapObjects))

	// Stack metrics
	c.metrics.stackInuse.Set(float64(memStats.StackInuse))
	c.metrics.stackSys.Set(float64(memStats.StackSys))

	// Allocation deltas (as counters)
	if memStats.Mallocs > c.lastMallocs {
		c.metrics.mallocs.Add(float64(memStats.Mallocs - c.lastMallocs))
		c.lastMallocs = memStats.Mallocs
	}
	if memStats.Frees > c.lastFrees {
		c.metrics.frees.Add(float64(memStats.Frees - c.lastFrees))
		c.lastFrees = memStats.Frees
	}

	// GC metrics
	if memStats.PauseTotalNs > c.lastGCPauseTotal {
		c.metrics.gcPauseTotal.Add(float64(memStats.PauseTotalNs - c.lastGCPauseTotal))
		c.lastGCPauseTotal = memStats.PauseTotalNs
	}
	if memStats.NumGC > c.lastNumGC {
		c.metrics.numGC.Add(float64(memStats.NumGC - c.lastNumGC))
		c.lastNumGC = memStats.NumGC
	}
	if memStats.NumForcedGC > c.lastNumForcedGC {
		c.metrics.numForcedGC.Add(float64(memStats.NumForcedGC - c.lastNumForcedGC))
		c.lastNumForcedGC = memStats.NumForcedGC
	}

	c.metrics.lastGC.Set(float64(memStats.LastGC) / 1e9)
	c.metrics.nextGC.Set(float64(memStats.NextGC))
	c.metrics.gcCPUFraction.Set(memStats.GCCPUFraction)
}

// CollectNow triggers an immediate collection of runtime metrics.
func (c *RuntimeMetricsCollector) CollectNow() {
	c.collect()
}
