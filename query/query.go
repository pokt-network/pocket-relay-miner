package query

import (
	"bytes"
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
	query "github.com/cosmos/cosmos-sdk/types/query"
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
func (qc *Clients) Application() ApplicationQueryClient {
	return qc.applicationClient
}

// Supplier returns the supplier module query client.
func (qc *Clients) Supplier() SupplierQueryClient {
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

	// Simple in-memory cache for params, refreshed after liveParamsCacheTTL.
	paramsCache   *sharedtypes.Params
	paramsCacheAt time.Time
	paramsCacheMu sync.RWMutex

	// paramsAtHeightCache memoizes ParamsAtHeight results keyed by query height.
	// SAFE because params-at-a-height are immutable for every height our callers query:
	// poktroll only writes params-history entries at session boundaries (effective_height
	// = next session start) and never mid-session, and all callers pass a started session's
	// start/end height (<= the current session end < the next boundary). So no later
	// param change can alter the entry resolved for such a height — the same immutability
	// the serviceQueryClient relies on for GetServiceRelayDifficultyAtHeight.
	// Entries carry a fetch time so the immutableCacheTTLFloor expires them (mandate).
	paramsAtHeightCache   map[int64]paramsAtHeightEntry
	paramsAtHeightCacheMu sync.RWMutex
}

// paramsAtHeightEntry is an immutable params-at-height value plus its fetch time,
// for the TTL safety floor.
type paramsAtHeightEntry struct {
	params   *sharedtypes.Params
	cachedAt time.Time
}

// maxParamsAtHeightCacheEntries bounds the height-keyed params cache. Entries are
// immutable, so eviction is purely to cap memory; the lowest (oldest) heights are
// dropped first since settled sessions are not re-queried.
const maxParamsAtHeightCacheEntries = 1024

// liveParamsCacheTTL bounds how long live module params (shared/session/proof
// GetParams) are served from memory before re-querying the chain. Governance
// params change rarely but DO change (e.g. weekly CUTTM updates on mainnet);
// a fetch-once cache freezes them at process start, silently diverging the
// miner's claim valuation from the chain's until restart (June 2026
// PROOF_MISSING wave). ~1.5 blocks of staleness is the most a "live" read can
// drift without materially diverging from what the chain reads.
//
// A var (not const) so tests can shrink it without sleeping.
var liveParamsCacheTTL = 90 * time.Second

// liveEntityCacheTTL bounds how long a "latest"-semantics entity (application,
// supplier, claim) is served from a query-client cache before re-querying the
// chain. Stake/delegation/service-list/proof-status are LATEST reads; a
// fetch-once cache freezes them until process restart. Mirrors liveParamsCacheTTL.
// A var (not const) so tests can shrink it without sleeping.
var liveEntityCacheTTL = 90 * time.Second

// immutableCacheTTLFloor is the max-age SAFETY FLOOR for caches we believe are
// immutable / height-keyed / session-bound (session header, account, params at
// height, difficulty at height). Per the project mandate, NO cache entry may live
// for the process lifetime even when we think the value never changes: an
// assumption that turns out wrong (upstream semantic change, reorg, our own bug)
// must self-heal within one floor instead of needing a restart. Long enough not
// to add meaningful L3 load for genuinely-immutable data. A var (not const) so
// tests can shrink it without sleeping.
var immutableCacheTTLFloor = 30 * time.Minute

var _ client.SharedQueryClient = (*sharedQueryClient)(nil)

func newSharedQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *sharedQueryClient {
	return &sharedQueryClient{
		logger:              logger.With().Str("query_client", "shared").Logger(),
		queryClient:         sharedtypes.NewQueryClient(conn),
		queryTimeout:        timeout,
		paramsAtHeightCache: make(map[int64]paramsAtHeightEntry),
	}
}

func (c *sharedQueryClient) GetParams(ctx context.Context) (*sharedtypes.Params, error) {
	// Serve from cache while fresh
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("shared", "params").Inc()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	// Double-check after acquiring a lock
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		queryCacheHits.WithLabelValues("shared", "params").Inc()
		return c.paramsCache, nil
	}

	queryCacheMisses.WithLabelValues("shared", "params").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &sharedtypes.QueryParamsRequest{})
	if err != nil {
		// Serve the stale value on a failed refresh: a transient RPC error must
		// not break callers (e.g. the proof-requirement path) that previously
		// had a value. Staleness here is bounded by the outage, not unbounded.
		if c.paramsCache != nil {
			c.logger.Warn().Err(err).Msg("shared params refresh failed; serving stale cache")
			return c.paramsCache, nil
		}
		return nil, fmt.Errorf("failed to query shared params: %w", err)
	}

	c.paramsCache = &res.Params
	c.paramsCacheAt = time.Now()
	queryCacheSize.WithLabelValues("shared", "params").Set(1)
	return &res.Params, nil
}

