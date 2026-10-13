package pebblestore

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// gather scrapes c alone, as the process's registry would, by family name.
func gather(t *testing.T, c prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(c))
	families, err := reg.Gather()
	require.NoError(t, err)
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

// value is the single sample of a gauge or counter family.
func value(t *testing.T, families map[string]*dto.MetricFamily, name string) float64 {
	t.Helper()
	f, ok := families[name]
	require.True(t, ok, "%s is not in the scrape", name)
	require.Len(t, f.GetMetric(), 1, name)
	m := f.GetMetric()[0]
	switch f.GetType() {
	case dto.MetricType_GAUGE:
		return m.GetGauge().GetValue()
	case dto.MetricType_COUNTER:
		return m.GetCounter().GetValue()
	}
	t.Fatalf("%s has type %s", name, f.GetType())
	return 0
}

func syncCount(t *testing.T, families map[string]*dto.MetricFamily) uint64 {
	t.Helper()
	f, ok := families["ha_standalone_wal_sync_duration_seconds"]
	require.True(t, ok, "the sync histogram is not in the scrape")
	require.Len(t, f.GetMetric(), 1)
	return f.GetMetric()[0].GetHistogram().GetSampleCount()
}

func openDisk(t *testing.T, fs vfs.FS) *Store {
	t.Helper()
	s, err := Open(zerolog.Nop(), Config{Path: t.TempDir(), SyncInterval: time.Hour, FS: fs})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var lsmFamilies = []string{
	"ha_standalone_store_disk_used_bytes",
	"ha_standalone_store_memtable_bytes",
	"ha_standalone_store_l0_files",
	"ha_standalone_store_read_amplification",
	"ha_standalone_store_compaction_debt_bytes",
	"ha_standalone_store_flushes_total",
	"ha_standalone_store_compactions_total",
}

// The collector reports what Pebble reports, after writes, a flush and a
// compaction have changed it.
func TestCollector_ReportsTheDatabaseMetrics(t *testing.T) {
	s := openDisk(t, nil)
	c := s.Collector()

	for i := 0; i < 1000; i++ {
		put(t, s, fmt.Sprintf("key-%04d", i), strings.Repeat("v", 100))
	}
	require.NoError(t, s.DB().Flush())

	got := gather(t, c)
	want := s.DB().Metrics()
	for _, name := range lsmFamilies {
		_, ok := got[name]
		require.True(t, ok, "%s is not in the scrape", name)
	}
	require.Equal(t, float64(want.DiskSpaceUsage()), value(t, got, "ha_standalone_store_disk_used_bytes"))
	require.Greater(t, value(t, got, "ha_standalone_store_disk_used_bytes"), float64(100*1000)/10, "1000 values of 100 bytes take room even compressed")
	require.Equal(t, float64(want.MemTable.Size), value(t, got, "ha_standalone_store_memtable_bytes"))
	require.Greater(t, value(t, got, "ha_standalone_store_memtable_bytes"), 0.0, "an open database has a memtable")
	require.Equal(t, float64(1), value(t, got, "ha_standalone_store_l0_files"), "one flush, one sstable in L0")
	require.Equal(t, float64(want.ReadAmp()), value(t, got, "ha_standalone_store_read_amplification"))
	require.GreaterOrEqual(t, value(t, got, "ha_standalone_store_read_amplification"), 1.0, "the flushed sstable is a sublevel")
	require.Equal(t, float64(want.Compact.EstimatedDebt), value(t, got, "ha_standalone_store_compaction_debt_bytes"))
	require.GreaterOrEqual(t, value(t, got, "ha_standalone_store_flushes_total"), 1.0)
	require.Equal(t, float64(want.Flush.Count), value(t, got, "ha_standalone_store_flushes_total"))
	compactionsBefore := value(t, got, "ha_standalone_store_compactions_total")
	require.Equal(t, float64(want.Compact.Count), compactionsBefore)

	require.NoError(t, s.DB().Compact([]byte("key-"), []byte("key-~"), true))

	got = gather(t, c)
	require.Greater(t, value(t, got, "ha_standalone_store_compactions_total"), compactionsBefore)
	require.Equal(t, float64(0), value(t, got, "ha_standalone_store_l0_files"), "the compaction moved L0 down")
	require.Equal(t, float64(0), value(t, got, "ha_standalone_store_compaction_debt_bytes"), "nothing in L0, nothing to compact")

	// Data in L0 over data in the level below is debt: Pebble counts both.
	for i := 0; i < 1000; i++ {
		put(t, s, fmt.Sprintf("key-%04d", i), strings.Repeat("w", 100))
	}
	require.NoError(t, s.DB().Flush())
	got = gather(t, c)
	require.Greater(t, value(t, got, "ha_standalone_store_compaction_debt_bytes"), 0.0)
	require.Equal(t, float64(s.DB().Metrics().Compact.EstimatedDebt), value(t, got, "ha_standalone_store_compaction_debt_bytes"))
}

// A scrape after Close reads no database (Pebble panics on a closed one): the
// database's families are left out, the sync metrics stay.
func TestCollector_AClosedStoreReportsOnlyTheSyncMetrics(t *testing.T) {
	s, err := Open(zerolog.Nop(), Config{Path: t.TempDir(), SyncInterval: time.Hour})
	require.NoError(t, err)
	c := s.Collector()
	put(t, s, "a", "1")
	require.NoError(t, s.Close())

	got := gather(t, c)
	for _, name := range lsmFamilies {
		require.NotContains(t, got, name)
	}
	require.Equal(t, uint64(1), syncCount(t, got), "the sync Close made")
	require.Equal(t, float64(0), value(t, got, "ha_standalone_wal_sync_failures_total"))
}

// Every sync is timed, the periodic one included.
func TestSync_ThePeriodicSyncIsTimed(t *testing.T) {
	s, _ := openStrict(t, 5*time.Millisecond)
	t.Cleanup(func() { _ = s.Close() })
	s.synced = make(chan struct{}, 1)
	c := s.Collector()
	require.Equal(t, uint64(0), syncCount(t, gather(t, c)), "nothing synced yet")

	put(t, s, "a", "1")
	<-s.synced

	got := gather(t, c)
	require.GreaterOrEqual(t, syncCount(t, got), uint64(1))
	require.Equal(t, float64(0), value(t, got, "ha_standalone_wal_sync_failures_total"))
}

// A failed fsync of the log is counted, and timed like any other.
func TestSync_AFailedSyncIsCounted(t *testing.T) {
	fs := &failingSyncFS{FS: vfs.Default}
	s := openDisk(t, fs)
	c := s.Collector()
	put(t, s, "a", "1")

	require.NoError(t, s.Sync(), "the control: a sync that works counts no failure")
	got := gather(t, c)
	require.Equal(t, uint64(1), syncCount(t, got))
	require.Equal(t, float64(0), value(t, got, "ha_standalone_wal_sync_failures_total"))

	fs.fail.Store(true)
	put(t, s, "b", "2")
	err := s.Sync()
	require.ErrorIs(t, err, errInjectedSync)

	got = gather(t, c)
	require.Equal(t, uint64(2), syncCount(t, got))
	require.Equal(t, float64(1), value(t, got, "ha_standalone_wal_sync_failures_total"))

	fs.fail.Store(false)
	require.NoError(t, s.Sync())
	got = gather(t, c)
	require.Equal(t, uint64(3), syncCount(t, got))
	require.Equal(t, float64(1), value(t, got, "ha_standalone_wal_sync_failures_total"), "a later good sync does not undo the count")
}

var errInjectedSync = errors.New("injected sync failure")

// failingSyncFS fails the fsync of log files while fail is set.
type failingSyncFS struct {
	vfs.FS
	fail atomic.Bool
}

func (fs *failingSyncFS) Create(name string) (vfs.File, error) {
	f, err := fs.FS.Create(name)
	if err != nil || !isWAL(name) {
		return f, err
	}
	return &failingSyncFile{File: f, fs: fs}, nil
}

func (fs *failingSyncFS) ReuseForWrite(oldname, newname string) (vfs.File, error) {
	f, err := fs.FS.ReuseForWrite(oldname, newname)
	if err != nil || !isWAL(newname) {
		return f, err
	}
	return &failingSyncFile{File: f, fs: fs}, nil
}

type failingSyncFile struct {
	vfs.File
	fs *failingSyncFS
}

func (f *failingSyncFile) Sync() error {
	if f.fs.fail.Load() {
		return errInjectedSync
	}
	return f.File.Sync()
}
