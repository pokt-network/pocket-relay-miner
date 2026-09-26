//go:build test

package redis

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// fakeStatter stands in for a pool. The counters go-redis exposes are what this
// collector has to reason about, and the interesting values (a uint32 about to
// wrap) cannot be produced by a real pool inside a test.
type fakeStatter struct {
	mu    sync.Mutex
	stats redis.PoolStats
}

func (f *fakeStatter) PoolStats() *redis.PoolStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stats
	return &s
}

func (f *fakeStatter) set(mutate func(s *redis.PoolStats)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(&f.stats)
}

// gatherValue reads one metric by name from a registry, failing if the family
// is missing -- "the series is absent" and "the series is zero" must never be
// the same signal.
func gatherValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		require.Len(t, f.GetMetric(), 1, "%s must have exactly one series", name)
		m := f.GetMetric()[0]
		if m.Counter != nil {
			return m.Counter.GetValue()
		}
		if m.Gauge != nil {
			return m.Gauge.GetValue()
		}
		t.Fatalf("%s is neither a counter nor a gauge", name)
	}
	t.Fatalf("metric %s is not in the registry", name)
	return 0
}

func newTestPoolCollector(t *testing.T) (*PoolCollector, *prometheus.Registry) {
	t.Helper()
	c := NewPoolCollector("miner")
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	return c, reg
}

// TestPoolCollector_CounterWrapAddsTheDelta is the point of the whole item.
// go-redis counts in uint32 and those counters WRAP; exporting the raw value
// makes Prometheus read a wrap as a counter reset and silently zero every rate
// over that window.
func TestPoolCollector_CounterWrapAddsTheDelta(t *testing.T) {
	c, reg := newTestPoolCollector(t)
	pool := &fakeStatter{}
	c.Add("shared", pool)

	// Park the pool just below the wrap and take a reading.
	pool.set(func(s *redis.PoolStats) { s.Hits = 1<<32 - 10 })
	before := gatherValue(t, reg, "ha_transport_redis_pool_hits_total")

	// Ten more hits reach the wrap, five more pass it: fifteen in total.
	pool.set(func(s *redis.PoolStats) { s.Hits = 5 })
	after := gatherValue(t, reg, "ha_transport_redis_pool_hits_total")

	require.Equal(t, float64(15), after-before, "a wrap must add the real delta, not reset the counter")
}

// TestPoolCollector_WaitDurationIsSeconds: WaitDurationNs is int64, so it takes
// no wrap handling -- but it does need converting, or the series would read
// nanoseconds under a name that says seconds.
func TestPoolCollector_WaitDurationIsSeconds(t *testing.T) {
	c, reg := newTestPoolCollector(t)
	pool := &fakeStatter{}
	c.Add("shared", pool)

	pool.set(func(s *redis.PoolStats) { s.WaitDurationNs = 1_500_000_000 })
	require.InDelta(t, 1.5, gatherValue(t, reg, "ha_transport_redis_pool_wait_seconds_total"), 1e-9)
}

// TestPoolCollector_RemoveKeepsWhatThePoolDid: a pool going away must never
// lower a counter. A counter that goes down reads as a process restart and
// ruins every rate() across it.
func TestPoolCollector_RemoveKeepsWhatThePoolDid(t *testing.T) {
	c, reg := newTestPoolCollector(t)
	pool := &fakeStatter{}
	c.Add("supplier-a", pool)

	pool.set(func(s *redis.PoolStats) { s.Hits = 7 })
	require.Equal(t, float64(7), gatherValue(t, reg, "ha_transport_redis_pool_hits_total"))

	// Two more hits land after the last scrape but before the teardown: Remove
	// takes a final reading, so they are not lost either.
	pool.set(func(s *redis.PoolStats) { s.Hits = 9 })
	c.Remove("supplier-a")

	require.Equal(t, float64(9), gatherValue(t, reg, "ha_transport_redis_pool_hits_total"))
	require.Equal(t, float64(0), gatherValue(t, reg, "ha_transport_redis_pool_pools"))
}

// TestPoolCollector_ReaddingAfterRemoveContinuesTheTotal covers the CLEAN path:
// a supplier released and then adopted again. Remove has already dropped the id,
// so the Add below takes the empty branch -- the defect that Add itself has to
// guard against is in TestPoolCollector_AddingALiveIDFoldsItFirst, which is the
// path that reaches it.
func TestPoolCollector_ReaddingAfterRemoveContinuesTheTotal(t *testing.T) {
	c, reg := newTestPoolCollector(t)

	first := &fakeStatter{}
	c.Add("supplier-a", first)
	first.set(func(s *redis.PoolStats) { s.Hits = 7 })
	require.Equal(t, float64(7), gatherValue(t, reg, "ha_transport_redis_pool_hits_total"))
	c.Remove("supplier-a")

	// The supplier is adopted again: a brand new pool, counting from zero.
	second := &fakeStatter{}
	c.Add("supplier-a", second)
	require.Equal(t, float64(7), gatherValue(t, reg, "ha_transport_redis_pool_hits_total"),
		"a fresh pool under a reused id must not add a wrap-sized jump")

	second.set(func(s *redis.PoolStats) { s.Hits = 3 })
	require.Equal(t, float64(10), gatherValue(t, reg, "ha_transport_redis_pool_hits_total"),
		"and it must go on counting from where the total was")
}

