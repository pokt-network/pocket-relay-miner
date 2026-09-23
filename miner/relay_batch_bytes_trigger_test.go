//go:build test

package miner

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

var errInjectedFlushCut = errors.New("injected: flush cut before the script")

func batchFlushes(supplier, trigger string) float64 {
	return testutil.ToFloat64(relayBatchFlushesTotal.WithLabelValues(supplier, trigger))
}

// newBytesTriggerFixture runs the supplier's real consume loop with a flush
// interval no test waits for, so a flush can only come from the byte trigger.
func newBytesTriggerFixture(t *testing.T, supplier string) *panicLoopFixture {
	t.Helper()
	f := newPanicLoopFixture(t, supplier, func(int32) {})
	f.w.mgr.config.RelayBatchFlushInterval = time.Hour
	return f
}

// waitProcessed waits for n relays to go through the handler.
func (f *panicLoopFixture) waitProcessed(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.processed:
		case <-time.After(20 * time.Second):
			t.Fatalf("only %d of %d relays were processed", i, n)
		}
	}
}

// TestRelayBatch_BigRelaysFlushOnBytesWithoutTheTick: a leaf keeps its relay
// until a flush commits and compacts it, and before the byte trigger only the
// tick flushed, so a supplier of big relays held a whole interval of them
// (3.0-3.6 GiB measured with 1 MiB relays). Two relays that together pass
// relayBatchFlushBytes must be flushed -- counted, acknowledged -- with the
// tick an hour away.
func TestRelayBatch_BigRelaysFlushOnBytesWithoutTheTick(t *testing.T) {
	const supplier, sessionID = "pokt1bytes_trigger_big", "sess-bytes-trigger-big"
	f := newBytesTriggerFixture(t, supplier)
	half := relayBatchFlushBytes/2 + 1
	f.addRelay(t, sessionID, "a"+strings.Repeat("x", half))
	f.addRelay(t, sessionID, "b"+strings.Repeat("x", half))
	before := batchFlushes(supplier, relayBatchFlushBytesTrigger)

	f.start(t)
	f.waitProcessed(t, 2)

	require.Equal(t, before+1, batchFlushes(supplier, relayBatchFlushBytesTrigger),
		"the second relay took the supplier past relayBatchFlushBytes: exactly one byte flush")
	require.Zero(t, f.w.held(sessionID), "the byte flush finished both relays, it did not wait for the tick")
	require.Zero(t, f.w.smst.LeafBytesSinceFlush(), "the flush started the count again")
	require.Zero(t, f.w.pending(), "both relays acknowledged by the flush")
	require.Equal(t, int64(2), f.w.snapshot(sessionID).RelayCount, "both relays counted once")
}

// TestRelayBatch_ARelayBiggerThanTheThresholdFlushesAlone: one relay larger
// than relayBatchFlushBytes is enough on its own.
func TestRelayBatch_ARelayBiggerThanTheThresholdFlushesAlone(t *testing.T) {
	const supplier, sessionID = "pokt1bytes_trigger_alone", "sess-bytes-trigger-alone"
	f := newBytesTriggerFixture(t, supplier)
	f.addRelay(t, sessionID, strings.Repeat("y", relayBatchFlushBytes+1))
	before := batchFlushes(supplier, relayBatchFlushBytesTrigger)

	f.start(t)
	f.waitProcessed(t, 1)

	require.Equal(t, before+1, batchFlushes(supplier, relayBatchFlushBytesTrigger))
	require.Zero(t, f.w.held(sessionID))
	require.Zero(t, f.w.pending())
}

// TestRelayBatch_SmallRelaysNeverFlushOnBytes is the control: relays of the
// size of an eth_blockNumber stay in the batch for the tick, as before.
func TestRelayBatch_SmallRelaysNeverFlushOnBytes(t *testing.T) {
	const supplier, sessionID = "pokt1bytes_trigger_small", "sess-bytes-trigger-small"
	f := newBytesTriggerFixture(t, supplier)
	for i := 0; i < 5; i++ {
		f.addRelay(t, sessionID, `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`+string(rune('a'+i)))
	}
	before := batchFlushes(supplier, relayBatchFlushBytesTrigger)

	f.start(t)
	f.waitProcessed(t, 5)

	require.Equal(t, before, batchFlushes(supplier, relayBatchFlushBytesTrigger), "small relays never reach the byte trigger")
	require.Equal(t, 5, f.w.held(sessionID), "they wait in the batch for the tick")
	require.Positive(t, f.w.smst.LeafBytesSinceFlush(), "premise: their bytes were counted")
}

// TestRelayBatch_AFlushThatKeepsItsRelaysStillStartsTheCountAgain: the count is
// of bytes put in since the last flush ATTEMPT. A flush that fails and keeps
// its relays must not leave the count up, or every relay after it would flush
// again -- one round trip per relay, or one Redis timeout per relay.
func TestRelayBatch_AFlushThatKeepsItsRelaysStillStartsTheCountAgain(t *testing.T) {
	const supplier, sessionID = "pokt1bytes_trigger_retry", "sess-bytes-trigger-retry"
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, supplier, "a")
	w.batch.hook = func(point flushPoint, _ string) error {
		if point == flushPointBeforeScript {
			return errInjectedFlushCut
		}
		return nil
	}
	w.deliver(w.msg("1-1", sessionID, strings.Repeat("z", 1024), 100))
	require.Equal(t, int64(1024), w.smst.LeafBytesSinceFlush(), "premise: the relay's bytes are counted")

	w.batch.FlushAll(w.ctx)

	require.Equal(t, 1, w.held(sessionID), "premise: the cut flush kept the relay")
	require.Zero(t, w.smst.LeafBytesSinceFlush(), "a flush attempt starts the count again whatever its outcome")
}
