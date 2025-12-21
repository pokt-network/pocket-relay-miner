package relay

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// RelayMetrics tracks metrics for relay load testing.
type RelayMetrics struct {
	mu sync.Mutex

	// Request counts
	totalRequests int
	successCount  int
	errorCount    int

	// Latencies (in milliseconds)
	latencies []float64

	// Timing
	startTime time.Time
	endTime   time.Time

	// Error tracking
	errors map[string]int // error message -> count
}

// NewRelayMetrics creates a new metrics collector.
func NewRelayMetrics() *RelayMetrics {
	return &RelayMetrics{
		errors: make(map[string]int),
	}
}

// Start marks the beginning of the load test.
func (m *RelayMetrics) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startTime = time.Now()
}

// End marks the end of the load test.
func (m *RelayMetrics) End() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endTime = time.Now()
}

// RecordSuccess records a successful relay request.
func (m *RelayMetrics) RecordSuccess(latencyMs float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.totalRequests++
	m.successCount++
	m.latencies = append(m.latencies, latencyMs)
}

// RecordError records a failed relay request.
func (m *RelayMetrics) RecordError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.totalRequests++
	m.errorCount++

	// Track error types
	errMsg := err.Error()
	m.errors[errMsg]++
}

// GetSummary returns a formatted summary of the load test results.
func (m *RelayMetrics) GetSummary() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.totalRequests == 0 {
		return "No requests recorded"
	}

	duration := m.endTime.Sub(m.startTime)
	successRate := float64(m.successCount) / float64(m.totalRequests) * 100
	throughput := float64(m.totalRequests) / duration.Seconds()

	summary := fmt.Sprintf(`
=== Load Test Results ===
Total Requests: %d
Successful: %d
Errors: %d
Success Rate: %.2f%%

Duration: %s
Throughput: %.2f RPS
`,
		m.totalRequests,
		m.successCount,
		m.errorCount,
		successRate,
		duration.Round(time.Millisecond),
		throughput,
	)

	// Add latency percentiles if we have successful requests
	if len(m.latencies) > 0 {
		summary += m.getLatencyPercentiles()
	}

	// Add error breakdown if we have errors
	if m.errorCount > 0 {
		summary += m.getErrorBreakdown()
	}

	return summary
}

// getLatencyPercentiles calculates and formats latency percentiles.
func (m *RelayMetrics) getLatencyPercentiles() string {
	// Sort latencies
	sorted := make([]float64, len(m.latencies))
	copy(sorted, m.latencies)
	sort.Float64s(sorted)

	p50 := percentile(sorted, 50)
	p95 := percentile(sorted, 95)
	p99 := percentile(sorted, 99)
	min := sorted[0]
	max := sorted[len(sorted)-1]

	return fmt.Sprintf(`
Latency Percentiles (ms):
  min: %.2f
  p50: %.2f
  p95: %.2f
  p99: %.2f
  max: %.2f
`,
		min, p50, p95, p99, max,
	)
}

// getErrorBreakdown formats the error breakdown.
func (m *RelayMetrics) getErrorBreakdown() string {
	breakdown := "\nError Breakdown:\n"

	// Sort errors by count (descending)
	type errorCount struct {
		msg   string
		count int
	}
	var errors []errorCount
	for msg, count := range m.errors {
		errors = append(errors, errorCount{msg, count})
	}
	sort.Slice(errors, func(i, j int) bool {
		return errors[i].count > errors[j].count
	})

	// Show top 10 errors
	limit := 10
	if len(errors) < limit {
		limit = len(errors)
	}

	for i := 0; i < limit; i++ {
		breakdown += fmt.Sprintf("  %d: %s\n", errors[i].count, errors[i].msg)
	}

	if len(errors) > 10 {
		breakdown += fmt.Sprintf("  ... and %d more error types\n", len(errors)-10)
	}

	return breakdown
}

// percentile calculates the nth percentile from a sorted slice.
func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}

	// Calculate index (0-based)
	rank := float64(p) / 100.0 * float64(len(sorted)-1)
	lowerIndex := int(rank)
	upperIndex := lowerIndex + 1

	if upperIndex >= len(sorted) {
		return sorted[len(sorted)-1]
	}

	// Linear interpolation
	weight := rank - float64(lowerIndex)
	return sorted[lowerIndex]*(1-weight) + sorted[upperIndex]*weight
}
