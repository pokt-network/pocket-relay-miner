//go:build test

package relayer

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"
)

// TestReceiptDigest_Golden pins the exact digest for fixed inputs.
//
// Changing this value is a BREAKING change to the receipt wire contract: every
// external verifier recomputes this digest from the request it sent and the
// response it received, and would stop matching without any error message
// pointing at the cause.
func TestReceiptDigest_Golden(t *testing.T) {
	reqSig, err := hex.DecodeString("aabbccdd11223344")
	require.NoError(t, err)
	payloadHash, err := hex.DecodeString(
		"035246ede2579f9e904957f7aae6b9ca3395dd74b18e2d36dbb4fcd12ee69931")
	require.NoError(t, err)

	got := receiptDigest(reqSig, payloadHash)

	require.Equal(t,
		"5bb8a3914486e4ff4ecde225a4c13d2939419fb5621b320bd33f44b5ac854319",
		hex.EncodeToString(got[:]),
		"receipt digest changed — this breaks every external verifier")
}

// TestReceiptDomainTag_Exact pins the tag bytes themselves.
//
// The leading byte matters: a marshaled protobuf (a RelayResponse, a Cosmos
// SignDoc) begins 0x0a, so a tag beginning 0x50 keeps the two signing contexts
// of the same supplier key disjoint at the very first byte.
func TestReceiptDomainTag_Exact(t *testing.T) {
	require.Equal(t, "POKT-RELAY-RECEIPT-v1\x00", receiptDomainTag)
	require.Len(t, receiptDomainTag, 22)
	require.Equal(t, byte(0x50), receiptDomainTag[0],
		"the tag must not start with 0x0a, the first byte of a marshaled protobuf")
}

func TestBuildReceipt_RejectsEmptyInputs(t *testing.T) {
	signer := &privKeySigner{privKey: secp256k1.GenPrivKey()}

	_, err := buildReceipt(signer, nil, make([]byte, 32))
	require.Error(t, err, "an empty request signature must be refused")

	_, err = buildReceipt(signer, []byte("sig"), nil)
	require.Error(t, err, "an empty payload hash must be refused")

	_, err = buildReceipt(nil, []byte("sig"), make([]byte, 32))
	require.Error(t, err, "a nil signer must be refused, not panic")
}

func TestBuildReceipt_ProducesAVerifiableSignature(t *testing.T) {
	priv := secp256k1.GenPrivKey()
	signer := &privKeySigner{privKey: priv}
	reqSig := []byte("ring-signature-bytes")
	payloadHash := make([]byte, 32)
	payloadHash[0] = 0xAB

	sig, err := buildReceipt(signer, reqSig, payloadHash)
	require.NoError(t, err)
	require.Len(t, sig, 64, "cosmos secp256k1 signatures are 64 bytes R||S")

	digest := receiptDigest(reqSig, payloadHash)
	require.True(t, priv.PubKey().VerifySignature(digest[:], sig))
}

func TestClientWantsReceipt(t *testing.T) {
	tests := []struct {
		name      string
		setHeader bool
		value     string
		want      bool
	}{
		{name: "absent", setHeader: false, want: false},
		{name: "true", setHeader: true, value: "true", want: true},
		{name: "TRUE", setHeader: true, value: "TRUE", want: true},
		{name: "True", setHeader: true, value: "True", want: true},
		{name: "false", setHeader: true, value: "false", want: false},
		{name: "one", setHeader: true, value: "1", want: false},
		{name: "yes", setHeader: true, value: "yes", want: false},
		{name: "on", setHeader: true, value: "on", want: false},
		{name: "empty", setHeader: true, value: "", want: false},
		{name: "garbage", setHeader: true, value: "\x00\xff", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/svc", nil)
			if tt.setHeader {
				r.Header.Set(receiptRequestHeader, tt.value)
			}
			require.Equal(t, tt.want, clientWantsReceipt(r))
		})
	}
}

func TestClientWantsReceipt_NilRequest(t *testing.T) {
	require.False(t, clientWantsReceipt(nil), "a nil request must not panic")
}

// TestFormatReceiptHeader pins the header encoding.
//
// Lowercase hex, no padding. Like the digest above, this is wire contract:
// a verifier written against one encoding rejects the other outright.
func TestFormatReceiptHeader(t *testing.T) {
	got := formatReceiptHeader([]byte{0x00, 0x01, 0xfe, 0xff})
	require.Equal(t, "v1.0001feff", got)
	require.True(t, strings.HasPrefix(got, "v1."))
}

// TestFormatReceiptHeader_IsHeaderSafe checks the property the encoding exists
// for: a raw signature can contain any byte, including 0x0a, which would break
// header framing. Every byte value must survive as printable ASCII.
func TestFormatReceiptHeader_IsHeaderSafe(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}

	got := formatReceiptHeader(all)

	value := strings.TrimPrefix(got, "v1.")
	require.Len(t, value, 512, "hex is two characters per byte")
	for i := 0; i < len(value); i++ {
		c := value[i]
		require.True(t,
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"byte %d encoded to %q, which is not lowercase hex", i, c)
	}

	decoded, err := hex.DecodeString(value)
	require.NoError(t, err)
	require.Equal(t, all, decoded, "the encoding must round-trip every byte value")
}

// TestFormatReceiptHeader_RealSignatureLength documents what a caller sees: a
// 64-byte secp256k1 signature becomes 128 hex characters plus the version.
func TestFormatReceiptHeader_RealSignatureLength(t *testing.T) {
	priv := secp256k1.GenPrivKey()
	sig, err := buildReceipt(&privKeySigner{privKey: priv}, []byte("sig"), make([]byte, 32))
	require.NoError(t, err)

	got := formatReceiptHeader(sig)
	require.Len(t, got, len("v1.")+128)
}
