package rings

import (
	"encoding/hex"
	"fmt"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	ringtypes "github.com/pokt-network/go-dleq/types"
	"github.com/pokt-network/poktroll/x/service/types"
	"github.com/pokt-network/ring-go"
)

// PubKeyFromHex parses a compressed secp256k1 public key from its hex
// encoding (33 bytes / 66 hex characters). This is how a pinned ring member
// public key from config (rather than an on-chain account query) is loaded.
func PubKeyFromHex(hexStr string) (cryptotypes.PubKey, error) {
	keyBz, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("invalid pubkey hex: %w", err)
	}

	if len(keyBz) != secp256k1.PubKeySize {
		return nil, fmt.Errorf(
			"invalid pubkey length: expected %d bytes (compressed secp256k1), got %d",
			secp256k1.PubKeySize, len(keyBz),
		)
	}

	return &secp256k1.PubKey{Key: keyBz}, nil
}

// PrecomputeRingPoints converts the given public keys to secp256k1 curve
// points and returns them keyed by their encoded bytes. This is the same
// map shape ringClient.getRingPointsForAddressAtHeight builds on chain
// (rings/client.go:294-340), but computed once from config-pinned pubkeys
// instead of an application/gateway on-chain lookup. Callers reuse the
// returned map across every relay verified with VerifySignatureAgainstPoints
// for a given pinned ring, avoiding repeated DecodeToPoint calls.
func PrecomputeRingPoints(pubKeys []cryptotypes.PubKey) (map[string]ringtypes.Point, error) {
	points, err := pointsFromPublicKeys(pubKeys...)
	if err != nil {
		return nil, err
	}

	ringPoints := make(map[string]ringtypes.Point, len(points))
	for _, point := range points {
		// Use the point's encoded bytes as the key, matching
		// ringPointsContain's lookup and getRingPointsForAddressAtHeight's
		// cache shape.
		ringPoints[string(point.Encode())] = point
	}

	return ringPoints, nil
}

// VerifySignatureAgainstPoints verifies a relay request's ring signature
// against a precomputed set of pinned ring points, with no on-chain lookup.
// It mirrors the crypto steps of ringClient.VerifyRelayRequestSignature
// (rings/client.go:148-215) minus the on-chain ring source: deserialize the
// request's ring signature, confirm every one of its ring member points is
// present in the pinned set, then verify the signature over the request's
// signable bytes hash.
func VerifySignatureAgainstPoints(rr *types.RelayRequest, points map[string]ringtypes.Point) error {
	rrMeta := rr.GetMeta()
	sigBz := rrMeta.GetSignature()
	if sigBz == nil {
		return ErrRingClientInvalidRelayRequest.Wrap("missing signature")
	}

	sig := new(ring.RingSig)
	if err := sig.Deserialize(ringCurve, sigBz); err != nil {
		return ErrRingClientInvalidRelayRequestSignature.Wrapf("deserialize: %s", err)
	}

	if !ringPointsContain(points, sig) {
		return ErrRingClientInvalidRelayRequestSignature.Wrap("ring not in pinned set")
	}

	signableBz, err := rr.GetSignableBytesHash()
	if err != nil {
		return ErrRingClientInvalidRelayRequest.Wrapf("signable bytes: %v", err)
	}

	if !sig.Verify(signableBz) {
		return ErrRingClientInvalidRelayRequestSignature.Wrap("invalid signature")
	}

	return nil
}

// IsPlaceholderPubKey reports whether pk is the deterministic placeholder
// ring key (PlaceholderRingPubKey) used to pad rings for applications
// without delegated gateways. Its private half is publicly derivable (it's
// generated from a well-known seed), so it must never be accepted as a
// pinned ring member: the config validator (Task 2) and admission verifier
// (Task 6) call this to refuse pinning it. Returns false for a nil pk rather
// than panicking, since callers may check untrusted/partially-parsed input.
func IsPlaceholderPubKey(pk cryptotypes.PubKey) bool {
	if pk == nil {
		return false
	}
	return pk.Equals(PlaceholderRingPubKey)
}
