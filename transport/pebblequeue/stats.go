package pebblequeue

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble"

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
