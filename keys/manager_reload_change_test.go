//go:build test

package keys

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestOperatorAddressIsDerivedFromTheKeyMaterial pins the property that makes
// Reload's added/removed diff COMPLETE.
//
// The diff compares address sets, so it can only be the whole story if an
// address cannot outlive the key behind it. It cannot, because the address is
// derived from the public key -- so two different keys can never share an
// address, and "the key changed but the address did not" is unreachable rather
// than merely unlikely. A provider that took the address from configuration
// instead of deriving it would break this test and the diff together, which is
// the point of having it.
func TestOperatorAddressIsDerivedFromTheKeyMaterial(t *testing.T) {
	firstKey, firstAddr, err := parseHexKeyWithAddress(validKeyHex)
	require.NoError(t, err)
	secondKey, secondAddr, err := parseHexKeyWithAddress(replacedKeyHex)
	require.NoError(t, err)

	require.False(t, firstKey.Equals(secondKey), "the two test keys must differ for this to prove anything")
	require.NotEqual(t, firstAddr, secondAddr, "two different keys produced the same operator address")

	// And the derivation is a function: the same material always lands on the
	// same address, so an unchanged key never looks like a change.
	_, sameAddr, err := parseHexKeyWithAddress(validKeyHex)
	require.NoError(t, err)
	require.Equal(t, firstAddr, sameAddr)
}

// staticProvider serves whatever key map it currently holds, and stands in for
// the keyring: it reports no hot-reload support and returns nil from
// WatchForChanges, so nothing will ever tell the manager that its keys changed.
//
// Guarded by a mutex because a test rewrites the keys while the manager's reload
// timer is reading them.
type staticProvider struct {
	mu   sync.Mutex
	keys map[string]cryptotypes.PrivKey
}

func (p *staticProvider) Name() string { return "static" }

func (p *staticProvider) setKeys(keys map[string]cryptotypes.PrivKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = keys
}

func (p *staticProvider) LoadKeys(context.Context) (map[string]cryptotypes.PrivKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]cryptotypes.PrivKey, len(p.keys))
	for addr, key := range p.keys {
		out[addr] = key
	}
	return out, nil
}

func (p *staticProvider) SupportsHotReload() bool { return false }

func (p *staticProvider) WatchForChanges(context.Context) <-chan struct{} { return nil }

func (p *staticProvider) Close() error { return nil }

// newReloadManager wires a manager over a provider serving initial, and returns
// it with the provider and a per-address change log. The subscription is
// registered after Start so the initial load is not counted: a subscriber takes
// its first snapshot itself.
func newReloadManager(
	t *testing.T,
	initial map[string]cryptotypes.PrivKey,
) (*MultiProviderKeyManager, *staticProvider, *[]string) {
	t.Helper()

	provider := &staticProvider{keys: initial}
	m := NewMultiProviderKeyManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		[]KeyProvider{provider},
		KeyManagerConfig{HotReloadEnabled: false},
	)
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start(context.Background()))

	changes := make([]string, 0)
	m.OnKeyChange(func(addr string, added bool) {
		verb := "removed"
		if added {
			verb = "added"
		}
		changes = append(changes, verb+":"+addr)
	})

	return m, provider, &changes
}

// TestReloadIsSilentWhenNothingChanged is the precondition for driving reloads
// on a timer over sources that cannot be watched (the keyring cannot). The
// steady state is a reload that finds the same keys, and it must cost nothing a
// subscriber or an operator can see -- otherwise a quiet fleet produces a
// stream of events and the reload that mattered is buried in the ones that did
// not.
func TestReloadIsSilentWhenNothingChanged(t *testing.T) {
	m, _, changes := newReloadManager(t, map[string]cryptotypes.PrivKey{
		"pokt1unchanged": secp256k1.GenPrivKey(),
	})

	reloadsBefore := testutil.ToFloat64(keyReloadsTotal)

	for i := 0; i < 5; i++ {
		require.NoError(t, m.Reload(context.Background()))
	}

	require.Empty(t, *changes, "an unchanged reload woke the subscribers")
	require.Equal(t, reloadsBefore, testutil.ToFloat64(keyReloadsTotal),
		"an unchanged reload counted as a reload: the counter measures ticks, not changes")
	require.Equal(t, []string{"pokt1unchanged"}, m.ListSuppliers())
}

