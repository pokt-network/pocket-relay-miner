//go:build test

package relayer

import (
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/rings"
)

// --- test fixtures ---

// hexPubKey returns the compressed secp256k1 pubkey hex for a fresh random
// key pair — a well-formed, non-placeholder pubkey suitable for "valid"
// fixtures.
func hexPubKey(t *testing.T) string {
	t.Helper()
	priv := secp256k1.GenPrivKey()
	return hex.EncodeToString(priv.PubKey().Bytes())
}

// placeholderPubKeyHex returns the hex encoding of rings.PlaceholderRingPubKey
// — the forbidden padding key whose private half is publicly derivable.
func placeholderPubKeyHex(t *testing.T) string {
	t.Helper()
	return hex.EncodeToString(rings.PlaceholderRingPubKey.Bytes())
}

func validIdentity(t *testing.T) SimIdentity {
	t.Helper()
	return SimIdentity{
		KeyID:             "sim-1",
		Enabled:           true,
		MaxRPS:            5,
		AppPubKeyHex:      hexPubKey(t),
		GatewayPubKeysHex: []string{hexPubKey(t)},
	}
}

// --- SimulationConfig.Validate() ---

func TestSimulationConfig_Validate_Disabled_SkipsValidation(t *testing.T) {
	cfg := SimulationConfig{
		Enabled: false,
		Identities: []SimIdentity{
			{KeyID: "", AppPubKeyHex: "not-hex", GatewayPubKeysHex: nil},
		},
	}
	require.NoError(t, cfg.Validate(), "disabled simulation config must skip all identity validation")
}

func TestSimulationConfig_Validate_ValidConfig(t *testing.T) {
	id1 := validIdentity(t)
	id2 := validIdentity(t)
	id2.KeyID = "sim-2"

	cfg := SimulationConfig{
		Enabled:    true,
		Identities: []SimIdentity{id1, id2},
	}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.NoError(t, err)

	// Field-level checks: defaults must not have mutated explicit values.
	require.Equal(t, 32, cfg.MaxConcurrent)
	require.Equal(t, 30, cfg.FreshnessWindowSeconds)
	require.Equal(t, "sim-1", cfg.Identities[0].KeyID)
	require.Equal(t, 5, cfg.Identities[0].MaxRPS)
	require.Equal(t, "sim-2", cfg.Identities[1].KeyID)
}

func TestSimulationConfig_Validate_ValidConfig_WithNotAfterAndServices(t *testing.T) {
	id := validIdentity(t)
	id.NotAfter = "2027-01-01T00:00:00Z"
	id.AllowedServices = []string{"eth", "poly"}

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()
	require.NoError(t, cfg.Validate())
}

func TestSimulationConfig_Validate_EmptyKeyID(t *testing.T) {
	id := validIdentity(t)
	id.KeyID = ""

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimEmptyKeyID), "got: %v", err)
}

func TestSimulationConfig_Validate_DuplicateKeyID(t *testing.T) {
	id1 := validIdentity(t)
	id2 := validIdentity(t)
	id2.KeyID = id1.KeyID // duplicate on purpose

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id1, id2}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimDuplicateKeyID), "got: %v", err)
}

func TestSimulationConfig_Validate_PlaceholderInGatewayList(t *testing.T) {
	id := validIdentity(t)
	id.GatewayPubKeysHex = []string{hexPubKey(t), placeholderPubKeyHex(t)}

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimPlaceholderForbidden), "got: %v", err)
}

// This is the core security-control test: pinning the deterministic
// placeholder key as the APPLICATION pubkey would let anyone forge
// simulated relays for that identity, since the placeholder's private
// half is publicly derivable from a well-known seed.
func TestSimulationConfig_Validate_PlaceholderAsAppPubKey_Rejected(t *testing.T) {
	id := validIdentity(t)
	id.AppPubKeyHex = placeholderPubKeyHex(t)

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimPlaceholderForbidden), "got: %v", err)
}

