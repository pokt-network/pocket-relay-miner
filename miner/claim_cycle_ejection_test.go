//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// batchSpy records WHICH SESSIONS travelled on each call, not how many
// messages did. The count alone cannot tell a correctly shrunk batch from one
// that kept the right length and the wrong contents, and the whole risk of the
// ejection is exactly that: four parallel views of one batch, of which only
// interfaceClaimMsgs is transmitted.
type batchSpy struct {
	pocktclient.SupplierClient
	errs  []error // errs[i] is returned on call i; nil (or past the end) accepts
	calls [][]string
	// onCall runs after recording call i (0-based). It exists so a test can move
	// the chain forward BETWEEN sends -- the window closing while the batch is
	// being split is a real sequence and cannot be set up in advance.
	onCall func(i int)
}

func (b *batchSpy) CreateClaims(_ context.Context, _ int64, msgs ...pocktclient.MsgCreateClaim) error {
	sent := make([]string, 0, len(msgs))
	for _, m := range msgs {
		sent = append(sent, m.(*prooftypes.MsgCreateClaim).SessionHeader.GetSessionId())
	}
	b.calls = append(b.calls, sent)
	if b.onCall != nil {
		b.onCall(len(b.calls) - 1)
	}

	if len(b.calls) <= len(b.errs) {
		return b.errs[len(b.calls)-1]
	}
	return nil
}

func namedRejection(index int) error {
	return &tx.TxRejection{
		Stage:       tx.TxStageSimulate,
		HasMsgIndex: true,
		MsgIndex:    index,
		RawLog:      "failed to execute message; message index: 1: some per-message refusal",
	}
}

func ejectionFixture(t *testing.T, spy *batchSpy, ids ...string) (*LifecycleCallback, SessionStore, *RebroadcastStore, *heightedBlocks, []*SessionSnapshot) {
	t.Helper()

	sessionStore, redisClient := setupTestSessionStore(t)
	t.Cleanup(func() { _ = sessionStore.Close() })
	coord := NewSessionCoordinator(testLogger(), sessionStore, SMSTRecoveryConfig{SupplierAddress: "pokt1eject"})
	t.Cleanup(func() { _ = coord.Close() })
	rebroadcast := NewRebroadcastStore(redisClient, time.Hour)

	blocks := &heightedBlocks{}
	blocks.currentHeight = 103 // inside the claim window (102..106) for end height 100

	snapshots := make([]*SessionSnapshot, 0, len(ids))
	for _, id := range ids {
		snap := &SessionSnapshot{
			SessionID:               id,
			SessionEndHeight:        100,
			SessionStartHeight:      81,
			SupplierOperatorAddress: "pokt1eject",
			ServiceID:               "svc-" + id,
			RelayCount:              10,
			TotalComputeUnits:       100,
			State:                   SessionStateClaiming,
		}
		if err := sessionStore.Save(context.Background(), snap); err != nil {
			t.Fatalf("seeding %q: %v", id, err)
		}
		snapshots = append(snapshots, snap)
	}

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:       &defaultParamsShared{},
		blockClient:        blocks,
		smstManager:        smstStub{},
		supplierClient:     spy,
		serviceClient:      erroringService{},
		rebroadcastStore:   rebroadcast,
		sessionCoordinator: coord,
		config:             LifecycleCallbackConfig{ClaimRetryAttempts: 2, ClaimRetryDelay: time.Millisecond},
	}
	return lc, sessionStore, rebroadcast, blocks, snapshots
}

func origTxHashOf(t *testing.T, store *RebroadcastStore, sessionID string) (string, bool) {
	t.Helper()
	pending, err := store.List(context.Background(), RebroadcastPhaseClaim, "pokt1eject", 100)
	if err != nil {
		t.Fatalf("listing the rebroadcast store: %v", err)
	}
	raw, ok := pending[sessionID]
	if !ok {
		return "", false
	}
	entry, decErr := unmarshalRebroadcastEntry(raw)
	if decErr != nil {
		t.Fatalf("entry for %q will not decode: %v", sessionID, decErr)
	}
	return entry.OrigTxHash, true
}

