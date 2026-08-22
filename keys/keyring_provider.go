package keys

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

var _ KeyProvider = (*KeyringProvider)(nil)

// KeyringProvider loads keys from a Cosmos SDK keyring.
type KeyringProvider struct {
	logger  logging.Logger
	keyring keyring.Keyring
	appName string

	// Optional: list of specific key names to load.
	// If empty, loads all keys from keyring.
	keyNames []string
}

// KeyringProviderConfig contains configuration for the KeyringProvider.
type KeyringProviderConfig struct {
	// Backend is the keyring backend type: "file" or "test".
	// See keys.ValidateKeyringBackend for why the other cosmos-sdk backends
	// ("memory", "os", "kwallet", "pass") are not supported.
	// A caller that wants an in-memory keyring builds it itself and uses
	// NewKeyringProviderWithKeyring, which is what the tests do.
	Backend string

	// Dir is the directory containing the keyring (for "file" backend).
	Dir string

	// AppName is the application name for the keyring.
	AppName string

	// KeyNames is an optional list of specific key names to load.
	// If empty, loads all keys from the keyring.
	KeyNames []string

	// PasswordReader is where password-protected backends ("file", and "os"
	// when it falls back to an encrypted file) read the passphrase from.
	// Defaults to os.Stdin, so a caller that must stay non-interactive pipes
	// the password in (e.g. from a secret manager). Tests inject a reader.
	PasswordReader io.Reader
}

// getKeyringCodec returns a codec for keyring operations.
func getKeyringCodec() codec.Codec {
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	return codec.NewProtoCodec(registry)
}

