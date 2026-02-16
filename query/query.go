package query

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"

	cometrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	accounttypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
	"github.com/puzpuzpuz/xsync/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// queryCodec is a codec used to unmarshal the account interface returned by the
// account querier into the concrete account interface implementation.
var queryCodec *codec.ProtoCodec

func init() {
	reg := codectypes.NewInterfaceRegistry()
	accounttypes.RegisterInterfaces(reg)
	cryptocodec.RegisterInterfaces(reg)
	queryCodec = codec.NewProtoCodec(reg)
}

const (
	// defaultQueryTimeout is the default timeout for chain queries.
	defaultQueryTimeout = 5 * time.Second
)

// ClientConfig contains configuration for query clients.
type ClientConfig struct {
	// GRPCEndpoint is the gRPC endpoint for the full node.
	// Example: "localhost:9090"
	GRPCEndpoint string

	// QueryTimeout is the timeout for chain queries.
	// Default: 5 seconds
	QueryTimeout time.Duration

	// UseTLS enables TLS for the gRPC connection.
	// Set to true when connecting to endpoints on port 443 or with TLS enabled.
	// Default: false (insecure connection)
	UseTLS bool
}

// Clients provide access to all on-chain query clients.
type Clients struct {
	logger logging.Logger
	config ClientConfig

	// gRPC connection
	grpcConn *grpc.ClientConn

	// Individual query clients
	sharedClient      *sharedQueryClient
	sessionClient     *sessionQueryClient
	applicationClient *applicationQueryClient
	supplierClient    *supplierQueryClient
	proofClient       *proofQueryClient
	serviceClient     *serviceQueryClient
	accountClient     *accountQueryClient
	bankClient        *bankQueryClient

	// Lifecycle
	mu     sync.RWMutex
	closed bool
}

// NewQueryClients creates a new Clients instance.
func NewQueryClients(
	logger logging.Logger,
	config ClientConfig,
) (*Clients, error) {
	if config.GRPCEndpoint == "" {
		return nil, fmt.Errorf("gRPC endpoint is required")
	}
	if config.QueryTimeout == 0 {
		config.QueryTimeout = defaultQueryTimeout
	}

	// Establish gRPC connection with appropriate credentials
	var transportCreds credentials.TransportCredentials
	if config.UseTLS {
		transportCreds = credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
		})
	} else {
		transportCreds = insecure.NewCredentials()
	}

	// Production-optimized gRPC connection for high-volume queries
	grpcConn, err := grpc.NewClient(
		config.GRPCEndpoint,
		grpc.WithTransportCredentials(transportCreds),

		// Keepalive: Prevent connection timeouts and detect broken connections
		// Note: Servers enforce minimum ping intervals (often 5 minutes).
		// Pinging too frequently triggers ENHANCE_YOUR_CALM / GoAway.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                60 * time.Second, // Send keepalive ping every 60s if no activity
			Timeout:             10 * time.Second, // Wait 10s for ping ack before considering connection dead
			PermitWithoutStream: false,            // Only ping when there are active RPCs
		}),

		// Initial window size: Improve throughput for large query responses
		grpc.WithInitialWindowSize(1<<20), // 1MB (default 64KB)

		// Connection window size: Control flow control for the connection
		grpc.WithInitialConnWindowSize(1<<20), // 1MB

		// Max message size: Allow larger responses for bulk queries
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(10*1024*1024), // 10MB max receive
		),

		// Connection backoff: Graceful reconnection on network issues
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  1.0 * time.Second,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   30 * time.Second,
			},
			MinConnectTimeout: 5 * time.Second, // Fail fast on dead nodes
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
	}

	qc := &Clients{
		logger:   logging.ForComponent(logger, logging.ComponentQueryClients),
		config:   config,
		grpcConn: grpcConn,
	}

	// Initialize individual clients
	qc.sharedClient = newSharedQueryClient(logger, grpcConn, config.QueryTimeout)
	qc.sessionClient = newSessionQueryClient(logger, grpcConn, qc.sharedClient, config.QueryTimeout)
	qc.applicationClient = newApplicationQueryClient(logger, grpcConn, config.QueryTimeout)
	qc.supplierClient = newSupplierQueryClient(logger, grpcConn, config.QueryTimeout)
	qc.proofClient = newProofQueryClient(logger, grpcConn, config.QueryTimeout)
	qc.serviceClient = newServiceQueryClient(logger, grpcConn, config.QueryTimeout)
	qc.accountClient = newAccountQueryClient(logger, grpcConn, config.QueryTimeout)
	qc.bankClient = newBankQueryClient(logger, grpcConn, config.QueryTimeout)

	qc.logger.Info().
		Str("endpoint", config.GRPCEndpoint).
		Msg("query clients initialized")

	return qc, nil
}

