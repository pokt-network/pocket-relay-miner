package miner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// balance_monitor.enabled: false must turn the monitor off. It used to start
// anyway whenever the balance threshold was above 0, and DefaultConfig's
// threshold is 1 POKT, so false was unreachable.
func TestBalanceMonitorWanted(t *testing.T) {
	const base = "redis:\n" +
		"  url: redis://localhost:6379\n" +
		"  consumer_name: miner-1\n" +
		"pocket_node:\n" +
		"  query_node_rpc_url: http://localhost:26657\n" +
		"  query_node_grpc_url: localhost:9090\n" +
		"keys:\n" +
		"  keys_file: /path/to/keys.yaml\n" +
		"block_time_seconds: 60\n"

	for _, tc := range []struct {
		name          string
		balanceBlock  string
		wantWanted    bool
		wantThreshold int64
	}{
		{
			name:          "enabled false turns it off despite the default threshold",
			balanceBlock:  "balance_monitor:\n  enabled: false\n",
			wantWanted:    false,
			wantThreshold: 1_000_000,
		},
		{
			name:          "enabled false with an explicit threshold stays off",
			balanceBlock:  "balance_monitor:\n  enabled: false\n  balance_threshold_upokt: 5000000\n",
			wantWanted:    false,
			wantThreshold: 5_000_000,
		},
		{
			name:          "no block keeps the default on",
			balanceBlock:  "",
			wantWanted:    true,
			wantThreshold: 1_000_000,
		},
		{
			name:          "a block without enabled keeps the default on",
			balanceBlock:  "balance_monitor:\n  check_interval_seconds: 60\n",
			wantWanted:    true,
			wantThreshold: 1_000_000,
		},
		{
			name:          "an empty block keeps the default on",
			balanceBlock:  "balance_monitor:\n",
			wantWanted:    true,
			wantThreshold: 1_000_000,
		},
		{
			name:          "enabled true with threshold 0 still runs",
			balanceBlock:  "balance_monitor:\n  enabled: true\n  balance_threshold_upokt: 0\n",
			wantWanted:    true,
			wantThreshold: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "miner.yaml")
			require.NoError(t, os.WriteFile(path, []byte(base+tc.balanceBlock), 0o600))

			cfg, err := LoadConfig(path)
			require.NoError(t, err)
			require.Equal(t, tc.wantThreshold, cfg.GetBalanceMonitorThreshold(), "the threshold is loaded as written")
			require.Equal(t, tc.wantWanted, balanceMonitorWanted(cfg))
		})
	}
}
