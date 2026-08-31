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
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
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

// The variadic key names are not decoration: NewKeyringProvider is the only
// constructor that sets keyringDir, and keyringDir is what switches on both the
// reload cache and the broken-keyring guard. Until 2026-08-31 this helper took
// no names, so the suite could express "guard on, no key_names" and "key_names,
// guard off" but never the two together -- which is the shipping configuration
// (config/keys.go, schema-validated in both binaries). The defect that lived in
// that blind spot was not merely untested, it was UNREPRESENTABLE.
func newOnDiskProvider(t *testing.T, parentDir string, keyNames ...string) *KeyringProvider {
	t.Helper()
	p, err := NewKeyringProvider(logging.NewLoggerFromConfig(logging.DefaultConfig()),
		KeyringProviderConfig{Backend: "test", Dir: parentDir, AppName: "pocket",
			KeyNames: keyNames})
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

// The emptied-keyring test above removes EVERY file, which is not what deleting
// a key does. cosmos-sdk's file backend writes a "keyhash" beside the records on
// first unlock and never removes it -- not per key, not on the last one. So an
// operator who withdraws their only key leaves a directory holding exactly one
// file, and counting that file as a record made LoadKeys report a broken
// keyring: Reload then abandoned the reload, kept the previous key set, and the
// process went on signing with the withdrawn key, retrying every tick forever.
// Measured 2026-08-28. That is this branch's own failure mode, reintroduced by
// the guard that was added to prevent a different one.
func TestABackendBookkeepingFileIsNotAKeyRecord(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})
	p := newOnDiskProvider(t, parentDir)

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 1)

	// Remove the key RECORDS, the way `keys delete` does...
	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, os.Remove(filepath.Join(recordsDir, e.Name())))
	}
	// ...and leave the passphrase hash behind, the way the backend does.
	require.NoError(t, os.WriteFile(filepath.Join(recordsDir, "keyhash"), []byte("not-a-key"), 0o600))

	after, err := p.LoadKeys(context.Background())
	require.NoError(t, err,
		"a directory holding only the backend's own bookkeeping is an EMPTY keyring, "+
			"not a broken one -- reporting an error here keeps a withdrawn key signing")
	require.Empty(t, after, "the removal must be applied, or a pulled key keeps signing")
}

// The procedure docs/SUPPLIER_KEYS.md actually documents, end to end: remove the
// .info and leave everything else. It was measured on a four-pod fleet on
// 2026-08-22 and it works because cosmos-sdk lists a keyring by its .info files,
// so the orphaned .address is inert -- which is exactly why .address must not
// count as a record here either.
func TestTheDocumentedWithdrawalLeavesAnEmptyKeyring(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})
	p := newOnDiskProvider(t, parentDir)

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 1)

	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	removed := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".info") {
			require.NoError(t, os.Remove(filepath.Join(recordsDir, e.Name())))
			removed++
		}
	}
	require.Equal(t, 1, removed, "precondition: exactly one .info to withdraw")
	require.NoError(t, os.WriteFile(filepath.Join(recordsDir, "keyhash"), []byte("not-a-key"), 0o600))

	after, err := p.LoadKeys(context.Background())
	require.NoError(t, err, "the documented withdrawal must not read as a broken keyring")
	require.Empty(t, after, "the withdrawal must be applied, or a pulled key keeps signing")
}

// thirdAppHex gives these tests a fleet rather than a pair, so "one record is
// broken" can be told apart from "nothing decoded".
const thirdAppHex = "2b00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0c"

// corruptRecord overwrites a record's .info in place, leaving the file present.
// That is what an undecodable record looks like: cosmos-sdk's MigrateAll skips
// it, printing to stderr and returning a nil error.
func corruptRecord(t *testing.T, recordsDir, uidPrefix string) {
	t.Helper()
	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), uidPrefix) && strings.HasSuffix(e.Name(), ".info") {
			require.NoError(t, os.WriteFile(
				filepath.Join(recordsDir, e.Name()), []byte("corrupt"), 0o600))
			n++
		}
	}
	require.Equal(t, 1, n, "precondition: exactly one record matching %q was corrupted", uidPrefix)
}

// withdrawRecord removes a record's .info and nothing else, which is the
// withdrawal docs/SUPPLIER_KEYS.md documents and a four-pod fleet measured.
func withdrawRecord(t *testing.T, recordsDir, uidPrefix string) {
	t.Helper()
	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), uidPrefix) && strings.HasSuffix(e.Name(), ".info") {
			require.NoError(t, os.Remove(filepath.Join(recordsDir, e.Name())))
			n++
		}
	}
	require.Equal(t, 1, n, "precondition: exactly one record matching %q was withdrawn", uidPrefix)
}

