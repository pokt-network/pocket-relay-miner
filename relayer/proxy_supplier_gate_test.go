//go:build test

package relayer

import (
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
)

const (
	gateTestSupplier = "pokt1supplieroperatoraddr"
	gateTestService  = "develop-http"
)

// newGateProxy builds a minimal ProxyServer whose ResponseSigner holds keys for
// exactly the given operator addresses. decideSupplierServe only reads the
// logger and responseSigner, so nothing else needs wiring.
func newGateProxy(t *testing.T, ownedSuppliers ...string) *ProxyServer {
	t.Helper()
	keys := make(map[string]cryptotypes.PrivKey, len(ownedSuppliers))
	for _, addr := range ownedSuppliers {
		keys[addr] = secp256k1.GenPrivKey()
	}
	rs, err := NewResponseSigner(testLogger(), keys)
	require.NoError(t, err)
	return &ProxyServer{logger: testLogger(), responseSigner: rs}
}

func activeState(services ...string) *cache.SupplierState {
	return &cache.SupplierState{
		Status:          cache.SupplierStatusActive,
		Staked:          true,
		Services:        services,
		OperatorAddress: gateTestSupplier,
	}
}

// TestDecideSupplierServe_BootWindowOwnedSupplier is the core of the boot-window
// fix: no registry state yet, but we hold the supplier's key, so serve
// optimistically instead of returning 503.
func TestDecideSupplierServe_BootWindowOwnedSupplier(t *testing.T) {
	p := newGateProxy(t, gateTestSupplier)

	d := p.decideSupplierServe(nil, gateTestSupplier, gateTestService)

	require.True(t, d.serve, "an owned supplier absent from the registry must be served")
	require.True(t, d.optimistic, "serving an absent-but-owned supplier is the optimistic path")
	require.Empty(t, d.rejectReason)
	require.Empty(t, d.clientMsg)
}

// TestDecideSupplierServe_UnknownSupplierRejected: no state AND we hold no key
// for it — a genuine unknown supplier, must still 503.
func TestDecideSupplierServe_UnknownSupplierRejected(t *testing.T) {
	p := newGateProxy(t, "pokt1someothersupplier") // key for a DIFFERENT supplier

	d := p.decideSupplierServe(nil, gateTestSupplier, gateTestService)

	require.False(t, d.serve, "a supplier we hold no key for must be rejected")
	require.False(t, d.optimistic)
	// The reason moved from supplier_not_found to no_local_signer when the key
	// check became the first, unconditional gate. Both were true here -- we have
	// neither state nor key -- but the missing KEY is the root cause and the
	// actionable one: it points the operator at their keys file instead of at the
	// miner. supplier_not_found had no reachable branch left and was deleted.
	require.Equal(t, rejectReasonNoLocalSigner, d.rejectReason)
	require.Contains(t, d.clientMsg, gateTestSupplier)
}

// TestDecideSupplierServe_NilSignerRejects: defensive — no signer configured at
// all means we cannot claim ownership, so nil state rejects.
func TestDecideSupplierServe_NilSignerRejects(t *testing.T) {
	p := &ProxyServer{logger: testLogger()} // responseSigner == nil

	d := p.decideSupplierServe(nil, gateTestSupplier, gateTestService)

	require.False(t, d.serve)
	// A nil signer means no key for anybody, so the key check answers "no" and
	// owns this rejection now. Still unreachable in production: handleRelay
	// rejects a nil signer with HTTP 500 before this gate runs.
	require.Equal(t, rejectReasonNoLocalSigner, d.rejectReason)
}

// TestDecideSupplierServe_ActiveForServiceServes: normal steady-state accept.
func TestDecideSupplierServe_ActiveForServiceServes(t *testing.T) {
	p := newGateProxy(t, gateTestSupplier)

	d := p.decideSupplierServe(activeState(gateTestService), gateTestSupplier, gateTestService)

	require.True(t, d.serve)
	require.False(t, d.optimistic, "present authoritative state is not the optimistic path")
	require.Empty(t, d.rejectReason)
}

// TestDecideSupplierServe_UnstakingButActiveServes: an unstaking supplier that
// still lists the service must be served (services empty at the boundary, not
// on status alone).
func TestDecideSupplierServe_UnstakingButActiveServes(t *testing.T) {
	p := newGateProxy(t, gateTestSupplier)
	state := activeState(gateTestService)
	state.Status = cache.SupplierStatusUnstaking

	d := p.decideSupplierServe(state, gateTestSupplier, gateTestService)

	require.True(t, d.serve)
	require.False(t, d.optimistic)
}

