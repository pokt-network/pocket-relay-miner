//go:build test

// Adversarial properties of the relay receipt construction.
//
// These establish what a receipt does and does not prove. They are separate
// from receipt_test.go, which covers the unit surface: this file is about the
// security claims, and every test here corresponds to a claim made in the
// operator documentation.
//
// Two assumptions are NOT tested here and cannot be: sha256 collision and
// preimage resistance, and ECDSA-over-secp256k1 unforgeability. If either
// falls, far more than this feature breaks.
package relayer

import (
	"bytes"
	"crypto/sha256"
	"testing"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/rings"
)

// --- helpers -----------------------------------------------------------

// receiptPreimage exposes the bytes receiptDigest hashes. Production code never
// needs them; the domain-separation tests inspect them directly.
func receiptPreimage(reqSig, respPayloadHash []byte) []byte {
	out := make([]byte, 0, len(receiptDomainTag)+len(reqSig)+len(respPayloadHash))
	out = append(out, receiptDomainTag...)
	out = append(out, reqSig...)
	out = append(out, respPayloadHash...)
	return out
}

func signTestReceipt(t *testing.T, key *secp256k1.PrivKey, reqSig, payloadHash []byte) []byte {
	t.Helper()
	sig, err := buildReceipt(&privKeySigner{privKey: key}, reqSig, payloadHash)
	require.NoError(t, err)
	return sig
}

// verifyTestReceipt is written the way an INDEPENDENT verifier would write it:
// it rebuilds the digest from its own copies of the two inputs and never
// parses the receipt.
func verifyTestReceipt(pub cryptotypes.PubKey, reqSig, payloadHash, receipt []byte) bool {
	d := receiptDigest(reqSig, payloadHash)
	return pub.VerifySignature(d[:], receipt)
}

func receiptTestSessionHeader() *sessiontypes.SessionHeader {
	return &sessiontypes.SessionHeader{
		ApplicationAddress:      "pokt1app000000000000000000000000000000000",
		ServiceId:               "develop-http",
		SessionId:               "session-abc",
		SessionStartBlockHeight: 100,
		SessionEndBlockHeight:   160,
	}
}

// newReceiptTestResponse mirrors what the relayer serves: a payload plus the
// PayloadHash that SignRelayResponse populates.
func newReceiptTestResponse(t *testing.T, payload []byte) *servicetypes.RelayResponse {
	t.Helper()
	res := &servicetypes.RelayResponse{
		Meta:    servicetypes.RelayResponseMetadata{SessionHeader: receiptTestSessionHeader()},
		Payload: payload,
	}
	require.NoError(t, res.UpdatePayloadHash())
	return res
}

func newReceiptTestRelayRequest(payload []byte) *servicetypes.RelayRequest {
	return &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader:           receiptTestSessionHeader(),
			SupplierOperatorAddress: "pokt1supplier0000000000000000000000000000",
		},
		Payload: payload,
	}
}

// ringSignTestRequest produces a genuine bLSAG signature by the same sequence
// production uses (client/relay_client/signer.go): hash the signable bytes,
// build the ring, decode the key to a curve scalar, sign, serialize.
func ringSignTestRequest(
	t *testing.T,
	rr *servicetypes.RelayRequest,
	members []cryptotypes.PubKey,
	signerPriv *secp256k1.PrivKey,
) []byte {
	t.Helper()

	signableBz, err := rr.GetSignableBytesHash()
	require.NoError(t, err)

	appRing, err := rings.GetRingFromPubKeys(members)
	require.NoError(t, err)

	scalar, err := ring_secp256k1.NewCurve().DecodeToScalar(signerPriv.Key)
	require.NoError(t, err)

	ringSig, err := appRing.Sign(signableBz, scalar)
	require.NoError(t, err)
	require.True(t, ringSig.Verify(signableBz),
		"the ring signature just produced must satisfy the production verifier")

	sigBz, err := ringSig.Serialize()
	require.NoError(t, err)
	return sigBz
}

// --- determinism -------------------------------------------------------

func TestReceipt_DigestAndSignatureAreDeterministic(t *testing.T) {
	key := secp256k1.GenPrivKey()
	reqSig := []byte("ring-signature-bytes-for-relay-A")
	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))

	require.Equal(t,
		receiptDigest(reqSig, res.PayloadHash),
		receiptDigest(reqSig, res.PayloadHash))

	require.Equal(t,
		signTestReceipt(t, key, reqSig, res.PayloadHash),
		signTestReceipt(t, key, reqSig, res.PayloadHash),
		"cosmos secp256k1 is expected to use a deterministic nonce (RFC6979)")
}

