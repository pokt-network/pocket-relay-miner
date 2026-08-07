package main

// Ground truth for the RETURN TRIP: what a caller receives and how it is
// checked. Signing a relay is only half the job — the other half is reading
// what comes back and proving it answers the request you sent.
//
// Nothing here is reimplemented. The response is built and signed with the same
// poktroll and shannon-sdk types the relayer uses, so a language port that
// disagrees with these vectors disagrees with the relayer, not with this file.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	dleqsecp "github.com/pokt-network/go-dleq/secp256k1"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	ring "github.com/pokt-network/ring-go"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"google.golang.org/protobuf/proto"
)

// supplierPrivHex is a throwaway test key, fixed so the vectors are
// reproducible across runs and machines. It holds nothing and never will.
const supplierPrivHex = "4f3edf983ac636a65a842ce7c78d9aa706d3b113b37e0e6a3c0f7c7a1e6a41c0"

// receiptDomainTag MUST match relayer/receipt.go. It is restated rather than
// imported for the same reason the language examples restate it: a verifier
// that shares the producer's constant cannot catch the producer changing it.
const receiptDomainTag = "POKT-RELAY-RECEIPT-v1\x00"

type responseVectors struct {
	Note             string           `json:"note"`
	Supplier         supplierKey      `json:"supplier"`
	RelayResponseHex string           `json:"relay_response_hex"`
	SignableBytesHex string           `json:"signable_bytes_hex"`
	ResponseSigHex   string           `json:"response_signature_hex"`
	PayloadHex       string           `json:"payload_hex"`
	PayloadHashHex   string           `json:"payload_hash_hex"`
	Inner            innerResponse    `json:"inner_pokt_http_response"`
	Receipt          receiptVectors   `json:"receipt"`
	NegativeControls negativeControls `json:"negative_controls"`
}

type supplierKey struct {
	PrivHex string `json:"priv_hex"`
	PubHex  string `json:"pub_hex"`
	Note    string `json:"note"`
}

type innerResponse struct {
	Note       string              `json:"note"`
	StatusCode int                 `json:"status_code"`
	BodyBzHex  string              `json:"body_bz_hex"`
	BodyText   string              `json:"body_text"`
	Headers    map[string][]string `json:"headers"`
}

type receiptVectors struct {
	Note                string `json:"note"`
	RequestSignatureHex string `json:"request_signature_hex"`
	DomainTagHex        string `json:"domain_tag_hex"`
	PreimageHex         string `json:"preimage_hex"`
	DigestHex           string `json:"digest_hex"`
	SignatureHex        string `json:"signature_hex"`
	HeaderValue         string `json:"header_value"`
}

type negativeControls struct {
	Note                     string `json:"note"`
	OtherPayloadHashHex      string `json:"other_payload_hash_hex"`
	OtherRequestSignatureHex string `json:"other_request_signature_hex"`
}

