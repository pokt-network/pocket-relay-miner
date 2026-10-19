//go:build test

package rings

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/query"
)

// keyedAccountQuerier answers each address from pubKeys or errs, and counts calls.
type keyedAccountQuerier struct {
	mockAccountQuerier
	errs  map[string]error
	calls atomic.Int64
}

func (m *keyedAccountQuerier) GetPubKeyFromAddress(ctx context.Context, address string) (cryptotypes.PubKey, error) {
	m.calls.Add(1)
	if err, ok := m.errs[address]; ok {
		return nil, err
	}
	return m.mockAccountQuerier.GetPubKeyFromAddress(ctx, address)
}

type testIdentity struct {
	priv *secp256k1.PrivKey
	pub  cryptotypes.PubKey
	addr string
}

func newTestIdentity() testIdentity {
	priv := secp256k1.GenPrivKey()
	return testIdentity{priv: priv, pub: priv.PubKey(), addr: createTestAddress(priv.PubKey())}
}

func testSessionHeader(appAddress string, sessionEndHeight int64) *sessiontypes.SessionHeader {
	return &sessiontypes.SessionHeader{
		ApplicationAddress:      appAddress,
		ServiceId:               "test_service",
		SessionId:               "session_123",
		SessionStartBlockHeight: 1,
		SessionEndBlockHeight:   sessionEndHeight,
	}
}

// signedRelayRequest returns a relay request for header, signed by signer
// over a ring of ringKeys.
func signedRelayRequest(t *testing.T, header *sessiontypes.SessionHeader, signer *secp256k1.PrivKey, ringKeys ...cryptotypes.PubKey) *servicetypes.RelayRequest {
	t.Helper()
	rr := &servicetypes.RelayRequest{
		Meta:    servicetypes.RelayRequestMetadata{SessionHeader: header},
		Payload: []byte(`{"jsonrpc":"2.0","method":"test","params":[],"id":1}`),
	}
	r, err := GetRingFromPubKeys(ringKeys)
	require.NoError(t, err)
	signableBz, err := rr.GetSignableBytesHash()
	require.NoError(t, err)
	scalar, err := ring_secp256k1.NewCurve().DecodeToScalar(signer.Key)
	require.NoError(t, err)
	sig, err := r.Sign(signableBz, scalar)
	require.NoError(t, err)
	rr.Meta.Signature, err = sig.Serialize()
	require.NoError(t, err)
	return rr
}

func newTestRingClient(app *apptypes.Application, acc *keyedAccountQuerier, params *sharedtypes.Params) *ringClient {
	return NewRingClient(newNopLogger(), &mockApplicationQuerier{app: app}, acc, &mockSharedQuerier{params: params}).(*ringClient)
}

func accountsOf(ids ...testIdentity) *keyedAccountQuerier {
	keys := make(map[string]cryptotypes.PubKey, len(ids))
	for _, id := range ids {
		keys[id.addr] = id.pub
	}
	return &keyedAccountQuerier{mockAccountQuerier: mockAccountQuerier{pubKeys: keys}, errs: map[string]error{}}
}

// requireSizeRejection checks err comes from validateRingSignatureSize.
func requireSizeRejection(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequestSignature)
	require.Regexp(t, `ring signature (is|has) \d+ (bytes|members)`, err.Error())
}

