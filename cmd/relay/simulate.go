package relay

import (
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// ResolveSimulationFlags validates and defaults the --simulate flag group. It
// is a no-op when --simulate is not set, so a disabled simulate block never
// blocks a normal relay.
//
// Call this after ResolveKeys/--localnet defaulting (RelayAppPrivKey /
// RelayGatewayPrivKey must already hold whatever hex the operator supplied)
// and before any --supplier placeholder fallback, since --simulate requires a
// real supplier (the relayer's SimulationVerifier rejects an empty or unloaded
// supplier operator address).
//
// Defaulting: when the operator does not pass an explicit --sim-app-pubkey /
// --sim-gateway-pubkeys, the corresponding pubkey hex is derived from the
// already-resolved app/gateway private key, so
// `--simulate --sim-key-id X --supplier Y --service Z` works with no flags
// beyond the ones a normal gateway-mode relay already needs.
func ResolveSimulationFlags() error {
	if !RelaySimulate {
		return nil
	}
	if RelaySupplierAddr == "" {
		return fmt.Errorf("--simulate requires --supplier (the simulation identity's supplier)")
	}
	if RelaySimKeyID == "" {
		return fmt.Errorf("--simulate requires --sim-key-id")
	}

	if RelaySimAppPubKey == "" {
		pub, err := relay_client.PubKeyHexFromPrivKeyHex(RelayAppPrivKey)
		if err != nil {
			return fmt.Errorf("derive simulated app pubkey from the resolved app key: %w", err)
		}
		RelaySimAppPubKey = pub
	}

	if len(RelaySimGatewayPubKeys) == 0 {
		if RelayGatewayPrivKey == "" {
			return fmt.Errorf("--simulate requires a gateway key (--gateway-priv-key/--gateway-key/--keys-file) or explicit --sim-gateway-pubkeys")
		}
		pub, err := relay_client.PubKeyHexFromPrivKeyHex(RelayGatewayPrivKey)
		if err != nil {
			return fmt.Errorf("derive simulated gateway pubkey from the resolved gateway key: %w", err)
		}
		RelaySimGatewayPubKeys = []string{pub}
	}

	return nil
}

// simulationHTTPHeader returns the Pocket-Simulation-Key-Id header (key,
// value) to attach to an outgoing HTTP/WebSocket relay request when
// --simulate is active. ok is false when --simulate is not set, telling the
// caller to skip setting the header entirely — its absence is how the
// relayer distinguishes a simulated relay from a normal one.
//
// Shared by the http (jsonrpc + cometbft), stream, and websocket send sites —
// a single source of truth for the header name/value pair so all three stay
// in lockstep with relayer.HeaderSimulationKeyID.
func simulationHTTPHeader() (key, value string, ok bool) {
	if !RelaySimulate {
		return "", "", false
	}
	return relayer.HeaderSimulationKeyID, RelaySimKeyID, true
}

// simulationGRPCMetadataPair returns the pocket-simulation-key-id metadata
// (key, value) to attach to an outgoing gRPC relay when --simulate is active.
// ok is false when --simulate is not set.
func simulationGRPCMetadataPair() (key, value string, ok bool) {
	if !RelaySimulate {
		return "", "", false
	}
	return relayer.MetaSimulationKeyID, RelaySimKeyID, true
}