// TestWithdrawingASelectedKeyIsNotABrokenKeyring is the defect the deep-review
// found and this branch measured on 2026-08-31. With key_names configured,
// LoadKeys attempts ONLY the named records, but the guard was comparing that
// result against every .info in the directory. Withdrawing the one selected key
// from a keyring that holds others therefore read as a broken keyring, and
// MultiProviderKeyManager.Reload abandoned the reload and kept the previous set.
func TestWithdrawingASelectedKeyIsNotABrokenKeyring(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir, "app2")

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 1, "precondition: key_names selects exactly one of the two records")

	withdrawRecord(t, recordsDir, "app2")

	after, err := p.LoadKeys(context.Background())
	require.NoError(t, err,
		"the unselected records were never attempted, so they cannot testify that "+
			"this keyring is broken")
	require.Empty(t, after, "the withdrawal must be applied, or a pulled key keeps signing")
}

// TestAWithdrawnSelectedKeyStopsSigning is the money half of the test above, and
// the reason it is worth its own test: the provider returning an error is not
// the damage. The damage is that Reload gives up on it, so the retired key stays
// in the running signer -- with no TTL, because the error path never advances
// the fingerprint, so every later tick fails identically.
func TestAWithdrawnSelectedKeyStopsSigning(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir, "app2")

	m := NewMultiProviderKeyManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		[]KeyProvider{p},
		KeyManagerConfig{HotReloadEnabled: false},
	)
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start(context.Background()))

	held := m.ListSuppliers()
	require.Len(t, held, 1, "precondition: the manager holds exactly the selected key")
	withdrawn := held[0]

	withdrawRecord(t, recordsDir, "app2")

	require.NoError(t, m.Reload(context.Background()))
	_, err := m.GetSigner(withdrawn)
	require.Error(t, err,
		"one reload after the withdrawal must retire the key: decideSupplierServe "+
			"asks HasSigner first, so a key still in the signer is a supplier still served")
}

// TestAWithdrawalBesideANonSigningRecordIsApplied covers the same defect on the
// OTHER branch, where key_names is not involved at all. A keyring may hold
// records that are not signing keys -- an offline pubkey, a multisig, a ledger
// entry -- and those return keyring.ErrPrivKeyExtr on every call, forever, so
// LoadKeys skips them by design. They still leave a .info on disk, so counting
// FILES kept the old guard's denominator above zero and withdrawing the last
// REAL key read as a broken keyring: the same freeze the keyhash file reached on
// 2026-08-28. Counting what List DECODED sees them for what they are -- List
// returns them fine; it is the private-key export that fails.
func TestAWithdrawalBesideANonSigningRecordIsApplied(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{"app": testAppHex})

	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	kr, err := keyring.New("pocket", keyring.BackendTest, parentDir, nil,
		codec.NewProtoCodec(registry))
	require.NoError(t, err)
	_, err = kr.SaveOfflineKey("watcher", secp256k1.GenPrivKey().PubKey())
	require.NoError(t, err, "precondition: cosmos-sdk accepts a pubkey-only record")

	p := newOnDiskProvider(t, parentDir)
	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 1, "precondition: the offline record yields no signing key")

	withdrawRecord(t, recordsDir, "app.")

	after, err := p.LoadKeys(context.Background())
	require.NoError(t, err,
		"a record that can never yield a private key must not testify that the "+
			"keyring is broken")
	require.Empty(t, after, "the withdrawal must be applied, or a pulled key keeps signing")
}

// TestPartialCorruptionIsReportedAndApplied pins the decision taken on
// 2026-08-31, and it is a decision rather than a deduction: an undecodable
// record means that supplier loses service, loudly, instead of the whole reload
// being refused.
//
// Refusing was tried and measured over the same afternoon. The condition is
// STABLE, so every later reload failed identically, the manager kept its
// previous key set, and a key withdrawn afterwards went on signing forever --
// one corrupt file freezing the withdrawal of every other. Keeping the address
// alive instead cannot be done honestly either: nothing can sign with a record
// it cannot decode, and a pod that restarts loads the same partial set with no
// previous state to protect, so the fleet would split between pods holding the
// address in memory and pods that never saw it.
//
// What must NOT come back is the silence: before 2026-08-31 this returned a
// shorter map with a nil error and moved no series at all.
func TestPartialCorruptionIsReportedAndApplied(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir)

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 2)

	errsBefore := testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind()))
	corruptRecord(t, recordsDir, "app2")

	after, err := p.LoadKeys(context.Background())
	require.NoError(t, err,
		"a per-record failure must not refuse the whole load: doing so freezes every "+
			"other key's reload for the life of the process")
	require.Len(t, after, 1, "the record that still decodes must still load")
	require.Greater(t, testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind())), errsBefore,
		"applying the removal is only defensible if it is visible")
	require.Equal(t, 1.0,
		testutil.ToFloat64(keyringUndecodableRecords.WithLabelValues(p.Kind())),
		"the standing condition needs a gauge: a counter cannot say how many records "+
			"are broken RIGHT NOW, which is what an operator has to act on")
}

