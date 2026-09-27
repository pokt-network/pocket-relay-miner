//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// The height gauge follows the block events the process receives, with no
// supplier and no session lifecycle behind it: a miner that serves nothing yet
// (or a standby) still reports where the chain is. An older event arriving late
// does not move it back.
func TestTrackBlockHeight_FollowsBlockEventsWithNoSupplier(t *testing.T) {
	currentBlockHeight.Set(0)
	events := make(chan cache.BlockEvent, 3)
	events <- cache.BlockEvent{Height: 680249}
	events <- cache.BlockEvent{Height: 680251}
	events <- cache.BlockEvent{Height: 680250}
	close(events)

	trackBlockHeight(context.Background(), events)

	require.Equal(t, float64(680251), testutil.ToFloat64(currentBlockHeight),
		"the gauge holds the highest block received, and a late older event does not rewind it")
}

// Before the first block event reaches the process, the gauge already holds the
// height read from the chain at startup.
func TestSeedChainState_SetsTheHeightGauge(t *testing.T) {
	currentBlockHeight.Set(0)
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	w := &SupplierWorker{
		redisBlockSubscriber:    cache.NewRedisBlockSubscriber(logger, nil, nil),
		redisBlockClientAdapter: cache.NewRedisBlockClientAdapter(logger, cache.NewRedisBlockSubscriber(logger, nil, nil), nil),
	}

	w.seedChainState(43, time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC))

	require.Equal(t, float64(43), testutil.ToFloat64(currentBlockHeight))
}

// The block health monitor feeds current_block_interval_seconds, so a config
// that does not mention it runs it; only an explicit false turns it off.
func TestBlockHealthMonitorEnabled_DefaultsToTrue(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"absent", "block_time_seconds: 30\n", true},
		{"section without enabled", "block_health_monitor:\n  slowness_threshold: 2\n", true},
		{"explicit true", "block_health_monitor:\n  enabled: true\n", true},
		{"explicit false", "block_health_monitor:\n  enabled: false\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			require.NoError(t, yaml.Unmarshal([]byte(tc.yaml), &cfg))
			require.Equal(t, tc.want, cfg.BlockHealthMonitorEnabled())
		})
	}
}
