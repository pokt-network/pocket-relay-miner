//go:build test

package relayer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/gorilla/websocket"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/rings"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// neverCallValidator is a RelayValidator test double that records whether
// ValidateRelayRequest was invoked. A simulated WebSocket message must NEVER
// reach the real relayPipeline's Validate/Meter step (a simulated relay must
// never consume stake). Because RelayPipeline.MeterRelay is only reached
// AFTER ValidateRelay succeeds, proving this call count stays at zero also
// proves the meter was never touched -- without needing a fully-wired
// *RelayMeter backed by real chain query clients. If the guard under test is
// ever broken, this validator returns an error (not a panic) so the bridge
// takes its normal validation-failure path instead of crashing the test
// process.
type neverCallValidator struct {
	calls atomic.Int32
}

func (v *neverCallValidator) ValidateRelayRequest(context.Context, *servicetypes.RelayRequest) error {
	v.calls.Add(1)
	return errors.New("ValidateRelay must never be called for a simulated websocket relay")
}

func (v *neverCallValidator) CheckRewardEligibility(context.Context, *servicetypes.RelayRequest) error {
	return nil
}

func (v *neverCallValidator) GetCurrentBlockHeight() int64 { return 0 }
func (v *neverCallValidator) SetCurrentBlockHeight(int64)  {}

// newSimWSBackendServer starts a fake WebSocket backend: every BinaryMessage
// it receives increments hits and gets a canned JSON-RPC-shaped reply.
func newSimWSBackendServer(t *testing.T) (wsURL string, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			n := hits.Add(1)
			reply := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","result":"0x%d","id":1}`, n))
			if err := conn.WriteMessage(mt, reply); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), hits
}

// newGatewaySideHarness stands in for the PATH gateway's side of the
// WebSocket handshake. It returns the relayer-side *websocket.Conn (the value
// WebSocketHandler would normally get from Upgrade(), passed as gatewayConn
// into NewWebSocketBridge) and a client-side *websocket.Conn the test uses to
// write RelayRequests and read signed RelayResponses, exactly like a real
// gateway would.
func newGatewaySideHarness(t *testing.T) (relayerSide, testSide *websocket.Conn) {
	t.Helper()
	connCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCh <- c
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)

	select {
	case relayerConn := <-connCh:
		return relayerConn, client
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for gateway-side upgrade")
		return nil, nil
	}
}

// simWSFixture wires a simulated WebSocketBridge with: a real ring-signed
// pinned identity, a recording publisher (deliberately NOT nil -- see
// simulated below), a recording relay processor, a validator that fails the
// test if ever invoked, and a real backend + gateway WebSocket pair.
type simWSFixture struct {
	gwClient     *websocket.Conn
	backendHits  *atomic.Int32
	pub          *recordingPublisher
	proc         *recordingProcessor
	validator    *neverCallValidator
	appPriv      *secp256k1.PrivKey
	gwPriv       *secp256k1.PrivKey
	supplierAddr string
	appAddr      string
	clock        func() time.Time
	mr           *miniredis.Miniredis
}

const simWSTestService = simTestService

