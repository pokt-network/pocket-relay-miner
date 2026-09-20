//go:build test

package miner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"

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
	errs  []error // errs[i] is returned on call i; nil (or past the end) accepts
	calls [][]string
	// onCall runs after recording call i (0-based). It exists so a test can move
	// the chain forward BETWEEN sends -- the window closing while the batch is
	// being split is a real sequence and cannot be set up in advance.
	onCall func(i int)
}

func (b *batchSpy) CreateClaimsReturningHash(_ context.Context, _ int64, msgs ...pocktclient.MsgCreateClaim) (string, tx.SignedTxPayload, error) {
	sent := make([]string, 0, len(msgs))
	for _, m := range msgs {
		sent = append(sent, m.(*prooftypes.MsgCreateClaim).SessionHeader.GetSessionId())
	}
	b.calls = append(b.calls, sent)
	if b.onCall != nil {
		b.onCall(len(b.calls) - 1)
	}

	// A named rejection comes from simulation, before signing, so a refused
	// call carries no payload.
	if len(b.calls) <= len(b.errs) && b.errs[len(b.calls)-1] != nil {
		return "", tx.SignedTxPayload{}, b.errs[len(b.calls)-1]
	}
	hash, signed := fakeSigned("claim")
	return hash, signed, nil
}

func (*batchSpy) SubmitProofsReturningHash(context.Context, int64, ...pocktclient.MsgSubmitProof) (string, tx.SignedTxPayload, error) {
	panic("batchSpy is a claim double; the proof path must not reach it")
}

// GetEstimatedFeeUpokt answers zero, which leaves the economic viability floor
// off: the batch under test must reach the submit loop whole.
func (*batchSpy) GetEstimatedFeeUpokt(context.Context) uint64 { return 0 }

// BroadcastRawReturningHash and LatestBlockTime make this double satisfy the
// client interface. The zero clock makes reusable() answer NO, so a retry
// through this double signs again -- which is what these tests were written
// against, and keeps them measuring what they were measuring.
func (*batchSpy) BroadcastRawReturningHash(context.Context, string, tx.SignedTxPayload) (string, error) {
	return "", errors.New("batchSpy does not re-inject")
}

func (*batchSpy) LatestBlockTime() time.Time { return time.Time{} }

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

// cmdOrder records, in order, every Redis command a client issues. It exists
// because the property under test is an ORDERING between two writes that both
// succeed: nothing in the resulting state distinguishes the two orders, so the
// only place the difference is visible is the wire.
//
// Both hooks are needed and neither is redundant: RebroadcastStore.Put issues
// its HSet inside a TxPipeline (MULTI/EXEC), which reaches ProcessPipelineHook
// and never ProcessHook, while UpdateState runs a Lua script through
// ProcessHook. A recorder with only one of the two would see one of the writes
// and silently rank it against nothing.
type cmdOrder struct {
	mu   sync.Mutex
	seen []string
}

// argString keeps the command name and its STRING arguments only. The numeric
// key count of EVALSHA and the marshalled payload of HSET are both dropped --
// the payload because it is binary and would swamp the record, the numbers
// because nothing here matches on them.
func argString(cmd redis.Cmder) string {
	var b strings.Builder
	b.WriteString(cmd.Name())
	for _, a := range cmd.Args() {
		if s, ok := a.(string); ok {
			b.WriteString(" ")
			b.WriteString(s)
		}
	}
	return b.String()
}

func (r *cmdOrder) record(cmds ...redis.Cmder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cmd := range cmds {
		r.seen = append(r.seen, argString(cmd))
	}
}

func (r *cmdOrder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *cmdOrder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		r.record(cmd)
		return err
	}
}

func (r *cmdOrder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		r.record(cmds...)
		return err
	}
}

// indexOf returns the position of the first recorded command containing every
// one of want, or -1.
func (r *cmdOrder) indexOf(want ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, line := range r.seen {
		all := true
		for _, w := range want {
			if !strings.Contains(line, w) {
				all = false
				break
			}
		}
		if all {
			return i
		}
	}
	return -1
}

