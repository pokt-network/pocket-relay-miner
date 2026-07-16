//go:build test

package rings

import (
	"encoding/hex"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
)

// buildSignedRelayRequest builds a RelayRequest whose session header
// application address is derived from appPubKey, ring-signs it with
// signerPriv over the ring [appPubKey, signerPriv.PubKey()], and returns the
// signed request. Mirrors the production signing sequence in
// relay_client.Signer.SignRelayRequestWithRing (client/relay_client/signer.go:193):
// assemble the request with a nil signature, hash the signable bytes, build
// the ring from the member pubkeys, sign with the chosen member's scalar,
// then serialize the ring signature into Meta.Signature.
func buildSignedRelayRequest(
	t *testing.T,
	appPubKey cryptotypes.PubKey,
	signerPriv *secp256k1.PrivKey,
) *servicetypes.RelayRequest {
	t.Helper()

	return buildSignedRelayRequestWithRing(
		t,
		appPubKey,
		[]cryptotypes.PubKey{appPubKey, signerPriv.PubKey()},
		signerPriv,
	)
}

// buildSignedRelayRequestWithRing is the general form of buildSignedRelayRequest:
// it lets the caller control the full ring membership independently of the
// signer, which negative tests need (e.g. signing over a ring that is not the
// one pinned in the verifier's points map).
func buildSignedRelayRequestWithRing(
	t *testing.T,
	appPubKey cryptotypes.PubKey,
	ringPubKeys []cryptotypes.PubKey,
	signerPriv *secp256k1.PrivKey,
) *servicetypes.RelayRequest {
	t.Helper()

	relayRequest := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      createTestAddress(appPubKey),
				ServiceId:               "test_service",
				SessionId:               "session_123",
				SessionStartBlockHeight: 1,
				SessionEndBlockHeight:   100,
			},
			// Signature is nil at this point; GetSignableBytesHash below
			// zeroes it anyway, but leaving it unset here mirrors the
			// production signer, which signs before Meta.Signature exists.
		},
		Payload: []byte(`{"jsonrpc":"2.0","method":"test","params":[],"id":1}`),
	}

	signableBz, err := relayRequest.GetSignableBytesHash()
	require.NoError(t, err)

	appRing, err := GetRingFromPubKeys(ringPubKeys)
	require.NoError(t, err)

	curve := ring_secp256k1.NewCurve()
	scalar, err := curve.DecodeToScalar(signerPriv.Key)
	require.NoError(t, err)

	ringSig, err := appRing.Sign(signableBz, scalar)
	require.NoError(t, err)

	sigBz, err := ringSig.Serialize()
	require.NoError(t, err)

	relayRequest.Meta.Signature = sigBz

	return relayRequest
}

// A valid ring sig over [appPub, gwPub] verifies against precomputed points.
func TestVerifySignatureAgainstPoints_ValidRingSig(t *testing.T) {
	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	pts, err := PrecomputeRingPoints([]cryptotypes.PubKey{appPriv.PubKey(), gwPriv.PubKey()})
	require.NoError(t, err)
	req := buildSignedRelayRequest(t, appPriv.PubKey(), gwPriv) // helper: builds RelayRequest, ring-signs with gwPriv over ring [appPub,gwPub]
	require.NoError(t, VerifySignatureAgainstPoints(req, pts))
}

// R1: IsPlaceholderPubKey identifies the deterministic padding key whose
// private half is publicly derivable. This is the primitive the config
// validator (Task 2) and verifier (Task 6) use to REFUSE pinning it.
func TestIsPlaceholderPubKey(t *testing.T) {
	phPriv := secp256k1.GenPrivKeyFromSecret([]byte("placeholder_ring_key_for_apps_without_gateways"))
	require.True(t, IsPlaceholderPubKey(phPriv.PubKey()), "must match rings.PlaceholderRingPubKey")
	require.False(t, IsPlaceholderPubKey(secp256k1.GenPrivKey().PubKey()))
}

