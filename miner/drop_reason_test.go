//go:build test

package miner

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// A drop the live gate accepts as "announced" explains a missing relay away.
// A REDELIVERED copy cannot be that explanation: it was delivered before, to a
// consumer that did not finish it -- the relay is either already in the tree or
// was lost in a handoff. In the L3 of df5441c (2026-09-11) seven redelivered
// copies were counted as session_sealed and the lost relay among them read as
// accounted for. Each of the three drop branches below is asked twice: as a
// first delivery (the control, the reason the gate accepts) and as a reclaim.

func rejected(supplier, reason string) float64 {
	return testutil.ToFloat64(relaysRejected.WithLabelValues(supplier, reason, "svc-1"))
}

func requireDropReason(t *testing.T, supplier, plain string, deliver func(reclaim bool)) {
	t.Helper()
	redelivered := plain + "_redelivered"

	plainBefore, redeliveredBefore := rejected(supplier, plain), rejected(supplier, redelivered)
	deliver(false)
	require.Equal(t, plainBefore+1, rejected(supplier, plain), "a first delivery keeps the reason the gate accepts")
	require.Equal(t, redeliveredBefore, rejected(supplier, redelivered))

	plainBefore = rejected(supplier, plain)
	deliver(true)
	require.Equal(t, redeliveredBefore+1, rejected(supplier, redelivered),
		"a redelivered copy is counted apart: it cannot explain a missing relay")
	require.Equal(t, plainBefore, rejected(supplier, plain), "and not under the reason the gate accepts")
}

// Branch 1: the session is already terminal.
func TestDropReason_TerminalSessionSplitsRedeliveries(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1drop_terminal")
	const sessionID = "sess-drop-terminal"
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, newStreamMessage(f.supplierAddr, sessionID, "first", 100)))
	require.NoError(t, f.sessionStore.UpdateState(f.ctx, sessionID, SessionStateProved))

	n := 0
	requireDropReason(t, f.supplierAddr, "session_sealed", func(reclaim bool) {
		n++
		m := newStreamMessage(f.supplierAddr, sessionID, "late-"+string(rune('a'+n)), 100)
		m.IsReclaim = reclaim
		require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, m))
	})
}

// Branch 2: the claim window has closed.
func TestDropReason_ClosedClaimWindowSplitsRedeliveries(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1drop_window")
	f.coordinator.SetClaimWindowClosedFn(func(int64) bool { return true })

	n := 0
	requireDropReason(t, f.supplierAddr, "claim_window_closed", func(reclaim bool) {
		n++
		m := newStreamMessage(f.supplierAddr, "sess-drop-window", "late-"+string(rune('a'+n)), 100)
		m.IsReclaim = reclaim
		require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, m))
	})
}

// Branch 3: the session is not terminal yet, but its tree is sealed.
func TestDropReason_SealedTreeSplitsRedeliveries(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1drop_sealed")
	const sessionID = "sess-drop-sealed"
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, newStreamMessage(f.supplierAddr, sessionID, "first", 100)))
	_, err := f.smstMgr.FlushTree(f.ctx, sessionID)
	require.NoError(t, err)

	n := 0
	requireDropReason(t, f.supplierAddr, "session_sealed", func(reclaim bool) {
		n++
		m := newStreamMessage(f.supplierAddr, sessionID, "late-"+string(rune('a'+n)), 100)
		m.IsReclaim = reclaim
		require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, m))
	})
}
