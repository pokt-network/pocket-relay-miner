package relay

// Package-level variables for relay command flags.
// These are set by the parent cmd package when parsing flags.
// Exported so cmd package can bind them, and relay package files can use them directly.
var (
	RelayAppPrivKey     string
	RelayGatewayPrivKey string // Gateway private key for ring signing (matches PATH's approach)
	// Key sources that avoid raw hex on the command line (resolved to the hex
	// fields above, in memory, before signing). See resolveRelayKeys.
	RelayKeyringBackend string // Cosmos keyring backend: file|os|test
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

	// Simulated-relay flags: fire a real, ring-signed relay verified by the
	// relayer's SimulationVerifier against a config-pinned ring instead of an
	// on-chain session (served but never charged). See simulate.go.
	RelaySimulate          bool     // --simulate: use the simulated-relay path
	RelaySimKeyID          string   // --sim-key-id: simulation identity pinned in the relayer's config
	RelaySimAppPubKey      string   // --sim-app-pubkey: default derived from the resolved app key
	RelaySimGatewayPubKeys []string // --sim-gateway-pubkeys: default derived from the resolved gateway key
)
