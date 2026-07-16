//go:build test

package relayer

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/rings"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// --- test fixtures ---------------------------------------------------------

type simFixture struct {
	appPriv      *secp256k1.PrivKey
	gwPriv       *secp256k1.PrivKey
	supplierPriv *secp256k1.PrivKey
	supplierAddr string
	cfg          *SimulationConfig
	verifier     *SimulationVerifier
	mr           *miniredis.Miniredis
	redisClient  *redisutil.Client
	clock        func() time.Time
}

const simTestKeyID = "k1"
const simTestService = "svc-test"

// newSimFixture builds a verifier with one enabled identity, a loaded supplier
// signer, a fixed clock, and a fresh miniredis. serviceIDs includes svc-test
// and other-svc so service-binding tests can distinguish unknown vs not-allowed.
func newSimFixture(t *testing.T) *simFixture {
	t.Helper()
	fixed := time.Unix(1_700_000_000, 0).UTC()
	clock := func() time.Time { return fixed }

	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	supplierPriv := secp256k1.GenPrivKey()
	supplierAddr := cosmostypes.AccAddress(supplierPriv.PubKey().Address()).String()

	cfg := &SimulationConfig{
		Enabled:                true,
		MaxConcurrent:          4,
		FreshnessWindowSeconds: 30,
		Identities: []SimIdentity{{
			KeyID:             simTestKeyID,
			Enabled:           true,
			MaxRPS:            5,
			AppPubKeyHex:      hex.EncodeToString(appPriv.PubKey().Bytes()),
			GatewayPubKeysHex: []string{hex.EncodeToString(gwPriv.PubKey().Bytes())},
			AllowedServices:   []string{simTestService},
		}},
	}
	require.NoError(t, cfg.Validate())

	mr, err := miniredis.Run()
	require.NoError(t, err)
	rc, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{URL: fmt.Sprintf("redis://%s", mr.Addr())})
	require.NoError(t, err)

	signer, err := NewResponseSigner(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		map[string]cryptotypes.PrivKey{supplierAddr: supplierPriv},
	)
	require.NoError(t, err)

	serviceIDs := map[string]struct{}{simTestService: {}, "other-svc": {}}
	v, err := NewSimulationVerifier(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		cfg, rc, signer, serviceIDs, clock,
	)
	require.NoError(t, err)

	f := &simFixture{
		appPriv: appPriv, gwPriv: gwPriv, supplierPriv: supplierPriv, supplierAddr: supplierAddr,
		cfg: cfg, verifier: v, mr: mr, redisClient: rc, clock: clock,
	}
	t.Cleanup(func() { _ = rc.Close(); mr.Close() })
	return f
}

