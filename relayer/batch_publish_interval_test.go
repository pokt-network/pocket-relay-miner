//go:build test

package relayer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The batch publish interval is the knob that turns batched relay publishing
// on. Its bounds are not cosmetic: below 500ms a batch stops being a batch and
// the relayer pays the coordination without buying the round trip back, and
// above 10s the delay a served relay waits before it is written starts to
// matter against the chain's block time.
//
// 0 is the disabled value and has to stay valid, because it is the default and
// every existing deployment's config omits the field entirely.
func TestConfigValidate_BatchPublishIntervalBounds(t *testing.T) {
	cases := []struct {
		name     string
		ms       int
		accepted bool
	}{
		{"zero disables and is the default", 0, true},
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

// TestDefaultConfig_BatchPublishIsOff pins that the default keeps today's
// publisher. Turning batching on changes what the four publish counters mean
// while it is on, so it is an operator decision and must never arrive with a
// deploy.
func TestDefaultConfig_BatchPublishIsOff(t *testing.T) {
	require.Equal(t, 0, DefaultConfig().Redis.BatchPublishIntervalMs)
}

// TestConfig_BatchPublishIntervalYAMLKey pins the yaml key. A typo in the
// struct tag leaves the field at 0 no matter what the operator writes, which
// reads exactly like deciding not to enable it.
func TestConfig_BatchPublishIntervalYAMLKey(t *testing.T) {
	cfg := validConfigFromYAML(t, "redis:\n  batch_publish_interval_ms: 1500\n")
	require.Equal(t, 1500, cfg.Redis.BatchPublishIntervalMs)
}
