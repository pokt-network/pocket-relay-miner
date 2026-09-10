package relay_client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"

	"github.com/pokt-network/ring-go"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/rings"
	"github.com/pokt-network/poktroll/pkg/crypto"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sdk "github.com/pokt-network/shannon-sdk"
)

// ringCacheKey identifies a cached ring by app address and session end height.
// This matches PATH's caching approach where rings are valid for an entire session.
type ringCacheKey struct {
	appAddress       string
	sessionEndHeight int64
}

// RelayClient builds and signs relay requests for testing and development.
//
// It fetches application and session data from the blockchain and constructs
// valid RelayRequest protobufs that can be sent to a relayer. The client handles:
//   - Application private key management and signing
//   - Gateway signing mode (sign with gateway key on behalf of app)
//   - Session and ring caching for optimal load test performance
//   - Ring signature creation for relay authentication
//   - Protobuf serialization of relay requests
//
// Thread-safe for concurrent use in load testing scenarios.
type RelayClient struct {
	queryClients *query.Clients
	ringClient   crypto.RingClient
	signer       *Signer
	appAddress   string

	// gatewayMode indicates whether we're signing with a gateway key on behalf of the app.
	// This matches PATH's approach where gateway signs relays for delegated apps.
	gatewayMode bool

	// sessions answers which session the app is in at a height. It is always
	// asked with the chain's current height, never 0: the query client caches a
	// session under the start height of the height it was asked for, so height
	// 0 is one constant key that keeps answering the first session it saw for
	// as long as that cache keeps it, long after the session ended.
	sessions sessionGetter

	// height is the chain's current height, read at most once per maxAge.
	height *latestHeight

	// ringCache stores rings keyed by (appAddress, sessionEndHeight).
	// This matches PATH's caching approach where rings are built once per session
	// and reused for all requests within that session.
	// Thread-safe via sync.Map for concurrent load testing.
	ringCache *xsync.Map[ringCacheKey, *ring.Ring]

	// simRingCache stores rings for the simulated-relay path, keyed by the
	// pinned pubkeys they are built from (simRingCacheKey). Separate from
	// ringCache because a pinned ring has no session to scope it: it is
	// valid for as long as the pubkeys are, so it is built once per identity
	// and reused for every simulated relay. See simRingFor for why building
	// it per call is expensive.
	// Thread-safe via sync.Map for concurrent load testing.
	simRingCache *xsync.Map[string, *simPinnedRing]
}

// Config contains configuration for the relay client.
//
// AppPrivateKeyHex and QueryClients are required.
// GatewayPrivateKeyHex is optional - when provided, enables gateway signing mode.
type Config struct {
	// AppPrivateKeyHex is the application's private key in hex format (64 hex characters = 32 bytes).
	// Used to derive the app address for session/ring construction.
	// When GatewayPrivateKeyHex is not provided, this key is used for signing.
	AppPrivateKeyHex string

	// GatewayPrivateKeyHex is the gateway's private key in hex format (optional).
	// When provided, enables "gateway mode" matching PATH's approach:
	//   - The gateway signs relay requests on behalf of the application
	//   - The ring is still constructed from app + delegated gateways
	//   - The gateway must be in app.DelegateeGatewayAddresses to be valid
	//
	// This allows testing the full PATH-compatible signing flow where gateways
	// sign relays for their delegated applications.
	GatewayPrivateKeyHex string

	// QueryClients provides access to on-chain data (Application, Session, Account, Shared params).
	// Used to fetch application info, session data, and construct rings for signing.
	QueryClients *query.Clients
}

