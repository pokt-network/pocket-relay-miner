//go:build test

package relayer

import (
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/logging"
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

// simOffCurvePubKeyHex is 33 bytes of well-formed hex with a valid compressed
// prefix whose x coordinate is NOT on secp256k1. Hex and length checks accept
// it; only decoding to a curve point rejects it. This is the exact shape of
// pubkey an operator ends up with after copying a documentation placeholder
// into a real config.
const simOffCurvePubKeyHex = "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// TestSimulationConfig_Validate_OffCurveAppPubKey verifies an app_pubkey_hex
// that is well-formed but not a curve point is rejected by config validation,
// naming the offending identity and field, rather than surviving to fail
// inside ring-point precompute at verifier construction.
func TestSimulationConfig_Validate_OffCurveAppPubKey(t *testing.T) {
	id := validIdentity(t)
	id.AppPubKeyHex = simOffCurvePubKeyHex

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimBadPubKey), "got: %v", err)
	require.Contains(t, err.Error(), "app_pubkey_hex", "error must name the offending field")
	require.Contains(t, err.Error(), id.KeyID, "error must name the offending identity")
}

// TestSimulationConfig_Validate_OffCurveGatewayPubKey is the gateway-list
// counterpart: an off-curve ring member must be rejected at validation, with
// the failing list index named.
func TestSimulationConfig_Validate_OffCurveGatewayPubKey(t *testing.T) {
	id := validIdentity(t)
	id.GatewayPubKeysHex = []string{hexPubKey(t), simOffCurvePubKeyHex}

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSimBadPubKey), "got: %v", err)
	require.Contains(t, err.Error(), "gateway_pubkeys_hex[1]", "error must name the offending list index")
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

// TestExampleConfig_SimulationBlockStartsCleanly walks the shipped example
// config through the exact sequence a relayer performs at startup: load,
// validate, then build the simulation verifier. An operator who copies
// config.relayer.example.yaml verbatim must get a relayer that starts.
//
// This is a regression test for a shipped example that carried illustrative
// pubkeys which were not secp256k1 curve points: validation skipped them
// (feature disabled), verifier construction did not, and the relayer failed to
// boot on a feature nobody had turned on.
func TestExampleConfig_SimulationBlockStartsCleanly(t *testing.T) {
	const examplePath = "../config.relayer.example.yaml"

	cfg, err := LoadConfig(examplePath)
	require.NoError(t, err, "the shipped example config must load")

	require.False(t, cfg.Simulation.Enabled, "the example must ship with simulation off")
	require.Empty(t, cfg.Simulation.Identities,
		"the example must not ship identities: illustrative pubkeys are not real curve points")

	// The startup call that previously failed (cmd/cmd_relayer.go wires this
	// with the loaded config). nil redis/signer/serviceIDs are sufficient:
	// construction only parses config and precomputes ring points.
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	v, err := NewSimulationVerifier(logger, &cfg.Simulation, nil, nil, nil, nil)
	require.NoError(t, err, "the shipped example config must not fail verifier construction")
	require.NotNil(t, v)
	require.False(t, v.Enabled())
}

// uncommentSimTemplate performs, mechanically, the edit docs/simulated-relays.md
// tells an operator to perform on the example config: find the commented
// simulated-relay identity template and strip the leading "# " from every one
// of its lines. It returns the rewritten document.
//
// The template block is located by content, not by line number: it starts at
// the commented `identities:` key and runs for as long as the lines stay
// comments. Locating it by content is deliberate — a test pinned to line
// numbers stops testing the template the moment the file above it grows.
func uncommentSimTemplate(t *testing.T, raw string) string {
	t.Helper()

	// Strips one leading '#' plus at most one space, preserving indentation.
	// A line carrying two '#' therefore stays a comment: that is how optional
	// fields stay inert through the operator's single uncomment pass.
	stripOne := regexp.MustCompile(`^(\s*)# ?`)

	lines := strings.Split(raw, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "# identities:" {
			start = i
			break
		}
	}
	require.NotEqual(t, -1, start,
		"the example must carry a commented `# identities:` template; "+
			"an active `identities:` key above a commented block cannot be uncommented as documented")

	end := start
	for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "#") {
		lines[end] = stripOne.ReplaceAllString(lines[end], "$1")
		end++
	}
	require.Greater(t, end-start, 5, "the uncommented template looks truncated")

	return strings.Join(lines, "\n")
}