// --- the property the feature exists for -------------------------------

func TestReceipt_BindsExactlyOneRequestToOneResponse(t *testing.T) {
	key := secp256k1.GenPrivKey()
	pub := key.PubKey()

	reqSigA := []byte("ring-signature-of-request-A")
	reqSigB := []byte("ring-signature-of-request-B")
	resA := newReceiptTestResponse(t, []byte(`{"result":"0xAAAA"}`))
	resB := newReceiptTestResponse(t, []byte(`{"result":"0xBBBB"}`))

	receiptA := signTestReceipt(t, key, reqSigA, resA.PayloadHash)

	cases := []struct {
		name       string
		reqSig     []byte
		payload    []byte
		wantVerify bool
	}{
		{"the genuine pair (A,A)", reqSigA, resA.PayloadHash, true},
		{"same request, other response (A,B)", reqSigA, resB.PayloadHash, false},
		{"other request, same response (B,A)", reqSigB, resA.PayloadHash, false},
		{"entirely different pair (B,B)", reqSigB, resB.PayloadHash, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantVerify,
				verifyTestReceipt(pub, tc.reqSig, tc.payload, receiptA))
		})
	}
}

func TestReceipt_SingleBitFlipInResponseBodyBreaksIt(t *testing.T) {
	key := secp256k1.GenPrivKey()
	pub := key.PubKey()
	reqSig := []byte("ring-signature-of-request-A")

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":"0x10f2c"}`)
	res := newReceiptTestResponse(t, body)
	receipt := signTestReceipt(t, key, reqSig, res.PayloadHash)
	require.True(t, verifyTestReceipt(pub, reqSig, res.PayloadHash, receipt))

	for i := range body {
		for bit := 0; bit < 8; bit++ {
			tampered := bytes.Clone(body)
			tampered[i] ^= 1 << bit

			tamperedRes := newReceiptTestResponse(t, tampered)
			require.False(t,
				verifyTestReceipt(pub, reqSig, tamperedRes.PayloadHash, receipt),
				"receipt verified against a body tampered at byte %d bit %d", i, bit)
		}
	}
}

func TestReceipt_FromAnotherSupplierDoesNotVerify(t *testing.T) {
	keyA := secp256k1.GenPrivKey()
	keyB := secp256k1.GenPrivKey()

	reqSig := []byte("ring-signature-of-request-A")
	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))

	receiptFromB := signTestReceipt(t, keyB, reqSig, res.PayloadHash)
	require.False(t, verifyTestReceipt(keyA.PubKey(), reqSig, res.PayloadHash, receiptFromB))
}

// --- domain separation -------------------------------------------------

func TestReceipt_PreimageDisjointFromRelayResponseAtFirstByte(t *testing.T) {
	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))

	// The RelayResponse signable preimage, mirroring GetSignableBytesHash.
	signable := *res
	signable.Meta.SupplierOperatorSignature = nil
	signable.Payload = nil
	respPreimage, err := signable.Marshal()
	require.NoError(t, err)

	recPreimage := receiptPreimage([]byte("ring-sig"), res.PayloadHash)

	t.Logf("RelayResponse preimage starts 0x%02x (protobuf field 1, wiretype 2)", respPreimage[0])
	t.Logf("receipt       preimage starts 0x%02x (%q)", recPreimage[0], recPreimage[0])

	require.NotEqual(t, respPreimage[0], recPreimage[0],
		"the two signing contexts must be disjoint at the first byte")
	require.True(t, bytes.HasPrefix(recPreimage, []byte(receiptDomainTag)))
	require.False(t, bytes.HasPrefix(respPreimage, []byte(receiptDomainTag)))
}

func TestReceipt_SignatureIsNotValidAsAResponseSignature(t *testing.T) {
	key := secp256k1.GenPrivKey()
	pub := key.PubKey()

	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))
	reqSig := []byte("ring-signature-of-request-A")
	receipt := signTestReceipt(t, key, reqSig, res.PayloadHash)

	// Pass the receipt off as the RelayResponse signature.
	res.Meta.SupplierOperatorSignature = receipt
	respSignable, err := res.GetSignableBytesHash()
	require.NoError(t, err)
	require.False(t, pub.VerifySignature(respSignable[:], receipt),
		"a receipt signature must not verify as a RelayResponse signature")

	// And the reverse.
	res.Meta.SupplierOperatorSignature = nil
	respHash, err := res.GetSignableBytesHash()
	require.NoError(t, err)
	respSig, err := key.Sign(respHash[:])
	require.NoError(t, err)
	require.False(t, verifyTestReceipt(pub, reqSig, res.PayloadHash, respSig),
		"a RelayResponse signature must not verify as a receipt")
}

