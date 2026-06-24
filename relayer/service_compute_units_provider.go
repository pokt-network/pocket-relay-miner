package relayer

import (
	"context"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// serviceComputeUnitsLookupTimeout bounds the cache lookup on the relay hot
// path. L1 (xsync) hits return in well under a microsecond; this timeout only
// applies on a cold L1/L2 that must fall through to an L3 chain query.
const serviceComputeUnitsLookupTimeout = 5 * time.Second

// serviceCacheComputeUnitsProvider adapts the orchestrator-refreshed ServiceCache
// (L1 -> L2 -> L3 with pub/sub invalidation) into a ServiceComputeUnitsProvider.
//
// The value it returns becomes MinedRelayMessage.ComputeUnitsPerRelay, which the
// miner uses verbatim as the SMST leaf weight (the claimed compute units). Reading
// it from the SAME refreshed cache the RelayMeter already consumes means an
// on-chain compute_units_per_relay change is picked up within the orchestrator's
// refresh/invalidation window — instead of being frozen for the process lifetime,
// which is what the previous sync.Map-backed provider did.
type serviceCacheComputeUnitsProvider struct {
	logger logging.Logger
	cache  ServiceCache
}

// NewServiceCacheComputeUnitsProvider builds a compute-units provider backed by
// the refreshed service cache.
func NewServiceCacheComputeUnitsProvider(logger logging.Logger, cache ServiceCache) ServiceComputeUnitsProvider {
	return &serviceCacheComputeUnitsProvider{
		logger: logging.ForComponent(logger, logging.ComponentRelayProcessor),
		cache:  cache,
	}
}

// GetServiceComputeUnits returns the compute units per relay for a service,
// reading the refreshed service cache. It floors to 1 on any error or a zero
// value so cost/claim math never divides by or multiplies against zero.
func (p *serviceCacheComputeUnitsProvider) GetServiceComputeUnits(serviceID string) uint64 {
	if p.cache == nil {
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), serviceComputeUnitsLookupTimeout)
	defer cancel()

	service, err := p.cache.Get(ctx, serviceID)
	if err != nil {
		// Hot path: keep this at Debug. A miss falls back to 1 CU; miners keep
		// the cache warm, so this should be rare in steady state.
		p.logger.Debug().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Msg("service compute units cache miss, using default of 1")
		return 1
	}

	computeUnits := service.GetComputeUnitsPerRelay()
	if computeUnits == 0 {
		return 1
	}

	return computeUnits
}
