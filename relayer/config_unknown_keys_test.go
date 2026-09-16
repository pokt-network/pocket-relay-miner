//go:build test

package relayer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// minimalValidConfig returns a config that passes Validate before the case
// under test is applied.
func minimalValidConfig() *Config {
	c := DefaultConfig()
	c.ListenAddr = "0.0.0.0:8080"
	c.Redis.URL = "redis://localhost:6379"
	c.PocketNode.QueryNodeRPCUrl = "http://localhost:26657"
	c.PocketNode.QueryNodeGRPCUrl = "localhost:9090"
	// Exactly one key source is required (keys.ValidateKeySources): a relayer
	// with none rejects every relay while looking healthy, so Validate refuses
	// it rather than letting it boot.
	c.Keys.KeysFile = "/keys/supplier-keys.yaml"
	c.Services = map[string]ServiceConfig{
		"svc-test": {
			DefaultBackend: BackendTypeJSONRPC,
			Backends: map[string]BackendConfig{
				BackendTypeJSONRPC: {URL: "http://backend:8545"},
			},
		},
	}
	return &c
}

// writeConfigWithExtra renders the minimal valid config to a file and appends
// raw YAML, so the test drives the REAL path (file -> LoadConfig -> Warnings)
// rather than a struct literal. A struct literal could never have caught a typo,
// which is the case that actually cost money here.
func writeConfigWithExtra(t *testing.T, anchor, extra string) string {
	t.Helper()
	bz, err := yaml.Marshal(minimalValidConfig())
	require.NoError(t, err)

	doc := string(bz)
	if anchor == "" {
		// A top-level key the rendered config does not already carry.
		doc += extra
	} else {
		// A key nested under a block the rendered config ALREADY has. Appending
		// a second `relay_meter:` would be a duplicate mapping key, which yaml.v3
		// rejects outright -- a different failure than the one under test.
		require.Contains(t, doc, anchor, "anchor %q not present in the rendered config", anchor)
		doc = strings.Replace(doc, anchor, anchor+extra, 1)
	}

	path := filepath.Join(t.TempDir(), "relayer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	return path
}

// TestLoadConfig_RetiredKeysAreNamedWithWhatTheyChanged replaces the four
// tombstone tests this file used to carry (relay_meter.redis_key_prefix,
// keys.keys_dir, grace_period_extra_blocks, relay_meter.fail_behavior).
//
// The tombstone STRUCT FIELDS are gone: a field per retired key is config that
// configures nothing, and no number of them could cover the case that actually
// bit us -- a key that was never a field at all.
//
// What must survive the deletion is the SENTENCE. A bare "field not found" tells
// the operator a key is unknown; it does not tell them their relays at the
// session edge now get rejected instead of served for free. That knowledge was
// paid for with incidents, so each retired key is pinned here by the thing it
// changed, not merely by its name.
func TestLoadConfig_RetiredKeysAreNamedWithWhatTheyChanged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		anchor    string
		extra     string
		key       string
		mustCarry string
		why       string
	}{
		{
			name:      "grace_period_extra_blocks",
			extra:     "grace_period_extra_blocks: 2\n",
			key:       "grace_period_extra_blocks",
			mustCarry: "served for free",
			why:       "it widened the serve window past the chain's grace period, so those relays were served and never paid",
		},
		{
			name:      "relay_meter.fail_behavior",
			anchor:    "relay_meter:\n",
			extra:     "    fail_behavior: open\n",
			key:       "fail_behavior",
			mustCarry: "unbilled",
			why:       "an operator who had it set to open must expect rejections where relays used to be served unbilled",
		},
		{
			name:      "relay_meter.redis_key_prefix",
			anchor:    "relay_meter:\n",
			extra:     "    redis_key_prefix: legacy\n",
			key:       "redis_key_prefix",
			mustCarry: "base_prefix",
			why:       "the dangerous 'fix' is pointing base_prefix at the retired value, which relocates the whole keyspace including the WAL",
		},
		{
			name:      "keys.keys_dir",
			anchor:    "keys:\n",
			extra:     "    keys_dir: /etc/pocket/keys\n",
			key:       "keys_dir",
			mustCarry: "keys_file",
			why:       "the advice must name the safe migration, not just the removed mechanism",
		},
		{
			name:      "relay_meter.enabled",
			anchor:    "relay_meter:\n",
			extra:     "    enabled: false\n",
			key:       "enabled",
			mustCarry: "served for free",
			why:       "an operator who had the meter off must expect rejections where relays used to be served uncharged",
		},
		{
			name:      "relay_meter.service_factor_missing_ttl",
			anchor:    "relay_meter:\n",
			extra:     "    service_factor_missing_ttl: 5s\n",
			key:       "service_factor_missing_ttl",
			mustCarry: "refuses",
			why:       "an operator who tuned it must expect a relayer that starts before the miner to refuse relays, not price them by the protocol formula",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfigWithExtra(t, tc.anchor, tc.extra))
			require.NoError(t, err,
				"a retired key must NOT fail the load: the serving binary warns and starts, because "+
					"a rolling deploy lands a new binary beside an older ConfigMap as a matter of course")

			warnings := strings.Join(cfg.Warnings(), "\n")
			require.Contains(t, warnings, tc.key)
			require.Contains(t, warnings, "REMOVED",
				"a retired key must read as removed, not merely unknown")
			require.Contains(t, warnings, tc.mustCarry, tc.why)
		})
	}
}

// TestLoadConfig_AnUnknownKeyIsReportedButDoesNotFailTheLoad covers what no
// tombstone could: a key that was never a field.
func TestLoadConfig_AnUnknownKeyIsReportedButDoesNotFailTheLoad(t *testing.T) {
	cfg, err := LoadConfig(writeConfigWithExtra(t, "", "totally_bogus_key: 1\n"))
	require.NoError(t, err)

	warnings := cfg.Warnings()
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "totally_bogus_key")
	require.Contains(t, warnings[0], "line ",
		"the operator must be pointed at the line, or a long config is a hunt")
	require.NotContains(t, warnings[0], "REMOVED",
		"a key that was never a field is unknown, not retired: calling it removed would be a lie")
}

// TestLoadConfig_AGoodConfigWarnsAboutNothing keeps the warning worth reading.
// A false positive here trains the operator to ignore the output, which is worse
// than the silence it replaced.
func TestLoadConfig_AGoodConfigWarnsAboutNothing(t *testing.T) {
	cfg, err := LoadConfig(writeConfigWithExtra(t, "", ""))
	require.NoError(t, err)
	require.Empty(t, cfg.Warnings())
}

// TestLoadConfig_ARetiredLeafIsNotBlamedOnAnotherBlock is the hostile half of
// relay_meter.enabled. "enabled" is a leaf of many blocks, so the retired sentence
// is keyed by the type that owned it: an enabled left under a block that never had
// one is reported as unknown, and must not tell the operator the meter was removed.
func TestLoadConfig_ARetiredLeafIsNotBlamedOnAnotherBlock(t *testing.T) {
	cfg, err := LoadConfig(writeConfigWithExtra(t, "redis:\n", "    enabled: true\n"))
	require.NoError(t, err)

	warnings := strings.Join(cfg.Warnings(), "\n")
	require.Contains(t, warnings, "enabled", "control: the stray key must still be reported")
	require.NotContains(t, warnings, "REMOVED", "redis never had an enabled setting to remove")
}
