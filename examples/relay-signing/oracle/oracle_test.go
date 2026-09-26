package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
	"testing"

	dsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	dleqsecp "github.com/pokt-network/go-dleq/secp256k1"
	ring "github.com/pokt-network/ring-go"
	"golang.org/x/crypto/sha3"
)

// The vectors this oracle publishes are only useful if the primitives that
// produce them behave like the unexported originals. Asserting that directly
// is impossible — they are unexported. So these tests assert it end-to-end:
// build a bLSAG signature using ONLY the mirrored primitives, and require the
// real ring-go verifier to accept it. If a primitive drifts, the challenge
// chain stops closing and these fail.

// signWithMirroredPrimitives is a complete from-scratch bLSAG signer built on
// primitives.go. It is deliberately independent of ring-go: it exists to prove
// the published spec is sufficient to sign with, which is exactly the claim a
// port depends on.
func signWithMirroredPrimitives(t *testing.T, m [32]byte, pubs []*dsecp.PublicKey, priv *big.Int, ourIdx int, canonical bool) []byte {
	t.Helper()

	size := len(pubs)
	c := make([]*big.Int, size)
	s := make([]*big.Int, size)

	h := hashToScalarFn(canonical)

	challenge := func(l, r *dsecp.PublicKey) *big.Int {
		t := make([]byte, 0, 32+33+33)
		t = append(t, m[:]...)
		t = append(t, l.SerializeCompressed()...)
		t = append(t, r.SerializeCompressed()...)
		return h(t)
	}

	hOur := hashToCurve(pubs[ourIdx])
	image := scalarMul(priv, hOur)

	u, err := rand.Int(rand.Reader, secpN)
	if err != nil {
		t.Fatalf("rand: %v", err)
	}
	c[(ourIdx+1)%size] = challenge(scalarBaseMul(u), scalarMul(u, hOur))

	for i := 1; i < size; i++ {
		idx := (ourIdx + i) % size
		si, err := rand.Int(rand.Reader, secpN)
		if err != nil {
			t.Fatalf("rand: %v", err)
		}
		s[idx] = si

		l := pointAdd(scalarMul(c[idx], pubs[idx]), scalarBaseMul(si))
		r := pointAdd(scalarMul(c[idx], image), scalarMul(si, hashToCurve(pubs[idx])))
		c[(idx+1)%size] = challenge(l, r)
	}

	cx := new(big.Int).Mul(c[ourIdx], priv)
	cx.Mod(cx, secpN)
	sj := new(big.Int).Sub(u, cx)
	sj.Mod(sj, secpN)
	s[ourIdx] = sj

	out := make([]byte, 0, 69+65*size)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(size))
	out = append(out, sz[:]...)
	out = append(out, encodeScalar(c[0])...)
	out = append(out, image.SerializeCompressed()...)
	for i := 0; i < size; i++ {
		out = append(out, encodeScalar(s[i])...)
		out = append(out, pubs[i].SerializeCompressed()...)
	}
	return out
}

// hashToScalarFn returns either the real (quirk-preserving) derivation or the
// canonical one a correct-by-instinct port would write.
func hashToScalarFn(canonical bool) func([]byte) *big.Int {
	if !canonical {
		return hashToScalar
	}
	return func(in []byte) *big.Int {
		h := sha3.Sum512(in)
		n := new(big.Int).SetBytes(h[:])
		n.Mod(n, secpN)
		return n // right-aligned by construction: the natural, wrong choice
	}
}

func scalarFromBig(n *big.Int) *dsecp.ModNScalar {
	var b [32]byte
	new(big.Int).Mod(n, secpN).FillBytes(b[:])
	s := new(dsecp.ModNScalar)
	s.SetBytes(&b)
	return s
}

func scalarBaseMul(k *big.Int) *dsecp.PublicKey {
	var res dsecp.JacobianPoint
	dsecp.ScalarBaseMultNonConst(scalarFromBig(k), &res)
	res.ToAffine()
	return dsecp.NewPublicKey(&res.X, &res.Y)
}

