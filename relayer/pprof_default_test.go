//go:build test

package relayer

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/config"
)

// requireLoopback fails unless addr is host:port with a loopback IP literal.
// ":6060" and "0.0.0.0:6060" both listen on every interface and both fail here.
func requireLoopback(t *testing.T, addr, what string) {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	require.NoError(t, err, "%s: %q is not host:port", what, addr)
	ip := net.ParseIP(host)
	require.NotNil(t, ip, "%s: %q has no IP literal host, so it listens on every interface or depends on the resolver", what, addr)
	require.True(t, ip.IsLoopback(), "%s: %q is not loopback: pprof serves heap and goroutine dumps to whoever reaches it", what, addr)
}

// The relayer runs pprof by default, so its default address is what an operator
// who never wrote a pprof block exposes. It must be loopback.
func TestDefaultConfig_PprofBindsLoopback(t *testing.T) {
	cfg := DefaultConfig()
	require.True(t, cfg.Pprof.Enabled, "premise: pprof is on by default, which is why its address matters")
	requireLoopback(t, cfg.Pprof.Addr, "LINK pprof-default-loopback: DefaultConfig")
}

// The real path: a file with no pprof block loads the default through LoadConfig,
// not through a constructor a test called by hand.
func TestLoadConfig_WithoutAPprofBlockBindsLoopback(t *testing.T) {
	c := minimalValidConfig()
	c.Pprof = config.PprofConfig{}
	bz, err := yaml.Marshal(c)
	require.NoError(t, err)
	require.NotContains(t, string(bz), "6060", "premise: the file must not carry a pprof address")

	path := filepath.Join(t.TempDir(), "relayer.yaml")
	require.NoError(t, os.WriteFile(path, bz, 0o600))
	loaded, err := LoadConfig(path)
	require.NoError(t, err)
	require.True(t, loaded.Pprof.Enabled, "an omitted pprof block keeps the default: enabled")
	requireLoopback(t, loaded.Pprof.Addr, "LINK pprof-default-loopback: LoadConfig without a pprof block")
}

// schemaPprofAddrDefault reads properties.pprof.properties.addr.default from a
// config schema.
func schemaPprofAddrDefault(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc struct {
		Properties struct {
			Pprof struct {
				Properties struct {
					Addr struct {
						Default string `yaml:"default"`
					} `yaml:"addr"`
				} `yaml:"properties"`
			} `yaml:"pprof"`
		} `yaml:"properties"`
	}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	got := doc.Properties.Pprof.Properties.Addr.Default
	require.NotEmpty(t, got, "%s declares no default for pprof.addr: the test would compare nothing", path)
	return got
}

// The schema and the example file are what an operator reads to learn the
// default; they must say what the code does. One place changed alone is the
// drift this test exists to catch.
func TestPprofDefault_SchemaAndExampleMatchTheCode(t *testing.T) {
	want := DefaultConfig().Pprof.Addr
	require.Equal(t, want, schemaPprofAddrDefault(t, "../config.relayer.schema.yaml"),
		"LINK pprof-default-docs: config.relayer.schema.yaml advertises a pprof default the relayer does not use")
	// The miner has no DefaultConfig for pprof: its default is the observability
	// server's fallback, which is this constant.
	require.Equal(t, config.DefaultPprofAddr, schemaPprofAddrDefault(t, "../config.miner.schema.yaml"),
		"LINK pprof-default-docs: config.miner.schema.yaml advertises a pprof default the miner does not use")

	example, err := LoadConfig("../config.relayer.example.yaml")
	require.NoError(t, err)
	require.Equal(t, want, example.Pprof.Addr,
		"LINK pprof-default-docs: config.relayer.example.yaml shows a pprof addr other than the default")
}
