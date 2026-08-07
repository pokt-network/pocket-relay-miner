package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"google.golang.org/protobuf/proto"
)

// TestResponseVectors_SelfConsistent checks the emitted vectors against the
// real Go libraries. If this fails, every language example is being tested
// against a lie.
func TestResponseVectors_SelfConsistent(t *testing.T) {
	v := buildResponseVectors()

	respBz, err := hex.DecodeString(v.RelayResponseHex)
	if err != nil {
		t.Fatalf("decode relay_response_hex: %v", err)
	}
	var resp servicetypes.RelayResponse
	if err := resp.Unmarshal(respBz); err != nil {
		t.Fatalf("unmarshal relay response: %v", err)
	}

	privBz, err := hex.DecodeString(v.Supplier.PrivHex)
	if err != nil {
		t.Fatalf("decode supplier priv: %v", err)
	}
	priv := &secp256k1.PrivKey{Key: privBz}
	pub := priv.PubKey()

	if hex.EncodeToString(pub.Bytes()) != v.Supplier.PubHex {
		t.Fatalf("pub_hex does not match the private key: %s != %s",
			hex.EncodeToString(pub.Bytes()), v.Supplier.PubHex)
	}

	// The response signature must satisfy the SAME verifier the relayer's
	// callers use, not a round trip of our own.
	if err := resp.VerifySupplierOperatorSignature(pub); err != nil {
		t.Fatalf("response signature does not verify under poktroll: %v", err)
	}

	// signable_bytes_hex must be exactly what Go signs, byte for byte. Every
	// language example rebuilds these bytes by filtering the received protobuf;
	// if this pins the wrong value they are all checked against the wrong thing.
	signable := resp
	signable.Meta.SupplierOperatorSignature = nil
	if signable.PayloadHash != nil {
		signable.Payload = nil
	}
	want, err := signable.Marshal()
	if err != nil {
		t.Fatalf("marshal signable: %v", err)
	}
	if hex.EncodeToString(want) != v.SignableBytesHex {
		t.Fatalf("signable_bytes_hex does not match Go's marshal:\n got %s\nwant %s",
			v.SignableBytesHex, hex.EncodeToString(want))
	}

	// The payload hash must really be sha256 of the payload that travelled.
	payload, err := hex.DecodeString(v.PayloadHex)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != v.PayloadHashHex {
		t.Fatal("payload_hash_hex is not sha256(payload)")
	}

	// The inner POKTHTTPResponse must decode, and its body must be the JSON-RPC
	// answer a caller is actually after.
	var inner sdktypes.POKTHTTPResponse
	if err := proto.Unmarshal(payload, &inner); err != nil {
		t.Fatalf("unmarshal inner POKTHTTPResponse: %v", err)
	}
	if inner.StatusCode != uint32(v.Inner.StatusCode) {
		t.Fatalf("inner status code %d != %d", inner.StatusCode, v.Inner.StatusCode)
	}
	if hex.EncodeToString(inner.BodyBz) != v.Inner.BodyBzHex {
		t.Fatal("body_bz_hex does not match the inner response body")
	}
}

// TestReceiptVectors_VerifyAndFailControls is the point of the receipt: it must
// verify for the pair it was issued for, and be useless for any other.
func TestReceiptVectors_VerifyAndFailControls(t *testing.T) {
	v := buildResponseVectors()

	privBz, _ := hex.DecodeString(v.Supplier.PrivHex)
	pub := (&secp256k1.PrivKey{Key: privBz}).PubKey()

	mustHex := func(what, s string) []byte {
		t.Helper()
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatalf("decode %s: %v", what, err)
		}
		return b
	}

	reqSig := mustHex("request_signature_hex", v.Receipt.RequestSignatureHex)
	payloadHash := mustHex("payload_hash_hex", v.PayloadHashHex)
	sig := mustHex("receipt signature_hex", v.Receipt.SignatureHex)

	// The digest must be reproducible from the parts, and the parts must be
	// what the header claims. A verifier in another language rebuilds exactly
	// this and nothing else.
	preimage := append(append(append([]byte{}, mustHex("domain_tag_hex", v.Receipt.DomainTagHex)...),
		reqSig...), payloadHash...)
	if hex.EncodeToString(preimage) != v.Receipt.PreimageHex {
		t.Fatal("preimage_hex is not tag || request signature || payload hash")
	}
	digest := sha256.Sum256(preimage)
	if hex.EncodeToString(digest[:]) != v.Receipt.DigestHex {
		t.Fatal("digest_hex is not sha256(preimage)")
	}

	if !pub.VerifySignature(digest[:], sig) {
		t.Fatal("the receipt does not verify against the supplier key")
	}

	// Negative controls. Without these the vectors only ever demonstrate
	// success, which teaches nothing about what the receipt proves.
	otherHash := mustHex("other_payload_hash_hex", v.NegativeControls.OtherPayloadHashHex)
	otherSig := mustHex("other_request_signature_hex", v.NegativeControls.OtherRequestSignatureHex)

	for _, tc := range []struct {
		name        string
		reqSig      []byte
		payloadHash []byte
	}{
		{"a different response", reqSig, otherHash},
		{"a different request", otherSig, payloadHash},
		{"both different", otherSig, otherHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pre := append(append(append([]byte{}, mustHex("tag", v.Receipt.DomainTagHex)...),
				tc.reqSig...), tc.payloadHash...)
			d := sha256.Sum256(pre)
			if pub.VerifySignature(d[:], sig) {
				t.Fatal("the receipt verified for a pair it was never issued for")
			}
		})
	}

	// The header value the relayer actually puts on the wire.
	if want := "v1." + hex.EncodeToString(sig); v.Receipt.HeaderValue != want {
		t.Fatalf("header_value %q != %q", v.Receipt.HeaderValue, want)
	}
}

// TestNegativeControls_AreActuallyDifferent guards the fixture itself. If the
// controls happened to equal the real values the tests above would pass
// vacuously — the exact failure this suite exists to catch elsewhere.
func TestNegativeControls_AreActuallyDifferent(t *testing.T) {
	v := buildResponseVectors()

	if v.NegativeControls.OtherPayloadHashHex == v.PayloadHashHex {
		t.Fatal("other_payload_hash_hex equals the real payload hash; the control proves nothing")
	}
	if v.NegativeControls.OtherRequestSignatureHex == v.Receipt.RequestSignatureHex {
		t.Fatal("other_request_signature_hex equals the real request signature; the control proves nothing")
	}
}
