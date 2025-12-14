package relayer

import (
	"time"

	pond "github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// MetricRecorder handles async metric recording to avoid blocking the hot path.
// Uses pond worker pool subpool for controlled concurrency.
type MetricRecorder struct {
	logger  logging.Logger
	subpool pond.Pool
}

// NewMetricRecorder creates a new async metric recorder using a pond subpool.
func NewMetricRecorder(logger logging.Logger, subpool pond.Pool) *MetricRecorder {
	return &MetricRecorder{
		logger:  logger,
		subpool: subpool,
	}
}

// Start is a no-op - workers are managed by pond subpool.
func (m *MetricRecorder) Start() {
	m.logger.Info().Msg("metric recorder ready (using pond subpool)")
}

// Record submits a metric observation to the pond subpool.
// Uses non-blocking submission with unbounded queue (never blocks hot path).
func (m *MetricRecorder) Record(histogram *prometheus.HistogramVec, labels []string, value float64) {
	// Submit to pond subpool (non-blocking, unbounded queue)
	m.subpool.Submit(func() {
		histogram.WithLabelValues(labels...).Observe(value)
	})
}

// RecordDuration is a convenience wrapper for recording time.Duration as seconds.
func (m *MetricRecorder) RecordDuration(histogram *prometheus.HistogramVec, labels []string, duration time.Duration) {
	m.Record(histogram, labels, duration.Seconds())
}

// Close is a no-op - subpool cleanup is handled by master pool.
func (m *MetricRecorder) Close() error {
	m.logger.Info().Msg("metric recorder stopped")
	return nil
}
