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

// isFailure returns true if the result should increment the circuit breaker's
// consecutive-failure counter. The breaker trips on symptoms that indicate
// something genuinely wrong with the *backend*: HTTP 5xx, or hard transport
// errors (connection refused, DNS failure, reset by peer). Those mean the
// backend is down, misconfigured, or broken and traffic must fail over.
//
// What is NOT a failure:
//   - context.Canceled: the caller cut the request short (client timeout,
//     PATH disconnect). That's the client's budget, not the backend's fault.
//   - context.DeadlineExceeded / net.Error with Timeout()=true: slow but
//     possibly healthy backend; tripping on timeouts blackholes traffic.
//   - HTTP 1xx-4xx: application-level responses from a working backend.
//   - nil error with status 0: no result yet.
//
// The active health checker is the second layer of defence — it probes
// backends independently and can mark them unhealthy through a different
// path. The breaker here reacts to real traffic.
func isFailure(statusCode int, err error) bool {
	if err != nil {
		// Caller went away — unknown backend state, must not trip.
		if errors.Is(err, context.Canceled) {
			return false
		}
		// Slow but possibly healthy — must not trip.
		if isTimeoutError(err) {
			return false
		}
		// Hard transport errors: connection refused, DNS, reset, TLS.
		return true
	}
	return statusCode >= 500 && statusCode < 600
}

// IsRetryable returns true if the request should be retried on an alternate
// backend. Shares the same classification as isFailure: hard transport errors
// and 5xx are worth failing over to a healthy peer; timeouts and client
// cancellations are not (budget gone / caller disappeared).
func IsRetryable(statusCode int, err error) bool {
	return isFailure(statusCode, err)
}

// ClassifyFailure returns a short, log-friendly reason describing why a
// response counted as a failure. Empty string when the result was not a
// failure. Used by the circuit breaker transition logger so operators can
// see *what* tripped the breaker without parsing raw errors.
func ClassifyFailure(statusCode int, err error) string {
	if !isFailure(statusCode, err) {
		return ""
	}
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) {
			return "transport_error"
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return "dns_error"
		}
		return "backend_error"
	}
	return "backend_5xx"
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