// --- the encoding invariant --------------------------------------------

func TestReceipt_OneVariableLengthFieldYieldsNoPreimageCollisions(t *testing.T) {
	seen := make(map[string][2]string)

	for sigLen := 0; sigLen <= 72; sigLen++ {
		for _, filler := range []byte{0x00, 0x41, 0xff} {
			reqSig := bytes.Repeat([]byte{filler}, sigLen)
			for _, body := range []string{"a", "bb", "ccc"} {
				ph := sha256.Sum256([]byte(body))
				pre := string(receiptPreimage(reqSig, ph[:]))
				id := [2]string{string(reqSig), body}
				if prev, dup := seen[pre]; dup && prev != id {
					t.Fatalf("preimage collision between %v and %v", prev, id)
				}
				seen[pre] = id
			}
		}
	}
	t.Logf("%d distinct (reqSig, payloadHash) pairs produced %d distinct preimages",
		len(seen), len(seen))
}

// TestReceipt_TwoVariableLengthFieldsWouldCollide is the counterexample the
// invariant exists to prevent. If PayloadHash were ever replaced by a
// variable-length field, this is what happens.
func TestReceipt_TwoVariableLengthFieldsWouldCollide(t *testing.T) {
	naive := func(a, b []byte) []byte { return append(append([]byte{}, a...), b...) }

	require.Equal(t,
		naive([]byte("abc"), []byte("de")),
		naive([]byte("ab"), []byte("cde")),
		"setup error: the counterexample should collide")

	t.Log(`("abc","de") and ("ab","cde") both encode to "abcde": a signature over`)
	t.Log("one would be valid for the other. This is why the scheme keeps exactly")
	t.Log("one variable-length field. A second requires explicit length prefixes.")
}

// --- the verifier contract ---------------------------------------------

// TestReceipt_VerifierMustPassTheDigestNotThePreimage documents the trap that
// costs a foreign implementer a day: cosmos secp256k1 hashes its input
// internally on both sign and verify, so what is signed is
// sha256(sha256(preimage)) and the verifier is handed the 32-byte digest.
func TestReceipt_VerifierMustPassTheDigestNotThePreimage(t *testing.T) {
	key := secp256k1.GenPrivKey()
	pub := key.PubKey()

	reqSig := []byte("ring-signature-of-request-A")
	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))
	receipt := signTestReceipt(t, key, reqSig, res.PayloadHash)

	digest := receiptDigest(reqSig, res.PayloadHash)
	preimage := receiptPreimage(reqSig, res.PayloadHash)
	doubleHashed := sha256.Sum256(digest[:])

	require.True(t, pub.VerifySignature(digest[:], receipt),
		"verifying with the 32-byte digest must succeed")
	require.False(t, pub.VerifySignature(preimage, receipt),
		"verifying with the raw preimage must fail")
	require.False(t, pub.VerifySignature(doubleHashed[:], receipt),
		"verifying with a pre-applied second hash must fail")
}

// TestReceipt_BytesAreCanonicalAndTamperEvident: cosmos rejects high-S
// signatures, so a third party cannot produce a different byte string that
// also verifies. The receipt therefore doubles as a unique identifier.
func TestReceipt_BytesAreCanonicalAndTamperEvident(t *testing.T) {
	key := secp256k1.GenPrivKey()
	pub := key.PubKey()

	reqSig := []byte("ring-signature-of-request-A")
	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))
	receipt := signTestReceipt(t, key, reqSig, res.PayloadHash)

	require.Len(t, receipt, 64, "cosmos secp256k1 signatures are 64 bytes R||S")

	for i := range receipt {
		tampered := bytes.Clone(receipt)
		tampered[i] ^= 0x01
		require.False(t, verifyTestReceipt(pub, reqSig, res.PayloadHash, tampered),
			"a receipt with byte %d flipped still verified", i)
	}

	require.False(t, verifyTestReceipt(pub, reqSig, res.PayloadHash, receipt[:63]),
		"a truncated receipt must not verify")
	require.False(t, verifyTestReceipt(pub, reqSig, res.PayloadHash,
		append(bytes.Clone(receipt), 0x00)),
		"an extended receipt must not verify")
}

// --- genuine production inputs -----------------------------------------

