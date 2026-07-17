package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// stakeCheckSupplierServer is a minimal supplier query server that returns a
// fixed on-chain Supplier regardless of the requested address, so runCheckStake
// can be exercised end-to-end against a real gRPC endpoint.
type stakeCheckSupplierServer struct {
	suppliertypes.UnimplementedQueryServer
	services []*sharedtypes.SupplierServiceConfig
	notFound bool // when true, every Supplier query returns codes.NotFound
}

func (s *stakeCheckSupplierServer) Supplier(
	_ context.Context, req *suppliertypes.QueryGetSupplierRequest,
) (*suppliertypes.QueryGetSupplierResponse, error) {
	if s.notFound {
		return nil, status.Errorf(codes.NotFound, "supplier with operator address: %q: supplier not found", req.OperatorAddress)
	}
	return &suppliertypes.QueryGetSupplierResponse{
		Supplier: sharedtypes.Supplier{
			OperatorAddress: req.OperatorAddress,
			Services:        s.services,
		},
	}, nil
}

// startStakeCheckServer spins an in-process gRPC supplier server and returns its
// host:port. The server is stopped via t.Cleanup.
func startStakeCheckServer(t *testing.T, services []*sharedtypes.SupplierServiceConfig) string {
	return startStakeCheckServerImpl(t, &stakeCheckSupplierServer{services: services})
}

func startStakeCheckServerImpl(t *testing.T, impl *stakeCheckSupplierServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := grpc.NewServer()
	suppliertypes.RegisterQueryServer(server, impl)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String()
}

// writeSupplierKeysFile writes a valid single-key supplier keys file and returns
// its path. The key is a fixed valid 32-byte secp256k1 scalar — the derived
// address is irrelevant here because the mock server ignores it.
func writeSupplierKeysFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "supplier-keys.yaml")
	const key = "1111111111111111111111111111111111111111111111111111111111111111"
	require.NoError(t, os.WriteFile(path, []byte("keys:\n  - \""+key+"\"\n"), 0o600))
	return path
}

// stakeCheckConfig builds an in-memory relayer config whose only wiring the
// stake check reads: keys file, gRPC node (insecure), and service backends.
func stakeCheckConfig(t *testing.T, grpcAddr string, backends map[string][]string) *relayer.Config {
	services := map[string]relayer.ServiceConfig{}
	for svc, bts := range backends {
		bmap := map[string]relayer.BackendConfig{}
		for _, bt := range bts {
			bmap[bt] = relayer.BackendConfig{URL: "http://backend"}
		}
		services[svc] = relayer.ServiceConfig{Backends: bmap}
	}
	return &relayer.Config{
		Services: services,
		Keys:     relayer.KeysConfig{KeysFile: writeSupplierKeysFile(t)},
		PocketNode: relayer.PocketNodeConfig{
			QueryNodeGRPCUrl: grpcAddr,
			GRPCInsecure:     true,
		},
	}
}

func TestRunCheckStake_CanonicalMissingErrors(t *testing.T) {
	initSDKConfig()
	// On-chain: staked for eth over grpc. Config: eth has only a jsonrpc backend.
	grpcAddr := startStakeCheckServer(t, []*sharedtypes.SupplierServiceConfig{
		{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{
			{Url: "grpc://eth", RpcType: sharedtypes.RPCType_GRPC},
		}},
	})
	config := stakeCheckConfig(t, grpcAddr, map[string][]string{"eth": {relayer.BackendTypeJSONRPC}})

	err := runCheckStake(context.Background(), config, "")

	require.Error(t, err, "staked eth/grpc with no grpc backend must fail")
	assert.Contains(t, err.Error(), "no backend")
}

