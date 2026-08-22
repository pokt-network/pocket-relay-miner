package keys

import (
	"fmt"
	"os"
	"strings"
)

// ValidateKeySources requires a configuration to name EXACTLY ONE key source.
//
// It is a deliberate narrowing rather than a limitation of the loader. The
// manager can merge providers, but a deployment that names both leaves questions
// the key path should never have to answer at runtime: which source wins a
// duplicate, what it means when one is unreadable while the other is fine, and
// which one an operator has to change to take a supplier out. That cost is paid
// on the day someone is trying to work out why a key they removed is still being
// served. So the answer is a startup error naming both fields, not a precedence
// rule nobody remembers.
//
// ZERO is an error too, for both binaries. Neither has anything to do without a
// signing key: a miner mines nothing, and a relayer answers every relay with a
// rejection while looking healthy from the outside. Until 2026-08-22 the relayer
// merely warned and carried on, which turned a config mistake into a silent
// outage that only showed up as relays failing.
//
// It lives here, taking plain strings, because both binaries have their own
// config types and this rule must not be able to differ between them -- they
// already disagree about where the hot-reload flag sits in the YAML
// (keys.hot_reload_enabled vs hot_reload_enabled), which is the kind of drift
// worth not repeating.
func ValidateKeySources(keysFile, keyringBackend string) error {
	hasFile := keysFile != ""
	hasKeyring := keyringBackend != ""

	switch {
	case hasFile && hasKeyring:
		return fmt.Errorf(
			"keys.keys_file and keys.keyring are mutually exclusive, but both are set "+
				"(keys_file=%q, keyring.backend=%q): configure exactly one key source and remove the other",
			keysFile, keyringBackend,
		)
	case !hasFile && !hasKeyring:
		return fmt.Errorf(
			"no key source configured: set exactly one of keys.keys_file or keys.keyring.backend " +
				"(without a signing key this process signs nothing and rejects every relay)",
		)
	}

	return nil
}

// NoKeysLoadedError is the error for a key source that is configured but
// produced nothing.
//
// It names the SOURCE and the uid, because that is what the answer turns on and
// what the previous wording made the reader go and find out: a keyring written
// by a root init container is unreadable to a process running as uid 1000, and
// "check your key configuration" does not point at that. Both binaries use it so
// the same fault reads the same way in either log.
func NoKeysLoadedError(keysFile, keyringBackend, keyringDir string) error {
	if keyringBackend != "" {
		return fmt.Errorf(
			"no signing keys loaded from keyring %q at %q (this process runs as uid %d): "+
				"empty, unreadable, or holds no valid secp256k1 key",
			keyringBackend, keyringDir, os.Getuid(),
		)
	}

	return fmt.Errorf(
		"no signing keys loaded from keys_file %q (this process runs as uid %d): "+
			"empty, unreadable, or holds no valid secp256k1 key",
		keysFile, os.Getuid(),
	)
}

// keyringBackends are the keyring backends this project supports, anywhere: the
// relayer, the miner and the CLI all accept exactly these.
//
// The bar is "we can confirm it works", not "cosmos-sdk offers it":
//
//   - "file" is passphrase-protected, the passphrase is piped in (see
//     NewKeyringProvider's PasswordReader) and unlocking is cached for the whole
//     keyring; a regression test seeds a real on-disk file keyring and reads it
//     back, and the local stack runs it end to end.
//   - "test" uses a fixed password and no prompt; the local stack runs it too.
//
// The ones cosmos-sdk also offers are refused, and the reasons are not symmetric:
//
//   - "memory" CANNOT hold supplier keys. Its keyring is created EMPTY and
//     nothing in this codebase ever writes a key into a keyring (verified
//     2026-08-22: no ImportPrivKeyHex, NewAccount or SaveOfflineKey outside
//     tests), so a process configured with it holds none for its whole life. It
//     used to be accepted, which meant the one thing an operator could not tell
//     from their config was that it could never work.
//   - "os" cannot be confirmed. cosmos-sdk sets no AllowedBackends for it, so it
//     uses a system keychain where dbus and an unlocked session exist -- which a
//     service account does not have -- and silently falls back to an encrypted
//     file where they do not, in a DIFFERENT directory than "file" ("os" uses the
//     dir it is given, "file" appends keyring-file/). Which of those a host does
//     is not something this code can establish, and there is no portable test.
//     An operator who wants a passphrase-protected keyring says "file" and gets
//     the behaviour they asked for.
//   - "kwallet" and "pass" were never wired at all.
var keyringBackends = []string{"file", "test"}

// ValidateKeyringBackend rejects a keyring backend this project does not support.
// One list for every caller, so a config, a flag and a provider cannot disagree
// about what is usable.
func ValidateKeyringBackend(backend string) error {
	for _, b := range keyringBackends {
		if backend == b {
			return nil
		}
	}

	switch backend {
	case "memory":
		return fmt.Errorf(
			"keyring backend %q cannot hold supplier keys: an in-memory keyring starts empty "+
				"and nothing writes to it, so this process would run with no signing key. Use one of: %s",
			backend, strings.Join(keyringBackends, ", "))
	case "os":
		return fmt.Errorf(
			"keyring backend %q is not supported: it needs a system keychain (dbus and an unlocked "+
				"session) where one is reachable and silently falls back to an encrypted file elsewhere, "+
				"in a different directory. Use \"file\" for a passphrase-protected keyring. Valid: %s",
			backend, strings.Join(keyringBackends, ", "))
	}

	return fmt.Errorf("invalid keyring backend %q: use one of: %s",
		backend, strings.Join(keyringBackends, ", "))
}