// TestOnSessionsNeedClaim_NamedMessageIsEjectedAndTheRestAreClaimed is the
// property S6 exists for: when the chain executes the messages and names the
// one it refused, the OTHER sessions must not pay for it.
func TestOnSessionsNeedClaim_NamedMessageIsEjectedAndTheRestAreClaimed(t *testing.T) {
	spy := &batchSpy{errs: []error{namedRejection(1)}}
	lc, sessionStore, rebroadcast, _, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb", "sess-ccc")

	result, err := lc.OnSessionsNeedClaim(context.Background(), snapshots)
	if err != nil {
		t.Errorf("the surviving batch was accepted, so the cycle must not error: %v", err)
	}

	// 1. Named by identity: the two survivors, and not the ejected one.
	// 2. WHAT TRAVELLED, by session id. This is the assertion that catches a
	// stale interfaceClaimMsgs: re-deriving the three bookkeeping views and
	// re-sending the old interface slice keeps the send looking plausible while
	// every outcome lands on the wrong session.
	if len(spy.calls) != 2 {
		t.Fatalf("expected one failed send and one retry, got %d sends: %v", len(spy.calls), spy.calls)
	}
	// The expectation is DERIVED from what actually travelled first, never from
	// the input order: claims are built by a worker pool and collected in
	// completion order, so the batch reaching the chain is in no fixed order
	// (the proof path sorts by build index, this one does not -- filed
	// separately). Hard-coding the input order here would be a FLAKY test that
	// happens to pass. The property does not need the order: the retry must be
	// the first batch minus the message the chain named, which is index 1.
	first := spy.calls[0]
	if len(first) != 3 {
		t.Fatalf("the first send must carry the whole batch, it carried %v", first)
	}
	ejectedID := first[1]
	want := []string{first[0], first[2]}
	if len(spy.calls[1]) != len(want) {
		t.Fatalf("the retry must carry only the survivors, it carried %v (first send was %v)", spy.calls[1], first)
	}
	for i, id := range want {
		if spy.calls[1][i] != id {
			t.Fatalf(
				"the retry carried %v, want %v -- the batch that TRAVELLED disagrees with the bookkeeping",
				spy.calls[1], want,
			)
		}
	}

	// 1. Named by identity: the two survivors, and not the ejected one. Checked
	// after the send, because which session is ejected is only known from what
	// travelled.
	for _, id := range want {
		if !result.IsClaimed(id) {
			t.Errorf("survivor %q must be claimed, named = %v", id, keysOf(result.Claimed))
		}
	}
	if result.IsClaimed(ejectedID) {
		t.Errorf("the ejected session %q must NOT be claimed, named = %v", ejectedID, keysOf(result.Claimed))
	}

	// 3. Terminal state on the ejected one only.
	if got := stateOf(t, sessionStore, ejectedID); got != SessionStateClaimTxError {
		t.Errorf("the ejected session must be terminal, got %q", got)
	}
	for _, id := range want {
		if got := stateOf(t, sessionStore, id); got == SessionStateClaimTxError {
			t.Errorf("survivor %q must not be marked claim_tx_error", id)
		}
	}

	// 4. The ejected one KEEPS its entry. Council condition 1: no claim verdict
	// is demonstrated terminal, and one of them heals on the next block, so a
	// message with no entry would be forfeited for a transient condition.
	hash, present := origTxHashOf(t, rebroadcast, ejectedID)
	if !present {
		t.Fatalf("the ejected message must keep its rebroadcast entry, or it is forfeited")
	}
	if hash != "" {
		t.Errorf("it never travelled, so OrigTxHash must be empty, got %q", hash)
	}
}

