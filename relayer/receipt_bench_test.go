//go:build test

package relayer

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
)

// Microbenchmarks for the relay receipt.
//
// These isolate what the live A/B run tries to detect at process level. A
// single secp256k1 signature is small enough that on a loaded development
// machine the live delta can sit inside the noise floor; when that happens
// these numbers are the answer and the live run supports only the weaker
// claim of no regression in p99 or RSS.
//
//	go test -tags test -bench Receipt -benchmem -run '^$' ./relayer/

// benchReqSig is sized like the real thing: a bLSAG ring signature is 69+65n
// bytes, so 199 at ring size 2 — the near-universal mainnet case, since apps
// today delegate to a single gateway.
var benchReqSig = make([]byte, 199)

// BenchmarkBuildReceipt measures the whole cost a caller pays for asking:
// digest plus signature.
func BenchmarkBuildReceipt(b *testing.B) {
	signer := &privKeySigner{privKey: secp256k1.GenPrivKey()}
	ph := sha256.Sum256([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10f2c"}`))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := buildReceipt(signer, benchReqSig, ph[:]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReceiptDigestOnly separates hashing from signing, so the split
// between the two is measured rather than assumed. BuildReceipt minus this is
// the signature's share.
func BenchmarkReceiptDigestOnly(b *testing.B) {
	ph := sha256.Sum256([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10f2c"}`))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = receiptDigest(benchReqSig, ph[:])
	}
}

// BenchmarkClientWantsReceipt is the cost paid by callers who do NOT want a
// receipt. It runs on every relay, so it has to be negligible — this is the
// number behind the claim that the feature is free when unrequested.
func BenchmarkClientWantsReceipt(b *testing.B) {
	r := httptest.NewRequest(http.MethodPost, "/svc", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = clientWantsReceipt(r)
	}
}

// BenchmarkSignRelayResponse is the reference point: the signature the relayer
// already performs on every relay. The receipt should cost about the same, and
// a large divergence means something unintended is happening.
func BenchmarkSignRelayResponse(b *testing.B) {
	priv := secp256k1.GenPrivKey()
	signer := &privKeySigner{privKey: priv}
	digest := sha256.Sum256([]byte("reference"))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := signer.Sign(digest); err != nil {
			b.Fatal(err)
		}
	}
}
