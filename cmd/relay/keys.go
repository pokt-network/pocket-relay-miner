package relay

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// relayKeysFile is the schema of a CLI keys file. It mirrors the format an
// operator keeps for beta/mainnet testing: lists of hex-encoded secp256k1
// private keys grouped by role. Only the first application (and optional first
// gateway) is used — the file exists so the private key never appears on the
// command line (and thus never in shell history or `ps`).
//
// Both `gateway` (singular) and `gateways` (plural) are accepted.
//
//	applications:
//	  - <hex privkey>
//	gateway:
//	  - <hex privkey>
type relayKeysFile struct {
	Applications []string `yaml:"applications"`
	Gateway      []string `yaml:"gateway"`
	Gateways     []string `yaml:"gateways"`
}

// ResolveKeys fills RelayAppPrivKey / RelayGatewayPrivKey (the in-memory hex
// fields the signer path uses) from a secure key source, so a raw private key
// never has to be passed on the command line. It supports three mutually
// exclusive sources:
//
//   - explicit hex: --app-priv-key / --gateway-priv-key (testing/localnet)
//   - keyring by name: --app-key / --gateway-key (via --keyring-backend/-dir)
//   - keys file: --keys-file (applications:[hex] + gateway:[hex])
//
// Combining more than one source is rejected. Anything left unset is filled later
// by the --localnet defaults (or fails validation), so this is a no-op when no
// key flags are given.
func ResolveKeys(logger logging.Logger) error {
	hexSource := RelayAppPrivKey != "" || RelayGatewayPrivKey != ""
	keyringSource := RelayAppKeyName != "" || RelayGatewayKeyName != ""
	fileSource := RelayKeysFile != ""

	sources := 0
	for _, set := range []bool{hexSource, keyringSource, fileSource} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return fmt.Errorf("choose a single key source: --app-priv-key/--gateway-priv-key (hex), --app-key/--gateway-key (keyring), or --keys-file")
	}

	switch {
	case keyringSource:
		if RelayAppKeyName != "" {
			appHex, err := resolveKeyringKey(logger, RelayKeyringBackend, RelayKeyringDir, RelayAppKeyName)
			if err != nil {
				return err
			}
			RelayAppPrivKey = appHex
		}
		if RelayGatewayKeyName != "" {
			gatewayHex, err := resolveKeyringKey(logger, RelayKeyringBackend, RelayKeyringDir, RelayGatewayKeyName)
			if err != nil {
				return err
			}
			RelayGatewayPrivKey = gatewayHex
		}
	case fileSource:
		appHex, gatewayHex, err := loadKeysFile(RelayKeysFile)
		if err != nil {
			return err
		}
		RelayAppPrivKey = appHex
		if gatewayHex != "" {
			RelayGatewayPrivKey = gatewayHex
		}
	}

	return nil
}

// loadKeysFile reads a CLI keys file and returns the application and (optional)
// gateway private keys as normalized hex (no 0x prefix). It requires exactly one
// application; a gateway is optional (app-only signing) but at most one. More
// than one key in a role is rejected rather than silently picking one, since the
// file carries no selector.
func loadKeysFile(path string) (appHex, gatewayHex string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("failed to read keys file: %w", err)
	}

	var kf relayKeysFile
	if err := yaml.Unmarshal(data, &kf); err != nil {
		// Do NOT wrap the raw YAML error: yaml.v3 echoes a prefix of the offending
		// scalar, so a "key as a scalar" typo (applications: <hex> instead of a
		// list item) would leak private key material to stderr — the exact exposure
		// --keys-file exists to prevent. Return a structural hint with only the
		// (safe) path.
		return "", "", fmt.Errorf("failed to parse keys file %s as YAML (expected lists under 'applications:' and 'gateway:'; check indentation)", path)
	}

	switch len(kf.Applications) {
	case 0:
		return "", "", fmt.Errorf("keys file has no application key (expected exactly one under 'applications')")
	case 1:
		// ok
	default:
		return "", "", fmt.Errorf("keys file must contain exactly one application key (found %d); no selector is supported", len(kf.Applications))
	}

	appHex, err = normalizeHexPrivKey(kf.Applications[0])
	if err != nil {
		return "", "", fmt.Errorf("invalid application key: %w", err)
	}

	// Accept either `gateway` or `gateways`; a gateway is optional.
	gateways := append(append([]string{}, kf.Gateway...), kf.Gateways...)
	switch len(gateways) {
	case 0:
		// app-only mode
	case 1:
		gatewayHex, err = normalizeHexPrivKey(gateways[0])
		if err != nil {
			return "", "", fmt.Errorf("invalid gateway key: %w", err)
		}
	default:
		return "", "", fmt.Errorf("keys file must contain at most one gateway key (found %d); no selector is supported", len(gateways))
	}

	return appHex, gatewayHex, nil
}

// privKeyToHex renders a private key as the 64-char hex the signer path expects.
// secp256k1 PrivKey.Bytes() returns the raw 32-byte key, so this round-trips a
// keyring/keys-file key back to the same hex that NewSignerFromHex would decode —
// letting resolved keys reuse the existing hex-based signer without ever printing
// the hex on the command line.
func privKeyToHex(pk cryptotypes.PrivKey) string {
	return hex.EncodeToString(pk.Bytes())
}

// resolveKeyringKey loads a named key from a Cosmos keyring and returns it as hex.
// The backend (file|os|test) must be given explicitly, mirroring the miner/relayer
// flags; the hex it returns lives only in memory.
func resolveKeyringKey(logger logging.Logger, backend, dir, name string) (string, error) {
	if backend == "" {
		return "", fmt.Errorf("--keyring-backend is required to resolve key %q from a keyring (file, os, or test)", name)
	}

	provider, err := keys.NewKeyringProvider(logger, keys.KeyringProviderConfig{
		Backend: backend,
		Dir:     dir,
	})
	if err != nil {
		return "", fmt.Errorf("failed to open keyring (backend %q): %w", backend, err)
	}
	defer func() { _ = provider.Close() }()

	privKey, _, err := provider.LoadKeyByName(name)
	if err != nil {
		return "", fmt.Errorf("failed to load key %q from keyring: %w", name, err)
	}

	return privKeyToHex(privKey), nil
}

// normalizeHexPrivKey validates a hex-encoded secp256k1 private key and returns
// it lowercased without any 0x prefix. It enforces exactly 64 hex characters (32
// bytes) so a malformed key fails here rather than deep inside signer creation.
func normalizeHexPrivKey(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")

	if len(s) != 64 {
		return "", fmt.Errorf("expected 64 hex characters (32 bytes), got %d", len(s))
	}
	for i, c := range s {
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		isUpperHex := c >= 'A' && c <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return "", fmt.Errorf("invalid hex character %q at position %d", c, i)
		}
	}

	return strings.ToLower(s), nil
}
