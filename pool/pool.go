package pool

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
