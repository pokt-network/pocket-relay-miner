//go:build test

package relayer

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alitto/pond/v2"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/rings"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

type simHTTPFixture struct {
	proxy        *ProxyServer
	pub          *recordingPublisher
	appPriv      *secp256k1.PrivKey
	gwPriv       *secp256k1.PrivKey
	supplierAddr string
	appAddr      string
	clock        func() time.Time
}

// newSimHTTPFixture wires a ProxyServer (struct literal, like ready_service_test)
// with a real backend, a loaded supplier signer, a recording publisher, and an
// ENABLED simulation verifier for one identity.
func newSimHTTPFixture(t *testing.T, backendURL string, validationMode ValidationMode) *simHTTPFixture {
	t.Helper()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	fixed := time.Unix(1_700_000_000, 0).UTC()
	clock := func() time.Time { return fixed }

	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	supplierPriv := secp256k1.GenPrivKey()
	supplierAddr := cosmostypes.AccAddress(supplierPriv.PubKey().Address()).String()
	appAddr := cosmostypes.AccAddress(appPriv.PubKey().Address()).String()

	simCfg := SimulationConfig{
		Enabled:                true,
		MaxConcurrent:          8,
		FreshnessWindowSeconds: 30,
		Identities: []SimIdentity{{
			KeyID:             simTestKeyID,
			Enabled:           true,
			MaxRPS:            50,
			AppPubKeyHex:      hex.EncodeToString(appPriv.PubKey().Bytes()),
			GatewayPubKeysHex: []string{hex.EncodeToString(gwPriv.PubKey().Bytes())},
			AllowedServices:   []string{simTestService},
		}},
	}

	c := &Config{
		ListenAddr:              "0.0.0.0:8080",
		Redis:                   RedisConfig{URL: "redis://localhost:6379"},
		PocketNode:              PocketNodeConfig{QueryNodeRPCUrl: "http://x", QueryNodeGRPCUrl: "x:9090"},
		DefaultValidationMode:   validationMode,
		DefaultMaxBodySizeBytes: 1 << 20,
		HTTPTransport:           HTTPTransportConfig{MaxConnsPerHost: 500, MaxIdleConnsPerHost: 100, IdleConnTimeoutSeconds: 90},
		TimeoutProfiles: map[string]TimeoutProfile{
			"fast":      {Name: "fast", RequestTimeoutSeconds: 30, ResponseHeaderTimeoutSeconds: 30, DialTimeoutSeconds: 5, TLSHandshakeTimeoutSeconds: 10},
			"streaming": {Name: "streaming", RequestTimeoutSeconds: 600, DialTimeoutSeconds: 10, TLSHandshakeTimeoutSeconds: 15},
		},
		Services: map[string]ServiceConfig{
			simTestService: {
				TimeoutProfile: "fast",
				PoolProfile:    "high",
				DefaultBackend: "jsonrpc",
				ValidationMode: validationMode,
				Backends:       map[string]BackendConfig{"jsonrpc": {URL: backendURL}},
			},
		},
		Simulation: simCfg,
	}
	require.NoError(t, c.Validate())
	require.NoError(t, c.BuildPools())
	require.NoError(t, simCfg.Validate())

	clients, fallback := buildClientPool(c, &c.HTTPTransport)

	signer, err := NewResponseSigner(logger, map[string]cryptotypes.PrivKey{supplierAddr: supplierPriv})
	require.NoError(t, err)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	rc, err := redisutil.NewClient(t.Context(), redisutil.ClientConfig{URL: "redis://" + mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Close(); mr.Close() })

	simVerifier, err := NewSimulationVerifier(logger, &simCfg, rc, signer,
		map[string]struct{}{simTestService: {}}, clock)
	require.NoError(t, err)

	pub := &recordingPublisher{}
	pool := pond.NewPool(4)
	t.Cleanup(func() { pool.StopAndWait() })

	p := &ProxyServer{
		logger:             logger,
		config:             c,
		clientPool:         clients,
		clientPoolFallback: fallback,
		responseSigner:     signer,
		supplierCache:      cache.NewSupplierCache(logger, rc, cache.SupplierCacheConfig{FailOpen: true}),
		publisher:          pub,
		healthChecker:      NewHealthChecker(logger),
		metricRecorder:     NewMetricRecorder(logger, pool),
		bufferPool:         NewBufferPool(1 << 20),
		simVerifier:        simVerifier,
	}

	return &simHTTPFixture{proxy: p, pub: pub, appPriv: appPriv, gwPriv: gwPriv, supplierAddr: supplierAddr, appAddr: appAddr, clock: clock}
}

