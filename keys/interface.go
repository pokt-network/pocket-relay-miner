package keys

import (
	"context"
	"time"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
)

// KeyManager provides dynamic management of supplier signing keys.
// It supports hot-reload of keys without service restart.
type KeyManager interface {
	// GetSigner returns the private key for signing for a given operator address.
	// Returns error if no key is found for the address.
	GetSigner(operatorAddr string) (cryptotypes.PrivKey, error)

	// ListSuppliers returns all operator addresses that have signing keys.
	ListSuppliers() []string

	// Reload reloads keys from all configured sources.
	// This is called automatically on file changes if hot-reload is enabled.
	Reload(ctx context.Context) error

	// OnKeyChange registers a callback that is called when keys change.
	// The callback receives the operator address and whether the key was added (true) or removed (false).
	OnKeyChange(callback KeyChangeCallback)

	// Start starts background processes (file watching, etc.)
	Start(ctx context.Context) error

	// Close gracefully shuts down the key manager.
	Close() error
}

// KeyChangeCallback is called when a key is added or removed.
type KeyChangeCallback func(operatorAddr string, added bool)

// KeyProvider is a source of keys for the KeyManager.
// Multiple providers can be combined (keyring + file).
type KeyProvider interface {
	// Name returns a human-readable name for this provider.
	Name() string

	// LoadKeys loads all keys from this provider.
	// Returns a map of operator address -> private key.
	LoadKeys(ctx context.Context) (map[string]cryptotypes.PrivKey, error)

	// SupportsHotReload returns true if this provider supports hot-reload.
	SupportsHotReload() bool

	// WatchForChanges returns a channel that signals when keys may have changed.
	// Only called if SupportsHotReload returns true.
	WatchForChanges(ctx context.Context) <-chan struct{}

	// Close gracefully shuts down the provider.
	Close() error
}

// KeyManagerConfig contains configuration for the KeyManager.
type KeyManagerConfig struct {

	// HotReloadEnabled enables automatic key reload on file changes.
	HotReloadEnabled bool

	// ReloadInterval is how often the keys are re-read regardless of whether
	// any source reported a change. Zero means DefaultReloadInterval.
	//
	// It is not an operator setting and has no YAML field: the interval IS the
	// promise made to whoever pulls a key ("it takes effect within this"), and
	// a promise that varies per deployment is not one. It exists as a field so
	// a test can drive the timer.
	ReloadInterval time.Duration
}
