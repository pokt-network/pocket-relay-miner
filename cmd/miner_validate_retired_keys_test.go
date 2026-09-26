package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// minimalMinerYAML is the smallest miner.yaml that `miner validate` accepts.
// Every case below appends one retired key to it, so the control case proves
// that a failure is caused by that key and not by the rest of the file.
const minimalMinerYAML = "redis:\n" +
	"  url: redis://localhost:6379\n" +
	"  consumer_name: miner-1\n" +
	"pocket_node:\n" +
	"  query_node_rpc_url: http://localhost:26657\n" +
	"  query_node_grpc_url: localhost:9090\n" +
	"keys:\n" +
	"  keys_file: /path/to/keys.yaml\n" +
	"block_time_seconds: 60\n"

// runMinerValidate runs `miner validate --config <file>` on doc and returns the
// command's error, which is what cobra turns into the exit code.
func runMinerValidate(t *testing.T, doc string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "miner.yaml")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	c := minerValidateCmd()
	c.SetArgs([]string{"--config", path})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	return c.Execute()
}

// A retired miner key must fail `miner validate` with the sentence that says
// what its removal changed, not with the bare "field not found" line. It drives
// the real door rather than the retiredKeys map, so it goes red both if the
// entry is deleted (the REMOVED sentence disappears) and if the field comes
// back to miner.Config (the key stops being unknown and validate passes).
func TestMinerValidate_RetiredKeysFailWithTheirSentence(t *testing.T) {
	require.NoError(t, runMinerValidate(t, minimalMinerYAML),
		"CONTROL: the base file must validate, or every failure below is the base file's")

	cases := []struct {
		key      string
		line     string
		sentence string
	}{
		{
			key:      "smst_live_root_checkpoint_interval",
			line:     "smst_live_root_checkpoint_interval: 1\n",
			sentence: "no interval left to tune",
		},
		{
			key:      "deduplication_ttl_blocks",
			line:     "deduplication_ttl_blocks: 20\n",
			sentence: "the miner never read it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			err := runMinerValidate(t, minimalMinerYAML+tc.line)
			require.Error(t, err, "validate must fail on the retired key %q", tc.key)

			msg := err.Error()
			require.Contains(t, msg, tc.key)
			require.Contains(t, msg, "this setting was REMOVED",
				"the key must read as removed, not merely unknown")
			require.Contains(t, msg, tc.sentence,
				"the finding must carry what the removal changed for the operator")
		})
	}
}
