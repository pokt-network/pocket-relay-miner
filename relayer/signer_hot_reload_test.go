//go:build test

package relayer

import (
	"context"
	"sync"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/stretchr/testify/require"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

const (
	hotReloadSupplierA = "pokt1hotreloadsuppliera"
	hotReloadSupplierB = "pokt1hotreloadsupplierb"
)

// hotReloadKeys builds a key set for the given operator addresses.
//
// The addresses here are literals, not derived from the generated keys, because
// ResponseSigner only ever looks an address up -- it never re-derives one. In
// production the two always agree: every key provider derives the operator
// address from the key material (see keys.parseHexKeyWithAddress), which is
// also why a reload can only add and remove addresses.
func hotReloadKeys(t *testing.T, addrs ...string) map[string]cryptotypes.PrivKey {
	t.Helper()
	ks := make(map[string]cryptotypes.PrivKey, len(addrs))
	for _, addr := range addrs {
		ks[addr] = secp256k1.GenPrivKey()
	}
	return ks
}

func signableResponse() *servicetypes.RelayResponse {
	return &servicetypes.RelayResponse{
		Meta: servicetypes.RelayResponseMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      "pokt1hotreloadapp",
				ServiceId:               "develop-http",
				SessionId:               "hot-reload-session",
				SessionStartBlockHeight: 1,
				SessionEndBlockHeight:   20,
			},
		},
		Payload: []byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`),
	}
}

// TestReplaceKeysIsVisibleThroughAReferenceCapturedBeforehand is the reason
// hot reload cannot be a pointer swap on ProxyServer.
//
// The *ResponseSigner built at startup is captured by SIX independent holders:
// ProxyServer.responseSigner, RelayGRPCService.responseSigner (via
// RelayGRPCServiceConfig.ResponseSigner), the WebSocket handler, the HTTP
// streaming handler, SimulationVerifier.signer, and ResponseSignerAdapter. A
// reload that replaced ProxyServer's field would leave the other five holding
// the old key set and signing with retired keys, silently. So the key set has
// to be mutable INSIDE the signer, and every holder of the pointer has to see
// the change.
//
// `captured` here stands for those five other holders: it is a second reference
// to the same object, obtained before the reload.
func TestReplaceKeysIsVisibleThroughAReferenceCapturedBeforehand(t *testing.T) {
	rs, err := NewResponseSigner(testLogger(), hotReloadKeys(t, hotReloadSupplierA))
	require.NoError(t, err)

	captured := rs
	require.True(t, captured.HasSigner(hotReloadSupplierA))
	require.False(t, captured.HasSigner(hotReloadSupplierB))

	rs.ReplaceKeys(hotReloadKeys(t, hotReloadSupplierB))

	require.False(t, captured.HasSigner(hotReloadSupplierA),
		"a holder captured before the reload still sees the removed key")
	require.True(t, captured.HasSigner(hotReloadSupplierB),
		"a holder captured before the reload does not see the added key")
	require.Equal(t, []string{hotReloadSupplierB}, captured.GetOperatorAddresses())
}

// TestReplaceKeysStopsSigningForARemovedKey is the operator-visible half: the
// point of hot reload is that pulling a key from the secret stops a RUNNING
// relayer from signing for that supplier, without a restart.
func TestReplaceKeysStopsSigningForARemovedKey(t *testing.T) {
	rs, err := NewResponseSigner(testLogger(), hotReloadKeys(t, hotReloadSupplierA, hotReloadSupplierB))
	require.NoError(t, err)

	resp := signableResponse()
	require.NoError(t, rs.SignRelayResponse(resp, hotReloadSupplierA))
	require.NotEmpty(t, resp.Meta.SupplierOperatorSignature)

	// The key file now lists only B.
	rs.ReplaceKeys(hotReloadKeys(t, hotReloadSupplierB))

	after := signableResponse()
	err = rs.SignRelayResponse(after, hotReloadSupplierA)
	require.Error(t, err, "signing must fail for a key that was removed")
	require.Empty(t, after.Meta.SupplierOperatorSignature)
	// The "available" list names the CURRENT set, not the snapshot taken at
	// construction: an operator reading this error while diagnosing a removal
	// must not be told the removed key is still available. Asserted as the
	// whole list, because the removed address legitimately appears earlier in
	// the message as the operator that was asked for.
	require.Contains(t, err.Error(), "available: ["+hotReloadSupplierB+"]")

	// B, untouched by the removal, keeps signing.
	kept := signableResponse()
	sig, signErr := rs.SignRelayResponseWithContext(context.Background(), kept, hotReloadSupplierB)
	require.NoError(t, signErr)
	require.NotEmpty(t, sig)
}

// TestReplaceKeysHandlesAddedRemovedAndUnchangedInOneReload is the shape a real
// reload has: one key stays, one goes, one arrives, all in the same rewrite of
// the key file.
//
// The unchanged key is the assertion that matters. An implementation that
// mutated the live map in place -- clear it, then re-add what the file lists --
// would leave the UNCHANGED supplier unable to sign for the width of that
// window, losing relays for a supplier nobody touched. Replacing an immutable
// snapshot cannot produce that window: a reader either sees the whole old set
// or the whole new one.
func TestReplaceKeysHandlesAddedRemovedAndUnchangedInOneReload(t *testing.T) {
	const (
		stays   = "pokt1hotreloadstays"
		removed = "pokt1hotreloadremoved"
		added   = "pokt1hotreloadadded"
	)

	initial := hotReloadKeys(t, stays, removed)
	rs, err := NewResponseSigner(testLogger(), initial)
	require.NoError(t, err)

	// Same key material for the address that stays; that is what "unchanged"
	// means in a key file that was rewritten for other reasons.
	next := map[string]cryptotypes.PrivKey{
		stays: initial[stays],
		added: secp256k1.GenPrivKey(),
	}
	rs.ReplaceKeys(next)

	require.True(t, rs.HasSigner(stays), "the untouched supplier lost its key")
	require.True(t, rs.HasSigner(added))
	require.False(t, rs.HasSigner(removed))
	require.Equal(t, []string{added, stays}, rs.GetOperatorAddresses())

	// The untouched supplier still signs, and with the same key: the signature
	// over a fixed response is unchanged.
	before := signableResponse()
	rsBefore, err := NewResponseSigner(testLogger(), initial)
	require.NoError(t, err)
	require.NoError(t, rsBefore.SignRelayResponse(before, stays))

	after := signableResponse()
	require.NoError(t, rs.SignRelayResponse(after, stays))
	require.Equal(t, before.Meta.SupplierOperatorSignature, after.Meta.SupplierOperatorSignature,
		"the unchanged key produced a different signature after the reload")
}

// TestConcurrentSignAndReplaceKeys is the race the TODO on SetResponseSigner
// warned about. HasSigner runs once per relay on the hot path
// (decideSupplierServe), so a reload lands concurrently with reads by
// construction. Run under -race.
func TestConcurrentSignAndReplaceKeys(t *testing.T) {
	rs, err := NewResponseSigner(testLogger(), hotReloadKeys(t, hotReloadSupplierA, hotReloadSupplierB))
	require.NoError(t, err)

	const readers = 8
	const iterations = 200

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = rs.HasSigner(hotReloadSupplierA)
				_ = rs.GetOperatorAddresses()
				_ = rs.SignRelayResponse(signableResponse(), hotReloadSupplierB)
			}
		}()
	}

	for i := 0; i < iterations; i++ {
		if i%2 == 0 {
			rs.ReplaceKeys(hotReloadKeys(t, hotReloadSupplierA, hotReloadSupplierB))
		} else {
			rs.ReplaceKeys(hotReloadKeys(t, hotReloadSupplierB))
		}
	}
	close(stop)
	wg.Wait()

	// The last write wins and is observable: no torn state, no lost update.
	rs.ReplaceKeys(hotReloadKeys(t, hotReloadSupplierA))
	require.Equal(t, []string{hotReloadSupplierA}, rs.GetOperatorAddresses())
}

// TestReplaceKeysWithAnEmptySetRejectsEverything covers the boundary an
// operator can actually reach: a key file emptied or a secret whose keys were
// all pulled. It must reject, not panic and not keep the previous set.
func TestReplaceKeysWithAnEmptySetRejectsEverything(t *testing.T) {
	rs, err := NewResponseSigner(testLogger(), hotReloadKeys(t, hotReloadSupplierA))
	require.NoError(t, err)

	rs.ReplaceKeys(nil)

	require.False(t, rs.HasSigner(hotReloadSupplierA))
	require.Empty(t, rs.GetOperatorAddresses())
	require.Error(t, rs.SignRelayResponse(signableResponse(), hotReloadSupplierA))
}
