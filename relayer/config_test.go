//go:build test

package relayer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- BackendEndpointConfig UnmarshalYAML Tests ---

func TestBackendEndpointConfig_UnmarshalYAML_String(t *testing.T) {
	var cfg BackendEndpointConfig
	err := yaml.Unmarshal([]byte(`"http://node1:8545"`), &cfg)
	require.NoError(t, err)
	require.Equal(t, "http://node1:8545", cfg.URL)
	require.Empty(t, cfg.Name)
}

func TestBackendEndpointConfig_UnmarshalYAML_Object(t *testing.T) {
	var cfg BackendEndpointConfig
	err := yaml.Unmarshal([]byte("name: primary\nurl: http://node1:8545"), &cfg)
	require.NoError(t, err)
	require.Equal(t, "primary", cfg.Name)
	require.Equal(t, "http://node1:8545", cfg.URL)
}

func TestBackendEndpointConfig_UnmarshalYAML_MixedArray(t *testing.T) {
	input := `
- "http://node1:8545"
- name: backup
  url: "http://node2:8545"
`
	var cfgs []BackendEndpointConfig
	err := yaml.Unmarshal([]byte(input), &cfgs)
	require.NoError(t, err)
	require.Len(t, cfgs, 2)
	require.Equal(t, "http://node1:8545", cfgs[0].URL)
	require.Empty(t, cfgs[0].Name)
	require.Equal(t, "backup", cfgs[1].Name)
	require.Equal(t, "http://node2:8545", cfgs[1].URL)
}

func TestBackendEndpointConfig_UnmarshalYAML_EmptyString(t *testing.T) {
	var cfg BackendEndpointConfig
	err := yaml.Unmarshal([]byte(`""`), &cfg)
	require.Error(t, err)
}

func TestBackendEndpointConfig_UnmarshalYAML_WhitespaceOnly(t *testing.T) {
	var cfg BackendEndpointConfig
	err := yaml.Unmarshal([]byte(`"   "`), &cfg)
	require.Error(t, err)
}

// --- BackendConfig Validation Tests ---

func TestBackendConfig_Validate_URLOnly(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.validateServiceConfig("svc1", cfg.Services["svc1"])
	require.NoError(t, err)
}

func TestBackendConfig_Validate_URLsOnly(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - name: backup
            url: "http://node2:8545"
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.validateServiceConfig("svc1", cfg.Services["svc1"])
	require.NoError(t, err)
}

func TestBackendConfig_Validate_BothURLAndURLs(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
        urls:
          - "http://node2:8545"
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.validateServiceConfig("svc1", cfg.Services["svc1"])
	require.Error(t, err)
	require.Contains(t, err.Error(), "mutually exclusive")
}

func TestBackendConfig_Validate_NeitherURLNorURLs(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        headers:
          X-Key: "test"
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.validateServiceConfig("svc1", cfg.Services["svc1"])
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least one of")
}

func TestBackendConfig_Validate_DuplicateURLs(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - "http://node1:8545"
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.validateServiceConfig("svc1", cfg.Services["svc1"])
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate URL")
}

func TestBackendConfig_Validate_DuplicateNames(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - name: primary
            url: "http://node1:8545"
          - name: primary
            url: "http://node2:8545"
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.validateServiceConfig("svc1", cfg.Services["svc1"])
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate name")
}

