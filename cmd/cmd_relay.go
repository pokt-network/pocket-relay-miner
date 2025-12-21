package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/cmd/relay"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
)

// relayCmd is the main command for testing relay requests.
var relayCmd = &cobra.Command{
	Use:   "relay [mode]",
	Short: "Send relay requests to a relayer (testing tool)",
	Long: `Send relay requests to a relayer for testing and load testing.

Modes:
  jsonrpc   - Send HTTP/JSONRPC relay requests
  websocket - Send WebSocket relay requests
  grpc      - Send gRPC relay requests
  stream    - Send streaming relay requests (SSE/NDJSON)

Examples:
  # Send a single JSONRPC relay
  pocket-relay-miner relay jsonrpc --app-priv-key <hex> --service develop --node localhost:9090 --chain-id poktroll

  # Load test with 1000 concurrent requests
  pocket-relay-miner relay jsonrpc --load-test -n 1000 --concurrency 100 --app-priv-key <hex> --service develop

  # WebSocket relay test
  pocket-relay-miner relay websocket --app-priv-key <hex> --service develop --relayer-url ws://localhost:8080
`,
	Args: cobra.ExactArgs(1),
	RunE: runRelayCommand,
}

// Relay command flags are now in the relay package (relay.relay.RelayAppPrivKey, etc.)

// Localnet defaults (from tilt/config/all-keys.yaml)
// Genesis has 4 services with corresponding apps:
//   - develop-http      -> app1 (pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4)
//   - develop-websocket -> app2 (pokt184zvylazwu4queyzpl0gyz9yf5yxm2kdhh9hpm)
//   - develop-stream    -> app3 (pokt1lqyu4v88vp8tzc86eaqr4lq8rwhssyn6rfwzex)
//   - develop-grpc      -> app4 (pokt1pn64d94e6u5g8cllsnhgrl6t96ysnjw59j5gst)
//
// All apps delegate to gateway1 (pokt15vzxjqklzjtlz7lahe8z2dfe9nm5vxwwmscne4)
const (
	// App private keys (each app is staked for one service)
	localnetApp1PrivKey = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a" // develop-http
	localnetApp2PrivKey = "7e7571a8c61b0887ff8a9017bb4ad83c016b193234f9dc8b6a8ce10c7c483600" // develop-websocket
	localnetApp3PrivKey = "7cbbaa043b9b63baa7d6bb087483b0a6a9f82596c19dce4c5028eb43e5b63674" // develop-stream
	localnetApp4PrivKey = "84e4f2257f24d9e1517d414b834bbbfa317e0d53fef21c1528a07a5fa8c70d57" // develop-grpc

	// Gateway private key (all apps delegate to this gateway)
	// When --gateway-priv-key is provided, relays are signed with this key on behalf of the app
	// This matches PATH's approach for gateway signing
	localnetGateway1PrivKey = "cf09805c952fa999e9a63a9f434147b0a5abfd10f268879694c6b5a70e1ae177"

	// First supplier address (for relay routing)
	localnetSupplier1Addr = "pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj"

	// Network endpoints
	localnetGRPCEndpoint = "localhost:9090"
	localnetRPCEndpoint  = "http://localhost:26657"
	localnetChainID      = "poktroll"
	localnetRelayerURL   = "http://localhost:8180"
)

// localnetAppKeys maps service IDs to their corresponding app private keys.
// This allows automatic selection of the correct app when --localnet and --service are used together.
var localnetAppKeys = map[string]string{
	"develop-http":      localnetApp1PrivKey,
	"develop-websocket": localnetApp2PrivKey,
	"develop-stream":    localnetApp3PrivKey,
	"develop-grpc":      localnetApp4PrivKey,
}

