package keys

import "fmt"

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
