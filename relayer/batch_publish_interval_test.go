//go:build test

package relayer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The batch publish interval sets how often the always-on batch is written.
// Its bounds are not cosmetic: below 500ms a batch stops being a batch and
// the relayer pays the coordination without buying the round trip back, and
// above 10s the delay a served relay waits before it is written starts to
// matter against the chain's block time.
//
// 0 has to stay valid and means the default interval: every existing
// deployment's config omits the field, and several callers build a Config
// without DefaultConfig.
func TestConfigValidate_BatchPublishIntervalBounds(t *testing.T) {
	cases := []struct {
		name     string
		ms       int
		accepted bool
	}{
		{"zero means the default", 0, true},
		{"one below the floor", 499, false},
		{"the floor itself", 500, true},
		{"a value in the middle", 2000, true},
		{"the ceiling itself", 10000, true},
		{"one above the ceiling", 10001, false},
		{"negative", -1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// minimalValidRelayerConfig and not DefaultConfig: the default has
			// no key source, so Validate fails on that first and every case
			// here would pass on an error that has nothing to do with the knob.
			cfg := minimalValidRelayerConfig()
			cfg.Redis.BatchPublishIntervalMs = tc.ms

			err := cfg.Validate()
			if tc.accepted {
				require.NoError(t, err, "%d ms should be accepted", tc.ms)
				return
			}
			require.Error(t, err, "%d ms should be rejected", tc.ms)
			require.Contains(t, err.Error(), "redis.batch_publish_interval_ms",
				"the error must name the field the operator has to fix")
		})
	}
}

// TestDefaultConfig_BatchPublishIsOneSecond pins the default interval. The batch
// is always on (Jorge, 2026-09-12); only its interval is configurable.
func TestDefaultConfig_BatchPublishIsOneSecond(t *testing.T) {
	require.Equal(t, DefaultBatchPublishIntervalMs, DefaultConfig().Redis.BatchPublishIntervalMs)
	require.Equal(t, time.Second, DefaultConfig().Redis.BatchPublishInterval())
}

// TestBatchPublishInterval_ZeroAndAbsentMeanTheDefault pins that 0 cannot turn the
// batch off, from any of the three places a 0 can come from. A zero interval
// cannot reach the dispatcher either way: time.NewTicker panics on a
// non-positive duration (time/tick.go, go1.26.5).
func TestBatchPublishInterval_ZeroAndAbsentMeanTheDefault(t *testing.T) {
	require.Equal(t, time.Second, RedisConfig{}.BatchPublishInterval(),
		"a Config built without DefaultConfig must still get the default interval")
	require.Equal(t, 1500*time.Millisecond, RedisConfig{BatchPublishIntervalMs: 1500}.BatchPublishInterval())

	// Rendered from minimalValidConfig with the field zeroed, so omitempty drops
	// the key: the helper writeConfigWithExtra cannot be used here, because the
	// config it renders now carries the default and the file would hold the key.
	c := minimalValidConfig()
	c.Redis.BatchPublishIntervalMs = 0
	bz, err := yaml.Marshal(c)
	require.NoError(t, err)
	doc := string(bz)
	require.NotContains(t, doc, "batch_publish_interval_ms", "premise: the file must not carry the key")

	write := func(name, body string) string {
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}

	absent, err := LoadConfig(write("absent.yaml", doc))
	require.NoError(t, err)
	require.Equal(t, time.Second, absent.Redis.BatchPublishInterval(), "an omitted key must load the default")

	require.Contains(t, doc, "redis:\n", "premise: the rendered config has a redis block to put the key under")
	explicitDoc := strings.Replace(doc, "redis:\n", "redis:\n    batch_publish_interval_ms: 0\n", 1)
	explicit, err := LoadConfig(write("explicit.yaml", explicitDoc))
	require.NoError(t, err, "an explicit 0 must still load: it means the default, it does not switch the batch off")
	require.Equal(t, 0, explicit.Redis.BatchPublishIntervalMs, "premise: the explicit 0 was read")
	require.Equal(t, time.Second, explicit.Redis.BatchPublishInterval())
}

// TestConfig_BatchPublishIntervalYAMLKey pins the yaml key. A typo in the
// struct tag leaves the field at 0 no matter what the operator writes, which
// reads exactly like deciding not to enable it.
func TestConfig_BatchPublishIntervalYAMLKey(t *testing.T) {
	cfg := validConfigFromYAML(t, "redis:\n  batch_publish_interval_ms: 1500\n")
	require.Equal(t, 1500, cfg.Redis.BatchPublishIntervalMs)
}