// NewKeyringProvider creates a new provider that reads from Cosmos keyring.
func NewKeyringProvider(
	logger logging.Logger,
	config KeyringProviderConfig,
) (*KeyringProvider, error) {
	if config.AppName == "" {
		config.AppName = "pocket"
	}

	cdc := getKeyringCodec()

	// Password-protected backends dereference this reader inside cosmos-sdk's
	// newRealPrompt; a nil one panics before doing any work.
	passwordReader := config.PasswordReader
	if passwordReader == nil {
		passwordReader = os.Stdin
	}

	warnIfKeyringDirIsTheKeyringItself(logger, config.Backend, config.Dir)

	// Create keyring based on backend type
	var kr keyring.Keyring
	var err error

	switch config.Backend {
	case "test":
		// Test backend stores to disk but doesn't require password
		kr, err = keyring.New(
			config.AppName,
			keyring.BackendTest,
			config.Dir,
			nil, // No stdin for non-interactive
			cdc,
		)
	case "file":
		// A "file" keyring with nothing configured to feed it reads the
		// passphrase from stdin. That is right for a human at a terminal or a
		// pipe -- echo "$SECRET" | pocket-relay-miner ... -- and wrong for a
		// container, whose stdin is /dev/null: cosmos-sdk gets EOF, retries
		// three times and the process dies at startup. Saying so here costs one
		// line and turns that into an expected outcome rather than a mystery.
		if config.PasswordReader == nil {
			logger.Warn().
				Str("keyring_dir", config.Dir).
				Msg("no keyring passphrase source configured: it will be read from stdin. " +
					"Set keys.keyring.passphrase_file (a mounted secret) or passphrase_env " +
					"for anything that runs without a terminal -- a container's stdin is /dev/null")
		}

		// The file backend is password-protected, so it ALWAYS needs a reader to
		// prompt from. Unlike "test", which uses a fixed password, passing nil
		// here makes the backend unusable in every case: the passphrase prompt
		// dereferences the reader and panics before any key is read.
		logger.Info().Msg("keyring backend \"file\" is password-protected; the passphrase is read from the configured reader (stdin by default)")
		kr, err = keyring.New(
			config.AppName,
			keyring.BackendFile,
			config.Dir,
			passwordReader,
			cdc,
		)
	default:
		return nil, fmt.Errorf("unsupported keyring backend: %s", config.Backend)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	return &KeyringProvider{
		logger:   logging.ForComponent(logger, logging.ComponentKeyRingProvider),
		keyring:  kr,
		appName:  config.AppName,
		keyNames: config.KeyNames,
	}, nil
}

// keyringSubdirs maps a backend to the subdirectory cosmos-sdk appends to the
// configured directory. This is the whole reason the directory is the PARENT of
// the keyring: pointing at the keyring itself yields dir/keyring-file/keyring-file,
// which is empty, and every lookup then fails as "key not found" -- a message
// that reads like a wrong key name rather than a wrong path.
var keyringSubdirs = map[string]string{
	"file": "keyring-file",
	"test": "keyring-test",
}

// keyringDirLooksLikeKeyringItself reports whether dir points at the keyring
// directory instead of its parent, along with the parent to suggest.
func keyringDirLooksLikeKeyringItself(backend, dir string) (suggested string, ok bool) {
	subdir, known := keyringSubdirs[backend]
	if !known || dir == "" {
		return "", false
	}
	clean := filepath.Clean(dir)
	if filepath.Base(clean) != subdir {
		return "", false
	}
	return filepath.Dir(clean), true
}

// warnIfKeyringDirIsTheKeyringItself flags the mistake above at open time, while
// the path is still in front of the operator.
func warnIfKeyringDirIsTheKeyringItself(logger logging.Logger, backend, dir string) {
	suggested, ok := keyringDirLooksLikeKeyringItself(backend, dir)
	if !ok {
		return
	}
	logger.Warn().
		Str("keyring_dir", dir).
		Str("backend", backend).
		Str("suggested_keyring_dir", suggested).
		Msgf("keyring directory points at the keyring itself: it must be the PARENT "+
			"directory, since the %q backend looks for %s/ inside it. Keys will be "+
			"reported as \"key not found\"; pass %q instead",
			backend, keyringSubdirs[backend], suggested)
}

// NewKeyringProviderWithKeyring creates a provider with an existing keyring.
func NewKeyringProviderWithKeyring(
	logger logging.Logger,
	kr keyring.Keyring,
	keyNames []string,
) *KeyringProvider {
	return &KeyringProvider{
		logger:   logging.ForComponent(logger, logging.ComponentKeyRingProvider),
		keyring:  kr,
		keyNames: keyNames,
	}
}

// Name returns a human-readable name for this provider.
func (p *KeyringProvider) Name() string {
	return "keyring"
}

// LoadKeys loads all keys from the keyring.
func (p *KeyringProvider) LoadKeys(ctx context.Context) (map[string]cryptotypes.PrivKey, error) {
	keys := make(map[string]cryptotypes.PrivKey)

	// If specific key names are provided, load only those
	if len(p.keyNames) > 0 {
		for _, name := range p.keyNames {
			privKey, addr, err := p.loadKeyByName(name)
			if err != nil {
				p.logger.Warn().
					Err(err).
					Str("key_name", name).
					Msg("failed to load key from keyring")
				keyLoadErrors.WithLabelValues("keyring").Inc()
				continue
			}
			keys[addr] = privKey
			p.logger.Debug().
				Str("key_name", name).
				Str("operator", addr).
				Msg("loaded key from keyring")
		}
	} else {
		// Load all keys from keyring
		records, err := p.keyring.List()
		if err != nil {
			return nil, fmt.Errorf("failed to list keyring keys: %w", err)
		}

		for _, record := range records {
			privKey, addr, err := p.loadKeyByName(record.Name)
			if err != nil {
				p.logger.Warn().
					Err(err).
					Str("key_name", record.Name).
					Msg("failed to load key from keyring")
				keyLoadErrors.WithLabelValues("keyring").Inc()
				continue
			}
			keys[addr] = privKey
			p.logger.Debug().
				Str("key_name", record.Name).
				Str("operator", addr).
				Msg("loaded key from keyring")
		}
	}

	p.logger.Info().
		Int("loaded", len(keys)).
		Msg("loaded keys from keyring")

	return keys, nil
}

// LoadKeyByName loads a single key from the keyring by its name and returns the
// private key and its operator address. It is the by-name counterpart to
// LoadKeys, used by tools (e.g. the relay CLI) that resolve one specific key
// rather than the whole keyring.
func (p *KeyringProvider) LoadKeyByName(name string) (cryptotypes.PrivKey, string, error) {
	return p.loadKeyByName(name)
}

// loadKeyByName loads a single key by name and returns the private key and address.
func (p *KeyringProvider) loadKeyByName(name string) (cryptotypes.PrivKey, string, error) {
	// Get the key record
	record, err := p.keyring.Key(name)
	if err != nil {
		return nil, "", fmt.Errorf("key not found: %w", err)
	}

	// Get the address
	addr, err := record.GetAddress()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get address: %w", err)
	}

	// Export the armored private key
	armoredPrivKey, err := p.keyring.ExportPrivKeyArmorByAddress(addr, "")
	if err != nil {
		return nil, "", fmt.Errorf("failed to export armored private key: %w", err)
	}

	// Unarmor the private key
	privKey, _, err := crypto.UnarmorDecryptPrivKey(armoredPrivKey, "")
	if err != nil {
		return nil, "", fmt.Errorf("failed to unarmor private key: %w", err)
	}

	// Ensure it's a secp256k1 key
	secpPrivKey, ok := privKey.(*secp256k1.PrivKey)
	if !ok {
		return nil, "", fmt.Errorf("key %s is not a secp256k1 key", name)
	}

	// NOT addr.String(): that encodes with the global SDK prefix, which the
	// miner never sets -- see OperatorAddressPrefix.
	operatorAddr, err := OperatorAddress(addr)
	if err != nil {
		return nil, "", err
	}

	return secpPrivKey, operatorAddr, nil
}

// SupportsHotReload returns false - keyring doesn't support hot-reload.
func (p *KeyringProvider) SupportsHotReload() bool {
	return false
}

// WatchForChanges returns nil - keyring doesn't support hot-reload.
func (p *KeyringProvider) WatchForChanges(ctx context.Context) <-chan struct{} {
	return nil
}

// Close gracefully shuts down the provider.
func (p *KeyringProvider) Close() error {
	// Nothing to close for keyring
	return nil
}
