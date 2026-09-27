//go:build test

package miner

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// recordFlushed counted as duplicates every relay the script's SADD found
// already marked. A flush whose script ran but whose answer was lost is
// retried, and the retry finds all its own relays marked: n duplicates, of
// relays delivered once.

// TestRelayBatch_ARetriedFlushCountsNoDuplicates: the script runs, its answer
// is lost, the flush runs again.
func TestRelayBatch_ARetriedFlushCountsNoDuplicates(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_dup_retry", "sess-batch-dup-retry"
	w := newBatchWorker(t, client, supplier, "a")
	for i, id := range w.publish(3) {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("retry-%d", i), 100))
	}
	lost := true
	w.batch.hook = func(p flushPoint, _ string) error {
		if p == flushPointAfterScript && lost {
			lost = false
			return errors.New("injected: the script's answer was lost")
		}
		return nil
	}
	dups := relaysRejected.WithLabelValues(supplier, "duplicate", "svc-1")
	before := testutil.ToFloat64(dups)

	w.batch.FlushAll(w.ctx)
	require.Equal(t, 3, w.held(sessionID), "premise: the answer was lost, so the batch is kept for a retry")
	require.Zero(t, w.streamLen(), "premise: the script ran: its entries are gone")

	w.batch.FlushAll(w.ctx)
	require.Zero(t, w.held(sessionID))
	require.Equal(t, before, testutil.ToFloat64(dups), "a retry of the flush's own run is not a duplicate")
	require.Equal(t, int64(3), w.snapshot(sessionID).RelayCount, "and the relays are counted once")
}

// TestRelayBatch_ARelayAnotherConsumerFinishedIsOneDuplicate is the control:
// its entry is still here, and its relay is already marked. It is the LAST of
// the batch, right after a relay whose entry is gone (XACKDEL answers -1), so
// that reading the answer of a neighbouring position, in either direction,
// finds no duplicate there.
func TestRelayBatch_ARelayAnotherConsumerFinishedIsOneDuplicate(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_dup_genuine", "sess-batch-dup-genuine"
	w := newBatchWorker(t, client, supplier, "a")
	ids := w.publish(3)
	w.deliver(w.msg(ids[0], sessionID, "dup-fresh-0", 100))
	w.deliver(w.msg(ids[1], sessionID, "dup-fresh-1", 100))
	w.deliver(w.msg("1-1", sessionID, "dup-gone", 100)) // an entry the stream no longer has
	w.deliver(w.msg(ids[2], sessionID, "dup-finished", 100))
	finished := sha256.Sum256([]byte("dup-finished"))
	require.NoError(t, client.SAdd(w.ctx, w.dedup.sessionKey(sessionID), hashMember(finished[:])).Err())
	dups := relaysRejected.WithLabelValues(supplier, "duplicate", "svc-1")
	before := testutil.ToFloat64(dups)

	w.batch.FlushAll(w.ctx)

	require.Equal(t, before+1, testutil.ToFloat64(dups), "the copy another consumer finished is one duplicate")
	require.Equal(t, int64(3), w.snapshot(sessionID).RelayCount, "the other three are counted, the gone one included")
	require.Zero(t, w.streamLen())
}