func TestSimulationConfig_Validate_EmptyGatewayList(t *testing.T) {
	id := validIdentity(t)
	id.GatewayPubKeysHex = nil

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimEmptyGateways), "got: %v", err)
}

func TestSimulationConfig_Validate_MalformedAppPubKey(t *testing.T) {
	id := validIdentity(t)
	id.AppPubKeyHex = "not-valid-hex"

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimBadPubKey), "got: %v", err)
}

func TestSimulationConfig_Validate_MalformedAppPubKey_WrongLength(t *testing.T) {
	id := validIdentity(t)
	id.AppPubKeyHex = "aabbcc" // valid hex, wrong length

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimBadPubKey), "got: %v", err)
}

func TestSimulationConfig_Validate_MalformedGatewayPubKey(t *testing.T) {
	id := validIdentity(t)
	id.GatewayPubKeysHex = []string{"zz-not-hex"}

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimBadPubKey), "got: %v", err)
}

func TestSimulationConfig_Validate_NegativeMaxRPS(t *testing.T) {
	id := validIdentity(t)
	id.MaxRPS = -1 // explicit negative survives ApplyDefaults (only 0 is defaulted)

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimInvalidMaxRPS), "got: %v", err)
}

func TestSimulationConfig_Validate_MaxRPSZero_DefaultedBeforeValidate(t *testing.T) {
	id := validIdentity(t)
	id.MaxRPS = 0 // omitted in YAML => defaulted to 5 by ApplyDefaults

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	require.Equal(t, 5, cfg.Identities[0].MaxRPS)
	require.NoError(t, cfg.Validate())
}

func TestSimulationConfig_Validate_BadNotAfter(t *testing.T) {
	id := validIdentity(t)
	id.NotAfter = "not-a-timestamp"

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimBadNotAfter), "got: %v", err)
}

// --- SimulationConfig.ApplyDefaults() ---

func TestSimulationConfig_ApplyDefaults_ZeroValueGetsDefaults(t *testing.T) {
	cfg := SimulationConfig{
		Identities: []SimIdentity{
			{KeyID: "sim-1"},
		},
	}
	cfg.ApplyDefaults()

	require.Equal(t, 32, cfg.MaxConcurrent)
	require.Equal(t, 30, cfg.FreshnessWindowSeconds)
	require.Len(t, cfg.Identities, 1)
	require.Equal(t, 5, cfg.Identities[0].MaxRPS)
}

func TestSimulationConfig_ApplyDefaults_PreservesExplicitValues(t *testing.T) {
	cfg := SimulationConfig{
		MaxConcurrent:          64,
		FreshnessWindowSeconds: 90,
		Identities: []SimIdentity{
			{KeyID: "sim-1", MaxRPS: 100},
		},
	}
	cfg.ApplyDefaults()

	require.Equal(t, 64, cfg.MaxConcurrent, "explicit MaxConcurrent must not be overwritten")
	require.Equal(t, 90, cfg.FreshnessWindowSeconds, "explicit FreshnessWindowSeconds must not be overwritten")
	require.Equal(t, 100, cfg.Identities[0].MaxRPS, "explicit per-identity MaxRPS must not be overwritten")
}

// ApplyDefaults must NOT force per-identity Enabled to true. An omitted
// (zero-value) `enabled` field means the identity stays inactive
// (fail-closed) — the operator must explicitly opt an identity in.
func TestSimulationConfig_ApplyDefaults_DoesNotForceEnabledTrue(t *testing.T) {
	cfg := SimulationConfig{
		Identities: []SimIdentity{
			{KeyID: "sim-1"}, // Enabled omitted => zero value (false)
		},
	}
	cfg.ApplyDefaults()

	require.False(t, cfg.Identities[0].Enabled, "ApplyDefaults must not force Enabled=true; fail-closed by design")
}

