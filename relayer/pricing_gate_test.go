//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/pool"
)

// A relayer whose miner has not published the service factor manifest does not
// know what to charge. These tests hold the refusal at every transport that can
// admit a relay: a relay served at a price nobody published is revenue that
// never comes back, and unlike the boot-window optimistic serve, nothing
// arbitrates it afterwards.

// notPricedProvider is a provider that resolves no factor AND reports itself
// unpriced. The distinction is the point: (0, false) alone means "no factor
// configured, use the protocol formula", which IS a price.
type notPricedProvider struct{}

func (notPricedProvider) GetServiceFactor(_ context.Context, _ string) (float64, bool) {
	return 0, false
}

func (notPricedProvider) Priced() bool { return false }

// unpricedMeter is a meter that is wired but cannot price. Built as a literal
// because Priced reads only this field, so the rest of the meter's dependencies
// are not needed to exercise admission.
func unpricedMeter() *RelayMeter {
	return &RelayMeter{serviceFactorProvider: notPricedProvider{}}
}

func pricingRejections(serviceID, rpcType string) float64 {
	return testutil.ToFloat64(relaysRejected.WithLabelValues(serviceID, rpcType, rejectReasonPricingUnavailable))
}

// TestHTTPWithoutAPriceRefusesBeforeServing covers both validation modes.
// Optimistic is the one that matters: it meters AFTER the response is sent, so
// a check placed there could only serve the relay and then fail to price it.
func TestHTTPWithoutAPriceRefusesBeforeServing(t *testing.T) {
	for _, mode := range []ValidationMode{ValidationModeEager, ValidationModeOptimistic} {
		t.Run(string(mode), func(t *testing.T) {
			var backendHits atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				backendHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer backend.Close()

			f := newSimHTTPFixture(t, backend.URL, mode)
			f.proxy.relayMeter = unpricedMeter()

			body := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-unpriced-"+string(mode))
			before := pricingRejections(simTestService, BackendTypeJSONRPC)

			w := f.post(t, body, false)

			require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
			require.Equal(t, before+1, pricingRejections(simTestService, BackendTypeJSONRPC))
			require.Equal(t, int32(0), backendHits.Load(), "a relay nobody can price must not be served")
			require.Equal(t, int32(0), f.pub.calls.Load(), "and must not be published")
		})
	}
}

// TestGRPCWithoutAPriceRefusesBeforeTheBackend proves the gRPC transport reaches
// the price through its pipeline and answers Unavailable -- not Internal, which
// is reserved for wiring that will never fix itself. This one clears the moment
// the miner publishes.
func TestGRPCWithoutAPriceRefusesBeforeTheBackend(t *testing.T) {
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	fx := newGRPCPublishFixture(t, backend.URL)
	fx.svc.relayPipeline = NewRelayPipeline(acceptAnyValidator{}, unpricedMeter(), testLogger())
	before := pricingRejections(fx.serviceID, BackendTypeGRPC)

	err := fx.svc.handleSendRelay(fx.stream)

	require.Equal(t, codes.Unavailable, status.Code(err), "err=%v", err)
	require.Equal(t, before+1, pricingRejections(fx.serviceID, BackendTypeGRPC))
	require.Equal(t, int32(0), backendHits.Load(), "a relay nobody can price must not reach the backend")
	require.Equal(t, int32(0), fx.pub.calls.Load())
	require.Empty(t, fx.stream.sent)
}

// TestWebSocketWithoutAPriceRefusesTheUpgrade refuses the connection itself.
// Frames are charged one at a time once the socket is up, so admitting the
// upgrade unpriced buys a whole subscription's worth of relays at a price
// nobody published.
func TestWebSocketWithoutAPriceRefusesTheUpgrade(t *testing.T) {
	ep, err := pool.NewBackendEndpoint("ws1", "ws://unreachable.invalid:8545")
	require.NoError(t, err)
	require.True(t, ep.IsHealthy(), "precondition: the fast-fail check must let this through")
	wsPool := pool.NewPool(
		"develop-websocket:websocket",
		[]*pool.BackendEndpoint{ep},
		&pool.FirstHealthySelector{},
		"first_healthy(test)",
	)

	proxy := &ProxyServer{
		logger: testLogger(),
		config: &Config{
			Services: map[string]ServiceConfig{"develop-websocket": {}},
			pools:    map[string]*pool.Pool{"develop-websocket:websocket": wsPool},
		},
		responseSigner: &ResponseSigner{},
		relayProcessor: &noopRelayProcessor{},
		publisher:      &noopPublisher{},
		relayMeter:     unpricedMeter(),
	}

	srv := httptest.NewServer(proxy.WebSocketHandler())
	t.Cleanup(srv.Close)
	before := pricingRejections("develop-websocket", BackendTypeWebSocket)

	headers := http.Header{}
	headers.Set("Target-Service-Id", "develop-websocket")
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), headers)
	if conn != nil {
		_ = conn.Close()
	}
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "an unpriced relayer must not upgrade the connection")
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, before+1, pricingRejections("develop-websocket", BackendTypeWebSocket))
}

// TestPricedIsNotTheNegationOfAConfiguredFactor is the control that keeps the
// gate from rejecting legitimate traffic: an operator who deliberately
// configures no factor is PRICED, and their relays must be admitted and priced
// by the protocol formula. Without this, criterion 1 would take the fleet down.
func TestPricedIsNotTheNegationOfAConfiguredFactor(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()

	writeManifest(t, client, ServiceFactorManifest{Overrides: map[string]float64{}})
	require.NoError(t, client.loadManifest(ctx))

	meter := &RelayMeter{serviceFactorProvider: client}
	require.True(t, meter.Priced(), "a manifest configuring nothing is still a price")

	factor, found := client.GetServiceFactor(ctx, "any-service")
	require.False(t, found, "and it resolves to the protocol formula")
	require.Zero(t, factor)
}
