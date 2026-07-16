package relay

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// resetSimulationFlags clears every --simulate-related package var so each
// test starts from a clean slate, restoring the originals afterward. Mirrors
// resetKeyFlags in keys_test.go.
func resetSimulationFlags(t *testing.T) {
	t.Helper()
	origSimulate := RelaySimulate
	origKeyID := RelaySimKeyID
	origAppPub := RelaySimAppPubKey
	origGwPubs := RelaySimGatewayPubKeys
	origSupplier := RelaySupplierAddr
	origAppPriv := RelayAppPrivKey
	origGwPriv := RelayGatewayPrivKey

	RelaySimulate = false
	RelaySimKeyID = ""
	RelaySimAppPubKey = ""
	RelaySimGatewayPubKeys = nil
	RelaySupplierAddr = ""
	RelayAppPrivKey = ""
	RelayGatewayPrivKey = ""

	t.Cleanup(func() {
		RelaySimulate = origSimulate
		RelaySimKeyID = origKeyID
		RelaySimAppPubKey = origAppPub
		RelaySimGatewayPubKeys = origGwPubs
		RelaySupplierAddr = origSupplier
		RelayAppPrivKey = origAppPriv
		RelayGatewayPrivKey = origGwPriv
	})
}

// TestResolveSimulationFlags_NotSimulateIsNoop proves --simulate off leaves
// every sim field untouched, so a disabled simulate block never blocks (or
// mutates flags for) a normal relay.
func TestResolveSimulationFlags_NotSimulateIsNoop(t *testing.T) {
	resetSimulationFlags(t)

	require.NoError(t, ResolveSimulationFlags())

	require.Empty(t, RelaySimAppPubKey)
	require.Empty(t, RelaySimGatewayPubKeys)
}

// TestResolveSimulationFlags_MissingSupplierErrors proves --simulate without
// --supplier is rejected with a clear message, BEFORE the placeholder
// supplier fallback in cmd_relay.go would otherwise mask it.
func TestResolveSimulationFlags_MissingSupplierErrors(t *testing.T) {
	resetSimulationFlags(t)
	RelaySimulate = true
	RelaySimKeyID = "k1"

	err := ResolveSimulationFlags()

	require.Error(t, err)
	require.Contains(t, err.Error(), "--supplier")
}

// TestResolveSimulationFlags_MissingKeyIDErrors proves --simulate without
// --sim-key-id is rejected.
func TestResolveSimulationFlags_MissingKeyIDErrors(t *testing.T) {
	resetSimulationFlags(t)
	RelaySimulate = true
	RelaySupplierAddr = "pokt1supplier"

	err := ResolveSimulationFlags()

	require.Error(t, err)
	require.Contains(t, err.Error(), "--sim-key-id")
}

// TestResolveSimulationFlags_DefaultsPubKeysFromResolvedKeys proves the
// ergonomic path: with only the normal --app-priv-key/--gateway-priv-key
// (already resolved into RelayAppPrivKey/RelayGatewayPrivKey by ResolveKeys
// or --localnet) plus --simulate/--sim-key-id/--supplier, the sim pubkeys are
// derived automatically with no extra flags.
func TestResolveSimulationFlags_DefaultsPubKeysFromResolvedKeys(t *testing.T) {
	resetSimulationFlags(t)
	RelaySimulate = true
	RelaySimKeyID = "k1"
	RelaySupplierAddr = "pokt1supplier"
	RelayAppPrivKey = validAppHex
	RelayGatewayPrivKey = validGatewayHex

	require.NoError(t, ResolveSimulationFlags())

	require.NotEmpty(t, RelaySimAppPubKey)
	require.Len(t, RelaySimGatewayPubKeys, 1)
	require.NotEmpty(t, RelaySimGatewayPubKeys[0])

	// Defaulting must be deterministic: re-running with the same resolved
	// keys reproduces the same pubkeys (proves it derives, not randomizes).
	wantApp := RelaySimAppPubKey
	wantGw := RelaySimGatewayPubKeys[0]
	RelaySimAppPubKey = ""
	RelaySimGatewayPubKeys = nil
	require.NoError(t, ResolveSimulationFlags())
	require.Equal(t, wantApp, RelaySimAppPubKey)
	require.Equal(t, []string{wantGw}, RelaySimGatewayPubKeys)
}