// TestACorruptRecordDoesNotBlockTheWithdrawalOfAnother is the regression test
// for the freeze itself, and it is the one that would have gone red on
// 2026-08-31 before the fix: with a corrupt record present, the manager kept
// BOTH addresses across five reloads, including one the operator had withdrawn.
func TestACorruptRecordDoesNotBlockTheWithdrawalOfAnother(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
		"app3": thirdAppHex,
	})
	p := newOnDiskProvider(t, parentDir)
	m := NewMultiProviderKeyManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		[]KeyProvider{p}, KeyManagerConfig{HotReloadEnabled: false})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start(context.Background()))
	require.Len(t, m.ListSuppliers(), 3, "precondition: three keys held")

	corruptRecord(t, recordsDir, "app2")
	require.NoError(t, m.Reload(context.Background()),
		"one broken record is not a broken keyring while others still decode")
	require.Len(t, m.ListSuppliers(), 2)

	withdrawn := ""
	for _, addr := range m.ListSuppliers() {
		withdrawn = addr
		break
	}
	require.NotEmpty(t, withdrawn)

	withdrawRecord(t, recordsDir, "app.")
	require.NoError(t, m.Reload(context.Background()))
	require.Len(t, m.ListSuppliers(), 1,
		"the withdrawal must land while a corrupt record sits beside it -- a key that "+
			"cannot be pulled is the failure this whole guard exists to prevent")
}

// TestNothingDecodedIsRefused is the other half of the split, and the reason the
// split exists at all. A rotated or wrong passphrase makes every jose.Decode
// fail, so List returns an empty slice and a NIL error with not one corrupt byte
// on disk -- measured 2026-08-31. Applying that would diff every supplier as
// removed at once, which is a fleet-wide outage nobody performed, so a load
// where nothing decoded is refused however many records are on disk.
func TestNothingDecodedIsRefused(t *testing.T) {
	parentDir := t.TempDir()
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	kr, err := keyring.New("pocket", keyring.BackendFile, parentDir,
		strings.NewReader(testKeyringPassword+"\n"+testKeyringPassword+"\n"),
		codec.NewProtoCodec(registry))
	require.NoError(t, err)
	require.NoError(t, kr.ImportPrivKeyHex("app", testAppHex, "secp256k1"))
	require.NoError(t, kr.ImportPrivKeyHex("app2", secondAppHex, "secp256k1"))

	p, err := NewKeyringProvider(logging.NewLoggerFromConfig(logging.DefaultConfig()),
		KeyringProviderConfig{Backend: "file", Dir: parentDir, AppName: "pocket",
			PasswordReader: strings.NewReader(strings.Repeat("thewrongpassphrase\n", 40))})
	require.NoError(t, err)

	keys, err := p.LoadKeys(context.Background())
	require.Error(t, err,
		"a wrong passphrase decodes nothing and reports nothing: applying it removes "+
			"every supplier at once")
	require.Contains(t, err.Error(), "broken keyring")
	require.Empty(t, keys)
}

