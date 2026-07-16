//go:build test

package relayer

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/pool"
	"github.com/pokt-network/pocket-relay-miner/rings"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// newSimGRPCService wires a RelayGRPCService with an ENABLED simulation verifier
// and a recording publisher, plus a valid ring-signed simulated RelayRequest.
func newSimGRPCService(t *testing.T, backendURL string) (*RelayGRPCService, *recordingPublisher, *servicetypes.RelayRequest, string) {
	t.Helper()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	fixed := time.Unix(1_700_000_000, 0).UTC()
	clock := func() time.Time { return fixed }

	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	supplierPriv := secp256k1.GenPrivKey()
	supplierAddr := cosmostypes.AccAddress(supplierPriv.PubKey().Address()).String()
	appAddr := cosmostypes.AccAddress(appPriv.PubKey().Address()).String()
	const serviceID = "develop-http"

	signer, err := NewResponseSigner(logger, map[string]cryptotypes.PrivKey{supplierAddr: supplierPriv})
	require.NoError(t, err)

	simCfg := SimulationConfig{
		Enabled: true, MaxConcurrent: 8, FreshnessWindowSeconds: 30,
		Identities: []SimIdentity{{
			KeyID: simTestKeyID, Enabled: true, MaxRPS: 50,
			AppPubKeyHex:      hex.EncodeToString(appPriv.PubKey().Bytes()),
			GatewayPubKeysHex: []string{hex.EncodeToString(gwPriv.PubKey().Bytes())},
			AllowedServices:   []string{serviceID},
		}},
	}
	require.NoError(t, simCfg.Validate())

	mr, err := miniredis.Run()
	require.NoError(t, err)
	rc, err := redisutil.NewClient(t.Context(), redisutil.ClientConfig{URL: "redis://" + mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Close(); mr.Close() })

	simVerifier, err := NewSimulationVerifier(logger, &simCfg, rc, signer, map[string]struct{}{serviceID: {}}, clock)
	require.NoError(t, err)

	endpoint, err := pool.NewBackendEndpoint("backend", backendURL)
	require.NoError(t, err)
	healthyPool := pool.NewPool("test-pool", []*pool.BackendEndpoint{endpoint}, &pool.FirstHealthySelector{}, "first_healthy(test)")

	pub := &recordingPublisher{}
	proc := &recordingProcessor{}
	svc := NewRelayGRPCService(logger, RelayGRPCServiceConfig{
		ServiceConfigs: map[string]ServiceConfig{serviceID: {}},
		ResponseSigner: signer,
		Publisher:      pub,
		RelayProcessor: proc,
		SimVerifier:    simVerifier,
		GetPool:        func(string, string) *pool.Pool { return healthyPool },
	})

	// Build a valid ring-signed simulated RelayRequest.
	poktReq := &sdktypes.POKTHTTPRequest{Method: http.MethodPost, Url: "/", BodyBz: []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`)}
	payloadBz, err := proto.Marshal(poktReq)
	require.NoError(t, err)
	rr := &servicetypes.RelayRequest{
		Payload: payloadBz,
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress: appAddr, ServiceId: serviceID,
				SessionId:               FormatSimSessionID(clock(), "g1"),
				SessionStartBlockHeight: 1, SessionEndBlockHeight: 2,
			},
			SupplierOperatorAddress: supplierAddr,
		},
	}
	hash, err := rr.GetSignableBytesHash()
	require.NoError(t, err)
	ring, err := rings.GetRingFromPubKeys([]cryptotypes.PubKey{appPriv.PubKey(), gwPriv.PubKey()})
	require.NoError(t, err)
	scalar, err := ring_secp256k1.NewCurve().DecodeToScalar(gwPriv.Bytes())
	require.NoError(t, err)
	sig, err := ring.Sign(hash, scalar)
	require.NoError(t, err)
	sigBz, err := sig.Serialize()
	require.NoError(t, err)
	rr.Meta.Signature = sigBz

	return svc, pub, rr, supplierAddr
}

// TestServeSimulatedGRPC_SuccessNoPublish proves a valid simulated gRPC relay is
// served (signed response sent on the stream) but never published to the WAL.
func TestServeSimulatedGRPC_SuccessNoPublish(t *testing.T) {
	backendHit := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHit++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x1","id":1}`))
	}))
	defer backend.Close()

	svc, pub, rr, supplier := newSimGRPCService(t, backend.URL)
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("rpc-type", "3", MetaSimulationKeyID, simTestKeyID))
	stream := &mockServerStream{ctx: ctx, req: rr}

	before := testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues("grpc", "develop-http", supplier, SimResultSuccess))
	require.NoError(t, svc.handleSendRelay(stream))

	require.Equal(t, 1, backendHit, "backend hit once")
	require.Len(t, stream.sent, 1, "exactly one signed response sent")
	resp, ok := stream.sent[0].(*servicetypes.RelayResponse)
	require.True(t, ok, "response is a RelayResponse")
	require.NotEmpty(t, resp.Meta.SupplierOperatorSignature, "response supplier-signed")
	require.Equal(t, int32(0), pub.calls.Load(), "simulated gRPC relay must NOT publish")
	require.Equal(t, before+1,
		testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues("grpc", "develop-http", supplier, SimResultSuccess)))
}

// TestServeSimulatedGRPC_ForgedRejected proves a forged simulated gRPC relay is
// rejected with PermissionDenied and never reaches the backend or the WAL.
func TestServeSimulatedGRPC_ForgedRejected(t *testing.T) {
	backendHit := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	svc, pub, rr, _ := newSimGRPCService(t, backend.URL)
	rr.Meta.Signature[len(rr.Meta.Signature)-1] ^= 0xFF // tamper
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("rpc-type", "3", MetaSimulationKeyID, simTestKeyID))
	stream := &mockServerStream{ctx: ctx, req: rr}

	err := svc.handleSendRelay(stream)
	require.Error(t, err, "forged simulated gRPC relay rejected")
	require.Equal(t, 0, backendHit, "backend must NOT be called")
	require.Empty(t, stream.sent, "no response sent")
	require.Equal(t, int32(0), pub.calls.Load())
}
