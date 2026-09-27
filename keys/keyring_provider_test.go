package keys

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// testAppHex is a real secp256k1 private key (localnet app1), used only as a
// well-formed hex input to import into a transient keyring.
const testAppHex = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"

// newInMemoryKeyring returns a transient keyring for tests.
func newInMemoryKeyring(t *testing.T) keyring.Keyring {
	t.Helper()
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	cdc := codec.NewProtoCodec(registry)
	return keyring.NewInMemory(cdc)
}

// TestKeyringProvider_LoadKeyByName proves a named key imported into the keyring
// is returned as the exact secp256k1 private key (round-trips to the same hex)
// with a non-empty operator address — this is what lets the relay CLI resolve
// --app-key/--gateway-key without ever putting hex on the command line.
func TestKeyringProvider_LoadKeyByName(t *testing.T) {
	kr := newInMemoryKeyring(t)
	require.NoError(t, kr.ImportPrivKeyHex("app", testAppHex, "secp256k1"))

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	p := NewKeyringProviderWithKeyring(logger, kr, nil)

	privKey, addr, err := p.LoadKeyByName("app")

	require.NoError(t, err)
	require.NotEmpty(t, addr, "operator address must be derived")
	require.Equal(t, testAppHex, hex.EncodeToString(privKey.Bytes()),
		"the returned private key must round-trip to the imported hex")
}

// testKeyringPassword is the passphrase for the transient on-disk file keyring
// below. cosmos-sdk enforces a minimum length, so it cannot be shortened.
const testKeyringPassword = "testpassword123"

// TestNewKeyringProvider_FileBackend_LoadsKey is the regression test for the
// file backend being constructed with a nil passphrase reader: the passphrase
// prompt dereferences that reader, so every read from a file keyring panicked
// before returning a key. It seeds a real on-disk file keyring, then reads it
// back through NewKeyringProvider with the passphrase piped in — the exact
// non-interactive flow an operator uses from a secret manager.
func TestNewKeyringProvider_FileBackend_LoadsKey(t *testing.T) {
	dir := t.TempDir()
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	cdc := codec.NewProtoCodec(registry)

	// Creating a new file keyring prompts for the passphrase twice (set +
	// confirm); opening an existing one prompts once.
	seedKR, err := keyring.New("pocket", keyring.BackendFile, dir,
		strings.NewReader(testKeyringPassword+"\n"+testKeyringPassword+"\n"), cdc)
	require.NoError(t, err)
	require.NoError(t, seedKR.ImportPrivKeyHex("app", testAppHex, "secp256k1"))

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	p, err := NewKeyringProvider(logger, KeyringProviderConfig{
		Backend:        "file",
		Dir:            dir,
		AppName:        "pocket",
		PasswordReader: strings.NewReader(testKeyringPassword + "\n"),
	})
	require.NoError(t, err)

	privKey, addr, err := p.LoadKeyByName("app")

	require.NoError(t, err)
	require.NotEmpty(t, addr, "operator address must be derived")
	require.Equal(t, testAppHex, hex.EncodeToString(privKey.Bytes()),
		"the returned private key must round-trip to the imported hex")
}

// TestNewKeyringProvider_FileBackend_WrongPassword proves a bad passphrase is a
// returned error, not a panic and not a silently empty keyring.
func TestNewKeyringProvider_FileBackend_WrongPassword(t *testing.T) {
	dir := t.TempDir()
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	cdc := codec.NewProtoCodec(registry)

	seedKR, err := keyring.New("pocket", keyring.BackendFile, dir,
		strings.NewReader(testKeyringPassword+"\n"+testKeyringPassword+"\n"), cdc)
	require.NoError(t, err)
	require.NoError(t, seedKR.ImportPrivKeyHex("app", testAppHex, "secp256k1"))

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	p, err := NewKeyringProvider(logger, KeyringProviderConfig{
		Backend:        "file",
		Dir:            dir,
		AppName:        "pocket",
		PasswordReader: strings.NewReader("wrongpassword123\n"),
	})
	require.NoError(t, err)

	_, _, err = p.LoadKeyByName("app")

	require.Error(t, err)
}

