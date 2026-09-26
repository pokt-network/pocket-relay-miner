//go:build test

package redis

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
)

// testRedisURL is the real Redis these tests need; NewClient pings.
func testRedisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL not set; start one with: eval \"$(scripts/gates/redis.sh up)\"")
	}
	return url
}

// TestEffectivePoolOptionsReadsTheClientNotTheConfig is the assertion that
// separates "what we asked for" from "what runs".
//
// The config asks for NOTHING -- PoolTimeoutSeconds is left at its zero value,
// which is how every deployment of this repository has run. A reading taken
// from the config would report 0, and a 0 pool timeout reads as "no limit".
// What the client actually holds is a real deadline, and every relay has been
// running against it unnamed.
func TestEffectivePoolOptionsReadsTheClientNotTheConfig(t *testing.T) {
	cfg := ClientConfig{URL: testRedisURL(t)} // nothing set: pool size and timeout both unspecified
	client, err := NewClient(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	eff, ok := client.EffectivePoolOptions()
	require.True(t, ok, "a standalone client must be readable")

	require.Zero(t, cfg.PoolTimeoutSeconds, "the fixture asks for nothing, which is the point")
	require.NotZero(t, eff.PoolTimeout,
		"the running client must hold a real deadline: reporting the config here would "+
			"publish 0, and a zero pool timeout reads as 'waits forever'")
	require.Equal(t, time.Duration(config.DefaultPoolTimeoutSeconds)*time.Second, eff.PoolTimeout,
		"and it must be the deadline THIS repository chose, not the one go-redis "+
			"substitutes when the field is left at zero")

	require.Positive(t, eff.PoolSize)
}

// TestMinIdleConnsDefaultsToAQuarterOfThePool pins the promise config/redis.go
// has always made and the code did not keep: the value used to travel through
// raw, arrive as 0, and leave go-redis holding no warm connections at all, so
// every burst after a quiet moment paid a dial.
func TestMinIdleConnsDefaultsToAQuarterOfThePool(t *testing.T) {
	client, err := NewClient(context.Background(), ClientConfig{
		URL:      testRedisURL(t),
		PoolSize: 40,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	eff, ok := client.EffectivePoolOptions()
	require.True(t, ok)
	require.Equal(t, 40, eff.PoolSize)
	require.Equal(t, 10, eff.MinIdleConns,
		"a quarter of the pool must be kept warm: this is the default config/redis.go "+
			"has always promised, and until 2026-09-12 the value arrived as 0 and go-redis "+
			"held no warm connections at all")
}

// testRedisURLBench is testRedisURL for a benchmark: testing.B has no Skip on
// the same receiver type, so the helper takes testing.TB.
func testRedisURLBench(tb testing.TB) string {
	tb.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		tb.Skip("REDIS_TEST_URL not set; start one with: eval \"$(scripts/gates/redis.sh up)\"")
	}
	return url
}