// Shared returns the shared module query client.
func (qc *Clients) Shared() client.SharedQueryClient {
	return qc.sharedClient
}

// Session returns the session module query client.
func (qc *Clients) Session() client.SessionQueryClient {
	return qc.sessionClient
}

// Application returns the application module query client.
func (qc *Clients) Application() client.ApplicationQueryClient {
	return qc.applicationClient
}

// Supplier returns the supplier module query client.
func (qc *Clients) Supplier() client.SupplierQueryClient {
	return qc.supplierClient
}

// Proof returns the proof module query client.
func (qc *Clients) Proof() client.ProofQueryClient {
	return qc.proofClient
}

// Service returns the service module query client.
func (qc *Clients) Service() client.ServiceQueryClient {
	return qc.serviceClient
}

// ServiceDifficultyClient provides height-aware relay mining difficulty queries.
// This is separate from client.ServiceQueryClient because the poktroll interface
// may not yet include the height-aware method.
type ServiceDifficultyClient interface {
	GetServiceRelayDifficultyAtHeight(ctx context.Context, serviceId string, blockHeight int64) (servicetypes.RelayMiningDifficulty, error)
}

// ServiceDifficulty returns the service query client with height-aware difficulty queries.
// Use this when you need GetServiceRelayDifficultyAtHeight (e.g., for proof requirement checks).
func (qc *Clients) ServiceDifficulty() ServiceDifficultyClient {
	return qc.serviceClient
}

// Account returns the account query client.
func (qc *Clients) Account() client.AccountQueryClient {
	return qc.accountClient
}

// Bank returns the bank query client.
func (qc *Clients) Bank() client.BankQueryClient {
	return qc.bankClient
}

// GRPCConnection returns the underlying gRPC connection.
// This allows sharing the connection with other clients (e.g., TxClient).
func (qc *Clients) GRPCConnection() *grpc.ClientConn {
	return qc.grpcConn
}

// Close closes all query clients and the underlying gRPC connection.
func (qc *Clients) Close() error {
	qc.mu.Lock()
	defer qc.mu.Unlock()

	if qc.closed {
		return nil
	}
	qc.closed = true

	if qc.grpcConn != nil {
		if err := qc.grpcConn.Close(); err != nil {
			return fmt.Errorf("failed to close gRPC connection: %w", err)
		}
	}

	qc.logger.Info().Msg("query clients closed")
	return nil
}

// =============================================================================
// Shared Query Client
// =============================================================================

type sharedQueryClient struct {
	logger       logging.Logger
	queryClient  sharedtypes.QueryClient
	queryTimeout time.Duration

	// Simple in-memory cache for params
	paramsCache   *sharedtypes.Params
	paramsCacheMu sync.RWMutex
}

var _ client.SharedQueryClient = (*sharedQueryClient)(nil)

func newSharedQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *sharedQueryClient {
	return &sharedQueryClient{
		logger:       logger.With().Str("query_client", "shared").Logger(),
		queryClient:  sharedtypes.NewQueryClient(conn),
		queryTimeout: timeout,
	}
}

func (c *sharedQueryClient) GetParams(ctx context.Context) (*sharedtypes.Params, error) {
	// Check cache first
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	// Double-check after acquiring a lock
	if c.paramsCache != nil {
		return c.paramsCache, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &sharedtypes.QueryParamsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to query shared params: %w", err)
	}

	c.paramsCache = &res.Params
	return &res.Params, nil
}

