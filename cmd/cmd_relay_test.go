package cmd

import (
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
)

// TestRunRelayCommand_InitializesPoktBech32Prefix proves initSDKConfig (which
// runRelayCommand now calls, mirroring runHARelayer) leaves the Cosmos SDK
// global Bech32 config set to Pocket Network's "pokt" prefix.
//
// This matters specifically for the --simulate feature: relay_client's
// BuildSimulatedRelayRequest derives the simulated session's
// SessionHeader.ApplicationAddress via cosmostypes.AccAddress(pub.Address()).
// String() — the SAME method relayer/simulation.go's buildSimIdentities uses
// to derive its pinned identity's address. That method reads the process's
// GLOBAL Bech32 config, which defaults to cosmos-sdk's stock "cosmos" prefix
// until something sets it. The relayer binary always sets it to "pokt" via
// runHARelayer -> initSDKConfig before touching any address. Before this fix,
// `pocket-relay-miner relay ...` (a DIFFERENT process) never called
// initSDKConfig, so a simulated relay's ApplicationAddress would be
// "cosmos1..." instead of "pokt1...", and the relayer would reject it as an
// identity mismatch — a bug invisible to any single-process unit test (both
// sides of an in-process test share the same global default and "agree" on
// the wrong prefix), only surfaced by driving the actual compiled binary as
// two separate processes.
func TestRunRelayCommand_InitializesPoktBech32Prefix(t *testing.T) {
	initSDKConfig()

	require.Equal(t, "pokt", sdk.GetConfig().GetBech32AccountAddrPrefix(),
		"the relay subcommand's process must use the pokt Bech32 prefix, matching the relayer binary")
}
