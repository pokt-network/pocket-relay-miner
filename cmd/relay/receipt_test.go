//go:build test

package relay

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"
)

// signTestReceipt produces a receipt the way the relayer does, so this file
// tests the verifier against the real construction rather than against itself.
func signTestReceipt(t *testing.T, key *secp256k1.PrivKey, reqSig, payloadHash []byte) string {
	t.Helper()
	h := sha256.New()
	h.Write([]byte(receiptDomainTag))
	h.Write(reqSig)
	h.Write(payloadHash)
	sig, err := key.Sign(h.Sum(nil))
	require.NoError(t, err)
	return "v1." + base64.URLEncoding.EncodeToString(sig)
}

func TestVerifyRelayReceipt_GenuinePairVerifies(t *testing.T) {
	key := secp256k1.GenPrivKey()
	reqSig := []byte("ring-signature-bytes")
	ph := sha256.Sum256([]byte(`{"result":"0x1"}`))

	header := signTestReceipt(t, key, reqSig, ph[:])
	require.NoError(t, VerifyRelayReceipt(header, reqSig, ph[:], key.PubKey()))
}

// TestVerifyRelayReceipt_NegativeControls is the point of the feature: a
// receipt must be useless for any pair other than the one it was issued for.
func TestVerifyRelayReceipt_NegativeControls(t *testing.T) {
	key := secp256k1.GenPrivKey()
	reqSig := []byte("ring-signature-bytes")
	ph := sha256.Sum256([]byte(`{"result":"0x1"}`))
	header := signTestReceipt(t, key, reqSig, ph[:])

	t.Run("a different response", func(t *testing.T) {
		other := sha256.Sum256([]byte(`{"result":"0x2"}`))
		require.Error(t, VerifyRelayReceipt(header, reqSig, other[:], key.PubKey()))
	})

	t.Run("a different request", func(t *testing.T) {
		require.Error(t, VerifyRelayReceipt(header, []byte("other-signature"), ph[:], key.PubKey()))
	})

	t.Run("a different supplier", func(t *testing.T) {
		other := secp256k1.GenPrivKey()
		require.Error(t, VerifyRelayReceipt(header, reqSig, ph[:], other.PubKey()))
	})
}

func TestVerifyRelayReceipt_MalformedInput(t *testing.T) {
	key := secp256k1.GenPrivKey()
	reqSig := []byte("ring-signature-bytes")
	ph := sha256.Sum256([]byte(`{"result":"0x1"}`))
	header := signTestReceipt(t, key, reqSig, ph[:])

	tests := []struct {
		name        string
		header      string
		reqSig      []byte
		payloadHash []byte
		nilPubKey   bool
	}{
		{name: "empty header", header: "", reqSig: reqSig, payloadHash: ph[:]},
		{name: "unsupported version", header: "v2." + header[3:], reqSig: reqSig, payloadHash: ph[:]},
		{name: "no version prefix", header: header[3:], reqSig: reqSig, payloadHash: ph[:]},
		{name: "not base64url", header: "v1.!!!!", reqSig: reqSig, payloadHash: ph[:]},
		{name: "truncated signature", header: "v1.AAAA", reqSig: reqSig, payloadHash: ph[:]},
		{name: "empty request signature", header: header, reqSig: nil, payloadHash: ph[:]},
		{name: "empty payload hash", header: header, reqSig: reqSig, payloadHash: nil},
		{name: "nil public key", header: header, reqSig: reqSig, payloadHash: ph[:], nilPubKey: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub := key.PubKey()
			if tt.nilPubKey {
				pub = nil
			}
			require.Error(t, VerifyRelayReceipt(tt.header, tt.reqSig, tt.payloadHash, pub))
		})
	}
}

// TestReceiptDomainTag_MatchesTheProducer guards the duplication. The tag is
// deliberately not imported from the relayer package — a verifier sharing the
// producer's constant cannot catch the producer changing it — so this pins the
// value on the verifier side too.
func TestReceiptDomainTag_MatchesTheProducer(t *testing.T) {
	require.Equal(t, "POKT-RELAY-RECEIPT-v1\x00", receiptDomainTag)
	require.Len(t, receiptDomainTag, 22)
}