func (c *sharedQueryClient) GetSessionGracePeriodEndHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, err := c.GetParams(ctx)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetSessionGracePeriodEndHeight(params, queryHeight), nil
}

func (c *sharedQueryClient) GetClaimWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, err := c.GetParams(ctx)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetClaimWindowOpenHeight(params, queryHeight), nil
}

func (c *sharedQueryClient) GetEarliestSupplierClaimCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	params, err := c.GetParams(ctx)
	if err != nil {
		return 0, err
	}

	// NOTE: Block hash parameter not included in interface signature.
	// Poktroll's GetEarliestSupplierClaimCommitHeight (x/shared/types/session.go:107-124)
	// currently ignores the block hash - distribution logic is commented out and it just
	// returns claimWindowOpenHeight. See poktroll TODO_TECHDEBT(@red-0ne) line 129-133.
	//
	// When poktroll enables claim distribution, this interface will need to be extended
	// to accept block hash, or callers will need to call sharedtypes directly.
	return sharedtypes.GetEarliestSupplierClaimCommitHeight(
		params,
		queryHeight,
		nil, // Block hash - not in interface signature, ignored by poktroll anyway
		supplierOperatorAddr,
	), nil
}

func (c *sharedQueryClient) GetProofWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, err := c.GetParams(ctx)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetProofWindowOpenHeight(params, queryHeight), nil
}

func (c *sharedQueryClient) GetEarliestSupplierProofCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	params, err := c.GetParams(ctx)
	if err != nil {
		return 0, err
	}

	// NOTE: Block hash parameter not included in interface signature.
	// Poktroll's GetEarliestSupplierProofCommitHeight (x/shared/types/session.go:134-151)
	// currently ignores the block hash - distribution logic is commented out and it just
	// returns proofWindowOpenHeight. See poktroll TODO_TECHDEBT(@red-0ne) line 129-133.
	//
	// When poktroll enables proof distribution, this interface will need to be extended
	// to accept block hash, or callers will need to call sharedtypes directly.
	return sharedtypes.GetEarliestSupplierProofCommitHeight(
		params,
		queryHeight,
		nil, // Block hash - not in interface signature, ignored by poktroll anyway
		supplierOperatorAddr,
	), nil
}

// InvalidateCache clears the cached params.
func (c *sharedQueryClient) InvalidateCache() {
	c.paramsCacheMu.Lock()
	c.paramsCache = nil
	c.paramsCacheMu.Unlock()
}

// =============================================================================
// Session Query Client
// =============================================================================

type sessionQueryClient struct {
	logger       logging.Logger
	queryClient  sessiontypes.QueryClient
	sharedClient *sharedQueryClient
	queryTimeout time.Duration

	// Simple in-memory cache for sessions
	sessionCache   map[string]*sessiontypes.Session
	sessionCacheMu sync.RWMutex

	// Params cache
	paramsCache   *sessiontypes.Params
	paramsCacheMu sync.RWMutex
}

var _ client.SessionQueryClient = (*sessionQueryClient)(nil)

func newSessionQueryClient(
	logger logging.Logger,
	conn *grpc.ClientConn,
	sharedClient *sharedQueryClient,
	timeout time.Duration,
) *sessionQueryClient {
	return &sessionQueryClient{
		logger:       logger.With().Str("query_client", "session").Logger(),
		queryClient:  sessiontypes.NewQueryClient(conn),
		sharedClient: sharedClient,
		queryTimeout: timeout,
		sessionCache: make(map[string]*sessiontypes.Session),
	}
}

