package relay

// Package-level variables for relay command flags.
// These are set by the parent cmd package when parsing flags.
// Exported so cmd package can bind them, and relay package files can use them directly.
var (
	RelayAppPrivKey     string
	RelayGatewayPrivKey string // Gateway private key for ring signing (matches PATH's approach)
	RelayServiceID      string
	RelayNodeGRPC       string
	RelayNodeRPC        string
	RelayChainID        string
	RelayRelayerURL     string
	RelaySupplierAddr   string
	RelayCount          int
	RelayLoadTest       bool
	RelayConcurrency    int
	RelayRPS            int
	RelayPayloadJSON    string
	RelayOutputJSON     bool
	RelayTimeout        int
	RelayVerbose        bool
	RelayLocalnet       bool
)
