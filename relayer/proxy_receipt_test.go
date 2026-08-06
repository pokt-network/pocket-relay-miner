//go:build test

package relayer

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// backendForReceiptTests serves a fixed JSON-RPC body.
func backendForReceiptTests(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// parseReceiptHeader decodes "v1.<base64url>" exactly as an external caller
// must. It deliberately does not reach into anything the relayer knows.
func parseReceiptHeader(t *testing.T, header string) []byte {
	t.Helper()
	encoded, ok := strings.CutPrefix(header, "v1.")
	require.True(t, ok, "receipt header must carry the v1 prefix, got %q", header)
	sig, err := base64.URLEncoding.DecodeString(encoded)
	require.NoError(t, err)
	return sig
}

func (f *simHTTPFixture) supplierPubKey() cryptotypes.PubKey {
	return f.supplierPriv.PubKey()
}

// signerWithNoKeyFor builds a ResponseSigner holding a key under a DIFFERENT
// operator address, so signerFor(supplierAddr) returns nil.
func signerWithNoKeyFor(t *testing.T) *ResponseSigner {
	t.Helper()
	other := secp256k1.GenPrivKey()
	signer, err := NewResponseSigner(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		map[string]cryptotypes.PrivKey{"pokt1someotheroperatoraddress": other},
	)
	require.NoError(t, err)
	return signer
}

// TestHandleRelay_NoReceiptHeaderWhenNotRequested is the zero-cost guarantee:
// a caller that knows nothing about receipts must be entirely unaffected.
func TestHandleRelay_NoReceiptHeaderWhenNotRequested(t *testing.T) {
	backend := backendForReceiptTests(t, `{"jsonrpc":"2.0","result":"0x10","id":1}`)
	f := newSimHTTPFixture(t, backend.URL, ValidationModeEager)

	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "simv1:1700000000:aabbccdd")
	w := f.post(t, body, true)

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, w.Header().Get(receiptResponseHeader),
		"a caller that did not ask must not receive a receipt")
}

// TestHandleRelay_ReceiptHeaderPresentAndVerifiable verifies the receipt the
// way an EXTERNAL caller would: rebuilding the digest from the request it sent
// and the response bytes it received, never parsing the receipt for content.
func TestHandleRelay_ReceiptHeaderPresentAndVerifiable(t *testing.T) {
	backend := backendForReceiptTests(t, `{"jsonrpc":"2.0","result":"0x10","id":1}`)
	f := newSimHTTPFixture(t, backend.URL, ValidationModeEager)

	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "simv1:1700000000:aabbccdd")

	// The caller keeps the request it sent; it needs Meta.Signature later.
	var sent servicetypes.RelayRequest
	require.NoError(t, sent.Unmarshal(body))
	require.NotEmpty(t, sent.Meta.Signature)

	w := f.postWithHeaders(t, body, true, map[string]string{receiptRequestHeader: "true"})
	require.Equal(t, http.StatusOK, w.Code)

	header := w.Header().Get(receiptResponseHeader)
	require.NotEmpty(t, header, "a requested receipt must come back")
	sig := parseReceiptHeader(t, header)

	var got servicetypes.RelayResponse
	require.NoError(t, got.Unmarshal(w.Body.Bytes()))
	require.NotEmpty(t, got.PayloadHash)

	// The caller can also confirm the hash covers what it actually received.
	sum := sha256.Sum256(got.Payload)
	require.Equal(t, sum[:], got.PayloadHash)

	digest := receiptDigest(sent.Meta.Signature, got.PayloadHash)
	require.True(t, f.supplierPubKey().VerifySignature(digest[:], sig),
		"the receipt must verify against the supplier's public key")
}

// TestHandleRelay_ReceiptFromOneRelayFailsAgainstAnother is the NEGATIVE
// CONTROL. Without it this suite proves nothing — it is the property the whole
// feature exists for.
// The backend MUST return a different body per call. With a fixed body the two
// responses are byte-identical, their PayloadHashes are equal, and the
// response-side control passes vacuously — a receipt for (request1, bodyB)
// legitimately verifies against any response whose body is also B, because
// there is nothing distinguishing them. Only distinct bodies make that
// assertion mean anything.
func TestHandleRelay_ReceiptFromOneRelayFailsAgainstAnother(t *testing.T) {
	call := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x` + strings.Repeat("A", call) + `","id":1}`))
	}))
	t.Cleanup(backend.Close)

	f := newSimHTTPFixture(t, backend.URL, ValidationModeEager)

	body1 := f.buildSignedSimBody(t, f.appAddr, simTestService, "simv1:1700000000:11111111")
	var sent1 servicetypes.RelayRequest
	require.NoError(t, sent1.Unmarshal(body1))
	w1 := f.postWithHeaders(t, body1, true, map[string]string{receiptRequestHeader: "true"})
	require.Equal(t, http.StatusOK, w1.Code)
	receipt1 := parseReceiptHeader(t, w1.Header().Get(receiptResponseHeader))

	// A second relay: different session id, so a different signed request.
	body2 := f.buildSignedSimBody(t, f.appAddr, simTestService, "simv1:1700000000:22222222")
	var sent2 servicetypes.RelayRequest
	require.NoError(t, sent2.Unmarshal(body2))
	w2 := f.postWithHeaders(t, body2, true, map[string]string{receiptRequestHeader: "true"})
	require.Equal(t, http.StatusOK, w2.Code)

	require.NotEqual(t, sent1.Meta.Signature, sent2.Meta.Signature,
		"the two relays must carry distinct ring signatures for this test to mean anything")

	var got1, got2 servicetypes.RelayResponse
	require.NoError(t, got1.Unmarshal(w1.Body.Bytes()))
	require.NoError(t, got2.Unmarshal(w2.Body.Bytes()))

	require.NotEqual(t, got1.PayloadHash, got2.PayloadHash,
		"the two responses must differ for the response-side control to mean anything")

	pub := f.supplierPubKey()

	// Relay 1's receipt against relay 2's response: must fail.
	crossedResponse := receiptDigest(sent1.Meta.Signature, got2.PayloadHash)
	require.False(t, pub.VerifySignature(crossedResponse[:], receipt1),
		"a receipt must not verify against a different relay's response")

	// Relay 2's request paired with relay 1's receipt: must also fail.
	crossedRequest := receiptDigest(sent2.Meta.Signature, got1.PayloadHash)
	require.False(t, pub.VerifySignature(crossedRequest[:], receipt1),
		"a receipt must not verify against a different relay's request")
}

// TestHandleRelay_ReceiptFailureStillServesTheRelay pins fail-open: a supplier
// with no loaded signer costs the header, never a malformed one.
func TestHandleRelay_ReceiptFailureStillServesTheRelay(t *testing.T) {
	backend := backendForReceiptTests(t, `{"jsonrpc":"2.0","result":"0x10","id":1}`)
	f := newSimHTTPFixture(t, backend.URL, ValidationModeEager)

	f.proxy.responseSigner = signerWithNoKeyFor(t)

	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "simv1:1700000000:aabbccdd")
	w := f.postWithHeaders(t, body, true, map[string]string{receiptRequestHeader: "true"})

	require.Empty(t, w.Header().Get(receiptResponseHeader),
		"a receipt failure must leave the header absent, never malformed")
}
