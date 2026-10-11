package query

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/puzpuzpuz/xsync/v4"
	"golang.org/x/sync/singleflight"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// heightKey names one chain value at one height: a service's value (relay
// mining difficulty, compute units per relay) or, with an empty id, a module's
// params.
type heightKey struct {
	id     string
	height int64
}

// heightEntry is a chain answer for a heightKey.
type heightEntry[V any] struct {
	value V
	// answeredAt is the height the node answered from (the
	// x-cosmos-block-height response header); 0 when the header was absent.
	answeredAt int64
	// seenAt is this process's block height when the answer was stored. An
	// answer that is not final is asked again once that height moves.
	seenAt    int64
	fetchedAt time.Time
}

// heightFetch asks the chain for key's value and returns it with the height the
// node answered from (0 when unknown).
type heightFetch[V any] func(ctx context.Context, key heightKey) (V, int64, error)

// blockHeightSource is this process's own view of the chain height, shared by
// the height caches of one Clients. It decides only WHEN an answer that is not
// final is asked again, never whether an answer is final: that comes from the
// node's own answer height.
type blockHeightSource struct {
	fn atomic.Pointer[func() int64]
}

func (s *blockHeightSource) current() int64 {
	if fn := s.fn.Load(); fn != nil {
		return (*fn)()
	}
	return 0
}

// heightCache holds chain values keyed by (id, height).
//
// Every value it holds is written by the chain as history: an update is
// effective from some height on and is never effective before the block that
// writes it. So once the node has committed height h, the value at h is final,
// and an answer is final when the node answered from a height at or above h.
// An answer from below h (the node lags, or the caller is ahead) can still be
// rewritten by a block the node has not committed; it is served but held apart
// and asked again once this process sees a new block. The one exception is an
// upgrade handler, which can rewrite past values: every final entry is asked
// again after a floor, keeps being served meanwhile, and a different answer is
// counted.
type heightCache[V any] struct {
	logger       logging.Logger
	cacheType    string
	queryTimeout time.Duration
	fetch        heightFetch[V]
	equal        func(a, b V) bool
	blockHeight  *blockHeightSource

	final      *xsync.Map[heightKey, heightEntry[V]]
	pending    *xsync.Map[heightKey, heightEntry[V]]
	maxFinal   int
	maxPending int
	evictMu    sync.Mutex
	flight     singleflight.Group

	// floor is how old a final entry gets before it is asked again; read from
	// immutableCacheTTLFloor at construction.
	floor time.Duration

	hits, misses, changed, heightMissing prometheus.Counter
	size, pendingSize                    prometheus.Gauge
	heightMissingOnce                    sync.Once

	// onFlightJoin, when set, is called once a caller has joined the RPC for a
	// key. Tests use it to know every caller shares the RPC before it answers.
	onFlightJoin func()
}

func newHeightCache[V any](
	logger logging.Logger,
	client, cacheType string,
	maxFinal int,
	queryTimeout time.Duration,
	blockHeight *blockHeightSource,
	fetch heightFetch[V],
	equal func(a, b V) bool,
) *heightCache[V] {
	return &heightCache[V]{
		logger:        logger,
		cacheType:     cacheType,
		queryTimeout:  queryTimeout,
		fetch:         fetch,
		equal:         equal,
		blockHeight:   blockHeight,
		final:         xsync.NewMap[heightKey, heightEntry[V]](),
		pending:       xsync.NewMap[heightKey, heightEntry[V]](),
		maxFinal:      maxFinal,
		maxPending:    maxPendingHeightCacheEntries,
		floor:         immutableCacheTTLFloor,
		hits:          queryCacheHits.WithLabelValues(client, cacheType),
		misses:        queryCacheMisses.WithLabelValues(client, cacheType),
		changed:       queryCacheFinalAnswerChanged.WithLabelValues(client, cacheType),
		heightMissing: queryCacheAnswerHeightMissing.WithLabelValues(client, cacheType),
		size:          queryCacheSize.WithLabelValues(client, cacheType),
		pendingSize:   queryCachePending.WithLabelValues(client, cacheType),
	}
}

// maxPendingHeightCacheEntries bounds the answers held as not final. Legit
// callers ask at most a block or two above the committed height, so a few per
// key in use is all a healthy process holds; the bound is for callers asking
// heights that do not exist yet.
const maxPendingHeightCacheEntries = 256

// get returns key's value from the cache, or asks the chain.
func (c *heightCache[V]) get(ctx context.Context, key heightKey) (V, error) {
	v, _, err := c.getWithFinality(ctx, key)
	return v, err
}

// getWithFinality is get, also saying whether the value is final: whether a
// cache in front of this one may keep it.
func (c *heightCache[V]) getWithFinality(ctx context.Context, key heightKey) (V, bool, error) {
	if v, final, ok := c.cached(key); ok {
		return v, final, nil
	}
	return c.ask(ctx, key)
}

// cached returns key's value when the cache may serve it: a final answer, or an
// answer that is not final while this process has seen no newer block since it
// arrived. Allocation-free: it runs on every relay.
func (c *heightCache[V]) cached(key heightKey) (v V, final bool, ok bool) {
	if e, found := c.final.Load(key); found {
		c.hits.Inc()
		if time.Since(e.fetchedAt) >= c.floor {
			c.claimRevalidation(key, e.fetchedAt)
		}
		return e.value, true, true
	}
	if e, found := c.pending.Load(key); found {
		if b := c.blockHeight.current(); b > 0 && b <= e.seenAt {
			c.hits.Inc()
			return e.value, false, true
		}
	}
	return v, false, false
}

// heightAnswer is what one RPC for a key returns to every caller sharing it.
type heightAnswer[V any] struct {
	value V
	final bool
}

