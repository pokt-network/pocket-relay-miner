package relay

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewRelayMetrics tests metrics initialization.
func TestNewRelayMetrics(t *testing.T) {
	metrics := NewRelayMetrics()

	require.NotNil(t, metrics, "NewRelayMetrics should return non-nil metrics")
	require.NotNil(t, metrics.errors, "errors map should be initialized")
}

// TestRecordSuccess tests success recording.
func TestRecordSuccess(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	metrics.RecordSuccess(10.5)
	metrics.RecordSuccess(20.3)
	metrics.RecordSuccess(15.7)

	metrics.End()

	// Verify through GetSummary
	summary := metrics.GetSummary()
	require.Contains(t, summary, "Total Requests: 3")
	require.Contains(t, summary, "Successful: 3")
	require.Contains(t, summary, "Errors: 0")
	require.Contains(t, summary, "Success Rate: 100.00%")
}

// TestRecordError tests error recording.
func TestRecordError(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	err1 := errors.New("connection timeout")
	err2 := errors.New("invalid response")

	metrics.RecordError(err1)
	metrics.RecordError(err2)

	metrics.End()

	summary := metrics.GetSummary()
	require.Contains(t, summary, "Total Requests: 2")
	require.Contains(t, summary, "Successful: 0")
	require.Contains(t, summary, "Errors: 2")
	require.Contains(t, summary, "Success Rate: 0.00%")
	require.Contains(t, summary, "connection timeout")
	require.Contains(t, summary, "invalid response")
}

// TestRecordMixed tests mixed success and error recording.
func TestRecordMixed(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	metrics.RecordSuccess(10.0)
	metrics.RecordError(errors.New("timeout"))
	metrics.RecordSuccess(20.0)
	metrics.RecordSuccess(30.0)
	metrics.RecordError(errors.New("timeout"))

	metrics.End()

	summary := metrics.GetSummary()
	require.Contains(t, summary, "Total Requests: 5")
	require.Contains(t, summary, "Successful: 3")
	require.Contains(t, summary, "Errors: 2")
	require.Contains(t, summary, "Success Rate: 60.00%")
}

// TestStartEnd tests start/end timing.
func TestStartEnd(t *testing.T) {
	metrics := NewRelayMetrics()

	startTime := time.Now()
	metrics.Start()

	// Add at least one request so summary includes duration
	metrics.RecordSuccess(10.0)

	time.Sleep(10 * time.Millisecond)
	metrics.End()

	// Verify through GetSummary - duration should be > 0
	summary := metrics.GetSummary()
	require.Contains(t, summary, "Duration:")
	require.True(t, metrics.startTime.After(startTime) || metrics.startTime.Equal(startTime), "Start time should be set")
	require.True(t, metrics.endTime.After(metrics.startTime), "End time should be after start time")
}

// TestGetSummary tests summary generation with no data.
func TestGetSummary_Empty(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()
	metrics.End()

	summary := metrics.GetSummary()

	require.NotEmpty(t, summary, "Summary should not be empty")
	require.Contains(t, summary, "No requests recorded")
}

// TestGetSummary tests summary generation with success data.
func TestGetSummary_WithSuccesses(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	// Add some successful requests
	for i := 0; i < 100; i++ {
		metrics.RecordSuccess(float64(i + 1))
	}

	metrics.End()
	summary := metrics.GetSummary()

	require.Contains(t, summary, "Total Requests: 100")
	require.Contains(t, summary, "Successful: 100")
	require.Contains(t, summary, "Success Rate: 100.00%")
	require.Contains(t, summary, "p50:")
	require.Contains(t, summary, "p95:")
	require.Contains(t, summary, "p99:")
	require.Contains(t, summary, "min:")
	require.Contains(t, summary, "max:")
	require.Contains(t, summary, "Throughput:")
}

