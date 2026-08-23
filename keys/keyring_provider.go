package keys

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

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

	// keyringDir is the directory cosmos-sdk actually stores records in --
	// config.Dir plus the backend's subdirectory -- or "" for a keyring this
	// process built in memory (the tests, via NewKeyringProviderWithKeyring).
	// Two things need it, and neither can be done through keyring.Keyring:
	// telling "the operator emptied the keyring" apart from "the directory is
	// gone", and knowing that nothing changed so a reload can skip the argon2
	// work. Empty means both fall back to the safe behaviour.
	keyringDir string

	// fingerprintMu guards the reload cache below.
	fingerprintMu sync.Mutex
	// lastFingerprint is the keyring directory's state at the last successful
	// full load, and cachedKeys is what that load produced.
	lastFingerprint string
	cachedKeys      map[string]cryptotypes.PrivKey
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
		logger:     logging.ForComponent(logger, logging.ComponentKeyRingProvider),
		keyring:    kr,
		appName:    config.AppName,
		keyNames:   config.KeyNames,
		keyringDir: keyringRecordDir(config.Backend, config.Dir),
	}, nil
}

// keyringRecordDir returns the directory cosmos-sdk stores records in for this
// backend, or "" when it cannot be known (no directory configured, or a backend
// with no on-disk form). Verified in cosmos-sdk v0.53.7
// crypto/keyring/keyring.go:677,704 -- newKeyringGeneric joins the backend's
// subdirectory onto the configured root.
func keyringRecordDir(backend, dir string) string {
	subdir, known := keyringSubdirs[backend]
	if !known || dir == "" {
		return ""
	}
	return filepath.Join(dir, subdir)
}

// keyringDirFingerprint summarises the keyring directory: every entry's name,
// size and modification time. A key added, removed or rewritten moves it; a
// tick where nothing happened does not.
//
// Returns an error the caller must NOT treat as "no keys": an unreadable
// directory is the condition this exists to distinguish.
func (p *KeyringProvider) keyringDirFingerprint() (string, error) {
	entries, err := os.ReadDir(p.keyringDir)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			return "", fmt.Errorf("stat %s: %w", e.Name(), ierr)
		}
		names = append(names, fmt.Sprintf("%s:%d:%d", e.Name(), info.Size(), info.ModTime().UnixNano()))
	}
	sort.Strings(names)
	// Prefixed with the count so the fingerprint is never the empty string: an
	// EMPTY keyring directory is a real, cacheable state, and letting it share a
	// value with "not computed" is how a sentinel turns into a bug.
	return fmt.Sprintf("%d\n%s", len(names), strings.Join(names, "\n")), nil
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

// Kind returns the provider family, for metric labels.
func (p *KeyringProvider) Kind() string { return "keyring" }

// isPermanentKeyFailure reports whether a per-key load failure will repeat on
// every future reload, in which case the record is simply not a signing key and
// must not stall the reload of the ones that are.
func isPermanentKeyFailure(err error) bool {
	return errors.Is(err, keyring.ErrPrivKeyExtr) || errors.Is(err, sdkerrors.ErrKeyNotFound)
}

