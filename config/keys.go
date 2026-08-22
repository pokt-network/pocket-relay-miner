package config

// KeysConfig contains key provider configuration.
// Shared between miner and relayer for loading supplier signing keys.
type KeysConfig struct {
	// KeysFile is the path to a supplier-keys.yaml file with hex-encoded keys.
	KeysFile string `yaml:"keys_file,omitempty"`

	// Keyring configuration for Cosmos SDK keyring.
	Keyring *KeyringConfig `yaml:"keyring,omitempty"`

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
	// "memory" is rejected: see keys.ValidateKeyringBackend.
	Backend string `yaml:"backend"`

	// Dir is the directory containing the keyring (for "file" backend).
	Dir string `yaml:"dir,omitempty"`

	// AppName is the application name for the keyring.
	// Default: "pocket"
	AppName string `yaml:"app_name,omitempty"`

	// KeyNames is an optional list of specific key names to load.
	// If empty, all keys are loaded.
	KeyNames []string `yaml:"key_names,omitempty"`
}