func scalarMul(k *big.Int, p *dsecp.PublicKey) *dsecp.PublicKey {
	var jp, res dsecp.JacobianPoint
	p.AsJacobian(&jp)
	dsecp.ScalarMultNonConst(scalarFromBig(k), &jp, &res)
	res.ToAffine()
	return dsecp.NewPublicKey(&res.X, &res.Y)
}

func pointAdd(a, b *dsecp.PublicKey) *dsecp.PublicKey {
	var ja, jb, res dsecp.JacobianPoint
	a.AsJacobian(&ja)
	b.AsJacobian(&jb)
	dsecp.AddNonConst(&ja, &jb, &res)
	res.ToAffine()
	return dsecp.NewPublicKey(&res.X, &res.Y)
}

func ringGoAccepts(t *testing.T, m [32]byte, sigBz []byte) bool {
	t.Helper()
	var sig ring.RingSig
	if err := sig.Deserialize(dleqsecp.NewCurve(), sigBz); err != nil {
		return false
	}
	return sig.Verify(m)
}

func testRing(t *testing.T) ([]*dsecp.PublicKey, *big.Int) {
	t.Helper()
	appPub := pubFromHex(appPrivHex)
	gwPub := pubFromHex(gwPrivHex)
	gwPrivBz := mustHex(t, gwPrivHex)
	return []*dsecp.PublicKey{appPub, gwPub}, new(big.Int).SetBytes(gwPrivBz)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		var v int
		if _, err := fmt.Sscanf(s[2*i:2*i+2], "%02x", &v); err != nil {
			t.Fatalf("bad hex: %v", err)
		}
		b[i] = byte(v)
	}
	return b
}

// TestMirroredHashToScalarMatchesRealGoDleq closes a circularity: `oracle
// vectors` publishes values computed by primitives.go, which is a MIRROR of
// go-dleq rather than go-dleq itself. A port that matches those vectors has
// matched the mirror, and would inherit any drift in it.
//
// So compare the mirror against the REAL go-dleq directly, over enough inputs
// to cover the ~1-in-256 short reduction many times over. The other tests here
// check the mirror end-to-end (sign with it, require ring-go to accept); this
// one checks it head-on, which is what makes the published vectors trustworthy.
func TestMirroredHashToScalarMatchesRealGoDleq(t *testing.T) {
	curve := dleqsecp.NewCurve()

	const trials = 20000
	shortReductions := 0

	for i := 0; i < trials; i++ {
		in := []byte(fmt.Sprintf("relay-msg-%d", i))

		realScalar, err := curve.HashToScalar(in) // the actual dependency
		if err != nil {
			t.Fatalf("real go-dleq HashToScalar(%q): %v", in, err)
		}
		want := realScalar.Encode()
		got := encodeScalar(hashToScalar(in)) // the mirror

		if !bytes.Equal(got, want) {
			t.Fatalf("hashToScalar(%q) drifted from go-dleq:\n  mirror   = %x\n  go-dleq  = %x",
				in, got, want)
		}

		// Track how often the quirk's branch was actually taken, so this test
		// cannot silently degrade into checking only the easy path.
		h := sha3.Sum512(in)
		n := new(big.Int).SetBytes(h[:])
		n.Mod(n, secpN)
		if len(n.Bytes()) < 32 {
			shortReductions++
		}
	}

	// Expect ~78 (trials/256). Assert loosely: the point is that the quirk's
	// branch was exercised, not the exact count of a random variable.
	if shortReductions < 20 {
		t.Fatalf("only %d/%d inputs took the short-reduction branch; this test is no "+
			"longer covering the quirk, which is the only interesting part of hashToScalar",
			shortReductions, trials)
	}
	t.Logf("mirror matches real go-dleq on %d/%d inputs, %d of them short reductions",
		trials, trials, shortReductions)
}