func TestSimulationConfig_ApplyDefaults_MultipleIdentitiesEachDefaulted(t *testing.T) {
	cfg := SimulationConfig{
		Identities: []SimIdentity{
			{KeyID: "sim-1", MaxRPS: 0},
			{KeyID: "sim-2", MaxRPS: 20},
			{KeyID: "sim-3", MaxRPS: 0},
		},
	}
	cfg.ApplyDefaults()

	require.Equal(t, 5, cfg.Identities[0].MaxRPS)
	require.Equal(t, 20, cfg.Identities[1].MaxRPS)
	require.Equal(t, 5, cfg.Identities[2].MaxRPS)
}

// --- YAML round-trip ---

func TestSimulationConfig_UnmarshalYAML_ValidBlock(t *testing.T) {
	appHex := hexPubKey(t)
	gwHex := hexPubKey(t)
	yamlDoc := `
enabled: true
max_concurrent: 16
freshness_window_seconds: 45
identities:
  - key_id: sim-1
    enabled: true
    max_rps: 10
    app_pubkey_hex: "` + appHex + `"
    gateway_pubkeys_hex:
      - "` + gwHex + `"
    allowed_services:
      - eth
`
	var cfg SimulationConfig
	require.NoError(t, yaml.Unmarshal([]byte(yamlDoc), &cfg))

	require.True(t, cfg.Enabled)
	require.Equal(t, 16, cfg.MaxConcurrent)
	require.Equal(t, 45, cfg.FreshnessWindowSeconds)
	require.Len(t, cfg.Identities, 1)
	require.Equal(t, "sim-1", cfg.Identities[0].KeyID)
	require.True(t, cfg.Identities[0].Enabled)
	require.Equal(t, 10, cfg.Identities[0].MaxRPS)
	require.Equal(t, appHex, cfg.Identities[0].AppPubKeyHex)
	require.Equal(t, []string{gwHex}, cfg.Identities[0].GatewayPubKeysHex)
	require.Equal(t, []string{"eth"}, cfg.Identities[0].AllowedServices)

	require.NoError(t, cfg.Validate())
}

// --- Config.Validate() wiring (integration: proves the two pieces work
// together, not just that SimulationConfig.Validate() works in isolation) ---

// minimalValidRelayerConfig returns the smallest Config that satisfies every
// OTHER Config.Validate() check, so failures observed in these tests can
// only come from the simulation wiring under test.
func minimalValidRelayerConfig() *Config {
	return &Config{
		ListenAddr:            "0.0.0.0:8080",
		Redis:                 RedisConfig{URL: "redis://localhost:6379"},
		PocketNode:            PocketNodeConfig{QueryNodeRPCUrl: "http://x", QueryNodeGRPCUrl: "x:9090"},
		DefaultValidationMode: ValidationModeOptimistic,
		Services: map[string]ServiceConfig{
			"op": {
				DefaultBackend: "jsonrpc",
				Backends: map[string]BackendConfig{
					"jsonrpc": {URL: "http://backend:8545"},
				},
			},
		},
	}
}

// TestConfigValidate_IncludesSimulationValidation proves Config.Validate()
// actually calls into Simulation.Validate() — a missing hook here would let
// an insecure simulation config (e.g. a pinned placeholder key) reach
// production, defeating the whole point of the security control.
func TestConfigValidate_IncludesSimulationValidation(t *testing.T) {
	c := minimalValidRelayerConfig()
	id := validIdentity(t)
	id.AppPubKeyHex = placeholderPubKeyHex(t)
	c.Simulation = SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}

	err := c.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimPlaceholderForbidden), "got: %v", err)
}

// TestConfigValidate_SimulationDisabledByDefault proves that a Config with
// no simulation block at all (the common case) validates successfully —
// the feature must be opt-in and never block unrelated startups.
func TestConfigValidate_SimulationDisabledByDefault(t *testing.T) {
	c := minimalValidRelayerConfig()
	require.NoError(t, c.Validate())
	require.False(t, c.Simulation.Enabled)
}