// TestKeyringProvider_LoadKeyByName_Missing proves an unknown key name is a clear
// error rather than a nil key.
func TestKeyringProvider_LoadKeyByName_Missing(t *testing.T) {
	kr := newInMemoryKeyring(t)
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	p := NewKeyringProviderWithKeyring(logger, kr, nil)

	_, _, err := p.LoadKeyByName("does-not-exist")

	require.Error(t, err)
}

// TestKeyringDirLooksLikeKeyringItself covers the "key not found" trap: cosmos-sdk
// appends keyring-file/ to the directory it is given, so an operator who passes
// the real keyring path gets an empty keyring and an error that blames the key
// name instead of the path.
func TestKeyringDirLooksLikeKeyringItself(t *testing.T) {
	tests := []struct {
		name      string
		backend   string
		dir       string
		wantWarn  bool
		suggested string
	}{
		{
			name:      "file backend pointed at the keyring itself",
			backend:   "file",
			dir:       "/home/op/bin/keyring-file",
			wantWarn:  true,
			suggested: "/home/op/bin",
		},
		{
			name:      "trailing slash still detected",
			backend:   "file",
			dir:       "/home/op/bin/keyring-file/",
			wantWarn:  true,
			suggested: "/home/op/bin",
		},
		{
			name:      "test backend has its own subdir",
			backend:   "test",
			dir:       "/home/op/.pocket/keyring-test",
			wantWarn:  true,
			suggested: "/home/op/.pocket",
		},
		{name: "correct parent directory", backend: "file", dir: "/home/op/bin"},
		{name: "empty dir uses backend default", backend: "file", dir: ""},
		{
			// The other supported backend has its own subdir, so the same mistake
			// is possible there and must be caught with the right suggestion.
			name:      "test backend, dir points at the keyring itself",
			backend:   "test",
			dir:       "/home/op/.pocket/keyring-test",
			wantWarn:  true,
			suggested: "/home/op/.pocket",
		},
		{name: "unsupported backend has no subdir to confuse", backend: "kwallet", dir: "/home/op/keyring-file"},
		{name: "unrelated directory named similarly", backend: "file", dir: "/home/op/keyring-files"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suggested, ok := keyringDirLooksLikeKeyringItself(tt.backend, tt.dir)

			require.Equal(t, tt.wantWarn, ok)
			require.Equal(t, tt.suggested, suggested)
		})
	}
}

// TestIsPermanentKeyFailure pins the classification against the REAL cosmos-sdk
// sentinels, because getting it wrong is silent in both directions: calling a
// transient failure permanent brings back the phantom removals, and calling a
// permanent one transient freezes the key set for the life of the process --
// every reload abandoned, hot reload dead, and a pulled key still signing.
func TestIsPermanentKeyFailure(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{
			// An offline pubkey, a multisig or a ledger entry: keyring.List()
			// keeps returning the record and the export fails on every call.
			name:      "record is not a Local key",
			err:       fmt.Errorf("failed to export: %w", keyring.ErrPrivKeyExtr),
			permanent: true,
		},
		{
			// With key_names set, a key the operator deleted is gone for good.
			name:      "named key no longer in the keyring",
			err:       fmt.Errorf("key not found: %w", sdkerrors.ErrKeyNotFound),
			permanent: true,
		},
		{
			// The reason the guard exists: retrying can succeed.
			name:      "keyring temporarily unreadable",
			err:       errors.New("open /keyring/keyring-file/x.info: permission denied"),
			permanent: false,
		},
		{
			name:      "decrypt failed mid-rewrite",
			err:       errors.New("ciphertext decryption failed"),
			permanent: false,
		},
		{
			// A record's algorithm is a property of the record, so this repeats
			// forever. It is here because the commit that added the sentinel
			// touched only the production file: this table is OPEN, it was green
			// before and after, and it could not catch the omission. The
			// commit's "deliberately no test" reasoning was about a DIFFERENT
			// test -- one that fabricates a keyring record of another algorithm,
			// which no default cosmos-sdk keyring will accept. This assertion
			// fabricates nothing: it feeds the classifier an error value.
			name:      "record is of the wrong algorithm",
			err:       fmt.Errorf("key app: %w", ErrNotSecp256k1Key),
			permanent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.permanent, isPermanentKeyFailure(tt.err))
		})
	}
}
