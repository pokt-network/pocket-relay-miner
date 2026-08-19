package pool

import (
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// BackendEndpoint represents a single backend in a pool.
// Health state fields use atomics for lock-free concurrent access from
// request handlers and health check goroutines.
type BackendEndpoint struct {
	// Name is the display name for this endpoint (operator-provided or derived from URL).
	// Used in logs and Prometheus metrics labels.
	Name string

	// URL is the parsed backend URL.
	URL *url.URL

	// RawURL is the original URL string as provided in configuration.
	RawURL string

	// Health state (atomic for lock-free concurrent access)
	healthy             atomic.Bool
	consecutiveFailures atomic.Int32

	// Recovery timeout: auto-recover unhealthy endpoints after this duration.
	// Prevents circuit breaker death spiral when no active health checks are configured.
	// Zero means no auto-recovery (rely on health checks or successful requests).
	recoveryTimeout    time.Duration
	unhealthySinceNano atomic.Int64

	// pendingRecovery marks that IsHealthy auto-recovered this endpoint
	// (half-open timeout) with no way to report it. RecordResult turns the
	// mark into a TransitionEvent on the first success, so "BACKEND UP" is
	// logged for auto-recoveries too, not only for traffic-driven ones.
	pendingRecovery atomic.Bool
}

// NewBackendEndpoint creates a new BackendEndpoint from a name and raw URL string.
// If name is empty, it is derived from the URL's hostname:port.
// The endpoint starts in a healthy state.
// Returns an error if the URL is empty or cannot be parsed.
func NewBackendEndpoint(name, rawURL string) (*BackendEndpoint, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("backend endpoint URL must not be empty")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid backend endpoint URL %q: %w", rawURL, err)
	}

	// Handle scheme-less URLs like "host:port" (common for gRPC backends).
	// Go's url.Parse treats "host:port" as scheme="host", opaque="port" with empty Host.
	// Re-parse with a default scheme so Host is populated correctly.
	if parsed.Host == "" && !strings.Contains(rawURL, "://") {
		parsed, err = url.Parse("http://" + rawURL)
		if err != nil {
			return nil, fmt.Errorf("invalid backend endpoint URL %q: %w", rawURL, err)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("invalid backend endpoint URL %q: missing host", rawURL)
		}
	} else if parsed.Host == "" {
		return nil, fmt.Errorf("invalid backend endpoint URL %q: missing host", rawURL)
	}

	if name == "" {
		name = parsed.Host
	}

	ep := &BackendEndpoint{
		Name:   name,
		URL:    parsed,
		RawURL: rawURL,
	}
	ep.healthy.Store(true) // All backends start healthy
	return ep, nil
}

// IsHealthy returns whether this endpoint is considered healthy.
// If the endpoint is unhealthy and a recovery timeout is configured,
// it auto-recovers after the timeout elapses (half-open circuit breaker).
// This prevents the death spiral where all backends are unhealthy and
// no traffic flows to trigger recovery via RecordResult.
func (ep *BackendEndpoint) IsHealthy() bool {
	if ep.healthy.Load() {
		return true
	}

	// Check recovery timeout (half-open state)
	if ep.recoveryTimeout > 0 {
		unhealthySince := ep.unhealthySinceNano.Load()
		if unhealthySince > 0 && time.Since(time.Unix(0, unhealthySince)) >= ep.recoveryTimeout {
			// Auto-recover: CAS ensures only one goroutine triggers recovery.
			// This runs on the selection path, which has nowhere to report a
			// transition — leave a pending mark (and keep unhealthySinceNano
			// so the downtime survives) for RecordResult to publish as a
			// TransitionEvent on the first success.
			if ep.healthy.CompareAndSwap(false, true) {
				ep.consecutiveFailures.Store(0)
				ep.pendingRecovery.Store(true)
			}
			return ep.healthy.Load()
		}
	}

	return false
}

// CurrentlyHealthy is the pure read of the health flag: no half-open
// auto-recovery, no side effects. Observers (the active health checker, the
// status API) MUST use this instead of IsHealthy — computing "was it
// unhealthy?" with IsHealthy() mutates the very state being observed and
// swallows the observer's own recovery transition.
func (ep *BackendEndpoint) CurrentlyHealthy() bool {
	return ep.healthy.Load()
}

// SetHealthy marks this endpoint as healthy and clears the unhealthy timestamp.
// Callers (the active health checker) log their own transition, so any pending
// auto-recovery report is dropped to avoid a duplicate "BACKEND UP".
func (ep *BackendEndpoint) SetHealthy() {
	ep.unhealthySinceNano.Store(0)
	ep.pendingRecovery.Store(false)
	ep.healthy.Store(true)
}

// SetUnhealthy marks this endpoint as unhealthy and records the timestamp
// for recovery timeout tracking. A pending (unreported) auto-recovery dies
// with the new outage — the operator sees the DOWN, not a stale UP.
func (ep *BackendEndpoint) SetUnhealthy() {
	ep.unhealthySinceNano.Store(time.Now().UnixNano())
	ep.pendingRecovery.Store(false)
	ep.healthy.Store(false)
}

// IncrementFailures atomically increments the consecutive failure count and returns the new value.
func (ep *BackendEndpoint) IncrementFailures() int32 {
	return ep.consecutiveFailures.Add(1)
}

// ResetFailures resets the consecutive failure count to zero.
func (ep *BackendEndpoint) ResetFailures() {
	ep.consecutiveFailures.Store(0)
}

// ConsecutiveFailures returns the current consecutive failure count.
func (ep *BackendEndpoint) ConsecutiveFailures() int32 {
	return ep.consecutiveFailures.Load()
}

// SetRecoveryTimeout configures the auto-recovery timeout for this endpoint.
// When an endpoint is unhealthy for longer than this duration, IsHealthy()
// auto-recovers it (half-open circuit breaker). Zero disables auto-recovery.
func (ep *BackendEndpoint) SetRecoveryTimeout(d time.Duration) {
	ep.recoveryTimeout = d
}

// RecoveryTimeout returns the configured recovery timeout.
func (ep *BackendEndpoint) RecoveryTimeout() time.Duration {
	return ep.recoveryTimeout
}