func newSimWSFixture(t *testing.T) *simWSFixture {
	t.Helper()
	logger := testLogger()
	fixed := time.Unix(1_700_000_000, 0).UTC()
	clock := func() time.Time { return fixed }

	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	supplierPriv := secp256k1.GenPrivKey()
	supplierAddr := cosmostypes.AccAddress(supplierPriv.PubKey().Address()).String()
	appAddr := cosmostypes.AccAddress(appPriv.PubKey().Address()).String()

	signer, err := NewResponseSigner(logger, map[string]cryptotypes.PrivKey{supplierAddr: supplierPriv})
	require.NoError(t, err)

	simCfg := SimulationConfig{
		Enabled: true, MaxConcurrent: 8, FreshnessWindowSeconds: 30,
		Identities: []SimIdentity{{
			KeyID: simTestKeyID, Enabled: true, MaxRPS: 50,
			AppPubKeyHex:      hex.EncodeToString(appPriv.PubKey().Bytes()),
			GatewayPubKeysHex: []string{hex.EncodeToString(gwPriv.PubKey().Bytes())},
			AllowedServices:   []string{simWSTestService},
		}},
	}
	require.NoError(t, simCfg.Validate())

	mr, err := miniredis.Run()
	require.NoError(t, err)
	rc, err := redisutil.NewClient(t.Context(), redisutil.ClientConfig{URL: "redis://" + mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Close(); mr.Close() })

	simVerifier, err := NewSimulationVerifier(logger, &simCfg, rc, signer,
		map[string]struct{}{simWSTestService: {}}, clock)
	require.NoError(t, err)

	pub := &recordingPublisher{}
	proc := &recordingProcessor{}
	validator := &neverCallValidator{}
	// relayMeter is deliberately nil: MeterRelay is only reached AFTER
	// ValidateRelay succeeds in the real (non-simulated) branch, and
	// neverCallValidator always errors, so MeterRelay/relayMeter is never
	// dereferenced even if the simulated guard were broken.
	pipeline := NewRelayPipeline(validator, nil, signer, proc, logger, nil, nil)

	backendURL, backendHits := newSimWSBackendServer(t)
	relayerConn, gwClient := newGatewaySideHarness(t)
	t.Cleanup(func() { _ = gwClient.Close() })

	bridge, err := NewWebSocketBridge(
		logger,
		relayerConn,
		backendURL,
		simWSTestService,
		supplierAddr,
		1, // arrivalHeight
		proc,
		pub, // NOT nil -- proves the "never publish" guard doesn't depend on nil propagation
		signer,
		http.Header{},
		nil, // sessionMonitor
		pipeline,
		1, // computeUnits
		2*time.Second,
		true, // simulated
		simVerifier,
		simTestKeyID,
	)
	require.NoError(t, err)

	go bridge.Run()
	t.Cleanup(func() { _ = bridge.Close() })

	return &simWSFixture{
		gwClient:     gwClient,
		backendHits:  backendHits,
		pub:          pub,
		proc:         proc,
		validator:    validator,
		appPriv:      appPriv,
		gwPriv:       gwPriv,
		supplierAddr: supplierAddr,
		appAddr:      appAddr,
		clock:        clock,
		mr:           mr,
	}
}