func TestBackendConfig_Validate_EmptyURL(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - ""
`
	// Empty string in urls array should fail at UnmarshalYAML level
	cfg := Config{}
	err := yaml.Unmarshal([]byte(input), &cfg)
	require.Error(t, err)
}

// --- BuildPools and GetPool Tests ---

func TestConfig_BuildPools(t *testing.T) {
	t.Run("single-URL creates 1-endpoint pool", func(t *testing.T) {
		input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
`
		cfg := validConfigFromYAML(t, input)
		err := cfg.BuildPools()
		require.NoError(t, err)

		p := cfg.GetPool("svc1", "jsonrpc")
		require.NotNil(t, p)
		require.Equal(t, 1, p.Len())
	})

	t.Run("multi-URL creates multi-endpoint pool", func(t *testing.T) {
		input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - name: backup
            url: "http://node2:8545"
`
		cfg := validConfigFromYAML(t, input)
		err := cfg.BuildPools()
		require.NoError(t, err)

		p := cfg.GetPool("svc1", "jsonrpc")
		require.NotNil(t, p)
		require.Equal(t, 2, p.Len())
	})

	t.Run("multiple services and backends", func(t *testing.T) {
		input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
      websocket:
        url: "ws://node1:8546"
  svc2:
    backends:
      rest:
        urls:
          - "http://api1:3000"
          - "http://api2:3000"
`
		cfg := validConfigFromYAML(t, input)
		err := cfg.BuildPools()
		require.NoError(t, err)

		require.NotNil(t, cfg.GetPool("svc1", "jsonrpc"))
		require.NotNil(t, cfg.GetPool("svc1", "websocket"))
		require.NotNil(t, cfg.GetPool("svc2", "rest"))
		require.Nil(t, cfg.GetPool("unknown-svc", "grpc"))
	})
}

func TestConfig_GetPool(t *testing.T) {
	t.Run("returns nil for unknown service", func(t *testing.T) {
		input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
`
		cfg := validConfigFromYAML(t, input)
		require.NoError(t, cfg.BuildPools())
		require.Nil(t, cfg.GetPool("unknown", "jsonrpc"))
	})

	// The four cases below used to assert the cross-transport fallback chain
	// (default_backend -> jsonrpc -> rest -> any). That behaviour was removed:
	// a relay is served only by an exact-type backend, because a transport is a
	// wire protocol, not a preference. These now pin the strict contract — a
	// mismatched request resolves to nil, and the caller rejects it cleanly.

	t.Run("no fallback to default_backend for a concrete mismatched type", func(t *testing.T) {
		input := `
services:
  svc1:
    default_backend: rest
    backends:
      rest:
        url: "http://api:3000"
`
		cfg := validConfigFromYAML(t, input)
		require.NoError(t, cfg.BuildPools())

		// A concrete jsonrpc request must NOT be served by the rest backend just
		// because rest is the default. default_backend applies only to a
		// no-Rpc-Type request, and that is resolved by the caller before GetPool.
		require.Nil(t, cfg.GetPool("svc1", "jsonrpc"),
			"a concrete type mismatch must not fall back to default_backend")
	})

	t.Run("no fallback between HTTP-family types", func(t *testing.T) {
		input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
`
		cfg := validConfigFromYAML(t, input)
		require.NoError(t, cfg.BuildPools())

		require.Nil(t, cfg.GetPool("svc1", "rest"),
			"rest must not be served by the jsonrpc backend")
		require.NotNil(t, cfg.GetPool("svc1", "jsonrpc"),
			"the configured type still resolves")
	})

	t.Run("no fallback across incompatible transports (the bug)", func(t *testing.T) {
		input := `
services:
  svc1:
    backends:
      websocket:
        url: "ws://node:8546"
`
		cfg := validConfigFromYAML(t, input)
		require.NoError(t, cfg.BuildPools())

		// The old "any available" tier handed a gRPC request the websocket pool.
		// A gRPC relay can never be spoken over a ws:// backend.
		require.Nil(t, cfg.GetPool("svc1", "grpc"),
			"a gRPC request must not be handed a websocket backend")
		// And the reverse: a websocket request must not get an http backend.
		require.Nil(t, cfg.GetPool("svc1", "jsonrpc"))
	})
}

func TestConfig_LoadBalancingField(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
        load_balancing: round_robin
`
	cfg := validConfigFromYAML(t, input)
	backend := cfg.Services["svc1"].Backends["jsonrpc"]
	require.Equal(t, "round_robin", backend.LoadBalancing)
}

func TestConfig_CircuitBreakerFields(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
        health_check:
          enabled: true
          endpoint: "/health"
          interval_seconds: 10
          unhealthy_threshold: 5
          healthy_threshold: 2
`
	cfg := validConfigFromYAML(t, input)
	hc := cfg.Services["svc1"].Backends["jsonrpc"].HealthCheck
	require.NotNil(t, hc)
	require.True(t, hc.Enabled)
	require.Equal(t, 5, hc.UnhealthyThreshold)
	require.Equal(t, 2, hc.HealthyThreshold)
}

// --- Backward Compatibility ---

