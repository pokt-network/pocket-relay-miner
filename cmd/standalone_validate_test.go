package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// standalone validate reports what relayer validate reports about a simulation
// block it does not check: an identity past its not_after, and one the relayer
// would refuse once simulation is enabled. Advisory: the exit stays 0.
func TestStandaloneValidate_ReportsTheSimulationWarnings(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	example, err := os.ReadFile(filepath.Join("..", "config.standalone.example.yaml"))
	require.NoError(t, err)
	doc := strings.Replace(string(example), "\nrelayer:\n",
		"\nrelayer:\n  simulation:\n    enabled: false\n    identities:\n"+
			"      - key_id: expired-key\n        enabled: true\n        not_after: \"2020-01-01T00:00:00Z\"\n        app_pubkey_hex: \"zz\"\n", 1)
	require.NotEqual(t, string(example), doc)
	path := filepath.Join(t.TempDir(), "standalone.yaml")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	var out bytes.Buffer
	c := standaloneValidateCmd()
	c.SetArgs([]string{"--config", path})
	c.SetOut(&out)
	c.SetErr(&out)
	require.NoError(t, c.Execute(), "the warnings are advisory")

	require.Contains(t, out.String(), "past their not_after and will reject every relay: expired-key")
	require.Contains(t, out.String(), "it would be REJECTED if you set simulation.enabled: true")
}