// buildResponseVectors produces a real signed RelayResponse and a real receipt.
func buildResponseVectors() responseVectors {
	privBz, err := hex.DecodeString(supplierPrivHex)
	if err != nil {
		fatal("decode supplier key: %v", err)
	}
	priv := &secp256k1.PrivKey{Key: privBz}
	pub := priv.PubKey()

	// The relayer wraps the backend's HTTP answer in a POKTHTTPResponse and
	// puts THAT in RelayResponse.Payload. The JSON-RPC answer a caller wants is
	// two layers down, in BodyBz. See relayer/signer.go serializeHTTPResponse.
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":"0x1234"}`)
	inner := &sdktypes.POKTHTTPResponse{
		StatusCode: 200,
		Header: map[string]*sdktypes.Header{
			"Content-Type": {Key: "Content-Type", Values: []string{"application/json"}},
		},
		BodyBz: body,
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(inner)
	if err != nil {
		fatal("marshal inner response: %v", err)
	}

	resp := &servicetypes.RelayResponse{
		Meta: servicetypes.RelayResponseMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      "pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4",
				ServiceId:               "example-svc",
				SessionId:               "0000000000000000000000000000000000000000000000000000000000000001",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   110,
			},
		},
		Payload: payload,
	}

	// UpdatePayloadHash then sign, in that order: the hash is part of what the
	// signature covers.
	if err := resp.UpdatePayloadHash(); err != nil {
		fatal("update payload hash: %v", err)
	}
	signableHash, err := resp.GetSignableBytesHash()
	if err != nil {
		fatal("signable bytes hash: %v", err)
	}
	respSig, err := priv.Sign(signableHash[:])
	if err != nil {
		fatal("sign response: %v", err)
	}
	resp.Meta.SupplierOperatorSignature = respSig

	respBz, err := resp.Marshal()
	if err != nil {
		fatal("marshal relay response: %v", err)
	}

	// The bytes the signature covers: signature cleared, and payload cleared
	// because PayloadHash is set. A caller rebuilds this by FILTERING the
	// protobuf it received, never by re-encoding it.
	signable := *resp
	signable.Meta.SupplierOperatorSignature = nil
	signable.Payload = nil
	signableBz, err := signable.Marshal()
	if err != nil {
		fatal("marshal signable: %v", err)
	}

	// A genuine 199-byte bLSAG request signature, produced by the same ring
	// code the rest of this oracle uses, so the receipt binds something real.
	reqSig := genuineRequestSignature()
	otherReqSig := genuineRequestSignature()

	digest, preimage := receiptDigest(reqSig, resp.PayloadHash)
	receiptSig, err := priv.Sign(digest[:])
	if err != nil {
		fatal("sign receipt: %v", err)
	}

	// A different response's payload hash, for the negative control.
	otherBody := []byte(`{"jsonrpc":"2.0","id":1,"result":"0xdead"}`)
	otherInner := &sdktypes.POKTHTTPResponse{
		StatusCode: 200,
		Header:     inner.Header,
		BodyBz:     otherBody,
	}
	otherPayload, err := proto.MarshalOptions{Deterministic: true}.Marshal(otherInner)
	if err != nil {
		fatal("marshal other response: %v", err)
	}
	otherHash := sha256.Sum256(otherPayload)

	headers := make(map[string][]string, len(inner.Header))
	for k, h := range inner.Header {
		headers[k] = h.Values
	}

	return responseVectors{
		Note: "What a caller receives from a relayer, and everything needed to check it. " +
			"Built with the real poktroll and shannon-sdk types — if your port disagrees " +
			"with these bytes it disagrees with the relayer.",
		Supplier: supplierKey{
			PrivHex: supplierPrivHex,
			PubHex:  hex.EncodeToString(pub.Bytes()),
			Note:    "throwaway test key; pub is 33-byte compressed secp256k1",
		},
		RelayResponseHex: hex.EncodeToString(respBz),
		SignableBytesHex: hex.EncodeToString(signableBz),
		ResponseSigHex:   hex.EncodeToString(respSig),
		PayloadHex:       hex.EncodeToString(payload),
		PayloadHashHex:   hex.EncodeToString(resp.PayloadHash),
		Inner: innerResponse{
			Note: "RelayResponse.Payload is itself a marshalled POKTHTTPResponse. " +
				"The JSON-RPC answer is in body_bz, two layers down.",
			StatusCode: int(inner.StatusCode),
			BodyBzHex:  hex.EncodeToString(inner.BodyBz),
			BodyText:   string(inner.BodyBz),
			Headers:    headers,
		},
		Receipt: receiptVectors{
			Note: "digest = sha256(domain tag || request signature || response payload hash), " +
				"signed by the supplier operator key. Returned as the Pocket-Relay-Receipt header " +
				"when the request carried Pocket-Sign-Receipt: true.",
			RequestSignatureHex: hex.EncodeToString(reqSig),
			DomainTagHex:        hex.EncodeToString([]byte(receiptDomainTag)),
			PreimageHex:         hex.EncodeToString(preimage),
			DigestHex:           hex.EncodeToString(digest[:]),
			SignatureHex:        hex.EncodeToString(receiptSig),
			HeaderValue:         "v1." + hex.EncodeToString(receiptSig),
		},
		NegativeControls: negativeControls{
			Note: "The receipt above MUST FAIL against either of these. A tutorial whose " +
				"test only demonstrates success teaches nothing about what the receipt proves.",
			OtherPayloadHashHex:      hex.EncodeToString(otherHash[:]),
			OtherRequestSignatureHex: hex.EncodeToString(otherReqSig),
		},
	}
}

