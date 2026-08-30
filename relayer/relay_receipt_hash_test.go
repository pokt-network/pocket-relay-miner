//go:build test

package relayer

import (
	"context"
	"net/http"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
)

// The receipt contract, in one sentence: a client holding (request, signed
// response) must be able to derive the SMST leaf key the miner used, byte for
// byte. Everything that would let a receipt holder prove a served relay was
// never claimed rests on that equality -- an inclusion proof needs the key.
//
// This test drives the TWO REAL PRODUCTION BUILDERS over one backend body:
// the one whose bytes reach the client (ResponseSigner.BuildAndSignRelayResponseFromBody,
// called at proxy.go:1215) and the one whose bytes reach the tree
// (relayProcessor.ProcessRelay, called at proxy.go:2493). A test that built one
// RelayResponse and handed the same object to both sides would pass while
// production diverges, which is the shape this repository calls decoration.

const (
	receiptTestSupplier  = "pokt1testsupplieroperator"
	receiptTestServiceID = "develop-http"
	receiptTestSessionID = "session-receipt-test"
	receiptTestAppAddr   = "pokt1testapplicationaddr"
)

// receiptFixture wires a real ResponseSigner and a real relayProcessor over the
// same signing key, which is what a single relayer process has.
type receiptFixture struct {
	signer    *ResponseSigner
	processor *relayProcessor
	request   *servicetypes.RelayRequest
	requestBz []byte
}

func newReceiptFixture(t *testing.T) *receiptFixture {
	t.Helper()

	keys := map[string]cryptotypes.PrivKey{receiptTestSupplier: secp256k1.GenPrivKey()}
	rs, err := NewResponseSigner(testLogger(), keys)
	require.NoError(t, err)

	// The publisher is nil on purpose: ProcessRelay builds the mined message and
	// returns it; publishing is the caller's job, and this test is about the
	// message's contents. The ring client is nil for the same reason -- signature
	// validation happens before ProcessRelay is ever reached.
	proc := NewRelayProcessor(testLogger(), nil, NewResponseSignerAdapter(rs), nil)

	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      receiptTestAppAddr,
				ServiceId:               receiptTestServiceID,
				SessionId:               receiptTestSessionID,
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   119,
			},
			SupplierOperatorAddress: receiptTestSupplier,
		},
		Payload: []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`),
	}
	reqBz, err := req.Marshal()
	require.NoError(t, err)

	return &receiptFixture{signer: rs, processor: proc, request: req, requestBz: reqBz}
}

// deriveLeafKeyClientSide is the derivation a receipt holder can perform with
// nothing but the bytes it received: unmarshal the signed response, dehydrate
// its payload (PayloadHash travels inside the signed bytes, so the response
// still verifies without it), pair it with the request it sent, marshal, hash.
//
// It mirrors relay_processor.go:140-158 deliberately. If that ever moves into a
// shared constructor, this function is what should call it.
func deriveLeafKeyClientSide(
	t *testing.T,
	request *servicetypes.RelayRequest,
	signedResponseBz []byte,
) []byte {
	t.Helper()

	clientResp := &servicetypes.RelayResponse{}
	require.NoError(t, clientResp.Unmarshal(signedResponseBz),
		"the client must be able to unmarshal what the relayer sent it")

	clientResp.Payload = nil

	relay := &servicetypes.Relay{Req: request, Res: clientResp}
	relayBz, err := relay.Marshal()
	require.NoError(t, err)

	hash := protocol.GetRelayHashFromBytes(relayBz)
	return hash[:]
}

// TestMinedLeafMatchesWireResponse is the measurement the whole receipt arm
// hangs on. It is expected to hold; if it does not, the receipt design cannot
// proceed as written and the divergence -- not the test -- is what needs fixing.
func TestMinedLeafMatchesWireResponse(t *testing.T) {
	f := newReceiptFixture(t)

	// One backend body, served once. Both paths below see exactly these bytes,
	// which is what production does: proxy.go hands respBody to the signer at
	// :1215 and the same respBody to ProcessRelay at :2493.
	respBody := []byte(`{"jsonrpc":"2.0","id":1,"result":"0x10f2c"}`)
	respHeaders := http.Header{"Content-Type": []string{"application/json"}}

	// (a) what the CLIENT receives
	_, signedResponseBz, err := f.signer.BuildAndSignRelayResponseFromBody(
		f.request, respBody, respHeaders, http.StatusOK,
	)
	require.NoError(t, err)

	// (b) what reaches the TREE
	mined, err := f.processor.ProcessRelay(
		context.Background(),
		f.requestBz,
		respBody,
		receiptTestSupplier,
		receiptTestServiceID,
		100,
	)
	require.NoError(t, err)
	require.NotNil(t, mined, "the relay must be mined for a leaf key to exist")
	require.Len(t, mined.RelayHash, 32, "a leaf key is a 32-byte hash")

	clientHash := deriveLeafKeyClientSide(t, f.request, signedResponseBz)

	require.Equal(t, mined.RelayHash, clientHash,
		"the leaf key a receipt holder derives must equal the one the miner used; "+
			"without this equality no inclusion proof can be requested for a receipt")
}

// TestMinedLeafMatchesWireResponse_WebSocket exists to keep the failure above
// honest. If the HTTP case failed because this test's fixture or its client-side
// derivation were wrong, the WebSocket case would fail too -- it uses the same
// fixture, the same derivation, the same assertion, and the same request object.
//
// The only thing that changes is which builder produces the client's bytes.
// BuildAndSignWebSocketRelayResponse (signer.go:281-286) sets Payload to the raw
// payload with no HTTP wrapping, which is exactly what buildRelayResponse
// (relay_processor.go:222-227) does. Same payload on both sides.
//
// So a PASS here says three things the HTTP failure alone cannot:
//   - the cross-marshal works: the client marshalling a CONSTRUCTED request plus
//     an UNMARSHALED response reproduces what the relayer marshalled from an
//     UNMARSHALED request plus a CONSTRUCTED response. That is the gogoproto
//     round-trip question, answered by measurement rather than by reading.
//   - the client-side dehydration (Res.Payload = nil, Req.Payload kept) is right.
//   - the HTTP failure is caused by the payload the two builders disagree on,
//     and by nothing else in this file.
func TestMinedLeafMatchesWireResponse_WebSocket(t *testing.T) {
	f := newReceiptFixture(t)

	rawPayload := []byte(`{"jsonrpc":"2.0","id":1,"result":"0x10f2c"}`)

	// (a) what the CLIENT receives on a WebSocket relay
	_, signedResponseBz, err := f.signer.BuildAndSignWebSocketRelayResponse(f.request, rawPayload)
	require.NoError(t, err)

	// (b) what reaches the TREE -- websocket.go:834 hands ProcessRelay the same
	// msg.data it just signed
	mined, err := f.processor.ProcessRelay(
		context.Background(),
		f.requestBz,
		rawPayload,
		receiptTestSupplier,
		receiptTestServiceID,
		100,
	)
	require.NoError(t, err)
	require.NotNil(t, mined)

	clientHash := deriveLeafKeyClientSide(t, f.request, signedResponseBz)

	require.Equal(t, mined.RelayHash, clientHash,
		"with one payload on both sides the derivation must close; if this fails "+
			"the fixture or the client-side derivation is wrong, not the pipeline")
}