// GetParamsAtHeight returns the shared params that were effective at queryHeight.
//
// Window-timing computations must evaluate a session with the num_blocks_per_session
// that was in effect when that session started, not the live value. After a
// session-length change (poktroll #543 anchored grid), an old-epoch session computed
// with live (new-epoch) params would resolve to the wrong session grid and the miner
// would submit its claim/proof at the wrong window. queryHeight <= 0 falls back to the
// live params.
func (c *sharedQueryClient) GetParamsAtHeight(ctx context.Context, queryHeight int64) (*sharedtypes.Params, error) {
	if queryHeight <= 0 {
		return c.GetParams(ctx)
	}

	// Serve from the height-keyed cache when present and within the TTL floor
	// (entries are immutable, see field doc; the floor only forces an occasional
	// re-query to satisfy the cache-TTL mandate).
	c.paramsAtHeightCacheMu.RLock()
	if e, ok := c.paramsAtHeightCache[queryHeight]; ok && time.Since(e.cachedAt) < immutableCacheTTLFloor {
		c.paramsAtHeightCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("shared", "params_at_height").Inc()
		return e.params, nil
	}
	c.paramsAtHeightCacheMu.RUnlock()

	queryCacheMisses.WithLabelValues("shared", "params_at_height").Inc()

	// Always resolve through the chain's ParamsAtHeight RPC. We deliberately do NOT
	// short-circuit on c.GetParams(): paramsCache is populated once and never
	// invalidated in production, so after an on-chain MsgUpdateParam its cached
	// SessionGridAnchorHeight belongs to the OLD epoch. A fast path keyed on that stale
	// anchor (anchor <= queryHeight) would be satisfied for current-epoch heights and
	// return stale params — silently defeating the height-aware semantics this method
	// exists for. ParamsAtHeight is authoritative: the chain returns the live params for
	// a current-epoch height (no history entry <= height) and the historical snapshot
	// for an older-epoch height.
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.ParamsAtHeight(queryCtx, &sharedtypes.QueryParamsAtHeightRequest{Height: queryHeight})
	if err != nil {
		return nil, fmt.Errorf("failed to query shared params at height %d: %w", queryHeight, err)
	}

	params := &res.Params
	c.storeParamsAtHeight(queryHeight, params)
	return params, nil
}

// storeParamsAtHeight caches an immutable params-at-height entry, evicting the lowest
// heights first when the cache is full. A single write lock guards the whole store so
// the size check and eviction cannot race.
func (c *sharedQueryClient) storeParamsAtHeight(height int64, params *sharedtypes.Params) {
	c.paramsAtHeightCacheMu.Lock()
	defer c.paramsAtHeightCacheMu.Unlock()

	if c.paramsAtHeightCache == nil {
		c.paramsAtHeightCache = make(map[int64]paramsAtHeightEntry)
	}

	if _, ok := c.paramsAtHeightCache[height]; !ok && len(c.paramsAtHeightCache) >= maxParamsAtHeightCacheEntries {
		// Evict the lowest (oldest) heights down to half capacity. Settled sessions are
		// not re-queried, so dropping the smallest heights is the cheapest useful policy.
		heights := make([]int64, 0, len(c.paramsAtHeightCache))
		for h := range c.paramsAtHeightCache {
			heights = append(heights, h)
		}
		sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
		for _, h := range heights[:len(heights)-maxParamsAtHeightCacheEntries/2] {
			delete(c.paramsAtHeightCache, h)
		}
	}

	c.paramsAtHeightCache[height] = paramsAtHeightEntry{params: params, cachedAt: time.Now()}
	queryCacheSize.WithLabelValues("shared", "params_at_height").Set(float64(len(c.paramsAtHeightCache)))
}

func (c *sharedQueryClient) GetSessionGracePeriodEndHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, err := c.GetParamsAtHeight(ctx, queryHeight)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetSessionGracePeriodEndHeight(params, queryHeight), nil
}

func (c *sharedQueryClient) GetClaimWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, err := c.GetParamsAtHeight(ctx, queryHeight)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetClaimWindowOpenHeight(params, queryHeight), nil
}

func (c *sharedQueryClient) GetEarliestSupplierClaimCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	params, err := c.GetParamsAtHeight(ctx, queryHeight)
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
	params, err := c.GetParamsAtHeight(ctx, queryHeight)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetProofWindowOpenHeight(params, queryHeight), nil
}

func (c *sharedQueryClient) GetEarliestSupplierProofCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	params, err := c.GetParamsAtHeight(ctx, queryHeight)
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

	// In-memory cache for sessions, keyed by app/service/sessionStartHeight so
	// distinct sessions never collide. A session is immutable for its height, so
	// entries are correct to serve permanently — but per the cache-TTL mandate an
	// immutableCacheTTLFloor expires them anyway (self-heal + bound growth).
	sessionCache   map[string]sessionCacheEntry
	sessionCacheMu sync.RWMutex

	// Params cache, refreshed after liveParamsCacheTTL.
	paramsCache   *sessiontypes.Params
	paramsCacheAt time.Time
	paramsCacheMu sync.RWMutex
}

var _ client.SessionQueryClient = (*sessionQueryClient)(nil)

// sessionCacheEntry is a cached session plus its session-start height (for
// window-bounded eviction) and fetch time (for TTL expiry).
type sessionCacheEntry struct {
	session  *sessiontypes.Session
	height   int64
	cachedAt time.Time
}

// maxSessionCacheEntries bounds the in-process session cache. Sessions are keyed
// by (app, service, sessionStartHeight); only recent sessions are read, so when
// the map exceeds this it evicts entries whose start height is far below the
// newest. Without a bound the map grows one entry per distinct session served for
// the process lifetime.
const maxSessionCacheEntries = 2048

