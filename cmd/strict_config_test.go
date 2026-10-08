package cmd

import (
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The refusal lists every key itself, for one key and for several.
func TestStrictConfigError_ListsEveryKey(t *testing.T) {
	for _, keys := range [][]string{
		{"line 3: field retired_key_a not found"},
		{"line 3: field retired_key_a not found", "line 9: field retired_key_b not found"},
	} {
		err := strictConfigError("this relayer", keys)
		require.Error(t, err)
		for _, k := range keys {
			require.Contains(t, err.Error(), k)
		}
		require.Contains(t, err.Error(), "this relayer")
		require.NotContains(t, err.Error(), "listed above")
	}
}

// A standalone process refused by --strict-config names the keys in the error
// it returns. Its async logger may exit before writing the warnings, so the
// error is the only place an operator is sure to read them. The keys are the
// four a stale local tilt_config.yaml carried on 2026-10-08.
func TestStandalone_StrictConfigRefusalNamesTheKeys(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	example, err := os.ReadFile(filepath.Join("..", "config.standalone.example.yaml"))
	require.NoError(t, err)
	doc := strings.Replace(string(example), "\nrelayer:\n",
		"\nrelayer:\n  relay_meter:\n    enabled: true\n    fail_behavior: open\n", 1)
	doc = strings.Replace(doc, "\nminer:\n",
		"\nminer:\n  deduplication_ttl_blocks: 10\n  smst_live_root_checkpoint_interval: 100\n", 1)
	require.NotEqual(t, string(example), doc)
	path := filepath.Join(t.TempDir(), "standalone.yaml")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	c := StandaloneCmd()
	c.SetArgs([]string{"--config", path, "--strict-config"})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	err = c.Execute()

	require.Error(t, err)
	require.Contains(t, err.Error(), "--strict-config: refusing to start, 4 key(s)")
	for _, key := range []string{"enabled", "fail_behavior", "deduplication_ttl_blocks", "smst_live_root_checkpoint_interval"} {
		require.Contains(t, err.Error(), "field "+key+" not found", "the refusal must name %s", key)
	}
}
