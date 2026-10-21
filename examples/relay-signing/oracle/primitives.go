package main

import (
	"crypto/sha3"
	"encoding/hex"
	"math/big"

	dsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	dleqtypes "github.com/pokt-network/go-dleq/types"
)

// dleqTypesPoint aliases the go-dleq point interface so main.go reads without
// a second import of the types package.
type dleqTypesPoint = dleqtypes.Point

// The three functions below mirror primitives that are unexported in ring-go
// and go-dleq, so that `oracle vectors` can publish their intermediate values.
// They are mirrors, not the source of truth: `oracle verify` always calls the
// real ring-go. Their fidelity is covered by oracle_test.go, which signs with
// these primitives and requires ring-go to accept the result.

// hashToScalar mirrors go-dleq secp256k1/curve_decred.go HashToScalar:
// SHA3-512 (FIPS-202, NOT Keccak), reduced mod N, then encoded by copying
// big.Int.Bytes() into a [32]byte.
//
// That last step is the subtle one. big.Int.Bytes() returns the MINIMAL
// big-endian encoding, and copy() writes from offset 0, so a value needing
// fewer than 32 bytes is LEFT-aligned — i.e. multiplied by 256 per missing
// byte. This happens whenever the reduction lands below 2^248, about 1 input
// in 256.
//
// A port must reproduce this. Right-aligning (the natural choice, and what
// FillBytes does) yields a different scalar for those inputs, so the challenge
// chain fails to close and the relayer rejects the relay. With a 2-member
// [app, gateway] ring that is ~1 relay in 128: rare enough to look like a
// flaky network, frequent enough to matter.
func hashToScalar(in []byte) *big.Int {
	h := sha3.Sum512(in)
	n := new(big.Int).SetBytes(h[:])
	n.Mod(n, secpN)

	var reduced [32]byte
	copy(reduced[:], n.Bytes()) // left-aligns; NOT FillBytes. See above.

	return new(big.Int).SetBytes(reduced[:])
}

// hashToCurve mirrors ring-go helpers.go hashToCurveSecp256k1: SHA3-256 the
// compressed public key, read the digest as a field element X, and take the
// point with EVEN Y. About half of all X values are not on the curve; on a
// miss it re-hashes THE DIGEST (not the original key) and retries, up to 128
// times.
func hashToCurve(pub *dsecp.PublicKey) *dsecp.PublicKey {
	const safety = 128

	hash := sha3.Sum256(pub.SerializeCompressed())
	fe := new(dsecp.FieldVal)
	fe.SetBytes(&hash)
	maybeY := new(dsecp.FieldVal)

	for i := 0; i < safety; i++ {
		if dsecp.DecompressY(fe, false, maybeY) { // false => even Y
			fe.Normalize()
			maybeY.Normalize()
			return dsecp.NewPublicKey(fe, maybeY)
		}
		hash = sha3.Sum256(hash[:])
		fe.SetBytes(&hash)
	}
	return nil
}

// encodeScalar is the wire encoding for scalars: 32 bytes big-endian, zero
// padded on the LEFT. Note the contrast with hashToScalar's internal encode —
// scalars on the wire are canonical; only the challenge derivation is not.
func encodeScalar(n *big.Int) []byte {
	b := make([]byte, 32)
	new(big.Int).Mod(n, secpN).FillBytes(b)
	return b
}

// pubFromHex derives the compressed public key for a hex private key.
func pubFromHex(privHex string) *dsecp.PublicKey {
	bz, err := hex.DecodeString(privHex)
	if err != nil {
		fatal("private key is not hex: %v", err)
	}
	priv := dsecp.PrivKeyFromBytes(bz)
	return priv.PubKey()
}
