package config

// KeysConfig contains key provider configuration.
// Shared between miner and relayer for loading supplier signing keys.
type KeysConfig struct {
	// KeysFile is the path to a supplier-keys.yaml file with hex-encoded keys.
	KeysFile string `yaml:"keys_file,omitempty"`

	// Keyring configuration for Cosmos SDK keyring.
	Keyring *KeyringConfig `yaml:"keyring,omitempty"`

	// HotReloadEnabled reloads the signing keys while the process runs, so a key
	// added or pulled takes effect without a restart.
	//
	// It covers every key source. keys_file is additionally WATCHED (its provider
	// watches the containing directory for Write|Create, which is what makes a
	// Kubernetes secret's ..data swap register), so a change there lands almost
	// at once. A keyring cannot be watched, so its changes are found by the
	// reload timer -- within keys.DefaultReloadInterval. The key manager logs
	// which sources are watched and which rely on the timer at startup.
	//
	// Defaults to true.
	HotReloadEnabled bool `yaml:"hot_reload_enabled"`

	// RemovedKeysDir is the tombstone for the retired keys_dir setting. The
	// YAML decoder drops unknown fields silently, so a config still carrying
	// keys_dir would boot without those supplier keys — the fleet serves and
	// signs nothing for them, which is revenue loss with no diagnostic.
	// Validation turns that case into a hard, explicit error instead.
	RemovedKeysDir string `yaml:"keys_dir,omitempty"`
}

// KeyringConfig contains Cosmos SDK keyring configuration.
type KeyringConfig struct {
	// Backend is the keyring backend type: "file" or "test".
	// Everything else cosmos-sdk offers is rejected: see keys.ValidateKeyringBackend.
	Backend string `yaml:"backend"`

	// Dir is the directory containing the keyring (for "file" backend).
	Dir string `yaml:"dir,omitempty"`

	// AppName is the application name for the keyring.
	// Default: "pocket"
	AppName string `yaml:"app_name,omitempty"`

	// KeyNames is an optional list of specific key names to load.
	// If empty, all keys are loaded.
	KeyNames []string `yaml:"key_names,omitempty"`

	// PassphraseFile is a file holding the keyring passphrase, for the "file"
	// backend. This is how a deployment actually supplies it: a Kubernetes
	// Secret or a docker compose secret mounted as a file. Preferred over
	// PassphraseEnv, because an environment variable is readable from
	// /proc/<pid>/environ by anything in the same namespace and tends to end up
	// in crash dumps and process listings.
	PassphraseFile string `yaml:"passphrase_file,omitempty"`

	// PassphraseEnv is the NAME of an environment variable holding the keyring
	// passphrase (not the passphrase itself). For deployments that only have
	// env vars to work with -- a bare compose file, a PaaS.
	//
	// Exactly one of PassphraseFile and PassphraseEnv may be set. With neither,
	// the passphrase is read from stdin, which is right for a human at a
	// terminal or a pipe from a secret manager, and wrong for a container:
	// there is nothing on stdin, so validation refuses that combination rather
	// than letting the process hang or fail obscurely.
	PassphraseEnv string `yaml:"passphrase_env,omitempty"`
}
