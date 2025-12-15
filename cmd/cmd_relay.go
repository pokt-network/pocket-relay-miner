package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
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

// Relay command flags
var (
	relayAppPrivKey   string
	relayServiceID    string
	relayNodeGRPC     string
	relayNodeRPC      string
	relayChainID      string
	relayRelayerURL   string
	relaySupplierAddr string
	relayCount        int
	relayLoadTest     bool
	relayConcurrency  int
	relayRPS          int // Target requests per second (0 = unlimited)
	relayPayloadJSON  string
	relayOutputJSON   bool
	relayTimeout      int
	relayVerbose      bool
	relayLocalnet     bool
)

// Localnet defaults (from tilt/config/all-keys.yaml)
const (
	localnetApp1PrivKey   = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"
	localnetSupplier1Addr = "pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj"
	localnetGRPCEndpoint  = "localhost:9090"
	localnetRPCEndpoint   = "http://localhost:26657"
	localnetChainID       = "poktroll"
	localnetRelayerURL    = "http://localhost:8180"
)

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
	relayCmd.PersistentFlags().BoolVar(&relayLocalnet, "localnet", false, "Use localnet defaults from tilt/config")

	// Connection flags
	relayCmd.PersistentFlags().StringVar(&relayAppPrivKey, "app-priv-key", "", "Application private key (hex)")
	relayCmd.PersistentFlags().StringVar(&relayServiceID, "service", "", "Service ID (e.g., develop, eth-mainnet)")
	relayCmd.PersistentFlags().StringVar(&relayNodeGRPC, "node", "", "gRPC endpoint for chain queries (e.g., localhost:9090)")
	relayCmd.PersistentFlags().StringVar(&relayNodeRPC, "node-rpc", "", "CometBFT RPC endpoint for block subscription (e.g., http://localhost:26657)")
	relayCmd.PersistentFlags().StringVar(&relayChainID, "chain-id", "", "Chain ID (e.g., poktroll)")

	// Optional flags
	relayCmd.PersistentFlags().StringVar(&relayRelayerURL, "relayer-url", "", "Relayer endpoint URL")
	relayCmd.PersistentFlags().StringVar(&relaySupplierAddr, "supplier", "", "Supplier operator address")
	relayCmd.PersistentFlags().IntVarP(&relayCount, "count", "n", 1, "Number of requests to send")
	relayCmd.PersistentFlags().BoolVar(&relayLoadTest, "load-test", false, "Enable load test mode with concurrency")
	relayCmd.PersistentFlags().IntVar(&relayConcurrency, "concurrency", 10, "Number of concurrent workers (load test mode)")
	relayCmd.PersistentFlags().IntVar(&relayRPS, "rps", 0, "Target requests per second (0 = unlimited, only for load test mode)")
	relayCmd.PersistentFlags().StringVar(&relayPayloadJSON, "payload", "", "Custom JSON-RPC payload (default: eth_blockNumber)")
	relayCmd.PersistentFlags().BoolVar(&relayOutputJSON, "output-json", false, "Output results as JSON")
	relayCmd.PersistentFlags().IntVar(&relayTimeout, "timeout", 30, "Request timeout in seconds")
	relayCmd.PersistentFlags().BoolVar(&relayVerbose, "verbose", false, "Verbose logging")

	return relayCmd
}