// TestReloadReportsKeysAddedAndRemovedInTheSameReload covers the shape of a
// real key file rewrite: one supplier arrives, one is pulled, one is untouched.
func TestReloadReportsKeysAddedAndRemovedInTheSameReload(t *testing.T) {
	const (
		kept    = "pokt1kept"
		pulled  = "pokt1pulled"
		arrived = "pokt1arrived"
	)
	keptKey := secp256k1.GenPrivKey()

	m, provider, changes := newReloadManager(t, map[string]cryptotypes.PrivKey{
		kept:   keptKey,
		pulled: secp256k1.GenPrivKey(),
	})

	reloadsBefore := testutil.ToFloat64(keyReloadsTotal)

	provider.setKeys(map[string]cryptotypes.PrivKey{
		kept:    keptKey,
		arrived: secp256k1.GenPrivKey(),
	})
	require.NoError(t, m.Reload(context.Background()))

	require.ElementsMatch(t, []string{"added:" + arrived, "removed:" + pulled}, *changes)
	require.Equal(t, reloadsBefore+1, testutil.ToFloat64(keyReloadsTotal))

	// The pulled key stops resolving; the untouched one keeps its material.
	_, err := m.GetSigner(pulled)
	require.Error(t, err, "the manager still serves a key that was pulled")
	signer, err := m.GetSigner(kept)
	require.NoError(t, err)
	require.True(t, signer.Equals(keptKey), "an untouched supplier's key changed under a reload")
}

// TestReloadToAnEmptyKeySetReportsTheRemovals is the boundary an operator
// reaches by emptying the key file: it must report the change, not treat "no
// keys left" as nothing worth saying.
func TestReloadToAnEmptyKeySetReportsTheRemovals(t *testing.T) {
	const addr = "pokt1last"
	m, provider, changes := newReloadManager(t, map[string]cryptotypes.PrivKey{
		addr: secp256k1.GenPrivKey(),
	})

	provider.setKeys(map[string]cryptotypes.PrivKey{})
	require.NoError(t, m.Reload(context.Background()))

	require.Equal(t, []string{"removed:" + addr}, *changes)
	require.Empty(t, m.ListSuppliers())
	_, err := m.GetSigner(addr)
	require.Error(t, err)
}

// TestPeriodicReloadPicksUpAChangeFromAnUnwatchableSource is the reason the
// timer exists rather than the watch alone.
//
// staticProvider reports SupportsHotReload() == false and returns nil from
// WatchForChanges, exactly like the keyring provider. Nothing will ever tell
// this manager that its keys changed, so on watches alone a key pulled here
// would never take effect. The timer notices anyway.
func TestPeriodicReloadPicksUpAChangeFromAnUnwatchableSource(t *testing.T) {
	const arrived = "pokt1arrivedbytimer"
	const initial = "pokt1initial"
	initialKey := secp256k1.GenPrivKey()

	provider := &staticProvider{keys: map[string]cryptotypes.PrivKey{
		initial: initialKey,
	}}
	m := NewMultiProviderKeyManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		[]KeyProvider{provider},
		KeyManagerConfig{HotReloadEnabled: true, ReloadInterval: 5 * time.Millisecond},
	)
	t.Cleanup(func() { _ = m.Close() })

	require.NoError(t, m.Start(context.Background()))
	require.False(t, provider.SupportsHotReload(), "this test is only meaningful for an unwatchable source")
	require.Nil(t, provider.WatchForChanges(context.Background()))

	// Subscribed after Start so the initial load -- which legitimately reports
	// every key as added -- is not mistaken for the change under test. No event
	// can be missed in between: a tick that finds the same keys is silent.
	seen := make(chan string, 8)
	m.OnKeyChange(func(addr string, added bool) {
		if added {
			seen <- addr
		}
	})

	provider.setKeys(map[string]cryptotypes.PrivKey{
		initial: initialKey,
		arrived: secp256k1.GenPrivKey(),
	})

	select {
	case addr := <-seen:
		require.Equal(t, arrived, addr)
	case <-time.After(10 * time.Second):
		t.Fatal("the periodic reload never noticed a key added to an unwatchable source")
	}
}
