package relayer

import (
	"context"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// serviceComputeUnitsLookupTimeout bounds the CUPR lookup on the relay hot path.
// L1 (xsync) hits return in well under a microsecond; this timeout only applies
// on a cold L1 that must fall through to a chain query.
const serviceComputeUnitsLookupTimeout = 5 * time.Second

// ServiceCUPRAtHeightQueryClient resolves the compute_units_per_relay that was
// effective for a service at a block height. Satisfied by the query layer's
// service client, which caches per (serviceID, height) — immutable data, so a
// session's relays all hit L1 after the first lookup.
type ServiceCUPRAtHeightQueryClient interface {
	GetServiceComputeUnitsPerRelayAtHeight(ctx context.Context, serviceID string, blockHeight int64) (uint64, error)
}

// serviceCacheComputeUnitsProvider resolves a service's compute_units_per_relay
// at a session's START height.
//
// The value it returns becomes MinedRelayMessage.ComputeUnitsPerRelay, which the
// miner uses verbatim as the SMST leaf weight. From poktroll v0.1.35 the chain
// resolves CUPR at session start in x/proof claim validation and x/tokenomics
// settlement, so pinning to that height is what keeps
// smstSum == numRelays * cupr across a mid-session CUPR change. Reading the LIVE
// value — which this provider used to do via the refreshed service cache —
// produces a mixed-weight tree and forfeits the session with
// ErrProofComputeUnitsMismatch.
//
// The orchestrator-refreshed ServiceCache is retained ONLY for relays that carry
// no usable session start height; there is no height to pin to, so the live value
// is the sole option and behaviour is unchanged for that path.
type serviceCacheComputeUnitsProvider struct {
	logger      logging.Logger
	cache       ServiceCache
	queryClient ServiceCUPRAtHeightQueryClient
}

// NewServiceCacheComputeUnitsProvider builds a compute-units provider that pins
// CUPR to session start via queryClient, falling back to the refreshed service
// cache when a relay carries no session start height.
//
// Either dependency may be nil; the provider floors to 1 CU rather than failing
// a relay.
func NewServiceCacheComputeUnitsProvider(
	logger logging.Logger,
	cache ServiceCache,
	queryClient ServiceCUPRAtHeightQueryClient,
) ServiceComputeUnitsProvider {
	return &serviceCacheComputeUnitsProvider{
		logger:      logging.ForComponent(logger, logging.ComponentRelayProcessor),
		cache:       cache,
		queryClient: queryClient,
	}
}

// GetServiceComputeUnits returns the compute units per relay effective at
// sessionStartHeight. It floors to 1 on any error or a zero value so cost/claim
// math never divides by or multiplies against zero.
func (p *serviceCacheComputeUnitsProvider) GetServiceComputeUnits(
	ctx context.Context,
	serviceID string,
	sessionStartHeight int64,
) uint64 {
	// A non-positive height means the relay carried no session header, so there is
	// no session to pin to. Querying at height 0 would be meaningless; serve the
	// live value, which is what this path did before v0.1.35.
	if sessionStartHeight <= 0 || p.queryClient == nil {
		return p.liveComputeUnits(ctx, serviceID)
	}

	queryCtx, cancel := context.WithTimeout(ctx, serviceComputeUnitsLookupTimeout)
	defer cancel()

	computeUnits, err := p.queryClient.GetServiceComputeUnitsPerRelayAtHeight(queryCtx, serviceID, sessionStartHeight)
	if err != nil {
		// Hot path: keep this at Debug. Fall back to the refreshed cache rather than
		// dropping the relay — a wrong weight costs one session, a dropped relay is
		// certain loss. The query layer already degrades a pre-v0.1.35 node to the
		// live value internally, so reaching here means a real query failure.
		p.logger.Debug().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Int64("session_start_height", sessionStartHeight).
			Msg("compute units at session start height unavailable, falling back to live value")
		return p.liveComputeUnits(ctx, serviceID)
	}

	if computeUnits == 0 {
		return 1
	}

	return computeUnits
}

// liveComputeUnits reads the service's current compute_units_per_relay from the
// orchestrator-refreshed cache (L1 -> L2 -> L3 with pub/sub invalidation).
func (p *serviceCacheComputeUnitsProvider) liveComputeUnits(ctx context.Context, serviceID string) uint64 {
	if p.cache == nil {
		return 1
	}

	lookupCtx, cancel := context.WithTimeout(ctx, serviceComputeUnitsLookupTimeout)
	defer cancel()

	service, err := p.cache.Get(lookupCtx, serviceID)
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
