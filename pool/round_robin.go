package pool

import "sync/atomic"

// RoundRobinSelector distributes requests across healthy endpoints using
// an atomic counter. When the selected endpoint is unhealthy, it scans
// forward (wrapping around) to find the next healthy one.
// The counter always advances, ensuring even distribution over time.
type RoundRobinSelector struct {
	counter atomic.Uint64
}

// Select returns the index of the next healthy endpoint using round-robin.
// Returns -1 if no healthy endpoint is available or endpoints is empty/nil.
func (s *RoundRobinSelector) Select(endpoints []*BackendEndpoint) int {
	n := len(endpoints)
	if n == 0 {
		return -1
	}

	// Always advance the counter (even if endpoint is unhealthy)
	idx := int(s.counter.Add(1) % uint64(n))

	// Fast path: selected endpoint is healthy
	if endpoints[idx].IsHealthy() {
		return idx
	}

	// Slow path: scan forward for next healthy endpoint
	for i := 1; i < n; i++ {
		candidate := (idx + i) % n
		if endpoints[candidate].IsHealthy() {
			return candidate
		}
	}

	return -1
}
