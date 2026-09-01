package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
)

// Relay receipt verification, written from the CALLER's side.
//
// A receipt answers the question the response signature cannot: which request
// does this response answer? The response signature covers only the session
// header and the payload hash, and thousands of relays in one session share a
// session header.
const (
	// ReceiptRequestHeader asks the relayer for a receipt. Absent means no, so
	// a caller that does not want one pays nothing.
	ReceiptRequestHeader = "Pocket-Sign-Receipt"

	// ReceiptResponseHeader carries it back.
	ReceiptResponseHeader = "Pocket-Relay-Receipt"

	// receiptDomainTag MUST match relayer/receipt.go. It is duplicated
	// deliberately rather than imported: this is the verifier side, and a
	// verifier that shares the producer's constant cannot catch the producer
	// changing it. The golden test in relayer/receipt_test.go pins the value
	// for both sides.
	receiptDomainTag = "POKT-RELAY-RECEIPT-v1\x00"
)

// VerifyRelayReceipt rebuilds the digest from what the caller sent and what it
// received, then checks the supplier signature over it.
//
// The receipt is never parsed for content — it is a check, not a container. If
// the request signature or the payload hash differ by one bit from what the
// supplier used, the digest differs and this fails. That is the whole property.
func VerifyRelayReceipt(
	headerValue string,
	reqSig, respPayloadHash []byte,
	supplierPub cryptotypes.PubKey,
) error {
	if headerValue == "" {
		return fmt.Errorf("no receipt header")
	}
	if supplierPub == nil {
		return fmt.Errorf("no supplier public key")
	}
	if len(reqSig) == 0 {
		return fmt.Errorf("empty request signature")
	}
	if len(respPayloadHash) == 0 {
		return fmt.Errorf("empty response payload hash")
	}

	encoded, ok := strings.CutPrefix(headerValue, "v1.")
	if !ok {
		return fmt.Errorf("unsupported receipt version: %q", headerValue)
	}
	// hex.DecodeString accepts either case. The relayer emits lowercase; a
	// verifier in another language may hand back uppercase, and rejecting that
	// would be a trap with no security value.
	sig, err := hex.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("receipt is not valid hex: %w", err)
	}

	h := sha256.New()
	h.Write([]byte(receiptDomainTag))
	h.Write(reqSig)
	h.Write(respPayloadHash)
	digest := h.Sum(nil)

	// cosmos secp256k1 hashes its input internally on both sign and verify, so
	// the 32-byte digest goes in here — not the preimage, and not a
	// pre-applied second hash.
	if !supplierPub.VerifySignature(digest, sig) {
		return fmt.Errorf("receipt signature does not verify")
	}
	return nil
}
