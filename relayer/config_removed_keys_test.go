//go:build test

package relayer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
)

// minimalValidConfig returns a config that passes Validate before the case
// under test is applied.
func minimalValidConfig() *Config {
	c := DefaultConfig()
	c.ListenAddr = "0.0.0.0:8080"
	c.Redis.URL = "redis://localhost:6379"
	c.PocketNode.QueryNodeRPCUrl = "http://localhost:26657"
	c.PocketNode.QueryNodeGRPCUrl = "localhost:9090"
	c.Services = map[string]ServiceConfig{
		"svc-test": {
			DefaultBackend: BackendTypeJSONRPC,
			Backends: map[string]BackendConfig{
				BackendTypeJSONRPC: {URL: "http://backend:8545"},
			},
		},
	}
	return &c
}

// TestValidate_RemovedRedisKeyPrefix pins the upgrade contract for the retired
// relay_meter.redis_key_prefix. The YAML decoder drops unknown fields
// silently, so without the tombstone an old config carrying a non-default
// prefix would upgrade into a silent meter-key migration: budgets reset
// mid-session and a rolling deploy meters one session under two namespaces.
func TestValidate_RemovedRedisKeyPrefix(t *testing.T) {
	t.Run("absent is fine", func(t *testing.T) {
		require.NoError(t, minimalValidConfig().Validate())
	})

	t.Run("matching the default namespace is accepted", func(t *testing.T) {
		// Configs shipped before the removal carried the old default "ha",
		// which equals the namespace default: nothing moves, no edit demanded.
		c := minimalValidConfig()
		c.RelayMeter.RemovedRedisKeyPrefix = "ha"
		require.NoError(t, c.Validate())
	})

	t.Run("matching a custom namespace is accepted", func(t *testing.T) {
		c := minimalValidConfig()
		c.Redis.Namespace = config.RedisNamespaceConfig{BasePrefix: "prod"}
		c.RelayMeter.RemovedRedisKeyPrefix = "prod"
		require.NoError(t, c.Validate())
	})

	t.Run("diverging from the namespace is a hard error", func(t *testing.T) {
		c := minimalValidConfig()
		c.RelayMeter.RemovedRedisKeyPrefix = "prod" // namespace stays "ha"
		err := c.Validate()
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "redis_key_prefix"),
			"the error must name the retired key: %v", err)
		require.True(t, strings.Contains(err.Error(), "base_prefix"),
			"the error must say where the value belongs now: %v", err)
	})
}
