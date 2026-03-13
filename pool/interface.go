package pool

// Selector picks the next endpoint from a list of backend endpoints.
// The implementation determines the selection strategy (e.g., first-healthy, round-robin).
//
// Phase 1 provides FirstHealthySelector. Phase 2 adds RoundRobinSelector.
// Phase 10 may add additional strategies (weighted, least-connections).
type Selector interface {
	// Select returns the index of the next endpoint to use from the provided list.
	// The selector should skip unhealthy endpoints based on its strategy.
	// Returns -1 if no suitable endpoint is available.
	Select(endpoints []*BackendEndpoint) int
}

// FirstHealthySelector returns the first healthy endpoint in the list.
// This is the default selector used when no load balancing strategy is configured.
type FirstHealthySelector struct{}

// Select returns the index of the first healthy endpoint, or -1 if none are healthy.
func (s *FirstHealthySelector) Select(endpoints []*BackendEndpoint) int {
	for i, ep := range endpoints {
		if ep.IsHealthy() {
			return i
		}
	}
	return -1
}
