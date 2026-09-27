package keys

import (
	"context"
	"errors"
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

	// reloadMu serializes whole reloads. keysMu cannot do it: Reload does its
	// I/O (provider.LoadKeys) OUTSIDE that lock and takes it only to diff and
	// store, so two reloads can read the source at different moments and apply
	// their snapshots in the opposite order.
	//
	// Before the reload timer existed only the watchers called Reload, one per
	// source, so this could not happen. The timer makes concurrent reloaders
	// the normal case: the tick starts reading, the operator pulls a key, the
	// watcher's reload sees the removal and applies it, and then the tick --
	// holding a snapshot taken BEFORE the removal -- overwrites it and
	// resurrects the key, signing with material the operator withdrew until the
	// next tick happens to read again.
	reloadMu sync.Mutex

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

	// Read the count under the lock: the watchers and the reload timer are
	// already running by this point, so a reload can land between arming them
	// and logging here.
	m.keysMu.RLock()
	loaded := len(m.keys)
	m.keysMu.RUnlock()

	m.logger.Info().
		Int("providers", len(m.providers)).
		Int("keys", loaded).
		Msg("key manager started")

	m.logReloadPromptness()

	return nil
}

// reloadInterval is the interval the timer actually uses. Both the timer and the
// startup log read it from here so the number an operator is told cannot drift
// from the number being honoured.
func (m *MultiProviderKeyManager) reloadInterval() time.Duration {
	if m.config.ReloadInterval <= 0 {
		return DefaultReloadInterval
	}
	return m.config.ReloadInterval
}

// logReloadPromptness states, once at startup, how promptly each configured key
// source reacts to a change.
//
// It lives here rather than in either binary's startup because the answer
// depends only on the providers and the config, and because both binaries need
// it: the difference is invisible at runtime and it is what an operator relies
// on after pulling a key. keys_file is WATCHED -- its provider watches the
// containing directory for Write|Create, which is what makes a Kubernetes
// secret's ..data swap register -- so a change there lands almost at once. A
// keyring cannot be watched at all (WatchForChanges returns nil) and is picked
// up by the timer instead. Every source reloads; only the latency differs.
func (m *MultiProviderKeyManager) logReloadPromptness() {
	watched := make([]string, 0, len(m.providers))
	timerOnly := make([]string, 0, len(m.providers))
	for _, provider := range m.providers {
		if provider.SupportsHotReload() {
			watched = append(watched, provider.Name())
			continue
		}
		timerOnly = append(timerOnly, provider.Name())
	}

	if !m.config.HotReloadEnabled {
		// A fresh slice, not append into watched: it was allocated with capacity
		// for every provider, so appending would write into its backing array.
		all := make([]string, 0, len(watched)+len(timerOnly))
		all = append(all, watched...)
		all = append(all, timerOnly...)
		m.logger.Warn().
			Strs("sources", all).
			Msg("key hot reload is DISABLED: a key added or removed takes effect only on restart")
		return
	}

	m.logger.Info().
		Strs("watched_sources", watched).
		Strs("timer_only_sources", timerOnly).
		Dur("reload_interval", m.reloadInterval()).
		Msg("key hot reload active: watched sources react at once, every source within one reload interval")
}

// reloadPeriodically re-reads every key source on a fixed interval until the
// context ends.
//
// A failed reload is logged and the loop continues: the next tick retries, so a
// transient unreadable key file costs one interval of staleness rather than
// ending hot reload for the life of the process.
func (m *MultiProviderKeyManager) reloadPeriodically(ctx context.Context) {
	defer m.wg.Done()

	interval := m.reloadInterval()

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
			// Not logged here: Reload already reports a failure at Error with
			// which sources failed and how many keys it kept. Logging again
			// would double every line for as long as a source stays broken.
			_ = m.Reload(ctx)
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
	// One reload at a time, load included: see reloadMu.
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	newKeys := make(map[string]cryptotypes.PrivKey)
	var loadErrs []error

	// Load keys from each provider
	for _, provider := range m.providers {
		keys, err := provider.LoadKeys(ctx)
		if err != nil {
			// NOT counted here: every provider already increments
			// keyLoadErrors itself, per failing key. Counting again per failing
			// provider made one bad key move the series by two.
			loadErrs = append(loadErrs, fmt.Errorf("%s: %w", provider.Name(), err))
			m.logger.Warn().
				Err(err).
				Str("provider", provider.Name()).
				Msg("failed to load keys from provider")
			// Fall through: a provider may return keys AND an error, and the
			// keys it did read still matter on the FIRST load, which the guard
			// below exempts. On any later load the guard abandons the reload
			// anyway, so merging here cannot apply a partial set as a removal.
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

	// A source that could not be READ is not the operator REMOVING its keys, and
	// the diff below cannot tell those apart -- both are an address that is no
	// longer present. Translating the first into the second is severe and silent:
	// on the relayer every affected supplier stops being served, and on the miner
	// a removal drains the supplier's pipeline and releases its lease. The
	// triggers are ordinary -- a key file rewritten in place and read mid-write,
	// a projected secret caught mid-swap, a permissions blip, a keyring briefly
	// locked -- and the reload timer reaches them without anyone touching
	// anything.
	//
	// So a failed load abandons the WHOLE reload and keeps the previous set
	// intact. Applying the readable sources alone would still mean a removal
	// nobody performed; the price is that a genuine change in a healthy source
	// waits until the broken one is fixed, and the next reload retries.
	//
	// The FIRST load is exempt: with no previous set there is nothing to mistake
	// for a removal, and refusing to start over one misconfigured source would
	// take out a deployment whose other source is fine. The caller decides what
	// zero keys means -- the miner fails fast, the relayer warns.
	if len(loadErrs) > 0 && len(oldKeys) > 0 {
		m.keysMu.Unlock()

		joined := errors.Join(loadErrs...)
		m.logger.Error().
			Err(joined).
			Int("sources_failed", len(loadErrs)).
			Int("keys_held", len(oldKeys)).
			Msg("keeping the previous signing keys: a key source could not be read, and that is not a key removal")

		return fmt.Errorf("failed to reload keys: %w", joined)
	}

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
