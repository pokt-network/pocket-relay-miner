//go:build test

package miner

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Rejected relays are acknowledged by the relay batch when it flushes, in one
// XACKDEL, instead of one XACKDEL each as they arrive.

func TestRelayBatch_RejectionsAreAcknowledgedTogetherOnTheFlush(t *testing.T) {
	client, _ := newTestRedis(t)
	counter := newNamedCommandCounter(client)
	w := newBatchWorker(t, client, "pokt1rejection_acks", "consumer-a")
	const sessionID = "sess-rejection-acks"
	// Every relay below is rejected at the entry.
	require.NoError(t, w.smst.DeleteTree(w.ctx, sessionID))

	const n = 5
	ids := w.publish(n)
	counter.take()
	for i, id := range ids {
		require.False(t, w.deliver(w.msg(id, sessionID, fmt.Sprintf("rejection-acks-%d", i), 100)),
			"a rejection is not acknowledged by its own delivery")
	}
	require.Empty(t, counter.take(), "rejecting sends nothing to Redis")
	require.EqualValues(t, n, w.pending(), "the rejections wait for the flush, pending")

	counter.take()
	w.batch.FlushAll(w.ctx)
	require.Equal(t, map[string]int{"xackdel": 1}, counter.take(),
		"the flush acknowledges every rejection in one XACKDEL")
	require.Zero(t, w.pending())
	require.Zero(t, w.streamLen(), "acknowledged with DELREF, the entries are gone")
	require.Zero(t, w.marked(sessionID), "a rejection is not marked as processed")
	require.Nil(t, w.snapshot(sessionID), "a rejection creates no session")
}

func TestRelayBatch_RejectionsHeldAtACrashAreRejectedAgain(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1rejection_crash", "sess-rejection-crash"
	a := newBatchWorker(t, client, supplier, "consumer-a")
	ids := a.publish(2)

	a.deliver(a.msg(ids[0], sessionID, "rejection-crash-0", 100))
	a.batch.FlushAll(a.ctx)
	require.EqualValues(t, 1, a.snapshot(sessionID).RelayCount, "CONTROL: counted once")

	// A copy of the same relay comes back as a reclaim: a duplicate, rejected
	// and held for the flush.
	dup := a.msg(ids[1], sessionID, "rejection-crash-0", 100)
	dup.IsReclaim = true
	require.False(t, a.deliver(dup))
	require.EqualValues(t, 1, a.pending(), "CONTROL: the duplicate waits for a flush")
	// consumer-a dies here: its batch is never flushed.

	b := newBatchWorker(t, client, supplier, "consumer-b")
	again := b.msg(ids[1], sessionID, "rejection-crash-0", 100)
	again.IsReclaim = true
	require.False(t, b.deliver(again))
	b.batch.FlushAll(b.ctx)

	require.Zero(t, b.pending(), "the miner that takes the entry over rejects and acknowledges it")
	require.EqualValues(t, 1, b.snapshot(sessionID).RelayCount, "never counted twice")
	require.EqualValues(t, 1, b.marked(sessionID))
}

func TestRelayBatch_OnExitRejectionsAreAcknowledgedNotReleased(t *testing.T) {
	exits := []struct {
		name string
		run  func(w *batchWorker)
	}{
		{"released", func(w *batchWorker) { w.batch.ReleaseAll(w.ctx) }},
		{"key_removed", func(w *batchWorker) { w.batch.AckAllAsLost(w.ctx) }},
	}
	for _, exit := range exits {
		t.Run(exit.name, func(t *testing.T) {
			client, _ := newTestRedis(t)
			w := newBatchWorker(t, client, "pokt1rejection_exit_"+exit.name, "consumer-a")
			const sessionID = "sess-rejection-exit"
			require.NoError(t, w.smst.DeleteTree(w.ctx, sessionID))

			ids := w.publish(3)
			for i, id := range ids {
				w.deliver(w.msg(id, sessionID, fmt.Sprintf("rejection-exit-%d", i), 100))
			}
			require.EqualValues(t, 3, w.pending(), "CONTROL: the rejections are held")

			exit.run(w)
			require.Zero(t, w.pending(), "rejections are acknowledged on the way out, not handed back")
			require.Zero(t, w.streamLen())
		})
	}
}
