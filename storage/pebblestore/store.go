// Package pebblestore is the embedded store the standalone subcommand keeps its
// state in instead of Redis: one Pebble database per process, shared by the
// relayer and the miner.
//
// Durability model. Every write is a pebble.Batch committed without an fsync:
// it is in the write-ahead log, in the OS page cache, the moment Commit
// returns, so a crash of this PROCESS loses nothing. What an OS crash or a
// power loss can lose is the tail of the log since the last fsync, and the log
// is replayed as a prefix: a batch that survives implies every earlier batch
// survived, and a batch is all or nothing. Sync fsyncs the log, which also makes
// every earlier unsynced batch durable; the store calls it on a timer
// (SyncInterval) so that tail is bounded, and the miner calls it where a lost
// tail would cost more than rewards (before a claim is broadcast).
package pebblestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// DefaultSyncInterval bounds what an OS crash can lose: the batches committed
// since the last fsync. One second, the same order as the batch intervals the
// relayer and miner already flush on.
const DefaultSyncInterval = time.Second

// DefaultCacheBytes is Pebble's block cache when the config sets none.
const DefaultCacheBytes = 256 << 20

// Config opens a store.
type Config struct {
	// Path is the directory the database lives in.
	Path string
	// SyncInterval is how often the log is fsynced while there were writes.
	// Zero means DefaultSyncInterval.
	SyncInterval time.Duration
	// CacheBytes is the block cache size. Zero means DefaultCacheBytes.
	CacheBytes int64
	// FS replaces the filesystem; tests pass vfs.NewStrictMem to simulate a
	// crash. Nil means the disk.
	FS vfs.FS
}

// Store is an open database.
type Store struct {
	db     *pebble.DB
	logger logging.Logger

	dirty     atomic.Bool
	stop      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	// synced, when not nil, receives after every periodic sync. Tests only.
	synced chan struct{}
}

// Open opens or creates the database at cfg.Path and starts the periodic sync.
func Open(logger logging.Logger, cfg Config) (*Store, error) {
	if cfg.Path == "" {
		return nil, errors.New("pebblestore: path is required")
	}
	interval := cfg.SyncInterval
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	cacheBytes := cfg.CacheBytes
	if cacheBytes <= 0 {
		cacheBytes = DefaultCacheBytes
	}

	cache := pebble.NewCache(cacheBytes)
	defer cache.Unref() // the DB holds its own reference
	opts := &pebble.Options{
		Cache:  cache,
		FS:     cfg.FS,
		Logger: pebbleLogger{logger: logger},
	}
	db, err := pebble.Open(cfg.Path, opts)
	if err != nil {
		return nil, fmt.Errorf("pebblestore: open %s: %w", cfg.Path, err)
	}

	s := &Store{
		db:     db,
		logger: logging.ForComponent(logger, "pebblestore"),
		stop:   make(chan struct{}),
	}
	s.wg.Add(1)
	go logging.RecoverGoRoutine(s.logger, "pebblestore_sync", func(context.Context) {
		defer s.wg.Done()
		s.syncLoop(interval)
	})(context.Background())
	return s, nil
}

// DB is the database, for the packages that keep their state in it.
func (s *Store) DB() *pebble.DB { return s.db }

// Commit commits b without an fsync (see the package doc) and closes it.
func (s *Store) Commit(b *pebble.Batch) error {
	err := b.Commit(pebble.NoSync)
	_ = b.Close()
	if err != nil {
		return fmt.Errorf("pebblestore: commit: %w", err)
	}
	s.dirty.Store(true)
	return nil
}

// Sync fsyncs the log: every batch committed before it is durable once it
// returns.
func (s *Store) Sync() error {
	s.dirty.Store(false)
	if err := s.db.LogData(nil, pebble.Sync); err != nil {
		s.dirty.Store(true)
		return fmt.Errorf("pebblestore: sync: %w", err)
	}
	return nil
}

func (s *Store) syncLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if !s.dirty.Load() {
				continue
			}
			if err := s.Sync(); err != nil {
				s.logger.Warn().Err(err).Msg("periodic sync failed; retrying on the next tick")
				continue
			}
			if s.synced != nil {
				s.synced <- struct{}{}
			}
		}
	}
}

// Close stops the periodic sync, fsyncs what is left and closes the database.
// Idempotent.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.stopOnce.Do(func() { close(s.stop) })
		s.wg.Wait()
		syncErr := s.Sync()
		s.closeErr = errors.Join(syncErr, s.db.Close())
	})
	return s.closeErr
}

// pebbleLogger routes Pebble's own messages to the process logger. Pebble's
// Info lines (compactions, flushes) are routine, so they go to Debug.
type pebbleLogger struct{ logger logging.Logger }

func (l pebbleLogger) Infof(format string, args ...interface{}) {
	l.logger.Debug().Str(logging.FieldComponent, "pebble").Msgf(format, args...)
}

// Fatalf is how Pebble reports a state it cannot continue past (corruption, a
// failed WAL write). Pebble's contract is that it does not return; its own
// default logger exits the process, and so does this one, after the message is
// in the process log. Returning would leave the miner writing to a database
// Pebble has given up on.
func (l pebbleLogger) Fatalf(format string, args ...interface{}) {
	l.logger.Error().Str(logging.FieldComponent, "pebble").Msgf(format, args...)
	os.Exit(1)
}
