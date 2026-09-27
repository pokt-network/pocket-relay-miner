//go:build test

package relayer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigValidate_BatchMaxQueuedBounds(t *testing.T) {
	cases := []struct {
		name     string
		mib      int
		accepted bool
	}{
		{"zero means the default", 0, true},
		{"one below the floor", 63, false},
		{"the floor itself", 64, true},
		{"the ceiling itself", 8192, true},
		{"one above the ceiling", 8193, false},
		{"negative", -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalValidRelayerConfig()
			cfg.Redis.BatchMaxQueuedMiB = tc.mib
			err := cfg.Validate()
			if tc.accepted {
				require.NoError(t, err, "%d MiB should be accepted", tc.mib)
				return
			}
			require.Error(t, err, "%d MiB should be rejected", tc.mib)
			require.Contains(t, err.Error(), "redis.batch_max_queued_mib", "the error must name the field")
		})
	}
}

// TestBatchMaxQueuedBytes_ZeroMeansTheDefault pins the default and that 0 cannot
// disable the bound: a zero bound would refuse every relay, and no bound at all is
// how a slow Redis eats the relayer's memory.
func TestBatchMaxQueuedBytes_ZeroMeansTheDefault(t *testing.T) {
	require.Equal(t, DefaultBatchMaxQueuedMiB, DefaultConfig().Redis.BatchMaxQueuedMiB)
	require.Equal(t, 512<<20, RedisConfig{}.BatchMaxQueuedBytes())
	require.Equal(t, 64<<20, RedisConfig{BatchMaxQueuedMiB: 64}.BatchMaxQueuedBytes())
}