// TestIsPlaceholderPubKey_NilPubKey verifies the nil-safety guard: a nil
// interface must not panic and must not be treated as the placeholder.
func TestIsPlaceholderPubKey_NilPubKey(t *testing.T) {
	require.False(t, IsPlaceholderPubKey(nil))
}

// TestVerifySignatureAgainstPoints_TamperedPayload verifies that mutating
// the relay request's payload after signing invalidates the signature: the
// signature covers GetSignableBytesHash(), which is derived from the full
// request including Payload, so a tampered payload must fail verification
// even though the ring signature bytes themselves are untouched and still
// belong to the pinned ring.
func TestVerifySignatureAgainstPoints_TamperedPayload(t *testing.T) {
	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()

	pts, err := PrecomputeRingPoints([]cryptotypes.PubKey{appPriv.PubKey(), gwPriv.PubKey()})
	require.NoError(t, err)

	req := buildSignedRelayRequest(t, appPriv.PubKey(), gwPriv)

	// Mutate the payload after signing.
	req.Payload = []byte(`{"jsonrpc":"2.0","method":"tampered","params":[],"id":1}`)

	err = VerifySignatureAgainstPoints(req, pts)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequestSignature)
	require.Contains(t, err.Error(), "invalid signature")
}

// TestVerifySignatureAgainstPoints_RingNotInPinnedSet verifies that a
// validly-signed request over a DIFFERENT ring than the one pinned in the
// points map is rejected before signature verification even runs, because
// the ring signature's member points are not a subset of the pinned set.
func TestVerifySignatureAgainstPoints_RingNotInPinnedSet(t *testing.T) {
	// Points pinned for one ring.
	pinnedAppPriv := secp256k1.GenPrivKey()
	pinnedGwPriv := secp256k1.GenPrivKey()
	pts, err := PrecomputeRingPoints([]cryptotypes.PubKey{pinnedAppPriv.PubKey(), pinnedGwPriv.PubKey()})
	require.NoError(t, err)

	// The request is validly ring-signed, but over a completely different
	// (unpinned) ring.
	otherAppPriv := secp256k1.GenPrivKey()
	otherGwPriv := secp256k1.GenPrivKey()
	req := buildSignedRelayRequest(t, otherAppPriv.PubKey(), otherGwPriv)

	err = VerifySignatureAgainstPoints(req, pts)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequestSignature)
	require.Contains(t, err.Error(), "ring not in pinned set")
}

// TestVerifySignatureAgainstPoints_MissingSignature verifies the nil
// signature guard returns ErrRingClientInvalidRelayRequest, matching
// ringClient.VerifyRelayRequestSignature's behavior for the same case.
func TestVerifySignatureAgainstPoints_MissingSignature(t *testing.T) {
	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	pts, err := PrecomputeRingPoints([]cryptotypes.PubKey{appPriv.PubKey(), gwPriv.PubKey()})
	require.NoError(t, err)

	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      createTestAddress(appPriv.PubKey()),
				ServiceId:               "test_service",
				SessionId:               "session_123",
				SessionStartBlockHeight: 1,
				SessionEndBlockHeight:   100,
			},
			Signature: nil,
		},
		Payload: []byte(`{"test":"data"}`),
	}

	err = VerifySignatureAgainstPoints(req, pts)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequest)
	require.Contains(t, err.Error(), "missing signature")
}

// TestVerifySignatureAgainstPoints_MalformedSignatureBytes verifies that
// bytes which don't deserialize into a ring signature at all produce
// ErrRingClientInvalidRelayRequestSignature rather than panicking.
func TestVerifySignatureAgainstPoints_MalformedSignatureBytes(t *testing.T) {
	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	pts, err := PrecomputeRingPoints([]cryptotypes.PubKey{appPriv.PubKey(), gwPriv.PubKey()})
	require.NoError(t, err)

	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      createTestAddress(appPriv.PubKey()),
				ServiceId:               "test_service",
				SessionId:               "session_123",
				SessionStartBlockHeight: 1,
				SessionEndBlockHeight:   100,
			},
			Signature: []byte("not-a-valid-ring-signature"),
		},
		Payload: []byte(`{"test":"data"}`),
	}

	err = VerifySignatureAgainstPoints(req, pts)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequestSignature)
}

