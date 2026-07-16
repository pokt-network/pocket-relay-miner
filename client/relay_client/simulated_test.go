//go:build test

package relay_client

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/relayer"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const simCLITestServiceID = "svc-test"

// newSimTestRelayClient builds a RelayClient in gateway mode (app + gateway
// keys, so c.signer is the gateway signer per NewRelayClient) with
// QueryClients pointed at an unreachable placeholder endpoint.
// grpc.NewClient (used by query.NewQueryClients) does not dial synchronously
// in modern grpc-go, so this never performs a network call — matching
// production, where BuildSimulatedRelayRequest is the only method exercised
// here and never touches queryClients.
func newSimTestRelayClient(t *testing.T, appPrivHex, gwPrivHex string) *RelayClient {
	t.Helper()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	qc, err := query.NewQueryClients(logger, query.ClientConfig{GRPCEndpoint: "127.0.0.1:1"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = qc.Close() })

	rc, err := NewRelayClient(Config{
		AppPrivateKeyHex:     appPrivHex,
		GatewayPrivateKeyHex: gwPrivHex,
		QueryClients:         qc,
	}, logger)
	require.NoError(t, err)
	return rc
}

// simTestKeys bundles the app/gateway/supplier keys a test needs, both as
// generated cryptotypes.PrivKey (for building a relayer.SimulationVerifier
// fixture) and as hex (for driving the CLI-side builder under test).
type simTestKeys struct {
	appPriv      *secp256k1.PrivKey
	gwPriv       *secp256k1.PrivKey
	supplierPriv *secp256k1.PrivKey
	supplierAddr string

	appPrivHex string
	gwPrivHex  string
	appPubHex  string
	gwPubHex   string
}

func newSimTestKeys() simTestKeys {
	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	supplierPriv := secp256k1.GenPrivKey()
	return simTestKeys{
		appPriv:      appPriv,
		gwPriv:       gwPriv,
		supplierPriv: supplierPriv,
		supplierAddr: cosmostypes.AccAddress(supplierPriv.PubKey().Address()).String(),
		appPrivHex:   hex.EncodeToString(appPriv.Bytes()),
		gwPrivHex:    hex.EncodeToString(gwPriv.Bytes()),
		appPubHex:    hex.EncodeToString(appPriv.PubKey().Bytes()),
		gwPubHex:     hex.EncodeToString(gwPriv.PubKey().Bytes()),
	}
}

// newSimVerifierFixture builds a relayer.SimulationVerifier pinned to keys'
// app/gateway pubkeys under simKeyID, with a supplier signing key loaded and
// a fixed clock, backed by a fresh miniredis instance. This is the
// relayer-side counterpart the CLI-built request must satisfy.
func newSimVerifierFixture(t *testing.T, keys simTestKeys, simKeyID string, clock func() time.Time) *relayer.SimulationVerifier {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	redisClient, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	signer, err := relayer.NewResponseSigner(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		map[string]cryptotypes.PrivKey{keys.supplierAddr: keys.supplierPriv},
	)
	require.NoError(t, err)

	cfg := &relayer.SimulationConfig{
		Enabled:                true,
		MaxConcurrent:          4,
		FreshnessWindowSeconds: 30,
		Identities: []relayer.SimIdentity{{
			KeyID:             simKeyID,
			Enabled:           true,
			MaxRPS:            5,
			AppPubKeyHex:      keys.appPubHex,
			GatewayPubKeysHex: []string{keys.gwPubHex},
			AllowedServices:   []string{simCLITestServiceID},
		}},
	}
	require.NoError(t, cfg.Validate())

	verifier, err := relayer.NewSimulationVerifier(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		cfg, redisClient, signer,
		map[string]struct{}{simCLITestServiceID: {}},
		clock,
	)
	require.NoError(t, err)
	return verifier
}