// TestConfigValidate_AppliesSimulationDefaultsEndToEnd proves Config.Validate()
// re-applies SimulationConfig defaults after identities are populated (as
// they would be by YAML unmarshalling in LoadConfig), so an operator who
// omits max_rps still gets a valid, correctly-defaulted config instead of a
// spurious ErrSimInvalidMaxRPS.
func TestConfigValidate_AppliesSimulationDefaultsEndToEnd(t *testing.T) {
	c := minimalValidRelayerConfig()
	id := validIdentity(t)
	id.MaxRPS = 0 // omitted — must be defaulted to 5 by Config.Validate()
	c.Simulation = SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}

	err := c.Validate()
	require.NoError(t, err)
	require.Equal(t, 5, c.Simulation.Identities[0].MaxRPS)
	require.Equal(t, 32, c.Simulation.MaxConcurrent)
	require.Equal(t, 30, c.Simulation.FreshnessWindowSeconds)
}

// TestDefaultConfig_SimulationTopLevelDefaultsApplied proves DefaultConfig()
// itself calls SimulationConfig.ApplyDefaults(), matching the brief's
// instruction to default alongside DefaultValidationMode.
func TestDefaultConfig_SimulationTopLevelDefaultsApplied(t *testing.T) {
	cfg := DefaultConfig()
	require.False(t, cfg.Simulation.Enabled, "simulation must be disabled by default")
	require.Equal(t, 32, cfg.Simulation.MaxConcurrent)
	require.Equal(t, 30, cfg.Simulation.FreshnessWindowSeconds)
}

// TestLoadConfig_SimulationBlockEndToEnd proves the full YAML -> LoadConfig
// -> Validate pipeline wires the simulation block correctly: a valid
// identity loads and passes, defaults apply, and a placeholder-pinned
// identity is rejected at load time (fail-closed at startup, not at
// first-relay time).
func TestLoadConfig_SimulationBlockEndToEnd(t *testing.T) {
	appHex := hexPubKey(t)
	gwHex := hexPubKey(t)

	base := `
listen_addr: "0.0.0.0:8080"
redis:
  url: "redis://localhost:6379"
pocket_node:
  query_node_rpc_url: "http://x"
  query_node_grpc_url: "x:9090"
default_validation_mode: optimistic
services:
  op:
    default_backend: jsonrpc
    backends:
      jsonrpc:
        url: "http://backend:8545"
`

	t.Run("valid identity loads and passes", func(t *testing.T) {
		yamlDoc := base + `
simulation:
  enabled: true
  identities:
    - key_id: sim-1
      enabled: true
      app_pubkey_hex: "` + appHex + `"
      gateway_pubkeys_hex:
        - "` + gwHex + `"
`
		path := writeTempConfig(t, yamlDoc)
		cfg, err := LoadConfig(path)
		require.NoError(t, err)
		require.True(t, cfg.Simulation.Enabled)
		require.Equal(t, 5, cfg.Simulation.Identities[0].MaxRPS, "max_rps must be defaulted end-to-end through LoadConfig")
	})

	t.Run("placeholder-pinned identity is rejected at load time", func(t *testing.T) {
		yamlDoc := base + `
simulation:
  enabled: true
  identities:
    - key_id: sim-1
      enabled: true
      app_pubkey_hex: "` + placeholderPubKeyHex(t) + `"
      gateway_pubkeys_hex:
        - "` + gwHex + `"
`
		path := writeTempConfig(t, yamlDoc)
		_, err := LoadConfig(path)
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrSimPlaceholderForbidden), "got: %v", err)
	})
}

// writeTempConfig writes yamlDoc to a temp file and returns its path.
func writeTempConfig(t *testing.T, yamlDoc string) string {
	t.Helper()
	path := t.TempDir() + "/relayer.yaml"
	require.NoError(t, os.WriteFile(path, []byte(yamlDoc), 0o600))
	return path
}
