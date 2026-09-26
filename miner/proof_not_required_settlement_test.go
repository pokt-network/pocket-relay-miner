//go:build test

package miner

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// notRequiredAt builds the error the chain's refusal arrives as: the sentinel
// carrying the rejection, with the index naming one message of the batch.
func notRequiredAt(index int, hasIndex bool) error {
	return fmt.Errorf("%w: %w", tx.ErrTxProofNotRequired, &tx.TxRejection{
		Stage:       tx.TxStageSimulate,
		HasMsgIndex: hasIndex,
		MsgIndex:    index,
		RawLog:      "failed to execute message; message index: 2: proof not required",
	})
}

func settlementFixture(t *testing.T, sessionIDs ...string) (*LifecycleCallback, SessionStore, []*SessionSnapshot) {
	t.Helper()
	store, _ := setupTestSessionStore(t)
	t.Cleanup(func() { _ = store.Close() })

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
	t.Cleanup(func() { _ = coord.Close() })

	lc := createTestLifecycleCallback(&mockSMSTManager{})
	lc.sessionCoordinator = coord

	snapshots := make([]*SessionSnapshot, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		snap := reactivationTestSnapshot(id, SessionStateProving)
		// A service of its own per session, so the loss counters carry one
		// series per session. Sharing labels would make the money assertion
		// below unable to tell WHICH session was counted -- the same failure
		// this whole step exists to remove from the state.
		snap.ServiceID = "svc-" + id
		require.NoError(t, store.Save(context.Background(), snap))
		snapshots = append(snapshots, snap)
	}
	return lc, store, snapshots
}

func stateOf(t *testing.T, store SessionStore, sessionID string) SessionState {
	t.Helper()
	snap, err := store.Get(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, snap, "session %s vanished", sessionID)
	return snap.State
}

// The chain names ONE message of the batch, and that verdict belongs to that
// session alone: its claim settles without a proof, and a retry would get the
// same answer because the requirement is seeded from a fixed block hash. The
// others were never transmitted -- the batch is one transaction -- so their loss
// is real.
//
// Asserted per session BY NAME. "one proved and two failed" is also true when
// the wrong session was proved, which is exactly the mistake the index exists to
// prevent, and the index is 2 rather than 0 because a hardcoded zero passes at
// the head of the batch.
func TestNotRequiredSettlesTheNamedSessionAndFailsTheRest(t *testing.T) {
	lc, store, snaps := settlementFixture(t, "s-zero", "s-one", "s-named")

	before := proofTxErrorRelays(t, snaps[2])

	lc.settleNotRequiredBatch(context.Background(), testLogger(), notRequiredAt(2, true), snaps)

	require.Equal(t, SessionStateProbabilisticProved, stateOf(t, store, "s-named"),
		"the chain told us this claim needs no proof: recording it as a tx error is a false fact")
	require.Equal(t, SessionStateProofTxError, stateOf(t, store, "s-zero"),
		"never transmitted, so the loss is real")
	require.Equal(t, SessionStateProofTxError, stateOf(t, store, "s-one"))

	// The state alone cannot see this. OnProofTxError REFUSES to overwrite a
	// probabilistic_proved session (session_coordinator.go:688), so marking the
	// named one twice leaves the right state behind -- while RecordProofTxError
	// runs before that guard and without one, counting its relays and compute
	// units as lost. The state machine protects the state; nothing protects the
	// money.
	require.Equal(t, before, proofTxErrorRelays(t, snaps[2]),
		"the session the chain settled was also counted as a loss")
}

// proofTxErrorRelays reads the relays this session has contributed to the
// proof-tx-error loss counter.
func proofTxErrorRelays(t *testing.T, snap *SessionSnapshot) float64 {
	t.Helper()
	return testutil.ToFloat64(relaysLostTotal.WithLabelValues(
		snap.SupplierOperatorAddress, snap.ServiceID, "proof_tx_error"))
}

// With no index every message is indistinguishable, so all of them take the
// error. Not a second policy: this one with nothing to split on. A fee, nonce or
// TTL failure arrives this way, because the ante handler runs in simulation too
// and fails before any message executes.
func TestNotRequiredWithoutAnIndexFailsEverySession(t *testing.T) {
	lc, store, snaps := settlementFixture(t, "s-a", "s-b")

	lc.settleNotRequiredBatch(context.Background(), testLogger(), notRequiredAt(0, false), snaps)

	require.Equal(t, SessionStateProofTxError, stateOf(t, store, "s-a"),
		"index zero means 'message 0' only when there WAS an index")
	require.Equal(t, SessionStateProofTxError, stateOf(t, store, "s-b"))
}

// The index is parsed out of the server's own text, which can carry a second
// "message index:" of its own. An out-of-range value settles nothing as proved
// -- the loop compares rather than indexes, so that part is safe by
// construction -- and the assertion here is on the WARNING, because that is the
// only thing the range check actually buys. Without it the batch would still
// come out all-errors, silently, and silence here is indistinguishable from a
// batch that legitimately carried no index.
func TestNotRequiredWithAnOutOfRangeIndexIsAudible(t *testing.T) {
	lc, store, snaps := settlementFixture(t, "s-a", "s-b")

	var buf bytes.Buffer
	lc.settleNotRequiredBatch(context.Background(),
		zerolog.New(&buf).Level(zerolog.TraceLevel), notRequiredAt(7, true), snaps)

	require.Equal(t, SessionStateProofTxError, stateOf(t, store, "s-a"))
	require.Equal(t, SessionStateProofTxError, stateOf(t, store, "s-b"))
	require.Contains(t, buf.String(), "message index outside the batch",
		"a nonsense index must be reported: settling everything as an error in silence reads like a batch with no index")
}

// A single-proof batch is the common shape, and there the named message is the
// only one: nothing is left to fail.
func TestNotRequiredWithASingleProofProvesIt(t *testing.T) {
	lc, store, snaps := settlementFixture(t, "s-only")

	lc.settleNotRequiredBatch(context.Background(), testLogger(), notRequiredAt(0, true), snaps)

	require.Equal(t, SessionStateProbabilisticProved, stateOf(t, store, "s-only"))
}
