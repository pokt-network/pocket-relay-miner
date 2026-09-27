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

	t.Run("explicitly restating the default meter prefix is accepted", func(t *testing.T) {
		c := minimalValidConfig()
		c.Redis.Namespace = config.RedisNamespaceConfig{MeterPrefix: "meter"}
		c.RelayMeter.RemovedRedisKeyPrefix = "ha"
		require.NoError(t, c.Validate())
	})

	t.Run("diverging base prefix is a hard error", func(t *testing.T) {
		c := minimalValidConfig()
		c.RelayMeter.RemovedRedisKeyPrefix = "prod" // namespace stays "ha"
		err := c.Validate()
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "redis_key_prefix"),
			"the error must name the retired key: %v", err)
		requireSafeRemediation(t, err)
	})

	t.Run("custom meter_prefix moves the keys even with a matching base", func(t *testing.T) {
		// "ha" == base prefix, but meter keys land at ha:metering:* instead of
		// ha:meter:* — the keys DO move, so "retired equals base" is not enough.
		c := minimalValidConfig()
		c.Redis.Namespace = config.RedisNamespaceConfig{MeterPrefix: "metering"}
		c.RelayMeter.RemovedRedisKeyPrefix = "ha"
		err := c.Validate()
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "redis_key_prefix"),
			"the error must name the retired key: %v", err)
		requireSafeRemediation(t, err)
	})
}

// requireSafeRemediation pins the ADVICE in the tombstone error, not just the
// key names: the previous message said "set redis.namespace.base_prefix to
// the retired value", which — followed verbatim — relocates the relayer's
// entire keyspace including the WAL stream, orphaning mined relays from the
// miner. The safe remedy is deleting the retired line.
func requireSafeRemediation(t *testing.T, err error) {
	t.Helper()
	require.True(t, strings.Contains(err.Error(), "Remove the relay_meter.redis_key_prefix line"),
		"the error must advise removing the retired line: %v", err)
	require.True(t, strings.Contains(err.Error(), "WAL"),
		"the error must warn that repointing base_prefix moves the WAL: %v", err)
	require.False(t, strings.Contains(err.Error(), "Set redis.namespace.base_prefix to"),
		"the error must NOT advise repointing base_prefix at the retired value: %v", err)
}

// TestValidate_RemovedKeysDir pins the tombstone for the retired
// keys.keys_dir setting. The YAML decoder drops unknown fields silently, so
// without the tombstone an old config would boot WITHOUT those supplier keys:
// the relayer serves and signs nothing for them, with no diagnostic.
func TestValidate_RemovedKeysDir(t *testing.T) {
	t.Run("absent is fine", func(t *testing.T) {
		require.NoError(t, minimalValidConfig().Validate())
	})

	t.Run("any non-empty value is a hard error", func(t *testing.T) {
		c := minimalValidConfig()
		c.Keys.RemovedKeysDir = "/etc/pocket/keys"
		err := c.Validate()
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "keys_dir"),
			"the error must name the retired key: %v", err)
		// The advice must name the safe migrations, not the removed mechanism.
		require.True(t, strings.Contains(err.Error(), "keys_file"),
			"the error must point at keys_file: %v", err)
		require.True(t, strings.Contains(err.Error(), "keyring"),
			"the error must point at the keyring: %v", err)
	})
}

// TestValidate_RemovedGracePeriodExtraBlocks pins the upgrade contract for the
// retired grace_period_extra_blocks. It widened the serve window past the
// chain's grace period on the ADMISSION side only, so relays let in during
// those extra blocks were served and then judged ineligible for rewards --
// served for free. Silently dropping the key would instead narrow an
// operator's window with nothing in their config to explain the rejections
// that start appearing at the session boundary.
func TestValidate_RemovedGracePeriodExtraBlocks(t *testing.T) {
	t.Run("absent is fine", func(t *testing.T) {
		require.NoError(t, minimalValidConfig().Validate())
	})

	t.Run("explicit zero upgrades untouched", func(t *testing.T) {
		c := minimalValidConfig()
		zero := 0
		c.RemovedGracePeriodExtraBlocks = &zero
		require.NoError(t, c.Validate(), "a config that spelled out \"no extra\" changes nothing")
	})

	t.Run("the old default is a hard error naming the consequence", func(t *testing.T) {
		c := minimalValidConfig()
		two := 2
		c.RemovedGracePeriodExtraBlocks = &two
		err := c.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "grace_period_extra_blocks is no longer supported")
		require.Contains(t, strings.ToLower(err.Error()), "rejected as expired",
			"the operator must be told what changes for them, not just that a key went away")
	})
}
