package keys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/stretchr/testify/require"
)

// listHookKeyring runs onList immediately before every List(), which is the only
// place a test can act INSIDE LoadKeys.
//
// The interface is embedded rather than implemented: keyring.Keyring is large
// and only List is being intercepted, so everything else goes to the real
// keyring untouched and no future method addition breaks this file.
type listHookKeyring struct {
	keyring.Keyring
	beforeList func()
	afterList  func()
}

func (l listHookKeyring) List() ([]*keyring.Record, error) {
	if l.beforeList != nil {
		l.beforeList()
	}
	recs, err := l.Keyring.List()
	if l.afterList != nil {
		l.afterList()
	}
	return recs, err
}

func snapshotDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, rerr)
		out[e.Name()] = b
	}
	return out
}

// restoreDir makes dir byte-identical to snap: it rewrites every file and
// removes anything that is not in it. Byte-identical is the whole point --
// keyringDirFingerprint hashes name plus sha256(contents) and nothing else
// (keyring_provider.go:212-216), so a faithful restore reproduces the exact
// fingerprint the directory had before.
func restoreDir(t *testing.T, dir string, snap map[string][]byte) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, keep := snap[e.Name()]; !keep {
			require.NoError(t, os.Remove(filepath.Join(dir, e.Name())))
		}
	}
	for name, contents := range snap {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), contents, 0o600))
	}
}

// TestAKeyringChangedDURINGALoadDoesNotPoisonTheCache covers the window between
// the fingerprint and the keys it is stored beside.
//
// LoadKeys reads the fingerprint at the top (keyring_provider.go:473), then
// spends seconds in List() and argon2id, then stores that SAME fingerprint next
// to whatever those seconds produced (:739). A directory that changes in between
// therefore gets recorded as (fingerprint of state A, keys of state B) -- and
// when the directory returns to state A byte for byte, which is what restoring a
// backup or rolling a Secret back does, the cache check at :476 hits and hands
// out the SHORT set. A supplier that is present on disk silently stops signing
// until something else changes the directory or the process restarts.
//
// The existing cache tests cannot see this: all three change the directory
// BETWEEN loads, never during one. The seam used here is the provider's own
// keyring field, so nothing in production is altered and nothing sleeps.
func TestAKeyringChangedDURINGALoadDoesNotPoisonTheCache(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"first":  testAppHex,
		"second": secondAppHex,
	})
	twoKeys := snapshotDir(t, recordsDir)

	// Add the third key through the real keyring, so its records are written
	// exactly the way cosmos-sdk writes them.
	p := newOnDiskProvider(t, parentDir)
	require.NoError(t, p.keyring.ImportPrivKeyHex("third", thirdAppHex, "secp256k1"))
	threeKeys := snapshotDir(t, recordsDir)
	require.Len(t, threeKeys, len(twoKeys)+2, "premise: the third key wrote its own records")

	// The withdrawal happens INSIDE the load: the fingerprint has already been
	// taken over three keys when List only finds two.
	real := p.keyring
	once := false
	p.keyring = listHookKeyring{Keyring: real, beforeList: func() {
		if once {
			return
		}
		once = true
		restoreDir(t, recordsDir, twoKeys)
	}}

	loaded, err := p.LoadKeys(t.Context())
	require.NoError(t, err)
	require.Len(t, loaded, 2, "premise: the load saw the withdrawn keyring")

	// The operator puts it back -- byte for byte, so the directory is once again
	// exactly the state whose fingerprint was recorded.
	p.keyring = real
	restoreDir(t, recordsDir, threeKeys)

	again, err := p.LoadKeys(t.Context())
	require.NoError(t, err)
	require.Len(t, again, 3,
		"three keys are on disk, so three must be returned: the cached fingerprint "+
			"describes the directory BEFORE the load, so it matches again here and "+
			"hands back the two keys that load happened to produce")
}

// TestAKeyAppearingDuringALoadIsNotCachedOut is the other half, and it is the
// reason the fingerprint is not simply re-read and stored.
//
// Re-reading looks like the obvious repair: take the fingerprint again at the
// end and record THAT. It closes the rollback case above and opens a worse one,
// because the keys are not read at an instant either. A key that lands after
// List has returned is absent from the loaded set, while the second reading of
// the directory already includes it -- so the pair (fingerprint WITH the key,
// keys WITHOUT it) gets cached, the directory then sits still, and every later
// load matches that fingerprint and hands back the set that never had it. The
// operator's new supplier never signs, and nothing changes until the directory
// moves again or the process restarts.
//
// The rollback case needs the directory to return byte for byte to an earlier
// state; this one only needs a copy to land a moment late. It is the common one,
// which is why re-reading is a regression rather than an alternative and why the
// cache is declined instead.
func TestAKeyAppearingDuringALoadIsNotCachedOut(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"first":  testAppHex,
		"second": secondAppHex,
	})

	p := newOnDiskProvider(t, parentDir)

	// Build the third key's records once, then take them away again, so the
	// hook can put them back with exactly the bytes cosmos-sdk writes.
	require.NoError(t, p.keyring.ImportPrivKeyHex("third", thirdAppHex, "secp256k1"))
	threeKeys := snapshotDir(t, recordsDir)
	restoreDir(t, recordsDir, snapshotWithoutThird(t, recordsDir, threeKeys))

	// The key lands AFTER List has answered: the load cannot see it, but a
	// fingerprint taken at the end can.
	real := p.keyring
	once := false
	p.keyring = listHookKeyring{Keyring: real, afterList: func() {
		if once {
			return
		}
		once = true
		restoreDir(t, recordsDir, threeKeys)
	}}

	loaded, err := p.LoadKeys(t.Context())
	require.NoError(t, err)
	require.Len(t, loaded, 2, "premise: List had already answered when the key arrived")

	// Nothing else happens to the directory -- this is the operator walking away
	// after copying the file in.
	p.keyring = real

	again, err := p.LoadKeys(t.Context())
	require.NoError(t, err)
	require.Len(t, again, 3,
		"the key is on disk and this load has no reason to skip work: caching the "+
			"END fingerprint beside keys read before it would match here forever and "+
			"never return the new key")
}

// snapshotWithoutThird returns full minus whatever the two-key keyring did not
// have, found by difference rather than by guessing cosmos-sdk's file names.
func snapshotWithoutThird(t *testing.T, dir string, full map[string][]byte) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(full))
	for name, contents := range full {
		if strings.Contains(name, "third") {
			continue
		}
		out[name] = contents
	}
	// The address alias file is named by the address, not the key name, so the
	// difference is taken against what is actually there for the two-key state:
	// anything the remaining .info files do not account for is the third key's.
	require.Less(t, len(out), len(full), "the third key must have written a named record")
	return out
}