// NewRelayClient creates a new relay client with ring signature support.
//
// Initializes a relay client that can build and sign relay requests. The client:
//   - Creates a signer from the provided hex private key (app or gateway)
//   - Derives the application address from the app private key
//   - Sets up a ring client for signature operations
//   - Reads sessions at the chain's current height, so a long run follows them
//
// Gateway Mode (when GatewayPrivateKeyHex is provided):
//   - The gateway's private key is used for signing
//   - The app's address is still used for session/ring construction
//   - This matches PATH's approach where gateways sign for delegated apps
//
// Parameters:
//   - config: Configuration with private key and query clients (both required)
//   - logger: Logger for debug and error messages
//
// Returns:
//   - *RelayClient: Initialized relay client ready to build requests
//   - error: If private key is empty/invalid or query clients are nil
//
// Example (app mode):
//
//	relayClient, err := NewRelayClient(relay_client.Config{
//	    AppPrivateKeyHex: "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a",
//	    QueryClients:     queryClients,
//	}, logger)
//
// Example (gateway mode - matches PATH):
//
//	relayClient, err := NewRelayClient(relay_client.Config{
//	    AppPrivateKeyHex:     "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a",
//	    GatewayPrivateKeyHex: "cf09805c952fa999e9a63a9f434147b0a5abfd10f268879694c6b5a70e1ae177",
//	    QueryClients:         queryClients,
//	}, logger)
func NewRelayClient(config Config, logger logging.Logger) (*RelayClient, error) {
	if config.AppPrivateKeyHex == "" {
		return nil, fmt.Errorf("application private key is required")
	}
	if config.QueryClients == nil {
		return nil, fmt.Errorf("query clients are required")
	}

	// Create signer from app private key to derive app address
	appSigner, err := NewSignerFromHex(config.AppPrivateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to create app signer: %w", err)
	}

	// Extract application address from app signer
	appAddress := appSigner.GetAddress()

	// Determine which signer to use for signing relay requests
	var signer *Signer
	gatewayMode := false

	if config.GatewayPrivateKeyHex != "" {
		// Gateway mode: sign with gateway key on behalf of app (matches PATH)
		gatewaySigner, err := NewSignerFromHex(config.GatewayPrivateKeyHex)
		if err != nil {
			return nil, fmt.Errorf("failed to create gateway signer: %w", err)
		}
		signer = gatewaySigner
		gatewayMode = true

		logger.Info().
			Str("app_address", appAddress).
			Str("gateway_address", gatewaySigner.GetAddress()).
			Msg("gateway mode enabled - signing relays with gateway key on behalf of app")
	} else {
		// Standard mode: sign with app key
		signer = appSigner
	}

	// Create RingClient for signature verification and ring construction
	ringClient := rings.NewRingClient(
		logger,
		config.QueryClients.Application(),
		config.QueryClients.Account(),
		config.QueryClients.Shared(),
	)

	blocks := cmtservice.NewServiceClient(config.QueryClients.GRPCConnection())

	return &RelayClient{
		ringCache:    xsync.NewMap[ringCacheKey, *ring.Ring](),
		simRingCache: xsync.NewMap[string, *simPinnedRing](),
		queryClients: config.QueryClients,
		ringClient:   ringClient,
		signer:       signer,
		appAddress:   appAddress,
		gatewayMode:  gatewayMode,
		sessions:     config.QueryClients.Session(),
		height: &latestHeight{
			maxAge: heightMaxAge,
			fetch: func(ctx context.Context) (int64, error) {
				res, err := blocks.GetLatestBlock(ctx, &cmtservice.GetLatestBlockRequest{})
				if err != nil {
					return 0, err
				}
				header := res.GetSdkBlock().GetHeader()
				return header.GetHeight(), nil
			},
		},
	}, nil
}

// heightMaxAge is how long a read of the chain's height is reused: a load test
// asks the node once per maxAge whatever its rate, and after a session border
// it can keep signing for the session that just ended for up to maxAge.
const heightMaxAge = time.Second

// sessionGetter is what RelayClient asks of a session query client.
type sessionGetter interface {
	GetSession(ctx context.Context, appAddress, serviceID string, height int64) (*sessiontypes.Session, error)
}

// latestHeight is the chain's current height, read from the node at most once
// per maxAge and shared by every concurrent relay build.
type latestHeight struct {
	fetch  func(ctx context.Context) (int64, error)
	maxAge time.Duration

	mu     sync.Mutex
	height int64
	readAt time.Time
}