// TestOnSessionsNeedClaim_TransportFailureRetriesTheWholeBatch is the guard on
// the TRIGGER, and it is the one that keeps a network hiccup from breaking a
// group into singles forever. Nothing else in the estate holds this.
func TestOnSessionsNeedClaim_TransportFailureRetriesTheWholeBatch(t *testing.T) {
	// A REAL broadcast-stage rejection, not a bare error: the trigger must be
	// narrowed by HasMsgIndex, and a plain error would pass a widened trigger
	// too -- the injection "stop checking HasMsgIndex" has to have something to
	// bite on. This is the shape a transport failure actually arrives in
	// (newBroadcastRejection leaves MsgIndex/HasMsgIndex zero).
	spy := &batchSpy{errs: []error{&tx.TxRejection{
		Stage:  tx.TxStageBroadcast,
		RawLog: "connection refused by the full node",
	}}}
	lc, _, _, _, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb", "sess-ccc")

	if _, err := lc.OnSessionsNeedClaim(context.Background(), snapshots); err != nil {
		t.Errorf("the retry was accepted, so the cycle must not error: %v", err)
	}
	if len(spy.calls) != 2 {
		t.Fatalf("expected one failure and one retry, got %v", spy.calls)
	}
	if len(spy.calls[1]) != 3 {
		t.Fatalf("a failure that names NO message must retry the whole batch, it retried %v", spy.calls[1])
	}
}

// TestOnSessionsNeedClaim_TheLastMessageIsNeverEjected pins the floor. An empty
// batch is not a smaller batch: CreateClaims returns SUCCESS for zero messages
// and the tx hash is read from a field shared across groups, so ejecting the
// last message would report a claim as submitted carrying another group's hash.
func TestOnSessionsNeedClaim_TheLastMessageIsNeverEjected(t *testing.T) {
	spy := &batchSpy{errs: []error{namedRejection(0), namedRejection(0)}}
	lc, sessionStore, rebroadcast, _, snapshots := ejectionFixture(t, spy, "sess-only")

	result, err := lc.OnSessionsNeedClaim(context.Background(), snapshots)
	if err == nil {
		t.Errorf("every attempt failed, so the cycle must report an error")
	}
	if result.IsClaimed("sess-only") {
		t.Errorf("nothing was ever accepted, so no session may be named claimed")
	}
	for _, sent := range spy.calls {
		if len(sent) == 0 {
			t.Fatalf("a batch was sent with ZERO messages; that returns success and reports a claim that never travelled")
		}
	}
	if got := stateOf(t, sessionStore, "sess-only"); got != SessionStateClaimTxError {
		t.Errorf("the failed session must be terminal, got %q", got)
	}
	if _, present := origTxHashOf(t, rebroadcast, "sess-only"); !present {
		t.Errorf("the failed message must keep its rebroadcast entry")
	}
}

// The two tests below hold the fifth condition -- the ejected session LEAVES
// groupSnapshots -- which the three above cannot: they all exercise "eject, then
// succeed", and on that path the line is INERT. The retry works, lastErr is nil,
// and the failure block never runs. The condition only matters on the two paths
// nothing was walking, and it was the one condition added AFTER the success
// criterion was written, so it inherited no harness. A correct line with nothing
// holding it is deleted by the next refactor in silence.