func TestBackendConfig_BackwardCompatibility(t *testing.T) {
	// Existing single-URL config format must continue to work
	input := `
services:
  develop-http:
    backends:
      jsonrpc:
        url: "http://backend:8545"
        headers:
          X-Api-Key: "key123"
        authentication:
          bearer_token: "secret"
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.validateServiceConfig("develop-http", cfg.Services["develop-http"])
	require.NoError(t, err)

	require.NoError(t, cfg.BuildPools())
	p := cfg.GetPool("develop-http", "jsonrpc")
	require.NotNil(t, p)
	require.Equal(t, 1, p.Len())

	// Headers and auth should still be accessible from BackendConfig
	backend := cfg.Services["develop-http"].Backends["jsonrpc"]
	require.Equal(t, "key123", backend.Headers["X-Api-Key"])
	require.Equal(t, "secret", backend.Authentication.BearerToken)
}

// --- URL normalization for duplicate detection ---

func TestBackendConfig_Validate_DuplicateURLs_TrailingSlash(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - "http://node1:8545/"
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.validateServiceConfig("svc1", cfg.Services["svc1"])
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate URL")
}

// --- Strategy Auto-detect and Validation Tests ---

func TestBuildPools_AutoDetect_SingleURL(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
`
	cfg := validConfigFromYAML(t, input)
	require.NoError(t, cfg.BuildPools())

	p := cfg.GetPool("svc1", "jsonrpc")
	require.NotNil(t, p)
	require.Equal(t, "first_healthy(auto)", p.StrategyLabel())
}

func TestBuildPools_AutoDetect_MultiURL(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - "http://node2:8545"
`
	cfg := validConfigFromYAML(t, input)
	require.NoError(t, cfg.BuildPools())

	p := cfg.GetPool("svc1", "jsonrpc")
	require.NotNil(t, p)
	require.Equal(t, "round_robin(auto)", p.StrategyLabel())
}

func TestBuildPools_ExplicitRoundRobin(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - "http://node2:8545"
        load_balancing: round_robin
`
	cfg := validConfigFromYAML(t, input)
	require.NoError(t, cfg.BuildPools())

	p := cfg.GetPool("svc1", "jsonrpc")
	require.NotNil(t, p)
	require.Equal(t, "round_robin(explicit)", p.StrategyLabel())
}

func TestBuildPools_ExplicitFirstHealthy(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - "http://node2:8545"
        load_balancing: first_healthy
`
	cfg := validConfigFromYAML(t, input)
	require.NoError(t, cfg.BuildPools())

	p := cfg.GetPool("svc1", "jsonrpc")
	require.NotNil(t, p)
	require.Equal(t, "first_healthy(explicit)", p.StrategyLabel())
}

func TestBuildPools_UnknownStrategy(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        url: "http://node1:8545"
        load_balancing: least_conn
`
	cfg := validConfigFromYAML(t, input)
	err := cfg.BuildPools()
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown load_balancing strategy")
	require.Contains(t, err.Error(), "least_conn")
}

func TestBuildPools_EmptyStrategyIsAutoDetect(t *testing.T) {
	input := `
services:
  svc1:
    backends:
      jsonrpc:
        urls:
          - "http://node1:8545"
          - "http://node2:8545"
        load_balancing: ""
`
	cfg := validConfigFromYAML(t, input)
	require.NoError(t, cfg.BuildPools())

	p := cfg.GetPool("svc1", "jsonrpc")
	require.NotNil(t, p)
	require.Equal(t, "round_robin(auto)", p.StrategyLabel())
}

// validConfigFromYAML is a test helper that parses YAML into a Config struct.
// It only unmarshals; it does NOT call Validate() (caller decides what to test).
func validConfigFromYAML(t *testing.T, yamlStr string) Config {
	t.Helper()
	var cfg Config
	err := yaml.Unmarshal([]byte(yamlStr), &cfg)
	require.NoError(t, err)
	return cfg
}

// TestConfig_Validate_RejectsBadLoggingLevel pins that logging validation is
// wired into the relayer's Validate: a typo'd level used to silently run the
// process at Info.
func TestConfig_Validate_RejectsBadLoggingLevel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Logging.Level = "warning"

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "logging.level")
}