// get returns the current height; it never returns 0 without an error. A failed
// read keeps answering the last height it had, and is retried after maxAge
// rather than on every call, so a node blip does not turn into one query per
// relay.
func (h *latestHeight) get(ctx context.Context) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.height > 0 && time.Since(h.readAt) < h.maxAge {
		return h.height, nil
	}
	height, err := h.fetch(ctx)
	h.readAt = time.Now()
	if err == nil && height > 0 {
		h.height = height
		return height, nil
	}
	if h.height > 0 {
		return h.height, nil
	}
	if err == nil {
		err = fmt.Errorf("the node reported height %d", height)
	}
	return 0, fmt.Errorf("failed to read the chain's current height: %w", err)
}

// BuildRelayRequest builds and signs a relay request for the given service.
//
// This is the main method for creating relay requests. It handles the full workflow:
//  1. Fetch application from chain (validates app exists and has stake)
//  2. Get current session for the app and service (cached for efficiency)
//  3. Get or create ring for the session (cached per session for performance)
//  4. Create RelayRequest protobuf with payload and session metadata
//  5. Sign request with cached ring signature (authenticates the request)
//  6. Serialize to protobuf bytes (ready to send to relayer)
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - serviceID: Service identifier (e.g., "develop", "eth-mainnet")
//   - supplierAddr: Supplier operator address (relayer endpoint)
//   - payloadBz: Pre-serialized payload bytes (POKTHTTPRequest)
//
// Returns:
//   - *RelayRequest: The signed relay request protobuf
//   - []byte: Serialized relay request bytes (ready to send)
//   - error: If app fetch fails, session fetch fails, or signing fails
//
// Note: The payloadBz should be JSON-RPC or other protocol payload bytes.
// For HTTP requests, use sdktypes.SerializeHTTPRequest() to create it.
//
// Thread-safe: Can be called concurrently in load tests.
// Ring caching ensures minimal crypto overhead across requests within a session.
func (c *RelayClient) BuildRelayRequest(
	ctx context.Context,
	serviceID string,
	supplierAddr string,
	payloadBz []byte,
) (*servicetypes.RelayRequest, []byte, error) {
	// 1. Fetch application from chain
	app, err := c.queryClients.Application().GetApplication(ctx, c.appAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch application %s: %w", c.appAddress, err)
	}

	// 2. Get the session at the chain's current height
	session, err := c.currentSession(ctx, serviceID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get session: %w", err)
	}

	// 3. Get or create ring for this session (cached for performance)
	sessionEndHeight := session.Header.SessionEndBlockHeight
	appRing, err := c.getOrCreateRing(ctx, &app, sessionEndHeight)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get ring: %w", err)
	}

	// 4. Build RelayRequest
	relayRequest := &servicetypes.RelayRequest{
		Payload: payloadBz,
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader:           session.Header,
			SupplierOperatorAddress: supplierAddr,
			Signature:               nil, // Will be filled by signing
		},
	}

	// 5. Sign the relay request with cached ring
	if err := c.signer.SignRelayRequestWithRing(relayRequest, appRing); err != nil {
		return nil, nil, fmt.Errorf("failed to sign relay request: %w", err)
	}

	// 6. Serialize to protobuf bytes
	relayRequestBz, err := relayRequest.Marshal()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal relay request: %w", err)
	}

	return relayRequest, relayRequestBz, nil
}