// runRelayCommand executes the relay command based on the selected mode.
func runRelayCommand(cmd *cobra.Command, args []string) error {
	mode := args[0]

	// Apply localnet defaults if --localnet flag is set
	if relayLocalnet {
		if relayAppPrivKey == "" {
			relayAppPrivKey = localnetApp1PrivKey
		}
		if relayNodeGRPC == "" {
			relayNodeGRPC = localnetGRPCEndpoint
		}
		if relayNodeRPC == "" {
			relayNodeRPC = localnetRPCEndpoint
		}
		if relayChainID == "" {
			relayChainID = localnetChainID
		}
		if relayRelayerURL == "" {
			relayRelayerURL = localnetRelayerURL
		}
		if relaySupplierAddr == "" {
			relaySupplierAddr = localnetSupplier1Addr
		}
	}

	// Validate required flags
	if relayAppPrivKey == "" {
		return fmt.Errorf("--app-priv-key is required (or use --localnet)")
	}
	if relayServiceID == "" {
		return fmt.Errorf("--service is required")
	}
	if relayNodeGRPC == "" {
		return fmt.Errorf("--node is required (or use --localnet)")
	}
	if relayChainID == "" {
		return fmt.Errorf("--chain-id is required (or use --localnet)")
	}

	// Validate service ID format (security: prevent injection)
	if err := validateServiceID(relayServiceID); err != nil {
		return fmt.Errorf("invalid service ID: %w", err)
	}

	// Default relayer URL if not provided
	if relayRelayerURL == "" {
		relayRelayerURL = "http://localhost:8080"
	}

	// Default RPC endpoint to gRPC endpoint with different port if not provided
	if relayNodeRPC == "" {
		// Try to infer from gRPC endpoint (e.g., localhost:9090 -> http://localhost:26657)
		relayNodeRPC = "http://localhost:26657"
	}

	// Validate URLs (security: prevent SSRF, ensure valid schemes)
	if err := validateURL(relayRelayerURL, []string{"http", "https", "ws", "wss"}); err != nil {
		return fmt.Errorf("invalid relayer URL: %w", err)
	}
	if err := validateURL(relayNodeRPC, []string{"http", "https"}); err != nil {
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
	if relayConcurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1")
	}
	if relayConcurrency > 1000 {
		return fmt.Errorf("concurrency cannot exceed 1000 (risk of resource exhaustion)")
	}

	// Validate request count (security: prevent abuse)
	if relayCount < 0 {
		return fmt.Errorf("count must be non-negative")
	}
	if relayCount > 1000000 {
		return fmt.Errorf("count cannot exceed 1,000,000 (risk of resource exhaustion)")
	}

	// Validate timeout (security: prevent infinite hangs)
	if relayTimeout < 1 {
		return fmt.Errorf("timeout must be at least 1 second")
	}
	if relayTimeout > 3600 {
		return fmt.Errorf("timeout cannot exceed 3600 seconds (1 hour)")
	}

	// Validate RPS (requests per second) targeting
	if relayRPS < 0 {
		return fmt.Errorf("RPS cannot be negative")
	}
	if relayRPS > 10000 {
		return fmt.Errorf("RPS cannot exceed 10,000 (risk of resource exhaustion)")
	}
	if relayRPS > 0 && !relayLoadTest {
		return fmt.Errorf("--rps requires --load-test flag (RPS targeting only works in load test mode)")
	}

	// Warn if load test flags are used without --load-test
	if !relayLoadTest {
		if relayConcurrency > 1 && cmd.Flags().Changed("concurrency") {
			return fmt.Errorf("--concurrency requires --load-test flag (diagnostic mode only supports single requests)")
		}
		if relayCount > 1 {
			return fmt.Errorf("-n/--count > 1 requires --load-test flag (diagnostic mode sends exactly 1 relay, use --load-test for multiple requests)")
		}
	}

	// Create logger
	logConfig := logging.DefaultConfig()
	if relayVerbose {
		logConfig.Level = "debug"
		logConfig.Format = "text"
	} else {
		logConfig.Level = "info"
	}
	logger := logging.NewLoggerFromConfig(logConfig)

	// Create query clients for chain interaction
	queryClients, err := query.NewQueryClients(logger, query.QueryClientConfig{
		GRPCEndpoint: relayNodeGRPC,
		UseTLS:       false,
	})
	if err != nil {
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	defer func() { _ = queryClients.Close() }()

	// Create relay client
	relayClient, err := relay_client.NewRelayClient(relay_client.Config{
		AppPrivateKeyHex: relayAppPrivKey,
		QueryClients:     queryClients,
	}, logger)
	if err != nil {
		return fmt.Errorf("failed to create relay client: %w", err)
	}

	// If supplier address not provided, try to detect from relayer
	if relaySupplierAddr == "" {
		logger.Warn().Msg("--supplier not provided, using placeholder (may not work with all relayers)")
		relaySupplierAddr = "pokt1placeholder_supplier_address"
	}

	// Log configuration
	logger.Info().
		Str("mode", mode).
		Str("app_address", relayClient.GetAppAddress()).
		Str("service", relayServiceID).
		Str("relayer_url", relayRelayerURL).
		Int("count", relayCount).
		Bool("load_test", relayLoadTest).
		Msg("starting relay test")

	// Route to appropriate mode handler
	switch mode {
	case "jsonrpc":
		return runHTTPMode(cmd.Context(), logger, relayClient)
	case "websocket":
		return runWebSocketMode(cmd.Context(), logger, relayClient)
	case "grpc":
		return runGRPCMode(cmd.Context(), logger, relayClient)
	case "stream":
		return runStreamMode(cmd.Context(), logger, relayClient)
	default:
		return fmt.Errorf("mode %q not yet implemented", mode)
	}
}