func TestReceipt_BindingHoldsWithAGenuineRingSignature(t *testing.T) {
	supplierKey := secp256k1.GenPrivKey()
	pub := supplierKey.PubKey()

	appKey := secp256k1.GenPrivKey()
	gatewayKey := secp256k1.GenPrivKey()
	members := []cryptotypes.PubKey{appKey.PubKey(), gatewayKey.PubKey()}

	reqA := newReceiptTestRelayRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
	reqB := newReceiptTestRelayRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","id":2}`))
	reqSigA := ringSignTestRequest(t, reqA, members, gatewayKey)
	reqSigB := ringSignTestRequest(t, reqB, members, gatewayKey)

	t.Logf("real bLSAG signature: %d bytes at ring size %d", len(reqSigA), len(members))

	resA := newReceiptTestResponse(t, []byte(`{"result":"0xAAAA"}`))
	resB := newReceiptTestResponse(t, []byte(`{"result":"0xBBBB"}`))
	receiptA := signTestReceipt(t, supplierKey, reqSigA, resA.PayloadHash)

	require.True(t, verifyTestReceipt(pub, reqSigA, resA.PayloadHash, receiptA))
	require.False(t, verifyTestReceipt(pub, reqSigA, resB.PayloadHash, receiptA))
	require.False(t, verifyTestReceipt(pub, reqSigB, resA.PayloadHash, receiptA))
	require.False(t, verifyTestReceipt(pub, reqSigB, resB.PayloadHash, receiptA))
}

// TestReceipt_ReSigningTheSameRequestYieldsADifferentReceipt makes the
// "grinding" claim concrete: bLSAG uses a random nonce, so a caller can obtain
// unlimited distinct valid values for the receipt's variable field. That is
// precisely why the domain tag matters — the caller has wide influence over
// bytes a money-bearing key is asked to sign.
func TestReceipt_ReSigningTheSameRequestYieldsADifferentReceipt(t *testing.T) {
	supplierKey := secp256k1.GenPrivKey()
	pub := supplierKey.PubKey()

	appKey := secp256k1.GenPrivKey()
	gatewayKey := secp256k1.GenPrivKey()
	members := []cryptotypes.PubKey{appKey.PubKey(), gatewayKey.PubKey()}
	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))

	req := newReceiptTestRelayRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
	sig1 := ringSignTestRequest(t, req, members, gatewayKey)
	sig2 := ringSignTestRequest(t, req, members, gatewayKey)

	require.NotEqual(t, sig1, sig2,
		"bLSAG is expected to use a random nonce, so two signings must differ")

	r1 := signTestReceipt(t, supplierKey, sig1, res.PayloadHash)
	r2 := signTestReceipt(t, supplierKey, sig2, res.PayloadHash)
	require.NotEqual(t, r1, r2)

	require.True(t, verifyTestReceipt(pub, sig1, res.PayloadHash, r1))
	require.False(t, verifyTestReceipt(pub, sig2, res.PayloadHash, r1))
}

// TestReceipt_BindingHoldsAcrossRingSizes covers what the common mainnet
// pattern does not exercise. Ring membership is [app] + [delegated gateways],
// padded with a placeholder when there are none (rings/client.go:241,250) — a
// consequence of on-chain delegation, not a setting. Apps on mainnet today
// delegate to a single gateway, making n=2 near-universal; nothing prevents
// more.
func TestReceipt_BindingHoldsAcrossRingSizes(t *testing.T) {
	supplierKey := secp256k1.GenPrivKey()
	pub := supplierKey.PubKey()
	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))
	other := newReceiptTestResponse(t, []byte(`{"result":"0x2"}`))

	for _, extraGateways := range []int{0, 1, 3, 8} {
		appKey := secp256k1.GenPrivKey()
		signerKey := secp256k1.GenPrivKey()
		members := []cryptotypes.PubKey{appKey.PubKey(), signerKey.PubKey()}
		for i := 0; i < extraGateways; i++ {
			members = append(members, secp256k1.GenPrivKey().PubKey())
		}

		req := newReceiptTestRelayRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
		reqSig := ringSignTestRequest(t, req, members, signerKey)

		n := len(members)
		require.Equal(t, 69+65*n, len(reqSig),
			"bLSAG signature length is 69+65n; ring size %d", n)
		t.Logf("ring n=%-2d -> signature %4d bytes, preimage %d bytes",
			n, len(reqSig), len(receiptPreimage(reqSig, res.PayloadHash)))

		receipt := signTestReceipt(t, supplierKey, reqSig, res.PayloadHash)
		require.True(t, verifyTestReceipt(pub, reqSig, res.PayloadHash, receipt),
			"receipt must verify at ring size %d", n)
		require.False(t, verifyTestReceipt(pub, reqSig, other.PayloadHash, receipt),
			"receipt must not verify against the wrong response at ring size %d", n)

		pre := receiptPreimage(reqSig, res.PayloadHash)
		require.True(t, bytes.HasPrefix(pre, []byte(receiptDomainTag)),
			"the domain tag must survive a real variable-length field at ring size %d", n)
		require.Len(t, pre, len(receiptDomainTag)+len(reqSig)+32)
		require.Equal(t, reqSig, pre[len(receiptDomainTag):len(pre)-32],
			"the variable field must stay recoverable from the total length")
	}
}

