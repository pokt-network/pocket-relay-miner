//go:build test

package keys

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// These tests use REAL providers -- a real cosmos keyring and a real supplier
// keys file -- rather than a stand-in, because the question they answer is
// whether hot reload works for the sources an operator actually configures. The
// stand-in provider in manager_reload_change_test.go answers a different
// question (does the TIMER fire) and cannot answer this one.
//
// One source at a time, because that is all a configuration may name: both
// keys_file and keyring set is a startup error (keys.ValidateKeySources).

// hotReloadTestKeys are two well-formed secp256k1 private keys in hex. Their
// operator addresses are derived from the material, so importing one into a
// keyring and writing the other into a keys file gives two distinct suppliers.
const (
	hotReloadHexA = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"
	hotReloadHexB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func addressOfHex(t *testing.T, hexKey string) string {
	t.Helper()
	_, addr, err := parseHexKeyWithAddress(hexKey)
	require.NoError(t, err)
	return addr
}

func writeKeysFile(t *testing.T, path string, hexKeys ...string) {
	t.Helper()
	body := "keys:\n"
	for _, k := range hexKeys {
		body += "  - " + k + "\n"
	}
	// Written via a temp file and renamed, which is how a key file is actually
	// replaced (editors, sed -i, and projected volumes all do this) and what the
	// file provider's directory watch is built for.
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(body), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

// TestAKeyringCannotBeWatchedSoOnlyAReloadFindsItsChanges is the link between
// the keyring and the reload timer: it states, against the real provider, the
// fact that makes the timer necessary rather than merely nice.
//
// If this ever starts failing because the keyring became watchable, the timer is
// no longer the only way its changes are found -- and the promptness the startup
// log promises would need revisiting.
func TestAKeyringCannotBeWatchedSoOnlyAReloadFindsItsChanges(t *testing.T) {
	kr := newInMemoryKeyring(t)
	provider := NewKeyringProviderWithKeyring(logging.NewLoggerFromConfig(logging.DefaultConfig()), kr, nil)

	require.False(t, provider.SupportsHotReload(),
		"the keyring reports itself watchable: the timer is no longer its only path")
	require.Nil(t, provider.WatchForChanges(context.Background()),
		"the keyring returned a change channel: nothing would ever signal on it")
}

// TestKeyringAddAndRemoveReachTheManagerThroughAReload drives a REAL keyring
// through the whole cycle an operator performs: import a key, then delete it.
//
// Reload is called explicitly rather than waited for: what is under test here is
// the keyring provider and the diff, and the cosmos keyring is not built for a
// concurrent writer, so letting the timer read it while the test mutates it
// would be testing the wrong thing. That the TIMER fires at all is pinned
// separately by TestPeriodicReloadPicksUpAChangeFromAnUnwatchableSource.
func TestKeyringAddAndRemoveReachTheManagerThroughAReload(t *testing.T) {
	kr := newInMemoryKeyring(t)
	require.NoError(t, kr.ImportPrivKeyHex("first", hotReloadHexA, "secp256k1"))

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	provider := NewKeyringProviderWithKeyring(logger, kr, nil)

	m := NewMultiProviderKeyManager(logger, []KeyProvider{provider}, KeyManagerConfig{})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start(context.Background()))

	addrA := addressOfHex(t, hotReloadHexA)
	addrB := addressOfHex(t, hotReloadHexB)
	require.Equal(t, []string{addrA}, m.ListSuppliers())

	changes := make([]string, 0)
	m.OnKeyChange(func(addr string, added bool) {
		verb := "removed"
		if added {
			verb = "added"
		}
		changes = append(changes, verb+":"+addr)
	})

	// The operator imports a second supplier key into the keyring.
	require.NoError(t, kr.ImportPrivKeyHex("second", hotReloadHexB, "secp256k1"))
	require.NoError(t, m.Reload(context.Background()))

	require.Equal(t, []string{"added:" + addrB}, changes)
	require.ElementsMatch(t, []string{addrA, addrB}, m.ListSuppliers())
	signer, err := m.GetSigner(addrB)
	require.NoError(t, err)
	require.NotNil(t, signer)

	// And pulls the first one out again.
	changes = changes[:0]
	require.NoError(t, kr.Delete("first"))
	require.NoError(t, m.Reload(context.Background()))

	require.Equal(t, []string{"removed:" + addrA}, changes)
	require.Equal(t, []string{addrB}, m.ListSuppliers())
	_, err = m.GetSigner(addrA)
	require.Error(t, err, "the manager still serves a key deleted from the keyring")
}

// TestKeysFileAddAndRemoveReachTheManagerThroughItsWatch is the other source,
// end to end through the REAL file provider and its directory watch -- no
// explicit Reload, because here the watch is exactly what is under test.
func TestKeysFileAddAndRemoveReachTheManagerThroughItsWatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supplier-keys.yaml")
	writeKeysFile(t, path, hotReloadHexA)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	provider, err := NewSupplierKeysFileProvider(logger, path)
	require.NoError(t, err)
	require.True(t, provider.SupportsHotReload(), "the keys file must be watchable")

	m := NewMultiProviderKeyManager(logger, []KeyProvider{provider},
		// Hot reload on, but the timer pushed far out so a pass can only be the
		// WATCH firing. Otherwise this test would pass on either mechanism and
		// prove neither.
		KeyManagerConfig{HotReloadEnabled: true, ReloadInterval: time.Hour})
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, m.Start(ctx))

	addrA := addressOfHex(t, hotReloadHexA)
	addrB := addressOfHex(t, hotReloadHexB)
	require.Equal(t, []string{addrA}, m.ListSuppliers())

	added := make(chan string, 8)
	removed := make(chan string, 8)
	m.OnKeyChange(func(addr string, isAdd bool) {
		if isAdd {
			added <- addr
			return
		}
		removed <- addr
	})

	// A supplier arrives.
	writeKeysFile(t, path, hotReloadHexA, hotReloadHexB)
	select {
	case got := <-added:
		require.Equal(t, addrB, got)
	case <-time.After(30 * time.Second):
		t.Fatal("the keys file watch never reported the added key")
	}

	// And one is pulled.
	writeKeysFile(t, path, hotReloadHexB)
	select {
	case got := <-removed:
		require.Equal(t, addrA, got)
	case <-time.After(30 * time.Second):
		t.Fatal("the keys file watch never reported the removed key")
	}

	_, err = m.GetSigner(addrA)
	require.Error(t, err)
}

