package keys

import (
	"context"
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// KeyringSettings is the keyring half of a key configuration, reduced to what
// building a provider actually needs.
//
// It exists because the relayer and the miner each declare their own config
// types for the same thing, and the two functions that turned those into
// providers had already drifted: one defaulted an empty keyring directory to
// $HOME/.pocket and the other passed the empty string straight through. Neither
// is right, and worse, the difference was invisible.
type KeyringSettings struct {
	Backend        string
	Dir            string
	AppName        string
	KeyNames       []string
	PassphraseFile string
	PassphraseEnv  string
}

// BuildProviders turns a key configuration into the providers to load keys from.
// Both binaries and the CLI go through here, so what a key configuration MEANS
// is decided in one place.
//
// It returns providers in a stable order (keys file, then keyring) even though
// only one source may be configured -- see ValidateKeySources -- so that the
// order is not something a future second source has to rediscover.
func BuildProviders(logger logging.Logger, keysFile string, keyring *KeyringSettings) ([]KeyProvider, error) {
	var providers []KeyProvider

	if keysFile != "" {
		provider, err := NewSupplierKeysFileProvider(logger, keysFile)
		if err != nil {
			return nil, fmt.Errorf("failed to create supplier keys file provider: %w", err)
		}
		providers = append(providers, provider)
		logger.Info().Str("file", keysFile).Msg("added supplier keys file provider")
	}

	if keyring == nil || keyring.Backend == "" {
		return providers, nil
	}

	if err := ValidateKeyringDir(keyring.Dir); err != nil {
		return nil, err
	}

	// Where the passphrase comes from is a DEPLOYMENT concern -- a mounted
	// secret or an environment variable -- so it is resolved from configuration
	// rather than left to stdin, which would force a deployment to wrap the
	// command in a shell just to redirect a file into it. Nil means stdin, which
	// is what an interactive caller or a pipe relies on.
	passphrase, err := PassphraseReader(PassphraseSource{
		File: keyring.PassphraseFile,
		Env:  keyring.PassphraseEnv,
	})
	if err != nil {
		return nil, err
	}

	provider, err := NewKeyringProvider(logger, KeyringProviderConfig{
		Backend:        keyring.Backend,
		Dir:            keyring.Dir,
		AppName:        keyring.AppName,
		KeyNames:       keyring.KeyNames,
		PasswordReader: passphrase,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring provider: %w", err)
	}
	providers = append(providers, provider)
	logger.Info().
		Str("backend", keyring.Backend).
		Str("dir", keyring.Dir).
		Msg("added keyring provider")

	return providers, nil
}

// ValidateKeyringDir refuses an empty keyring directory.
//
// It is not defaulted because there is no default that can be right. cosmos-sdk
// joins the directory it is given with the backend's own subdirectory, so an
// empty one resolves to a RELATIVE "keyring-file" beside the process's working
// directory: a keyring silently somewhere else, therefore empty, after which
// every lookup fails as "key not found" -- a message that blames the key name.
// The miner used to paper over this with $HOME/.pocket, which is a workstation's
// layout rather than a deployment's, and the relayer passed the empty string
// straight through. Every caller shares this so the two cannot drift again.
func ValidateKeyringDir(dir string) error {
	if dir != "" {
		return nil
	}

	return fmt.Errorf(
		"a keyring directory is required: it is the directory CONTAINING the keyring, and an " +
			"empty value resolves to \"keyring-file\" relative to the working directory rather " +
			"than to any default location")
}

// OpenManager is the whole startup sequence for a process that signs with
// supplier keys: build the providers a configuration names, put a key manager
// over them, start it -- which loads once and arms both reload paths -- and
// refuse to continue with no keys.
//
// It exists so the relayer and the miner do not each carry their own copy of
// that sequence. They did, and the copies had already drifted on what an empty
// keyring directory means; a difference in how two binaries READ THE SAME KEYS
// is not something an operator should have to discover.
//
// The caller keeps what is genuinely its own: the relayer builds a ResponseSigner
// and subscribes to changes, the miner starts a supplier pipeline per key. Both
// do that through the returned manager, whose OnKeyChange fires only on a real
// change.
//
// The manager is returned started; the caller owns Close.
func OpenManager(
	ctx context.Context,
	logger logging.Logger,
	keysFile string,
	keyring *KeyringSettings,
	hotReloadEnabled bool,
) (*MultiProviderKeyManager, error) {
	providers, err := BuildProviders(logger, keysFile, keyring)
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		// Config validation refuses a configuration that names no source, so a
		// config loaded through LoadConfig cannot reach here. Kept because this
		// function does not itself prove the config was validated.
		return nil, fmt.Errorf("no key source configured: nothing to load signing keys from")
	}

	manager := NewMultiProviderKeyManager(logger, providers, KeyManagerConfig{
		HotReloadEnabled: hotReloadEnabled,
	})

	if err := manager.Start(ctx); err != nil {
		_ = manager.Close()
		return nil, fmt.Errorf("failed to start the key manager: %w", err)
	}

	if len(manager.ListSuppliers()) == 0 {
		_ = manager.Close()
		keyringBackend, keyringDir := "", ""
		if keyring != nil {
			keyringBackend, keyringDir = keyring.Backend, keyring.Dir
		}
		return nil, NoKeysLoadedError(keysFile, keyringBackend, keyringDir)
	}

	return manager, nil
}
