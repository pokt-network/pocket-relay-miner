package relayer

import (
	"errors"
	"fmt"
	"time"

	"github.com/pokt-network/pocket-relay-miner/rings"
)

// Defaults applied by SimulationConfig.ApplyDefaults when the corresponding
// field is left at its zero value.
const (
	// DefaultSimMaxConcurrent bounds the number of simulated relays that may
	// be in flight at once across all identities.
	DefaultSimMaxConcurrent = 32

	// DefaultSimFreshnessWindowSeconds bounds how old a simulated relay
	// request's timestamp may be before it is rejected as stale.
	DefaultSimFreshnessWindowSeconds = 30

	// DefaultSimIdentityMaxRPS bounds the per-identity simulated relay rate
	// when an operator does not set max_rps explicitly.
	DefaultSimIdentityMaxRPS = 5
)

// Sentinel errors returned by SimulationConfig.Validate. Each distinct
// validation failure has its own sentinel so callers (and tests) can assert
// the exact failure with errors.Is, rather than matching on error strings.
var (
	// ErrSimNoIdentities is returned when the feature is enabled but no
	// identities are pinned. Such a config boots cleanly and then rejects
	// every simulated relay as an unknown key_id, which is indistinguishable
	// at the metrics layer from a forged signature — so the empty list is
	// named here, at the config field that caused it, instead.
	ErrSimNoIdentities = errors.New("simulation: enabled but no identities are pinned")

	// ErrSimEmptyKeyID is returned when an identity has no key_id set.
	ErrSimEmptyKeyID = errors.New("simulation identity: key_id is required")

	// ErrSimDuplicateKeyID is returned when two or more identities share the
	// same key_id.
	ErrSimDuplicateKeyID = errors.New("simulation identity: duplicate key_id")

	// ErrSimBadPubKey is returned when an app_pubkey_hex or a
	// gateway_pubkeys_hex entry fails to parse as a compressed secp256k1
	// public key.
	ErrSimBadPubKey = errors.New("simulation identity: malformed pubkey hex")

	// ErrSimPlaceholderForbidden is returned when app_pubkey_hex or any
	// gateway_pubkeys_hex entry is the deterministic ring placeholder key
	// (rings.PlaceholderRingPubKey). Its private half is publicly derivable
	// from a well-known seed, so pinning it as a trusted ring member would
	// let anyone forge simulated relays for that identity. This is a
	// security control, not a usability check: it must never be relaxed.
	ErrSimPlaceholderForbidden = errors.New("simulation identity: placeholder ring key must not be pinned")

	// ErrSimEmptyGateways is returned when an identity has no
	// gateway_pubkeys_hex entries.
	ErrSimEmptyGateways = errors.New("simulation identity: gateway_pubkeys_hex must have at least one entry")

	// ErrSimInvalidMaxRPS is returned when max_rps is <= 0 after defaults
	// have been applied (only an unset/zero max_rps is defaulted; an
	// explicit non-positive value is a config error).
	ErrSimInvalidMaxRPS = errors.New("simulation identity: max_rps must be > 0")

	// ErrSimBadNotAfter is returned when not_after is set but is not a
	// valid RFC3339 timestamp.
	ErrSimBadNotAfter = errors.New("simulation identity: not_after must be RFC3339")
)

// SimIdentity is a single pinned simulated-relay identity: a synthetic
// (application, gateway-ring) pair the relayer will accept simulated relays
// for, verified against config-pinned public keys instead of an on-chain
// session. An identity is only served when Enabled is explicitly true —
// omitted/zero-value Enabled means the identity is inactive (fail-closed).
type SimIdentity struct {
	// KeyID uniquely identifies this identity within the simulation config.
	// Required, must be unique across all identities.
	KeyID string `yaml:"key_id"`

	// Enabled activates this identity. This is a plain bool (not a
	// pointer): an identity is served ONLY when enabled: true is explicitly
	// set in config. Omitting it (or leaving it false) keeps the identity
	// inactive; ApplyDefaults does NOT force this true.
	Enabled bool `yaml:"enabled"`

	// NotAfter is an optional RFC3339 timestamp after which this identity
	// is no longer valid for simulated relays. Empty means no expiry.
	NotAfter string `yaml:"not_after"`

	// MaxRPS bounds the simulated relay rate for this identity.
	// Default: 5 (applied by ApplyDefaults when unset/zero).
	MaxRPS int `yaml:"max_rps"`

	// AppPubKeyHex is the compressed secp256k1 public key (hex) of the
	// simulated application. Must not be the ring placeholder key.
	AppPubKeyHex string `yaml:"app_pubkey_hex"`

	// GatewayPubKeysHex are the compressed secp256k1 public keys (hex) of
	// the simulated gateway ring members. At least one is required; none
	// may be the ring placeholder key.
	GatewayPubKeysHex []string `yaml:"gateway_pubkeys_hex"`

	// AllowedServices restricts this identity to the listed service IDs.
	// Empty means all configured services are allowed.
	AllowedServices []string `yaml:"allowed_services"`
}

