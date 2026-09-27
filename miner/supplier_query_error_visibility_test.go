//go:build test

package miner

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestFilterStakedSuppliers_ChainQueryErrorIsCounted closes an observability gap
// in filterStakedSuppliers' fail-open branch.
//
// When the chain query fails for a transient reason (timeout, connection reset —
// anything that is NOT NotFound), the miner deliberately treats the address as
// staked so a fullnode blip cannot false-drain a live supplier. That decision is
// right. What was missing is that the branch ALSO left the supplier's cache entry
// untouched without recording it anywhere a query can reach:
//
//   - supplier_cache_write_skipped_total was not incremented, even though its own
//     Help text describes this exact case ("chain query error") and the sibling
//     path in resolveAndPublishSupplierState already reports it under the
//     "chain_query_error" label. Same event, one path counted it and the other
//     did not.
//   - the only signal was one Warn line PER SUPPLIER per reconcile pass. With a
//     large key set and an unreachable fullnode that is one line per supplier
//     every reconcile interval — the log flood CLAUDE.md forbids for exactly this
//     shape, and the reason per-entity conditions belong at Debug plus a metric.
//
// Skipping the write is correct (the stale entry beats a wrong one). Skipping it
// SILENTLY is what this pins: "the cache is fresh" and "we could not check"
// must never produce the same signal.
func TestFilterStakedSuppliers_ChainQueryErrorIsCounted(t *testing.T) {
	const addr = "pokt1chain_query_error"

	// A transient, non-NotFound error: the fail-open branch under test. A
	// NotFound would mean "genuinely not staked", which takes the other branch
	// and writes Staked:false to the cache.
	qc := &fakeSupplierQueryClient{err: errors.New("connection reset by peer")}
	mgr, _, _ := newCacheTestSupplierManager(t, qc)

	before := testutil.ToFloat64(supplierCacheWriteSkipped.WithLabelValues("chain_query_error"))

	staked := mgr.filterStakedSuppliers(context.Background(), []string{addr})

	require.Equal(t, []string{addr}, staked,
		"fail-open must stand: a transient query error may never drop a supplier, "+
			"because a false drain costs real revenue")
	require.Equal(t,
		before+1,
		testutil.ToFloat64(supplierCacheWriteSkipped.WithLabelValues("chain_query_error")),
		"a skipped cache write must be countable: without this, \"the entry is current\" "+
			"and \"we never managed to check\" look identical in Prometheus")
}
