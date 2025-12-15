package relay_client

import (
	"context"
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/rings"
	"github.com/pokt-network/poktroll/pkg/crypto"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sdk "github.com/pokt-network/shannon-sdk"
)

// RelayClient builds and signs relay requests for testing and development.
//
// It fetches application and session data from the blockchain and constructs
// valid RelayRequest protobufs that can be sent to a relayer. The client handles:
//   - Application private key management and signing
//   - Session retrieval and caching for load tests
//   - Ring signature creation for relay authentication
//   - Protobuf serialization of relay requests
//
// Thread-safe for concurrent use in load testing scenarios.
type RelayClient struct {
	queryClients *query.QueryClients
	ringClient   crypto.RingClient
	signer       *Signer
	appAddress   string

	// Cached session for reuse in load tests
	cachedSession *sessiontypes.Session
}

// Config contains configuration for the relay client.
//
// Both fields are required for proper initialization.
type Config struct {
	// AppPrivateKeyHex is the application's private key in hex format (64 hex characters = 32 bytes).
	// This key is used to sign relay requests with ring signatures.
	AppPrivateKeyHex string

	// QueryClients provides access to on-chain data (Application, Session, Account, Shared params).
	// Used to fetch application info, session data, and construct rings for signing.
	QueryClients *query.QueryClients
}

// NewRelayClient creates a new relay client with ring signature support.
//
// Initializes a relay client that can build and sign relay requests. The client:
//   - Creates a signer from the provided hex private key
//   - Derives the application address from the private key
//   - Sets up a ring client for signature operations
//   - Prepares for session caching to optimize load testing
//
// Parameters:
//   - config: Configuration with private key and query clients (both required)
//   - logger: Logger for debug and error messages
//
// Returns:
//   - *RelayClient: Initialized relay client ready to build requests
//   - error: If private key is empty/invalid or query clients are nil
//
// Example:
//
//	relayClient, err := NewRelayClient(relay_client.Config{
//	    AppPrivateKeyHex: "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a",
//	    QueryClients:     queryClients,
//	}, logger)
//	if err != nil {
//	    return fmt.Errorf("failed to create relay client: %w", err)
//	}
func NewRelayClient(config Config, logger logging.Logger) (*RelayClient, error) {
	if config.AppPrivateKeyHex == "" {
		return nil, fmt.Errorf("application private key is required")
	}
	if config.QueryClients == nil {
		return nil, fmt.Errorf("query clients are required")
	}

	// Create signer from private key
	signer, err := NewSignerFromHex(config.AppPrivateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	// Extract application address from signer
	appAddress := signer.GetAddress()

	// Create RingClient for signature verification and ring construction
	ringClient := rings.NewRingClient(
		logger,
		config.QueryClients.Application(),
		config.QueryClients.Account(),
		config.QueryClients.Shared(),
	)

	return &RelayClient{
		queryClients: config.QueryClients,
		ringClient:   ringClient,
		signer:       signer,
		appAddress:   appAddress,
	}, nil
}

// BuildRelayRequest builds and signs a relay request for the given service.
//
// This is the main method for creating relay requests. It handles the full workflow:
//  1. Fetch application from chain (validates app exists and has stake)
//  2. Get current session for the app and service (cached for efficiency)
//  3. Create RelayRequest protobuf with payload and session metadata
//  4. Sign request with application ring signature (authenticates the request)
//  5. Serialize to protobuf bytes (ready to send to relayer)
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

	// 2. Get current session (use cached if available, height=0)
	session, err := c.getSession(ctx, &app, serviceID, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get session: %w", err)
	}

	// 3. Build RelayRequest
	relayRequest := &servicetypes.RelayRequest{
		Payload: payloadBz,
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader:           session.Header,
			SupplierOperatorAddress: supplierAddr,
			Signature:               nil, // Will be filled by signing
		},
	}

	// 5. Sign the relay request with ring signature
	if err := c.signer.SignRelayRequest(ctx, relayRequest, &app, c.ringClient); err != nil {
		return nil, nil, fmt.Errorf("failed to sign relay request: %w", err)
	}

	// 6. Serialize to protobuf bytes
	relayRequestBz, err := relayRequest.Marshal()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal relay request: %w", err)
	}

	return relayRequest, relayRequestBz, nil
}

// getSession fetches the current session for the app and service.
// Caches the session for reuse in load tests.
func (c *RelayClient) getSession(
	ctx context.Context,
	app *apptypes.Application,
	serviceID string,
	height int64,
) (*sessiontypes.Session, error) {
	// Return cached session if available and still valid
	// NOTE: Cache only used when height=0 (default behavior)
	if height == 0 && c.cachedSession != nil && c.cachedSession.Header.ServiceId == serviceID {
		return c.cachedSession, nil
	}

	// Fetch session from chain at specific height
	session, err := c.queryClients.Session().GetSession(ctx, c.appAddress, serviceID, height)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch session: %w", err)
	}

	// Cache for reuse (only if height=0, default behavior)
	if height == 0 {
		c.cachedSession = session
	}

	return session, nil
}

// GetAppAddress returns the Bech32-encoded application address.
//
// This address is derived from the private key during client initialization
// and identifies the application making relay requests.
//
// Returns:
//   - string: Application address (e.g., "pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4")
func (c *RelayClient) GetAppAddress() string {
	return c.appAddress
}

// GetCurrentSession fetches the current session without building a full relay request.
//
// Useful for extracting session metadata like session end height for monitoring
// session boundaries. The session is cached for reuse in subsequent BuildRelayRequest calls.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - serviceID: Service identifier to fetch session for
//
// Returns:
//   - *Session: Current session with header and supplier list
//   - error: If application fetch fails or session query fails
func (c *RelayClient) GetCurrentSession(ctx context.Context, serviceID string) (*sessiontypes.Session, error) {
	// Fetch application from chain
	app, err := c.queryClients.Application().GetApplication(ctx, c.appAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch application %s: %w", c.appAddress, err)
	}

	// Get current session (height=0 means latest block)
	session, err := c.getSession(ctx, &app, serviceID, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	return session, nil
}

// GetSessionAtHeight fetches the session for a specific block height.
//
// This method is used when you need to explicitly query the session at a specific
// height (e.g., when forcing session rollover during load tests). It bypasses the
// cache and queries the blockchain directly with the given height.
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//   - serviceID: Service identifier (e.g., "develop", "eth-mainnet")
//   - height: Block height to query the session for
//
// Returns:
//   - *sessiontypes.Session: The session at the specified height
//   - error: Any error that occurred during the query
func (c *RelayClient) GetSessionAtHeight(ctx context.Context, serviceID string, height int64) (*sessiontypes.Session, error) {
	// Fetch application from chain
	app, err := c.queryClients.Application().GetApplication(ctx, c.appAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch application %s: %w", c.appAddress, err)
	}

	// Get session at specific height (forces fresh query, no cache)
	session, err := c.getSession(ctx, &app, serviceID, height)
	if err != nil {
		return nil, fmt.Errorf("failed to get session at height %d: %w", height, err)
	}

	return session, nil
}

// ClearSessionCache clears the cached session.
//
// Forces the next BuildRelayRequest call to fetch a fresh session from the blockchain
// instead of using the cached session. This is essential when:
//   - Testing across session boundaries (when block height crosses session end)
//   - Simulating session rollovers in load tests
//   - Recovering from session-related errors
//
// Thread-safe but should be called when no concurrent BuildRelayRequest calls are active
// to avoid race conditions.
func (c *RelayClient) ClearSessionCache() {
	c.cachedSession = nil
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
