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

	_, services := mgr.resolveAndPublishSupplierState(context.Background(), addr, nil)
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