// TestTheGuardBehavesTheSameOnTheFileBackend exists because every other test
// around this guard uses the "test" backend, and production uses "file". The two
// differ by a passphrase and share cosmos-sdk's keystore decode path, so they
// SHOULD classify identically -- a claim about a dependency, which is the kind
// this file has been wrong about before, so it is measured rather than asserted.
//
// Each case seeds its own keyring: subtests that share one are ordered, and an
// ordered test cannot be run alone or in parallel.
func TestTheGuardBehavesTheSameOnTheFileBackend(t *testing.T) {
	// The passphrase is read ONCE per keyring and cached on the instance
	// (99designs/keyring v1.2.2, fileKeyring.unlock, file.go:53-67), so a reload
	// does not prompt again. The reader is generously long anyway: one that runs
	// dry reports as a WRONG passphrase, which is the very condition these tests
	// distinguish.
	seed := func(t *testing.T) (parentDir, recordsDir string) {
		t.Helper()
		parentDir = t.TempDir()
		registry := codectypes.NewInterfaceRegistry()
		cryptocodec.RegisterInterfaces(registry)
		kr, err := keyring.New("pocket", keyring.BackendFile, parentDir,
			strings.NewReader(testKeyringPassword+"\n"+testKeyringPassword+"\n"),
			codec.NewProtoCodec(registry))
		require.NoError(t, err)
		require.NoError(t, kr.ImportPrivKeyHex("app", testAppHex, "secp256k1"))
		require.NoError(t, kr.ImportPrivKeyHex("app2", secondAppHex, "secp256k1"))
		return parentDir, filepath.Join(parentDir, "keyring-file")
	}
	newFileProvider := func(t *testing.T, parentDir string, names ...string) *KeyringProvider {
		t.Helper()
		p, err := NewKeyringProvider(logging.NewLoggerFromConfig(logging.DefaultConfig()),
			KeyringProviderConfig{Backend: "file", Dir: parentDir, AppName: "pocket",
				KeyNames:       names,
				PasswordReader: strings.NewReader(strings.Repeat(testKeyringPassword+"\n", 8))})
		require.NoError(t, err)
		return p
	}

	t.Run("withdrawing the selected key is applied", func(t *testing.T) {
		parentDir, recordsDir := seed(t)
		p := newFileProvider(t, parentDir, "app2")
		before, err := p.LoadKeys(context.Background())
		require.NoError(t, err)
		require.Len(t, before, 1)

		withdrawRecord(t, recordsDir, "app2")

		after, err := p.LoadKeys(context.Background())
		require.NoError(t, err, "the file backend must not read a withdrawal as a broken keyring")
		require.Empty(t, after)
	})

	t.Run("partial corruption is applied and reported", func(t *testing.T) {
		parentDir, recordsDir := seed(t)
		p := newFileProvider(t, parentDir)
		before, err := p.LoadKeys(context.Background())
		require.NoError(t, err)
		require.Len(t, before, 2)

		corruptRecord(t, recordsDir, "app2")

		after, err := p.LoadKeys(context.Background())
		require.NoError(t, err, "a swallowed record is per-record on either backend")
		require.Len(t, after, 1)
	})
}

// TestAKeyringRebuiltUnderNewNamesIsAppliedButNotSilent pins the second half of
// the 2026-08-31 decision. With key_names configured, "the operator withdrew the
// selected key" and "the keyring was rebuilt with records under different uids"
// are the same observable state -- every named lookup returns ErrKeyNotFound
// with records still on disk -- so the removal is applied rather than refused,
// because refusing froze the reload and kept a withdrawn key signing.
//
// The part that had to change is the silence: this returned an empty map and a
// nil error, so every supplier was released while ha_keys_load_errors_total
// stayed flat and nothing anywhere said so.
func TestAKeyringRebuiltUnderNewNamesIsAppliedButNotSilent(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir, "app2")

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 1)

	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	renamed := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".info") {
			require.NoError(t, os.Rename(
				filepath.Join(recordsDir, e.Name()),
				filepath.Join(recordsDir, "rebuilt-"+e.Name())))
			renamed++
		}
	}
	require.Equal(t, 2, renamed, "precondition: every record now carries a different uid")

	errsBefore := testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind()))
	after, err := p.LoadKeys(context.Background())

	require.NoError(t, err,
		"this is indistinguishable from the documented withdrawal of the selected "+
			"key, and refusing it is what froze the reload")
	require.Empty(t, after)
	require.Greater(t, testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind())), errsBefore,
		"releasing every supplier this process served must move the alertable series")
}

// TestAProviderWithoutADirectoryPublishesNoShortfall pins the fix for an
// arithmetic hole rather than a behaviour. The gauge is a shortfall between two
// counts, and a provider built without a keyring directory has no count for one
// side: it published 0 - N, measured as -1 on 2026-08-31, on a series whose help
// text reads "records present on disk that the keyring could not decode".
//
// The assertion is that the series carries NO SAMPLE, not that it reads zero.
// Zero is what an absent sample and a published zero both look like through
// ToFloat64, and only one of them is the invariant: with no directory there is
// nothing to compare against, so there is no number to report.
//
// The vec is reset first because it is process-global and every other test in
// this file writes it.
func TestAProviderWithoutADirectoryPublishesNoShortfall(t *testing.T) {
	parentDir, _ := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	withDir := newOnDiskProvider(t, parentDir)

	keyringUndecodableRecords.Reset()
	t.Cleanup(keyringUndecodableRecords.Reset)

	p := NewKeyringProviderWithKeyring(
		logging.NewLoggerFromConfig(logging.DefaultConfig()), withDir.keyring, nil)
	keys, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, keys, 2, "precondition: the directory-less provider still loads")

	require.Zero(t, testutil.CollectAndCount(keyringUndecodableRecords),
		"a provider with no directory has nothing to count against, so it must "+
			"publish no sample at all")
}

