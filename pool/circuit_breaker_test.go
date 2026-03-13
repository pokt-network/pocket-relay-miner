//go:build test

package pool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- Failure Classification Tests ---

func TestIsFailure(t *testing.T) {
	t.Run("5xx status codes are failures", func(t *testing.T) {
		for _, code := range []int{500, 502, 503, 504, 599} {
			require.True(t, isFailure(code, nil), "status %d should be a failure", code)
		}
	})

	t.Run("non-5xx status codes are not failures", func(t *testing.T) {
		for _, code := range []int{200, 201, 400, 404, 429, 499} {
			require.False(t, isFailure(code, nil), "status %d should not be a failure", code)
		}
	})

	t.Run("connection refused error is failure", func(t *testing.T) {
		connErr := &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: errors.New("connection refused"),
		}
		require.True(t, isFailure(0, connErr))
	})

	t.Run("DNS resolution error is failure", func(t *testing.T) {
		dnsErr := &net.DNSError{
			Err:  "no such host",
			Name: "unknown-host",
		}
		require.True(t, isFailure(0, dnsErr))
	})

	t.Run("context.DeadlineExceeded is NOT a failure", func(t *testing.T) {
		require.False(t, isFailure(0, context.DeadlineExceeded))
	})

	t.Run("wrapped context.DeadlineExceeded is NOT a failure", func(t *testing.T) {
		wrappedErr := fmt.Errorf("backend call: %w", context.DeadlineExceeded)
		require.False(t, isFailure(0, wrappedErr))
	})

	t.Run("net.Error with Timeout()=true is NOT a failure", func(t *testing.T) {
		timeoutErr := &mockTimeoutError{timeout: true}
		require.False(t, isFailure(0, timeoutErr))
	})

	t.Run("nil error with status 0 is NOT a failure", func(t *testing.T) {
		require.False(t, isFailure(0, nil))
	})
}

// --- RecordResult Tests ---

func TestRecordResult_ThresholdTriggersUnhealthy(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// 4 failures should NOT trigger transition
	for i := 0; i < 4; i++ {
		event := p.RecordResult(ep, 500, nil, 5)
		require.Nil(t, event, "failure %d should not trigger transition", i+1)
	}
	require.True(t, ep.IsHealthy(), "should still be healthy after 4 failures")

	// 5th failure triggers transition
	event := p.RecordResult(ep, 500, nil, 5)
	require.NotNil(t, event)
	require.True(t, event.OldHealthy)
	require.False(t, event.NewHealthy)
	require.Equal(t, int32(5), event.Failures)
	require.Equal(t, 500, event.StatusCode)
	require.Equal(t, ep, event.Endpoint)
	require.False(t, ep.IsHealthy())
}

func TestRecordResult_BelowThreshold(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// 4 failures with threshold of 5: no transition
	for i := 0; i < 4; i++ {
		event := p.RecordResult(ep, 500, nil, 5)
		require.Nil(t, event)
	}
	require.True(t, ep.IsHealthy())
	require.Equal(t, int32(4), ep.ConsecutiveFailures())
}

func TestRecordResult_CustomThreshold(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// Custom threshold of 3
	for i := 0; i < 2; i++ {
		event := p.RecordResult(ep, 503, nil, 3)
		require.Nil(t, event)
	}
	require.True(t, ep.IsHealthy())

	event := p.RecordResult(ep, 503, nil, 3)
	require.NotNil(t, event)
	require.False(t, event.NewHealthy)
	require.False(t, ep.IsHealthy())
}

func TestRecordResult_ThresholdZeroUsesDefault(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// Threshold 0 should use DefaultUnhealthyThreshold (5)
	for i := 0; i < 4; i++ {
		event := p.RecordResult(ep, 500, nil, 0)
		require.Nil(t, event)
	}
	require.True(t, ep.IsHealthy(), "should still be healthy after 4 failures with threshold=0 (default 5)")

	event := p.RecordResult(ep, 500, nil, 0)
	require.NotNil(t, event, "5th failure should trigger transition with default threshold")
	require.False(t, ep.IsHealthy())
}

func TestRecordResult_SuccessResetsCounter(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// Accumulate 4 failures
	for i := 0; i < 4; i++ {
		p.RecordResult(ep, 500, nil, 5)
	}
	require.Equal(t, int32(4), ep.ConsecutiveFailures())

	// Single success resets counter, no transition (still healthy)
	event := p.RecordResult(ep, 200, nil, 5)
	require.Nil(t, event)
	require.Equal(t, int32(0), ep.ConsecutiveFailures())
	require.True(t, ep.IsHealthy())
}

