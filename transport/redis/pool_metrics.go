package redis

import (
	"math"
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

// addAllPools sums the instantaneous gauges over the THREE pools a client can
// hold, not just the main one.
//
// PoolStats() carries all three: the main pool's fields inline, PubSubStats
// always, and PipelineStats when a pipeline pool exists. Reading only the main
// pool made pub/sub connections invisible -- and this process holds several,
// because block events and cache invalidation are pub/sub.
//
// PipelineStats is nil in this repository today, and that is the interesting
// part rather than an omission: go-redis builds a separate pipeline pool only
// when PipelineReadBufferSize OR PipelineWriteBufferSize is set, and nothing
// here sets either. So pipelined writes -- including every TxPipelined batch --
// take their connections from the MAIN pool and are already counted below. If
// somebody sets either field, pipelines move to their own pool and this is what
// keeps them visible.
// PubSubStats contributes to the CONNECTION COUNT only. Its shape is
// {Created, Untracked, Active} -- a pub/sub connection is held by a subscriber
// for as long as the subscription lives, so there is no idle list to report and
// nothing queues for one. Adding Active to the total is what makes those
// connections stop being invisible; inventing an idle or pending term for them
// would be reporting a number the pool does not have.
func addAllPools(cur *redis.PoolStats) (total, idle, pending float64) {
	total = float64(cur.TotalConns) + float64(cur.PubSubStats.Active)
	idle = float64(cur.IdleConns)
	pending = float64(cur.PendingRequests)
	if cur.PipelineStats != nil {
		total += float64(cur.PipelineStats.TotalConns)
		idle += float64(cur.PipelineStats.IdleConns)
		pending += float64(cur.PipelineStats.PendingRequests)
	}
	return total, idle, pending
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

		hits:     desc("hits_total", "Total times a free connection was found in the pool"),
		misses:   desc("misses_total", "Total times a free connection was NOT found in the pool"),
		timeouts: desc("timeouts_total", "Total times waiting for a connection timed out"),
		// The two wait series below count ONLY waits that ENDED IN A CONNECTION.
		// go-redis returns early when the wait times out (internal/pool/pool.go,
		// the `if err != nil { return err }` before waitDurationNs.Add and
		// WaitCount), so an expired wait lands in neither. Two consequences an
		// operator has to know at 3am: the mean is biased low, because the
		// longest waits are the ones excluded; and it IMPROVES when the pool
		// starts timing out, because those waits stop being counted while
		// timeouts_total climbs. Read them beside timeouts_total, which has no
		// such bias, and never alone.
		waitCount: desc("wait_count_total",
			"Total times a caller waited for a connection AND GOT ONE (expired waits are not counted -- see timeouts_total)"),
		unusable:   desc("unusable_total", "Total times a connection was found to be unusable"),
		staleConns: desc("stale_conns_total", "Total stale connections removed from the pool"),
		waitSecs: desc("wait_seconds_total",
			"Total seconds spent on waits THAT GOT A CONNECTION (expired waits are not counted, so this mean falls as the pool starts failing -- read with timeouts_total)"),

		totalConns: desc("total_conns", "Connections held right now, summed over live pools"),
		idleConns:  desc("idle_conns", "Idle connections right now, summed over live pools"),
		pending:    desc("pending_requests", "Callers waiting for a connection right now, summed over live pools"),
		pendingMax: desc("pending_requests_max", "Callers waiting on the WORST single pool right now"),
		pools:      desc("pools", "Pools being counted right now"),
	}
}

// RegisterEffectivePoolGauges publishes what the RUNNING client's pool is set
// to, read from the client on every scrape rather than captured from config at
// startup.
//
// Two series and not one: the pool SIZE is what every capacity conversation is
// about, and the pool TIMEOUT is the deadline a saturated pool fails against --
// and it is the one nobody set, so it is go-redis's 6s while this repo's
// comments said 4s and "waits forever". Publishing them from the client is what
// makes the number in a dashboard the number the process runs on.
//
// When the client type cannot be asked, both publish NaN rather than 0. A zero
// pool timeout reads as "no limit" and a zero pool size reads as "unbounded",
// so a failed read would look like the safest possible configuration. NaN
// leaves a gap in the graph, which is what not knowing looks like.
func RegisterEffectivePoolGauges(reg prometheus.Registerer, component string, c *Client) error {
	labels := prometheus.Labels{"component": component}

	size := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "ha_transport_redis_pool_size_effective",
		Help:        "Pool size the running client holds (NaN when the client type cannot be asked)",
		ConstLabels: labels,
	}, func() float64 {
		eff, ok := c.EffectivePoolOptions()
		if !ok {
			return math.NaN()
		}
		return float64(eff.PoolSize)
	})

	timeout := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "ha_transport_redis_pool_timeout_seconds_effective",
		Help:        "Seconds a caller waits for a connection before failing, as the running client holds it (NaN when it cannot be asked)",
		ConstLabels: labels,
	}, func() float64 {
		eff, ok := c.EffectivePoolOptions()
		if !ok {
			return math.NaN()
		}
		return eff.PoolTimeout.Seconds()
	})

	if err := reg.Register(size); err != nil {
		return err
	}
	return reg.Register(timeout)
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
		t, i, p := addAllPools(cur)
		totalConns += t
		idleConns += i
		pending += p
		if p > pendingMax {
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