// TestOnSessionsNeedClaim_TheEjectedSessionGetsItsWayBackBeforeItsVerdict pins
// the ORDER of the two writes settleEjectedClaim makes, which is the only thing
// standing between a crash mid-settlement and a claim that no longer exists for
// anybody.
//
// The two orders are indistinguishable once both writes land, so this asserts on
// the wire rather than on the state. What it protects: `claim_tx_error` is
// terminal, loadExistingSessions never loads a terminal session back into
// activeSessions, and the reconciler only ever looks at PERSISTED entries -- so
// a process that dies holding the terminal verdict and no entry has put the
// claim beyond the reach of both recovery paths, and this message never
// travelled, so there is nothing on-chain either.
//
// Injection: move the persist block back below the OnClaimTxError block in
// settleEjectedClaim. Red, naming which write came first.
func TestOnSessionsNeedClaim_TheEjectedSessionGetsItsWayBackBeforeItsVerdict(t *testing.T) {
	spy := &batchSpy{errs: []error{namedRejection(1)}}
	lc, sessionStore, rebroadcast, _, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb", "sess-ccc")

	redisStore, ok := sessionStore.(*RedisSessionStore)
	if !ok {
		t.Fatalf("the fixture must hand back a Redis-backed store to compute its keys, got %T", sessionStore)
	}

	// Installed AFTER seeding, so the record holds the cycle's writes only.
	rec := &cmdOrder{}
	redisStore.redisClient.AddHook(rec)

	if _, err := lc.OnSessionsNeedClaim(context.Background(), snapshots); err != nil {
		t.Fatalf("the surviving batch was accepted, so the cycle must not error: %v", err)
	}
	if len(spy.calls) == 0 {
		t.Fatalf("nothing was sent")
	}
	ejectedID := spy.calls[0][1]

	groupKey := rebroadcast.groupKey(RebroadcastPhaseClaim, "pokt1eject", 100)
	entryAt := rec.indexOf("hset", groupKey, ejectedID)
	verdictAt := rec.indexOf("evalsha", redisStore.sessionKey(ejectedID))
	if verdictAt < 0 {
		// EVAL is the fallback go-redis takes the first time a script has not
		// been cached by the server; asserting only on EVALSHA would make this
		// test's meaning depend on which test ran first.
		verdictAt = rec.indexOf("eval ", redisStore.sessionKey(ejectedID))
	}

	if entryAt < 0 {
		t.Fatalf("the ejected session %q never got a rebroadcast entry; recorded: %v", ejectedID, rec.seen)
	}
	if verdictAt < 0 {
		t.Fatalf("the ejected session %q never got its terminal verdict; recorded: %v", ejectedID, rec.seen)
	}
	if entryAt > verdictAt {
		t.Errorf(
			"the ejected session %q was marked terminal (position %d) BEFORE its rebroadcast entry landed (position %d): "+
				"a crash in between leaves the claim unreachable by the lifecycle and by the reconciler",
			ejectedID, verdictAt, entryAt,
		)
	}
}

// TestOnSessionsNeedClaim_AnEjectedClaimWithNoWayBackSaysSo covers the half the
// ordering cannot: a rebroadcast store that is reachable and REFUSES the write.
// No ordering helps there -- the entry does not exist whichever write went
// first -- so the requirement is that the verdict stop claiming a retry that
// will never come.
//
// The store is given its OWN client so the failure lands on the rebroadcast
// write and nowhere else; failing the shared client would take the session
// state write down with it and test a different, wider outage.
//
// Injection: call RecordClaimTxError unconditionally in settleEjectedClaim.
// Red, because the unrecoverable series never moves.
func TestOnSessionsNeedClaim_AnEjectedClaimWithNoWayBackSaysSo(t *testing.T) {
	spy := &batchSpy{errs: []error{namedRejection(1)}}
	lc, _, _, _, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb", "sess-ccc")

	deadClient, _ := newTestRedis(t)
	testredis.NewFailSwitch(deadClient).Fail("rebroadcast store is unreachable")
	lc.rebroadcastStore = NewRebroadcastStore(deadClient, time.Hour)

	// Which session is ejected is only known from what travelled, so both
	// series are sampled for every session and read back afterwards.
	beforeLost := map[string]float64{}
	beforePending := map[string]float64{}
	for _, snap := range snapshots {
		beforeLost[snap.SessionID] = testutil.ToFloat64(
			sessionsFailedTotal.WithLabelValues(snap.SupplierOperatorAddress, snap.ServiceID, "claim_ejected_unrecoverable"),
		)
		beforePending[snap.SessionID] = testutil.ToFloat64(
			sessionsFailedTotal.WithLabelValues(snap.SupplierOperatorAddress, snap.ServiceID, "claim_tx_error"),
		)
	}

	if _, err := lc.OnSessionsNeedClaim(context.Background(), snapshots); err != nil {
		t.Fatalf("the surviving batch was accepted, so the cycle must not error: %v", err)
	}
	if len(spy.calls) == 0 {
		t.Fatalf("nothing was sent")
	}
	ejected := snapshots[0]
	ejectedID := spy.calls[0][1]
	for _, snap := range snapshots {
		if snap.SessionID == ejectedID {
			ejected = snap
		}
	}

	lost := testutil.ToFloat64(
		sessionsFailedTotal.WithLabelValues(ejected.SupplierOperatorAddress, ejected.ServiceID, "claim_ejected_unrecoverable"),
	) - beforeLost[ejectedID]
	if lost != 1 {
		t.Errorf(
			"the ejected session %q got no rebroadcast entry, so its loss is FINAL and must be counted as such: "+
				"claim_ejected_unrecoverable moved by %v, want 1",
			ejectedID, lost,
		)
	}

	// The teeth of the distinction: counting it under the ordinary reason is
	// what makes a permanent loss look like one the reconciler still owes.
	pending := testutil.ToFloat64(
		sessionsFailedTotal.WithLabelValues(ejected.SupplierOperatorAddress, ejected.ServiceID, "claim_tx_error"),
	) - beforePending[ejectedID]
	if pending != 0 {
		t.Errorf(
			"the ejected session %q must NOT be counted as a retryable claim_tx_error when nothing can retry it, moved by %v",
			ejectedID, pending,
		)
	}
}

