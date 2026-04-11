package pool

import (
	"context"
	"errors"
	"net"
	"time"
)

// DefaultUnhealthyThreshold is the number of consecutive failures before
// a backend is marked unhealthy. Used when threshold is 0 (unconfigured).
const DefaultUnhealthyThreshold int32 = 5

// DefaultRecoveryTimeout is the duration after which an unhealthy endpoint
// auto-recovers if no active health checks are configured. This prevents
// the circuit breaker death spiral where all backends are unhealthy and
// fast-fail blocks all traffic, preventing recovery via RecordResult.
// Set to 30 seconds — long enough to avoid hammering a truly down backend,
// short enough to recover quickly when it comes back.
const DefaultRecoveryTimeout = 30 * time.Second

// TransitionEvent is returned by RecordResult when a backend's health state changes.
// A nil return means no state transition occurred. Callers use this to log
// transitions at the appropriate level (Warn for healthy->unhealthy, Info for recovery).
type TransitionEvent struct {
	// Endpoint that transitioned.
	Endpoint *BackendEndpoint
	// OldHealthy is the health state before the transition.
	OldHealthy bool
	// NewHealthy is the health state after the transition.
	NewHealthy bool
	// Failures is the consecutive failure count at the time of transition.
	Failures int32
	// StatusCode is the HTTP status code that triggered the transition (0 for connection errors).
	StatusCode int
	// Error is the error that triggered the transition (nil for HTTP status-only failures).
	Error error
	// DowntimeDuration is how long the backend was unhealthy (only set on recovery transitions).
	DowntimeDuration time.Duration
}

// isFailure returns true if the result represents a circuit-breaker-countable failure.
//
// Failures are:
//   - HTTP 5xx status codes (500-599)
//   - Non-timeout errors (connection refused, DNS resolution, connection reset, etc.)
//
// NOT failures:
//   - HTTP 1xx-4xx status codes
//   - Timeouts (context.DeadlineExceeded, net.Error with Timeout()=true)
//   - nil error with status 0 (edge case: no result yet)
func isFailure(statusCode int, err error) bool {
	if err != nil {
		// Timeouts are explicitly NOT failures per design decision.
		// Slow but healthy backends should not be circuit-broken.
		if isTimeoutError(err) {
			return false
		}
		// All other errors: connection refused, DNS, reset, etc.
		return true
	}
	// HTTP 5xx status codes
	return statusCode >= 500
}

// IsRetryable returns true if the given status code and error indicate that
// the request should be retried on an alternate backend.
//
// Retryable: connection errors (refused, DNS, reset) and HTTP 5xx.
// NOT retryable: timeouts (shared budget exhausted), HTTP 1xx-4xx, nil error with 2xx.
//
// This is an exported wrapper around isFailure for clarity of intent
// (retry decision vs circuit breaker counting use the same classification).
func IsRetryable(statusCode int, err error) bool {
	return isFailure(statusCode, err)
}

// isTimeoutError returns true if the error represents a timeout.
// Checks for context.DeadlineExceeded and net.Error with Timeout()=true.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}
