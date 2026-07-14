package relay

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// resetKeyFlags clears every key-source package var so each ResolveKeys test
// starts from a clean slate, and restores them afterward.
func resetKeyFlags(t *testing.T) {
	t.Helper()
	save := []*string{
		&RelayAppPrivKey, &RelayGatewayPrivKey,
		&RelayKeyringBackend, &RelayKeyringDir, &RelayAppKeyName, &RelayGatewayKeyName,
		&RelayKeysFile,
	}
	orig := make([]string, len(save))
	for i, p := range save {
		orig[i] = *p
		*p = ""
	}
	t.Cleanup(func() {
		for i, p := range save {
			*p = orig[i]
		}
	})
}

func testLogger() logging.Logger {
	return logging.NewLoggerFromConfig(logging.DefaultConfig())
}

// validAppHex and validGatewayHex are real secp256k1 private keys (localnet
// app1/gateway1), used here only as well-formed 64-char hex inputs.
const (
	validAppHex     = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"
	validGatewayHex = "cf09805c952fa999e9a63a9f434147b0a5abfd10f268879694c6b5a70e1ae177"
)

// writeKeysFile writes content to a temp keys file and returns its path.
func writeKeysFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestLoadKeysFile_AppAndGateway proves the beta schema (applications:[hex] +
// gateway:[hex]) yields the single app and gateway hex, normalized (no 0x).
func TestLoadKeysFile_AppAndGateway(t *testing.T) {
	path := writeKeysFile(t, `
applications:
  - `+validAppHex+`
gateway:
  - `+validGatewayHex+`
`)

	appHex, gatewayHex, err := loadKeysFile(path)

	require.NoError(t, err)
	require.Equal(t, validAppHex, appHex)
	require.Equal(t, validGatewayHex, gatewayHex)
}

// TestLoadKeysFile_AppOnly proves a keys file with no gateway is valid (app-only
// signing mode) and returns an empty gateway hex.
func TestLoadKeysFile_AppOnly(t *testing.T) {
	path := writeKeysFile(t, "applications:\n  - "+validAppHex+"\n")

	appHex, gatewayHex, err := loadKeysFile(path)

	require.NoError(t, err)
	require.Equal(t, validAppHex, appHex)
	require.Empty(t, gatewayHex, "no gateway section means app-only mode")
}

// TestLoadKeysFile_GatewaysPluralAlias proves the plural `gateways:` key is
// accepted as an alias for `gateway:`.
func TestLoadKeysFile_GatewaysPluralAlias(t *testing.T) {
	path := writeKeysFile(t, "applications:\n  - "+validAppHex+"\ngateways:\n  - "+validGatewayHex+"\n")

	appHex, gatewayHex, err := loadKeysFile(path)

	require.NoError(t, err)
	require.Equal(t, validAppHex, appHex)
	require.Equal(t, validGatewayHex, gatewayHex)
}

// TestLoadKeysFile_StripsHexPrefix proves a 0x-prefixed key is normalized.
func TestLoadKeysFile_StripsHexPrefix(t *testing.T) {
	path := writeKeysFile(t, "applications:\n  - 0x"+validAppHex+"\n")

	appHex, _, err := loadKeysFile(path)

	require.NoError(t, err)
	require.Equal(t, validAppHex, appHex, "0x prefix must be stripped")
}

// TestLoadKeysFile_NoApplications proves an empty/absent applications list is a
// clear error, not a silent empty app key.
func TestLoadKeysFile_NoApplications(t *testing.T) {
	path := writeKeysFile(t, "gateway:\n  - "+validGatewayHex+"\n")

	_, _, err := loadKeysFile(path)

	require.Error(t, err)
	require.Contains(t, err.Error(), "application")
}

// TestLoadKeysFile_MultipleApplicationsAmbiguous proves that more than one app —
// with no selector — is rejected rather than silently picking one.
func TestLoadKeysFile_MultipleApplicationsAmbiguous(t *testing.T) {
	path := writeKeysFile(t, "applications:\n  - "+validAppHex+"\n  - "+validGatewayHex+"\n")

	_, _, err := loadKeysFile(path)

	require.Error(t, err)
	require.Contains(t, err.Error(), "one application")
}

// TestLoadKeysFile_MultipleGatewaysAmbiguous proves more than one gateway is
// rejected.
func TestLoadKeysFile_MultipleGatewaysAmbiguous(t *testing.T) {
	path := writeKeysFile(t, "applications:\n  - "+validAppHex+"\ngateway:\n  - "+validGatewayHex+"\n  - "+validAppHex+"\n")

	_, _, err := loadKeysFile(path)

	require.Error(t, err)
	require.Contains(t, err.Error(), "one gateway")
}

// TestLoadKeysFile_InvalidHex proves a malformed private key (wrong length) is
// rejected with a descriptive error.
func TestLoadKeysFile_InvalidHex(t *testing.T) {
	path := writeKeysFile(t, "applications:\n  - deadbeef\n")

	_, _, err := loadKeysFile(path)

	require.Error(t, err)
	require.Contains(t, err.Error(), "64")
}

// TestPrivKeyToHex_RoundTrips proves a secp256k1 private key converts back to the
// exact hex it was built from — the conversion used to feed keyring/keys-file
// resolved keys into the (hex-based) signer path without exposing hex on the CLI.
func TestPrivKeyToHex_RoundTrips(t *testing.T) {
	keyBytes, err := hex.DecodeString(validAppHex)
	require.NoError(t, err)
	pk := &secp256k1.PrivKey{Key: keyBytes}

	require.Equal(t, validAppHex, privKeyToHex(pk))
}

// TestResolveKeys_KeysFileSetsHex proves --keys-file populates the in-memory hex
// fields the signer path reads, so no hex ever appears on the command line.
func TestResolveKeys_KeysFileSetsHex(t *testing.T) {
	resetKeyFlags(t)
	RelayKeysFile = writeKeysFile(t, "applications:\n  - "+validAppHex+"\ngateway:\n  - "+validGatewayHex+"\n")

	require.NoError(t, ResolveKeys(testLogger()))

	require.Equal(t, validAppHex, RelayAppPrivKey)
	require.Equal(t, validGatewayHex, RelayGatewayPrivKey)
}

// TestResolveKeys_KeysFileAppOnlyLeavesGateway proves an app-only keys file does
// not clobber the gateway field (app-only signing mode).
func TestResolveKeys_KeysFileAppOnlyLeavesGateway(t *testing.T) {
	resetKeyFlags(t)
	RelayKeysFile = writeKeysFile(t, "applications:\n  - "+validAppHex+"\n")

	require.NoError(t, ResolveKeys(testLogger()))

	require.Equal(t, validAppHex, RelayAppPrivKey)
	require.Empty(t, RelayGatewayPrivKey)
}

// TestResolveKeys_NoSourceIsNoop proves ResolveKeys does nothing when no key flag
// is set, leaving the fields for the --localnet defaults (or validation) to fill.
func TestResolveKeys_NoSourceIsNoop(t *testing.T) {
	resetKeyFlags(t)

	require.NoError(t, ResolveKeys(testLogger()))

	require.Empty(t, RelayAppPrivKey)
	require.Empty(t, RelayGatewayPrivKey)
}

// TestResolveKeys_ConflictingSourcesRejected proves that mixing two key sources
// (explicit hex + keys-file) is rejected rather than silently preferring one.
func TestResolveKeys_ConflictingSourcesRejected(t *testing.T) {
	resetKeyFlags(t)
	RelayAppPrivKey = validAppHex // hex source
	RelayKeysFile = writeKeysFile(t, "applications:\n  - "+validGatewayHex+"\n")

	err := ResolveKeys(testLogger())

	require.Error(t, err)
	require.Contains(t, err.Error(), "single key source")
}

// TestResolveKeys_KeyringWithoutBackendErrors proves that asking to resolve a
// keyring key name without a backend fails with a clear message instead of
// silently doing nothing.
func TestResolveKeys_KeyringWithoutBackendErrors(t *testing.T) {
	resetKeyFlags(t)
	RelayAppKeyName = "app" // keyring source, but no --keyring-backend

	err := ResolveKeys(testLogger())

	require.Error(t, err)
	require.Contains(t, err.Error(), "keyring-backend")
}

// TestLoadKeysFile_NotFound proves a missing file surfaces a read error.
func TestLoadKeysFile_NotFound(t *testing.T) {
	_, _, err := loadKeysFile(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
}

// TestLoadKeysFile_BadYAML proves a non-YAML file surfaces a parse error.
func TestLoadKeysFile_BadYAML(t *testing.T) {
	path := writeKeysFile(t, "\tnot: [valid: yaml")
	_, _, err := loadKeysFile(path)
	require.Error(t, err)
}