// TestAnEmptiedKeysFileIsRefusedRatherThanTreatedAsRemoveEverything is a
// deliberate assertion about what a key file with no entries MEANS.
//
// It is refused, so the previous keys are kept. That is the safe reading and the
// only defensible one: "the file has no keys" is far more often a write that was
// truncated, a bad template render, or a half-written deploy than an operator
// asking to stop serving every supplier at once -- and the reload timer would
// apply it within one interval, with no human watching. An operator who really
// wants to stop serving unstakes or stops the process; there is no way to say
// "remove everything" through this file, on purpose.
func TestAnEmptiedKeysFileIsRefusedRatherThanTreatedAsRemoveEverything(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supplier-keys.yaml")
	writeKeysFile(t, path, hotReloadHexA)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	provider, err := NewSupplierKeysFileProvider(logger, path)
	require.NoError(t, err)

	m := NewMultiProviderKeyManager(logger, []KeyProvider{provider}, KeyManagerConfig{})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start(context.Background()))

	addrA := addressOfHex(t, hotReloadHexA)
	require.Equal(t, []string{addrA}, m.ListSuppliers())

	changes := make([]string, 0)
	m.OnKeyChange(func(addr string, _ bool) { changes = append(changes, addr) })

	writeKeysFile(t, path) // no entries at all
	require.Error(t, m.Reload(context.Background()),
		"a key file with no entries must be refused, not applied")

	require.Empty(t, changes, "an emptied key file removed every supplier's key")
	require.Equal(t, []string{addrA}, m.ListSuppliers())
	signer, err := m.GetSigner(addrA)
	require.NoError(t, err)
	require.NotNil(t, signer)
}

// TestAnUnreadableKeysFileKeepsTheKeysItAlreadyLoaded is the failure mode that
// the reload timer makes reachable without anyone touching anything, driven
// through the REAL file provider: the file becomes unreadable, and the keys it
// had already provided must NOT be reported as removed.
func TestAnUnreadableKeysFileKeepsTheKeysItAlreadyLoaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supplier-keys.yaml")
	writeKeysFile(t, path, hotReloadHexA)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	provider, err := NewSupplierKeysFileProvider(logger, path)
	require.NoError(t, err)

	m := NewMultiProviderKeyManager(logger, []KeyProvider{provider}, KeyManagerConfig{})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start(context.Background()))

	addrA := addressOfHex(t, hotReloadHexA)
	require.Equal(t, []string{addrA}, m.ListSuppliers())

	changes := make([]string, 0)
	m.OnKeyChange(func(addr string, addedKey bool) { changes = append(changes, addr) })

	// Gone, as during an in-place rewrite or a bad deploy.
	require.NoError(t, os.Remove(path))
	require.Error(t, m.Reload(context.Background()),
		"a reload that could not read its only source must report it")

	require.Empty(t, changes, "an unreadable key file was reported as the operator removing its keys")
	require.Equal(t, []string{addrA}, m.ListSuppliers())
	signer, err := m.GetSigner(addrA)
	require.NoError(t, err)
	require.NotNil(t, signer)

	// And it recovers on the next reload once the file is back.
	writeKeysFile(t, path, hotReloadHexA, hotReloadHexB)
	require.NoError(t, m.Reload(context.Background()))
	require.ElementsMatch(t,
		[]string{addrA, addressOfHex(t, hotReloadHexB)}, m.ListSuppliers())
}