// receiptDigest returns the 32 bytes the supplier key signs, and the preimage
// they come from. Exactly one variable-length field, so no length prefixes are
// needed: the tag is 22 fixed bytes and the payload hash is 32, which pins the
// request signature's extent.
func receiptDigest(reqSig, payloadHash []byte) ([32]byte, []byte) {
	preimage := make([]byte, 0, len(receiptDomainTag)+len(reqSig)+len(payloadHash))
	preimage = append(preimage, receiptDomainTag...)
	preimage = append(preimage, reqSig...)
	preimage = append(preimage, payloadHash...)

	return sha256.Sum256(preimage), preimage
}

func emitResponseVectors() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(buildResponseVectors()); err != nil {
		fatal("encode response vectors: %v", err)
	}
}

type verifyReceiptIn struct {
	DigestHex string `json:"digest_hex"`
	SigHex    string `json:"sig_hex"`
	PubHex    string `json:"pub_hex"`
}

// verifyReceiptFromStdin reports what the real cosmos verifier says about a
// receipt, for the same reason `verify` exists for ring signatures: a port must
// be judged by the library the relayer runs, not by its own round trip.
func verifyReceiptFromStdin() {
	var in verifyReceiptIn
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fatal("decode stdin: %v", err)
	}

	digest, err := hex.DecodeString(in.DigestHex)
	if err != nil {
		reject("digest_hex is not hex: %v", err)
	}
	if len(digest) != 32 {
		reject("digest must be exactly 32 bytes, got %d", len(digest))
	}
	sig, err := hex.DecodeString(in.SigHex)
	if err != nil {
		reject("sig_hex is not hex: %v", err)
	}
	pubBz, err := hex.DecodeString(in.PubHex)
	if err != nil {
		reject("pub_hex is not hex: %v", err)
	}
	if len(pubBz) != secp256k1.PubKeySize {
		reject("pub_hex must be a %d-byte compressed key, got %d", secp256k1.PubKeySize, len(pubBz))
	}

	pub := &secp256k1.PubKey{Key: pubBz}
	if !pub.VerifySignature(digest, sig) {
		reject("cosmos secp256k1 rejected the signature (a high-S signature is rejected too)")
	}

	out, _ := json.Marshal(verifyOut{Valid: true})
	fmt.Println(string(out))
}

// genuineRequestSignature produces a real 199-byte bLSAG signature with the
// same ring code the rest of this oracle uses. The receipt binds the request
// signature, so a stand-in would make the vectors a lie about length and shape.
// Signing is randomised, so two calls differ — which is exactly what makes the
// second one a usable negative control.
func genuineRequestSignature() []byte {
	curve := dleqsecp.NewCurve()

	appPoint, err := curve.DecodeToPoint(pubFromHex(appPrivHex).SerializeCompressed())
	if err != nil {
		fatal("decode app point: %v", err)
	}
	gwPoint, err := curve.DecodeToPoint(pubFromHex(gwPrivHex).SerializeCompressed())
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
		fatal("sign request: %v", err)
	}
	sigBz, err := sig.Serialize()
	if err != nil {
		fatal("serialize request signature: %v", err)
	}

	return sigBz
}