// TestResolveSimulationFlags_ExplicitPubKeysNotOverwritten proves an operator
// who passes explicit --sim-app-pubkey/--sim-gateway-pubkeys is honored
// verbatim, even though a resolved app/gateway key is also present — the
// explicit override always wins over the default.
func TestResolveSimulationFlags_ExplicitPubKeysNotOverwritten(t *testing.T) {
	resetSimulationFlags(t)
	RelaySimulate = true
	RelaySimKeyID = "k1"
	RelaySupplierAddr = "pokt1supplier"
	RelayAppPrivKey = validAppHex
	RelayGatewayPrivKey = validGatewayHex
	RelaySimAppPubKey = "explicit-app-pubkey-hex"
	RelaySimGatewayPubKeys = []string{"explicit-gw-pubkey-hex"}

	require.NoError(t, ResolveSimulationFlags())

	require.Equal(t, "explicit-app-pubkey-hex", RelaySimAppPubKey)
	require.Equal(t, []string{"explicit-gw-pubkey-hex"}, RelaySimGatewayPubKeys)
}

// TestResolveSimulationFlags_NoGatewayKeyOrPubKeysErrors proves that without
// EITHER a resolved gateway key OR an explicit --sim-gateway-pubkeys, the
// operator gets a clear error instead of a relay silently built with an empty
// ring.
func TestResolveSimulationFlags_NoGatewayKeyOrPubKeysErrors(t *testing.T) {
	resetSimulationFlags(t)
	RelaySimulate = true
	RelaySimKeyID = "k1"
	RelaySupplierAddr = "pokt1supplier"
	RelayAppPrivKey = validAppHex
	// RelayGatewayPrivKey and RelaySimGatewayPubKeys both left empty.

	err := ResolveSimulationFlags()

	require.Error(t, err)
	require.Contains(t, err.Error(), "gateway")
}

// TestResolveSimulationFlags_InvalidAppKeyErrors proves a malformed resolved
// app key surfaces a clear derivation error rather than panicking.
func TestResolveSimulationFlags_InvalidAppKeyErrors(t *testing.T) {
	resetSimulationFlags(t)
	RelaySimulate = true
	RelaySimKeyID = "k1"
	RelaySupplierAddr = "pokt1supplier"
	RelayAppPrivKey = "not-valid-hex"
	RelayGatewayPrivKey = validGatewayHex

	err := ResolveSimulationFlags()

	require.Error(t, err)
	require.Contains(t, err.Error(), "app pubkey")
}

// --- header/metadata injection helpers ---------------------------------

// TestSimulationHTTPHeader_Disabled proves no header is emitted when
// --simulate is off.
func TestSimulationHTTPHeader_Disabled(t *testing.T) {
	resetSimulationFlags(t)

	_, _, ok := simulationHTTPHeader()

	require.False(t, ok)
}

// TestSimulationHTTPHeader_Enabled proves the exact header name/value pair
// used at the http, stream, and websocket send sites when --simulate is on.
func TestSimulationHTTPHeader_Enabled(t *testing.T) {
	resetSimulationFlags(t)
	RelaySimulate = true
	RelaySimKeyID = "my-key-id"

	key, value, ok := simulationHTTPHeader()

	require.True(t, ok)
	require.Equal(t, relayer.HeaderSimulationKeyID, key)
	require.Equal(t, "my-key-id", value)
}

// TestSimulationGRPCMetadataPair_Disabled proves no metadata pair is emitted
// when --simulate is off.
func TestSimulationGRPCMetadataPair_Disabled(t *testing.T) {
	resetSimulationFlags(t)

	_, _, ok := simulationGRPCMetadataPair()

	require.False(t, ok)
}

// TestSimulationGRPCMetadataPair_Enabled proves the exact lowercase gRPC
// metadata key/value pair used at the grpc send site when --simulate is on.
func TestSimulationGRPCMetadataPair_Enabled(t *testing.T) {
	resetSimulationFlags(t)
	RelaySimulate = true
	RelaySimKeyID = "my-key-id"

	key, value, ok := simulationGRPCMetadataPair()

	require.True(t, ok)
	require.Equal(t, relayer.MetaSimulationKeyID, key)
	require.Equal(t, "my-key-id", value)
}