func (c *sessionQueryClient) GetSession(
	ctx context.Context,
	appAddress string,
	serviceId string,
	blockHeight int64,
) (*sessiontypes.Session, error) {
	// Get shared params for cache key calculation
	sharedParams, err := c.sharedClient.GetParams(ctx)
	if err != nil {
		return nil, err
	}

	// Calculate session start height for consistent caching
	sessionStartHeight := sharedtypes.GetSessionStartHeight(sharedParams, blockHeight)
	cacheKey := fmt.Sprintf("%s/%s/%d", appAddress, serviceId, sessionStartHeight)

	// Check cache
	c.sessionCacheMu.RLock()
	if session, ok := c.sessionCache[cacheKey]; ok {
		c.sessionCacheMu.RUnlock()
		return session, nil
	}
	c.sessionCacheMu.RUnlock()

	// Query chain
	c.sessionCacheMu.Lock()
	defer c.sessionCacheMu.Unlock()

	// Double-check after acquiring lock
	if session, ok := c.sessionCache[cacheKey]; ok {
		return session, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.GetSession(queryCtx, &sessiontypes.QueryGetSessionRequest{
		ApplicationAddress: appAddress,
		ServiceId:          serviceId,
		BlockHeight:        blockHeight,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query session: %w", err)
	}

	c.sessionCache[cacheKey] = res.Session
	return res.Session, nil
}

func (c *sessionQueryClient) GetParams(ctx context.Context) (*sessiontypes.Params, error) {
	// Check cache first
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	// Double-check after acquiring lock
	if c.paramsCache != nil {
		return c.paramsCache, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &sessiontypes.QueryParamsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to query session params: %w", err)
	}

	c.paramsCache = &res.Params
	return &res.Params, nil
}

// InvalidateCache clears all cached data.
func (c *sessionQueryClient) InvalidateCache() {
	c.sessionCacheMu.Lock()
	c.sessionCache = make(map[string]*sessiontypes.Session)
	c.sessionCacheMu.Unlock()

	c.paramsCacheMu.Lock()
	c.paramsCache = nil
	c.paramsCacheMu.Unlock()
}

// =============================================================================
// Application Query Client
// =============================================================================

type applicationQueryClient struct {
	logger       logging.Logger
	queryClient  apptypes.QueryClient
	queryTimeout time.Duration

	// Simple in-memory cache
	appCache   map[string]apptypes.Application
	appCacheMu sync.RWMutex

	paramsCache   *apptypes.Params
	paramsCacheMu sync.RWMutex
}

var _ client.ApplicationQueryClient = (*applicationQueryClient)(nil)

func newApplicationQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *applicationQueryClient {
	return &applicationQueryClient{
		logger:       logger.With().Str("query_client", "application").Logger(),
		queryClient:  apptypes.NewQueryClient(conn),
		queryTimeout: timeout,
		appCache:     make(map[string]apptypes.Application),
	}
}

func (c *applicationQueryClient) GetApplication(ctx context.Context, appAddress string) (apptypes.Application, error) {
	// Check cache
	c.appCacheMu.RLock()
	if app, ok := c.appCache[appAddress]; ok {
		c.appCacheMu.RUnlock()
		return app, nil
	}
	c.appCacheMu.RUnlock()

	// Query chain
	c.appCacheMu.Lock()
	defer c.appCacheMu.Unlock()

	// Double-check after acquiring lock
	if app, ok := c.appCache[appAddress]; ok {
		return app, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Application(queryCtx, &apptypes.QueryGetApplicationRequest{
		Address: appAddress,
	})
	if err != nil {
		return apptypes.Application{}, fmt.Errorf("failed to query application: %w", err)
	}

	c.appCache[appAddress] = res.Application
	return res.Application, nil
}

func (c *applicationQueryClient) GetAllApplications(ctx context.Context) ([]apptypes.Application, error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.AllApplications(queryCtx, &apptypes.QueryAllApplicationsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to query all applications: %w", err)
	}

	return res.Applications, nil
}

func (c *applicationQueryClient) GetParams(ctx context.Context) (*apptypes.Params, error) {
	// Check cache first
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	if c.paramsCache != nil {
		return c.paramsCache, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &apptypes.QueryParamsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to query application params: %w", err)
	}

	c.paramsCache = &res.Params
	return &res.Params, nil
}

// =============================================================================
// Supplier Query Client
// =============================================================================

type supplierQueryClient struct {
	logger       logging.Logger
	queryClient  suppliertypes.QueryClient
	queryTimeout time.Duration

	// Simple in-memory cache
	supplierCache   map[string]sharedtypes.Supplier
	supplierCacheMu sync.RWMutex

	paramsCache   *suppliertypes.Params
	paramsCacheMu sync.RWMutex
}

var _ client.SupplierQueryClient = (*supplierQueryClient)(nil)

func newSupplierQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *supplierQueryClient {
	return &supplierQueryClient{
		logger:        logger.With().Str("query_client", "supplier").Logger(),
		queryClient:   suppliertypes.NewQueryClient(conn),
		queryTimeout:  timeout,
		supplierCache: make(map[string]sharedtypes.Supplier),
	}
}

func (c *supplierQueryClient) GetSupplier(ctx context.Context, supplierOperatorAddress string) (sharedtypes.Supplier, error) {
	// Check cache
	c.supplierCacheMu.RLock()
	if supplier, ok := c.supplierCache[supplierOperatorAddress]; ok {
		c.supplierCacheMu.RUnlock()
		return supplier, nil
	}
	c.supplierCacheMu.RUnlock()

	// Query chain
	c.supplierCacheMu.Lock()
	defer c.supplierCacheMu.Unlock()

	// Double-check after acquiring lock
	if supplier, ok := c.supplierCache[supplierOperatorAddress]; ok {
		return supplier, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Supplier(queryCtx, &suppliertypes.QueryGetSupplierRequest{
		OperatorAddress: supplierOperatorAddress,
	})
	if err != nil {
		return sharedtypes.Supplier{}, fmt.Errorf("failed to query supplier: %w", err)
	}

	c.supplierCache[supplierOperatorAddress] = res.Supplier
	return res.Supplier, nil
}

func (c *supplierQueryClient) GetParams(ctx context.Context) (*suppliertypes.Params, error) {
	// Check cache first
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	if c.paramsCache != nil {
		return c.paramsCache, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &suppliertypes.QueryParamsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to query supplier params: %w", err)
	}

	c.paramsCache = &res.Params
	return &res.Params, nil
}

// =============================================================================
// Proof Query Client
// =============================================================================

type proofQueryClient struct {
	logger       logging.Logger
	queryClient  prooftypes.QueryClient
	queryTimeout time.Duration

	// Simple in-memory cache
	claimCache   map[string]*prooftypes.Claim
	claimCacheMu sync.RWMutex

	paramsCache   *prooftypes.Params
	paramsCacheMu sync.RWMutex
}

var _ client.ProofQueryClient = (*proofQueryClient)(nil)

func newProofQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *proofQueryClient {
	return &proofQueryClient{
		logger:       logger.With().Str("query_client", "proof").Logger(),
		queryClient:  prooftypes.NewQueryClient(conn),
		queryTimeout: timeout,
		claimCache:   make(map[string]*prooftypes.Claim),
	}
}

func (c *proofQueryClient) GetParams(ctx context.Context) (client.ProofParams, error) {
	// Check cache first
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	if c.paramsCache != nil {
		return c.paramsCache, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &prooftypes.QueryParamsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to query proof params: %w", err)
	}

	c.paramsCache = &res.Params
	return &res.Params, nil
}

func (c *proofQueryClient) GetClaim(ctx context.Context, supplierOperatorAddress string, sessionId string) (client.Claim, error) {
	cacheKey := fmt.Sprintf("%s/%s", supplierOperatorAddress, sessionId)

	// Check cache
	c.claimCacheMu.RLock()
	if claim, ok := c.claimCache[cacheKey]; ok {
		c.claimCacheMu.RUnlock()
		return claim, nil
	}
	c.claimCacheMu.RUnlock()

	// Query chain
	c.claimCacheMu.Lock()
	defer c.claimCacheMu.Unlock()

	// Double-check after acquiring lock
	if claim, ok := c.claimCache[cacheKey]; ok {
		return claim, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Claim(queryCtx, &prooftypes.QueryGetClaimRequest{
		SupplierOperatorAddress: supplierOperatorAddress,
		SessionId:               sessionId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query claim: %w", err)
	}

	c.claimCache[cacheKey] = &res.Claim
	return &res.Claim, nil
}

// =============================================================================
// Service Query Client
// =============================================================================

// maxHeightDifficultyCacheEntries is the maximum number of height-aware difficulty
// entries (keyed by serviceID@height) before eviction is triggered. This is a simple
// size cap — no assumptions about session length or block timing. The data is immutable
// and cheap to re-query from chain if an evicted entry is needed again.
// Sized generously: a provider with 28 services across many session heights fits easily.
const maxHeightDifficultyCacheEntries = 1000

type serviceQueryClient struct {
	logger       logging.Logger
	queryClient  servicetypes.QueryClient
	queryTimeout time.Duration

	// Simple in-memory cache for services (keyed by serviceID)
	serviceCache   map[string]sharedtypes.Service
	serviceCacheMu sync.RWMutex

	// Cache for latest difficulty queries (keyed by serviceID only)
	difficultyCache   map[string]servicetypes.RelayMiningDifficulty
	difficultyCacheMu sync.RWMutex

	// Bounded cache for height-aware difficulty queries (keyed by "serviceID@height").
	// Uses xsync.MapOf for lock-free concurrent reads (project standard).
	// Difficulty at a given height is immutable — no invalidation needed.
	heightDifficultyCache *xsync.Map[string, heightDifficultyCacheEntry]
	// heightDifficultyCacheSize tracks entry count atomically for O(1) threshold checks.
	heightDifficultyCacheSize atomic.Int64

	paramsCache   *servicetypes.Params
	paramsCacheMu sync.RWMutex
}

// heightDifficultyCacheEntry stores difficulty data alongside the block height
// for efficient eviction sweeps.
type heightDifficultyCacheEntry struct {
	difficulty  servicetypes.RelayMiningDifficulty
	blockHeight int64
}

var _ client.ServiceQueryClient = (*serviceQueryClient)(nil)
var _ ServiceDifficultyClient = (*serviceQueryClient)(nil)

func newServiceQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *serviceQueryClient {
	return &serviceQueryClient{
		logger:                logger.With().Str("query_client", "service").Logger(),
		queryClient:           servicetypes.NewQueryClient(conn),
		queryTimeout:          timeout,
		serviceCache:          make(map[string]sharedtypes.Service),
		difficultyCache:       make(map[string]servicetypes.RelayMiningDifficulty),
		heightDifficultyCache: xsync.NewMap[string, heightDifficultyCacheEntry](),
	}
}

func (c *serviceQueryClient) GetService(ctx context.Context, serviceId string) (sharedtypes.Service, error) {
	// Check cache
	c.serviceCacheMu.RLock()
	if service, ok := c.serviceCache[serviceId]; ok {
		c.serviceCacheMu.RUnlock()
		return service, nil
	}
	c.serviceCacheMu.RUnlock()

	// Query chain
	c.serviceCacheMu.Lock()
	defer c.serviceCacheMu.Unlock()

	// Double-check after acquiring lock
	if service, ok := c.serviceCache[serviceId]; ok {
		return service, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Service(queryCtx, &servicetypes.QueryGetServiceRequest{
		Id: serviceId,
	})
	if err != nil {
		return sharedtypes.Service{}, fmt.Errorf("failed to query service: %w", err)
	}

	c.serviceCache[serviceId] = res.Service
	return res.Service, nil
}

func (c *serviceQueryClient) GetServiceRelayDifficulty(ctx context.Context, serviceId string) (servicetypes.RelayMiningDifficulty, error) {
	// Check cache
	c.difficultyCacheMu.RLock()
	if difficulty, ok := c.difficultyCache[serviceId]; ok {
		c.difficultyCacheMu.RUnlock()
		return difficulty, nil
	}
	c.difficultyCacheMu.RUnlock()

	// Query chain
	c.difficultyCacheMu.Lock()
	defer c.difficultyCacheMu.Unlock()

	// Double-check after acquiring lock
	if difficulty, ok := c.difficultyCache[serviceId]; ok {
		return difficulty, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.RelayMiningDifficulty(queryCtx, &servicetypes.QueryGetRelayMiningDifficultyRequest{
		ServiceId: serviceId,
	})
	if err != nil {
		return servicetypes.RelayMiningDifficulty{}, fmt.Errorf("failed to query relay mining difficulty: %w", err)
	}

	c.difficultyCache[serviceId] = res.RelayMiningDifficulty
	return res.RelayMiningDifficulty, nil
}

// GetServiceRelayDifficultyAtHeight queries the chain for the relay mining difficulty
// of a service at a specific block height. This is used to get the difficulty that was
// effective at session start, ensuring consistency with on-chain proof validation.
// Results are cached in a bounded xsync.MapOf with composite key "serviceID@blockHeight".
// Difficulty at a given height is immutable — no invalidation needed, only eviction of old entries.
func (c *serviceQueryClient) GetServiceRelayDifficultyAtHeight(ctx context.Context, serviceId string, blockHeight int64) (servicetypes.RelayMiningDifficulty, error) {
	cacheKey := fmt.Sprintf("%s@%d", serviceId, blockHeight)

	// Check cache (lock-free read via xsync.MapOf)
	if entry, ok := c.heightDifficultyCache.Load(cacheKey); ok {
		return entry.difficulty, nil
	}

	// Query chain
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.RelayMiningDifficultyAtHeight(queryCtx, &servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest{
		ServiceId:   serviceId,
		BlockHeight: blockHeight,
	})
	if err != nil {
		return servicetypes.RelayMiningDifficulty{}, fmt.Errorf("failed to query relay mining difficulty at height %d: %w", blockHeight, err)
	}

	// Store in cache — LoadOrStore avoids duplicate inserts from concurrent queries
	entry := heightDifficultyCacheEntry{
		difficulty:  res.RelayMiningDifficulty,
		blockHeight: blockHeight,
	}
	if _, loaded := c.heightDifficultyCache.LoadOrStore(cacheKey, entry); !loaded {
		c.heightDifficultyCacheSize.Add(1)
	}

	// Evict oldest entries if cache exceeds size cap
	if c.heightDifficultyCacheSize.Load() > maxHeightDifficultyCacheEntries {
		c.evictOldestHeightDifficultyEntries()
	}

	return res.RelayMiningDifficulty, nil
}

// evictOldestHeightDifficultyEntries finds the median block height across all
// cached entries and removes everything at or below it. This evicts roughly
// half the cache without assuming any particular session length or block timing.
// The data is immutable — evicted entries are simply re-queried from chain if needed.
func (c *serviceQueryClient) evictOldestHeightDifficultyEntries() {
	// Collect all heights to find the median
	var heights []int64
	c.heightDifficultyCache.Range(func(_ string, entry heightDifficultyCacheEntry) bool {
		heights = append(heights, entry.blockHeight)
		return true
	})
	if len(heights) == 0 {
		return
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
	medianHeight := heights[len(heights)/2]

	// Delete entries at or below the median height
	c.heightDifficultyCache.Range(func(key string, entry heightDifficultyCacheEntry) bool {
		if entry.blockHeight <= medianHeight {
			c.heightDifficultyCache.Delete(key)
			c.heightDifficultyCacheSize.Add(-1)
		}
		return true
	})
}

func (c *serviceQueryClient) GetParams(ctx context.Context) (*servicetypes.Params, error) {
	// Check cache first
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	if c.paramsCache != nil {
		return c.paramsCache, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &servicetypes.QueryParamsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to query service params: %w", err)
	}

	c.paramsCache = &res.Params
	return &res.Params, nil
}

// =============================================================================
// Account Query Client
// =============================================================================

type accountQueryClient struct {
	logger       logging.Logger
	queryClient  accounttypes.QueryClient
	queryTimeout time.Duration

	// Simple in-memory cache for accounts
	accountCache   map[string]cosmostypes.AccountI
	accountCacheMu sync.RWMutex
}

var _ client.AccountQueryClient = (*accountQueryClient)(nil)

func newAccountQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *accountQueryClient {
	return &accountQueryClient{
		logger:       logger.With().Str("query_client", "account").Logger(),
		queryClient:  accounttypes.NewQueryClient(conn),
		queryTimeout: timeout,
		accountCache: make(map[string]cosmostypes.AccountI),
	}
}

func (c *accountQueryClient) GetAccount(ctx context.Context, address string) (cosmostypes.AccountI, error) {
	// Check cache
	c.accountCacheMu.RLock()
	if acc, ok := c.accountCache[address]; ok {
		c.accountCacheMu.RUnlock()
		return acc, nil
	}
	c.accountCacheMu.RUnlock()

	// Query chain
	c.accountCacheMu.Lock()
	defer c.accountCacheMu.Unlock()

	// Double-check after acquiring lock
	if acc, ok := c.accountCache[address]; ok {
		return acc, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Account(queryCtx, &accounttypes.QueryAccountRequest{
		Address: address,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query account %s: %w", address, err)
	}

	// Unpack the account from Any
	var account cosmostypes.AccountI
	if err := queryCodec.UnpackAny(res.Account, &account); err != nil {
		return nil, fmt.Errorf("failed to unpack account %s: %w", address, err)
	}

	// Only cache accounts that have a public key set
	// Accounts without public keys may be genesis accounts that haven't transacted yet
	if account.GetPubKey() != nil {
		c.accountCache[address] = account
	}

	return account, nil
}

func (c *accountQueryClient) GetPubKeyFromAddress(ctx context.Context, address string) (cryptotypes.PubKey, error) {
	acc, err := c.GetAccount(ctx, address)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, fmt.Errorf("account not found: %s", address)
	}

	pubKey := acc.GetPubKey()
	if pubKey == nil {
		return nil, fmt.Errorf("public key not found for account: %s", address)
	}

	return pubKey, nil
}

// =============================================================================
// Block Query Client (using CometBFT RPC)
// =============================================================================

// BlockQueryClient provides block queries using CometBFT RPC.
type BlockQueryClient interface {
	client.BlockQueryClient
	Close() error
}

type blockQueryClient struct {
	logger       logging.Logger
	rpcEndpoint  string
	queryTimeout time.Duration
}

// NewBlockQueryClient creates a new block query client.
// Note: This requires a CometBFT RPC endpoint, not gRPC.
func NewBlockQueryClient(
	logger logging.Logger,
	rpcEndpoint string,
	queryTimeout time.Duration,
) BlockQueryClient {
	if queryTimeout == 0 {
		queryTimeout = defaultQueryTimeout
	}
	return &blockQueryClient{
		logger:       logger.With().Str("query_client", "block").Logger(),
		rpcEndpoint:  rpcEndpoint,
		queryTimeout: queryTimeout,
	}
}

func (c *blockQueryClient) Block(ctx context.Context, height *int64) (*cometrpctypes.ResultBlock, error) {
	// This is a simplified implementation
	// For full functionality, use the cometbft/rpc/client package
	return nil, fmt.Errorf("block query requires CometBFT RPC client - use pkg/client/block for full implementation")
}

func (c *blockQueryClient) Close() error {
	return nil
}

// =============================================================================
// Bank Query Client
// =============================================================================

type bankQueryClient struct {
	logger       logging.Logger
	queryClient  banktypes.QueryClient
	queryTimeout time.Duration
}

var _ client.BankQueryClient = (*bankQueryClient)(nil)

func newBankQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *bankQueryClient {
	return &bankQueryClient{
		logger:       logger.With().Str("query_client", "bank").Logger(),
		queryClient:  banktypes.NewQueryClient(conn),
		queryTimeout: timeout,
	}
}

func (c *bankQueryClient) GetBalance(ctx context.Context, address string) (*cosmostypes.Coin, error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Balance(queryCtx, &banktypes.QueryBalanceRequest{
		Address: address,
		Denom:   "upokt",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query balance for %s: %w", address, err)
	}

	return res.Balance, nil
}