func TestRunCheckStake_FullyConfiguredPasses(t *testing.T) {
	initSDKConfig()
	// On-chain: staked for eth over grpc. Config: eth HAS a grpc backend.
	grpcAddr := startStakeCheckServer(t, []*sharedtypes.SupplierServiceConfig{
		{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{
			{Url: "grpc://eth", RpcType: sharedtypes.RPCType_GRPC},
		}},
	})
	config := stakeCheckConfig(t, grpcAddr, map[string][]string{"eth": {relayer.BackendTypeGRPC}})

	err := runCheckStake(context.Background(), config, "")

	require.NoError(t, err, "staked eth/grpc with a grpc backend must pass")
}

func TestRunCheckStake_NodeOverride(t *testing.T) {
	initSDKConfig()
	// Config points at a dead address; --node override points at the live mock.
	grpcAddr := startStakeCheckServer(t, []*sharedtypes.SupplierServiceConfig{
		{ServiceId: "eth", Endpoints: []*sharedtypes.SupplierEndpoint{
			{Url: "grpc://eth", RpcType: sharedtypes.RPCType_GRPC},
		}},
	})
	config := stakeCheckConfig(t, "127.0.0.1:1", map[string][]string{"eth": {relayer.BackendTypeGRPC}})

	err := runCheckStake(context.Background(), config, grpcAddr)

	require.NoError(t, err)
}

func TestRunCheckStake_NoKeysConfigured(t *testing.T) {
	initSDKConfig()
	grpcAddr := startStakeCheckServer(t, nil)
	config := &relayer.Config{
		Services: map[string]relayer.ServiceConfig{
			"eth": {Backends: map[string]relayer.BackendConfig{relayer.BackendTypeJSONRPC: {URL: "http://backend"}}},
		},
		PocketNode: relayer.PocketNodeConfig{QueryNodeGRPCUrl: grpcAddr, GRPCInsecure: true},
	}

	err := runCheckStake(context.Background(), config, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supplier keys configured")
}

func TestRunCheckStake_NotStakedSupplierSkipped(t *testing.T) {
	initSDKConfig()
	// Every supplier query returns NotFound (keys loaded but not staked on-chain).
	// This must NOT abort and must NOT flag the configured backend as missing —
	// nothing is staked, so nothing is unserved.
	grpcAddr := startStakeCheckServerImpl(t, &stakeCheckSupplierServer{notFound: true})
	config := stakeCheckConfig(t, grpcAddr, map[string][]string{"eth": {relayer.BackendTypeJSONRPC}})

	err := runCheckStake(context.Background(), config, "")

	require.NoError(t, err, "an unstaked supplier key is info, not a fatal error")
}

func TestRunCheckStake_TransportErrorAborts(t *testing.T) {
	initSDKConfig()
	// Node points at a dead port: the query fails with a non-NotFound transport
	// error, which must abort (we cannot certify a config we could not check).
	config := stakeCheckConfig(t, "127.0.0.1:1", map[string][]string{"eth": {relayer.BackendTypeJSONRPC}})

	err := runCheckStake(context.Background(), config, "")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "no supplier keys configured")
}

func TestIsSupplierNotFoundError(t *testing.T) {
	assert.False(t, isSupplierNotFoundError(nil))
	assert.True(t, isSupplierNotFoundError(status.Error(codes.NotFound, "supplier not found")))
	// Wrapped via fmt.Errorf(%w), as the real query client does.
	wrapped := fmt.Errorf("failed to query supplier: %w", status.Error(codes.NotFound, "x"))
	assert.True(t, isSupplierNotFoundError(wrapped))
	// Non-NotFound transport error is not "not staked".
	assert.False(t, isSupplierNotFoundError(status.Error(codes.Unavailable, "connection refused")))
}

func TestStripGRPCScheme(t *testing.T) {
	cases := map[string]string{
		"grpc://host:9090":  "host:9090",
		"grpcs://host:9090": "host:9090",
		"http://host:9090":  "host:9090",
		"https://host:9090": "host:9090",
		"host:9090":         "host:9090",
	}
	for in, want := range cases {
		assert.Equal(t, want, stripGRPCScheme(in), in)
	}
}
