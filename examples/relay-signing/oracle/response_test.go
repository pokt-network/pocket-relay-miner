package main

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
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
	if v.Inner.BodyText != string(inner.BodyBz) {
		t.Fatal("body_text does not match the inner response body")
	}

	// response_signature_hex is published as ground truth on its own. Verifying
	// the signature EMBEDDED in relay_response_hex does not check that the
	// separately emitted hex is the same bytes — and a port with a correct
	// implementation would "fix" itself to match a wrong vector.
	if hex.EncodeToString(resp.Meta.SupplierOperatorSignature) != v.ResponseSigHex {
		t.Fatal("response_signature_hex is not the signature inside relay_response_hex")
	}

	// The headers a caller reads back must be the headers that were signed over.
	gotHeaders := make(map[string][]string, len(inner.Header))
	for k, h := range inner.Header {
		gotHeaders[k] = h.Values
	}
	if !reflect.DeepEqual(gotHeaders, v.Inner.Headers) {
		t.Fatalf("headers do not match: %v != %v", v.Inner.Headers, gotHeaders)
	}
	if len(gotHeaders) < 2 {
		t.Fatal("the fixture needs at least two headers or it never exercises " +
			"deterministic protobuf map marshalling, which is the thing that " +
			"diverges in production")
	}

	// The value raw ECDSA actually covers. Cosmos hashes its argument again
	// inside Sign, so this second hash is what a non-Go verifier must use.
	outer := sha256.Sum256(signableHashOf(t, &resp))
	if hex.EncodeToString(outer[:]) != v.ResponseEcdsaMsg {
		t.Fatal("response_ecdsa_message_hash_hex is not sha256(signable hash)")
	}
}

// signableHashOf returns sha256 of the bytes the response signature covers.
func signableHashOf(t *testing.T, resp *servicetypes.RelayResponse) []byte {
	t.Helper()
	h, err := resp.GetSignableBytesHash()
	if err != nil {
		t.Fatalf("signable bytes hash: %v", err)
	}
	return h[:]
}

// TestReceiptDomainTag_Golden pins the tag literal.
//
// The tag is restated here rather than imported from relayer/receipt.go, so a
// verifier cannot inherit a producer's mistake — but restating without pinning
// is the worst of both: the relayer could change it, this oracle would keep
// emitting the old one, every language port would be validated against a tag
// the relayer does not use, and CI would stay green the whole way.
func TestReceiptDomainTag_Golden(t *testing.T) {
	// Must track relayer/receipt.go. Changing either without the other is a
	// breaking wire change for every external verifier.
	const want = "POKT-RELAY-RECEIPT-v1\x00"

	if receiptDomainTag != want {
		t.Fatalf("domain tag drifted from relayer/receipt.go: %q != %q", receiptDomainTag, want)
	}
	if len(receiptDomainTag) != 22 {
		t.Fatalf("domain tag must be 22 bytes, got %d", len(receiptDomainTag))
	}
	if receiptDomainTag[len(receiptDomainTag)-1] != 0x00 {
		t.Fatal("domain tag must end in a NUL, which is what separates it from a marshalled protobuf")
	}
	if receiptDomainTag[0] != 0x50 {
		t.Fatal("domain tag must not start with 0x0a, the first byte of a marshalled protobuf")
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

	// What raw ECDSA covers, which is NOT digest_hex. A port that feeds
	// digest_hex to its verifier gets a rejection while Go says the receipt is
	// valid, and goes looking in the wrong place.
	outer := sha256.Sum256(digest[:])
	if hex.EncodeToString(outer[:]) != v.Receipt.EcdsaMessageHashHex {
		t.Fatal("ecdsa_message_hash_hex is not sha256(digest)")
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
