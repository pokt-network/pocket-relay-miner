// Command oracle is the authority a non-Go relay signer is tested against.
//
// A ring signature is only correct if the Go verifier that guards a real
// relayer accepts it. Round-tripping your own sign->verify proves nothing:
// an implementation can be self-consistent and still be rejected by the
// relayer for the whole of its life. So this program does not reimplement
// anything. `verify` hands your bytes to the same github.com/pokt-network/ring-go
// that the relayer runs, and reports what it says.
//
// Signing is randomized (the seed scalar and every decoy come from
// crypto/rand), so there is no fixed signature to diff against. `vectors`
// therefore emits the deterministic pieces underneath the signature — the
// ones a port gets wrong silently — so a failure can be bisected to a
// primitive instead of guessed at.
//
// Usage:
//
//	oracle vectors                 # deterministic test vectors, JSON to stdout
//	echo '{"msg_hex":..,"sig_hex":..}' | oracle verify
//	oracle sign                    # a known-good Go signature to verify against
//	oracle response-vectors        # a signed RelayResponse + receipt, JSON to stdout
//	echo '{"digest_hex":..,"sig_hex":..,"pub_hex":..}' | oracle verify-receipt
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	dsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	dleqsecp "github.com/pokt-network/go-dleq/secp256k1"
	ring "github.com/pokt-network/ring-go"
	"golang.org/x/crypto/sha3"
)

// Fixed keys so vectors are reproducible across runs and machines. These are
// throwaway test keys and are not used to hold anything.
const (
	appPrivHex = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"
	gwPrivHex  = "1a11ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ab11"
)

var secpN, _ = new(big.Int).SetString(
	"fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 16)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "vectors":
		emitVectors()
	case "verify":
		verify()
	case "sign":
		emitSignature()
	case "response-vectors":
		emitResponseVectors()
	case "verify-receipt":
		verifyReceiptFromStdin()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `oracle - test a relay ring signature against the real Go verifier

  oracle vectors    deterministic test vectors (JSON)
  oracle sign       a known-good Go signature (JSON)
  oracle verify     read {"msg_hex","sig_hex"} on stdin, verify with ring-go

  oracle response-vectors   a signed RelayResponse and receipt (JSON)
  oracle verify-receipt     read {"digest_hex","sig_hex","pub_hex"} on stdin,
                            verify with cosmos secp256k1

exit code for verify and verify-receipt: 0 accepted, 1 rejected
`)
	os.Exit(2)
}

type vectorFile struct {
	Note          string          `json:"note"`
	Keys          keyVectors      `json:"keys"`
	HashToScalar  []hashVector    `json:"hash_to_scalar"`
	HashToCurve   []hashVector    `json:"hash_to_curve"`
	ScalarEncode  []hashVector    `json:"scalar_encoding"`
	SignatureWire wireDescription `json:"signature_wire_format"`
}

type keyVectors struct {
	AppPrivHex string `json:"app_priv_hex"`
	AppPubHex  string `json:"app_pub_hex"`
	GwPrivHex  string `json:"gateway_priv_hex"`
	GwPubHex   string `json:"gateway_pub_hex"`
	Note       string `json:"note"`
}

type hashVector struct {
	Note      string `json:"note,omitempty"`
	InputHex  string `json:"input_hex"`
	OutputHex string `json:"output_hex"`
}

type wireDescription struct {
	Layout string `json:"layout"`
	Length string `json:"length"`
}

func emitVectors() {
	appPub := pubFromHex(appPrivHex)
	gwPub := pubFromHex(gwPrivHex)

	v := vectorFile{
		Note: "Deterministic pieces of a Pocket relay ring signature. The signature " +
			"itself is randomized and cannot be diffed; bisect a failing port with these.",
		Keys: keyVectors{
			AppPrivHex: appPrivHex,
			AppPubHex:  hex.EncodeToString(appPub.SerializeCompressed()),
			GwPrivHex:  gwPrivHex,
			GwPubHex:   hex.EncodeToString(gwPub.SerializeCompressed()),
			Note:       "throwaway test keys; pubkeys are 33-byte compressed secp256k1",
		},
		SignatureWire: wireDescription{
			Layout: "[4B big-endian ring size n][32B c][33B key image I][n x (32B s_i || 33B P_i)]",
			Length: "69 + 65n bytes (199 for the 2-member [app, gateway] ring)",
		},
	}

	// hash_to_scalar: SHA3-512 mod N, then go-dleq's left-aligning encode.
	// The chosen inputs include one that triggers a short reduction, which is
	// the case a canonical implementation gets wrong. See README.
	for _, in := range hashToScalarProbes() {
		v.HashToScalar = append(v.HashToScalar, hashVector{
			Note:      in.note,
			InputHex:  hex.EncodeToString(in.data),
			OutputHex: hex.EncodeToString(encodeScalar(hashToScalar(in.data))),
		})
	}

	// hash_to_curve: try-and-increment over SHA3-256 of a compressed pubkey.
	for _, pk := range []*dsecp.PublicKey{appPub, gwPub} {
		v.HashToCurve = append(v.HashToCurve, hashVector{
			InputHex:  hex.EncodeToString(pk.SerializeCompressed()),
			OutputHex: hex.EncodeToString(hashToCurve(pk).SerializeCompressed()),
		})
	}

	// scalar encoding: fixed 32-byte big-endian, left zero-padded.
	one := big.NewInt(1)
	v.ScalarEncode = append(v.ScalarEncode, hashVector{
		Note:      "scalar 1 encodes as 32 bytes, zero-padded on the LEFT (unlike hash_to_scalar's output)",
		InputHex:  "01",
		OutputHex: hex.EncodeToString(encodeScalar(one)),
	})

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fatal("encode vectors: %v", err)
	}
}