// buildSignedSimBody builds a ring-signed simulated RelayRequest and returns its
// marshaled bytes (the HTTP body a gateway would POST).
func (f *simHTTPFixture) buildSignedSimBody(t *testing.T, appAddr, serviceID, sessionID string) []byte {
	t.Helper()
	poktReq := &sdktypes.POKTHTTPRequest{Method: http.MethodPost, Url: "/", BodyBz: []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`)}
	payloadBz, err := proto.Marshal(poktReq)
	require.NoError(t, err)

	rr := &servicetypes.RelayRequest{
		Payload: payloadBz,
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      appAddr,
				ServiceId:               serviceID,
				SessionId:               sessionID,
				SessionStartBlockHeight: 1,
				SessionEndBlockHeight:   2,
			},
			SupplierOperatorAddress: f.supplierAddr,
		},
	}
	hash, err := rr.GetSignableBytesHash()
	require.NoError(t, err)
	ring, err := rings.GetRingFromPubKeys([]cryptotypes.PubKey{f.appPriv.PubKey(), f.gwPriv.PubKey()})
	require.NoError(t, err)
	scalar, err := ring_secp256k1.NewCurve().DecodeToScalar(f.gwPriv.Bytes())
	require.NoError(t, err)
	sig, err := ring.Sign(hash, scalar)
	require.NoError(t, err)
	sigBz, err := sig.Serialize()
	require.NoError(t, err)
	rr.Meta.Signature = sigBz

	bodyBz, err := rr.Marshal()
	require.NoError(t, err)
	return bodyBz
}

func (f *simHTTPFixture) post(t *testing.T, body []byte, setSimHeader bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Rpc-Type", "3") // JSON_RPC
	if setSimHeader {
		req.Header.Set(HeaderSimulationKeyID, simTestKeyID)
	}
	w := httptest.NewRecorder()
	f.proxy.handleRelay(w, req)
	return w
}

// TestServeSimulatedHTTP_SuccessNoPublishNoRealMetrics is the core integration
// proof: a valid simulated relay is served (200) and supplier-signed, the
// backend is hit, but NOTHING is published to the WAL and the real relaysServed
// counter is untouched — only the simulated metric moves.
func TestServeSimulatedHTTP_SuccessNoPublishNoRealMetrics(t *testing.T) {
	backendHit := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHit++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x10","id":1}`))
	}))
	defer backend.Close()

	f := newSimHTTPFixture(t, backend.URL, ValidationModeOptimistic)
	body := f.buildSignedSimBody(t, f.appAddr, simTestService, FormatSimSessionID(f.clock(), "n1"))

	realBefore := testutil.ToFloat64(relaysServed.WithLabelValues(simTestService, "3", "200"))
	receivedBefore := testutil.ToFloat64(relaysReceived.WithLabelValues(simTestService, "3"))
	simBefore := testutil.CollectAndCount(simulatedRelaysTotal)

	w := f.post(t, body, true)

	require.Equal(t, http.StatusOK, w.Code, "simulated relay served OK; body=%s", w.Body.String())
	require.Equal(t, 1, backendHit, "backend hit exactly once (real round-trip)")

	// Response is a supplier-signed RelayResponse.
	var resp servicetypes.RelayResponse
	require.NoError(t, resp.Unmarshal(w.Body.Bytes()))
	require.NotEmpty(t, resp.Meta.SupplierOperatorSignature, "simulated response must be supplier-signed")

	// No publish, real counter untouched, simulated metric moved.
	require.Equal(t, int32(0), f.pub.calls.Load(), "simulated relay must NOT publish to the WAL")
	require.Equal(t, realBefore, testutil.ToFloat64(relaysServed.WithLabelValues(simTestService, "3", "200")),
		"real relaysServed must be untouched by a simulated relay")
	require.Equal(t, receivedBefore, testutil.ToFloat64(relaysReceived.WithLabelValues(simTestService, "3")),
		"real relaysReceived must be untouched by a simulated relay (goal 8)")
	require.GreaterOrEqual(t, testutil.CollectAndCount(simulatedRelaysTotal), simBefore,
		"simulated metric family should have at least as many series")
	require.GreaterOrEqual(t,
		testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues(BackendTypeJSONRPC, simTestService, f.supplierAddr, SimResultSuccess)),
		float64(1), "simulated success metric incremented")
}

// TestServeSimulatedHTTP_ForgedUnderOptimisticNeverHitsBackend proves eager
// admission: even with the service in OPTIMISTIC mode (serve-first for real
// relays), a forged simulated relay is rejected BEFORE the backend is called.
func TestServeSimulatedHTTP_ForgedUnderOptimisticNeverHitsBackend(t *testing.T) {
	backendHit := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := newSimHTTPFixture(t, backend.URL, ValidationModeOptimistic)
	body := f.buildSignedSimBody(t, f.appAddr, simTestService, FormatSimSessionID(f.clock(), "n2"))
	// Tamper the signed body so the ring signature no longer verifies.
	body[len(body)-1] ^= 0xFF

	w := f.post(t, body, true)

	require.Equal(t, http.StatusForbidden, w.Code, "forged simulated relay rejected")
	require.Equal(t, 0, backendHit, "backend must NOT be called for a forged simulated relay, even under optimistic mode")
	require.Equal(t, int32(0), f.pub.calls.Load())
}

// TestSimHeaderIgnoredWhenDisabled proves R7's gate: the handleRelay guard
// routes to the simulated path only when the verifier is Enabled(). Disabling
// via Reload flips Enabled() to false, so the guard short-circuits and the sim
// header is ignored (the request falls through to the normal path). The full
// behavioral end-to-end — a disabled header serving normally, not a 403 — is
// exercised in the live Tilt test, which has the complete normal-path wiring.
func TestSimHeaderIgnoredWhenDisabled(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := newSimHTTPFixture(t, backend.URL, ValidationModeOptimistic)
	require.True(t, f.proxy.simVerifier.Enabled(), "verifier starts enabled")

	f.proxy.config.Simulation.Enabled = false
	require.NoError(t, f.proxy.simVerifier.Reload(&f.proxy.config.Simulation))
	require.False(t, f.proxy.simVerifier.Enabled(),
		"disabled verifier makes the handleRelay guard skip the sim path (header ignored, R7)")
}