// sessionCacheKeepHeights is how many blocks below the newest cached session-start
// height to retain on an eviction sweep.
const sessionCacheKeepHeights = 600

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
		sessionCache: make(map[string]sessionCacheEntry),
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

	// Serve from cache while fresh
	c.sessionCacheMu.RLock()
	if e, ok := c.sessionCache[cacheKey]; ok && time.Since(e.cachedAt) < immutableCacheTTLFloor {
		c.sessionCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("session", "session").Inc()
		return e.session, nil
	}
	c.sessionCacheMu.RUnlock()

	// Query chain
	c.sessionCacheMu.Lock()
	defer c.sessionCacheMu.Unlock()

	// Double-check after acquiring lock
	if e, ok := c.sessionCache[cacheKey]; ok && time.Since(e.cachedAt) < immutableCacheTTLFloor {
		queryCacheHits.WithLabelValues("session", "session").Inc()
		return e.session, nil
	}

	queryCacheMisses.WithLabelValues("session", "session").Inc()

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

	c.sessionCache[cacheKey] = sessionCacheEntry{session: res.Session, height: sessionStartHeight, cachedAt: time.Now()}
	c.evictOldSessionsLocked(sessionStartHeight)
	queryCacheSize.WithLabelValues("session", "session").Set(float64(len(c.sessionCache)))
	return res.Session, nil
}

// evictOldSessionsLocked bounds the session cache. It must be called with
// sessionCacheMu held for writing. When the map exceeds maxSessionCacheEntries it
// drops every entry whose session-start height is more than sessionCacheKeepHeights
// below newestHeight — sessions that old are settled and never re-read.
func (c *sessionQueryClient) evictOldSessionsLocked(newestHeight int64) {
	if len(c.sessionCache) <= maxSessionCacheEntries {
		return
	}
	cutoff := newestHeight - sessionCacheKeepHeights
	for k, e := range c.sessionCache {
		if e.height < cutoff {
			delete(c.sessionCache, k)
		}
	}
}

func (c *sessionQueryClient) GetParams(ctx context.Context) (*sessiontypes.Params, error) {
	// Serve from cache while fresh
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("session", "params").Inc()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	// Double-check after acquiring lock
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		queryCacheHits.WithLabelValues("session", "params").Inc()
		return c.paramsCache, nil
	}

	queryCacheMisses.WithLabelValues("session", "params").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &sessiontypes.QueryParamsRequest{})
	if err != nil {
		// Serve the stale value on a failed refresh (see sharedQueryClient.GetParams).
		if c.paramsCache != nil {
			c.logger.Warn().Err(err).Msg("session params refresh failed; serving stale cache")
			return c.paramsCache, nil
		}
		return nil, fmt.Errorf("failed to query session params: %w", err)
	}

	c.paramsCache = &res.Params
	c.paramsCacheAt = time.Now()
	queryCacheSize.WithLabelValues("session", "params").Set(1)
	return &res.Params, nil
}

// InvalidateCache clears all cached data.
func (c *sessionQueryClient) InvalidateCache() {
	c.sessionCacheMu.Lock()
	c.sessionCache = make(map[string]sessionCacheEntry)
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

	// In-memory cache, TTL-bounded by liveEntityCacheTTL (application stake /
	// delegations are LATEST reads — a frozen entry hides a stake or delegation
	// change until restart).
	appCache   map[string]appCacheEntry
	appCacheMu sync.RWMutex

	paramsCache   *apptypes.Params
	paramsCacheAt time.Time
	paramsCacheMu sync.RWMutex
}

// appCacheEntry is a cached application plus its fetch time, for TTL expiry.
type appCacheEntry struct {
	app      apptypes.Application
	cachedAt time.Time
}

// ApplicationQueryClient extends the base ApplicationQueryClient with cache
// invalidation. Callers invoke InvalidateApplication so that subsequent
// GetApplication calls fetch fresh data from chain, picking up delegation
// or stake changes that occurred since the prior query.
type ApplicationQueryClient interface {
	client.ApplicationQueryClient
	InvalidateApplication(address string)
}

var _ ApplicationQueryClient = (*applicationQueryClient)(nil)

func newApplicationQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *applicationQueryClient {
	return &applicationQueryClient{
		logger:       logger.With().Str("query_client", "application").Logger(),
		queryClient:  apptypes.NewQueryClient(conn),
		queryTimeout: timeout,
		appCache:     make(map[string]appCacheEntry),
	}
}

func (c *applicationQueryClient) GetApplication(ctx context.Context, appAddress string) (apptypes.Application, error) {
	// Serve from cache while fresh
	c.appCacheMu.RLock()
	if e, ok := c.appCache[appAddress]; ok && time.Since(e.cachedAt) < liveEntityCacheTTL {
		c.appCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("application", "entity").Inc()
		return e.app, nil
	}
	c.appCacheMu.RUnlock()

	// Query chain
	c.appCacheMu.Lock()
	defer c.appCacheMu.Unlock()

	// Double-check after acquiring lock
	if e, ok := c.appCache[appAddress]; ok && time.Since(e.cachedAt) < liveEntityCacheTTL {
		queryCacheHits.WithLabelValues("application", "entity").Inc()
		return e.app, nil
	}

	queryCacheMisses.WithLabelValues("application", "entity").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Application(queryCtx, &apptypes.QueryGetApplicationRequest{
		Address: appAddress,
	})
	if err != nil {
		return apptypes.Application{}, fmt.Errorf("failed to query application: %w", err)
	}

	c.appCache[appAddress] = appCacheEntry{app: res.Application, cachedAt: time.Now()}
	queryCacheSize.WithLabelValues("application", "entity").Set(float64(len(c.appCache)))
	return res.Application, nil
}

