package keys

import (
	"context"
	"crypto/sha256"
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

// keyringDirFingerprint summarises the keyring directory by hashing the NAME
// AND CONTENTS of every entry, and returns how many entries it saw.
//
// Contents, not size and mtime, and that is the whole point. The first version
// used name:size:mtime and was WRONG for the rotation that matters most -- same
// key name, new key material. Measured against cosmos-sdk v0.53.7 by importing
// three different secp256k1 keys under one record name: app.info came out 732,
// 732 and 731 bytes. The size therefore carries about one bit, leaving mtime as
// the only discriminator, and mtime does not survive the ordinary ways a key
// file arrives: rsync -a, cp -p, tar -x and kubectl cp all restore the SOURCE
// file's mtime rather than "now", and NFS- or SMB-backed volumes quantise it. In
// any of those a rotated key would have been served from the cache forever and
// the process would have kept signing with the key the operator replaced --
// reintroducing, through an optimisation, the exact failure this branch exists
// to prevent.
//
// Hashing costs one read of ~732 bytes per key, about 435 KB at 594 keys:
// microseconds against the 40.5 ms per key of argon2 the cache avoids.
//
// Returns an error the caller must NOT treat as "no keys": an unreadable
// directory is the condition this exists to distinguish.
func (p *KeyringProvider) keyringDirFingerprint() (fingerprint string, entries int, err error) {
	dirEntries, err := os.ReadDir(p.keyringDir)
	if err != nil {
		return "", 0, err
	}
	lines := make([]string, 0, len(dirEntries))
	for _, e := range dirEntries {
		if e.IsDir() {
			lines = append(lines, "dir:"+e.Name())
			continue
		}
		contents, rerr := os.ReadFile(filepath.Join(p.keyringDir, e.Name()))
		if rerr != nil {
			return "", 0, fmt.Errorf("read %s: %w", e.Name(), rerr)
		}
		lines = append(lines, fmt.Sprintf("%s:%x", e.Name(), sha256.Sum256(contents)))
	}
	sort.Strings(lines)
	// Prefixed with the count so the fingerprint is never the empty string: an
	// EMPTY keyring directory is a real, cacheable state, and letting it share a
	// value with "not computed" is how a sentinel turns into a bug.
	//
	// The fingerprint hashes EVERY file, so any change is seen. The returned
	// COUNT is narrower on purpose -- it feeds the "records present, no keys out
	// of them" guard, and only key records may answer that question. See
	// isKeyRecord.
	records := 0
	for _, e := range dirEntries {
		if !e.IsDir() && isKeyRecord(e.Name()) {
			records++
		}
	}
	return fmt.Sprintf("%d\n%s", len(lines), strings.Join(lines, "\n")), records, nil
}

// isKeyRecord reports whether a file in a keyring directory is a KEY, as opposed
// to the backend's own bookkeeping.
//
// cosmos-sdk's file backend writes one <name>.info per key and one
// <addressHex>.address alias, and it also writes a "keyhash" file that records
// the passphrase hash. That keyhash is created on first unlock and is NEVER
// removed -- not when a key is deleted, not when the LAST key is deleted. So a
// keyring the operator legitimately emptied still has one file in it.
//
// Counting it as a record made the guard below read that state as "record files
// present, yet not one yielded a key", i.e. a broken keyring: LoadKeys returned
// an error, MultiProviderKeyManager.Reload abandoned the reload and kept the
// PREVIOUS key set, and the process went on signing with a key that had been
// withdrawn -- forever, retrying every tick. That is the exact failure this
// branch exists to prevent, arrived at from the other side. Measured 2026-08-28.
//
// Only ".info" counts, and that is cosmos-sdk's own semantics rather than a
// guess: it LISTS a keyring by its .info files and ignores a leftover .address.
// docs/SUPPLIER_KEYS.md documents removing just the .info as the way to withdraw
// a key -- measured on a four-pod fleet on 2026-08-22 -- so counting the orphaned
// .address as a record would make that documented, working procedure report a
// broken keyring.
//
// Matching the record suffix rather than excluding "keyhash" by name is
// deliberate: a future bookkeeping file would slip past an exclusion list and
// rebuild the same bug, while a new record type is a visible, deliberate edit
// here. A keyring whose .info files are all corrupt still has records and still
// reports broken, which is the condition the guard was written for.
func isKeyRecord(name string) bool {
	return strings.HasSuffix(name, ".info")
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

// ErrNotSecp256k1Key marks a record whose algorithm is not the one this
// service signs with. It is a SENTINEL rather than a string match because
// isPermanentKeyFailure has to recognise it: a record's algorithm is a property
// of the record, so this failure repeats on every reload forever.
var ErrNotSecp256k1Key = errors.New("key is not a secp256k1 key")

// isPermanentKeyFailure reports whether a per-key load failure will repeat on
// every future reload, in which case the record is simply not a signing key and
// must not stall the reload of the ones that are.
//
// Getting this set WRONG IN EITHER DIRECTION is harmful, and the two harms are
// not symmetric:
//
//   - Too NARROW: a permanent failure is reported as transient, the manager's
//     guard keeps the previous keys and abandons the reload, the failure repeats
//     next tick, and hot reload is dead for the life of the process while a key
//     the operator pulled keeps signing. "not a secp256k1 key" used to be a bare
//     fmt.Errorf that nothing matched, so it fell in this bucket.
//
//     HOW REACHABLE that particular branch is was MEASURED, not assumed, and the
//     answer is: not through this keyring's own API. A default cosmos-sdk
//     keyring rejects the import outright -- ImportPrivKeyHex with "ed25519" or
//     "sr25519" returns "unsupported signing algo" (probed against v0.53.7),
//     and hd exposes no Ed25519 algorithm to pass to NewAccount. It would take a
//     record written by a tool built with different SupportedAlgos and then
//     mounted into this directory. So the sentinel below is defence in depth,
//     not a fix for a reproduced failure, and there is deliberately no test for
//     it: a test that fabricates a state no provider can produce proves nothing
//     and teaches the next reader a false model.
//
//   - Too WIDE: a TRANSIENT failure is treated as "not a signing key", the
//     address is skipped, and the diff reports it as removed -- precisely the
//     silent removal this whole guard exists to prevent.
//
// So a failure only counts as permanent when it is a property of the RECORD, not
// of the attempt. A record with no extractable private key (offline pubkey,
// multisig, ledger), a named key that is gone, and a record of the wrong
// algorithm all qualify.
//
// Deliberately NOT here, though both are deterministic in practice: a
// GetAddress failure and an UnarmorDecryptPrivKey failure. A .info file caught
// mid-rewrite can produce either, and calling that permanent would turn a
// half-written file into a supplier removal. They stay transient, which costs a
// stalled reload and never costs a signature.
func isPermanentKeyFailure(err error) bool {
	return errors.Is(err, keyring.ErrPrivKeyExtr) ||
		errors.Is(err, sdkerrors.ErrKeyNotFound) ||
		errors.Is(err, ErrNotSecp256k1Key)
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
// A record cosmos-sdk drops on the floor is a different problem, and it IS
// closed, at the end of this function: keyring.List() is keystore.MigrateAll
// (crypto/keyring/keyring.go, v0.53.7), which SKIPS any record it cannot
// decode -- it prints to stderr and continues, returning a nil error. Read in
// the dependency's source, not inferred. Such a record would otherwise be
// invisible here and read as a removal, so the records the directory holds are
// counted against the records List reported.
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
	dirEntryCount := 0
	if p.keyringDir != "" {
		fp, n, err := p.keyringDirFingerprint()
		if err != nil {
			// Deliberately an error, not an empty key set. This is the PRIMARY
			// check for a keyring this process cannot read; the guard near the
			// end of this function covers a DIFFERENT condition (records the
			// keyring listed but could not decode).
			//
			// The keyring itself cannot answer this. 99designs/keyring v1.2.2,
			// the file backend under cosmos-sdk, DISCARDS the error --
			// fileKeyring.Keys() is `files, _ := os.ReadDir(dir)` (file.go:174)
			// -- and resolveDir MkdirAlls a directory that is missing
			// (file.go:44-49). Read in the dependency's source, not inferred. So
			// a keyring directory deleted, remounted, or stripped of its
			// permissions comes back as an empty list with a NIL error, which is
			// indistinguishable from an operator who removed every key. Reading
			// the directory ourselves is what separates them.
			//
			// The metric is not decoration: manager.Reload does not count
			// provider failures, on the stated grounds that "every provider
			// already increments keyLoadErrors itself, per failing key", so a
			// return that skips it leaves ha_keys_load_errors_total flat while
			// the process runs on stale keys and only an Error log repeats.
			keyLoadErrors.WithLabelValues(p.Kind()).Inc()
			return nil, fmt.Errorf("keyring directory %s could not be read: %w", p.keyringDir, err)
		}
		fingerprint, dirEntryCount = fp, n

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

	// Records the keyring listed but could not decode. Only the List() branch
	// below can produce one, and only it can count them -- see the guard near
	// the end of this function.
	swallowedRecords := 0

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
			// Same reason as the fingerprint read above: manager.Reload counts
			// nothing, so a provider return that skips the metric leaves the
			// alertable signal flat.
			keyLoadErrors.WithLabelValues(p.Kind()).Inc()
			return nil, fmt.Errorf("failed to list keyring keys: %w", err)
		}

		// dirEntryCount and List() enumerate the SAME set: isKeyRecord matches
		// ".info" and MigrateAll skips every other name (cosmos-sdk v0.53.7,
		// crypto/keyring/keyring.go:917-919). So len(records) can only be lower,
		// and the shortfall is exactly what MigrateAll swallowed.
		swallowedRecords = dirEntryCount - len(records)

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

	// A RECORD THAT VANISHED WITHOUT AN ERROR is the dangerous answer, because
	// the manager cannot tell it from a key the operator withdrew: both are an
	// address that is no longer there. It applies the second, so every affected
	// address is diffed as removed -- the relayer rejects those relays while
	// /ready still answers true, and the miner drains their pipelines and
	// releases their leases. keys.OpenManager already refuses zero keys AT
	// STARTUP for exactly this reason; nothing refused it on RELOAD.
	//
	// cosmos-sdk's keystore.MigrateAll SKIPS any record it cannot decode -- it
	// prints to stderr and returns a nil error (v0.53.7, keyring.go:920-924) --
	// and List() IS MigrateAll (keyring.go:539-541). That swallow is the whole
	// hazard, and it is PER RECORD, so it is counted per record: dirEntryCount
	// enumerates the .info files and List() reports the ones it decoded.
	//
	// Asking instead whether the whole load came back EMPTY -- what this did
	// until 2026-08-31 -- was wrong in both directions, and both were measured:
	//
	//   - It missed partial corruption entirely. Two records, one corrupt, and
	//     LoadKeys returned the survivor with a nil error: one supplier silently
	//     dropped, no signal anywhere.
	//   - It fired on withdrawals it had no business judging, because
	//     dirEntryCount counts records this load never even attempted. A record
	//     that is not a signing key -- an offline pubkey, a multisig, a ledger
	//     entry, all of which return keyring.ErrPrivKeyExtr forever -- kept the
	//     count above zero, so withdrawing the last REAL key read as a broken
	//     keyring: the manager abandoned the reload, kept the previous set, and
	//     the withdrawn key went on signing, retrying every tick forever. The
	//     same shape as the keyhash bug of 2026-08-28, from a third side.
	//
	// The by-name branch above needs no guard of its own, and that is a property
	// of the dependency rather than a choice: Key(uid) calls migrate directly
	// (keyring.go:603-609) and PROPAGATES the decode error, so a corrupt
	// selected record already lands in loadErrs and returns above -- measured
	// 2026-08-31. Only List() swallows, so only List() counts.
	//
	// The unreadable-DIRECTORY case is caught earlier still, by the fingerprint
	// read at the top of this function.
	//
	// The keys that DID load are returned alongside the error: the manager
	// exempts the first load from its keep-the-previous-set guard, so a
	// deployment whose keyring is partly corrupt at startup still comes up on
	// the records it could read.
	if swallowedRecords > 0 {
		keyLoadErrors.WithLabelValues(p.Kind()).Inc()
		return keys, fmt.Errorf(
			"keyring directory %s holds %d record file(s) but only %d could be decoded: "+
				"%d were skipped, so this is a broken keyring rather than the operator "+
				"removing keys", p.keyringDir, dirEntryCount, dirEntryCount-swallowedRecords,
			swallowedRecords)
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
		return nil, "", fmt.Errorf("key %s: %w", name, ErrNotSecp256k1Key)
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