// validateURL validates a URL and ensures it uses an allowed scheme.
//
// Security checks:
//   - Valid URL format
//   - Scheme is in allowedSchemes (http, https, ws, wss)
//   - No credentials in URL (user:pass@host is rejected)
//   - Hostname is not empty
func validateURL(rawURL string, allowedSchemes []string) error {
	if rawURL == "" {
		return fmt.Errorf("URL cannot be empty")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL format: %w", err)
	}

	// Check for credentials in URL (security risk - credentials in plaintext)
	if parsed.User != nil {
		return fmt.Errorf("credentials in URL are not allowed (use separate authentication)")
	}

	// Validate scheme
	schemeValid := false
	for _, allowed := range allowedSchemes {
		if parsed.Scheme == allowed {
			schemeValid = true
			break
		}
	}
	if !schemeValid {
		return fmt.Errorf("invalid URL scheme %q (allowed: %s)", parsed.Scheme, strings.Join(allowedSchemes, ", "))
	}

	// Ensure hostname is not empty
	if parsed.Host == "" {
		return fmt.Errorf("URL must have a valid host")
	}

	return nil
}

// validateServiceID validates a service ID format.
//
// Service IDs should be alphanumeric with hyphens/underscores only.
func validateServiceID(serviceID string) error {
	if serviceID == "" {
		return fmt.Errorf("service ID cannot be empty")
	}

	// Basic validation: alphanumeric, hyphens, underscores
	for _, ch := range serviceID {
		isLower := ch >= 'a' && ch <= 'z'
		isUpper := ch >= 'A' && ch <= 'Z'
		isDigit := ch >= '0' && ch <= '9'
		isHyphen := ch == '-'
		isUnderscore := ch == '_'
		if !isLower && !isUpper && !isDigit && !isHyphen && !isUnderscore {
			return fmt.Errorf("service ID %q contains invalid character %q (allowed: a-z, A-Z, 0-9, -, _)", serviceID, ch)
		}
	}

	// Length check (reasonable bounds)
	if len(serviceID) > 64 {
		return fmt.Errorf("service ID too long (max 64 characters)")
	}

	return nil
}

// RelayCmd returns the relay command for testing relay requests.
func RelayCmd() *cobra.Command {
	// Localnet mode
	relayCmd.PersistentFlags().BoolVar(&relay.RelayLocalnet, "localnet", false, "Use localnet defaults from tilt/config (auto-selects app for service)")

	// Connection flags
	relayCmd.PersistentFlags().StringVar(&relay.RelayAppPrivKey, "app-priv-key", "", "Application private key (hex)")
	relayCmd.PersistentFlags().StringVar(&relay.RelayGatewayPrivKey, "gateway-priv-key", "", "Gateway private key for ring signing (hex, matches PATH approach)")
	relayCmd.PersistentFlags().StringVar(&relay.RelayServiceID, "service", "", "Service ID (e.g., develop-http, develop-websocket, develop-stream, develop-grpc)")
	relayCmd.PersistentFlags().StringVar(&relay.RelayNodeGRPC, "node", "", "gRPC endpoint for chain queries (e.g., localhost:9090)")
	relayCmd.PersistentFlags().StringVar(&relay.RelayNodeRPC, "node-rpc", "", "CometBFT RPC endpoint for block subscription (e.g., http://localhost:26657)")
	relayCmd.PersistentFlags().StringVar(&relay.RelayChainID, "chain-id", "", "Chain ID (e.g., poktroll)")

	// Optional flags
	relayCmd.PersistentFlags().StringVar(&relay.RelayRelayerURL, "relayer-url", "", "Relayer endpoint URL")
	relayCmd.PersistentFlags().StringVar(&relay.RelaySupplierAddr, "supplier", "", "Supplier operator address")
	relayCmd.PersistentFlags().IntVarP(&relay.RelayCount, "count", "n", 1, "Number of requests to send")
	relayCmd.PersistentFlags().BoolVar(&relay.RelayLoadTest, "load-test", false, "Enable load test mode with concurrency")
	relayCmd.PersistentFlags().IntVar(&relay.RelayConcurrency, "concurrency", 10, "Number of concurrent workers (load test mode)")
	relayCmd.PersistentFlags().IntVar(&relay.RelayRPS, "rps", 0, "Target requests per second (0 = unlimited, only for load test mode)")
	relayCmd.PersistentFlags().StringVar(&relay.RelayPayloadJSON, "payload", "", "Custom JSON-RPC payload (default: eth_blockNumber)")
	relayCmd.PersistentFlags().BoolVar(&relay.RelayOutputJSON, "output-json", false, "Output results as JSON")
	relayCmd.PersistentFlags().IntVar(&relay.RelayTimeout, "timeout", 30, "Request timeout in seconds")
	relayCmd.PersistentFlags().BoolVar(&relay.RelayVerbose, "verbose", false, "Verbose logging")

	return relayCmd
}