// TestPoolCollector_AddingALiveIDFoldsItFirst reaches the branch in Add that
// handles an id which is STILL live -- a supplier re-adopted without being
// released first: a rebalance that hands the supplier back to this miner, or a
// reconnect that rebuilds the client. Nothing calls Remove in between, so the
// old pool is still in the map when the new one arrives.
//
// Two things have to hold at once, and each fails a different way:
//   - what the OLD pool did after the last scrape is kept (hits move from 7 to
//     9 below with no gather in between, so an Add that does not fold loses 2);
//   - the NEW pool starts from zero (it reads 0, and against the old pool's
//     prev of 9 that is uint32(0-9), about 4.29 billion added to a monotonic
//     counter).
func TestPoolCollector_AddingALiveIDFoldsItFirst(t *testing.T) {
	c, reg := newTestPoolCollector(t)

	first := &fakeStatter{}
	c.Add("supplier-a", first)
	first.set(func(s *redis.PoolStats) { s.Hits = 7 })
	require.Equal(t, float64(7), gatherValue(t, reg, "ha_transport_redis_pool_hits_total"))

	// Two more hits AFTER the last scrape: without this the old pool has nothing
	// left unaccounted, and an Add that forgets to fold would look correct.
	first.set(func(s *redis.PoolStats) { s.Hits = 9 })

	// Re-adopted with no Remove: the id is still live.
	second := &fakeStatter{}
	c.Add("supplier-a", second)
	require.Equal(t, float64(9), gatherValue(t, reg, "ha_transport_redis_pool_hits_total"),
		"Add must fold what the live pool did before replacing it, and must not "+
			"measure the fresh pool against the old pool's prev")

	second.set(func(s *redis.PoolStats) { s.Hits = 3 })
	require.Equal(t, float64(12), gatherValue(t, reg, "ha_transport_redis_pool_hits_total"),
		"and the new pool goes on counting from the total")

	require.Equal(t, float64(1), gatherValue(t, reg, "ha_transport_redis_pool_pools"),
		"re-adding an id replaces its pool, it does not add a second one")
}

// TestPoolCollector_GaugesSumLivePoolsAndReportTheWorst: with a pool per
// supplier the SUM hides the one pool that is drowning, which is exactly the
// case a per-supplier client creates.
func TestPoolCollector_GaugesSumLivePoolsAndReportTheWorst(t *testing.T) {
	c, reg := newTestPoolCollector(t)

	calm := &fakeStatter{}
	drowning := &fakeStatter{}
	c.Add("supplier-calm", calm)
	c.Add("supplier-drowning", drowning)

	calm.set(func(s *redis.PoolStats) {
		s.TotalConns, s.IdleConns, s.PendingRequests = 3, 2, 0
	})
	drowning.set(func(s *redis.PoolStats) {
		s.TotalConns, s.IdleConns, s.PendingRequests = 3, 0, 11
	})

	require.Equal(t, float64(6), gatherValue(t, reg, "ha_transport_redis_pool_total_conns"))
	require.Equal(t, float64(2), gatherValue(t, reg, "ha_transport_redis_pool_idle_conns"))
	require.Equal(t, float64(11), gatherValue(t, reg, "ha_transport_redis_pool_pending_requests"))
	require.Equal(t, float64(11), gatherValue(t, reg, "ha_transport_redis_pool_pending_requests_max"))
	require.Equal(t, float64(2), gatherValue(t, reg, "ha_transport_redis_pool_pools"))
}

// TestPoolCollector_ConcurrentGathersAreSafe: Gather calls Collect on its own
// goroutine, so two scrapes can overlap. Run under -race.
func TestPoolCollector_ConcurrentGathersAreSafe(t *testing.T) {
	c, reg := newTestPoolCollector(t)
	pool := &fakeStatter{}
	c.Add("shared", pool)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, err := reg.Gather()
				require.NoError(t, err)
			}
		}()
	}
	// Pools come and go while the scrapes run, as they will with a client per
	// supplier.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			other := &fakeStatter{}
			hits := uint32(j)
			other.set(func(s *redis.PoolStats) { s.Hits = hits })
			c.Add("churn", other)
			c.Remove("churn")
		}
	}()
	wg.Wait()
}

// TestPoolCollector_SeriesCarryTheComponentLabel: the two binaries share one
// registry, so without this label their pools would be indistinguishable.
func TestPoolCollector_SeriesCarryTheComponentLabel(t *testing.T) {
	c := NewPoolCollector("relayer")
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	c.Add("shared", &fakeStatter{})

	families, err := reg.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, families)

	seen := 0
	for _, f := range families {
		require.Contains(t, f.GetName(), "ha_transport_redis_pool_")
		for _, m := range f.GetMetric() {
			require.Len(t, m.GetLabel(), 1)
			require.Equal(t, "component", m.GetLabel()[0].GetName())
			require.Equal(t, "relayer", m.GetLabel()[0].GetValue())
			seen++
		}
	}
	require.Equal(t, 12, seen, "every series the collector describes must be emitted")
}
