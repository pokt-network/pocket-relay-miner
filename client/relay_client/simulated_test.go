//go:build test

package relay_client

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
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
func newSimTestRelayClient(tb testing.TB, appPrivHex, gwPrivHex string) *RelayClient {
	tb.Helper()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	qc, err := query.NewQueryClients(logger, query.ClientConfig{GRPCEndpoint: "127.0.0.1:1"})
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = qc.Close() })

	rc, err := NewRelayClient(Config{
		AppPrivateKeyHex:     appPrivHex,
		GatewayPrivateKeyHex: gwPrivHex,
		QueryClients:         qc,
	}, logger)
	require.NoError(tb, err)
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

// --- pinned simulation ring cache ---

// The pinned ring is a pure function of the pubkeys it is built from, and the
// CLI load generator passes the SAME pubkeys on every one of its N requests.
// Rebuilding it per call re-runs secp256k1 point decompression for every ring
// member twice (once in rings.PubKeyFromHex's curve check, once in
// rings.GetRingFromPubKeys), which at ~48us per key dominates the builder.
// The ring must therefore be built once and reused.
func TestSimRingCache_ReusesRingAcrossCalls(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)

	first, err := rc.simRingFor(keys.appPubHex, []string{keys.gwPubHex})
	require.NoError(t, err)
	second, err := rc.simRingFor(keys.appPubHex, []string{keys.gwPubHex})
	require.NoError(t, err)

	require.Same(t, first, second, "the same pinned pubkeys must yield the cached ring, not a rebuilt one")
}

// Distinct pinned rings must not collide: a second identity has to get its own
// ring and its own app address, or relays would be signed against the wrong
// ring and rejected.
func TestSimRingCache_DistinctKeysGetDistinctEntries(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)
	otherGwPubHex := hex.EncodeToString(secp256k1.GenPrivKey().PubKey().Bytes())

	one, err := rc.simRingFor(keys.appPubHex, []string{keys.gwPubHex})
	require.NoError(t, err)
	two, err := rc.simRingFor(keys.appPubHex, []string{otherGwPubHex})
	require.NoError(t, err)

	require.NotSame(t, one, two, "different gateway sets must not share a cache entry")
	require.Equal(t, one.appAddress, two.appAddress, "same app pubkey must derive the same app address")

	three, err := rc.simRingFor(otherGwPubHex, []string{keys.gwPubHex})
	require.NoError(t, err)
	require.NotEqual(t, one.appAddress, three.appAddress, "a different app pubkey must derive a different address")
}

// The cache key must not let a different (app, gateways) split collide by
// concatenation — the classic delimiter bug, where {a, [b,c]} and {a+b, [c]}
// hash to the same string.
func TestSimRingCache_KeyIsUnambiguous(t *testing.T) {
	a := hex.EncodeToString(secp256k1.GenPrivKey().PubKey().Bytes())
	b := hex.EncodeToString(secp256k1.GenPrivKey().PubKey().Bytes())
	c := hex.EncodeToString(secp256k1.GenPrivKey().PubKey().Bytes())

	require.NotEqual(t,
		simRingCacheKey(a, []string{b, c}),
		simRingCacheKey(a+b, []string{c}),
		"the cache key must not be ambiguous across the app/gateway boundary")
	require.NotEqual(t,
		simRingCacheKey(a, []string{b, c}),
		simRingCacheKey(a, []string{b + c}),
		"the cache key must not be ambiguous across the gateway separator")
}

// A malformed pubkey must keep failing on every call: caching a failure would
// be indistinguishable from caching a success at the call site, and the error
// must name the field every time.
func TestSimRingCache_DoesNotCacheFailures(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)

	for i := 0; i < 3; i++ {
		_, err := rc.simRingFor("not-hex", []string{keys.gwPubHex})
		require.Error(t, err, "call %d must fail", i)
		require.Contains(t, err.Error(), "app pubkey")
	}

	// A valid build after failures still works (the failures poisoned nothing).
	got, err := rc.simRingFor(keys.appPubHex, []string{keys.gwPubHex})
	require.NoError(t, err)
	require.NotNil(t, got)
}

// The load generator drives this from many goroutines at once (-c 200), so the
// cache must be safe under concurrent first-touch of the same key.
func TestSimRingCache_ConcurrentAccess(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)

	const goroutines = 32
	results := make(chan *simPinnedRing, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := rc.simRingFor(keys.appPubHex, []string{keys.gwPubHex})
			require.NoError(t, err)
			results <- got
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	first := <-results
	require.NotNil(t, first)
	for got := range results {
		require.Same(t, first, got, "every goroutine must observe the same cached ring")
	}
}

// End-to-end: caching the ring must not freeze anything that has to stay
// per-request. Two successive builds share an app address but must carry
// distinct session ids (fresh nonce) and distinct signatures, or the relayer
// would reject the second as a replay.
func TestBuildSimulatedRelayRequest_CachedRingKeepsPerRequestFieldsFresh(t *testing.T) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(t, keys.appPrivHex, keys.gwPrivHex)
	now := time.Unix(1700000000, 0).UTC()

	first, _, err := rc.BuildSimulatedRelayRequest(
		keys.appPubHex, []string{keys.gwPubHex}, simCLITestServiceID, keys.supplierAddr, []byte(`{"a":1}`), now,
	)
	require.NoError(t, err)
	second, _, err := rc.BuildSimulatedRelayRequest(
		keys.appPubHex, []string{keys.gwPubHex}, simCLITestServiceID, keys.supplierAddr, []byte(`{"a":1}`), now,
	)
	require.NoError(t, err)

	require.Equal(t,
		first.GetMeta().SessionHeader.ApplicationAddress,
		second.GetMeta().SessionHeader.ApplicationAddress,
		"the app address is derived from the pinned key and must be stable")
	require.NotEqual(t,
		first.GetMeta().SessionHeader.SessionId,
		second.GetMeta().SessionHeader.SessionId,
		"each call must draw a fresh nonce; a cached ring must not freeze the session id")
	require.NotEqual(t, first.GetMeta().Signature, second.GetMeta().Signature,
		"each call must produce a distinct signature, or the relayer dedups the second as a replay")
	require.NoError(t, first.ValidateBasic())
	require.NoError(t, second.ValidateBasic())
}

// BenchmarkBuildSimulatedRelayRequest guards the pinned-ring cache. This is the
// CLI load generator's per-request path: `relay <transport> --simulate -n N`
// builds one of these per request, so its cost is subtracted from the RPS the
// operator reads off the run. Rebuilding the ring per call (decompressing every
// ring member to a curve point twice, at ~48us/key) measured ~1.35ms/op and 180
// allocs; reusing it measures ~1.16ms/op and 111 allocs on the same machine.
//
// A regression here does not break correctness — it makes a --simulate load
// test silently under-report the relayer by measuring its own client.
func BenchmarkBuildSimulatedRelayRequest(b *testing.B) {
	keys := newSimTestKeys()
	rc := newSimTestRelayClient(b, keys.appPrivHex, keys.gwPrivHex)
	now := time.Unix(1700000000, 0).UTC()
	payload := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := rc.BuildSimulatedRelayRequest(
			keys.appPubHex, []string{keys.gwPubHex}, simCLITestServiceID, keys.supplierAddr, payload, now,
		); err != nil {
			b.Fatal(err)
		}
	}
}
