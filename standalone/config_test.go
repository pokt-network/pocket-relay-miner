package standalone

import (
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// The common sections, written once in a standalone file and once per side in
// the two files of the relayer and miner subcommands.
const commonYAML = `pocket_node:
  query_node_rpc_url: "https://rpc.example.com"
  query_node_grpc_url: "grpc.example.com:443"
  chain_id: "pocket-lego-testnet"
  grpc_insecure: false
keys:
  keys_file: "/keys/supplier-keys.yaml"
logging:
  level: "debug"
  format: "json"
metrics:
  enabled: true
  addr: "0.0.0.0:9090"
pprof:
  enabled: true
  addr: "127.0.0.1:6060"
`

const relayerOnlyYAML = `listen_addr: "0.0.0.0:8080"
services:
  my-service:
    validation_mode: eager
    default_backend: jsonrpc
    backends:
      jsonrpc:
        url: "http://backend.example.com:8545"
health_check:
  enabled: true
  addr: "0.0.0.0:8081"
`

const minerOnlyYAML = `block_time_seconds: 30
batch_size: 500
`

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}

const storageYAML = "storage:\n  path: \"/data/standalone\"\n"

func standaloneYAML() string {
	return commonYAML + storageYAML +
		"relayer:\n" + indent(relayerOnlyYAML+"redis:\n  batch_publish_interval_ms: 750\n") +
		"miner:\n" + indent(minerOnlyYAML+"redis:\n  claim_idle_timeout_ms: 90000\n")
}

// A standalone file gives each side exactly the config that side's own file
// gives it: the same parser runs on the same settings.
func TestParseConfig_GivesEachSideWhatItsOwnFileGives(t *testing.T) {
	got, err := ParseConfig([]byte(standaloneYAML()))
	require.NoError(t, err)

	wantRelayer, err := relayer.ParseConfigWithoutRedis([]byte(commonYAML + relayerOnlyYAML +
		"redis:\n  batch_publish_interval_ms: 750\n"))
	require.NoError(t, err)
	wantMiner, err := miner.ParseConfigWithoutRedis([]byte(commonYAML + minerOnlyYAML +
		"redis:\n  claim_idle_timeout_ms: 90000\n"))
	require.NoError(t, err)

	// Each side's own unknown-key list is discarded by design (it names lines
	// of an assembled document); unknown keys come from Warnings() below.
	// relayer.Config's other unexported field is its built pools, checked
	// through GetPool.
	ignore := cmpopts.IgnoreUnexported(relayer.Config{}, miner.Config{})
	require.Empty(t, cmp.Diff(wantRelayer, got.Relayer, ignore))
	require.Empty(t, cmp.Diff(wantMiner, got.Miner, ignore))
	require.NotNil(t, got.Relayer.GetPool("my-service", "jsonrpc"), "the relayer's backend pools are built")
	require.Equal(t, "0.0.0.0:9090", got.Metrics.Addr)
	require.True(t, got.PProf.Enabled)
	require.Equal(t, "debug", got.Logging.Level)
	require.Equal(t, "/data/standalone", got.Storage.Path)
	require.Empty(t, got.Warnings(), "the file is clean: chain_id at the top level is a miner key, not an unknown one")
}

func TestParseConfig_RefusesASettingWrittenTwice(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{
			name: "common section inside a side",
			yaml: strings.Replace(standaloneYAML(), "relayer:\n", "relayer:\n  keys:\n    keys_file: \"/other.yaml\"\n", 1),
			want: "relayer.keys is set at the top level",
		},
		{
			name: "a redis server inside a side",
			yaml: strings.Replace(standaloneYAML(), "claim_idle_timeout_ms: 90000", "claim_idle_timeout_ms: 90000\n    url: \"redis://other:6379\"", 1),
			want: "miner.redis.url: standalone connects to no Redis",
		},
		{
			name: "side key at the top level",
			yaml: "listen_addr: \"0.0.0.0:8080\"\n" + standaloneYAML(),
			want: `"listen_addr" is not a section of a standalone config`,
		},
		{
			name: "a redis section at the top level",
			yaml: "redis:\n  url: \"redis://redis:6379\"\n" + standaloneYAML(),
			want: "a standalone config has no redis section",
		},
		{
			name: "missing storage path",
			yaml: strings.Replace(standaloneYAML(), storageYAML, "", 1),
			want: "storage.path is required",
		},
		{
			name: "missing side",
			yaml: commonYAML + storageYAML + "relayer:\n" + indent(relayerOnlyYAML),
			want: "the miner: section is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// Unknown keys are found in the operator's file, at the line they are on there,
// not in the documents this package assembles for each side.
func TestParseConfig_ReportsUnknownKeysAtTheirLineInTheFile(t *testing.T) {
	doc := standaloneYAML()
	doc = strings.Replace(doc, "  batch_size: 500\n", "  batch_size: 500\n  bach_size: 1\n", 1)
	var line int
	for i, l := range strings.Split(doc, "\n") {
		if strings.Contains(l, "bach_size") {
			line = i + 1
		}
	}

	got, err := ParseConfig([]byte(doc))

	require.NoError(t, err, "an unknown key warns, it does not refuse")
	require.Len(t, got.Warnings(), 1)
	require.Contains(t, got.Warnings()[0], "bach_size")
	require.Contains(t, got.Warnings()[0], "line "+itoa(line))
}

func itoa(n int) string { return strconv.Itoa(n) }

// Where the file leaves a common section out, the process gets the defaults
// the miner subcommand would give it, not zero values: metrics on at :9092 and
// async logging.
func TestParseConfig_AnOmittedCommonSectionKeepsItsDefault(t *testing.T) {
	doc := `pocket_node:
  query_node_rpc_url: "https://rpc.example.com"
  query_node_grpc_url: "grpc.example.com:443"
  chain_id: "pocket-lego-testnet"
keys:
  keys_file: "/keys/supplier-keys.yaml"
` + storageYAML + `relayer:
` + indent(relayerOnlyYAML) + "miner:\n" + indent(minerOnlyYAML)

	got, err := ParseConfig([]byte(doc))
	require.NoError(t, err)

	defaults := miner.DefaultConfig()
	require.Equal(t, defaults.Metrics, got.Metrics)
	require.Equal(t, defaults.PProf, got.PProf)
	require.Equal(t, defaults.Logging, got.Logging)
	require.True(t, got.Metrics.Enabled, "premise: the miner default serves metrics")
	require.True(t, got.Logging.Async, "premise: the miner default logs asynchronously")
}

// The example the repository ships parses clean, with no unknown key.
func TestParseConfig_TheShippedExampleIsClean(t *testing.T) {
	got, err := LoadConfig("../config.standalone.example.yaml")
	require.NoError(t, err)
	require.Empty(t, got.Warnings())
	require.Equal(t, ":9092", got.Metrics.Addr)
	require.True(t, got.Logging.Async, "a logging section without async keeps the default")
}