// InvalidateApplication removes an application from the local query cache.
// Must be called so the next GetApplication call fetches fresh data from
// the chain — picking up any delegation or stake changes (e.g. new gateway
// delegations) that occurred since the application was last queried.
func (c *applicationQueryClient) InvalidateApplication(address string) {
	c.appCacheMu.Lock()
	defer c.appCacheMu.Unlock()
	delete(c.appCache, address)
	queryCacheSize.WithLabelValues("application", "entity").Set(float64(len(c.appCache)))
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
	// Serve from cache while fresh
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("application", "params").Inc()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	// Double-check after acquiring a lock
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		queryCacheHits.WithLabelValues("application", "params").Inc()
		return c.paramsCache, nil
	}

	queryCacheMisses.WithLabelValues("application", "params").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &apptypes.QueryParamsRequest{})
	if err != nil {
		if c.paramsCache != nil {
			c.logger.Warn().Err(err).Msg("application params refresh failed; serving stale cache")
			return c.paramsCache, nil
		}
		return nil, fmt.Errorf("failed to query application params: %w", err)
	}

	c.paramsCache = &res.Params
	c.paramsCacheAt = time.Now()
	queryCacheSize.WithLabelValues("application", "params").Set(1)
	return &res.Params, nil
}

// =============================================================================
// Supplier Query Client
// =============================================================================

type supplierQueryClient struct {
	logger       logging.Logger
	queryClient  suppliertypes.QueryClient
	queryTimeout time.Duration

	// supplierCache caches supplier data within a session.
	// Keyed by operatorAddress. Invalidated at session boundaries via
	// InvalidateSupplier so the miner always re-queries the chain when
	// checking for staking changes (e.g. new services added mid-operation).
	// TTL-bounded by liveEntityCacheTTL as a safety floor in case an expected
	// boundary invalidation is ever missed (stake/services are LATEST reads).
	supplierCache   map[string]supplierCacheEntry
	supplierCacheMu sync.RWMutex

	// paramsCache is TTL-bounded (liveParamsCacheTTL) like the shared/session/proof
	// clients. Supplier MinStake is a governance param read by the leader's
	// stake-health monitor; a fetch-once cache froze it for the process lifetime
	// (the only param client that was missing paramsCacheAt), so a MinStake change
	// was invisible until restart and the leader's block-driven cache.Refresh kept
	// re-writing L1/L2 from this same frozen value.
	paramsCache   *suppliertypes.Params
	paramsCacheAt time.Time
	paramsCacheMu sync.RWMutex
}

// SupplierQueryClient extends the base SupplierQueryClient with cache
// invalidation. The miner calls InvalidateSupplier at session boundaries
// so that subsequent GetSupplier calls fetch fresh data from chain,
// picking up staking changes (e.g. new services added mid-operation).
type SupplierQueryClient interface {
	client.SupplierQueryClient
	InvalidateSupplier(operatorAddress string)
}

var _ SupplierQueryClient = (*supplierQueryClient)(nil)

// supplierCacheEntry is a cached supplier plus its fetch time, for TTL expiry.
type supplierCacheEntry struct {
	supplier sharedtypes.Supplier
	cachedAt time.Time
}

func newSupplierQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *supplierQueryClient {
	return &supplierQueryClient{
		logger:        logger.With().Str("query_client", "supplier").Logger(),
		queryClient:   suppliertypes.NewQueryClient(conn),
		queryTimeout:  timeout,
		supplierCache: make(map[string]supplierCacheEntry),
	}
}

func (c *supplierQueryClient) GetSupplier(ctx context.Context, supplierOperatorAddress string) (sharedtypes.Supplier, error) {
	// Serve from cache while fresh
	c.supplierCacheMu.RLock()
	if e, ok := c.supplierCache[supplierOperatorAddress]; ok && time.Since(e.cachedAt) < liveEntityCacheTTL {
		c.supplierCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("supplier", "entity").Inc()
		return e.supplier, nil
	}
	c.supplierCacheMu.RUnlock()

	// Query chain
	c.supplierCacheMu.Lock()
	defer c.supplierCacheMu.Unlock()

	// Double-check after acquiring lock
	if e, ok := c.supplierCache[supplierOperatorAddress]; ok && time.Since(e.cachedAt) < liveEntityCacheTTL {
		queryCacheHits.WithLabelValues("supplier", "entity").Inc()
		return e.supplier, nil
	}

	queryCacheMisses.WithLabelValues("supplier", "entity").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Supplier(queryCtx, &suppliertypes.QueryGetSupplierRequest{
		OperatorAddress: supplierOperatorAddress,
	})
	if err != nil {
		return sharedtypes.Supplier{}, fmt.Errorf("failed to query supplier: %w", err)
	}

	c.supplierCache[supplierOperatorAddress] = supplierCacheEntry{supplier: res.Supplier, cachedAt: time.Now()}
	queryCacheSize.WithLabelValues("supplier", "entity").Set(float64(len(c.supplierCache)))
	return res.Supplier, nil
}

// InvalidateSupplier removes a supplier from the local query cache.
// Must be called at session boundaries so the next GetSupplier call
// fetches fresh data from the chain — picking up any staking changes
// (e.g. new services added) that occurred during the previous session.
func (c *supplierQueryClient) InvalidateSupplier(operatorAddress string) {
	c.supplierCacheMu.Lock()
	defer c.supplierCacheMu.Unlock()
	delete(c.supplierCache, operatorAddress)
}

