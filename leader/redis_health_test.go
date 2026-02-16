package leader

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

func TestParseMemoryInfo(t *testing.T) {
	tests := []struct {
		name     string
		info     string
		wantUsed int64
		wantMax  int64
		wantErr  bool
	}{
		{
			name: "typical Redis INFO MEMORY output",
			info: "# Memory\r\n" +
				"used_memory:1073741824\r\n" +
				"used_memory_human:1.00G\r\n" +
				"used_memory_rss:1200000000\r\n" +
				"maxmemory:2147483648\r\n" +
				"maxmemory_human:2.00G\r\n" +
				"maxmemory_policy:noeviction\r\n",
			wantUsed: 1073741824,
			wantMax:  2147483648,
		},
		{
			name: "maxmemory is zero (no limit)",
			info: "# Memory\r\n" +
				"used_memory:524288000\r\n" +
				"maxmemory:0\r\n",
			wantUsed: 524288000,
			wantMax:  0,
		},
		{
			name:     "empty info string",
			info:     "",
			wantUsed: 0,
			wantMax:  0,
		},
		{
			name: "only comments",
			info: "# Memory\r\n" +
				"# Some other section\r\n",
			wantUsed: 0,
			wantMax:  0,
		},
		{
			name:    "invalid used_memory value",
			info:    "used_memory:not_a_number\r\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			used, max, err := parseMemoryInfo(tt.info)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if used != tt.wantUsed {
				t.Errorf("used_memory = %d, want %d", used, tt.wantUsed)
			}
			if max != tt.wantMax {
				t.Errorf("maxmemory = %d, want %d", max, tt.wantMax)
			}
		})
	}
}

func TestRedisHealthMonitorLifecycle(t *testing.T) {
	// Start miniredis for testing
	mr := miniredis.RunT(t)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL:       "redis://" + mr.Addr(),
		Namespace: config.DefaultRedisNamespaceConfig(),
	})
	if err != nil {
		t.Fatalf("failed to create redis client: %v", err)
	}
	defer func() { _ = redisClient.Close() }()

	monitor := NewRedisHealthMonitor(logger, redisClient)

	// Start
	if err := monitor.Start(ctx); err != nil {
		t.Fatalf("failed to start monitor: %v", err)
	}

	// Let it run one cycle
	time.Sleep(100 * time.Millisecond)

	// Close
	if err := monitor.Close(); err != nil {
		t.Fatalf("failed to close monitor: %v", err)
	}

	// Double close should be safe
	if err := monitor.Close(); err != nil {
		t.Fatalf("double close should be safe: %v", err)
	}
}
