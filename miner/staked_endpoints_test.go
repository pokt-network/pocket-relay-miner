//go:build test

package miner

import (
	"context"
	"testing"

	sdkmath "cosmossdk.io/math"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
)

func endpoint(rt sharedtypes.RPCType) *sharedtypes.SupplierEndpoint {
	return &sharedtypes.SupplierEndpoint{Url: "http://x", RpcType: rt}
}

func TestExtractStakedEndpoints_TwoTransports(t *testing.T) {
	got := extractStakedEndpoints([]*sharedtypes.SupplierServiceConfig{
		{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{
			endpoint(sharedtypes.RPCType_JSON_RPC),
			endpoint(sharedtypes.RPCType_GRPC),
		}},
	})
	require.ElementsMatch(t, []cache.StakedEndpoint{
		{ServiceID: "eth", RpcType: "jsonrpc"},
		{ServiceID: "eth", RpcType: "grpc"},
	}, got)
}

func TestExtractStakedEndpoints_DedupSameTransport(t *testing.T) {
	got := extractStakedEndpoints([]*sharedtypes.SupplierServiceConfig{
		{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{
			endpoint(sharedtypes.RPCType_GRPC),
			endpoint(sharedtypes.RPCType_GRPC),
		}},
	})
	require.Equal(t, []cache.StakedEndpoint{{ServiceID: "eth", RpcType: "grpc"}}, got)
}

func TestExtractStakedEndpoints_UnknownRPCTypeSkipped(t *testing.T) {
	got := extractStakedEndpoints([]*sharedtypes.SupplierServiceConfig{
		{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{
			endpoint(sharedtypes.RPCType_UNKNOWN_RPC),
			endpoint(sharedtypes.RPCType_JSON_RPC),
		}},
	})
	// The unmappable endpoint is dropped; the valid one survives.
	require.Equal(t, []cache.StakedEndpoint{{ServiceID: "eth", RpcType: "jsonrpc"}}, got)
}

func TestExtractStakedEndpoints_NilSafe(t *testing.T) {
	got := extractStakedEndpoints([]*sharedtypes.SupplierServiceConfig{
		nil,
		{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{nil}},
	})
	require.Empty(t, got)
}

// TestResolveAndPublish_PersistsStakedEndpoints is the miner-side integration
// check: a chain supplier with per-transport endpoints results in a cache entry
// whose StakedEndpoints carries the (service, backend-type) pairs — the data the
// relayer needs to detect undeclared-transport traffic.
func TestResolveAndPublish_PersistsStakedEndpoints(t *testing.T) {
	const addr = "pokt1staked_endpoints"
	qc := &fakeSupplierQueryClient{
		supplier: sharedtypes.Supplier{
			OperatorAddress: addr,
			OwnerAddress:    "pokt1owner",
			Stake:           &cosmostypes.Coin{Denom: "upokt", Amount: sdkmath.NewInt(1000)},
			Services: []*sharedtypes.SupplierServiceConfig{
				{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{
					endpoint(sharedtypes.RPCType_JSON_RPC),
					endpoint(sharedtypes.RPCType_WEBSOCKET),
				}},
				{ServiceId: "poly", Endpoints: []*sharedtypes.SupplierEndpoint{
					endpoint(sharedtypes.RPCType_GRPC),
				}},
			},
		},
	}
	mgr, supplierCache, _ := newCacheTestSupplierManager(t, qc)

	_, services, _ := mgr.resolveAndPublishSupplierState(context.Background(), addr, nil)
	require.ElementsMatch(t, []string{"eth", "poly"}, services)

	state, err := supplierCache.GetSupplierState(context.Background(), addr)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.ElementsMatch(t, []cache.StakedEndpoint{
		{ServiceID: "eth", RpcType: "jsonrpc"},
		{ServiceID: "eth", RpcType: "websocket"},
		{ServiceID: "poly", RpcType: "grpc"},
	}, state.StakedEndpoints, "the per-transport stake view must be persisted for the relayer")

	// End-to-end with the relayer check: eth/jsonrpc declared, eth/grpc not.
	require.True(t, state.TransportDeclared("eth", "jsonrpc"))
	require.False(t, state.TransportDeclared("eth", "grpc"))
}

// TestPublishUnstakingState_PreservesStakedEndpoints pins the write that runs
// when a supplier starts draining.
//
// The trap it guards: a draining supplier KEEPS SERVING (IsActive is true for
// unstaking), and SetSupplierState marshals the whole struct and overwrites
// rather than merging. So a drain write that rebuilds a partial state erases
// the per-transport stake view the relayer reads -- in a fleet that is entirely
// up to date, with no old miner anywhere.
func TestPublishUnstakingState_PreservesStakedEndpoints(t *testing.T) {
	const addr = "pokt1draining"
	qc := &fakeSupplierQueryClient{
		supplier: sharedtypes.Supplier{
			OperatorAddress: addr,
			OwnerAddress:    "pokt1owner",
			Stake:           &cosmostypes.Coin{Denom: "upokt", Amount: sdkmath.NewInt(1000)},
			Services: []*sharedtypes.SupplierServiceConfig{
				{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{
					endpoint(sharedtypes.RPCType_JSON_RPC),
					endpoint(sharedtypes.RPCType_WEBSOCKET),
				}},
			},
		},
	}
	mgr, supplierCache, _ := newCacheTestSupplierManager(t, qc)
	ctx := context.Background()

	_, services, endpoints := mgr.resolveAndPublishSupplierState(ctx, addr, nil)
	require.NotEmpty(t, endpoints, "precondition: the healthy write must publish a transport view")

	// The internal state exactly as removeSupplier finds it in m.suppliers.
	state := &SupplierState{OperatorAddr: addr}
	state.stakeView.Store(&supplierStakeView{Services: services, StakedEndpoints: endpoints})
	mgr.publishUnstakingState(ctx, state)

	got, err := supplierCache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, cache.SupplierStatusUnstaking, got.Status)
	require.True(t, got.IsActive(),
		"a draining supplier still serves relays -- that is why erasing its state matters")
	require.ElementsMatch(t, endpoints, got.StakedEndpoints,
		"the drain write must republish the per-transport stake view, not erase it")
	require.True(t, got.TransportDeclared("eth", "jsonrpc"),
		"the relayer must still see eth/jsonrpc as declared while the supplier drains")

	// The helper's comment claims it copies rather than aliases. Back the claim.
	state.stakeView.Load().StakedEndpoints[0].ServiceID = "mutated-after-publish"
	again, err := supplierCache.GetSupplierState(ctx, addr)
	require.NoError(t, err)
	require.NotContains(t, endpointServiceIDs(again.StakedEndpoints), "mutated-after-publish",
		"publishUnstakingState must copy the slice it publishes, not alias the caller's")
}

func endpointServiceIDs(eps []cache.StakedEndpoint) []string {
	ids := make([]string, 0, len(eps))
	for _, e := range eps {
		ids = append(ids, e.ServiceID)
	}
	return ids
}