// TestGetSummary tests summary generation with errors.
func TestGetSummary_WithErrors(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	// Add mixed successes and errors
	for i := 0; i < 70; i++ {
		metrics.RecordSuccess(10.0)
	}
	for i := 0; i < 30; i++ {
		metrics.RecordError(errors.New("test error"))
	}

	metrics.End()
	summary := metrics.GetSummary()

	require.Contains(t, summary, "Total Requests: 100")
	require.Contains(t, summary, "Successful: 70")
	require.Contains(t, summary, "Errors: 30")
	require.Contains(t, summary, "Success Rate: 70.00%")
	require.Contains(t, summary, "Error Breakdown:")
}

// TestPercentile tests percentile calculation.
func TestPercentile(t *testing.T) {
	// Test with known data
	latencies := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	p50 := percentile(latencies, 50)
	require.InDelta(t, 5.5, p50, 0.5, "p50 should be around 5.5")

	p95 := percentile(latencies, 95)
	require.InDelta(t, 9.5, p95, 0.5, "p95 should be around 9.5")

	p99 := percentile(latencies, 99)
	require.InDelta(t, 9.9, p99, 0.5, "p99 should be around 9.9")
}

// TestPercentile_Empty tests percentile with empty data.
func TestPercentile_Empty(t *testing.T) {
	latencies := []float64{}
	p50 := percentile(latencies, 50)
	require.Equal(t, 0.0, p50, "Empty data should return 0")
}

// TestPercentile_SingleValue tests percentile with single value.
func TestPercentile_SingleValue(t *testing.T) {
	latencies := []float64{42.5}
	p50 := percentile(latencies, 50)
	require.Equal(t, 42.5, p50, "Single value should return that value")
}

// TestConcurrentRecording tests thread-safe recording.
func TestConcurrentRecording(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	// Spawn 100 goroutines recording concurrently
	done := make(chan bool)
	for i := 0; i < 100; i++ {
		go func(val float64) {
			metrics.RecordSuccess(val)
			done <- true
		}(float64(i))
	}

	// Wait for all to complete
	for i := 0; i < 100; i++ {
		<-done
	}

	metrics.End()

	summary := metrics.GetSummary()
	require.Contains(t, summary, "Total Requests: 100")
	require.Contains(t, summary, "Successful: 100")
}

// TestErrorCounting tests error counting with duplicates.
func TestErrorCounting(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	// Record same error type multiple times
	for i := 0; i < 5; i++ {
		metrics.RecordError(errors.New("timeout"))
	}
	for i := 0; i < 3; i++ {
		metrics.RecordError(errors.New("invalid response"))
	}

	metrics.End()

	summary := metrics.GetSummary()
	require.Contains(t, summary, "Errors: 8")

	// Verify summary includes error breakdown
	require.Contains(t, summary, "timeout")
	require.Contains(t, summary, "invalid response")
	require.Contains(t, summary, "Error Breakdown:")
}

// TestThroughputCalculation tests that throughput is calculated correctly.
func TestThroughputCalculation(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	// Add 100 requests
	for i := 0; i < 100; i++ {
		metrics.RecordSuccess(float64(i))
	}

	time.Sleep(100 * time.Millisecond)
	metrics.End()

	summary := metrics.GetSummary()
	require.Contains(t, summary, "Throughput:")
	require.Contains(t, summary, "RPS")

	// Extract throughput value (should be > 0)
	lines := strings.Split(summary, "\n")
	var throughputLine string
	for _, line := range lines {
		if strings.Contains(line, "Throughput:") {
			throughputLine = line
			break
		}
	}
	require.NotEmpty(t, throughputLine, "Should have throughput line")
}

// TestLatencyPercentiles tests latency percentile formatting.
func TestLatencyPercentiles(t *testing.T) {
	metrics := NewRelayMetrics()
	metrics.Start()

	// Add latencies with known distribution
	latencies := []float64{1, 5, 10, 15, 20, 25, 30, 40, 50, 100}
	for _, lat := range latencies {
		metrics.RecordSuccess(lat)
	}

	metrics.End()

	summary := metrics.GetSummary()
	require.Contains(t, summary, "Latency Percentiles (ms):")
	require.Contains(t, summary, "min:")
	require.Contains(t, summary, "p50:")
	require.Contains(t, summary, "p95:")
	require.Contains(t, summary, "p99:")
	require.Contains(t, summary, "max:")
}