func TestVerifyRelayRequestSignature_SignatureSizeIsCheckedFirst(t *testing.T) {
	app := newTestIdentity()
	gw := newTestIdentity()
	header := testSessionHeader(app.addr, 100)
	valid := signedRelayRequest(t, header, app.priv, app.pub, gw.pub).Meta.Signature

	withCount := func(n uint32) []byte {
		bz := append([]byte(nil), valid...)
		binary.BigEndian.PutUint32(bz[:4], n)
		return bz
	}
	cases := map[string][]byte{
		"empty":                   {},
		"one byte":                {0x00},
		"three bytes":             {0x00, 0x00, 0x00},
		"count only":              {0x00, 0x00, 0x00, 0x02},
		"one member":              withCount(1),
		"above the member bound":  withCount(MaxRingSignatureMembers + 1),
		"max uint32 members":      withCount(^uint32(0)),
		"one byte short":          valid[:len(valid)-1],
		"one byte long":           append(append([]byte(nil), valid...), 0x00),
		"count says one more":     withCount(3),
		"count says zero members": withCount(0),
	}

	for name, sig := range cases {
		t.Run(name, func(t *testing.T) {
			acc := accountsOf(app, gw)
			rc := newTestRingClient(&apptypes.Application{Address: app.addr, DelegateeGatewayAddresses: []string{gw.addr}}, acc, nil)
			rr := &servicetypes.RelayRequest{Meta: servicetypes.RelayRequestMetadata{SessionHeader: header, Signature: sig}}

			var err error
			require.NotPanics(t, func() { err = rc.VerifyRelayRequestSignature(context.Background(), rr) })
			requireSizeRejection(t, err)

			// Wrong sizes are rejected before the ring is looked up.
			require.Zero(t, acc.calls.Load(), "no account query for a signature of the wrong size")

			points, pErr := PrecomputeRingPoints([]cryptotypes.PubKey{app.pub, gw.pub})
			require.NoError(t, pErr)
			require.NotPanics(t, func() { err = VerifySignatureAgainstPoints(rr, points) })
			requireSizeRejection(t, err)
		})
	}
}

func TestVerifyRelayRequestSignature_MissingSessionHeader(t *testing.T) {
	rc := newTestRingClient(&apptypes.Application{}, accountsOf(), nil)
	rr := &servicetypes.RelayRequest{Meta: servicetypes.RelayRequestMetadata{Signature: []byte{0x01}}}

	var err error
	require.NotPanics(t, func() { err = rc.VerifyRelayRequestSignature(context.Background(), rr) })
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequest)
}

func TestVerifyRelayRequestSignature_AppWithoutGatewaysSignsWithItsOwnKeyTwice(t *testing.T) {
	app := newTestIdentity()
	header := testSessionHeader(app.addr, 100)
	rc := newTestRingClient(&apptypes.Application{Address: app.addr}, accountsOf(app), nil)

	ok := signedRelayRequest(t, header, app.priv, app.pub, app.pub)
	require.NoError(t, rc.VerifyRelayRequestSignature(context.Background(), ok))

	padding := secp256k1.GenPrivKeyFromSecret([]byte("placeholder_ring_key_for_apps_without_gateways"))
	old := signedRelayRequest(t, header, padding, app.pub, padding.PubKey())
	require.ErrorIs(t, rc.VerifyRelayRequestSignature(context.Background(), old), ErrRingClientInvalidRelayRequestSignature)

	// The signer side builds the same ring.
	r, err := rc.GetRingForAddressAtHeight(context.Background(), app.addr, 100)
	require.NoError(t, err)
	require.Equal(t, 2, r.Size())
}

func TestVerifyRelayRequestSignature_SignatureMembersBoundedByExpectedRing(t *testing.T) {
	app := newTestIdentity()
	header := testSessionHeader(app.addr, 100)
	rc := newTestRingClient(&apptypes.Application{Address: app.addr}, accountsOf(app), nil)

	// Every member is a key of the expected ring, but there are more of them.
	extra := signedRelayRequest(t, header, app.priv, app.pub, app.pub, app.pub)
	require.ErrorIs(t, rc.VerifyRelayRequestSignature(context.Background(), extra), ErrRingClientInvalidRelayRequestSignature)
}

func TestVerifyRelayRequestSignature_AppDelegatedToItselfKeepsBothMembers(t *testing.T) {
	app := newTestIdentity()
	header := testSessionHeader(app.addr, 100)
	rc := newTestRingClient(&apptypes.Application{Address: app.addr, DelegateeGatewayAddresses: []string{app.addr}}, accountsOf(app), nil)

	rr := signedRelayRequest(t, header, app.priv, app.pub, app.pub)
	require.NoError(t, rc.VerifyRelayRequestSignature(context.Background(), rr))
}