// TestMirroredPrimitivesProduceSignaturesRingGoAccepts is the load-bearing
// test: it proves the documented spec is complete enough to sign with, and
// that the mirrored primitives match the originals.
func TestMirroredPrimitivesProduceSignaturesRingGoAccepts(t *testing.T) {
	pubs, gwPriv := testRing(t)

	const trials = 512
	for i := 0; i < trials; i++ {
		var m [32]byte
		copy(m[:], []byte(fmt.Sprintf("msg-%026d", i)))

		sigBz := signWithMirroredPrimitives(t, m, pubs, gwPriv, 1, false)

		if got := len(sigBz); got != 199 {
			t.Fatalf("signature length = %d, want 199 (69 + 65*2)", got)
		}
		if !ringGoAccepts(t, m, sigBz) {
			t.Fatalf("trial %d: ring-go rejected a signature built from the mirrored primitives; "+
				"a primitive has drifted from its original", i)
		}
	}
}

// TestCanonicalHashToScalarIsRejected is the negative control. It pins WHY the
// quirk is documented: an implementation that right-aligns the reduction is
// rejected by ring-go at the predicted rate. Without this test, someone would
// eventually "fix" hashToScalar and break every port that trusts these vectors.
//
// Rate: a ring of n members computes n challenges, each divergent with
// probability ~1/256, so P(reject) = 1 - (255/256)^2 ~= 0.78% for n=2.
func TestCanonicalHashToScalarIsRejected(t *testing.T) {
	pubs, gwPriv := testRing(t)

	const trials = 4000
	rejected := 0
	for i := 0; i < trials; i++ {
		var m [32]byte
		copy(m[:], []byte(fmt.Sprintf("canon-%024d", i)))

		sigBz := signWithMirroredPrimitives(t, m, pubs, gwPriv, 1, true)
		if !ringGoAccepts(t, m, sigBz) {
			rejected++
		}
	}

	if rejected == 0 {
		t.Fatal("a canonical (right-aligning) hashToScalar was never rejected over " +
			"4000 signatures. Either ring-go/go-dleq fixed the left-align, or this " +
			"test stopped exercising it. If go-dleq was fixed, hashToScalar and the " +
			"docs must be updated together — and every existing port breaks.")
	}

	// Expected ~31 of 4000. Bound loosely: this is a probabilistic assertion and
	// must not flake (see CONTRIBUTING.md rule #1). The point is that the rate is
	// small-but-real, not its exact value.
	rate := float64(rejected) / trials
	if rate > 0.05 {
		t.Fatalf("canonical rejection rate = %.2f%% (%d/%d), want ~0.78%%. A rate this "+
			"high means the divergence is not the left-align quirk but something "+
			"broader in the mirrored primitives.", rate*100, rejected, trials)
	}
	t.Logf("canonical hashToScalar rejected %d/%d = %.2f%% (predicted ~0.78%%)",
		rejected, trials, rate*100)
}

// TestHashToCurveIsDeterministic guards the vector file: a port bisecting a
// failure compares against these outputs, so they must not move.
func TestHashToCurveIsDeterministic(t *testing.T) {
	pub := pubFromHex(appPrivHex)
	first := hashToCurve(pub).SerializeCompressed()
	for i := 0; i < 8; i++ {
		if got := hashToCurve(pub).SerializeCompressed(); string(got) != string(first) {
			t.Fatal("hashToCurve is not deterministic")
		}
	}
	if len(first) != 33 {
		t.Fatalf("hash_to_curve output = %d bytes, want 33 (compressed point)", len(first))
	}
}

// TestScalarEncodingIsLeftPadded pins the contrast that trips ports up: wire
// scalars are right-aligned, the challenge derivation is not.
func TestScalarEncodingIsLeftPadded(t *testing.T) {
	got := encodeScalar(big.NewInt(1))
	if len(got) != 32 {
		t.Fatalf("encodeScalar(1) = %d bytes, want 32", len(got))
	}
	for i := 0; i < 31; i++ {
		if got[i] != 0 {
			t.Fatalf("encodeScalar(1) byte %d = %#x, want 0 (must be zero-padded on the left)", i, got[i])
		}
	}
	if got[31] != 1 {
		t.Fatalf("encodeScalar(1) last byte = %#x, want 0x01", got[31])
	}
}