// TestOnSessionsNeedClaim_AnEjectedSessionIsNotCountedTwice covers the first of
// those paths: eject, and then the reduced batch fails every attempt too.
//
// The assertion is the LOSS COUNTER and deliberately not the rebroadcast entry.
// Measured before writing it: RebroadcastStore.Put is an HSet keyed by session
// ID (rebroadcast_store.go:88), so a second persist OVERWRITES and the entry
// count stays at one either way -- "exactly one entry" is an assertion that can
// never go red. The counter is per (supplier, service, reason), which is why the
// fixture gives every session its own service: sharing one would make the money
// series unable to say WHICH session was counted.
func TestOnSessionsNeedClaim_AnEjectedSessionIsNotCountedTwice(t *testing.T) {
	spy := &batchSpy{errs: []error{namedRejection(1), namedRejection(0), namedRejection(0)}}
	lc, _, _, _, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb")

	// Which session is ejected is only known from what travelled, so the counter
	// is sampled for BOTH and the ejected one identified afterwards.
	before := map[string]float64{}
	for _, snap := range snapshots {
		before[snap.SessionID] = testutil.ToFloat64(
			sessionsFailedTotal.WithLabelValues(snap.SupplierOperatorAddress, snap.ServiceID, "claim_tx_error"),
		)
	}

	if _, err := lc.OnSessionsNeedClaim(context.Background(), snapshots); err == nil {
		t.Errorf("the reduced batch failed every attempt, so the cycle must report an error")
	}

	if len(spy.calls) == 0 {
		t.Fatalf("nothing was sent")
	}
	ejectedID := spy.calls[0][1]

	for _, snap := range snapshots {
		got := testutil.ToFloat64(
			sessionsFailedTotal.WithLabelValues(snap.SupplierOperatorAddress, snap.ServiceID, "claim_tx_error"),
		) - before[snap.SessionID]
		if got != 1 {
			what := "the surviving session"
			if snap.SessionID == ejectedID {
				what = "the EJECTED session (it was settled at ejection; the failure block must not reach it again)"
			}
			t.Errorf("%s %q must be counted lost exactly once, counted %v times", what, snap.SessionID, got)
		}
	}
}

// TestOnSessionsNeedClaim_AnEjectedSessionKeepsItsVerdictWhenTheWindowCloses
// covers the second path, and it is a DIFFERENT failure with a different red:
// here the ejected session is not double-counted, it is RE-JUDGED --
// markAndCountClaimWindowClosed writes claim_window_closed over the
// claim_tx_error it already had, leaving one session with two contradictory
// verdicts in one cycle. That is the exact property the design claims to hold,
// so it gets its own test rather than another assertion in the one above.
func TestOnSessionsNeedClaim_AnEjectedSessionKeepsItsVerdictWhenTheWindowCloses(t *testing.T) {
	spy := &batchSpy{errs: []error{namedRejection(1)}}
	lc, sessionStore, _, blocks, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb")

	// The window closes WHILE the batch is being split: the first send fails
	// naming a message, and by the time the ejection re-checks, the chain has
	// moved past the claim window close (106 for a session ending at 100).
	spy.onCall = func(i int) {
		if i == 0 {
			blocks.mu.Lock()
			blocks.currentHeight = 107
			blocks.mu.Unlock()
		}
	}

	// No assertion on the returned error: the in-loop window-closed path has
	// never appended one (the pre-existing chain-said-window-closed branch does
	// the same), and this test is not the place to change that. Measured, not
	// assumed -- the first version asserted an error and went red against
	// behaviour that predates the ejection.
	_, _ = lc.OnSessionsNeedClaim(context.Background(), snapshots)

	ejectedID := spy.calls[0][1]
	survivorID := spy.calls[0][0]

	// FIRST: prove the window-closed sweep actually ran. Without this the test
	// could pass because nothing happened at all -- an assertion aimed where the
	// defect cannot appear, which is the very failure this pair exists to fix.
	if got := stateOf(t, sessionStore, survivorID); got != SessionStateClaimWindowClosed {
		t.Fatalf(
			"the surviving session %q must be swept as claim_window_closed, got %q -- "+
				"without the sweep this test proves nothing",
			survivorID, got,
		)
	}

	if got := stateOf(t, sessionStore, ejectedID); got != SessionStateClaimTxError {
		t.Errorf(
			"the ejected session %q was already settled claim_tx_error; the window-closed sweep "+
				"must not re-judge it, got %q",
			ejectedID, got,
		)
	}
}
