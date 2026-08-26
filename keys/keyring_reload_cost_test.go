package keys

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	parentDir, _ := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})
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
	// signing -- the failure mode that matters far more than the cost. Written
	// as a VALID second record rather than garbage: the first version wrote
	// "not a record" and guarded the assertion with `if err == nil`, so a
	// dependency that started erroring on an undecodable file would have
	// silently disarmed the only assertion that matters here while the test
	// still reported PASS.
	seedSecondKey(t, parentDir, "app2", secondAppHex)
	third, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, third, 2, "the second key must now be visible")
	require.NotSame(t, first[addr], third[addr],
		"the directory changed, so the keys must be re-read, not served from the cache")
}

// seedSecondKey imports an extra key into an existing on-disk test keyring.
func seedSecondKey(t *testing.T, parentDir, name, hexKey string) {
	t.Helper()
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	cdc := codec.NewProtoCodec(registry)
	kr, err := keyring.New("pocket", keyring.BackendTest, parentDir, nil, cdc)
	require.NoError(t, err)
	require.NoError(t, kr.ImportPrivKeyHex(name, hexKey, "secp256k1"))
}

// TestAKeyRotatedInPlaceDefeatsTheCache is the defect the reload cache
// introduced and the review caught: with a fingerprint of name:size:mtime, the
// dominant rotation shape -- same record name, new key material -- was invisible.
// Measured on cosmos-sdk v0.53.7: three different secp256k1 keys under one
// record name produced app.info of 732, 732 and 731 bytes, so size carries
// about one bit and mtime was the only discriminator. mtime is not one: rsync
// -a, cp -p, tar -x and kubectl cp all preserve the source mtime, and NFS/SMB
// volumes quantise it. The cache would then have served the PREVIOUS key for
// the life of the process -- "a replaced key keeps signing", reintroduced by an
// optimisation.
//
// The fingerprint hashes file CONTENTS, so this test writes a different key
// under the SAME name and leaves name, size and mtime free to collide.
func TestAKeyRotatedInPlaceDefeatsTheCache(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})
	p := newOnDiskProvider(t, parentDir)

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 1)
	var oldAddr string
	for a := range before {
		oldAddr = a
	}

	// Build a second keyring holding a DIFFERENT key under the same record
	// name, and move its files over the first -- an in-place rotation, exactly
	// what an operator re-templating a Secret or rsyncing a keyring produces.
	rotatedParent, rotatedRecords := newOnDiskTestKeyring(t, map[string]string{"app": secondAppHex})
	_ = rotatedParent
	entries, err := os.ReadDir(rotatedRecords)
	require.NoError(t, err)
	for _, e := range entries {
		data, rerr := os.ReadFile(filepath.Join(rotatedRecords, e.Name()))
		require.NoError(t, rerr)
		require.NoError(t, os.WriteFile(filepath.Join(recordsDir, e.Name()), data, 0o600))
	}
	// NOTE: this end-to-end test proves the rotation becomes VISIBLE, not that
	// the content hash is what makes it visible -- copying files also moves
	// their size and mtime, so name:size:mtime would very likely notice this
	// one too. The property that actually matters is isolated, with name, size
	// and mtime held identical by construction, in
	// TestTheFingerprintSeesAContentChangeThatNameSizeAndMtimeHide.

	after, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.NotContains(t, after, oldAddr,
		"the rotated-out key must be gone: serving it from the cache means the process keeps "+
			"signing with the key the operator replaced")
	require.Len(t, after, 1, "exactly the rotated-in key should be present")
}