func (c *supplierQueryClient) GetParams(ctx context.Context) (*suppliertypes.Params, error) {
	// Serve from cache while fresh
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("supplier", "params").Inc()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	// Double-check after acquiring a lock
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		queryCacheHits.WithLabelValues("supplier", "params").Inc()
		return c.paramsCache, nil
	}

	queryCacheMisses.WithLabelValues("supplier", "params").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &suppliertypes.QueryParamsRequest{})
	if err != nil {
		// Serve the stale value on a failed refresh: a transient RPC error must not
		// break the leader's stake-health monitor. Staleness is bounded by the outage.
		if c.paramsCache != nil {
			c.logger.Warn().Err(err).Msg("supplier params refresh failed; serving stale cache")
			return c.paramsCache, nil
		}
		return nil, fmt.Errorf("failed to query supplier params: %w", err)
	}

	c.paramsCache = &res.Params
	c.paramsCacheAt = time.Now()
	queryCacheSize.WithLabelValues("supplier", "params").Set(1)
	return &res.Params, nil
}

// =============================================================================
// Proof Query Client
// =============================================================================

type proofQueryClient struct {
	logger       logging.Logger
	queryClient  prooftypes.QueryClient
	queryTimeout time.Duration

	// In-memory cache, TTL-bounded by liveEntityCacheTTL (a claim's
	// proof_validation_status is a LATEST read: PENDING -> VALIDATED must not be
	// frozen).
	claimCache   map[string]claimCacheEntry
	claimCacheMu sync.RWMutex

	// Simple in-memory cache for params, refreshed after liveParamsCacheTTL.
	paramsCache   *prooftypes.Params
	paramsCacheAt time.Time
	paramsCacheMu sync.RWMutex
}

var _ client.ProofQueryClient = (*proofQueryClient)(nil)

// claimCacheEntry is a cached claim plus its fetch time, for TTL expiry.
type claimCacheEntry struct {
	claim    *prooftypes.Claim
	cachedAt time.Time
}

func newProofQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *proofQueryClient {
	return &proofQueryClient{
		logger:       logger.With().Str("query_client", "proof").Logger(),
		queryClient:  prooftypes.NewQueryClient(conn),
		queryTimeout: timeout,
		claimCache:   make(map[string]claimCacheEntry),
	}
}

func (c *proofQueryClient) GetParams(ctx context.Context) (client.ProofParams, error) {
	// Serve from cache while fresh. The proof-requirement threshold and
	// probability are read "live" to match the chain's ProofRequirementForClaim;
	// a fetch-once cache would freeze them at process start.
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("proof", "params").Inc()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		queryCacheHits.WithLabelValues("proof", "params").Inc()
		return c.paramsCache, nil
	}

	queryCacheMisses.WithLabelValues("proof", "params").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &prooftypes.QueryParamsRequest{})
	if err != nil {
		// Serve the stale value on a failed refresh (see sharedQueryClient.GetParams).
		if c.paramsCache != nil {
			c.logger.Warn().Err(err).Msg("proof params refresh failed; serving stale cache")
			return c.paramsCache, nil
		}
		return nil, fmt.Errorf("failed to query proof params: %w", err)
	}

	c.paramsCache = &res.Params
	c.paramsCacheAt = time.Now()
	queryCacheSize.WithLabelValues("proof", "params").Set(1)
	return &res.Params, nil
}