// The ring is the one of the header's session, whatever the session length is now.
func TestVerifyRelayRequestSignature_RingIsBuiltAtTheHeaderSessionEndHeight(t *testing.T) {
	app := newTestIdentity()
	gw := newTestIdentity()
	header := testSessionHeader(app.addr, 100)
	// gw undelegated at 110, after the session ending at 100: still a member.
	chainApp := &apptypes.Application{
		Address: app.addr,
		PendingUndelegations: map[uint64]apptypes.UndelegatingGatewayList{
			110: {GatewayAddresses: []string{gw.addr}},
		},
	}
	// With 30-block sessions, height 100 would fall in the session ending at 120.
	params := &sharedtypes.Params{NumBlocksPerSession: 30}
	rc := newTestRingClient(chainApp, accountsOf(app, gw), params)

	rr := signedRelayRequest(t, header, gw.priv, app.pub, gw.pub)
	require.NoError(t, rc.VerifyRelayRequestSignature(context.Background(), rr))
}

func TestVerifyRelayRequestSignature_GatewaysWithoutUsableKeyAreLeftOut(t *testing.T) {
	app := newTestIdentity()
	gw := newTestIdentity()
	header := testSessionHeader(app.addr, 100)
	noAccount, noPubKey, otherCurve := newTestIdentity(), newTestIdentity(), newTestIdentity()

	acc := accountsOf(app, gw)
	acc.errs[noAccount.addr] = fmt.Errorf("failed to query account: %w", status.Error(codes.NotFound, "account not found"))
	acc.errs[noPubKey.addr] = fmt.Errorf("wrapped: %w", query.ErrAccountPubKeyNotFound)
	acc.pubKeys[otherCurve.addr] = ed25519.GenPrivKey().PubKey()

	chainApp := &apptypes.Application{
		Address:                   app.addr,
		DelegateeGatewayAddresses: []string{noAccount.addr, gw.addr, noPubKey.addr, otherCurve.addr},
	}
	rc := newTestRingClient(chainApp, acc, nil)

	rr := signedRelayRequest(t, header, gw.priv, app.pub, gw.pub)
	require.NoError(t, rc.VerifyRelayRequestSignature(context.Background(), rr))

	r, err := rc.GetRingForAddressAtHeight(context.Background(), app.addr, 100)
	require.NoError(t, err)
	require.Equal(t, 2, r.Size())
}

func TestGetRingForAddressAtHeight_AppKeyIsRequired(t *testing.T) {
	app := newTestIdentity()
	gw := newTestIdentity()
	acc := accountsOf(gw)
	acc.errs[app.addr] = fmt.Errorf("wrapped: %w", query.ErrAccountPubKeyNotFound)
	rc := newTestRingClient(&apptypes.Application{Address: app.addr, DelegateeGatewayAddresses: []string{gw.addr}}, acc, nil)

	_, err := rc.GetRingForAddressAtHeight(context.Background(), app.addr, 100)
	require.ErrorIs(t, err, query.ErrAccountPubKeyNotFound)
}

func TestGetRingForAddressAtHeight_GatewayQueryFailureIsReturned(t *testing.T) {
	app := newTestIdentity()
	gw := newTestIdentity()
	acc := accountsOf(app)
	unavailable := status.Error(codes.Unavailable, "connection refused")
	acc.errs[gw.addr] = fmt.Errorf("failed to query account: %w", unavailable)
	rc := newTestRingClient(&apptypes.Application{Address: app.addr, DelegateeGatewayAddresses: []string{gw.addr}}, acc, nil)

	_, err := rc.GetRingForAddressAtHeight(context.Background(), app.addr, 100)
	require.ErrorIs(t, err, unavailable)

	header := testSessionHeader(app.addr, 100)
	rr := signedRelayRequest(t, header, app.priv, app.pub, gw.pub)
	require.ErrorIs(t, rc.VerifyRelayRequestSignature(context.Background(), rr), ErrRingClientInvalidRelayRequest)
}

func TestGetRingAddressesAtSessionEndHeight_DoesNotWriteIntoTheApplication(t *testing.T) {
	delegations := make([]string, 1, 4)
	delegations[0] = "gateway1"
	app := &apptypes.Application{
		DelegateeGatewayAddresses: delegations,
		PendingUndelegations: map[uint64]apptypes.UndelegatingGatewayList{
			200: {GatewayAddresses: []string{"gateway2"}},
		},
	}

	got := GetRingAddressesAtSessionEndHeight(app, 100)
	require.ElementsMatch(t, []string{"gateway1", "gateway2"}, got)
	require.Equal(t, []string{"gateway1"}, app.DelegateeGatewayAddresses)
	require.Equal(t, "", delegations[:2][1], "the application's backing array is unchanged")
}