// buildSignedRelay builds a real ring-signed simulated RelayRequest bound to
// this fixture's pinned identity. The WebSocket payload is raw opaque bytes
// (unlike HTTP/gRPC, WS never wraps the payload in a POKTHTTPRequest).
func (f *simWSFixture) buildSignedRelay(t *testing.T, sessionID string) *servicetypes.RelayRequest {
	t.Helper()
	rr := &servicetypes.RelayRequest{
		Payload: []byte(`{"jsonrpc":"2.0","method":"eth_subscribe","id":1}`),
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      f.appAddr,
				ServiceId:               simWSTestService,
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
	return rr
}

// countMeterKeys counts miniredis keys under the meter/stake prefixes used by
// RelayMeter.CheckAndConsumeRelay (ha:meter:* and ha:app_stake:*). The
// simulation replay-dedup key (ha:sim:replay:*) is a legitimate Admission
// write and is deliberately excluded -- this helper proves Accounting
// (metering) never wrote anything, not that Redis was untouched.
func countMeterKeys(mr *miniredis.Miniredis) int {
	n := 0
	for _, k := range mr.Keys() {
		if strings.Contains(k, ":meter:") || strings.Contains(k, ":app_stake:") {
			n++
		}
	}
	return n
}

// TestWebSocketBridge_Simulated_MultiMessageSuccessNoPublishNoMeter drives a
// simulated bridge through a multi-message session (mirrors a real gateway
// sending several relay requests over one long-lived connection) and proves:
// every message is served and supplier-signed, the backend is actually hit,
// but the WAL is NEVER published to, the relay pipeline's Validate/Meter step
// is NEVER invoked, no meter/stake Redis keys are ever created, and the
// simulated-relay metric moves.
func TestWebSocketBridge_Simulated_MultiMessageSuccessNoPublishNoMeter(t *testing.T) {
	f := newSimWSFixture(t)

	before := testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues("websocket", simWSTestService, f.supplierAddr, SimResultSuccess))
	metersBefore := countMeterKeys(f.mr)

	nonces := []string{"n1", "n2", "n3"}
	for i, nonce := range nonces {
		sessionID := FormatSimSessionID(f.clock(), nonce)
		rr := f.buildSignedRelay(t, sessionID)
		bz, err := rr.Marshal()
		require.NoError(t, err)

		require.NoError(t, f.gwClient.WriteMessage(websocket.BinaryMessage, bz))
		require.NoError(t, f.gwClient.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, respBz, err := f.gwClient.ReadMessage()
		require.NoError(t, err, "message %d: expected a response", i)

		var resp servicetypes.RelayResponse
		require.NoError(t, resp.Unmarshal(respBz), "message %d: response must unmarshal as RelayResponse", i)
		require.NotEmpty(t, resp.Meta.SupplierOperatorSignature, "message %d: response must be supplier-signed", i)
		require.Equal(t, sessionID, resp.Meta.SessionHeader.GetSessionId(), "message %d: response echoes the request session", i)
		require.Contains(t, string(resp.Payload), fmt.Sprintf(`"result":"0x%d"`, i+1), "message %d: response carries the backend payload", i)
	}

	require.Equal(t, int32(len(nonces)), f.backendHits.Load(), "backend hit once per message")

	// The metric is the one assertion that cannot be read the instant the last
	// response arrives. The bridge writes the response to the client and only
	// then calls emitRelay, which increments the counter (websocket.go:
	// handleBackendMessage). That order is correct -- serve first, account
	// after -- but it means ReadMessage returning does not imply the increment
	// has happened yet. Reading immediately is a race the test loses whenever
	// the runtime is slow enough, which is exactly what CI is: it passed 40/40
	// locally and failed under `make test-coverage`, where instrumentation
	// widens the window.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues(
			"websocket", simWSTestService, f.supplierAddr, SimResultSuccess)) == before+float64(len(nonces))
	}, 5*time.Second, 5*time.Millisecond,
		"simulated success metric must increment once per message (got %v, want %v)",
		testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues("websocket", simWSTestService, f.supplierAddr, SimResultSuccess)),
		before+float64(len(nonces)))

	// These four are checked AFTER the metric settles, not before: each asserts
	// that something never happened, and "not yet" is indistinguishable from
	// "never" if you look too early. Waiting for the metric proves emitRelay
	// has run for every message, so the accounting path is done deciding.
	require.Equal(t, int32(0), f.pub.calls.Load(), "simulated websocket relay must NEVER publish to the WAL")
	require.Equal(t, int32(0), f.proc.calls.Load(), "simulated websocket relay must NEVER run mining/ProcessRelay")
	require.Equal(t, int32(0), f.validator.calls.Load(), "ValidateRelay/MeterRelay must NEVER run for a simulated relay")
	require.Equal(t, metersBefore, countMeterKeys(f.mr), "no meter/stake keys may be created by a simulated relay")
}

// TestWebSocketBridge_Simulated_ForgedRejectedNoBackendHit proves a forged
// simulated relay is rejected by Admission BEFORE the backend is ever
// touched, that the bridge closes the connection (the existing convention for
// an admission/validation failure on this bridge), and that the rejection is
// recorded on the simulated metric -- never on any real-relay counter.
func TestWebSocketBridge_Simulated_ForgedRejectedNoBackendHit(t *testing.T) {
	f := newSimWSFixture(t)

	rr := f.buildSignedRelay(t, FormatSimSessionID(f.clock(), "forged"))
	rr.Meta.Signature[len(rr.Meta.Signature)-1] ^= 0xFF // tamper with the ring signature
	bz, err := rr.Marshal()
	require.NoError(t, err)

	before := testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues("websocket", simWSTestService, f.supplierAddr, SimResultVerifyFailed))

	require.NoError(t, f.gwClient.WriteMessage(websocket.BinaryMessage, bz))

	require.NoError(t, f.gwClient.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, err = f.gwClient.ReadMessage()
	require.Error(t, err, "the bridge must close the connection on a rejected simulated relay, not serve it")
	require.True(t, websocket.IsCloseError(err, CloseValidationFailed),
		"expected the simulation-rejected close code, got: %v", err)

	require.Equal(t, int32(0), f.backendHits.Load(), "backend must NOT be called for a forged simulated relay")
	require.Equal(t, int32(0), f.pub.calls.Load())
	require.Equal(t, int32(0), f.proc.calls.Load())
	require.Equal(t, int32(0), f.validator.calls.Load(), "the forged relay must be rejected by simulation Admission, not the real pipeline")
	require.Equal(t, before+1,
		testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues("websocket", simWSTestService, f.supplierAddr, SimResultVerifyFailed)),
		"verify_failed simulated metric incremented")
}