// TestExampleConfig_SimulationTemplateUncommentsToInertIdentity is the
// regression test for the documented "uncomment and fill it in" workflow.
//
// Two distinct ways that workflow used to betray the operator, both covered
// here:
//
//  1. An active `identities: []` line sitting above the commented template
//     made the naive uncomment produce a block sequence under a key that
//     already held a flow-empty list — unparseable YAML, reported ~20 lines
//     away from the line that caused it.
//  2. `not_after` and `allowed_services` sat at the same comment depth as the
//     required fields, so uncommenting silently pinned an expiry date and a
//     localnet-only service restriction the operator never chose. The expiry
//     in particular fails open-endedly and silently, on a date years away.
//
// The invariant: one uncomment pass over the template must yield a parseable
// config whose single identity carries ONLY the fields the operator was asked
// to fill in.
func TestExampleConfig_SimulationTemplateUncommentsToInertIdentity(t *testing.T) {
	raw, err := os.ReadFile("../config.relayer.example.yaml")
	require.NoError(t, err)

	path := writeTempConfig(t, uncommentSimTemplate(t, string(raw)))

	cfg, err := LoadConfig(path)
	require.NoError(t, err, "uncommenting the identity template must yield parseable YAML")

	require.Len(t, cfg.Simulation.Identities, 1, "the template must parse as exactly one identity")
	id := cfg.Simulation.Identities[0]

	// Fields the operator is explicitly told to set.
	require.Equal(t, "my-sim-identity", id.KeyID)
	require.True(t, id.Enabled, "the template's per-identity switch must come through as set")
	require.NotEmpty(t, id.AppPubKeyHex, "the template must carry an app_pubkey_hex placeholder to replace")
	require.Len(t, id.GatewayPubKeysHex, 1, "the template must carry one gateway pubkey placeholder to replace")

	// Fields the operator did NOT ask for. These are the regression.
	require.Empty(t, id.NotAfter,
		"uncommenting the template must not pin an expiry the operator did not choose")
	require.Empty(t, id.AllowedServices,
		"uncommenting the template must not restrict the identity to services the operator did not choose")
	require.Equal(t, DefaultSimIdentityMaxRPS, id.MaxRPS,
		"max_rps must come from the default, not from an accidentally-active template value")

	// And the placeholder keys must still be unservable: turning the feature
	// on without replacing them fails loudly, naming the field.
	cfg.Simulation.Enabled = true
	cfg.Simulation.ApplyDefaults()
	err = cfg.Simulation.Validate()
	require.Error(t, err, "placeholder pubkeys must never produce a servable identity")
	require.True(t, errors.Is(err, ErrSimBadPubKey), "got: %v", err)
}

// TestSimulationConfig_Validate_EnabledWithNoIdentities pins the fail-fast
// behaviour for a simulation block that is switched on but pins nothing.
//
// Without this, the misconfiguration is invisible at the point it is made:
// the relayer boots, logs `identities=0`, and then rejects every simulated
// relay with ErrSimUnknownKeyID under the metric label `verify_failed` — the
// same label a forged signature produces. The operator sees a health-check
// pipeline that fails uniformly, with nothing naming the empty list.
func TestSimulationConfig_Validate_EnabledWithNoIdentities(t *testing.T) {
	cfg := SimulationConfig{Enabled: true}
	cfg.ApplyDefaults()

	err := cfg.Validate()
	require.Error(t, err, "enabling simulation with no identities pinned must be rejected")
	require.True(t, errors.Is(err, ErrSimNoIdentities), "got: %v", err)
}

// A disabled block with no identities is the shipped default and must stay
// silent — the fail-fast above must not turn the default config into an error.
func TestSimulationConfig_Validate_DisabledWithNoIdentitiesIsOK(t *testing.T) {
	cfg := SimulationConfig{Enabled: false}
	cfg.ApplyDefaults()

	require.NoError(t, cfg.Validate(), "the shipped default (disabled, no identities) must validate")
}

// --- SimulationConfig.ValidateAsIfEnabled() ---

// ValidateAsIfEnabled is the look-ahead that keeps a disabled-but-broken block
// from staying silent until the day someone enables it. A typo in an unused
// identity must be reportable while the feature is still off.
func TestSimulationConfig_ValidateAsIfEnabled_ReportsDisabledBlockDefect(t *testing.T) {
	id := validIdentity(t)
	id.GatewayPubKeysHex = []string{simOffCurvePubKeyHex}

	cfg := SimulationConfig{Enabled: false, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	// Validate stays silent: a disabled block never blocks startup.
	require.NoError(t, cfg.Validate(), "a disabled block must not block startup")

	// The look-ahead names the defect anyway.
	err := cfg.ValidateAsIfEnabled()
	require.Error(t, err, "the look-ahead must report what enabling this config would do")
	require.True(t, errors.Is(err, ErrSimBadPubKey), "got: %v", err)
}

// The look-ahead must agree with Validate on a config that is fine, so it
// never produces a warning for a block that would enable cleanly.
func TestSimulationConfig_ValidateAsIfEnabled_SilentOnValidDisabledBlock(t *testing.T) {
	cfg := SimulationConfig{Enabled: false, Identities: []SimIdentity{validIdentity(t)}}
	cfg.ApplyDefaults()

	require.NoError(t, cfg.ValidateAsIfEnabled(), "a disabled block that would enable cleanly must not warn")
}

// An enabled config must produce byte-identical results from both entry
// points — if they ever diverge, one of the two callers is checking something
// the other is not.
func TestSimulationConfig_ValidateAsIfEnabled_MatchesValidateWhenEnabled(t *testing.T) {
	id := validIdentity(t)
	id.NotAfter = "not-a-timestamp"

	cfg := SimulationConfig{Enabled: true, Identities: []SimIdentity{id}}
	cfg.ApplyDefaults()

	validateErr := cfg.Validate()
	lookAheadErr := cfg.ValidateAsIfEnabled()
	require.Error(t, validateErr)
	require.Error(t, lookAheadErr)
	require.Equal(t, validateErr.Error(), lookAheadErr.Error(),
		"Validate and ValidateAsIfEnabled must run the same checks when enabled")
}

// writeTempConfig writes yamlDoc to a temp file and returns its path.
func writeTempConfig(t *testing.T, yamlDoc string) string {
	t.Helper()
	path := t.TempDir() + "/relayer.yaml"
	require.NoError(t, os.WriteFile(path, []byte(yamlDoc), 0o600))
	return path
}
