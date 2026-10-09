package pebblequeue

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cockroachdb/pebble"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// QueueStats is what one supplier's queue holds, as `redis streams` shows a
// stream in high-availability mode.
type QueueStats struct {
	Supplier string `json:"supplier"`
	Stream   string `json:"stream"`
	// Length is the entries stored: delivered and not yet acknowledged ones
	// included, as XLEN counts them.
	Length int64 `json:"length"`
	// Pending is the entries delivered to the miner and not yet acknowledged;
	// Released, those handed back and waiting to be delivered again. Both
	// are zero when no consumer has been created since the process started.
	Pending  int `json:"pending"`
	Released int `json:"released"`
	// LastID is the ID of the last entry published, "<ms>-<seq>"; empty when
	// none was.
	LastID string `json:"last_id"`
}

// Stats returns the supplier's queue.
func (b *Broker) Stats(supplier string) (QueueStats, error) {
	return b.stats(transport.SupplierStreamName(b.streamPrefix, supplier), supplier)
}

// AllStats returns every queue a relay was ever published to.
func (b *Broker) AllStats() ([]QueueStats, error) {
	prefix := []byte(lastIDPrefix)
	iter, err := b.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return nil, fmt.Errorf("pebblequeue: list queues: %w", err)
	}
	var names []string
	for valid := iter.First(); valid; valid = iter.Next() {
		names = append(names, string(iter.Key()[len(prefix):]))
	}
	if err := errors.Join(iter.Error(), iter.Close()); err != nil {
		return nil, fmt.Errorf("pebblequeue: list queues: %w", err)
	}
	out := make([]QueueStats, 0, len(names))
	for _, name := range names {
		supplier := strings.TrimPrefix(name, b.streamPrefix+":")
		st, err := b.stats(name, supplier)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func (b *Broker) stats(name, supplier string) (QueueStats, error) {
	out := QueueStats{Supplier: supplier, Stream: name}
	prefix := entryPrefixOf(name)
	iter, err := b.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return out, fmt.Errorf("pebblequeue: count %s: %w", name, err)
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		out.Length++
	}
	if err := errors.Join(iter.Error(), iter.Close()); err != nil {
		return out, fmt.Errorf("pebblequeue: count %s: %w", name, err)
	}
	value, closer, err := b.store.DB().Get(lastIDKey(name))
	switch {
	case errors.Is(err, pebble.ErrNotFound):
	case err != nil:
		return out, fmt.Errorf("pebblequeue: read last id of %s: %w", name, err)
	default:
		if len(value) == idLen {
			out.LastID = formatID(binary.BigEndian.Uint64(value[:8]), binary.BigEndian.Uint64(value[8:]))
		}
		_ = closer.Close()
	}
	b.mu.Lock()
	c := b.consumers[name]
	b.mu.Unlock()
	if c != nil {
		c.mu.Lock()
		out.Pending, out.Released = len(c.pending), len(c.released)
		c.mu.Unlock()
	}
	return out, nil
}

// Collector exports each supplier's queue: the entries stored and the entries
// delivered and not yet acknowledged, labelled by supplier (bounded by the
// suppliers ever published to). It reads AllStats at scrape time rather than
// keeping a cached value: no goroutine, nothing added to publish or
// acknowledgement, and right after a restart without a recount. The cost is
// one pass over the stored entries per scrape; the queue holds what the miner
// has not consumed yet, normally seconds of traffic, and a scrape that takes
// longer because the miner is far behind is itself the signal.
func (b *Broker) Collector() prometheus.Collector {
	return &queueCollector{
		broker: b,
		length: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "queue_length",
			Help:      "Relays stored in the supplier's queue of a standalone process, delivered and not yet acknowledged ones included, as XLEN counts a stream",
		}, []string{"supplier"}),
		pending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "standalone",
			Name:      "queue_pending",
			Help:      "Relays of the supplier's queue delivered to the miner and not yet acknowledged, in a standalone process",
		}, []string{"supplier"}),
	}
}

// queueCollector rebuilds its gauges on every scrape, under mu, so a queue
// that is gone is gone from the scrape too.
type queueCollector struct {
	broker  *Broker
	mu      sync.Mutex
	length  *prometheus.GaugeVec
	pending *prometheus.GaugeVec
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	c.length.Describe(ch)
	c.pending.Describe(ch)
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.length.Reset()
	c.pending.Reset()
	var (
		all []QueueStats
		err error
	)
	if !c.broker.store.IfOpen(func() { all, err = c.broker.AllStats() }) {
		return
	}
	if err != nil {
		// Once per scrape, and a read of the store failing is a state, not a
		// relay: Warn.
		c.broker.logger.Warn().Err(err).Msg("queue metrics: failed to read the queues")
		return
	}
	for _, st := range all {
		c.length.WithLabelValues(st.Supplier).Set(float64(st.Length))
		c.pending.WithLabelValues(st.Supplier).Set(float64(st.Pending))
	}
	c.length.Collect(ch)
	c.pending.Collect(ch)
}