type probe struct {
	note string
	data []byte
}

// hashToScalarProbes returns inputs covering both branches of go-dleq's
// encode: the common full-width reduction, and the 1-in-256 short reduction
// where its left-align shifts the value. A port that only tests the first
// looks correct and fails ~1 relay in 128 in production.
func hashToScalarProbes() []probe {
	probes := []probe{
		{note: "ordinary input, 32-byte reduction", data: []byte("pocket")},
	}
	// Search for an input whose reduction is short, so the vector file always
	// pins the quirk even if the search space changes.
	for i := 0; i < 100000; i++ {
		in := []byte(fmt.Sprintf("relay-msg-%d", i))
		h := sha3.Sum512(in)
		n := new(big.Int).SetBytes(h[:])
		n.Mod(n, secpN)
		if len(n.Bytes()) < 32 {
			probes = append(probes, probe{
				note: fmt.Sprintf("SHORT reduction (%d-byte n): go-dleq LEFT-aligns this, a canonical "+
					"implementation right-aligns and computes a different scalar", len(n.Bytes())),
				data: in,
			})
			break
		}
	}
	return probes
}

type verifyIn struct {
	MsgHex string `json:"msg_hex"`
	SigHex string `json:"sig_hex"`
}

type verifyOut struct {
	Valid  bool   `json:"valid"`
	Reason string `json:"reason,omitempty"`
}

func verify() {
	var in verifyIn
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fatal("decode stdin: %v", err)
	}

	msg, err := hex.DecodeString(in.MsgHex)
	if err != nil {
		reject("msg_hex is not hex: %v", err)
	}
	if len(msg) != 32 {
		reject("msg must be exactly 32 bytes (the SHA-256 signable hash), got %d", len(msg))
	}
	sigBz, err := hex.DecodeString(in.SigHex)
	if err != nil {
		reject("sig_hex is not hex: %v", err)
	}

	var m [32]byte
	copy(m[:], msg)

	var sig ring.RingSig
	if err := sig.Deserialize(dleqsecp.NewCurve(), sigBz); err != nil {
		reject("ring-go could not deserialize the signature: %v", err)
	}

	if !sig.Verify(m) {
		reject("ring-go deserialized the signature but rejected it: the challenge chain does not close")
	}

	out, _ := json.Marshal(verifyOut{Valid: true})
	fmt.Println(string(out))
}

func reject(format string, args ...any) {
	out, _ := json.Marshal(verifyOut{Valid: false, Reason: fmt.Sprintf(format, args...)})
	fmt.Println(string(out))
	os.Exit(1)
}

type signOut struct {
	Note      string   `json:"note"`
	MsgHex    string   `json:"msg_hex"`
	SigHex    string   `json:"sig_hex"`
	RingPubs  []string `json:"ring_pubkeys_hex"`
	SignerIdx int      `json:"signer_index"`
}

// emitSignature produces a real Go signature so a port can be tested in the
// other direction: your verifier should accept this.
func emitSignature() {
	curve := dleqsecp.NewCurve()

	appPubBz := pubFromHex(appPrivHex).SerializeCompressed()
	gwPubBz := pubFromHex(gwPrivHex).SerializeCompressed()

	appPoint, err := curve.DecodeToPoint(appPubBz)
	if err != nil {
		fatal("decode app point: %v", err)
	}
	gwPoint, err := curve.DecodeToPoint(gwPubBz)
	if err != nil {
		fatal("decode gateway point: %v", err)
	}

	r, err := ring.NewFixedKeyRingFromPublicKeys(curve, []dleqTypesPoint{appPoint, gwPoint})
	if err != nil {
		fatal("build ring: %v", err)
	}

	gwPrivBz, _ := hex.DecodeString(gwPrivHex)
	priv, err := curve.DecodeToScalar(gwPrivBz)
	if err != nil {
		fatal("decode gateway scalar: %v", err)
	}

	var m [32]byte
	copy(m[:], []byte("pocket-relay-signable-hash-32byt"))

	sig, err := r.Sign(m, priv)
	if err != nil {
		fatal("sign: %v", err)
	}
	sigBz, err := sig.Serialize()
	if err != nil {
		fatal("serialize: %v", err)
	}

	out := signOut{
		Note: "A real ring-go signature over the [app, gateway] ring, signed with the " +
			"gateway key. Your verifier must accept it. Signing is randomized, so " +
			"sig_hex differs on every run — do not diff it, verify it.",
		MsgHex:    hex.EncodeToString(m[:]),
		SigHex:    hex.EncodeToString(sigBz),
		RingPubs:  []string{hex.EncodeToString(appPubBz), hex.EncodeToString(gwPubBz)},
		SignerIdx: 1,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fatal("encode signature: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
