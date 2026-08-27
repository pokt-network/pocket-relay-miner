package keys

import (
	"context"
	"fmt"
	"time"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
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
	// Name returns a human-readable name for this provider, for LOGS. It may
	// carry unbounded detail such as a file path.
	Name() string

	// Kind returns the provider's family -- "keyring", "supplier_keys_file" --
	// and is the only one of the two safe as a metric label. Name() puts the
	// key file's absolute path in the series, which is both unbounded and a
	// different value than the providers use when they count their own
	// failures, so one failed load produced two disjoint series.
	// It was added for exactly that and then left uncalled, while the three
	// increment sites repeated the literal. Wiring it up removed a dead
	// interface method and three copies of a string that could drift from it;
	// the values are identical, so no series moved.
	Kind() string

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

// OperatorAddressPrefix is Pocket Network's bech32 account prefix.
//
// Stated here, and applied through OperatorAddress, because a supplier operator
// address must not depend on process-global state. AccAddress.String() encodes
// with whatever prefix sdk.GetConfig() happens to hold, and only some entry
// points set it: the relayer calls initSDKConfig, the miner never has. So the
// keyring provider used to return cosmos1... addresses in the miner while the
// keys file provider returned pokt1... for the SAME private key -- measured
// 2026-08-22. Two consequences, both silent: a keyring-configured supplier could
// never match its on-chain identity and so was never mined, and with both
// sources configured one key appeared as two suppliers.
const OperatorAddressPrefix = "pokt"

// OperatorAddress bech32-encodes account bytes as a Pocket operator address.
// Every key provider goes through here, so all of them agree by construction,
// whatever any command did or did not do to the global SDK config.
func OperatorAddress(addr cosmostypes.AccAddress) (string, error) {
	encoded, err := cosmostypes.Bech32ifyAddressBytes(OperatorAddressPrefix, addr)
	if err != nil {
		return "", fmt.Errorf("failed to encode address with the %q prefix: %w", OperatorAddressPrefix, err)
	}
	return encoded, nil
}
