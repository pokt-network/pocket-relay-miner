package pool

import "time"

// Pool holds an immutable set of backend endpoints and a selector strategy.
// Once created, the endpoint list cannot be modified. On config reload,
// a new Pool is built and swapped atomically.
type Pool struct {
	endpoints     []*BackendEndpoint // Immutable after creation
	selector      Selector           // Strategy for selecting next endpoint
	name          string             // Pool identifier for logging (e.g., "serviceID:rpcType")
	strategyLabel string             // Human-readable strategy label (e.g., "round_robin(auto)")
}

// NewPool creates a new Pool with the given endpoints and selection strategy.
// The endpoints slice is copied internally to prevent external mutation.
func NewPool(name string, endpoints []*BackendEndpoint, selector Selector, strategyLabel string) *Pool {
	// Copy the slice to guarantee immutability
	eps := make([]*BackendEndpoint, len(endpoints))
	copy(eps, endpoints)

	return &Pool{
		endpoints:     eps,
		selector:      selector,
		name:          name,
		strategyLabel: strategyLabel,
	}
}

// Next returns the next healthy endpoint as determined by the selector.
// Returns nil if no healthy endpoint is available.
func (p *Pool) Next() *BackendEndpoint {
	idx := p.selector.Select(p.endpoints)
	if idx < 0 || idx >= len(p.endpoints) {
		return nil
	}
	return p.endpoints[idx]
}

// All returns a copy of all endpoints in this pool (for health status API / logging).
func (p *Pool) All() []*BackendEndpoint {
	eps := make([]*BackendEndpoint, len(p.endpoints))
	copy(eps, p.endpoints)
	return eps
}

// Len returns the total number of endpoints in this pool.
func (p *Pool) Len() int {
	return len(p.endpoints)
}

// PoolName returns the pool's identifier (used in logging and metrics).
func (p *Pool) PoolName() string {
	return p.name
}

// StrategyLabel returns the human-readable strategy label for this pool.
// Includes auto/explicit annotation (e.g., "round_robin(auto)", "first_healthy(explicit)").
func (p *Pool) StrategyLabel() string {
	return p.strategyLabel
}

// RecordResult records a backend response for circuit breaker evaluation.
// It classifies the result as a failure or success, updates atomic counters,
// and returns a TransitionEvent if the endpoint's health state changed.
//
// Returns non-nil only when a state transition occurs (healthy->unhealthy or
// unhealthy->healthy). Uses CompareAndSwap to ensure exactly one goroutine
// detects and reports each transition under concurrent load.
//
// If threshold <= 0, DefaultUnhealthyThreshold (5) is used.
func (p *Pool) RecordResult(ep *BackendEndpoint, statusCode int, err error, threshold int32) *TransitionEvent {
	if threshold <= 0 {
		threshold = DefaultUnhealthyThreshold
	}

	if isFailure(statusCode, err) {
		failures := ep.IncrementFailures()
		if failures >= threshold {
			// Use CompareAndSwap so exactly one goroutine detects the transition.
			// If another goroutine already flipped healthy->false, CAS returns false
			// and we return nil (no duplicate transition event).
			if ep.healthy.CompareAndSwap(true, false) {
				ep.unhealthySinceNano.Store(time.Now().UnixNano())
				return &TransitionEvent{
					Endpoint:   ep,
					OldHealthy: true,
					NewHealthy: false,
					Failures:   failures,
					StatusCode: statusCode,
				}
			}
		}
		return nil
	}

	// Success path: reset failure counter and potentially recover.
	if ep.ConsecutiveFailures() > 0 {
		ep.ResetFailures()
	}
	// Recovery: if endpoint was unhealthy, CAS false->true.
	// Only the goroutine that flips the state returns the event.
	if ep.healthy.CompareAndSwap(false, true) {
		ep.unhealthySinceNano.Store(0)
		return &TransitionEvent{
			Endpoint:   ep,
			OldHealthy: false,
			NewHealthy: true,
			Failures:   0,
			StatusCode: statusCode,
		}
	}

	return nil
}

// HasHealthy returns true if at least one endpoint in the pool is healthy.
// Uses a linear scan with early return — no allocation.
// For typical pool sizes (2-5 endpoints) this is <50ns.
func (p *Pool) HasHealthy() bool {
	for _, ep := range p.endpoints {
		if ep.IsHealthy() {
			return true
		}
	}
	return false
}

// NextExcluding returns the first healthy endpoint that is not the excluded endpoint.
// Returns nil if no alternative healthy endpoint exists.
// This guarantees retry goes to a different backend regardless of selector strategy.
func (p *Pool) NextExcluding(exclude *BackendEndpoint) *BackendEndpoint {
	for _, ep := range p.endpoints {
		if ep == exclude {
			continue
		}
		if ep.IsHealthy() {
			return ep
		}
	}
	return nil
}

// Healthy returns only the currently healthy endpoints (for logging/debug).
func (p *Pool) Healthy() []*BackendEndpoint {
	var healthy []*BackendEndpoint
	for _, ep := range p.endpoints {
		if ep.IsHealthy() {
			healthy = append(healthy, ep)
		}
	}
	return healthy
}