// LoadKeys loads all keys from the keyring.
// A key that cannot be read is returned as an ERROR alongside the keys that
// could, because the manager's "an unreadable source is not a key removal"
// guard keys off that error. Logging a warning and returning a shorter map with
// a nil error -- what this did until 2026-08-22 -- made the exact triggers that
// guard enumerates (a keyring briefly locked, a permissions blip, a .info file
// caught mid-rewrite) look like the operator having removed those suppliers:
// the relayer stops serving them and the miner drains their pipelines.
//
// Only a TRANSIENT failure counts. A record that can never yield a private key
// -- an offline pubkey, a multisig or a ledger entry, which return
// keyring.ErrPrivKeyExtr on every call -- and a named key the operator deleted
// are not signing keys and never will be, so reporting them would abandon every
// reload for the life of the process: the guard keeps the previous set, the
// stable failure repeats, and hot reload dies silently while a pulled key keeps
// signing. Those are skipped; only errors that may clear on a retry are
// returned.
//
// One gap remains and is NOT closed here: keyring.List() is cosmos-sdk's
// keystore.MigrateAll (crypto/keyring/keyring.go, v0.53.7), which SKIPS any
// record it cannot decode -- it prints to stderr and continues, returning a nil
// error. Read in the dependency's source, not inferred. A record lost that way
// is invisible to this provider, so it still reads as a removal.
func (p *KeyringProvider) LoadKeys(ctx context.Context) (map[string]cryptotypes.PrivKey, error) {
	// A reload that finds the directory byte-for-byte unchanged returns the
	// previous keys without touching the keyring. This is not a micro-
	// optimisation: reading one key runs argon2id TWICE (cosmos-sdk
	// crypto/armor.go:165 to armor it, :223 to unarmor it, both t=1 m=64MiB
	// p=4), measured at 40.5 ms and ~128 MiB transient on this machine. On the
	// 30 s reload timer that is 0.7 s per tick at 17 keys and 24 s at 594 --
	// the fleet size this project's own block-event scaling note describes --
	// so a busy core and tens of GiB of churn per tick on a process with a
	// 1000 RPS budget, and the interval stops being the promise it is
	// documented to be. ReloadInterval is deliberately not an operator knob,
	// so the cost had to come out of the no-op path instead.
	//
	// The cache is only ever consulted when the fingerprint MATCHES, so it can
	// hold a key past its removal only if a change left names, sizes and
	// nanosecond mtimes all identical.
	var fingerprint string
	if p.keyringDir != "" {
		fp, err := p.keyringDirFingerprint()
		if err != nil {
			// Deliberately an error, not an empty key set: see the guard at the
			// end of this function.
			return nil, fmt.Errorf("keyring directory %s could not be read: %w", p.keyringDir, err)
		}
		fingerprint = fp

		p.fingerprintMu.Lock()
		if p.cachedKeys != nil && p.lastFingerprint == fingerprint {
			cached := make(map[string]cryptotypes.PrivKey, len(p.cachedKeys))
			for addr, key := range p.cachedKeys {
				cached[addr] = key
			}
			p.fingerprintMu.Unlock()
			p.logger.Debug().
				Int("keys", len(cached)).
				Msg("keyring unchanged on disk, reusing the loaded keys")
			return cached, nil
		}
		p.fingerprintMu.Unlock()
	}

	keys := make(map[string]cryptotypes.PrivKey)
	var loadErrs []error

	// If specific key names are provided, load only those
	if len(p.keyNames) > 0 {
		for _, name := range p.keyNames {
			privKey, addr, err := p.loadKeyByName(name)
			if err != nil {
				p.logger.Warn().
					Err(err).
					Str("key_name", name).
					Msg("failed to load key from keyring")
				if isPermanentKeyFailure(err) {
					continue
				}
				keyLoadErrors.WithLabelValues(p.Kind()).Inc()
				loadErrs = append(loadErrs, fmt.Errorf("key %q: %w", name, err))
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
				if isPermanentKeyFailure(err) {
					continue
				}
				keyLoadErrors.WithLabelValues(p.Kind()).Inc()
				loadErrs = append(loadErrs, fmt.Errorf("key %q: %w", record.Name, err))
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

	if len(loadErrs) > 0 {
		return keys, fmt.Errorf("%d keyring key(s) could not be read: %w",
			len(loadErrs), errors.Join(loadErrs...))
	}

	// ZERO KEYS AND NO ERROR is the dangerous answer, because the keyring
	// cannot tell us which of two very different things happened.
	//
	// 99designs/keyring v1.2.2 (the file backend under cosmos-sdk) discards the
	// error: fileKeyring.Keys() is `files, _ := os.ReadDir(dir)` (file.go:174),
	// and resolveDir MkdirAlls a directory that is missing (file.go:44-49). Read
	// in the dependency's source, not inferred. So a keyring directory that was
	// deleted, remounted, or lost its permissions comes back as an empty list
	// with a nil error -- indistinguishable, here, from an operator who removed
	// every key.
	//
	// The manager's "an unreadable source is not a key removal" guard keys off
	// an error, so with no error it applies the empty set: every address is
	// diffed as removed, the relayer rejects every relay while /ready still
	// answers true, and the miner drains every pipeline and releases every
	// lease. keys.OpenManager already refuses zero keys AT STARTUP for exactly
	// this reason; nothing refused it on RELOAD.
	//
	// Reading the directory ourselves settles it, and keeps the distinction the
	// blunt fix would have lost: a genuinely emptied keyring is still applied,
	// so pulling the last key out of a single-key deployment still stops it
	// signing.
	if len(keys) == 0 && p.keyringDir != "" {
		if _, err := os.ReadDir(p.keyringDir); err != nil {
			return nil, fmt.Errorf(
				"keyring reported no keys, but its directory %s could not be read, "+
					"so this is not the operator removing keys: %w", p.keyringDir, err)
		}
	}

	if p.keyringDir != "" {
		p.fingerprintMu.Lock()
		p.lastFingerprint = fingerprint
		p.cachedKeys = make(map[string]cryptotypes.PrivKey, len(keys))
		for addr, key := range keys {
			p.cachedKeys[addr] = key
		}
		p.fingerprintMu.Unlock()
	}

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
