package keys

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// secondAppHex is a second well-formed secp256k1 key, so these tests can tell
// "the keyring emptied" from "one key went away".
const secondAppHex = "1c00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0b"

// newOnDiskTestKeyring seeds a real cosmos-sdk "test" backend keyring under a
// temp dir and returns the PARENT directory (what KeyringProviderConfig.Dir
// wants) and the records directory cosmos-sdk actually writes into.
func newOnDiskTestKeyring(t *testing.T, hexKeys map[string]string) (parentDir, recordsDir string) {
	t.Helper()
	parentDir = t.TempDir()
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	cdc := codec.NewProtoCodec(registry)

	kr, err := keyring.New("pocket", keyring.BackendTest, parentDir, nil, cdc)
	require.NoError(t, err)
	for name, hexKey := range hexKeys {
		require.NoError(t, kr.ImportPrivKeyHex(name, hexKey, "secp256k1"))
	}

	recordsDir = filepath.Join(parentDir, "keyring-test")
	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err, "cosmos-sdk should have written records into %s", recordsDir)
	require.NotEmpty(t, entries, "the seeded keyring wrote no files")
	return parentDir, recordsDir
}

func newOnDiskProvider(t *testing.T, parentDir string) *KeyringProvider {
	t.Helper()
	p, err := NewKeyringProvider(logging.NewLoggerFromConfig(logging.DefaultConfig()),
		KeyringProviderConfig{Backend: "test", Dir: parentDir, AppName: "pocket"})
	require.NoError(t, err)
	return p
}

// TestAVanishedKeyringDirectoryIsAnErrorNotAnEmptyKeySet is the HIGH the
// 2026-08-22 review found. 99designs/keyring v1.2.2 discards the ReadDir error
// (file.go:174) and recreates a missing directory (file.go:44-49), so a deleted
// or remounted keyring comes back as zero keys with a NIL error. The manager's
// "an unreadable source is not a key removal" guard keys off that error, so with
// nil it applied the empty set: every supplier diffed as removed, the relayer
// rejecting every relay while /ready still answered true.
func TestAVanishedKeyringDirectoryIsAnErrorNotAnEmptyKeySet(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir)

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 2, "the seeded keyring must load before the directory is removed")

	require.NoError(t, os.RemoveAll(recordsDir), "simulating a deleted/remounted keyring")

	after, err := p.LoadKeys(context.Background())
	require.Error(t, err,
		"a keyring directory that cannot be read must be an ERROR: returning %d keys with nil "+
			"tells the manager the operator removed every key", len(after))
	require.Contains(t, err.Error(), recordsDir, "the error must name the directory the operator has to fix")
}

// TestAGenuinelyEmptiedKeyringStillAppliesTheRemovals pins the distinction the
// blunt fix would have destroyed. Refusing every reload that reaches zero keys
// would have been simpler and would have broken the feature this branch exists
// for: a single-key deployment whose operator pulls its only key must stop
// signing, not keep signing until someone restarts it.
func TestAGenuinelyEmptiedKeyringStillAppliesTheRemovals(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})
	p := newOnDiskProvider(t, parentDir)

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 1)

	// Empty the directory but leave it readable: the operator removed the keys.
	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, os.Remove(filepath.Join(recordsDir, e.Name())))
	}

	after, err := p.LoadKeys(context.Background())
	require.NoError(t, err, "an empty but READABLE keyring directory is the operator's own doing, not a fault")
	require.Empty(t, after, "the removal must be applied, or a pulled key keeps signing")
}

// TestAnUnchangedKeyringSkipsTheArgon2Work is the second HIGH: reading one key
// runs argon2id twice (cosmos-sdk crypto/armor.go:165 and :223, t=1 m=64MiB
// p=4), measured at 40.5 ms and ~128 MiB transient per key on the machine this
// was written on -- 24 s per reload at 594 keys against a 30 s timer.
//
// Asserted by POINTER IDENTITY rather than by timing, which would be flaky:
// UnarmorDecryptPrivKey allocates a fresh PrivKey every time, so the same
// pointer can only mean the derivation was skipped, and a different pointer can
// only mean it ran.
func TestAnUnchangedKeyringSkipsTheArgon2Work(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})
	p := newOnDiskProvider(t, parentDir)

	first, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, first, 1)
	var addr string
	for a := range first {
		addr = a
	}

	second, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Same(t, first[addr], second[addr],
		"nothing changed on disk, so the key must come back from the cache -- a new object means "+
			"argon2 ran twice per key again on a no-op tick")

	// A change on disk must defeat the cache, or a pulled key would keep
	// signing -- the failure mode that matters far more than the cost.
	require.NoError(t, os.WriteFile(filepath.Join(recordsDir, "app2.info"), []byte("not a record"), 0o600))
	third, err := p.LoadKeys(context.Background())
	if err == nil {
		require.NotSame(t, first[addr], third[addr],
			"the directory changed, so the keys must be re-read, not served from the cache")
	}
}
