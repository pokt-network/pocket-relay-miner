//go:build test

package redis

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// The CLI decides two things -- which Redis, which keyspace -- from three flags.
// That used to be three exclusive branches, and the shape produced a real
// defect: `--base-prefix midemo` with no --config announced the prefix and then
// fell into the else that reset the namespace to the default, so the flag did
// nothing. These pin the rule instead: defaults, then the config file, then each
// flag overriding its own value.
func TestResolveTarget(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	writeMinerConfig := func(t *testing.T, url, base string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "miner.yaml")
		body := "redis:\n  url: " + url + "\n  namespace:\n    base_prefix: " + base + "\n"
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}

	reset := func() {
		RedisURL, RedisConfig, RedisBasePrefix = "", "", ""
	}
	t.Cleanup(reset)

	t.Run("no flags: the defaults, and they are usable", func(t *testing.T) {
		reset()
		url, ns, err := resolveTarget(logger)
		require.NoError(t, err)
		require.Equal(t, "", url, "no URL yet; the caller defaults it")
		require.Equal(t, "ha", ns.BasePrefix)
	})

	t.Run("--base-prefix alone survives to the end", func(t *testing.T) {
		// The defect: this used to come back as "ha".
		reset()
		RedisBasePrefix = "midemo"
		_, ns, err := resolveTarget(logger)
		require.NoError(t, err)
		require.Equal(t, "midemo", ns.BasePrefix,
			"the flag must reach the client, not be reset by a later branch")
	})

	t.Run("--base-prefix is validated, because this binary deletes by pattern", func(t *testing.T) {
		reset()
		RedisBasePrefix = "*"
		_, _, err := resolveTarget(logger)
		require.Error(t, err)
		require.Contains(t, err.Error(), "single namespace segment")
	})

	t.Run("--config supplies both", func(t *testing.T) {
		reset()
		RedisConfig = writeMinerConfig(t, "redis://cfg:6379", "fromcfg")
		url, ns, err := resolveTarget(logger)
		require.NoError(t, err)
		require.Equal(t, "redis://cfg:6379", url)
		require.Equal(t, "fromcfg", ns.BasePrefix)
	})

	t.Run("--base-prefix overrides the config's namespace but keeps its URL", func(t *testing.T) {
		// Both together is the ordinary case for an operator inspecting a
		// keyspace they were just told to migrate. Dropping the URL sent them to
		// localhost, where "No keys found" reads as "my data is gone".
		reset()
		RedisConfig = writeMinerConfig(t, "redis://cfg:6379", "fromcfg")
		RedisBasePrefix = "override"
		url, ns, err := resolveTarget(logger)
		require.NoError(t, err)
		require.Equal(t, "redis://cfg:6379", url, "the config still supplies the URL")
		require.Equal(t, "override", ns.BasePrefix)
	})

	t.Run("an unreadable config is fatal alone, survivable with --base-prefix", func(t *testing.T) {
		reset()
		RedisConfig = filepath.Join(t.TempDir(), "does-not-exist.yaml")
		_, _, err := resolveTarget(logger)
		require.Error(t, err, "nothing else can supply the namespace")

		RedisBasePrefix = "rescue"
		_, ns, err := resolveTarget(logger)
		require.NoError(t, err,
			"a config this tool cannot read is the situation --base-prefix exists for")
		require.Equal(t, "rescue", ns.BasePrefix)
	})
}