// signSimRelay builds a synthetic-session RelayRequest and ring-signs it with
// the gateway key over the ring [appPub, gwPub], mirroring production signing.
func (f *simFixture) signSimRelay(t *testing.T, appAddr, serviceID, sessionID string) *servicetypes.RelayRequest {
	t.Helper()
	rr := &servicetypes.RelayRequest{
		Payload: []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`),
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      appAddr,
				ServiceId:               serviceID,
				SessionId:               sessionID,
				SessionStartBlockHeight: 1,
				SessionEndBlockHeight:   2,
			},
			SupplierOperatorAddress: f.supplierAddr,
			Signature:               nil,
		},
	}
	hash, err := rr.GetSignableBytesHash()
	require.NoError(t, err)

	ring, err := rings.GetRingFromPubKeys([]cryptotypes.PubKey{f.appPriv.PubKey(), f.gwPriv.PubKey()})
	require.NoError(t, err)
	curve := ring_secp256k1.NewCurve()
	scalar, err := curve.DecodeToScalar(f.gwPriv.Bytes())
	require.NoError(t, err)
	sig, err := ring.Sign(hash, scalar)
	require.NoError(t, err)
	sigBz, err := sig.Serialize()
	require.NoError(t, err)
	rr.Meta.Signature = sigBz
	return rr
}

// validRelay returns a fully valid, fresh simulated relay for the fixture.
func (f *simFixture) validRelay(t *testing.T) *servicetypes.RelayRequest {
	appAddr := cosmostypes.AccAddress(f.appPriv.PubKey().Address()).String()
	return f.signSimRelay(t, appAddr, simTestService, FormatSimSessionID(f.clock(), "nonce01"))
}

// --- Verify: happy path + each rejection -----------------------------------

func TestSimVerify_Valid(t *testing.T) {
	f := newSimFixture(t)
	require.NoError(t, f.verifier.Verify(context.Background(), simTestKeyID, f.validRelay(t)))
}

func TestSimVerify_UnknownKeyID(t *testing.T) {
	f := newSimFixture(t)
	require.ErrorIs(t, f.verifier.Verify(context.Background(), "nope", f.validRelay(t)), ErrSimUnknownKeyID)
}

func TestSimVerify_Disabled(t *testing.T) {
	f := newSimFixture(t)
	f.cfg.Identities[0].Enabled = false
	require.NoError(t, f.verifier.Reload(f.cfg))
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, f.validRelay(t)), ErrSimDisabled)
}

func TestSimVerify_Expired(t *testing.T) {
	f := newSimFixture(t)
	f.cfg.Identities[0].NotAfter = f.clock().Add(-time.Hour).Format(time.RFC3339)
	require.NoError(t, f.verifier.Reload(f.cfg))
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, f.validRelay(t)), ErrSimExpired)
}

func TestSimVerify_SupplierMissing(t *testing.T) {
	f := newSimFixture(t)
	rr := f.validRelay(t)
	rr.Meta.SupplierOperatorAddress = ""
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, rr), ErrSimSupplierMissing)
}

func TestSimVerify_SupplierNotLoaded(t *testing.T) {
	f := newSimFixture(t)
	rr := f.validRelay(t)
	rr.Meta.SupplierOperatorAddress = cosmostypes.AccAddress(secp256k1.GenPrivKey().PubKey().Address()).String()
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, rr), ErrSimSupplierNotLoaded)
}

func TestSimVerify_IdentityMismatch(t *testing.T) {
	f := newSimFixture(t)
	otherAddr := cosmostypes.AccAddress(secp256k1.GenPrivKey().PubKey().Address()).String()
	// Header claims a different app address but is still signed by the pinned ring.
	rr := f.signSimRelay(t, otherAddr, simTestService, FormatSimSessionID(f.clock(), "nonce01"))
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, rr), ErrSimIdentityMismatch)
}

func TestSimVerify_ServiceNotAllowed(t *testing.T) {
	f := newSimFixture(t)
	appAddr := cosmostypes.AccAddress(f.appPriv.PubKey().Address()).String()
	rr := f.signSimRelay(t, appAddr, "other-svc", FormatSimSessionID(f.clock(), "nonce01"))
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, rr), ErrSimServiceNotAllowed)
}

func TestSimVerify_ServiceUnknown(t *testing.T) {
	f := newSimFixture(t)
	appAddr := cosmostypes.AccAddress(f.appPriv.PubKey().Address()).String()
	rr := f.signSimRelay(t, appAddr, "ghostsvc", FormatSimSessionID(f.clock(), "nonce01"))
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, rr), ErrSimServiceUnknown)
}

func TestSimVerify_StaleAndFuture(t *testing.T) {
	f := newSimFixture(t)
	appAddr := cosmostypes.AccAddress(f.appPriv.PubKey().Address()).String()
	stale := f.signSimRelay(t, appAddr, simTestService, FormatSimSessionID(f.clock().Add(-time.Hour), "n"))
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, stale), ErrSimStale)
	future := f.signSimRelay(t, appAddr, simTestService, FormatSimSessionID(f.clock().Add(time.Hour), "n"))
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, future), ErrSimStale)
}

func TestSimVerify_BadSessionID(t *testing.T) {
	f := newSimFixture(t)
	appAddr := cosmostypes.AccAddress(f.appPriv.PubKey().Address()).String()
	rr := f.signSimRelay(t, appAddr, simTestService, "garbage-not-simv1")
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, rr), ErrSimBadSessionID)
}

func TestSimVerify_Replay(t *testing.T) {
	f := newSimFixture(t)
	rr := f.validRelay(t)
	require.NoError(t, f.verifier.Verify(context.Background(), simTestKeyID, rr))
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, rr), ErrSimReplay)
}

func TestSimVerify_CrossReplicaReplay(t *testing.T) {
	f := newSimFixture(t)
	// A second verifier sharing the SAME miniredis (HA fleet).
	signer, err := NewResponseSigner(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		map[string]cryptotypes.PrivKey{f.supplierAddr: f.supplierPriv},
	)
	require.NoError(t, err)
	v2, err := NewSimulationVerifier(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		f.cfg, f.redisClient, signer, map[string]struct{}{simTestService: {}, "other-svc": {}}, f.clock,
	)
	require.NoError(t, err)

	rr := f.validRelay(t)
	require.NoError(t, f.verifier.Verify(context.Background(), simTestKeyID, rr))
	require.ErrorIs(t, v2.Verify(context.Background(), simTestKeyID, rr), ErrSimReplay,
		"replay must be rejected fleet-wide via shared Redis dedup, not per-process")
}

func TestSimVerify_BadSignature(t *testing.T) {
	f := newSimFixture(t)
	rr := f.validRelay(t)
	rr.Payload = []byte(`{"tampered":true}`) // mutate signed bytes after signing
	require.ErrorIs(t, f.verifier.Verify(context.Background(), simTestKeyID, rr), ErrSimBadSignature)
}

func TestSimVerify_DedupUnavailableFailsClosed(t *testing.T) {
	f := newSimFixture(t)
	rr := f.validRelay(t)
	f.mr.Close() // Redis down before verify
	err := f.verifier.Verify(context.Background(), simTestKeyID, rr)
	require.ErrorIs(t, err, ErrSimDedupUnavailable, "replay dedup must fail closed when Redis is unreachable")
}

// --- rate limiting ---------------------------------------------------------

func TestSimAllowKey_PerKeyBurst(t *testing.T) {
	f := newSimFixture(t) // MaxRPS 5, fixed clock => no refill
	allowed := 0
	for i := 0; i < 8; i++ {
		if f.verifier.AllowKey(simTestKeyID) {
			allowed++
		}
	}
	require.Equal(t, 5, allowed, "exactly max_rps tokens available with a frozen clock")
	require.False(t, f.verifier.AllowKey("unknown"), "unknown key_id gets no budget")
}

func TestSimAcquireGlobal_ConcurrencyCap(t *testing.T) {
	f := newSimFixture(t) // MaxConcurrent 4
	var releases []func()
	for i := 0; i < 4; i++ {
		rel, ok := f.verifier.AcquireGlobal()
		require.True(t, ok, "slot %d must be available", i)
		releases = append(releases, rel)
	}
	_, ok := f.verifier.AcquireGlobal()
	require.False(t, ok, "5th concurrent acquire must be refused")
	releases[0]()
	rel, ok := f.verifier.AcquireGlobal()
	require.True(t, ok, "releasing a slot frees capacity")
	rel()
	for _, r := range releases[1:] {
		r()
	}
}

// --- hot reload under race -------------------------------------------------

func TestSimReload_ConcurrentWithVerify(t *testing.T) {
	f := newSimFixture(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = f.verifier.Verify(context.Background(), simTestKeyID, f.validRelay(t))
			}
		}
	}()
	for i := 0; i < 50; i++ {
		require.NoError(t, f.verifier.Reload(f.cfg))
	}
	close(stop)
	wg.Wait()
}

// --- T8 extractors ---------------------------------------------------------

func TestSimDirectiveExtractors(t *testing.T) {
	h := http.Header{}
	require.Equal(t, "", SimDirectiveFromHTTP(h).KeyID)
	h.Set(HeaderSimulationKeyID, "kx")
	require.Equal(t, "kx", SimDirectiveFromHTTP(h).KeyID)
	require.Equal(t, "kx", SimDirectiveFromWS(h).KeyID)

	require.Equal(t, "", SimDirectiveFromGRPC(metadata.MD{}).KeyID)
	md := metadata.New(map[string]string{MetaSimulationKeyID: "kg"})
	require.Equal(t, "kg", SimDirectiveFromGRPC(md).KeyID)
}
