//go:build test

package miner

import (
	"context"
	"testing"
	"time"

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
}

func (b *batchSpy) CreateClaims(_ context.Context, _ int64, msgs ...pocktclient.MsgCreateClaim) error {
	sent := make([]string, 0, len(msgs))
	for _, m := range msgs {
		sent = append(sent, m.(*prooftypes.MsgCreateClaim).SessionHeader.GetSessionId())
	}
	b.calls = append(b.calls, sent)

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

func ejectionFixture(t *testing.T, spy *batchSpy, ids ...string) (*LifecycleCallback, SessionStore, *RebroadcastStore, []*SessionSnapshot) {
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
			ServiceID:               "svc",
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
	return lc, sessionStore, rebroadcast, snapshots
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
	lc, sessionStore, rebroadcast, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb", "sess-ccc")

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
	lc, _, _, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb", "sess-ccc")

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
	lc, sessionStore, rebroadcast, snapshots := ejectionFixture(t, spy, "sess-only")

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
