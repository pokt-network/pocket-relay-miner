package rings

import (
	"encoding/hex"
	"fmt"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	ringtypes "github.com/pokt-network/go-dleq/types"
	"github.com/pokt-network/poktroll/x/service/types"
)

// PubKeyFromHex parses a compressed secp256k1 public key from its hex
// encoding (33 bytes / 66 hex characters). This is how a pinned ring member
// public key from config (rather than an on-chain account query) is loaded.
//
// The key is decoded all the way to a curve point, not just length-checked:
// a well-formed 33-byte blob with a valid 0x02/0x03 prefix can still carry an
// x coordinate that is not on secp256k1. Without this check such a key passes
// config validation and only fails later at ring-point precompute, surfacing
// a low-level crypto error at startup instead of a config error at the field
// that caused it.
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

	pubKey := &secp256k1.PubKey{Key: keyBz}
	if _, err := pointsFromPublicKeys(pubKey); err != nil {
		return nil, fmt.Errorf("pubkey is not a valid secp256k1 curve point: %w", err)
	}

	return pubKey, nil
}

// PrecomputeRingPoints converts the given public keys to secp256k1 curve
// points and returns them keyed by their encoded bytes. This is the same
// map shape ringClient.getRingPointsForAddressAtSessionEndHeight builds, but
// computed once from config-pinned pubkeys instead of an application/gateway
// on-chain lookup. Callers reuse the
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
		// ringPointsContain's lookup and getRingPointsForAddressAtSessionEndHeight's
		// cache shape.
		ringPoints[string(point.Encode())] = point
	}

	return ringPoints, nil
}

// VerifySignatureAgainstPoints verifies a relay request's ring signature
// against a precomputed set of pinned ring points, with no on-chain lookup.
// It mirrors the crypto steps of ringClient.VerifyRelayRequestSignature minus
// the on-chain ring source: check the signature's size, deserialize it, confirm
// every one of its ring member points is present in the pinned set, then verify
// the signature over the request's signable bytes hash.
func VerifySignatureAgainstPoints(rr *types.RelayRequest, points map[string]ringtypes.Point) error {
	rrMeta := rr.GetMeta()
	sigBz := rrMeta.GetSignature()
	if sigBz == nil {
		return ErrRingClientInvalidRelayRequest.Wrap("missing signature")
	}

	// Same size check as the onchain path, before deserializing. The member
	// bound is the fixed one: points is deduplicated, and a ring can list a key
	// more than once.
	if err := validateRingSignatureSize(sigBz, MaxRingSignatureMembers); err != nil {
		return err
	}
	return verifyRingSignature(sigBz, points, rr, rrMeta.GetSessionHeader().GetApplicationAddress())
}

// placeholderRingPubKey is the deterministic key earlier versions padded the
// ring of an application without gateways with. Rings no longer contain it, but
// its private half is publicly derivable (it is generated from a well-known
// seed), so pinning it would let anyone sign as that identity.
var placeholderRingPubKey = secp256k1.GenPrivKeyFromSecret([]byte("placeholder_ring_key_for_apps_without_gateways")).PubKey()

// IsPlaceholderPubKey reports whether pk is placeholderRingPubKey, which must
// never be accepted as a pinned ring member: the simulation config validator
// calls this to refuse pinning it. Returns false for a nil pk rather than
// panicking, since callers may check untrusted/partially-parsed input.
func IsPlaceholderPubKey(pk cryptotypes.PubKey) bool {
	if pk == nil {
		return false
	}
	return pk.Equals(placeholderRingPubKey)
}
