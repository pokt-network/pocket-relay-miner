package pebblestore

import (
	"errors"
	"strings"
	"sync"

	"github.com/cockroachdb/pebble/vfs"
)

// walFS hands Pebble write-ahead log files whose syncs return at once, so a
// commit made with pebble.Sync costs a write() to the OS and no fsync: it
// survives a crash of the process. syncAll fsyncs those files for real, on the
// store's timer, which bounds what an OS crash can lose. Every other file
// (sstables, the MANIFEST, directories) keeps its real syncs.
type walFS struct {
	vfs.FS

	mu   sync.Mutex
	open map[*walFile]struct{}
}

func newWALFS(fs vfs.FS) *walFS {
	return &walFS{FS: fs, open: make(map[*walFile]struct{})}
}

func isWAL(name string) bool { return strings.HasSuffix(name, ".log") }

func (fs *walFS) Create(name string) (vfs.File, error) {
	f, err := fs.FS.Create(name)
	if err != nil || !isWAL(name) {
		return f, err
	}
	return fs.track(f), nil
}

func (fs *walFS) ReuseForWrite(oldname, newname string) (vfs.File, error) {
	f, err := fs.FS.ReuseForWrite(oldname, newname)
	if err != nil || !isWAL(newname) {
		return f, err
	}
	return fs.track(f), nil
}

func (fs *walFS) track(f vfs.File) *walFile {
	w := &walFile{File: f, fs: fs}
	fs.mu.Lock()
	fs.open[w] = struct{}{}
	fs.mu.Unlock()
	return w
}

// syncAll fsyncs every open log file: every commit that returned before it is
// durable once it returns.
func (fs *walFS) syncAll() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var errs []error
	for w := range fs.open {
		errs = append(errs, w.File.Sync())
	}
	return errors.Join(errs...)
}

// walFile is a log file whose syncs are left to walFS.syncAll.
type walFile struct {
	vfs.File
	fs *walFS
}

func (w *walFile) Sync() error     { return nil }
func (w *walFile) SyncData() error { return nil }
func (w *walFile) SyncTo(int64) (bool, error) {
	return true, nil
}

// Close fsyncs for real: Pebble closes a log when it rotates to the next one,
// whose records come after this one's, and a later fsync of the next log alone
// would leave this one's tail behind it.
func (w *walFile) Close() error {
	w.fs.mu.Lock()
	delete(w.fs.open, w)
	syncErr := w.File.Sync()
	w.fs.mu.Unlock()
	return errors.Join(syncErr, w.File.Close())
}