// SimulationConfig configures the relayer's simulated-relay feature: a
// pinned-pubkey, config-driven path for serving synthetic relays that bypass
// the normal on-chain session/ring lookup. Disabled by default.
type SimulationConfig struct {
	// Enabled turns the simulated-relay feature on. When false, Validate
	// skips all identity validation (identities may be left misconfigured
	// while the feature is off).
	Enabled bool `yaml:"enabled"`

	// MaxConcurrent bounds simulated relays in flight across all
	// identities. Default: 32.
	MaxConcurrent int `yaml:"max_concurrent"`

	// FreshnessWindowSeconds bounds how old a simulated relay request may
	// be before it is rejected as stale. Default: 30.
	FreshnessWindowSeconds int `yaml:"freshness_window_seconds"`

	// Identities lists the pinned simulated-relay identities.
	Identities []SimIdentity `yaml:"identities"`
}

// ApplyDefaults fills in zero-value fields with their defaults. It is
// idempotent (only touches fields still at their zero value) and safe to
// call multiple times, e.g. once in DefaultConfig() and again after YAML
// unmarshalling picks up identities the initial defaults couldn't have seen.
//
// ApplyDefaults deliberately does NOT touch per-identity Enabled: an
// identity is only served when the operator explicitly sets enabled: true
// (fail-closed).
func (c *SimulationConfig) ApplyDefaults() {
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = DefaultSimMaxConcurrent
	}
	if c.FreshnessWindowSeconds == 0 {
		c.FreshnessWindowSeconds = DefaultSimFreshnessWindowSeconds
	}
	for i := range c.Identities {
		if c.Identities[i].MaxRPS == 0 {
			c.Identities[i].MaxRPS = DefaultSimIdentityMaxRPS
		}
	}
}

// Validate checks the simulation config for correctness. It is a no-op when
// Enabled is false, so a disabled (default) simulation block never blocks
// startup regardless of what garbage it contains.
//
// Validate expects defaults to already be applied (call ApplyDefaults
// first, as Config.Validate does) — an explicit non-positive max_rps is
// still rejected, since only a zero/unset max_rps is defaulted.
//
// Each distinct failure returns a wrapped sentinel error so callers can
// assert the exact failure with errors.Is.
func (c *SimulationConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	return c.ValidateAsIfEnabled()
}

// ValidateAsIfEnabled runs the identity checks unconditionally: it answers
// "would this config be rejected if the feature were switched on?" for a
// block that is currently off.
//
// Validate deliberately skips a disabled block, which means a typo in an
// unused identity survives every deploy and only surfaces the day someone
// flips enabled: true — at which point the relayer refuses to boot and stops
// serving real, paid traffic. Callers that can afford to look ahead (config
// validation tooling, startup logging) use this to warn while the mistake is
// still cheap. It is advisory: it must never be promoted into a hard failure
// for a disabled block, since that would break the contract that a disabled
// block never blocks startup regardless of what it contains.
func (c *SimulationConfig) ValidateAsIfEnabled() error {
	// Serving with nothing pinned is always an operator mistake: the feature
	// was switched on because something was meant to be served, and every
	// simulated relay would instead be rejected as an unknown key_id. Fail
	// here rather than at the first health-check relay.
	if len(c.Identities) == 0 {
		return fmt.Errorf("simulation.identities: %w", ErrSimNoIdentities)
	}

	seenKeyIDs := make(map[string]struct{}, len(c.Identities))
	for i, id := range c.Identities {
		if id.KeyID == "" {
			return fmt.Errorf("simulation.identities[%d]: %w", i, ErrSimEmptyKeyID)
		}
		if _, dup := seenKeyIDs[id.KeyID]; dup {
			return fmt.Errorf("simulation.identities[%d] (key_id=%s): %w", i, id.KeyID, ErrSimDuplicateKeyID)
		}
		seenKeyIDs[id.KeyID] = struct{}{}

		appPubKey, err := rings.PubKeyFromHex(id.AppPubKeyHex)
		if err != nil {
			return fmt.Errorf("simulation.identities[%d] (key_id=%s): app_pubkey_hex: %w: %v", i, id.KeyID, ErrSimBadPubKey, err)
		}
		if rings.IsPlaceholderPubKey(appPubKey) {
			return fmt.Errorf("simulation.identities[%d] (key_id=%s): app_pubkey_hex: %w", i, id.KeyID, ErrSimPlaceholderForbidden)
		}

		if len(id.GatewayPubKeysHex) == 0 {
			return fmt.Errorf("simulation.identities[%d] (key_id=%s): %w", i, id.KeyID, ErrSimEmptyGateways)
		}
		for j, gwHex := range id.GatewayPubKeysHex {
			gwPubKey, err := rings.PubKeyFromHex(gwHex)
			if err != nil {
				return fmt.Errorf("simulation.identities[%d] (key_id=%s): gateway_pubkeys_hex[%d]: %w: %v", i, id.KeyID, j, ErrSimBadPubKey, err)
			}
			if rings.IsPlaceholderPubKey(gwPubKey) {
				return fmt.Errorf("simulation.identities[%d] (key_id=%s): gateway_pubkeys_hex[%d]: %w", i, id.KeyID, j, ErrSimPlaceholderForbidden)
			}
		}

		if id.MaxRPS <= 0 {
			return fmt.Errorf("simulation.identities[%d] (key_id=%s): max_rps=%d: %w", i, id.KeyID, id.MaxRPS, ErrSimInvalidMaxRPS)
		}

		if id.NotAfter != "" {
			if _, err := time.Parse(time.RFC3339, id.NotAfter); err != nil {
				return fmt.Errorf("simulation.identities[%d] (key_id=%s): not_after=%q: %w: %v", i, id.KeyID, id.NotAfter, ErrSimBadNotAfter, err)
			}
		}
	}

	return nil
}
