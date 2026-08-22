package keys

import (
	"context"
	"fmt"
	"sync"
	"time"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

var _ KeyManager = (*MultiProviderKeyManager)(nil)

// DefaultReloadInterval is how often the keys are re-read when no interval is
// configured. It is the promptness an operator can rely on for ANY key source,
// including the ones that cannot be watched.
const DefaultReloadInterval = 30 * time.Second

// MultiProviderKeyManager implements KeyManager using multiple KeyProviders.
// It aggregates keys from all providers and supports hot-reload.
type MultiProviderKeyManager struct {
	logger    logging.Logger
	providers []KeyProvider
	config    KeyManagerConfig

	// Keys storage
	keys   map[string]cryptotypes.PrivKey // operatorAddr -> privKey
	keysMu sync.RWMutex

	// Change callbacks
	callbacks   []KeyChangeCallback
	callbacksMu sync.RWMutex

	// Lifecycle
	mu       sync.Mutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewMultiProviderKeyManager creates a new KeyManager with multiple providers.
func NewMultiProviderKeyManager(
	logger logging.Logger,
	providers []KeyProvider,
	config KeyManagerConfig,
) *MultiProviderKeyManager {
	return &MultiProviderKeyManager{
		logger:    logging.ForComponent(logger, logging.ComponentKeyManager),
		providers: providers,
		config:    config,
		keys:      make(map[string]cryptotypes.PrivKey),
	}
}

// Start starts background processes (file watching, etc.)
func (m *MultiProviderKeyManager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("key manager is closed")
	}

	ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	// Initial key load
	if err := m.Reload(ctx); err != nil {
		return fmt.Errorf("failed to load initial keys: %w", err)
	}

	// Start watching each provider that supports hot-reload
	if m.config.HotReloadEnabled {
		for _, provider := range m.providers {
			if provider.SupportsHotReload() {
				m.wg.Add(1)
				go m.watchProvider(ctx, provider)
			}
		}

		// And re-read every source on a timer, whether or not anything said it
		// changed. The watch is an ACCELERATOR, not the mechanism:
		//
		//   - a source can be unwatchable. The keyring's WatchForChanges
		//     returns nil, so on watches alone a key pulled from a keyring
		//     never takes effect at all.
		//   - a watch can die and say nothing. fsnotify drops the watch when
		//     the inode it holds is renamed away, and a watcher goroutine that
		//     ends leaves a process that reloads for the rest of its life
		//     without ever reporting that it stopped.
		//
		// A timer has neither failure mode, and it turns promptness into a
		// number that can be stated: a key change takes effect within one
		// interval, from any source. Both paths call the same Reload, so there
		// is exactly one piece of code that decides what changed -- and Reload
		// is silent when nothing did, which is what makes a steady tick free.
		m.wg.Add(1)
		go logging.RecoverGoRoutine(m.logger, "key_manager_periodic_reload", m.reloadPeriodically)(ctx)
	}

	m.logger.Info().
		Int("providers", len(m.providers)).
		Int("keys", len(m.keys)).
		Msg("key manager started")

	return nil
}

// reloadPeriodically re-reads every key source on a fixed interval until the
// context ends.
//
// A failed reload is logged and the loop continues: the next tick retries, so a
// transient unreadable key file costs one interval of staleness rather than
// ending hot reload for the life of the process.
func (m *MultiProviderKeyManager) reloadPeriodically(ctx context.Context) {
	defer m.wg.Done()

	interval := m.config.ReloadInterval
	if interval <= 0 {
		interval = DefaultReloadInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.logger.Info().
		Dur("interval", interval).
		Msg("key reload timer started: a key change takes effect within one interval from any source")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Reload(ctx); err != nil {
				m.logger.Error().
					Err(err).
					Msg("periodic key reload failed; retrying on the next tick")
			}
		}
	}
}

// watchProvider watches a single provider for key changes.
func (m *MultiProviderKeyManager) watchProvider(ctx context.Context, provider KeyProvider) {
	defer m.wg.Done()

	changes := provider.WatchForChanges(ctx)
	if changes == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
			m.logger.Info().
				Str("provider", provider.Name()).
				Msg("key change detected, reloading")

			if err := m.Reload(ctx); err != nil {
				m.logger.Error().
					Err(err).
					Str("provider", provider.Name()).
					Msg("failed to reload keys")
			}
		}
	}
}

// GetSigner returns the private key for signing for a given operator address.
func (m *MultiProviderKeyManager) GetSigner(operatorAddr string) (cryptotypes.PrivKey, error) {
	m.keysMu.RLock()
	defer m.keysMu.RUnlock()

	key, ok := m.keys[operatorAddr]
	if !ok {
		return nil, fmt.Errorf("no key found for operator %s", operatorAddr)
	}

	return key, nil
}

