package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// TestSendStreamingRelay_SimulationHeaderInjected proves sendStreamingRelay
// (the stream send site) puts the Pocket-Simulation-Key-Id header on the wire
// alongside its Rpc-Type: 4 (REST) header when --simulate is active.
func TestSendStreamingRelay_SimulationHeaderInjected(t *testing.T) {
	var gotHeader, gotRpcType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(relayer.HeaderSimulationKeyID)
		gotRpcType = r.Header.Get("Rpc-Type")
		w.WriteHeader(http.StatusOK)
		// No body: readStreamingBatches returns zero batches on immediate EOF.
	}))
	defer srv.Close()

	resetSimulationFlags(t)
	origURL, origService, origTimeout := RelayRelayerURL, RelayServiceID, RelayTimeout
	t.Cleanup(func() { RelayRelayerURL, RelayServiceID, RelayTimeout = origURL, origService, origTimeout })

	RelayRelayerURL = srv.URL
	RelayServiceID = "svc-test"
	RelayTimeout = 5
	RelaySimulate = true
	RelaySimKeyID = "stream-key"

	_, err := sendStreamingRelay(context.Background(), []byte("payload"))

	require.NoError(t, err)
	require.Equal(t, "stream-key", gotHeader)
	require.Equal(t, "4", gotRpcType)
}

// TestSendStreamingRelay_NoSimulationHeaderWhenDisabled proves a normal
// stream relay carries no simulation header.
func TestSendStreamingRelay_NoSimulationHeaderWhenDisabled(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header[relayer.HeaderSimulationKeyID]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resetSimulationFlags(t)
	origURL, origService, origTimeout := RelayRelayerURL, RelayServiceID, RelayTimeout
	t.Cleanup(func() { RelayRelayerURL, RelayServiceID, RelayTimeout = origURL, origService, origTimeout })

	RelayRelayerURL = srv.URL
	RelayServiceID = "svc-test"
	RelayTimeout = 5
	RelaySimulate = false

	_, err := sendStreamingRelay(context.Background(), []byte("payload"))

	require.NoError(t, err)
	require.False(t, sawHeader)
}