// ask asks the chain for key's value. Concurrent callers for one key share one
// RPC, which runs detached from the caller that started it and bounded by the
// query timeout, so one caller giving up does not fail the others; each caller
// stops waiting when its own context ends. A failed RPC fails every caller that
// shared it, and nothing is stored.
func (c *heightCache[V]) ask(ctx context.Context, key heightKey) (V, bool, error) {
	c.misses.Inc()
	ch := c.flight.DoChan(flightKey(key), func() (any, error) {
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.queryTimeout)
		defer cancel()
		v, answeredAt, err := c.fetch(queryCtx, key)
		if err != nil {
			return nil, err
		}
		return heightAnswer[V]{value: v, final: c.store(key, v, answeredAt)}, nil
	})
	if c.onFlightJoin != nil {
		c.onFlightJoin()
	}
	var zero V
	select {
	case r := <-ch:
		if r.Err != nil {
			return zero, false, r.Err
		}
		a := r.Val.(heightAnswer[V])
		return a.value, a.final, nil
	case <-ctx.Done():
		return zero, false, ctx.Err()
	}
}

// store keeps an answer and reports whether it is final.
func (c *heightCache[V]) store(key heightKey, v V, answeredAt int64) bool {
	if answeredAt <= 0 {
		c.heightMissing.Inc()
		c.heightMissingOnce.Do(func() {
			c.logger.Warn().Str("cache_type", c.cacheType).
				Msg("the full node's answers carry no x-cosmos-block-height header (a proxy may strip it); no answer can be cached as final, so each is asked again once per block")
		})
	}
	e := heightEntry[V]{value: v, answeredAt: answeredAt, seenAt: c.blockHeight.current(), fetchedAt: time.Now()}
	final := answeredAt >= key.height
	if final {
		c.final.Store(key, e)
		c.pending.Delete(key)
		if c.final.Size() > c.maxFinal {
			c.evict(c.final, c.maxFinal, false)
		}
		c.size.Set(float64(c.final.Size()))
	} else {
		c.pending.Store(key, e)
		if c.pending.Size() > c.maxPending {
			c.evict(c.pending, c.maxPending, true)
		}
	}
	c.pendingSize.Set(float64(c.pending.Size()))
	return final
}

// claimRevalidation starts one re-validation of a final entry past the floor.
// Restamping the entry's fetch time is the claim: the lookup that restamps it
// starts the RPC, and every other lookup sees a fresh entry again, so the hits
// that keep arriving while the RPC is in flight cost nothing. A failed
// re-validation is then retried after another floor.
func (c *heightCache[V]) claimRevalidation(key heightKey, stale time.Time) {
	won := false
	c.final.Compute(key, func(cur heightEntry[V], loaded bool) (heightEntry[V], xsync.ComputeOp) {
		if !loaded || !cur.fetchedAt.Equal(stale) {
			return cur, xsync.CancelOp
		}
		cur.fetchedAt = time.Now()
		won = true
		return cur, xsync.UpdateOp
	})
	if won {
		c.revalidate(key)
	}
}

// revalidate asks the chain again for a final entry, without making the caller
// wait: the entry is served until the answer arrives.
func (c *heightCache[V]) revalidate(key heightKey) {
	c.flight.DoChan(flightKey(key), func() (any, error) {
		queryCtx, cancel := context.WithTimeout(context.Background(), c.queryTimeout)
		defer cancel()
		v, answeredAt, err := c.fetch(queryCtx, key)
		if err != nil {
			return nil, fmt.Errorf("re-validate %s at height %d: %w", c.cacheType, key.height, err)
		}
		cur, ok := c.final.Load(key)
		if !ok {
			// Evicted while the RPC was in flight: keep it like any other answer.
			return heightAnswer[V]{value: v, final: c.store(key, v, answeredAt)}, nil
		}
		if answeredAt < key.height {
			// A node behind a final height is behind, not a correction.
			return heightAnswer[V]{value: cur.value, final: true}, nil
		}
		if !c.equal(cur.value, v) {
			c.changed.Inc()
			c.logger.Warn().Str("cache_type", c.cacheType).Str("id", key.id).Int64("height", key.height).
				Int64("answered_at", answeredAt).
				Msg("the chain answered a different value at a height already answered as final; serving the new one")
		}
		c.final.Store(key, heightEntry[V]{value: v, answeredAt: answeredAt, fetchedAt: time.Now()})
		return heightAnswer[V]{value: v, final: true}, nil
	})
}

// evict brings m back to half of max. Final answers drop the lowest heights
// first (sessions long settled); answers that are not final drop the highest
// first (heights no real session has reached). Among equal heights the oldest
// answer goes first: every service's session starts on the same height, so a
// full cache is mostly one height, and the entries just stored are the ones in
// use. One sweep at a time: a caller that finds one running skips it.
func (c *heightCache[V]) evict(m *xsync.Map[heightKey, heightEntry[V]], max int, highestFirst bool) {
	if !c.evictMu.TryLock() {
		return
	}
	defer c.evictMu.Unlock()
	if m.Size() <= max {
		return
	}
	type candidate struct {
		key       heightKey
		fetchedAt time.Time
	}
	candidates := make([]candidate, 0, m.Size())
	m.Range(func(k heightKey, e heightEntry[V]) bool {
		candidates = append(candidates, candidate{key: k, fetchedAt: e.fetchedAt})
		return true
	})
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.key.height != b.key.height {
			if highestFirst {
				return a.key.height > b.key.height
			}
			return a.key.height < b.key.height
		}
		return a.fetchedAt.Before(b.fetchedAt)
	})
	for _, cand := range candidates[:len(candidates)-max/2] {
		m.Delete(cand.key)
	}
}

func flightKey(k heightKey) string {
	return k.id + "@" + strconv.FormatInt(k.height, 10)
}