// TestOnSessionsNeedClaim_AnEjectionWithTheReconcilerOffIsAnOrdinaryLoss pins the
// OTHER half of the verdict condition, which the two tests above leave inert: a
// nil rebroadcast store means there is no reconciler configured at all, and that
// is NOT the same event as a store that was asked and refused.
//
// Both end with no entry and no retry, which is exactly why the distinction has
// to be asserted rather than argued: an operator who turned the reconciler off
// already knows nothing will resend, so stamping every ejection as a final loss
// would bury the case that IS a surprise -- a store that was there and failed --
// under a stream of losses they configured on purpose.
//
// Injection: drop `|| lc.rebroadcastStore == nil` from the condition in
// settleEjectedClaim. Red, because a configured absence starts being reported as
// an unrecoverable failure.
func TestOnSessionsNeedClaim_AnEjectionWithTheReconcilerOffIsAnOrdinaryLoss(t *testing.T) {
	spy := &batchSpy{errs: []error{namedRejection(1)}}
	lc, _, _, _, snapshots := ejectionFixture(t, spy, "sess-aaa", "sess-bbb", "sess-ccc")

	// The reconciler is not wired at all -- the shape an operator gets by
	// disabling it, not a store that failed.
	lc.rebroadcastStore = nil

	beforeFinal := map[string]float64{}
	beforeOrdinary := map[string]float64{}
	for _, snap := range snapshots {
		beforeFinal[snap.SessionID] = testutil.ToFloat64(
			sessionsFailedTotal.WithLabelValues(snap.SupplierOperatorAddress, snap.ServiceID, "claim_ejected_unrecoverable"),
		)
		beforeOrdinary[snap.SessionID] = testutil.ToFloat64(
			sessionsFailedTotal.WithLabelValues(snap.SupplierOperatorAddress, snap.ServiceID, "claim_tx_error"),
		)
	}

	if _, err := lc.OnSessionsNeedClaim(context.Background(), snapshots); err != nil {
		t.Fatalf("the surviving batch was accepted, so the cycle must not error: %v", err)
	}
	if len(spy.calls) == 0 {
		t.Fatalf("nothing was sent")
	}
	ejectedID := spy.calls[0][1]
	ejected := snapshots[0]
	for _, snap := range snapshots {
		if snap.SessionID == ejectedID {
			ejected = snap
		}
	}

	// Asserted FIRST: without it the test could pass because the ejection never
	// happened at all, which is the way this assertion would rot.
	ordinary := testutil.ToFloat64(
		sessionsFailedTotal.WithLabelValues(ejected.SupplierOperatorAddress, ejected.ServiceID, "claim_tx_error"),
	) - beforeOrdinary[ejectedID]
	// Errorf and not Fatalf: when this one fails the NEXT assertion is what says
	// where the count went instead, and stopping here would leave the reader
	// hunting for a missing increment rather than reading the misplaced one.
	if ordinary != 1 {
		t.Errorf(
			"the ejected session %q must still be counted lost exactly once under the ordinary reason, moved by %v",
			ejectedID, ordinary,
		)
	}

	final := testutil.ToFloat64(
		sessionsFailedTotal.WithLabelValues(ejected.SupplierOperatorAddress, ejected.ServiceID, "claim_ejected_unrecoverable"),
	) - beforeFinal[ejectedID]
	if final != 0 {
		t.Errorf(
			"a reconciler the operator disabled is a configured absence, not a failure to report: "+
				"claim_ejected_unrecoverable moved by %v for %q, want 0",
			final, ejectedID,
		)
	}
}
