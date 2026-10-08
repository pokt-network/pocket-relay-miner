// Package pebblestore is the embedded store the standalone subcommand keeps its
// state in instead of Redis: one Pebble database per process, shared by the
// relayer and the miner.
//
// Durability model. Every write is a pebble.Batch committed with pebble.Sync,
// but the write-ahead log's syncs are left to a timer (walFS): when Commit
// returns, the batch has been written to the OS, so a crash of this PROCESS
// loses nothing. What an OS crash or a power loss can lose is the tail of the
// log since the last real fsync, and the log is replayed as a prefix: a batch
// that survives implies every earlier batch survived, and a batch is all or
// nothing. Sync fsyncs the log for real; the store calls it every SyncInterval
// so that tail is bounded, and callers call it where a lost tail would cost more
// than rewards.
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
	"github.com/prometheus/client_golang/prometheus"

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
	wal    *walFS
	fs     vfs.FS
	path   string
	logger logging.Logger

	dirty     atomic.Bool
	stop      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	// openMu guards closed: IfOpen holds it to read, Close to set it, so a
	// reader outside the process's own lifecycle (a metrics scrape) never
	// reaches a closed database, which Pebble answers with a panic.
	openMu sync.RWMutex
	closed bool

	// syncSeconds and syncFailures describe every fsync of the log. They are
	// the store's own, not package-level, so they are served only where a
	// store is open (standalone mode), through Collector.
	syncSeconds  prometheus.Histogram
	syncFailures prometheus.Counter

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

	fs := cfg.FS
	if fs == nil {
		fs = vfs.Default
	}
	wal := newWALFS(fs)

	cache := pebble.NewCache(cacheBytes)
	defer cache.Unref() // the DB holds its own reference
	opts := &pebble.Options{
		Cache:  cache,
		FS:     wal,
		Logger: pebbleLogger{logger: logger},
	}
	db, err := pebble.Open(cfg.Path, opts)
	if err != nil {
		return nil, fmt.Errorf("pebblestore: open %s: %w", cfg.Path, err)
	}

	s := &Store{
		db:     db,
		wal:    wal,
		fs:     fs,
		path:   cfg.Path,
		logger: logging.ForComponent(logger, "pebblestore"),
		stop:   make(chan struct{}),
		syncSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "wal_sync_duration_seconds",
			Help:      "Duration of one fsync of the embedded store's write-ahead log: the periodic one and those asked for, failed ones included",
			Buckets:   []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}),
		syncFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "wal_sync_failures_total",
			Help:      "Fsyncs of the embedded store's write-ahead log that failed; until one succeeds, an OS crash can lose every batch committed since the last good one",
		}),
	}
	s.wg.Add(1)
	go logging.RecoverGoRoutine(s.logger, "pebblestore_sync", func(context.Context) {
		defer s.wg.Done()
		s.syncLoop(interval)
	})(context.Background())
	return s, nil
}

// DiskUsage is the disk the database lives on: total bytes, and used as total
// minus what this process can still write (reserved blocks count as used).
func (s *Store) DiskUsage() (used, total uint64, err error) {
	u, err := s.fs.GetDiskUsage(s.path)
	if err != nil {
		return 0, 0, fmt.Errorf("pebblestore: disk usage of %s: %w", s.path, err)
	}
	if u.AvailBytes > u.TotalBytes {
		return 0, u.TotalBytes, nil
	}
	return u.TotalBytes - u.AvailBytes, u.TotalBytes, nil
}

// DB is the database, for the packages that keep their state in it.
func (s *Store) DB() *pebble.DB { return s.db }

// Commit commits b to the OS, without an fsync (see the package doc), and
// closes it.
func (s *Store) Commit(b *pebble.Batch) error {
	err := b.Commit(pebble.Sync)
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
	start := time.Now()
	err := s.wal.syncAll()
	s.syncSeconds.Observe(time.Since(start).Seconds())
	if err != nil {
		s.syncFailures.Inc()
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
		s.openMu.Lock()
		s.closed = true
		s.openMu.Unlock()
		s.stopOnce.Do(func() { close(s.stop) })
		s.wg.Wait()
		syncErr := s.Sync()
		s.closeErr = errors.Join(syncErr, s.db.Close())
	})
	return s.closeErr
}

// IfOpen runs fn and returns true while the store is open; once Close has
// begun it returns false without running fn. Close waits for a running fn, so
// fn may read the database. For readers that can run during shutdown, such as
// a metrics scrape.
func (s *Store) IfOpen(fn func()) bool {
	s.openMu.RLock()
	defer s.openMu.RUnlock()
	if s.closed {
		return false
	}
	fn()
	return true
}

// Collector exports the store's state for Prometheus: the WAL sync metrics,
// and the database's own metrics read from Pebble at scrape time. Reading them
// at scrape time needs no goroutine and is never staler than the scrape;
// db.Metrics copies counters under Pebble's mutex and is cheap at a scrape
// interval. Register it only where the store is the process's store
// (standalone mode).
func (s *Store) Collector() prometheus.Collector {
	c := &storeCollector{store: s}
	c.lsm = []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "store_disk_used_bytes",
			Help:      "Disk the embedded store's files take: logs, sstables, obsolete files not yet deleted, the manifest and compactions in progress",
		}, func() float64 { return float64(c.m.Load().DiskSpaceUsage()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "store_memtable_bytes",
			Help:      "Bytes allocated by the embedded store's memtables, the writes not yet flushed to sstables",
		}, func() float64 { return float64(c.m.Load().MemTable.Size) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "store_l0_files",
			Help:      "Sstables in level 0 of the embedded store; a count that keeps growing means compactions do not keep up with writes, and Pebble slows writes past its limit",
		}, func() float64 { return float64(c.m.Load().Levels[0].NumFiles) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "store_read_amplification",
			Help:      "Read amplification of the embedded store: the L0 sublevels plus the non-empty levels below L0, the sstables a read may have to look at",
		}, func() float64 { return float64(c.m.Load().ReadAmp()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "store_compaction_debt_bytes",
			Help:      "Pebble's estimate of the bytes the embedded store must compact to reach a stable shape",
		}, func() float64 { return float64(c.m.Load().Compact.EstimatedDebt) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "store_flushes_total",
			Help:      "Memtable flushes of the embedded store since the process opened it",
		}, func() float64 { return float64(c.m.Load().Flush.Count) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "store_compactions_total",
			Help:      "Compactions of the embedded store since the process opened it",
		}, func() float64 { return float64(c.m.Load().Compact.Count) }),
	}
	return c
}

// storeCollector reads db.Metrics once per scrape into m; the functions of lsm
// read the latest m. The registry calls them after Collect returns, so m is
// never cleared: two scrapes at once each read a snapshot no older than their
// own.
type storeCollector struct {
	store *Store
	lsm   []prometheus.Collector
	m     atomic.Pointer[pebble.Metrics]
}

func (c *storeCollector) Describe(ch chan<- *prometheus.Desc) {
	c.store.syncSeconds.Describe(ch)
	c.store.syncFailures.Describe(ch)
	for _, m := range c.lsm {
		m.Describe(ch)
	}
}

func (c *storeCollector) Collect(ch chan<- prometheus.Metric) {
	c.store.syncSeconds.Collect(ch)
	c.store.syncFailures.Collect(ch)
	// A closed store has no database to read: its families are left out of
	// the scrape rather than reported as zero.
	if !c.store.IfOpen(func() { c.m.Store(c.store.db.Metrics()) }) {
		return
	}
	for _, m := range c.lsm {
		m.Collect(ch)
	}
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
