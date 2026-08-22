//go:build test

package relayer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// loadFullConfig unmarshals a complete relayer config (with all required
// top-level fields) so Validate can be exercised end to end.
func loadFullConfig(t *testing.T, y string) *Config {
	t.Helper()
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(y), &cfg))
	// These fixtures are about backend resolution, not keys. Validate requires
	// exactly one key source (keys.ValidateKeySources), so one is supplied here
	// rather than repeated in every YAML literal below.
	if cfg.Keys.KeysFile == "" && cfg.Keys.Keyring == nil {
		cfg.Keys.KeysFile = "/keys/supplier-keys.yaml"
	}
	return &cfg
}

// These tests pin the strict backend-resolution contract: a relay of transport
// type X is served ONLY by a backend of type X. There is no cross-transport
// fallback. The old behaviour silently fell an unmatched request through to
// default_backend -> jsonrpc -> rest -> any, which handed WebSocket and gRPC
// relays an http:// backend they can never speak to (an operator hit exactly
// this: a websocket backend keyed `ws:` instead of `websocket:` fell back to
// the jsonrpc http:// backend and failed with gorilla's "malformed ws or wss
// URL"). The only legitimate default is applied by the caller when the request
// carries no Rpc-Type header at all, before GetPool is ever reached.

// TestGetPool_NoCrossTransportFallback is the core regression. A service with
// only an http (jsonrpc) backend must NOT serve websocket or grpc from it.
func TestGetPool_NoCrossTransportFallback(t *testing.T) {
	input := `
services:
  eth:
    default_backend: jsonrpc
    backends:
      jsonrpc:
        url: "http://node:8545"
`
	cfg := validConfigFromYAML(t, input)
	require.NoError(t, cfg.BuildPools())

	// Exact match still works.
	require.NotNil(t, cfg.GetPool("eth", "jsonrpc"), "jsonrpc relay must get the jsonrpc pool")

	// The bug: these must be nil, not the jsonrpc pool. A websocket relay cannot
	// be served over http://, and a gRPC relay cannot be served over HTTP/1.
	require.Nil(t, cfg.GetPool("eth", "websocket"),
		"a websocket relay must NOT fall back to the http jsonrpc backend")
	require.Nil(t, cfg.GetPool("eth", "grpc"),
		"a gRPC relay must NOT fall back to the http jsonrpc backend")
	require.Nil(t, cfg.GetPool("eth", "rest"),
		"a rest relay must NOT fall back to a different backend")
}

// TestGetPool_ExactMatchAcrossTypes confirms each configured type resolves to
// its OWN pool and nothing bleeds across.
func TestGetPool_ExactMatchAcrossTypes(t *testing.T) {
	input := `
services:
  eth:
    backends:
      jsonrpc:
        url: "http://node:8545"
      websocket:
        url: "ws://node:8546"
`
	cfg := validConfigFromYAML(t, input)
	require.NoError(t, cfg.BuildPools())

	require.NotNil(t, cfg.GetPool("eth", "jsonrpc"))
	require.NotNil(t, cfg.GetPool("eth", "websocket"))
	// grpc was never configured -> nil, even though other backends exist.
	require.Nil(t, cfg.GetPool("eth", "grpc"),
		"an unconfigured transport must be nil even when the service has other backends")
}

// TestGetBackendConfig_NoCrossTransportFallback pins the same strictness for the
// sibling lookup. Its old "any available" tier returned a random backend via
// non-deterministic map iteration, which is worse than wrong.
func TestGetBackendConfig_NoCrossTransportFallback(t *testing.T) {
	input := `
services:
  eth:
    default_backend: jsonrpc
    backends:
      jsonrpc:
        url: "http://node:8545"
`
	cfg := validConfigFromYAML(t, input)
	require.NoError(t, cfg.BuildPools())

	require.NotNil(t, cfg.GetBackendConfig("eth", "jsonrpc"))
	require.Nil(t, cfg.GetBackendConfig("eth", "websocket"),
		"websocket backend config must not fall back to jsonrpc")
	require.Nil(t, cfg.GetBackendConfig("eth", "grpc"),
		"grpc backend config must not fall back to any available backend")
}

// TestValidate_RejectsUnknownBackendKey catches the misconfiguration at boot,
// before any traffic. `ws` is the exact typo an AI agent produced by reading a
// schema that placed no constraint on backend key names; the error names the
// valid keys so the fix is obvious.
func TestValidate_RejectsUnknownBackendKey(t *testing.T) {
	input := `
listen_addr: "0.0.0.0:8080"
redis:
  url: "redis://localhost:6379"
pocket_node:
  query_node_rpc_url: "http://localhost:26657"
  query_node_grpc_url: "localhost:9090"
keys:
  supplier_keys_path: "/keys"
services:
  eth:
    default_backend: jsonrpc
    backends:
      jsonrpc:
        url: "http://node:8545"
      ws:
        url: "ws://node:8546"
`
	cfg := loadFullConfig(t, input)

	err := cfg.Validate()
	require.Error(t, err, "an unknown backend key must be rejected at config load")
	require.Contains(t, err.Error(), "ws", "the error must name the offending key")
	require.Contains(t, err.Error(), "websocket", "the error must point at the valid key")
}

// TestValidate_AcceptsAllValidBackendKeys guards against the rejection being too
// strict: every real backend type must still load.
func TestValidate_AcceptsAllValidBackendKeys(t *testing.T) {
	input := `
listen_addr: "0.0.0.0:8080"
redis:
  url: "redis://localhost:6379"
pocket_node:
  query_node_rpc_url: "http://localhost:26657"
  query_node_grpc_url: "localhost:9090"
keys:
  supplier_keys_path: "/keys"
default_validation_mode: optimistic
services:
  multi:
    default_backend: jsonrpc
    backends:
      jsonrpc:
        url: "http://node:8545"
      rest:
        url: "http://node:8546"
      websocket:
        url: "ws://node:8547"
      grpc:
        url: "grpc://node:9090"
      cometbft:
        url: "http://node:26657"
`
	cfg := loadFullConfig(t, input)
	require.NoError(t, cfg.Validate(), "all five valid backend keys must pass validation")
}