// ListSuppliers returns all operator addresses that have signing keys.
func (m *MultiProviderKeyManager) ListSuppliers() []string {
	m.keysMu.RLock()
	defer m.keysMu.RUnlock()

	suppliers := make([]string, 0, len(m.keys))
	for addr := range m.keys {
		suppliers = append(suppliers, addr)
	}
	return suppliers
}

// Reload reloads keys from all configured sources.
func (m *MultiProviderKeyManager) Reload(ctx context.Context) error {
	newKeys := make(map[string]cryptotypes.PrivKey)

	// Load keys from each provider
	for _, provider := range m.providers {
		keys, err := provider.LoadKeys(ctx)
		if err != nil {
			m.logger.Warn().
				Err(err).
				Str("provider", provider.Name()).
				Msg("failed to load keys from provider")
			continue
		}

		for addr, key := range keys {
			if _, exists := newKeys[addr]; exists {
				m.logger.Warn().
					Str("operator", addr).
					Str("provider", provider.Name()).
					Msg("duplicate key, using later provider")
			}
			newKeys[addr] = key
		}

		m.logger.Debug().
			Str("provider", provider.Name()).
			Int("keys", len(keys)).
			Msg("loaded keys from provider")
	}

	// Determine added and removed keys.
	//
	// An address appearing or disappearing is the WHOLE space of changes, and
	// that is a property of how addresses are made, not an assumption: every
	// provider DERIVES the operator address from the key material
	// (parseHexKeyWithAddress in supplier_keys_file.go, record.GetAddress in
	// keyring_provider.go). Same address means same public key means same
	// private key, so "the key behind an unchanged address changed" cannot
	// occur -- a rotation shows up as one address leaving and another arriving.
	// A provider that ever took an address from configuration instead of
	// deriving it would break this, and the diff below with it.
	m.keysMu.Lock()
	oldKeys := m.keys

	added := make([]string, 0)
	removed := make([]string, 0)

	// Find added keys
	for addr := range newKeys {
		if _, existed := oldKeys[addr]; !existed {
			added = append(added, addr)
		}
	}

	// Find removed keys
	for addr := range oldKeys {
		if _, exists := newKeys[addr]; !exists {
			removed = append(removed, addr)
		}
	}

	m.keys = newKeys
	m.keysMu.Unlock()

	// Notify callbacks
	for _, addr := range added {
		m.notifyKeyChange(addr, true)
	}
	for _, addr := range removed {
		m.notifyKeyChange(addr, false)
	}

	// A reload that changed nothing is SILENT: no counter, and a Debug line
	// rather than an Info one. Reloads can be driven on a timer over sources
	// that cannot be watched, so the steady state is a reload that finds the
	// same keys -- reporting that as an event would bury the reload that
	// mattered under the ones that did not.
	if len(added) == 0 && len(removed) == 0 {
		m.logger.Debug().
			Int("total", len(newKeys)).
			Msg("reloaded keys: unchanged")
		supplierKeysActive.Set(float64(len(newKeys)))
		return nil
	}

	m.logger.Info().
		Int("total", len(newKeys)).
		Int("added", len(added)).
		Int("removed", len(removed)).
		Msg("reloaded keys")

	keyReloadsTotal.Inc()
	supplierKeysActive.Set(float64(len(newKeys)))

	return nil
}

// notifyKeyChange notifies all callbacks of a key change.
func (m *MultiProviderKeyManager) notifyKeyChange(operatorAddr string, added bool) {
	m.callbacksMu.RLock()
	callbacks := make([]KeyChangeCallback, len(m.callbacks))
	copy(callbacks, m.callbacks)
	m.callbacksMu.RUnlock()

	for _, cb := range callbacks {
		cb(operatorAddr, added)
	}

	if added {
		keyChangesTotal.WithLabelValues("added").Inc()
	} else {
		keyChangesTotal.WithLabelValues("removed").Inc()
	}
}

// OnKeyChange registers a callback that is called when keys change.
func (m *MultiProviderKeyManager) OnKeyChange(callback KeyChangeCallback) {
	m.callbacksMu.Lock()
	defer m.callbacksMu.Unlock()

	m.callbacks = append(m.callbacks, callback)
}

// Close gracefully shuts down the key manager.
func (m *MultiProviderKeyManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}

	m.closed = true

	if m.cancelFn != nil {
		m.cancelFn()
	}

	// Close all providers
	for _, provider := range m.providers {
		if err := provider.Close(); err != nil {
			m.logger.Warn().
				Err(err).
				Str("provider", provider.Name()).
				Msg("error closing provider")
		}
	}

	m.wg.Wait()

	m.logger.Info().Msg("key manager closed")
	return nil
}
