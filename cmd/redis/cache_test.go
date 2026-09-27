//go:build test

package redis

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	transportredis "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// newTestCacheClient spins up a miniredis-backed DebugRedisClient.
func newTestCacheClient(t *testing.T) (*DebugRedisClient, *miniredis.Miniredis) {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	ctx := context.Background()
	cli, err := transportredis.NewClient(ctx, transportredis.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	logger := logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "text", Async: false})
	return &DebugRedisClient{Client: cli, Logger: logger}, mr
}

// seedSuppliers creates N supplier cache entries. There is no known-set for
// suppliers: nothing in production ever wrote cache:known:suppliers, so the
// CLI enumerates via SCAN of the supplier state pattern.
func seedSuppliers(t *testing.T, mr *miniredis.Miniredis, addrs ...string) {
	t.Helper()
	for _, a := range addrs {
		require.NoError(t, mr.Set(fmt.Sprintf("ha:supplier:%s", a), "payload"))
	}
}

func TestInvalidateAll_DryRunListsWithoutDeleting(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedSuppliers(t, mr, "pokt1a", "pokt1b", "pokt1c")

	err := invalidateAll(context.Background(), client, "supplier", true /*dryRun*/, false)
	require.NoError(t, err)

	// Keys still present.
	assert.True(t, mr.Exists("ha:supplier:pokt1a"))
	assert.True(t, mr.Exists("ha:supplier:pokt1b"))
	assert.True(t, mr.Exists("ha:supplier:pokt1c"))

}

func TestInvalidateAll_RemovesEveryEntry(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedSuppliers(t, mr, "pokt1a", "pokt1b", "pokt1c")

	err := invalidateAll(context.Background(), client, "supplier", false, true /*yes*/)
	require.NoError(t, err)

	assert.False(t, mr.Exists("ha:supplier:pokt1a"))
	assert.False(t, mr.Exists("ha:supplier:pokt1b"))
	assert.False(t, mr.Exists("ha:supplier:pokt1c"))

}

func TestInvalidateAll_ZeroEntriesCleanly(t *testing.T) {
	client, _ := newTestCacheClient(t)

	err := invalidateAll(context.Background(), client, "supplier", false, true)
	require.NoError(t, err)
}

func TestInvalidateFromFile_ProcessesAllLines(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedSuppliers(t, mr, "pokt1a", "pokt1b", "pokt1c")

	dir := t.TempDir()
	path := filepath.Join(dir, "addrs.txt")
	content := "# comment line\n" +
		"pokt1a\n" +
		"\n" +
		"  pokt1b  \n" +
		"pokt1c\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	err := invalidateFromFile(context.Background(), client, "supplier", path, false, true)
	require.NoError(t, err)

	assert.False(t, mr.Exists("ha:supplier:pokt1a"))
	assert.False(t, mr.Exists("ha:supplier:pokt1b"))
	assert.False(t, mr.Exists("ha:supplier:pokt1c"))

}

func TestInvalidateFromFile_DryRunDoesNotDelete(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedSuppliers(t, mr, "pokt1a")

	dir := t.TempDir()
	path := filepath.Join(dir, "addrs.txt")
	require.NoError(t, os.WriteFile(path, []byte("pokt1a\n"), 0o600))

	err := invalidateFromFile(context.Background(), client, "supplier", path, true, true)
	require.NoError(t, err)
	assert.True(t, mr.Exists("ha:supplier:pokt1a"))
}

func TestCacheCmd_MutualExclusivity(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"key + all", []string{"--type", "supplier", "--invalidate", "--key", "pokt1a", "--all"}},
		{"key + key-file", []string{"--type", "supplier", "--invalidate", "--key", "pokt1a", "--key-file", "/tmp/whatever"}},
		{"all + key-file", []string{"--type", "supplier", "--invalidate", "--all", "--key-file", "/tmp/whatever"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CacheCmd()
			c.SetArgs(tc.args)
			c.SilenceUsage = true
			c.SilenceErrors = true
			err := c.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mutually exclusive")
		})
	}
}

func TestCacheCmd_InvalidateWithoutSelector(t *testing.T) {
	c := CacheCmd()
	c.SetArgs([]string{"--type", "supplier", "--invalidate"})
	c.SilenceUsage = true
	c.SilenceErrors = true
	err := c.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of --key")
}

func TestCacheCmd_DryRunRequiresBulkMode(t *testing.T) {
	c := CacheCmd()
	c.SetArgs([]string{"--type", "supplier", "--invalidate", "--key", "pokt1a", "--dry-run"})
	c.SilenceUsage = true
	c.SilenceErrors = true
	err := c.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--dry-run")
}

func TestCacheCmd_BulkFlagsRequireInvalidate(t *testing.T) {
	c := CacheCmd()
	c.SetArgs([]string{"--type", "supplier", "--all"})
	c.SilenceUsage = true
	c.SilenceErrors = true
	err := c.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--invalidate")
}

func TestInvalidateCache_SingleKeyPreservesBehavior(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedSuppliers(t, mr, "pokt1a")

	err := invalidateCache(context.Background(), client, "supplier", "pokt1a", true)
	require.NoError(t, err)

	assert.False(t, mr.Exists("ha:supplier:pokt1a"))
	// Silent SREM on known-set should also drop membership.
}