func TestRecordResult_SuccessRecovery(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// Mark unhealthy via failures
	for i := 0; i < 5; i++ {
		p.RecordResult(ep, 500, nil, 5)
	}
	require.False(t, ep.IsHealthy())

	// Success on unhealthy endpoint triggers recovery transition
	event := p.RecordResult(ep, 200, nil, 5)
	require.NotNil(t, event)
	require.False(t, event.OldHealthy)
	require.True(t, event.NewHealthy)
	require.True(t, ep.IsHealthy())
	require.Equal(t, int32(0), ep.ConsecutiveFailures())
}

func TestRecordResult_5xxVariants(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		t.Run(fmt.Sprintf("HTTP_%d_counts_as_failure", code), func(t *testing.T) {
			ep, _ := NewBackendEndpoint("test", "http://node:8545")
			p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

			event := p.RecordResult(ep, code, nil, 1)
			require.NotNil(t, event, "status %d should count as failure", code)
			require.False(t, ep.IsHealthy())
		})
	}
}

func TestRecordResult_NonFailureStatusCodes(t *testing.T) {
	for _, code := range []int{200, 400, 404} {
		t.Run(fmt.Sprintf("HTTP_%d_not_failure", code), func(t *testing.T) {
			ep, _ := NewBackendEndpoint("test", "http://node:8545")
			p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

			// Pre-set some failures
			ep.IncrementFailures()
			ep.IncrementFailures()

			event := p.RecordResult(ep, code, nil, 5)
			require.Nil(t, event)
			require.Equal(t, int32(0), ep.ConsecutiveFailures(), "non-failure should reset counter")
		})
	}
}

func TestRecordResult_ConnectionRefusedError(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	connErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: errors.New("connection refused"),
	}

	// Connection error with threshold 1 should trigger
	event := p.RecordResult(ep, 0, connErr, 1)
	require.NotNil(t, event)
	require.False(t, ep.IsHealthy())
}

func TestRecordResult_TimeoutNotCounted(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// context.DeadlineExceeded should NOT count as failure
	event := p.RecordResult(ep, 0, context.DeadlineExceeded, 1)
	require.Nil(t, event)
	require.True(t, ep.IsHealthy())
	require.Equal(t, int32(0), ep.ConsecutiveFailures())
}

func TestRecordResult_NetTimeoutNotCounted(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	timeoutErr := &mockTimeoutError{timeout: true}
	event := p.RecordResult(ep, 0, timeoutErr, 1)
	require.Nil(t, event)
	require.True(t, ep.IsHealthy())
	require.Equal(t, int32(0), ep.ConsecutiveFailures())
}

func TestRecordResult_NilErrZeroStatus(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	event := p.RecordResult(ep, 0, nil, 5)
	require.Nil(t, event)
	require.Equal(t, int32(0), ep.ConsecutiveFailures())
}

func TestRecordResult_ConcurrentTransition(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// Pre-load 4 failures so next failure hits threshold
	for i := 0; i < 4; i++ {
		p.RecordResult(ep, 500, nil, 5)
	}

	// Launch 50 goroutines all recording the 5th+ failure simultaneously
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		events []*TransitionEvent
	)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			event := p.RecordResult(ep, 500, nil, 5)
			if event != nil {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Exactly ONE goroutine should have received the transition event
	require.Len(t, events, 1, "exactly one goroutine should detect the healthy->unhealthy transition")
	require.True(t, events[0].OldHealthy)
	require.False(t, events[0].NewHealthy)
	require.False(t, ep.IsHealthy())
}

func TestRecordResult_AlreadyUnhealthyNoTransition(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// Trigger unhealthy
	for i := 0; i < 5; i++ {
		p.RecordResult(ep, 500, nil, 5)
	}
	require.False(t, ep.IsHealthy())

	// More failures should return nil (no duplicate transition)
	for i := 0; i < 10; i++ {
		event := p.RecordResult(ep, 500, nil, 5)
		require.Nil(t, event, "already-unhealthy endpoint should not produce transition events")
	}
}

func TestRecordResult_AlreadyHealthySuccessNoTransition(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	// Success on already-healthy endpoint with zero failures
	event := p.RecordResult(ep, 200, nil, 5)
	require.Nil(t, event)
	require.True(t, ep.IsHealthy())
}

// --- Test Helpers ---

// mockTimeoutError implements net.Error with configurable Timeout() return.
type mockTimeoutError struct {
	timeout bool
}

func (e *mockTimeoutError) Error() string   { return "mock timeout error" }
func (e *mockTimeoutError) Timeout() bool   { return e.timeout }
func (e *mockTimeoutError) Temporary() bool { return false }

// Verify mockTimeoutError implements net.Error at compile time.
var _ net.Error = (*mockTimeoutError)(nil)
