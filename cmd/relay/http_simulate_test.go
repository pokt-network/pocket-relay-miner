package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// TestSendRelayOverHTTP_SimulationHeaderInjected proves the wiring end to
// end: with --simulate/--sim-key-id set, sendRelayOverHTTP (the shared send
// function for both jsonrpc and cometbft modes) actually puts the
// Pocket-Simulation-Key-Id header on the wire, not just on some intermediate
// value.
func TestSendRelayOverHTTP_SimulationHeaderInjected(t *testing.T) {
	var gotHeader string
	var gotRpcType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(relayer.HeaderSimulationKeyID)
		gotRpcType = r.Header.Get("Rpc-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resetSimulationFlags(t)
	origURL, origService := RelayRelayerURL, RelayServiceID
	t.Cleanup(func() { RelayRelayerURL, RelayServiceID = origURL, origService })

	RelayRelayerURL = srv.URL
	RelayServiceID = "svc-test"
	RelaySimulate = true
	RelaySimKeyID = "cli-fixture-key"

	_, err := sendRelayOverHTTP(context.Background(), []byte("payload"), rpcTypeJSONRPC)

	require.NoError(t, err)
	require.Equal(t, "cli-fixture-key", gotHeader, "the simulation key_id must reach the relayer as an HTTP header")
	require.Equal(t, rpcTypeJSONRPC, gotRpcType, "Rpc-Type must still be set alongside the simulation header")
}

// TestSendRelayOverHTTP_NoSimulationHeaderWhenDisabled proves a normal
// (non-simulated) relay carries NO simulation header at all — its absence is
// how the relayer tells a normal relay from a simulated one.
func TestSendRelayOverHTTP_NoSimulationHeaderWhenDisabled(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header[relayer.HeaderSimulationKeyID]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resetSimulationFlags(t)
	origURL, origService := RelayRelayerURL, RelayServiceID
	t.Cleanup(func() { RelayRelayerURL, RelayServiceID = origURL, origService })

	RelayRelayerURL = srv.URL
	RelayServiceID = "svc-test"
	RelaySimulate = false

	_, err := sendRelayOverHTTP(context.Background(), []byte("payload"), rpcTypeJSONRPC)

	require.NoError(t, err)
	require.False(t, sawHeader, "a normal relay must not carry the simulation header at all")
}

// TestSendRelayOverHTTP_CometBFTAlsoCarriesSimulationHeader proves the
// cometbft send path (sendCometBFTRelay -> sendRelayOverHTTP) picks up the
// same injection, since both modes funnel through the shared function.
func TestSendRelayOverHTTP_CometBFTAlsoCarriesSimulationHeader(t *testing.T) {
	var gotHeader, gotRpcType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(relayer.HeaderSimulationKeyID)
		gotRpcType = r.Header.Get("Rpc-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resetSimulationFlags(t)
	origURL, origService := RelayRelayerURL, RelayServiceID
	t.Cleanup(func() { RelayRelayerURL, RelayServiceID = origURL, origService })

	RelayRelayerURL = srv.URL
	RelayServiceID = "svc-test"
	RelaySimulate = true
	RelaySimKeyID = "cometbft-key"

	_, err := sendCometBFTRelay(context.Background(), []byte("payload"))

	require.NoError(t, err)
	require.Equal(t, "cometbft-key", gotHeader)
	require.Equal(t, rpcTypeCometBFT, gotRpcType)
}