// TestWithdrawingEVERYRecordIsNotSilent covers the maximal form of a release,
// which the first version of the loud path excluded by asking whether records
// were still on disk. Withdrawing every .info leaves a readable directory with
// no records at all: every supplier released,
// and measured on 2026-08-31 as moving nothing, in the very commit whose message
// said releasing every supplier must never be silent.
func TestWithdrawingEVERYRecordIsNotSilent(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir)

	before, err := p.LoadKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, before, 2)

	entries, err := os.ReadDir(recordsDir)
	require.NoError(t, err)
	removed := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".info") {
			require.NoError(t, os.Remove(filepath.Join(recordsDir, e.Name())))
			removed++
		}
	}
	require.Equal(t, 2, removed, "precondition: every record withdrawn")

	errsBefore := testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind()))
	after, err := p.LoadKeys(context.Background())

	require.NoError(t, err, "an emptied keyring is the operator's own doing and is applied")
	require.Empty(t, after)
	require.Greater(t, testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind())), errsBefore,
		"releasing EVERY supplier is the loudest thing this provider can do, so it "+
			"cannot be the quietest")
}

// TestOneLoadMovesTheErrorSeriesOnce guards a counting defect the manager
// already paid for on its own side: it stopped counting per failing provider
// because "one bad key moved the series by two". Two conditions here are
// reachable from a single load -- a corrupt record, and a load that yielded no
// signing keys -- and each used to increment.
func TestOneLoadMovesTheErrorSeriesOnce(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	kr, err := keyring.New("pocket", keyring.BackendTest, parentDir, nil,
		codec.NewProtoCodec(registry))
	require.NoError(t, err)
	_, err = kr.SaveOfflineKey("watcher", secp256k1.GenPrivKey().PubKey())
	require.NoError(t, err, "precondition: a record that can never yield a private key")

	p := newOnDiskProvider(t, parentDir)
	_, err = p.LoadKeys(context.Background())
	require.NoError(t, err)

	// Both signing records go undecodable; only the non-signing one still lists.
	corruptRecord(t, recordsDir, "app.")
	corruptRecord(t, recordsDir, "app2")

	before := testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind()))
	keys, err := p.LoadKeys(context.Background())
	require.NoError(t, err, "one record still decodes, so this is per-record, not a broken keyring")
	require.Empty(t, keys, "precondition: both conditions hold at once")

	require.Equal(t, 1.0, testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind()))-before,
		"two load-level conditions coinciding are one cause, so they count once; the "+
			"per-key increments in the loops are a different contract and stay per key")
}

// TestAFrozenReloadNeverGoesQuiet pins the decision taken on 2026-08-31: when a
// key source REPORTS that it could not be read, the reload is abandoned and the
// previous keys are kept, for as long as the condition lasts. Nothing is guessed
// and nothing is released on a source that said it could not be read.
//
// That policy is only defensible while it keeps saying so. A frozen manager and
// a healthy idle one differ in exactly one thing an operator can see, and it is
// this: the failure repeats, every tick, in the log and in the counter. The
// obvious future optimisation -- caching the fingerprint on the error path so a
// stable failure stops re-running argon2 over every healthy key -- would silence
// it and leave a process that holds withdrawn keys while looking well.
func TestAFrozenReloadNeverGoesQuiet(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir, "app", "app2")
	m := NewMultiProviderKeyManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		[]KeyProvider{p}, KeyManagerConfig{HotReloadEnabled: false})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start(context.Background()))
	require.Len(t, m.ListSuppliers(), 2, "precondition: both selected keys held")

	corruptRecord(t, recordsDir, "app2")

	base := testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind()))
	const ticks = 5
	for i := 1; i <= ticks; i++ {
		require.Error(t, m.Reload(context.Background()),
			"tick %d: a source that reported a failure must keep reporting it", i)
		require.Equal(t, float64(i),
			testutil.ToFloat64(keyLoadErrors.WithLabelValues(p.Kind()))-base,
			"tick %d: every attempt moves the series, or a stuck process looks idle", i)
	}

	require.Len(t, m.ListSuppliers(), 2,
		"the previous keys are KEPT while the source is unreadable -- that is the "+
			"policy, not a bug, and the operator repairs the record")
}
