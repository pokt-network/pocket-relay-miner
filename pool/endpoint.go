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
	lastCheckUnixNano   atomic.Int64
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

	if parsed.Host == "" {
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
func (ep *BackendEndpoint) IsHealthy() bool {
	return ep.healthy.Load()
}

// SetHealthy marks this endpoint as healthy.
func (ep *BackendEndpoint) SetHealthy() {
	ep.healthy.Store(true)
}

// SetUnhealthy marks this endpoint as unhealthy.
func (ep *BackendEndpoint) SetUnhealthy() {
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

// SetLastCheck records the time of the last health check.
func (ep *BackendEndpoint) SetLastCheck(t time.Time) {
	ep.lastCheckUnixNano.Store(t.UnixNano())
}

// LastCheck returns the time of the last health check.
func (ep *BackendEndpoint) LastCheck() time.Time {
	ns := ep.lastCheckUnixNano.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}
