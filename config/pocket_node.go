package config

// PocketNodeConfig contains Pocket blockchain connection configuration.
// Shared between miner and relayer for querying the blockchain.
type PocketNodeConfig struct {
	// QueryNodeRPCUrl is the URL for RPC queries and transaction submission.
	// Used for health checks, transaction broadcasting, and fallback queries.
	QueryNodeRPCUrl string `yaml:"query_node_rpc_url"`

	// QueryNodeGRPCUrl is the URL for gRPC queries.
	// Primary interface for chain queries (application, session, service, params, etc.)
	QueryNodeGRPCUrl string `yaml:"query_node_grpc_url"`

	// GRPCInsecure disables TLS for gRPC connections.
	// Default: false (use TLS for secure connections)
	// Only set to true for local development/testing.
	GRPCInsecure bool `yaml:"grpc_insecure,omitempty"`

	// QueryTimeoutSeconds is the timeout for blockchain queries in seconds.
	// Default: 5 seconds
	QueryTimeoutSeconds int `yaml:"query_timeout_seconds,omitempty"`

	// ChainID is the blockchain chain ID used for transaction signing.
	// Required for miner (claim/proof submission).
	// Examples: "pocket" (mainnet), "pocket-beta" (testnet), "pocket" (localnet)
	ChainID string `yaml:"chain_id,omitempty"`
}
