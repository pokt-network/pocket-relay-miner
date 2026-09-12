package redis

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// PoolStatter is the one thing a PoolCollector needs from whoever owns a
// connection pool. *Client satisfies it through the embedded UniversalClient,
// whose interface carries PoolStats for standalone, sentinel and cluster alike.
type PoolStatter interface {
	PoolStats() *redis.PoolStats
}

// poolTotals is what this process's pools have done in total, across pools that
// are still live and pools that have been retired.
type poolTotals struct {
	hits       uint64
	misses     uint64
	timeouts   uint64
	waitCount  uint64
	unusable   uint64
	staleConns uint64
	waitNanos  int64
}

// fold adds what a pool did since it was last read, and remembers the reading.
//
// go-redis keeps these counters as uint32 and they WRAP: at a few thousand
// commands a second a busy pool wraps hits in days, and exporting the raw value
// makes Prometheus read the wrap as a counter reset, which silently zeroes
// every rate() over that window. Subtracting in uint32 gives the true delta
// across a wrap -- 5 - (2^32-10) is 15 -- and uint64 holds the running sum.
//
// WaitDurationNs is int64, not uint32, so it cannot wrap in any lifetime this
// process will see and its delta is taken directly.
func (t *poolTotals) fold(prev *redis.PoolStats, cur *redis.PoolStats) {
	t.hits += uint64(cur.Hits - prev.Hits)
	t.misses += uint64(cur.Misses - prev.Misses)
	t.timeouts += uint64(cur.Timeouts - prev.Timeouts)
	t.waitCount += uint64(cur.WaitCount - prev.WaitCount)
	t.unusable += uint64(cur.Unusable - prev.Unusable)
	t.staleConns += uint64(cur.StaleConns - prev.StaleConns)
	t.waitNanos += cur.WaitDurationNs - prev.WaitDurationNs
	*prev = *cur
}

// livePool is a pool being counted, plus the last reading taken from it.
type livePool struct {
	statter PoolStatter
	prev    redis.PoolStats
}

// PoolCollector exports one process's Redis connection-pool statistics, and is
// also the registry of which pools exist: pools are added and removed as they
// come and go, which is what makes it survive per-supplier clients that are
// created when a supplier is adopted and closed when it is released.
//
// It reads PoolStats on every scrape, so it costs nothing on the hot path.
//
// It is NOT registered from NewClient: fifteen test files and the redis CLI
// build clients, and a repeated MustRegister panics. The binaries register one
// collector each, at wiring time, into the shared registry.
type PoolCollector struct {
	mu    sync.Mutex
	live  map[string]*livePool
	total poolTotals

	hits       *prometheus.Desc
	misses     *prometheus.Desc
	timeouts   *prometheus.Desc
	waitCount  *prometheus.Desc
	unusable   *prometheus.Desc
	staleConns *prometheus.Desc
	waitSecs   *prometheus.Desc

	totalConns *prometheus.Desc
	idleConns  *prometheus.Desc
	pending    *prometheus.Desc
	pendingMax *prometheus.Desc
	pools      *prometheus.Desc
}

// NewPoolCollector builds a collector whose series carry component, which is
// the binary this pool belongs to ("relayer" or "miner"). There is deliberately
// NO per-supplier label: with a client per supplier that would be unbounded
// cardinality, so a fleet is reported summed, plus the worst pool where the sum
// would hide it.
func NewPoolCollector(component string) *PoolCollector {
	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(
			"ha_transport_redis_pool_"+name,
			help,
			nil,
			prometheus.Labels{"component": component},
		)
	}
	return &PoolCollector{
		live: make(map[string]*livePool),

		hits:       desc("hits_total", "Total times a free connection was found in the pool"),
		misses:     desc("misses_total", "Total times a free connection was NOT found in the pool"),
		timeouts:   desc("timeouts_total", "Total times waiting for a connection timed out"),
		waitCount:  desc("wait_count_total", "Total times a caller had to wait for a connection"),
		unusable:   desc("unusable_total", "Total times a connection was found to be unusable"),
		staleConns: desc("stale_conns_total", "Total stale connections removed from the pool"),
		waitSecs:   desc("wait_seconds_total", "Total time spent waiting for a connection, in seconds"),

		totalConns: desc("total_conns", "Connections held right now, summed over live pools"),
		idleConns:  desc("idle_conns", "Idle connections right now, summed over live pools"),
		pending:    desc("pending_requests", "Callers waiting for a connection right now, summed over live pools"),
		pendingMax: desc("pending_requests_max", "Callers waiting on the WORST single pool right now"),
		pools:      desc("pools", "Pools being counted right now"),
	}
}

// Add starts counting a pool under id.
//
// Adding an id that is already live folds what the old pool did and starts the
// new one from zero. That is not defensive: with a client per supplier the same
// id comes back whenever a supplier is re-adopted, and a fresh pool reads 0
// against a high prev, which in uint32 arithmetic is a delta of about 4.29
// billion -- a counter that jumps by that is worse than no counter at all.
func (c *PoolCollector) Add(id string, s PoolStatter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.live[id]; ok {
		c.foldFinalLocked(old)
	}
	c.live[id] = &livePool{statter: s}
}

// Remove stops counting a pool, keeping what it did.
//
// Call it BEFORE closing the client it belongs to: it takes one last reading,
// so the work between the final scrape and the teardown is not lost, and a
// closed client has nothing left to report.
func (c *PoolCollector) Remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.live[id]; ok {
		c.foldFinalLocked(old)
		delete(c.live, id)
	}
}

// foldFinalLocked reads a pool one last time into the process totals. The
// caller holds mu.
func (c *PoolCollector) foldFinalLocked(lp *livePool) {
	if cur := lp.statter.PoolStats(); cur != nil {
		c.total.fold(&lp.prev, cur)
	}
}

// Describe implements prometheus.Collector.
func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.hits, c.misses, c.timeouts, c.waitCount, c.unusable, c.staleConns, c.waitSecs,
		c.totalConns, c.idleConns, c.pending, c.pendingMax, c.pools,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
//
// The whole read-and-fold runs under the mutex: Gather calls Collect on its own
// goroutine and two scrapes can overlap, so without it both would read the same
// prev and one delta would be counted twice or lost. Metrics are sent to the
// channel after the lock is released, because sending can block on the reader.
func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	var totalConns, idleConns, pending, pendingMax float64
	for _, lp := range c.live {
		cur := lp.statter.PoolStats()
		if cur == nil {
			continue
		}
		c.total.fold(&lp.prev, cur)
		totalConns += float64(cur.TotalConns)
		idleConns += float64(cur.IdleConns)
		pending += float64(cur.PendingRequests)
		if p := float64(cur.PendingRequests); p > pendingMax {
			pendingMax = p
		}
	}
	totals := c.total
	pools := float64(len(c.live))
	c.mu.Unlock()

	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	gauge := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)
	}

	counter(c.hits, float64(totals.hits))
	counter(c.misses, float64(totals.misses))
	counter(c.timeouts, float64(totals.timeouts))
	counter(c.waitCount, float64(totals.waitCount))
	counter(c.unusable, float64(totals.unusable))
	counter(c.staleConns, float64(totals.staleConns))
	counter(c.waitSecs, float64(totals.waitNanos)/1e9)

	gauge(c.totalConns, totalConns)
	gauge(c.idleConns, idleConns)
	gauge(c.pending, pending)
	gauge(c.pendingMax, pendingMax)
	gauge(c.pools, pools)
}