// TestPubKeyFromHex_Valid verifies a compressed secp256k1 pubkey round-trips
// through hex encoding.
func TestPubKeyFromHex_Valid(t *testing.T) {
	priv := secp256k1.GenPrivKey()
	pub := priv.PubKey()
	hexStr := hex.EncodeToString(pub.Bytes())

	parsed, err := PubKeyFromHex(hexStr)
	require.NoError(t, err)
	require.NotNil(t, parsed)
	require.True(t, parsed.Equals(pub))
	require.Equal(t, pub.Bytes(), parsed.Bytes())
}

// TestPubKeyFromHex_MalformedHex verifies non-hex input errors instead of
// silently truncating or panicking.
func TestPubKeyFromHex_MalformedHex(t *testing.T) {
	parsed, err := PubKeyFromHex("not-valid-hex-zzzz")
	require.Error(t, err)
	require.Nil(t, parsed)
}

// TestPubKeyFromHex_WrongLength verifies well-formed hex that doesn't decode
// to exactly 33 bytes (compressed secp256k1) is rejected.
func TestPubKeyFromHex_WrongLength(t *testing.T) {
	tests := []struct {
		name string
		hex  string
	}{
		{"too short", hex.EncodeToString([]byte{0x01, 0x02, 0x03})},
		{"too long", hex.EncodeToString(make([]byte, 65))},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := PubKeyFromHex(tt.hex)
			require.Error(t, err)
			require.Nil(t, parsed)
		})
	}
}

// TestPrecomputeRingPoints_Valid verifies the returned map is keyed by each
// pubkey's encoded curve point, matching the shape
// ringClient.getRingPointsForAddressAtHeight builds for on-chain rings.
func TestPrecomputeRingPoints_Valid(t *testing.T) {
	appPriv := secp256k1.GenPrivKey()
	gwPriv := secp256k1.GenPrivKey()
	pubKeys := []cryptotypes.PubKey{appPriv.PubKey(), gwPriv.PubKey()}

	pts, err := PrecomputeRingPoints(pubKeys)
	require.NoError(t, err)
	require.Len(t, pts, 2)

	curve := ring_secp256k1.NewCurve()
	for _, pk := range pubKeys {
		point, err := curve.DecodeToPoint(pk.Bytes())
		require.NoError(t, err)

		got, ok := pts[string(point.Encode())]
		require.True(t, ok, "expected point for pubkey %x to be present", pk.Bytes())
		require.Equal(t, point.Encode(), got.Encode())
	}
}

// TestPrecomputeRingPoints_NonSecp256k1Key verifies non-secp256k1 keys are
// rejected with the same sentinel error pointsFromPublicKeys returns.
func TestPrecomputeRingPoints_NonSecp256k1Key(t *testing.T) {
	invalidKey := ed25519.GenPrivKey().PubKey()

	pts, err := PrecomputeRingPoints([]cryptotypes.PubKey{invalidKey})
	require.Error(t, err)
	require.Nil(t, pts)
	require.ErrorIs(t, err, ErrRingsNotSecp256k1Curve)
}

// TestPrecomputeRingPoints_EmptyKeys verifies an empty ring produces an
// empty (not nil-panicking) points map, matching pointsFromPublicKeys'
// behavior for empty input.
func TestPrecomputeRingPoints_EmptyKeys(t *testing.T) {
	pts, err := PrecomputeRingPoints(nil)
	require.NoError(t, err)
	require.Empty(t, pts)
}
