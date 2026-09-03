package relay

// Package-level variables for relay command flags.
// These are set by the parent cmd package when parsing flags.
// Exported so cmd package can bind them, and relay package files can use them directly.
var (
	RelayAppPrivKey     string
	RelayGatewayPrivKey string // Gateway private key for ring signing (matches PATH's approach)
	// Key sources that avoid raw hex on the command line (resolved to the hex
	// fields above, in memory, before signing). See resolveRelayKeys.
	RelayKeyringBackend string // Cosmos keyring backend: file|test (keys.ValidateKeyringBackend)
	RelayKeyringDir     string // Cosmos keyring directory
	RelayAppKeyName     string // keyring key name for the application
	RelayGatewayKeyName string // keyring key name for the gateway
	RelayKeysFile       string // path to a YAML keys file (applications:[hex] + gateway:[hex])
	RelayServiceID      string
	RelayNodeGRPC       string
	RelayGRPCTLS        bool // TLS for the --node gRPC connection (beta/mainnet use TLS on :443)
	RelayNodeRPC        string
	RelayChainID        string
	RelayRelayerURL     string
	RelaySupplierAddr   string
	RelayCount          int
	RelayBatches        int
	RelayLoadTest       bool
	RelayConcurrency    int
	RelayRPS            int
	RelayPayloadJSON    string
	RelayOutputJSON     bool
	RelayTimeout        int
	RelayVerbose        bool
	RelayLocalnet       bool
	RelayAllSuppliers   bool

	// WebSocket handshake shape and adversarial cases. The two gateways in the
	// wild do NOT send the same handshake, and until this existed the CLI could
	// only produce one of them: it always names the supplier up front, which is
	// PATH's shape (v2). Sage sends only Target-Service-Id, App-Address and
	// Rpc-Type, so the supplier arrives inside the first RelayRequest instead
	// (v1) -- measured 2026-09-03 in both repos. That is the path the relayer's
	// owner-adoption rule exists for, and no gate at any level reached it.
	RelayWSHandshake string // --ws-handshake: v1 (no supplier header) or v2
	RelayWSCase      string // --ws-case: one adversarial scenario, asserted

	// gRPC-mode targeting. Without these, `relay grpc` can only reach the
	// localnet demo backend, so the one transport this CLI could not validate
	// against a real backend was gRPC -- recorded during the beta/pnf bring-up,
	// where confirming the transport meant reading the relayer's Prometheus
	// counters instead of a signed relay.
	RelayGRPCMethod     string // --grpc-method: /package.Service/Method to invoke
	RelayGRPCRequestHex string // --grpc-request-hex: protobuf request body, hex; empty = a request with no fields

	// Simulated-relay flags: fire a real, ring-signed relay verified by the
	// relayer's SimulationVerifier against a config-pinned ring instead of an
	// on-chain session (served but never charged). See simulate.go.
	RelaySimulate          bool     // --simulate: use the simulated-relay path
	RelaySimKeyID          string   // --sim-key-id: simulation identity pinned in the relayer's config
	RelaySimAppPubKey      string   // --sim-app-pubkey: default derived from the resolved app key
	RelaySimGatewayPubKeys []string // --sim-gateway-pubkeys: default derived from the resolved gateway key
)
