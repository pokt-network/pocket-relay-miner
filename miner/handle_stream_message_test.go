//go:build test

package miner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// handleStreamMessage decides what happens to a relay whose processing failed,
// and until now nothing asserted that decision -- the two mentions of the
// function in shutdown_drain_test.go are comments. The failure path is where
// money is lost, so it is the half that most needed the assertion.
//
// The two branches are deliberately opposite, and the difference is visible on
// the SERVER rather than in a call count:
//
//   - a recovered panic is DETERMINISTIC, so retrying it only spends the
//     failure again. The relay was already served, so it is acknowledged and
//     counted as lost work. AckMessage is XAckDel with DELREF, which deletes
//     the entry: pending 0, stream 0.
//   - anything else is the transient class -- the worker acknowledges and
//     counts the permanent ones before they ever reach here. It is handed back
//     so a later delivery retries it: pending 1, stream 1, and claimable.

// TestHandleStreamMessageAcksAndCountsARelayLostToAPanic pins the branch that
// did not exist before d706c75: the panic was logged loudly and then dropped
// with a Debug line, so nothing anywhere counted that a served relay went
// unbilled.
func TestHandleStreamMessageAcksAndCountsARelayLostToAPanic(t *testing.T) {
	f := newDrainFixture(t, 1)
	f.mgr.onRelay = func(context.Context, string, *transport.StreamMessage) error {
		panic("processing blew up")
	}

	// The child series, not the whole vec: a label set of its own keeps this
	// independent of anything else in the package that touches relays_lost_total.
	lost := relaysLostTotal.WithLabelValues(drainSupplier, "svc-a", "panic_recovered")
	before := testutil.ToFloat64(lost)

	acked := f.mgr.handleStreamMessage(context.Background(), f.state, f.msgs[0])

	require.True(t, acked, "a relay lost to a panic must be acknowledged, not retried forever")
	require.Equal(t, before+1, testutil.ToFloat64(lost),
		"and the loss must be counted: the panic log says it happened, this says it cost a relay")
	require.Equal(t, int64(0), f.pendingCount(t), "acknowledged means out of the pending list")
	require.Equal(t, int64(0), f.streamLen(t), "XAckDel with DELREF removes the entry")
}

// TestHandleStreamMessageHandsBackARelayThatFailedTransiently is the regression
// test for the defect item 15 describes: the entry used to be left pending on
// purpose, trusting a comment that said "let the reclaim retry". That was true
// in main and false in this stack -- 00a1b3d made the reclaim skip entries owned
// by the caller, so on a single-miner fleet the entry sat until the process
// restarted and its consumer name changed. If the claim window closed first the
// relay had been served and was never billed.
//
// The assertion that separates the fix from the defect is the last one: another
// consumer taking it with a min-idle far larger than the entry's real age. Merely
// still being pending is what the DEFECT also produced.
func TestHandleStreamMessageHandsBackARelayThatFailedTransiently(t *testing.T) {
	f := newDrainFixture(t, 1)
	f.mgr.onRelay = func(context.Context, string, *transport.StreamMessage) error {
		return errors.New("redis hiccup while writing the SMST")
	}

	acked := f.mgr.handleStreamMessage(context.Background(), f.state, f.msgs[0])

	require.False(t, acked, "a transient failure must NOT be acknowledged: acknowledging deletes the relay")
	require.Equal(t, int64(1), f.pendingCount(t), "it stays pending so a later delivery can retry it")
	require.Equal(t, int64(1), f.streamLen(t), "and it stays in the stream")

	claimed, _, err := f.client.XAutoClaimJustID(context.Background(), &redis.XAutoClaimArgs{
		Stream: f.stream, Group: f.group, Consumer: "other", MinIdle: 30 * time.Second, Start: "0", Count: 10,
	}).Result()
	require.NoError(t, err)
	require.Len(t, claimed, 1,
		"released, not merely pending: an entry still owned by this consumer is younger than "+
			"any min-idle and the reclaim skips it as its own in-flight delivery")
}

// TestHandleStreamMessageAcksASuccessfulRelay is the third leg. Without it the
// two above could both pass while every relay took the same path.
func TestHandleStreamMessageAcksASuccessfulRelay(t *testing.T) {
	f := newDrainFixture(t, 1)

	acked := f.mgr.handleStreamMessage(context.Background(), f.state, f.msgs[0])

	require.True(t, acked)
	require.Equal(t, []string{f.msgs[0].ID}, *f.processed, "the fixture's onRelay ran")
	require.Equal(t, int64(0), f.pendingCount(t))
	require.Equal(t, int64(0), f.streamLen(t))
}