// --- separation from claim and proof transactions ----------------------

// buildTestSignDocBytes produces the exact byte string a supplier operator key
// signs when submitting a claim: SIGN_MODE_DIRECT signs marshal(SignDoc).
func buildTestSignDocBytes(t *testing.T) []byte {
	t.Helper()

	msg := &prooftypes.MsgCreateClaim{
		SupplierOperatorAddress: "pokt1supplier0000000000000000000000000000",
		SessionHeader:           receiptTestSessionHeader(),
		RootHash:                bytes.Repeat([]byte{0xab}, 40),
	}
	anyMsg, err := codectypes.NewAnyWithValue(msg)
	require.NoError(t, err)

	body := &txtypes.TxBody{Messages: []*codectypes.Any{anyMsg}}
	bodyBz, err := body.Marshal()
	require.NoError(t, err)

	authInfo := &txtypes.AuthInfo{Fee: &txtypes.Fee{GasLimit: 200000}}
	authInfoBz, err := authInfo.Marshal()
	require.NoError(t, err)

	signDoc := &txtypes.SignDoc{
		BodyBytes:     bodyBz,
		AuthInfoBytes: authInfoBz,
		ChainId:       "pocket",
		AccountNumber: 42,
	}
	signDocBz, err := signDoc.Marshal()
	require.NoError(t, err)
	return signDocBz
}

func TestReceipt_RealSignDocIsStructurallyDisjoint(t *testing.T) {
	signDocBz := buildTestSignDocBytes(t)

	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))
	recPreimage := receiptPreimage(bytes.Repeat([]byte{0x11}, 199), res.PayloadHash)

	t.Logf("SignDoc(MsgCreateClaim) = %d bytes, starts 0x%02x", len(signDocBz), signDocBz[0])
	t.Logf("receipt preimage        = %d bytes, starts 0x%02x", len(recPreimage), recPreimage[0])

	require.NotEqual(t, signDocBz[0], recPreimage[0],
		"a real SignDoc and a receipt preimage must not share their first byte")
	require.False(t, bytes.HasPrefix(signDocBz, []byte(receiptDomainTag)),
		"a real SignDoc must not begin with the receipt domain tag")
}

// TestReceipt_AndClaimTransactionSignaturesDoNotCrossVerify is the operative
// test for domain separation: the SAME supplier key signs both, and neither
// signature is valid in the other's context.
func TestReceipt_AndClaimTransactionSignaturesDoNotCrossVerify(t *testing.T) {
	supplierKey := secp256k1.GenPrivKey()
	pub := supplierKey.PubKey()

	// Context: the claim transaction.
	signDocBz := buildTestSignDocBytes(t)
	txSig, err := supplierKey.Sign(signDocBz)
	require.NoError(t, err)
	require.True(t, pub.VerifySignature(signDocBz, txSig),
		"baseline: the tx signature must verify in its own context")

	// Context: the receipt.
	appKey := secp256k1.GenPrivKey()
	gatewayKey := secp256k1.GenPrivKey()
	members := []cryptotypes.PubKey{appKey.PubKey(), gatewayKey.PubKey()}
	res := newReceiptTestResponse(t, []byte(`{"result":"0x1"}`))
	req := newReceiptTestRelayRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
	reqSig := ringSignTestRequest(t, req, members, gatewayKey)

	receipt := signTestReceipt(t, supplierKey, reqSig, res.PayloadHash)
	require.True(t, verifyTestReceipt(pub, reqSig, res.PayloadHash, receipt),
		"baseline: the receipt must verify in its own context")

	require.False(t, verifyTestReceipt(pub, reqSig, res.PayloadHash, txSig),
		"a claim transaction signature must not verify as a receipt")
	require.False(t, pub.VerifySignature(signDocBz, receipt),
		"a receipt must not verify as a claim transaction signature")
}