// TestBuildSimulatedRelayRequest_AcceptedBySimulationVerifier is the critical
// compatibility test: a request built by the CLI's BuildSimulatedRelayRequest
// is fed to a relayer.SimulationVerifier configured with the SAME pinned
// pubkeys, and the verifier MUST accept it. This is the proof the CLI and the
// relayer's SimulationVerifier speak the exact same protocol.
func TestBuildSimulatedRelayRequest_AcceptedBySimulationVerifier(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)

	fixedNow := time.Unix(1_700_000_000, 0).UTC()
	clock := func() time.Time { return fixedNow }
	payload := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`)

	const simKeyID = "cli-fixture"
	rr, rrBz, err := rc.BuildSimulatedRelayRequest(
		keys.appPubHex, []string{keys.gwPubHex}, simCLITestServiceID, keys.supplierAddr, payload, fixedNow,
	)
	require.NoError(t, err)
	require.NotNil(t, rr)
	require.NotEmpty(t, rrBz)

	// Session id follows the relayer's simv1 format (relayer.FormatSimSessionID)
	// verbatim: "simv1:<unixSeconds>:<hexNonce>", with the timestamp equal to
	// fixedNow and a non-empty nonce.
	parts := strings.Split(rr.Meta.SessionHeader.SessionId, ":")
	require.Len(t, parts, 3, "session id must be simv1:<unixSeconds>:<hexNonce>")
	require.Equal(t, "simv1", parts[0])
	tsSecs, err := strconv.ParseInt(parts[1], 10, 64)
	require.NoError(t, err)
	require.Equal(t, fixedNow.Unix(), tsSecs)
	require.NotEmpty(t, parts[2], "nonce component must be non-empty")
	require.Equal(t, relayer.FormatSimSessionID(fixedNow, parts[2]), rr.Meta.SessionHeader.SessionId,
		"must match relayer.FormatSimSessionID verbatim, not a hand-rolled format")

	// App address matches the app pubkey, using the same derivation the
	// relayer's admission path uses (cosmostypes.AccAddress + active bech32 prefix).
	expectedAppAddr := cosmostypes.AccAddress(keys.appPriv.PubKey().Address()).String()
	require.Equal(t, expectedAppAddr, rr.Meta.SessionHeader.ApplicationAddress)
	require.Equal(t, keys.supplierAddr, rr.Meta.SupplierOperatorAddress)
	require.Equal(t, simCLITestServiceID, rr.Meta.SessionHeader.ServiceId)
	require.NoError(t, rr.ValidateBasic(), "the built request must satisfy poktroll's RelayRequest.ValidateBasic")

	verifier := newSimVerifierFixture(t, keys, simKeyID, clock)

	err = verifier.Verify(context.Background(), simKeyID, rr)
	require.NoError(t, err, "relayer.SimulationVerifier must accept a CLI-built simulated relay request signed against the same pinned pubkeys")
}

// TestBuildSimulatedRelayRequest_DistinctNoncePerCall proves that two calls
// with the identical `now` still produce distinct session ids/signatures. A
// fixed nonce would make the second identical call collide with the first as
// a replay in the relayer's dedup window.
func TestBuildSimulatedRelayRequest_DistinctNoncePerCall(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)
	fixedNow := time.Unix(1_700_000_000, 0).UTC()
	payload := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`)

	rr1, _, err := rc.BuildSimulatedRelayRequest(keys.appPubHex, []string{keys.gwPubHex}, simCLITestServiceID, keys.supplierAddr, payload, fixedNow)
	require.NoError(t, err)
	rr2, _, err := rc.BuildSimulatedRelayRequest(keys.appPubHex, []string{keys.gwPubHex}, simCLITestServiceID, keys.supplierAddr, payload, fixedNow)
	require.NoError(t, err)

	require.NotEqual(t, rr1.Meta.SessionHeader.SessionId, rr2.Meta.SessionHeader.SessionId,
		"same `now` must still yield distinct session ids so repeated calls are not self-replays")
	require.NotEqual(t, rr1.Meta.Signature, rr2.Meta.Signature)
}

// TestBuildSimulatedRelayRequest_InvalidAppPubKey proves a malformed app
// pubkey hex is rejected up front instead of silently building a bad ring.
func TestBuildSimulatedRelayRequest_InvalidAppPubKey(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)

	_, _, err := rc.BuildSimulatedRelayRequest("not-hex", []string{keys.gwPubHex}, simCLITestServiceID, keys.supplierAddr, nil, time.Now())
	require.Error(t, err)
	require.ErrorContains(t, err, "app pubkey")
}

// TestBuildSimulatedRelayRequest_NoGatewayPubKeys proves an empty gateway
// pubkey list is rejected — a simulated relay's ring must include at least
// one gateway member.
func TestBuildSimulatedRelayRequest_NoGatewayPubKeys(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)

	_, _, err := rc.BuildSimulatedRelayRequest(keys.appPubHex, nil, simCLITestServiceID, keys.supplierAddr, nil, time.Now())
	require.Error(t, err)
	require.ErrorContains(t, err, "gateway pubkey")
}

// TestBuildSimulatedRelayRequest_InvalidGatewayPubKey proves a malformed
// gateway pubkey hex is rejected.
func TestBuildSimulatedRelayRequest_InvalidGatewayPubKey(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)

	_, _, err := rc.BuildSimulatedRelayRequest(keys.appPubHex, []string{"still-not-hex"}, simCLITestServiceID, keys.supplierAddr, nil, time.Now())
	require.Error(t, err)
	require.ErrorContains(t, err, "gateway pubkey")
}

// TestBuildSimulatedRelayRequest_MultiGatewayRing proves a ring with more
// than one gateway pubkey builds and verifies successfully — the relayer
// pins an ordered LIST of gateway pubkeys, not just one.
func TestBuildSimulatedRelayRequest_MultiGatewayRing(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)

	otherGw := secp256k1.GenPrivKey()
	otherGwPubHex := hex.EncodeToString(otherGw.PubKey().Bytes())

	fixedNow := time.Unix(1_700_000_000, 0).UTC()
	payload := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`)

	rr, _, err := rc.BuildSimulatedRelayRequest(
		keys.appPubHex, []string{keys.gwPubHex, otherGwPubHex}, simCLITestServiceID, keys.supplierAddr, payload, fixedNow,
	)
	require.NoError(t, err)
	require.NoError(t, rr.ValidateBasic())
}
