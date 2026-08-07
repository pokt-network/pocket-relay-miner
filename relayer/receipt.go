package relayer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// Optional relay receipt.
//
// A receipt is a supplier-operator signature over a digest binding the
// request's ring signature to the response's payload hash. It answers a
// question the RelayResponse signature cannot: which request does this
// response answer?
//
// RelayResponseMetadata carries only a session header and the signature, and
// thousands of relays in one session share a session header — so a response
// signature proves "this supplier produced this body in this session" and
// nothing about which request produced it.
//
// The joint commitment does exist elsewhere: the SMST leaf hashes
// Relay{Req, Res}. But it is computed asynchronously, after the response has
// been written, and only for relays that meet mining difficulty, so it cannot
// serve as a per-relay receipt.
const (
	// receiptRequestHeader is the caller's opt-in. Absent means no, so a
	// caller that does not know about receipts pays nothing.
	receiptRequestHeader = "Pocket-Sign-Receipt"

	// receiptResponseHeader carries the receipt back.
	receiptResponseHeader = "Pocket-Relay-Receipt"

	receiptHeaderVersion = "v1"

	// receiptDomainTag separates this signing context from every other one the
	// supplier operator key serves: RelayResponse signatures and, on chain,
	// MsgCreateClaim / MsgSubmitProof transactions (their proto signer option
	// is supplier_operator_address). All three are secp256k1 signatures over a
	// sha256 digest made by the same key, and a signature carries no marker
	// saying which contract it belongs to. The tag makes the preimage spaces
	// disjoint by construction: it begins 0x50, while a marshaled protobuf in
	// any of the other contexts begins 0x0a.
	//
	// The trailing v1 versions the contract INSIDE the signed bytes. Change
	// what enters the digest and a v1 verifier fails cleanly rather than
	// silently verifying the wrong content.
	receiptDomainTag = "POKT-RELAY-RECEIPT-v1\x00"
)

// receiptDigest returns the 32 bytes the supplier key signs.
//
// The intermediate hash is not stylistic: Signer.Sign takes [32]byte.
//
// ENCODING INVARIANT — at most one variable-length field. The tag is 22 fixed
// bytes and respPayloadHash is 32 fixed bytes, so reqSig's extent is determined
// by the total length and no two distinct pairs can produce the same preimage.
// A second variable-length field would break that and would require explicit
// length prefixes for every field.
func receiptDigest(reqSig, respPayloadHash []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(receiptDomainTag))
	h.Write(reqSig)
	h.Write(respPayloadHash)

	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

// buildReceipt signs the receipt digest with the supplier operator key.
//
// Empty inputs are refused rather than signing a degenerate digest. At the
// production call site respPayloadHash is always populated — SignRelayResponse
// calls UpdatePayloadHash before signing, and that errors on an empty payload —
// but the guard keeps the function honest for any future caller.
func buildReceipt(signer Signer, reqSig, respPayloadHash []byte) ([]byte, error) {
	if signer == nil {
		return nil, fmt.Errorf("no signer")
	}
	if len(reqSig) == 0 {
		return nil, fmt.Errorf("empty request signature")
	}
	if len(respPayloadHash) == 0 {
		return nil, fmt.Errorf("empty response payload hash")
	}

	digest := receiptDigest(reqSig, respPayloadHash)
	return signer.Sign(digest)
}

// clientWantsReceipt reports whether the caller asked for a receipt.
//
// Exactly one accepted spelling, case-insensitive. Go's header parsing has
// already stripped surrounding whitespace.
func clientWantsReceipt(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(r.Header.Get(receiptRequestHeader), "true")
}

// formatReceiptHeader renders the response header value: "v1.<sig_hex>".
//
// An HTTP header cannot carry raw bytes: a signature contains arbitrary byte
// values, and a 0x0a in one would break header framing. Hex is the encoding
// this repository already uses for keys, hashes and signatures on the CLI, it
// has no padding character to explain to a verifier written in another
// language, and every standard library has it. It costs 128 characters for a
// 64-byte signature where base64url would cost 88 — the header is not on any
// budget that makes 40 bytes matter.
//
// Lowercase is the emitted form and is pinned by a golden test. Verifiers
// accept either case.
func formatReceiptHeader(sig []byte) string {
	return receiptHeaderVersion + "." + hex.EncodeToString(sig)
}
