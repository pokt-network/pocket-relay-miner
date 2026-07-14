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
	require.Equal(t, rejectReasonSupplierNotFound, d.rejectReason)
	require.Contains(t, d.clientMsg, gateTestSupplier)
	require.Contains(t, d.clientMsg, "not registered with any miner")
}

// TestDecideSupplierServe_NilSignerRejects: defensive — no signer configured at
// all means we cannot claim ownership, so nil state rejects.
func TestDecideSupplierServe_NilSignerRejects(t *testing.T) {
	p := &ProxyServer{logger: testLogger()} // responseSigner == nil

	d := p.decideSupplierServe(nil, gateTestSupplier, gateTestService)

	require.False(t, d.serve)
	require.Equal(t, rejectReasonSupplierNotFound, d.rejectReason)
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
