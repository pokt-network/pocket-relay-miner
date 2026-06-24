package cache

import (
	"context"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"

	"github.com/pokt-network/poktroll/pkg/client"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// applicationQueryClientAdapter adapts client.ApplicationQueryClient (returns value)
// to ApplicationQueryClient interface (expects pointer).
type applicationQueryClientAdapter struct {
	client client.ApplicationQueryClient
}

func (a *applicationQueryClientAdapter) GetApplication(ctx context.Context, address string) (*apptypes.Application, error) {
	app, err := a.client.GetApplication(ctx, address)
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// InvalidateApplication delegates to the underlying client if it supports
// cache invalidation (e.g. the in-process query.ApplicationQueryClient).
// No-op for clients that do not cache, so callers can always invoke it.
func (a *applicationQueryClientAdapter) InvalidateApplication(address string) {
	if inv, ok := a.client.(interface{ InvalidateApplication(string) }); ok {
		inv.InvalidateApplication(address)
	}
}

// NewApplicationQueryClientAdapter creates an adapter for client.ApplicationQueryClient.
func NewApplicationQueryClientAdapter(c client.ApplicationQueryClient) ApplicationQueryClient {
	return &applicationQueryClientAdapter{client: c}
}

// serviceQueryClientAdapter adapts client.ServiceQueryClient (returns value)
// to ServiceQueryClient interface (expects pointer).
type serviceQueryClientAdapter struct {
	client client.ServiceQueryClient
}

func (a *serviceQueryClientAdapter) GetService(ctx context.Context, serviceID string) (*sharedtypes.Service, error) {
	service, err := a.client.GetService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	return &service, nil
}

// InvalidateService delegates to the underlying client if it supports
// invalidation, so the service cache's force-refresh fetches fresh data from
// chain instead of the query layer's frozen copy.
func (a *serviceQueryClientAdapter) InvalidateService(serviceID string) {
	if inv, ok := a.client.(interface{ InvalidateService(string) }); ok {
		inv.InvalidateService(serviceID)
	}
}

// NewServiceQueryClientAdapter creates an adapter for client.ServiceQueryClient.
func NewServiceQueryClientAdapter(c client.ServiceQueryClient) ServiceQueryClient {
	return &serviceQueryClientAdapter{client: c}
}

// proofQueryClientAdapter adapts client.ProofQueryClient (returns ProofParams interface)
// to ProofQueryClient interface (expects *prooftypes.Params).
type proofQueryClientAdapter struct {
	client client.ProofQueryClient
}

func (a *proofQueryClientAdapter) GetParams(ctx context.Context) (*prooftypes.Params, error) {
	params, err := a.client.GetParams(ctx)
	if err != nil {
		return nil, err
	}

	// ProofParams is an interface, we need to extract the concrete type
	// The actual implementation returns *prooftypes.Params
	if concreteParams, ok := params.(*prooftypes.Params); ok {
		return concreteParams, nil
	}

	// Fallback: try to get compute units per relay (ProofParams interface method)
	// and construct a new Params object
	computeUnitsPerRelay := params.GetProofRequestProbability()
	proofRequirementThreshold := params.GetProofRequirementThreshold()
	proofMissingPenalty := params.GetProofMissingPenalty()
	proofSubmissionFee := params.GetProofSubmissionFee()

	return &prooftypes.Params{
		ProofRequestProbability:   computeUnitsPerRelay,
		ProofRequirementThreshold: proofRequirementThreshold,
		ProofMissingPenalty:       proofMissingPenalty,
		ProofSubmissionFee:        proofSubmissionFee,
	}, nil
}

// NewProofQueryClientAdapter creates an adapter for client.ProofQueryClient.
func NewProofQueryClientAdapter(c client.ProofQueryClient) ProofQueryClient {
	return &proofQueryClientAdapter{client: c}
}

// cachedApplicationQueryClient wraps KeyedEntityCache to implement client.ApplicationQueryClient.
// This allows the ring client to use the Redis-backed application cache.
//
// GetApplication resolves through the entity cache (L1→L2→L3 + pub/sub invalidation)
// so per-app state (stake, delegations) follows on-chain changes. GetParams is a
// separate concern — the application MODULE params (e.g. MinStake) are not a
// per-app entity — so it delegates to an optional params-capable client. Ring
// usage leaves paramsClient nil (the ring never reads module params); the relay
// meter wires a real client so app_min_stake_upokt reflects the on-chain value
// instead of a frozen 0.
type cachedApplicationQueryClient struct {
	cache KeyedEntityCache[string, *apptypes.Application]
	// paramsClient serves GetParams. nil for callers that never read module
	// params (the ring client), so GetParams returns (nil, nil) for them.
	paramsClient client.ApplicationQueryClient
}

func (c *cachedApplicationQueryClient) GetApplication(ctx context.Context, address string) (apptypes.Application, error) {
	app, err := c.cache.Get(ctx, address)
	if err != nil {
		return apptypes.Application{}, err
	}
	return *app, nil
}

func (c *cachedApplicationQueryClient) GetAllApplications(ctx context.Context) ([]apptypes.Application, error) {
	// Not implemented - ring client only needs GetApplication
	return nil, nil
}

func (c *cachedApplicationQueryClient) GetParams(ctx context.Context) (*apptypes.Params, error) {
	// The entity cache holds per-app state, not module params. Delegate to the
	// params-capable client when one is wired (relay meter); ring usage passes
	// nil and never calls this, so preserve the prior no-op behavior for it.
	if c.paramsClient == nil {
		return nil, nil
	}
	return c.paramsClient.GetParams(ctx)
}

// NewCachedApplicationQueryClient wraps an application cache to implement client.ApplicationQueryClient.
// This allows components that expect client.ApplicationQueryClient (like RingClient)
// to use the Redis-backed L1→L2→L3 cache instead of direct blockchain queries.
//
// GetParams is a no-op (returns nil) for this variant — the ring client, the only
// caller, never reads application module params. Use
// NewCachedApplicationQueryClientWithParams when the consumer needs GetParams.
func NewCachedApplicationQueryClient(cache KeyedEntityCache[string, *apptypes.Application]) client.ApplicationQueryClient {
	return &cachedApplicationQueryClient{cache: cache}
}

// NewCachedApplicationQueryClientWithParams is like NewCachedApplicationQueryClient
// but routes GetParams to paramsClient (e.g. the query-layer application client,
// which carries its own short TTL). The relay meter needs this so the application
// module's MinStake reaches its exhaustion diagnostics instead of reading the
// frozen (nil, nil) stub. GetApplication still resolves through the entity cache.
func NewCachedApplicationQueryClientWithParams(
	cache KeyedEntityCache[string, *apptypes.Application],
	paramsClient client.ApplicationQueryClient,
) client.ApplicationQueryClient {
	return &cachedApplicationQueryClient{cache: cache, paramsClient: paramsClient}
}

// cachedAccountQueryClient wraps KeyedEntityCache to implement client.AccountQueryClient.
// This allows the ring client to use the Redis-backed account cache for public key lookups.
type cachedAccountQueryClient struct {
	cache KeyedEntityCache[string, cryptotypes.PubKey]
}

func (c *cachedAccountQueryClient) GetPubKeyFromAddress(ctx context.Context, address string) (cryptotypes.PubKey, error) {
	return c.cache.Get(ctx, address)
}

func (c *cachedAccountQueryClient) GetAccount(ctx context.Context, address string) (cosmostypes.AccountI, error) {
	// Not implemented - ring client only needs GetPubKeyFromAddress
	return nil, nil
}

// NewCachedAccountQueryClient wraps an account cache to implement client.AccountQueryClient.
// This allows components that expect client.AccountQueryClient (like RingClient)
// to use the Redis-backed L1→L2→L3 cache instead of direct blockchain queries.
func NewCachedAccountQueryClient(cache KeyedEntityCache[string, cryptotypes.PubKey]) client.AccountQueryClient {
	return &cachedAccountQueryClient{cache: cache}
}

// cachedSharedQueryClient wraps SingletonEntityCache to implement client.SharedQueryClient.
// This allows the ring client to use the Redis-backed shared params cache for GetParams(),
// while delegating all other methods to the underlying direct client.
type cachedSharedQueryClient struct {
	cache        SingletonEntityCache[*sharedtypes.Params]
	directClient client.SharedQueryClient
}

func (c *cachedSharedQueryClient) GetParams(ctx context.Context) (*sharedtypes.Params, error) {
	// Use cache for GetParams (the hot path for ring verification)
	return c.cache.Get(ctx)
}

// GetParamsAtHeight delegates to the direct client. Height-aware params are only
// needed for window-timing computations (claim/proof submission), not the ring
// verification hot path, so there is no benefit to caching them here.
func (c *cachedSharedQueryClient) GetParamsAtHeight(ctx context.Context, queryHeight int64) (*sharedtypes.Params, error) {
	return c.directClient.GetParamsAtHeight(ctx, queryHeight)
}

// Delegate all other methods to the direct client
func (c *cachedSharedQueryClient) GetSessionGracePeriodEndHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return c.directClient.GetSessionGracePeriodEndHeight(ctx, queryHeight)
}

func (c *cachedSharedQueryClient) GetClaimWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return c.directClient.GetClaimWindowOpenHeight(ctx, queryHeight)
}

func (c *cachedSharedQueryClient) GetEarliestSupplierClaimCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	return c.directClient.GetEarliestSupplierClaimCommitHeight(ctx, queryHeight, supplierOperatorAddr)
}

func (c *cachedSharedQueryClient) GetProofWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return c.directClient.GetProofWindowOpenHeight(ctx, queryHeight)
}

func (c *cachedSharedQueryClient) GetEarliestSupplierProofCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	return c.directClient.GetEarliestSupplierProofCommitHeight(ctx, queryHeight, supplierOperatorAddr)
}

// NewCachedSharedQueryClient wraps a shared params cache to implement client.SharedQueryClient.
// This allows components that expect client.SharedQueryClient (like RingClient)
// to use the Redis-backed L1→L2→L3 cache for GetParams() calls while delegating
// other methods to the direct client.
func NewCachedSharedQueryClient(cache SingletonEntityCache[*sharedtypes.Params], directClient client.SharedQueryClient) client.SharedQueryClient {
	return &cachedSharedQueryClient{
		cache:        cache,
		directClient: directClient,
	}
}
