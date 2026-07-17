//go:build test

package relayer

import (
	"testing"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// svcConfig builds a poktroll on-chain service config with the given endpoints.
func svcConfig(serviceID string, rpcTypes ...sharedtypes.RPCType) *sharedtypes.SupplierServiceConfig {
	eps := make([]*sharedtypes.SupplierEndpoint, 0, len(rpcTypes))
	for _, rt := range rpcTypes {
		eps = append(eps, &sharedtypes.SupplierEndpoint{
			Url:     "http://example",
			RpcType: rt,
		})
	}
	return &sharedtypes.SupplierServiceConfig{ServiceId: serviceID, Endpoints: eps}
}

// cfgWithBackends builds a minimal relayer Config whose services expose the
// given canonical backend keys (URL is filler; cross-check only reads the keys).
func cfgWithBackends(services map[string][]string) *Config {
	c := &Config{Services: map[string]ServiceConfig{}}
	for svc, backends := range services {
		bmap := map[string]BackendConfig{}
		for _, bt := range backends {
			bmap[bt] = BackendConfig{URL: "http://backend"}
		}
		c.Services[svc] = ServiceConfig{Backends: bmap}
	}
	return c
}

func TestRPCTypeToBackendType_AllFive(t *testing.T) {
	cases := []struct {
		rt   sharedtypes.RPCType
		want string
	}{
		{sharedtypes.RPCType_GRPC, BackendTypeGRPC},
		{sharedtypes.RPCType_WEBSOCKET, BackendTypeWebSocket},
		{sharedtypes.RPCType_JSON_RPC, BackendTypeJSONRPC},
		{sharedtypes.RPCType_REST, BackendTypeREST},
		{sharedtypes.RPCType_COMET_BFT, BackendTypeCometBFT},
	}
	for _, tc := range cases {
		got, ok := rpcTypeToBackendType(tc.rt)
		assert.True(t, ok, "rpc type %v should map", tc.rt)
		assert.Equal(t, tc.want, got)
	}
}

func TestRPCTypeToBackendType_Unknown(t *testing.T) {
	got, ok := rpcTypeToBackendType(sharedtypes.RPCType_UNKNOWN_RPC)
	assert.False(t, ok)
	assert.Empty(t, got)

	got, ok = rpcTypeToBackendType(sharedtypes.RPCType(99))
	assert.False(t, ok)
	assert.Empty(t, got)
}

func TestExtractStakedPairs_TwoTransports(t *testing.T) {
	// Handoff miner test: a supplier with two rpc_types produces two pairs
	// with the correct backend-type strings.
	pairs := ExtractStakedPairs("pokt1abc", []*sharedtypes.SupplierServiceConfig{
		svcConfig("eth", sharedtypes.RPCType_GRPC, sharedtypes.RPCType_JSON_RPC),
	})
	require.Len(t, pairs, 2)

	byBackend := map[string]StakedPair{}
	for _, p := range pairs {
		byBackend[p.BackendType] = p
	}
	require.Contains(t, byBackend, BackendTypeGRPC)
	require.Contains(t, byBackend, BackendTypeJSONRPC)
	assert.Equal(t, "pokt1abc", byBackend[BackendTypeGRPC].Supplier)
	assert.Equal(t, "eth", byBackend[BackendTypeGRPC].ServiceID)
}

func TestExtractStakedPairs_DedupWithinService(t *testing.T) {
	// Two endpoints of the same rpc_type collapse to one pair.
	pairs := ExtractStakedPairs("pokt1abc", []*sharedtypes.SupplierServiceConfig{
		svcConfig("eth", sharedtypes.RPCType_GRPC, sharedtypes.RPCType_GRPC),
	})
	require.Len(t, pairs, 1)
	assert.Equal(t, BackendTypeGRPC, pairs[0].BackendType)
}

func TestExtractStakedPairs_UnknownRPCTypeKept(t *testing.T) {
	// Unknown enum is surfaced (BackendType empty) so the caller can debug-log
	// it rather than silently dropping it.
	pairs := ExtractStakedPairs("pokt1abc", []*sharedtypes.SupplierServiceConfig{
		svcConfig("eth", sharedtypes.RPCType_UNKNOWN_RPC),
	})
	require.Len(t, pairs, 1)
	assert.Empty(t, pairs[0].BackendType)
	assert.Equal(t, sharedtypes.RPCType_UNKNOWN_RPC, pairs[0].RPCType)
}

func TestExtractStakedPairs_NilSafe(t *testing.T) {
	pairs := ExtractStakedPairs("pokt1abc", []*sharedtypes.SupplierServiceConfig{
		nil,
		{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{nil}},
	})
	assert.Empty(t, pairs)
}

func TestCrossCheckStake_CanonicalMissing(t *testing.T) {
	// The canonical case: staked (eth, grpc) but config eth only has jsonrpc.
	cfg := cfgWithBackends(map[string][]string{"eth": {BackendTypeJSONRPC}})
	staked := ExtractStakedPairs("pokt1abc", []*sharedtypes.SupplierServiceConfig{
		svcConfig("eth", sharedtypes.RPCType_GRPC),
	})

	report := CrossCheckStake(cfg, staked)

	require.True(t, report.HasErrors())
	require.Len(t, report.Missing, 1)
	assert.Equal(t, "pokt1abc", report.Missing[0].Supplier)
	assert.Equal(t, "eth", report.Missing[0].ServiceID)
	assert.Equal(t, BackendTypeGRPC, report.Missing[0].BackendType)
	// The configured-but-unstaked jsonrpc backend is surplus.
	require.Len(t, report.Extra, 1)
	assert.Equal(t, "eth", report.Extra[0].ServiceID)
	assert.Equal(t, BackendTypeJSONRPC, report.Extra[0].BackendType)
}

func TestCrossCheckStake_FullyConfigured(t *testing.T) {
	// eth staked over grpc AND config has a grpc backend → no error, no surplus.
	cfg := cfgWithBackends(map[string][]string{"eth": {BackendTypeGRPC}})
	staked := ExtractStakedPairs("pokt1abc", []*sharedtypes.SupplierServiceConfig{
		svcConfig("eth", sharedtypes.RPCType_GRPC),
	})

	report := CrossCheckStake(cfg, staked)

	assert.False(t, report.HasErrors())
	assert.Empty(t, report.Missing)
	assert.Empty(t, report.Extra)
	assert.Empty(t, report.Unknown)
}

func TestCrossCheckStake_ServiceNotInConfigAtAll(t *testing.T) {
	// Staked for a service the relayer config does not mention → missing (error).
	cfg := cfgWithBackends(map[string][]string{"eth": {BackendTypeJSONRPC}})
	staked := ExtractStakedPairs("pokt1abc", []*sharedtypes.SupplierServiceConfig{
		svcConfig("poly", sharedtypes.RPCType_JSON_RPC),
	})

	report := CrossCheckStake(cfg, staked)

	require.Len(t, report.Missing, 1)
	assert.Equal(t, "poly", report.Missing[0].ServiceID)
	assert.Equal(t, BackendTypeJSONRPC, report.Missing[0].BackendType)
	// eth/jsonrpc is configured but nobody staked it → surplus.
	require.Len(t, report.Extra, 1)
	assert.Equal(t, "eth", report.Extra[0].ServiceID)
}

func TestCrossCheckStake_UnknownRPCTypeReported(t *testing.T) {
	cfg := cfgWithBackends(map[string][]string{"eth": {BackendTypeJSONRPC}})
	staked := ExtractStakedPairs("pokt1abc", []*sharedtypes.SupplierServiceConfig{
		svcConfig("eth", sharedtypes.RPCType_UNKNOWN_RPC),
	})

	report := CrossCheckStake(cfg, staked)

	// Unmapped rpc types never count as errors — they go to Unknown for a debug line.
	assert.False(t, report.HasErrors())
	require.Len(t, report.Unknown, 1)
	assert.Equal(t, "eth", report.Unknown[0].ServiceID)
}

func TestCrossCheckStake_MultiSupplierUnion(t *testing.T) {
	// A backend is surplus only if NO supplier stakes it. Supplier A stakes
	// (eth, grpc); supplier B stakes (eth, jsonrpc). Config has both → nothing
	// missing, nothing surplus.
	cfg := cfgWithBackends(map[string][]string{"eth": {BackendTypeGRPC, BackendTypeJSONRPC}})
	staked := append(
		ExtractStakedPairs("pokt1a", []*sharedtypes.SupplierServiceConfig{svcConfig("eth", sharedtypes.RPCType_GRPC)}),
		ExtractStakedPairs("pokt1b", []*sharedtypes.SupplierServiceConfig{svcConfig("eth", sharedtypes.RPCType_JSON_RPC)})...,
	)

	report := CrossCheckStake(cfg, staked)

	assert.Empty(t, report.Missing)
	assert.Empty(t, report.Extra)
}

func TestCrossCheckStake_Deterministic(t *testing.T) {
	// Output ordering is stable regardless of input ordering (stable CLI output).
	cfg := cfgWithBackends(map[string][]string{
		"eth":  {BackendTypeJSONRPC},
		"poly": {BackendTypeJSONRPC},
	})
	staked := []StakedPair{
		{Supplier: "pokt1b", ServiceID: "poly", RPCType: sharedtypes.RPCType_GRPC, BackendType: BackendTypeGRPC},
		{Supplier: "pokt1a", ServiceID: "eth", RPCType: sharedtypes.RPCType_GRPC, BackendType: BackendTypeGRPC},
	}

	r1 := CrossCheckStake(cfg, staked)
	r2 := CrossCheckStake(cfg, []StakedPair{staked[1], staked[0]})

	assert.Equal(t, r1.Missing, r2.Missing)
	assert.Equal(t, r1.Extra, r2.Extra)
	// Sorted by supplier then service: pokt1a/eth before pokt1b/poly.
	require.Len(t, r1.Missing, 2)
	assert.Equal(t, "pokt1a", r1.Missing[0].Supplier)
	assert.Equal(t, "pokt1b", r1.Missing[1].Supplier)
}

func TestCrossCheckStake_EmptyStakedNoPanic(t *testing.T) {
	cfg := cfgWithBackends(map[string][]string{"eth": {BackendTypeJSONRPC}})
	report := CrossCheckStake(cfg, nil)
	assert.False(t, report.HasErrors())
	// Every configured backend is surplus when nothing is staked.
	require.Len(t, report.Extra, 1)
}
