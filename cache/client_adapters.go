package cache

import (
	"context"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"

	"github.com/pokt-network/poktroll/pkg/client"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
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

// sessionQueryClientAdapter adapts client.SessionQueryClient to SessionQueryClient.
// Note: client.SessionQueryClient likely already returns *sessiontypes.Params,
// but we create an adapter for consistency.
type sessionQueryClientAdapter struct {
	client client.SessionQueryClient
}

func (a *sessionQueryClientAdapter) GetParams(ctx context.Context) (*sessiontypes.Params, error) {
	return a.client.GetParams(ctx)
}

// NewSessionQueryClientAdapter creates an adapter for client.SessionQueryClient.
func NewSessionQueryClientAdapter(c client.SessionQueryClient) SessionQueryClient {
	return &sessionQueryClientAdapter{client: c}
}

// cachedApplicationQueryClient wraps KeyedEntityCache to implement client.ApplicationQueryClient.
// This allows the ring client to use the Redis-backed application cache.
type cachedApplicationQueryClient struct {
	cache KeyedEntityCache[string, *apptypes.Application]
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
	// Not implemented - ring client doesn't need params
	return nil, nil
}

// NewCachedApplicationQueryClient wraps an application cache to implement client.ApplicationQueryClient.
// This allows components that expect client.ApplicationQueryClient (like RingClient)
// to use the Redis-backed L1→L2→L3 cache instead of direct blockchain queries.
func NewCachedApplicationQueryClient(cache KeyedEntityCache[string, *apptypes.Application]) client.ApplicationQueryClient {
	return &cachedApplicationQueryClient{cache: cache}
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