// runRelayCommand executes the relay command based on the selected mode.
func runRelayCommand(cmd *cobra.Command, args []string) error {
	mode := args[0]

	// Apply localnet defaults if --localnet flag is set
	if relay.RelayLocalnet {
		// Auto-select app key based on service ID (each app is staked for one service)
		if relay.RelayAppPrivKey == "" && relay.RelayServiceID != "" {
			if appKey, ok := localnetAppKeys[relay.RelayServiceID]; ok {
				relay.RelayAppPrivKey = appKey
			} else {
				// Fallback to app1 for unknown services
				relay.RelayAppPrivKey = localnetApp1PrivKey
			}
		} else if relay.RelayAppPrivKey == "" {
			// No service specified, default to app1
			relay.RelayAppPrivKey = localnetApp1PrivKey
		}

		// Set gateway key for gateway mode (matches PATH's approach)
		// All apps delegate to gateway1, so we can sign relays on their behalf
		if relay.RelayGatewayPrivKey == "" {
			relay.RelayGatewayPrivKey = localnetGateway1PrivKey
		}

		if relay.RelayNodeGRPC == "" {
			relay.RelayNodeGRPC = localnetGRPCEndpoint
		}
		if relay.RelayNodeRPC == "" {
			relay.RelayNodeRPC = localnetRPCEndpoint
		}
		if relay.RelayChainID == "" {
			relay.RelayChainID = localnetChainID
		}
		if relay.RelayRelayerURL == "" {
			relay.RelayRelayerURL = localnetRelayerURL
		}
		if relay.RelaySupplierAddr == "" {
			relay.RelaySupplierAddr = localnetSupplier1Addr
		}
	}

	// Validate required flags
	if relay.RelayAppPrivKey == "" {
		return fmt.Errorf("--app-priv-key is required (or use --localnet)")
	}
	if relay.RelayServiceID == "" {
		return fmt.Errorf("--service is required")
	}
	if relay.RelayNodeGRPC == "" {
		return fmt.Errorf("--node is required (or use --localnet)")
	}
	if relay.RelayChainID == "" {
		return fmt.Errorf("--chain-id is required (or use --localnet)")
	}

	// Validate service ID format (security: prevent injection)
	if err := validateServiceID(relay.RelayServiceID); err != nil {
		return fmt.Errorf("invalid service ID: %w", err)
	}

	// Default relayer URL if not provided
	if relay.RelayRelayerURL == "" {
		relay.RelayRelayerURL = "http://localhost:8080"
	}

	// Default RPC endpoint to gRPC endpoint with different port if not provided
	if relay.RelayNodeRPC == "" {
		// Try to infer from gRPC endpoint (e.g., localhost:9090 -> http://localhost:26657)
		relay.RelayNodeRPC = "http://localhost:26657"
	}

	// Validate URLs (security: prevent SSRF, ensure valid schemes)
	if err := validateURL(relay.RelayRelayerURL, []string{"http", "https", "ws", "wss"}); err != nil {
		return fmt.Errorf("invalid relayer URL: %w", err)
	}
	if err := validateURL(relay.RelayNodeRPC, []string{"http", "https"}); err != nil {
		return fmt.Errorf("invalid node RPC URL: %w", err)
	}

	// Validate mode
	validModes := map[string]bool{
		"jsonrpc":   true,
		"websocket": true,
		"grpc":      true,
		"stream":    true,
	}
	if !validModes[mode] {
		return fmt.Errorf("invalid mode %q. Valid modes: jsonrpc, websocket, grpc, stream", mode)
	}

	// Validate concurrency (security: prevent resource exhaustion)
	if relay.RelayConcurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1")
	}
	if relay.RelayConcurrency > 1000 {
		return fmt.Errorf("concurrency cannot exceed 1000 (risk of resource exhaustion)")
	}

	// Validate request count (security: prevent abuse)
	if relay.RelayCount < 0 {
		return fmt.Errorf("count must be non-negative")
	}
	if relay.RelayCount > 1000000 {
		return fmt.Errorf("count cannot exceed 1,000,000 (risk of resource exhaustion)")
	}

	// Validate timeout (security: prevent infinite hangs)
	if relay.RelayTimeout < 1 {
		return fmt.Errorf("timeout must be at least 1 second")
	}
	if relay.RelayTimeout > 3600 {
		return fmt.Errorf("timeout cannot exceed 3600 seconds (1 hour)")
	}

	// Validate RPS (requests per second) targeting
	if relay.RelayRPS < 0 {
		return fmt.Errorf("RPS cannot be negative")
	}
	if relay.RelayRPS > 10000 {
		return fmt.Errorf("RPS cannot exceed 10,000 (risk of resource exhaustion)")
	}
	if relay.RelayRPS > 0 && !relay.RelayLoadTest {
		return fmt.Errorf("--rps requires --load-test flag (RPS targeting only works in load test mode)")
	}

	// Warn if load test flags are used without --load-test
	if !relay.RelayLoadTest {
		if relay.RelayConcurrency > 1 && cmd.Flags().Changed("concurrency") {
			return fmt.Errorf("--concurrency requires --load-test flag (diagnostic mode only supports single requests)")
		}
		if relay.RelayCount > 1 {
			return fmt.Errorf("-n/--count > 1 requires --load-test flag (diagnostic mode sends exactly 1 relay, use --load-test for multiple requests)")
		}
	}

	// Create logger
	logConfig := logging.DefaultConfig()
	if relay.RelayVerbose {
		logConfig.Level = "debug"
		logConfig.Format = "text"
	} else {
		logConfig.Level = "info"
	}
	logger := logging.NewLoggerFromConfig(logConfig)

	// Create query clients for chain interaction
	queryClients, err := query.NewQueryClients(logger, query.ClientConfig{
		GRPCEndpoint: relay.RelayNodeGRPC,
		UseTLS:       false,
	})
	if err != nil {
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	defer func() { _ = queryClients.Close() }()

	// Create relay client with optional gateway signing mode
	relayClient, err := relay_client.NewRelayClient(relay_client.Config{
		AppPrivateKeyHex:     relay.RelayAppPrivKey,
		GatewayPrivateKeyHex: relay.RelayGatewayPrivKey, // Empty = app mode, set = gateway mode
		QueryClients:         queryClients,
	}, logger)
	if err != nil {
		return fmt.Errorf("failed to create relay client: %w", err)
	}

	// If supplier address not provided, try to detect from relayer
	if relay.RelaySupplierAddr == "" {
		logger.Warn().Msg("--supplier not provided, using placeholder (may not work with all relayers)")
		relay.RelaySupplierAddr = "pokt1placeholder_supplier_address"
	}

	// Log configuration
	logEvent := logger.Info().
		Str("mode", mode).
		Str("app_address", relayClient.GetAppAddress()).
		Str("service", relay.RelayServiceID).
		Str("relayer_url", relay.RelayRelayerURL).
		Int("count", relay.RelayCount).
		Bool("load_test", relay.RelayLoadTest).
		Bool("gateway_mode", relayClient.IsGatewayMode())

	if relayClient.IsGatewayMode() {
		logEvent.Str("signer_address", relayClient.GetSignerAddress())
	}
	logEvent.Msg("starting relay test")

	// Route to appropriate mode handler
	switch mode {
	case "jsonrpc":
		return relay.RunHTTPMode(cmd.Context(), logger, relayClient)
	case "websocket":
		return relay.RunWebSocketMode(cmd.Context(), logger, relayClient)
	case "grpc":
		return relay.RunGRPCMode(cmd.Context(), logger, relayClient)
	case "stream":
		return relay.RunStreamMode(cmd.Context(), logger, relayClient)
	default:
		return fmt.Errorf("mode %q not yet implemented", mode)
	}
}