// TestTheFingerprintSeesAContentChangeThatNameSizeAndMtimeHide isolates the
// property the reload cache depends on, and it is the test that bites when the
// fingerprint regresses to name:size:mtime.
//
// The file keeps its NAME, its exact SIZE and its exact MTIME; only the bytes
// change. That is not a contrived shape: measured on cosmos-sdk v0.53.7, three
// different secp256k1 keys under one record name produced app.info of 732, 732
// and 731 bytes, so a real rotation collides on size two times in three -- and
// rsync -a, cp -p, tar -x and kubectl cp all restore the source file's mtime,
// while NFS- and SMB-backed volumes quantise it. Under the old fingerprint such
// a rotation was invisible and the cache kept serving the replaced key.
func TestTheFingerprintSeesAContentChangeThatNameSizeAndMtimeHide(t *testing.T) {
	dir := t.TempDir()
	p := &KeyringProvider{
		logger:     logging.NewLoggerFromConfig(logging.DefaultConfig()),
		keyringDir: dir,
	}

	file := filepath.Join(dir, "app.info")
	before := bytes.Repeat([]byte("A"), 732)
	after := bytes.Repeat([]byte("B"), 732)
	require.Len(t, after, len(before), "the two payloads must be the same size for this test to mean anything")

	require.NoError(t, os.WriteFile(file, before, 0o600))
	statBefore, err := os.Stat(file)
	require.NoError(t, err)

	fp1, n1, err := p.keyringDirFingerprint()
	require.NoError(t, err)
	require.Equal(t, 1, n1)

	// Same name, same length, and the original mtime put back.
	require.NoError(t, os.WriteFile(file, after, 0o600))
	require.NoError(t, os.Chtimes(file, statBefore.ModTime(), statBefore.ModTime()))

	statAfter, err := os.Stat(file)
	require.NoError(t, err)
	require.Equal(t, statBefore.Size(), statAfter.Size(), "size must be unchanged, or this test proves nothing")
	require.Equal(t, statBefore.ModTime().UnixNano(), statAfter.ModTime().UnixNano(),
		"mtime must be unchanged, or this test proves nothing")

	fp2, _, err := p.keyringDirFingerprint()
	require.NoError(t, err)
	require.NotEqual(t, fp1, fp2,
		"the contents changed while name, size and mtime stayed identical: a fingerprint that "+
			"misses this lets the cache serve a key the operator has already replaced")
}

// TestAKeyringThatCannotBeReadMovesTheMetric pins the invariant keys/manager.go
// relies on: Reload counts nothing, on the stated grounds that "every provider
// already increments keyLoadErrors itself". A provider return that skips the
// metric therefore leaves ha_keys_load_errors_total flat while the process runs
// on stale keys, and per this repo's logging policy the alertable signal for
// that condition is the metric, not a repeating Error log.
func TestAKeyringThatCannotBeReadMovesTheMetric(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})
	p := newOnDiskProvider(t, parentDir)

	_, err := p.LoadKeys(context.Background())
	require.NoError(t, err)

	before := testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind()))
	require.NoError(t, os.RemoveAll(recordsDir))

	_, err = p.LoadKeys(context.Background())
	require.Error(t, err)
	require.Greater(t, testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind())), before,
		"an unreadable keyring must move ha_keys_load_errors_total: it is the only alertable "+
			"signal, since the manager deliberately counts nothing itself")
}

// TestRecordFilesThatYieldNoKeysAreNotAKeyRemoval covers the narrower half of
// the zero-keys problem. cosmos-sdk's keystore.MigrateAll SKIPS records it
// cannot decode -- printing to stderr and returning a nil error -- so a keyring
// whose files are all corrupt presents as "no keys, no error". Applying that as
// a removal is the same silent fleet-wide outage, reached from the other side.
func TestRecordFilesThatYieldNoKeysAreNotAKeyRemoval(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})
	p := newOnDiskProvider(t, parentDir)

	_, err := p.LoadKeys(context.Background())
	require.NoError(t, err)

	// Keep the directory and the file names; destroy only the contents.
	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, e := range entries {
		require.NoError(t, os.WriteFile(filepath.Join(recordsDir, e.Name()), []byte("corrupt"), 0o600))
	}

	keys, err := p.LoadKeys(context.Background())
	require.Error(t, err,
		"the directory still holds %d record file(s); none of them yielded a key, which is a "+
			"broken keyring and must not be applied as %d removals", len(entries), len(keys))
	require.Contains(t, err.Error(), "broken keyring")
}

// TestAnOnDiskProviderAppliesASingleKeyRemoval closes the coverage gap the
// review named: every pre-existing reload-semantics test builds the provider
// with NewKeyringProviderWithKeyring, where keyringDir is "" and both the cache
// and the zero-keys guard are switched OFF. So the assertions about what a
// reload does all ran down a branch production never takes.
func TestAnOnDiskProviderAppliesASingleKeyRemoval(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir)

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 2)

	// Remove one key's files, leaving the other intact: a partial removal, which
	// is what pulling one supplier's key actually looks like.
	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	removed := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "app2") {
			require.NoError(t, os.Remove(filepath.Join(recordsDir, e.Name())))
			removed++
		}
	}
	require.NotZero(t, removed, "the test removed nothing, so it asserts nothing")

	after, err := p.LoadKeys(context.Background())
	require.NoError(t, err, "a partial removal is the operator's own doing, not a fault")
	require.Len(t, after, 1, "exactly the pulled key must be gone")
}