func (c *proofQueryClient) GetClaim(ctx context.Context, supplierOperatorAddress string, sessionId string) (client.Claim, error) {
	cacheKey := fmt.Sprintf("%s/%s", supplierOperatorAddress, sessionId)

	// Serve from cache while fresh
	c.claimCacheMu.RLock()
	if e, ok := c.claimCache[cacheKey]; ok && time.Since(e.cachedAt) < liveEntityCacheTTL {
		c.claimCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("proof", "entity").Inc()
		return e.claim, nil
	}
	c.claimCacheMu.RUnlock()

	// Query chain
	c.claimCacheMu.Lock()
	defer c.claimCacheMu.Unlock()

	// Double-check after acquiring lock
	if e, ok := c.claimCache[cacheKey]; ok && time.Since(e.cachedAt) < liveEntityCacheTTL {
		queryCacheHits.WithLabelValues("proof", "entity").Inc()
		return e.claim, nil
	}

	queryCacheMisses.WithLabelValues("proof", "entity").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Claim(queryCtx, &prooftypes.QueryGetClaimRequest{
		SupplierOperatorAddress: supplierOperatorAddress,
		SessionId:               sessionId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query claim: %w", err)
	}

	c.claimCache[cacheKey] = claimCacheEntry{claim: &res.Claim, cachedAt: time.Now()}
	queryCacheSize.WithLabelValues("proof", "entity").Set(float64(len(c.claimCache)))
	return &res.Claim, nil
}

// supplierInclusionQuerier mirrors the miner-layer InclusionQueryClient
// interface that the inclusion reconciler type-asserts ProofQueryClient to. The
// reconciler activates via a runtime assertion (interfaces can't cross the
// miner→query import boundary the other way); this compile-time check ensures
// *proofQueryClient keeps the exact method set + signatures, so a signature
// drift fails the build instead of silently disabling the reconciler at runtime.
type supplierInclusionQuerier interface {
	GetSupplierClaimSessions(ctx context.Context, supplier string) (map[string]struct{}, error)
	GetSupplierProvenSessions(ctx context.Context, supplier string) (map[string]struct{}, error)
}

var _ supplierInclusionQuerier = (*proofQueryClient)(nil)

// inclusionPageLimit bounds each AllProofs/AllClaims page. A supplier serves at
// most a few dozen sessions per window (NumSuppliersPerSession-bounded across a
// handful of services, plus a few not-yet-pruned prior epochs), so one page
// usually suffices; the loop below follows pagination to completion regardless.
const inclusionPageLimit = 100

// maxInclusionPages caps the AllProofs/AllClaims pagination loop as a defense
// against a buggy/malicious node that returns a non-advancing or cyclic NextKey.
// A supplier's per-window set is a few dozen entries (one page); this ceiling is
// far above any legitimate response.
const maxInclusionPages = 10000

// GetSupplierProvenSessions returns the set of session IDs for which the given
// supplier's claim has been PROVEN — i.e. a proof was submitted and validated
// on-chain (the claim's ProofValidationStatus == VALIDATED). It is the
// per-supplier PROOF inclusion signal for the block-driven inclusion reconciler.
//
// Why this reads CLAIMS, not proofs: in poktroll a submitted proof is validated
// and then DELETED from module state in the EndBlocker of its submission height
// (x/proof/module/abci.go EndBlocker → ValidateSubmittedProofs → RemoveProof,
// every block). A proof therefore lives in queryable state for less than one
// block, so AllProofs/GetProof by supplier almost always returns empty even for
// a proof that landed and validated successfully — querying proofs to confirm
// proof inclusion produces a false "missing" for every proof. The durable record
// of proof inclusion is the CLAIM: the EndBlocker sets ProofValidationStatus to
// VALIDATED (or INVALID), and the claim persists until settlement. A claim still
// in PENDING_VALIDATION after the proof window opened means the proof is
// genuinely missing and should be (re)submitted.
//
// Intentionally uncached, index-safe (reads module state via the AllClaims
// supplier secondary index, NOT the Tendermint tx indexer, so it works on
// tx_index=null / pruned nodes), and pagination-complete — same properties as
// GetSupplierClaimSessions.
func (c *proofQueryClient) GetSupplierProvenSessions(ctx context.Context, supplierOperatorAddress string) (map[string]struct{}, error) {
	sessions := make(map[string]struct{})
	var nextKey []byte
	for page := 0; page < maxInclusionPages; page++ {
		queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
		res, err := c.queryClient.AllClaims(queryCtx, &prooftypes.QueryAllClaimsRequest{
			Filter: &prooftypes.QueryAllClaimsRequest_SupplierOperatorAddress{
				SupplierOperatorAddress: supplierOperatorAddress,
			},
			Pagination: &query.PageRequest{Limit: inclusionPageLimit, Key: nextKey},
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to query all claims (proven) for supplier %s: %w", supplierOperatorAddress, err)
		}
		for i := range res.Claims {
			// Only a VALIDATED claim confirms the proof landed. PENDING_VALIDATION
			// (proof not yet submitted/validated) and INVALID (proof rejected) are
			// both "not proven" — left out so the reconciler treats them as missing.
			if res.Claims[i].GetProofValidationStatus() != prooftypes.ClaimProofStatus_VALIDATED {
				continue
			}
			if sh := res.Claims[i].GetSessionHeader(); sh != nil {
				sessions[sh.GetSessionId()] = struct{}{}
			}
		}
		if res.Pagination == nil || len(res.Pagination.NextKey) == 0 {
			return sessions, nil
		}
		// Guard against a node that returns a non-advancing NextKey.
		if bytes.Equal(res.Pagination.NextKey, nextKey) {
			break
		}
		nextKey = res.Pagination.NextKey
	}
	return nil, fmt.Errorf("all claims (proven) pagination for supplier %s did not terminate within %d pages", supplierOperatorAddress, maxInclusionPages)
}

// GetSupplierClaimSessions returns the set of session IDs for which a claim
// exists on-chain for the given supplier, read from x/proof module state via the
// AllClaims supplier secondary index. Proof-side analogue is
// GetSupplierProvenSessions (which also reads claims — see that method for why
// proof inclusion can't be read from proofs); same uncached + index-safe
// (tx_index=null) + full-pagination semantics. It is the per-supplier inclusion
// signal for the claim phase of the block-driven inclusion reconciler.
func (c *proofQueryClient) GetSupplierClaimSessions(ctx context.Context, supplierOperatorAddress string) (map[string]struct{}, error) {
	sessions := make(map[string]struct{})
	var nextKey []byte
	for page := 0; page < maxInclusionPages; page++ {
		queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
		res, err := c.queryClient.AllClaims(queryCtx, &prooftypes.QueryAllClaimsRequest{
			Filter: &prooftypes.QueryAllClaimsRequest_SupplierOperatorAddress{
				SupplierOperatorAddress: supplierOperatorAddress,
			},
			Pagination: &query.PageRequest{Limit: inclusionPageLimit, Key: nextKey},
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to query all claims for supplier %s: %w", supplierOperatorAddress, err)
		}
		for i := range res.Claims {
			if sh := res.Claims[i].GetSessionHeader(); sh != nil {
				sessions[sh.GetSessionId()] = struct{}{}
			}
		}
		if res.Pagination == nil || len(res.Pagination.NextKey) == 0 {
			return sessions, nil
		}
		if bytes.Equal(res.Pagination.NextKey, nextKey) {
			break
		}
		nextKey = res.Pagination.NextKey
	}
	return nil, fmt.Errorf("all claims pagination for supplier %s did not terminate within %d pages", supplierOperatorAddress, maxInclusionPages)
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

	// In-memory cache for services (keyed by serviceID), TTL-bounded.
	// A service's compute_units_per_relay can change on-chain, so this MUST
	// expire — a frozen CUPR mis-weights claims (see serviceCacheTTL).
	serviceCache   map[string]serviceCacheEntry
	serviceCacheMu sync.RWMutex

	// Bounded cache for height-aware difficulty queries (keyed by "serviceID@height").
	// Uses xsync.MapOf for lock-free concurrent reads (project standard).
	// Difficulty at a given height is immutable — no invalidation needed.
	heightDifficultyCache *xsync.Map[string, heightDifficultyCacheEntry]
	// heightDifficultyCacheSize tracks entry count atomically for O(1) threshold checks.
	heightDifficultyCacheSize atomic.Int64

	paramsCache   *servicetypes.Params
	paramsCacheAt time.Time
	paramsCacheMu sync.RWMutex
}

// heightDifficultyCacheEntry stores difficulty data alongside the block height
// for efficient eviction sweeps, plus the fetch time for the TTL safety floor.
type heightDifficultyCacheEntry struct {
	difficulty  servicetypes.RelayMiningDifficulty
	blockHeight int64
	cachedAt    time.Time
}

var _ client.ServiceQueryClient = (*serviceQueryClient)(nil)
var _ ServiceDifficultyClient = (*serviceQueryClient)(nil)

func newServiceQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *serviceQueryClient {
	return &serviceQueryClient{
		logger:                logger.With().Str("query_client", "service").Logger(),
		queryClient:           servicetypes.NewQueryClient(conn),
		queryTimeout:          timeout,
		serviceCache:          make(map[string]serviceCacheEntry),
		heightDifficultyCache: xsync.NewMap[string, heightDifficultyCacheEntry](),
	}
}

// serviceCacheTTL bounds how long a cached service (including its
// compute_units_per_relay) is served before the chain is re-queried. CUPR can
// change on-chain; a permanent cache freezes it and mis-weights claims. var (not
// const) so tests can shrink it. Mirrors liveParamsCacheTTL.
var serviceCacheTTL = 90 * time.Second

// serviceCacheEntry is a cached service plus the time it was fetched, for TTL.
type serviceCacheEntry struct {
	service  sharedtypes.Service
	cachedAt time.Time
}

func (c *serviceQueryClient) GetService(ctx context.Context, serviceId string) (sharedtypes.Service, error) {
	// Check cache (fresh within TTL only)
	c.serviceCacheMu.RLock()
	if e, ok := c.serviceCache[serviceId]; ok && time.Since(e.cachedAt) < serviceCacheTTL {
		c.serviceCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("service", "entity").Inc()
		return e.service, nil
	}
	c.serviceCacheMu.RUnlock()

	// Query chain
	c.serviceCacheMu.Lock()
	defer c.serviceCacheMu.Unlock()

	// Double-check after acquiring lock
	if e, ok := c.serviceCache[serviceId]; ok && time.Since(e.cachedAt) < serviceCacheTTL {
		queryCacheHits.WithLabelValues("service", "entity").Inc()
		return e.service, nil
	}

	queryCacheMisses.WithLabelValues("service", "entity").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Service(queryCtx, &servicetypes.QueryGetServiceRequest{
		Id: serviceId,
	})
	if err != nil {
		return sharedtypes.Service{}, fmt.Errorf("failed to query service: %w", err)
	}

	c.serviceCache[serviceId] = serviceCacheEntry{service: res.Service, cachedAt: time.Now()}
	queryCacheSize.WithLabelValues("service", "entity").Set(float64(len(c.serviceCache)))
	return res.Service, nil
}

// InvalidateService removes a service from the local query cache so the next
// GetService fetches fresh data from the chain — picking up an on-chain
// compute_units_per_relay change. Mirrors InvalidateApplication; called by the
// service cache's force-refresh path so a CUPR change is not frozen for the
// process lifetime.
func (c *serviceQueryClient) InvalidateService(serviceId string) {
	c.serviceCacheMu.Lock()
	defer c.serviceCacheMu.Unlock()
	delete(c.serviceCache, serviceId)
}

// GetServiceRelayDifficulty queries the chain for the LATEST relay mining
// difficulty of a service. It is required by the poktroll
// client.ServiceQueryClient interface, but NOT used by any economic path in
// this repo — claim/proof economics use the height-bound
// GetServiceRelayDifficultyAtHeight to match on-chain validation.
//
// It queries the chain directly with no caching: the previous unkeyed,
// no-TTL difficultyCache was a frozen-latest foot-gun (a future caller would
// silently get a stale value). Difficulty changes over time, so a "latest"
// cache must never be a permanent map.
func (c *serviceQueryClient) GetServiceRelayDifficulty(ctx context.Context, serviceId string) (servicetypes.RelayMiningDifficulty, error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.RelayMiningDifficulty(queryCtx, &servicetypes.QueryGetRelayMiningDifficultyRequest{
		ServiceId: serviceId,
	})
	if err != nil {
		return servicetypes.RelayMiningDifficulty{}, fmt.Errorf("failed to query relay mining difficulty: %w", err)
	}

	recordRelayMiningDifficulty(serviceId, res.RelayMiningDifficulty.TargetHash)
	return res.RelayMiningDifficulty, nil
}

// GetServiceRelayDifficultyAtHeight queries the chain for the relay mining difficulty
// of a service at a specific block height. This is used to get the difficulty that was
// effective at session start, ensuring consistency with on-chain proof validation.
// Results are cached in a bounded xsync.MapOf with composite key "serviceID@blockHeight".
// Difficulty at a given height is immutable — no invalidation needed, only eviction of old entries.
func (c *serviceQueryClient) GetServiceRelayDifficultyAtHeight(ctx context.Context, serviceId string, blockHeight int64) (servicetypes.RelayMiningDifficulty, error) {
	cacheKey := fmt.Sprintf("%s@%d", serviceId, blockHeight)

	// Check cache (lock-free read via xsync.MapOf). Difficulty at a height is
	// immutable, but per the cache-TTL mandate a stale entry past
	// immutableCacheTTLFloor is treated as a miss and re-queried (self-heal floor).
	existing, existed := c.heightDifficultyCache.Load(cacheKey)
	if existed && time.Since(existing.cachedAt) < immutableCacheTTLFloor {
		queryCacheHits.WithLabelValues("service", "difficulty").Inc()
		c.logger.Debug().Str("service_id", serviceId).Int64("block_height", blockHeight).Msg("relay mining difficulty served from L1 cache")
		return existing.difficulty, nil
	}

	queryCacheMisses.WithLabelValues("service", "difficulty").Inc()

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

	// Store overwrites a stale entry (Store, not LoadOrStore, so a past-floor refresh
	// actually replaces the old value); the size counter only grows for a brand-new key.
	entry := heightDifficultyCacheEntry{
		difficulty:  res.RelayMiningDifficulty,
		blockHeight: blockHeight,
		cachedAt:    time.Now(),
	}
	c.heightDifficultyCache.Store(cacheKey, entry)
	if !existed {
		c.heightDifficultyCacheSize.Add(1)
	}
	queryCacheSize.WithLabelValues("service", "difficulty").Set(float64(c.heightDifficultyCacheSize.Load()))
	recordRelayMiningDifficulty(serviceId, res.RelayMiningDifficulty.TargetHash)

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
	// Serve from cache while fresh
	c.paramsCacheMu.RLock()
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		cached := c.paramsCache
		c.paramsCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("service", "params").Inc()
		return cached, nil
	}
	c.paramsCacheMu.RUnlock()

	// Query chain
	c.paramsCacheMu.Lock()
	defer c.paramsCacheMu.Unlock()

	// Double-check after acquiring a lock
	if c.paramsCache != nil && time.Since(c.paramsCacheAt) < liveParamsCacheTTL {
		queryCacheHits.WithLabelValues("service", "params").Inc()
		return c.paramsCache, nil
	}

	queryCacheMisses.WithLabelValues("service", "params").Inc()

	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.Params(queryCtx, &servicetypes.QueryParamsRequest{})
	if err != nil {
		if c.paramsCache != nil {
			c.logger.Warn().Err(err).Msg("service params refresh failed; serving stale cache")
			return c.paramsCache, nil
		}
		return nil, fmt.Errorf("failed to query service params: %w", err)
	}

	c.paramsCache = &res.Params
	c.paramsCacheAt = time.Now()
	queryCacheSize.WithLabelValues("service", "params").Set(1)
	return &res.Params, nil
}

// =============================================================================
// Account Query Client
// =============================================================================

type accountQueryClient struct {
	logger       logging.Logger
	queryClient  accounttypes.QueryClient
	queryTimeout time.Duration

	// In-memory cache for accounts. The cached pubkey + account number are
	// immutable; the sequence is non-load-bearing (txs are unordered). Per the
	// cache-TTL mandate an immutableCacheTTLFloor expires entries anyway so a
	// wrong assumption self-heals instead of needing a restart.
	accountCache   map[string]accountCacheEntry
	accountCacheMu sync.RWMutex
}

var _ client.AccountQueryClient = (*accountQueryClient)(nil)

// accountCacheEntry is a cached account plus its fetch time, for TTL expiry.
type accountCacheEntry struct {
	account  cosmostypes.AccountI
	cachedAt time.Time
}

func newAccountQueryClient(logger logging.Logger, conn *grpc.ClientConn, timeout time.Duration) *accountQueryClient {
	return &accountQueryClient{
		logger:       logger.With().Str("query_client", "account").Logger(),
		queryClient:  accounttypes.NewQueryClient(conn),
		queryTimeout: timeout,
		accountCache: make(map[string]accountCacheEntry),
	}
}

func (c *accountQueryClient) GetAccount(ctx context.Context, address string) (cosmostypes.AccountI, error) {
	// Serve from cache while fresh
	c.accountCacheMu.RLock()
	if e, ok := c.accountCache[address]; ok && time.Since(e.cachedAt) < immutableCacheTTLFloor {
		c.accountCacheMu.RUnlock()
		queryCacheHits.WithLabelValues("account", "entity").Inc()
		return e.account, nil
	}
	c.accountCacheMu.RUnlock()

	// Query chain
	c.accountCacheMu.Lock()
	defer c.accountCacheMu.Unlock()

	// Double-check after acquiring lock
	if e, ok := c.accountCache[address]; ok && time.Since(e.cachedAt) < immutableCacheTTLFloor {
		queryCacheHits.WithLabelValues("account", "entity").Inc()
		return e.account, nil
	}

	queryCacheMisses.WithLabelValues("account", "entity").Inc()

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
		c.accountCache[address] = accountCacheEntry{account: account, cachedAt: time.Now()}
		queryCacheSize.WithLabelValues("account", "entity").Set(float64(len(c.accountCache)))
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