// TestDecideSupplierServe_PresentStateAuthoritative_KeyDoesNotOverride is the
// key correctness guard: the optimistic path applies ONLY to absent (nil) state.
// A KNOWN-bad supplier is rejected even when we hold its key, because present
// registry state (written by the miner from chain) is authoritative.
func TestDecideSupplierServe_PresentStateAuthoritative_KeyDoesNotOverride(t *testing.T) {
	p := newGateProxy(t, gateTestSupplier) // we DO hold the key

	tests := []struct {
		name       string
		state      *cache.SupplierState
		wantReason string
		wantMsg    string
	}{
		{
			name:       "not staked",
			state:      &cache.SupplierState{Status: cache.SupplierStatusNotStaked, Staked: false, OperatorAddress: gateTestSupplier},
			wantReason: rejectReasonSupplierInactive,
			wantMsg:    "is not_staked",
		},
		{
			name:       "active but no services",
			state:      activeState(), // staked+active, empty services
			wantReason: rejectReasonNoServices,
			wantMsg:    "has no services registered",
		},
		{
			name:       "staked for a different service",
			state:      activeState("develop-grpc"),
			wantReason: rejectReasonWrongService,
			wantMsg:    "not staked for service " + gateTestService,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.decideSupplierServe(tt.state, gateTestSupplier, gateTestService)
			require.False(t, d.serve, "known-bad supplier must be rejected even when we hold its key")
			require.False(t, d.optimistic, "optimistic must never fire for present state")
			require.Equal(t, tt.wantReason, d.rejectReason)
			require.Contains(t, d.clientMsg, tt.wantMsg)
		})
	}
}

// TestDecideSupplierServe_PresentStateButNoKeyRejects is the other half of
// TestDecideSupplierServe_PresentStateAuthoritative_KeyDoesNotOverride, and the
// two are not in tension: serving requires good state AND the ability to sign,
// so the more restrictive answer wins in both directions. That one pins that
// HOLDING the key does not override state saying the supplier is bad; this one
// pins that NOT holding it rejects even when state says everything is fine.
//
// The state here is the one the miner's teardown writes when an operator removes
// a signing key: {unstaking, staked: true, services: [...]}, which reads as
// perfectly servable. Before the key check moved to the front, this relay was
// served -- a backend call was paid for and the response then failed to sign,
// returning a signing error instead of a clean 503 -- and it stayed that way for
// as long as the cache TTL, ~42 min on mainnet.
func TestDecideSupplierServe_PresentStateButNoKeyRejects(t *testing.T) {
	p := newGateProxy(t, "pokt1someothersupplier") // key for a DIFFERENT supplier

	state := activeState(gateTestService)
	state.Status = cache.SupplierStatusUnstaking

	d := p.decideSupplierServe(state, gateTestSupplier, gateTestService)

	require.False(t, d.serve,
		"a relay we cannot sign must be rejected before the backend is called, "+
			"not after the signing fails")
	require.False(t, d.optimistic)
	require.Equal(t, rejectReasonNoLocalSigner, d.rejectReason)
	require.Contains(t, d.clientMsg, gateTestSupplier)
}

// TestDecideSupplierServe_EveryRejectionCarriesALabel pins what makes a
// rejection diagnosable. handleRelay feeds decision.rejectReason straight into
// relaysRejected.WithLabelValues(serviceID, rpcType, reason), so the label is
// whatever the gate put there: a path that rejects without setting one produces
// a series with an empty reason, which an operator filtering by reason never
// sees. The relay is still refused, so nothing looks broken -- it just becomes
// invisible, which is the worst combination for a 503 someone has to explain.
//
// The clientMsg matters for the same reason on the other side: it is the 503
// body the gateway reads.
func TestDecideSupplierServe_EveryRejectionCarriesALabel(t *testing.T) {
	const foreignSupplier = "pokt1someothersupplier"

	notStaked := &cache.SupplierState{
		Status:          cache.SupplierStatusNotStaked,
		Staked:          false,
		OperatorAddress: gateTestSupplier,
	}

	for _, tt := range []struct {
		name  string
		proxy *ProxyServer
		state *cache.SupplierState
	}{
		{name: "no signer at all", proxy: &ProxyServer{logger: testLogger()}, state: nil},
		{name: "key for another supplier", proxy: newGateProxy(t, foreignSupplier), state: nil},
		{
			name:  "key for another supplier, state present and good",
			proxy: newGateProxy(t, foreignSupplier),
			state: activeState(gateTestService),
		},
		{name: "not staked", proxy: newGateProxy(t, gateTestSupplier), state: notStaked},
		{name: "no services", proxy: newGateProxy(t, gateTestSupplier), state: activeState()},
		{
			name:  "wrong service",
			proxy: newGateProxy(t, gateTestSupplier),
			state: activeState("develop-grpc"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.proxy.decideSupplierServe(tt.state, gateTestSupplier, gateTestService)

			require.False(t, d.serve, "premise: this case must reject")
			require.NotEmpty(t, d.rejectReason,
				"a rejection with no reason becomes a relays_rejected_total series with an "+
					"empty label, invisible to anyone filtering by reason")
			require.NotEmpty(t, d.clientMsg, "the 503 body must say something")
		})
	}
}