// getOrCreateRing returns a cached ring or creates a new one for the given app and session.
// This matches PATH's caching approach where rings are built once per session and reused.
// Thread-safe via sync.Map for concurrent load testing.
func (c *RelayClient) getOrCreateRing(
	ctx context.Context,
	app *apptypes.Application,
	sessionEndHeight int64,
) (*ring.Ring, error) {
	// Create cache key
	cacheKey := ringCacheKey{
		appAddress:       app.Address,
		sessionEndHeight: sessionEndHeight,
	}

	// Check cache first (fast path)
	if cached, ok := c.ringCache.Load(cacheKey); ok {
		return cached, nil
	}

	// Cache miss - build ring from app's address (not signer's address)
	// This is critical for gateway mode where the signer is a gateway
	newRing, err := c.ringClient.GetRingForAddressAtHeight(ctx, app.Address, sessionEndHeight)
	if err != nil {
		return nil, fmt.Errorf("failed to build ring for app %s at height %d: %w",
			app.Address, sessionEndHeight, err)
	}

	// Store in cache (LoadOrStore handles race conditions)
	actual, _ := c.ringCache.LoadOrStore(cacheKey, newRing)
	return actual, nil
}

// currentSession returns the app's session for serviceID at the chain's current
// height. A run longer than a session moves to the next one as soon as the
// height crosses the border.
func (c *RelayClient) currentSession(ctx context.Context, serviceID string) (*sessiontypes.Session, error) {
	height, err := c.height.get(ctx)
	if err != nil {
		return nil, err
	}
	session, err := c.sessions.GetSession(ctx, c.appAddress, serviceID, height)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch session at height %d: %w", height, err)
	}
	return session, nil
}

// GetAppAddress returns the Bech32-encoded application address.
//
// This address is derived from the app private key during client initialization
// and identifies the application making relay requests.
// In gateway mode, this is still the app's address (not the gateway's).
//
// Returns:
//   - string: Application address (e.g., "pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4")
func (c *RelayClient) GetAppAddress() string {
	return c.appAddress
}

// IsGatewayMode returns true if the client is configured to sign with a gateway key.
// This matches PATH's approach where gateways sign relays on behalf of delegated apps.
func (c *RelayClient) IsGatewayMode() bool {
	return c.gatewayMode
}

// GetSignerAddress returns the address of the key used for signing.
// In gateway mode, this returns the gateway address.
// In standard mode, this returns the app address.
func (c *RelayClient) GetSignerAddress() string {
	return c.signer.GetAddress()
}

// VerifyRelayResponse verifies the supplier's signature on the relay response.
//
// Validates that the relay response was signed by the specified supplier using the
// shannon-sdk's ValidateRelayResponse function. This ensures the response is authentic
// and hasn't been tampered with.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - supplierAddr: Expected supplier operator address
//   - relayResponseBz: Serialized relay response bytes
//
// Returns:
//   - *RelayResponse: Validated and parsed relay response
//   - error: If signature verification fails or unmarshaling fails
//
// Security: This is critical for ensuring relay integrity. Always verify responses
// in production environments to prevent malicious relayers from forging responses.
func (c *RelayClient) VerifyRelayResponse(
	ctx context.Context,
	supplierAddr string,
	relayResponseBz []byte,
) (*servicetypes.RelayResponse, error) {
	// Create account client adapter for shannon-sdk
	accountClient := c.queryClients.Account()

	// Validate relay response using shannon-sdk
	relayResponse, err := sdk.ValidateRelayResponse(
		ctx,
		sdk.SupplierAddress(supplierAddr),
		relayResponseBz,
		accountClient,
	)
	if err != nil {
		return nil, fmt.Errorf("signature verification failed: %w", err)
	}

	return relayResponse, nil
}

// SessionSupplierAddresses returns the operator addresses of every supplier
// in the app's current session for serviceID. Load tests use this to spread
// relays across the whole session instead of exhausting a single supplier's
// per-session claimable budget.
func (c *RelayClient) SessionSupplierAddresses(ctx context.Context, serviceID string) ([]string, error) {
	session, err := c.currentSession(ctx, serviceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	addrs := make([]string, 0, len(session.Suppliers))
	for _, s := range session.Suppliers {
		if s != nil && s.OperatorAddress != "" {
			addrs = append(addrs, s.OperatorAddress)
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("session for service %s has no suppliers", serviceID)
	}
	return addrs, nil
}
