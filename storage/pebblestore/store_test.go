package pebblestore

import (
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// crash simulates an OS crash on a strict in-memory filesystem: everything not
// fsynced is lost, then the database is opened again.
func crash(t *testing.T, s *Store, fs *vfs.MemFS) *Store {
	t.Helper()
	fs.SetIgnoreSyncs(true)
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
	require.NoError(t, s.db.Close())
	fs.ResetToSyncedState()
	fs.SetIgnoreSyncs(false)
	reopened, err := Open(zerolog.Nop(), Config{Path: "db", FS: fs, SyncInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func openStrict(t *testing.T, interval time.Duration) (*Store, *vfs.MemFS) {
	t.Helper()
	fs := vfs.NewStrictMem()
	// The database directory has to exist durably before the test starts, as
	// it does on a disk that already holds it: otherwise the crash removes the
	// whole directory and every test reads "nothing kept" for that reason.
	require.NoError(t, fs.MkdirAll("db", 0o755))
	root, err := fs.OpenDir("")
	require.NoError(t, err)
	require.NoError(t, root.Sync())
	require.NoError(t, root.Close())
	s, err := Open(zerolog.Nop(), Config{Path: "db", FS: fs, SyncInterval: interval})
	require.NoError(t, err)
	return s, fs
}

func put(t *testing.T, s *Store, key, value string) {
	t.Helper()
	b := s.DB().NewBatch()
	require.NoError(t, b.Set([]byte(key), []byte(value), nil))
	require.NoError(t, s.Commit(b))
}

func has(t *testing.T, s *Store, key string) bool {
	t.Helper()
	_, closer, err := s.DB().Get([]byte(key))
	if err == pebble.ErrNotFound {
		return false
	}
	require.NoError(t, err)
	require.NoError(t, closer.Close())
	return true
}

// The control: the strict filesystem does lose an unsynced commit, so the
// tests below that find one kept are not looking at a filesystem that keeps
// everything.
func TestCrash_LosesWhatWasNeverSynced(t *testing.T) {
	s, fs := openStrict(t, time.Hour)
	put(t, s, "a", "1")

	s = crash(t, s, fs)

	require.False(t, has(t, s, "a"))
}

// The durability model rests on this: one fsync makes every batch committed
// before it durable, not only the last one.
func TestCrash_ASyncKeepsEveryEarlierCommit(t *testing.T) {
	s, fs := openStrict(t, time.Hour)
	put(t, s, "a", "1")
	put(t, s, "b", "2")
	require.NoError(t, s.Sync())
	put(t, s, "c", "3")

	s = crash(t, s, fs)

	require.True(t, has(t, s, "a"))
	require.True(t, has(t, s, "b"))
	require.False(t, has(t, s, "c"), "after the sync, unsynced again")
}

// The timer bounds what an OS crash loses.
func TestCrash_ThePeriodicSyncKeepsCommitsWithoutAnExplicitSync(t *testing.T) {
	s, fs := openStrict(t, 5*time.Millisecond)
	s.synced = make(chan struct{}, 1)
	put(t, s, "a", "1")
	<-s.synced

	s = crash(t, s, fs)

	require.True(t, has(t, s, "a"))
}

// A clean shutdown loses nothing, even on a strict filesystem.
func TestClose_ACleanShutdownLosesNothing(t *testing.T) {
	s, fs := openStrict(t, time.Hour)
	put(t, s, "a", "1")
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "idempotent")

	fs.ResetToSyncedState()
	reopened, err := Open(zerolog.Nop(), Config{Path: "db", FS: fs})
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	require.True(t, has(t, reopened, "a"))
}
