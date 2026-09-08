//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/pocket-relay-miner/tx"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// --- mocks -------------------------------------------------------------------

type mockResubmitter struct {
	mu       sync.Mutex
	calls    []string
	attempts int
	failNext bool
	// failWith replaces the generic failure, so a test can distinguish a chain
	// rejection from never having reached the chain at all.
	failWith error
	// burnGroupBudget makes the resend consume the caller's whole context
	// instead of returning at once — the shape of a slow node, and the only way
	// to reach the code that persists the attempt with an expired context.
	burnGroupBudget bool
}

func (m *mockResubmitter) ResubmitMessage(ctx context.Context, phase RebroadcastPhase, supplier string, msgBytes []byte, _ int64) (string, error) {
	m.mu.Lock()
	burn := m.burnGroupBudget
	m.mu.Unlock()
	if burn {
		<-ctx.Done()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts++
	if m.failWith != nil {
		return "", m.failWith
	}
	if m.failNext {
		return "", fmt.Errorf("resubmit boom")
	}
	m.calls = append(m.calls, fmt.Sprintf("%s/%s/%s", phase, supplier, string(msgBytes)))
	return "newhash-" + string(msgBytes), nil
}

func (m *mockResubmitter) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// attemptCount returns the total number of ResubmitMessage invocations,
// including failed ones (count() only tallies successes).
func (m *mockResubmitter) attemptCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attempts
}

type capturedOutcome struct {
	supplier, sessionID, outcome string
	height                       int64
}

type reconcilerHarness struct {
	r           *InclusionReconciler
	store       *RebroadcastStore
	resub       *mockResubmitter
	mu          sync.Mutex
	outcomes    []capturedOutcome
	rebroadcast []string
	// onChain is what the chain reports for the supplier, in the same shape the
	// unified read returns: presence answers the claim phase, the value answers
	// the proof phase.
	onChain    map[string]query.SessionProofState
	onChainErr error
	// fetchCalls counts trips to the chain. The reconciler must make ONE per
	// (supplier, height) however many groups and phases want the answer.
	fetchCalls atomic.Int64
	// onChainWaitForDeadline makes the inclusion query block until the group
	// context expires. It waits for the CONDITION, not for a duration, so the
	// deadline is guaranteed to have fired when the loop below it runs -- no
	// sleep, no timing race.
	onChainWaitForDeadline bool
	// rc is the Redis client behind the store, kept so a test can close it and
	// exercise the paths that abandon a whole group when the store is gone.
	rc *redisutil.Client
	// outcomeErr, when set, makes recordOutcome fail — the reconciler must
	// then KEEP the pending entry so the observation gets another block.
	outcomeErr  error
	windowClose int64
}

// Window model used by the tests: a session "submitted" at height 110 with the
// window closing at 130 → midpoint = 110 + (130-110)/2 = 120; safety=1 caps
// resends to height < 129. So a SENT entry (OrigTxHash != "") resends in
// [120,128]; a NEVER-SENT entry (OrigTxHash == "") resends from 111 (submit+1).
const (
	testWindowClose = int64(130)
	testSubmit      = int64(110)
	testMid         = int64(120)
)

// phaseVerdict returns the SAME interpretation production wires, so a test that
// passes here is a test about the reconciler and not about a stand-in. Keeping a
// second copy would let the two drift, and the drift would be invisible: both
// phases would still answer found/missing, just not the way the miner does.
func phaseVerdict(p RebroadcastPhase) func(query.SessionProofState, bool) inclusionVerdict {
	if p == RebroadcastPhaseClaim {
		return func(_ query.SessionProofState, present bool) inclusionVerdict {
			if present {
				return verdictFound
			}
			return verdictMissing
		}
	}
	return func(state query.SessionProofState, _ bool) inclusionVerdict {
		if state == query.SessionProofValidated {
			return verdictFound
		}
		return verdictMissing
	}
}

// fetchStates is the single on-chain read both phases share, so a test that makes
// it fail or hang exercises the one query the reconciler really makes.
func (h *reconcilerHarness) fetchStates(ctx context.Context, _ string) (map[string]query.SessionProofState, error) {
	h.fetchCalls.Add(1)
	h.mu.Lock()
	wait := h.onChainWaitForDeadline
	h.mu.Unlock()
	if wait {
		<-ctx.Done()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.onChainErr != nil {
		return nil, h.onChainErr
	}
	cp := make(map[string]query.SessionProofState, len(h.onChain))
	for k, v := range h.onChain {
		cp[k] = v
	}
	return cp, nil
}

func newReconcilerHarness(t *testing.T, safetyBlocks int64) *reconcilerHarness {
	t.Helper()
	rc, _ := newTestRedis(t)

	h := &reconcilerHarness{
		rc:          rc,
		store:       NewRebroadcastStore(rc, time.Hour),
		resub:       &mockResubmitter{},
		onChain:     map[string]query.SessionProofState{},
		windowClose: testWindowClose,
	}

	mkPhase := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return h.windowClose },
			verdict:           phaseVerdict(p),
			recordOutcome: func(_ context.Context, _ rebroadcastEntry, supplier string, _ int64, sessionID, outcome string, height int64) error {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.outcomes = append(h.outcomes, capturedOutcome{supplier, sessionID, outcome, height})
				return h.outcomeErr
			},
			recordRebroadcast: func(supplier, _, result string) {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.rebroadcast = append(h.rebroadcast, supplier+"/"+result)
			},
		}
	}

	cfg := DefaultInclusionReconcilerConfig()
	cfg.MaxConcurrent = 4
	cfg.RebroadcastSafetyBlocks = safetyBlocks
	cfg.PerGroupTimeout = 2 * time.Second

	h.r = NewInclusionReconciler(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		&mockSharedQueryClient{},
		h.store,
		h.resub,
		mkPhase(RebroadcastPhaseClaim),
		mkPhase(RebroadcastPhaseProof),
		h.fetchStates,
		cfg,
	)
	t.Cleanup(func() { _ = h.r.Close() })
	return h
}

// seed: a SUCCESSFULLY-broadcast entry (OrigTxHash set) → mid-window resend.
func (h *reconcilerHarness) seed(t *testing.T, supplier string, sessionEnd int64, sessionID string, submitHeight int64) {
	t.Helper()
	h.put(t, RebroadcastPhaseProof, supplier, sessionEnd, sessionID, submitHeight, "tx-"+sessionID)
}

// seedNeverSent: a build-OK-but-submit-FAILED entry (OrigTxHash empty) → early
// emergency resend.
func (h *reconcilerHarness) seedNeverSent(t *testing.T, supplier string, sessionEnd int64, sessionID string, submitHeight int64) {
	t.Helper()
	h.put(t, RebroadcastPhaseProof, supplier, sessionEnd, sessionID, submitHeight, "")
}

func (h *reconcilerHarness) put(t *testing.T, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string, submitHeight int64, origTxHash string) {
	t.Helper()
	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:     []byte(sessionID),
		SubmitHeight: submitHeight,
		TxHash:       origTxHash,
		OrigTxHash:   origTxHash,
	})
	require.NoError(t, err)
	require.NoError(t, h.store.Put(context.Background(), phase, supplier, sessionEnd, sessionID, b))
}

func (h *reconcilerHarness) entry(t *testing.T, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string) rebroadcastEntry {
	t.Helper()
	m, err := h.store.List(context.Background(), phase, supplier, sessionEnd)
	require.NoError(t, err)
	raw, ok := m[sessionID]
	require.True(t, ok, "entry must still be present")
	e, err := unmarshalRebroadcastEntry(raw)
	require.NoError(t, err)
	return e
}

func (h *reconcilerHarness) getOutcomes() []capturedOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedOutcome, len(h.outcomes))
	copy(out, h.outcomes)
	return out
}

func (h *reconcilerHarness) pendingCount(t *testing.T, supplier string, sessionEnd int64) int {
	t.Helper()
	m, err := h.store.List(context.Background(), RebroadcastPhaseProof, supplier, sessionEnd)
	require.NoError(t, err)
	return len(m)
}

// --- tests -------------------------------------------------------------------

const (
	hSupplier = "pokt1abc"
	hEnd      = int64(110)
)

// On-chain found → outcome found, entry cleared, no rebroadcast.
func TestReconciler_Found(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.onChain = map[string]query.SessionProofState{"s1": query.SessionProofValidated}

	h.r.OnBlock(testMid)

	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionFound, outcomes[0].outcome)
	require.Equal(t, testMid, outcomes[0].height)
	require.Equal(t, 0, h.resub.count(), "found proof must not rebroadcast")
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd), "found entry must be cleared")
}

// An observed inclusion that could NOT be fully acted upon must KEEP its entry.
//
// This is the difference between a lost reward and a slash. The `found` outcome
// is what puts a session whose broadcast reported failure back into `claimed`
// so its proof goes out; if a transient Redis error while doing that also
// cleared the entry, the observation would be gone for good and the session
// would stay terminal with its claim on-chain — which is exactly the slash the
// whole path exists to prevent. Keeping the entry costs one more pass.
func TestReconciler_FoundButNotRecorded_KeepsEntryForRetry(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.onChain = map[string]query.SessionProofState{"s1": query.SessionProofValidated}
	h.outcomeErr = fmt.Errorf("redis unavailable while reactivating")

	h.r.OnBlock(testMid)

	require.Len(t, h.getOutcomes(), 1, "the outcome was still observed")
	require.Equal(t, 1, h.pendingCount(t, hSupplier, hEnd),
		"a found-but-unrecorded entry must survive for the next block")

	// Next block, the write succeeds: the entry is then cleared normally.
	h.outcomeErr = nil
	h.r.OnBlock(testMid + 1)
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd),
		"once recorded, the entry is cleared as usual")
}

// Sent-but-missing resends exactly once AT the window midpoint, not before.
func TestReconciler_SentResendsAtMidWindow(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testMid - 1) // before midpoint → no resend
	require.Equal(t, 0, h.resub.count(), "sent entry must wait until midpoint")

	h.r.OnBlock(testMid) // at midpoint → one resend
	require.Equal(t, 1, h.resub.count(), "sent entry resends at midpoint")
	require.Equal(t, 1, h.pendingCount(t, hSupplier, hEnd), "entry retained for continued tracking")

	// The block right after the midpoint is the one that used to be a storm: the
	// release condition `height >= threshold` stays true on every later block, so
	// what holds the second attempt back is the SPACING and not the cap. Before
	// the spacing existed this assertion passed too -- for the other reason -- so
	// the message names which one is doing the work.
	h.r.OnBlock(testMid + 1)
	require.Equal(t, 1, h.resub.count(),
		"the second resend waits for its spacing, it is not blocked by the cap")

	h.r.OnBlock(testMid + 2)
	require.Equal(t, 2, h.resub.count(),
		"the second resend goes out two blocks after the first: one to be included, one for the oracle to see it")

	// Now it IS the cap.
	h.r.OnBlock(testMid + 3)
	h.r.OnBlock(testMid + 4)
	require.Equal(t, 2, h.resub.count(), "resends are capped at MaxRebroadcasts")
}

// resendHeights drives every block in [from, to] and returns the heights at
// which a resend actually went out. It is only meaningful for a harness holding
// ONE session: two sessions resending in the same block would show up once.
func resendHeights(h *reconcilerHarness, from, to int64) []int64 {
	var out []int64
	prev := h.resub.attemptCount()
	for height := from; height <= to; height++ {
		h.r.OnBlock(height)
		if n := h.resub.attemptCount(); n > prev {
			out = append(out, height)
			prev = n
		}
	}
	return out
}

// TestReconciler_ResendCalendarOnTheClaimPhase pins the whole schedule at once,
// by the heights the resends actually left at rather than by a count.
//
// It runs on the CLAIM phase deliberately: every other test in this file seeds
// the proof phase, and the two window widths are INDEPENDENT governance
// parameters (poktroll x/shared: claim and proof close offsets are separate
// fields), so a calendar proven on one says nothing about the other.
func TestReconciler_ResendCalendarOnTheClaimPhase(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.put(t, RebroadcastPhaseClaim, hSupplier, hEnd, "s1", testSubmit, "tx-s1")

	got := resendHeights(h, testSubmit, testWindowClose)

	// testMid is the midpoint for a tx that WAS broadcast; the second follows
	// two blocks later. Nothing at testMid+1, and nothing at all once the guard
	// closes -- the last permitted height is windowClose-safety-1.
	require.Equal(t, []int64{testMid, testMid + 2}, got,
		"the claim-phase calendar must be the midpoint and two blocks later, and nothing else")
}

// A persistently FAILING resend (e.g. a CUPR-doomed claim whose gas simulation
// always fails) must be bounded by MaxRebroadcasts, NOT retried (and re-logged)
// on every block until the window closes. The attempt is counted even when the
// resubmit errors.
func TestReconciler_FailingResendIsBounded_NoPerBlockStorm(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failNext = true // every resend fails
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	// Asserted as the exact heights and not as a bound. `LessOrEqual(n, cap)`
	// is satisfied by ZERO attempts, so it cannot tell "the cap held" from "the
	// resend never fired at all" -- and the second is the failure that costs the
	// claim. The heights say both things at once: how many, and spaced how.
	got := resendHeights(h, testSubmit, testWindowClose)

	require.Equal(t, []int64{testSubmit + 1, testSubmit + 3}, got,
		"a never-broadcast entry resends after a 1-block grace and again two blocks later, then stops: "+
			"a persistently failing resend must be bounded by MaxRebroadcasts, not retried every block")
}

// A never-broadcast (submit-failed) entry resends EARLY (submit+1), not at mid.
func TestReconciler_NeverSentResendsEarly(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testSubmit) // == submit → grace, no resend
	require.Equal(t, 0, h.resub.count(), "no resend in the submit block")

	h.r.OnBlock(testSubmit + 1) // submit+1 → emergency resend (well before midpoint)
	require.Equal(t, 1, h.resub.count(), "never-sent entry resends promptly, not at mid-window")
}

// Inside the safety margin of window-close → no resend, not yet terminal.
func TestReconciler_SafetyMarginNoRebroadcast(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testWindowClose - 1) // 129: 129 < 130-1=129 false → no resend; 129 > 130 false → not missing
	require.Equal(t, 0, h.resub.count(), "must not rebroadcast inside the safety margin")
	require.Empty(t, h.getOutcomes())
	require.Equal(t, 1, h.pendingCount(t, hSupplier, hEnd))
}

// Window closed, still missing → outcome missing, cleared, no rebroadcast.
func TestReconciler_WindowClosedMissing(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testWindowClose + 1)

	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionMissing, outcomes[0].outcome)
	require.Equal(t, 0, h.resub.count())
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd))
}

// On-chain query error while window open → BLIND resend (degraded), gated by the
// same midpoint cadence; retained, no terminal outcome.
func TestReconciler_QueryErrorWindowOpen_BlindRebroadcast(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.onChainErr = fmt.Errorf("node down")

	h.r.OnBlock(testMid)

	require.Empty(t, h.getOutcomes(), "transient error must not produce a terminal outcome")
	require.Equal(t, 1, h.resub.count(), "query down + window open must blind-rebroadcast rather than forfeit")
	require.Equal(t, 1, h.pendingCount(t, hSupplier, hEnd), "entry retained for retry")
}

// Blind resend still respects the cadence (no resend before the threshold).
func TestReconciler_QueryErrorWindowOpen_RespectsCadence(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.onChainErr = fmt.Errorf("node down")

	h.r.OnBlock(testMid - 1) // before midpoint
	require.Equal(t, 0, h.resub.count(), "blind resend must still honor the midpoint cadence")
}

// On-chain query error after window close → poll_error recorded, cleared.
func TestReconciler_QueryErrorWindowClosed(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.onChainErr = fmt.Errorf("node down")

	h.r.OnBlock(testWindowClose + 1)

	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionPollErr, outcomes[0].outcome)
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd))
}

// Duplicate / non-advancing block height is a no-op (single-flight + dedupe).
func TestReconciler_BlockHeightDedupe(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testMid)
	require.Equal(t, 1, h.resub.count())

	h.r.OnBlock(testMid)     // same height → skip
	h.r.OnBlock(testMid - 1) // lower → skip
	require.Equal(t, 1, h.resub.count(), "non-advancing heights must not re-run the pass")
}

// A successful resend refreshes the entry's latest TxHash and increments the
// persisted resend count (so the cap survives across blocks / failover), while
// OrigTxHash stays put (claim outcome is keyed by it).
func TestReconciler_ResendRefreshesHashKeepsOrig(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testMid)

	e := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Equal(t, "newhash-s1", e.TxHash, "latest tx hash refreshed")
	require.Equal(t, "tx-s1", e.OrigTxHash, "original tx hash preserved for outcome lookup")
	require.Equal(t, 1, e.Rebroadcasts, "resend count persisted")
}

// One group, mixed sessions: found recorded+cleared, missing resent+retained.
func TestReconciler_MixedFoundAndMissing(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit) // found
	h.seed(t, hSupplier, hEnd, "s2", testSubmit) // missing
	h.onChain = map[string]query.SessionProofState{"s1": query.SessionProofValidated}

	h.r.OnBlock(testMid)

	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, "s1", outcomes[0].sessionID)
	require.Equal(t, inclusionFound, outcomes[0].outcome)
	require.Equal(t, 1, h.resub.count(), "only the missing session resends")
	require.Equal(t, 1, h.pendingCount(t, hSupplier, hEnd), "found cleared, missing retained")
}

// Found after window close still records found (found precedes window-closed).
func TestReconciler_FoundAfterWindowClose(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.onChain = map[string]query.SessionProofState{"s1": query.SessionProofValidated}

	h.r.OnBlock(testWindowClose + 1)

	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionFound, outcomes[0].outcome, "found must win over window-closed-missing")
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd))
}

// The CLAIM phase is exercised end-to-end (not just proof).
func TestReconciler_ClaimPhaseResends(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.put(t, RebroadcastPhaseClaim, hSupplier, hEnd, "c1", testSubmit, "tx-c1")

	h.r.OnBlock(testMid)

	require.Equal(t, 1, h.resub.count(), "missing claim in open window must resend")
	m, err := h.store.List(context.Background(), RebroadcastPhaseClaim, hSupplier, hEnd)
	require.NoError(t, err)
	require.Len(t, m, 1, "claim entry retained for continued tracking")
}

// Corrupt (non-JSON) stored payload is dropped without panic; no resend/outcome.
func TestReconciler_CorruptEntryDropped(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	require.NoError(t, h.store.Put(context.Background(), RebroadcastPhaseProof, hSupplier, hEnd, "bad", []byte("not-json")))

	h.r.OnBlock(testMid)

	require.Equal(t, 0, h.resub.count())
	require.Empty(t, h.getOutcomes())
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd), "corrupt entry must be cleared")
}

// MaxRebroadcasts=0 → observe-only: never resend, but still record outcomes.
func TestReconciler_ObserveOnly(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	_ = h.r.Close() // rebuild with MaxRebroadcasts=0
	cfg := DefaultInclusionReconcilerConfig()
	cfg.MaxConcurrent = 4
	cfg.MaxRebroadcasts = 0
	cfg.RebroadcastSafetyBlocks = 1
	cfg.PerGroupTimeout = 2 * time.Second
	mkPhase := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return h.windowClose },
			verdict:           phaseVerdict(p),
			recordOutcome: func(_ context.Context, _ rebroadcastEntry, supplier string, _ int64, sessionID, outcome string, height int64) error {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.outcomes = append(h.outcomes, capturedOutcome{supplier, sessionID, outcome, height})
				return h.outcomeErr
			},
			recordRebroadcast: func(string, string, string) {},
		}
	}
	h.r = NewInclusionReconciler(logging.NewLoggerFromConfig(logging.DefaultConfig()), &mockSharedQueryClient{}, h.store, h.resub,
		mkPhase(RebroadcastPhaseClaim), mkPhase(RebroadcastPhaseProof),
		func(context.Context, string) (map[string]query.SessionProofState, error) {
			return map[string]query.SessionProofState{}, nil
		}, cfg)
	t.Cleanup(func() { _ = h.r.Close() })

	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.r.OnBlock(testMid)
	require.Equal(t, 0, h.resub.count(), "observe-only must never resend")

	h.r.OnBlock(testWindowClose + 1)
	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionMissing, outcomes[0].outcome, "observe-only still records the (missing) outcome")
}

// The ownership filter skips suppliers this replica does not own.
func TestReconciler_OwnershipFilter(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.r.SetOwnershipFilter(func(string) bool { return false }) // owns nothing

	h.r.OnBlock(testMid)

	require.Equal(t, 0, h.resub.count(), "non-owner must not resend")
	require.Empty(t, h.getOutcomes(), "non-owner must not record outcomes")
	require.Equal(t, 1, h.pendingCount(t, hSupplier, hEnd), "non-owner must not clear another replica's state")

	h.r.SetOwnershipFilter(func(s string) bool { return s == hSupplier })
	h.r.OnBlock(testMid + 1)
	require.Equal(t, 1, h.resub.count(), "owner reconciles its supplier")
}

// Mixed ownership in one pass: only owned suppliers reconciled.
func TestReconciler_OwnershipFilter_Mixed(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, "pokt1mine", hEnd, "s1", testSubmit)
	h.seed(t, "pokt1theirs", hEnd, "s2", testSubmit)
	h.r.SetOwnershipFilter(func(s string) bool { return s == "pokt1mine" })

	h.r.OnBlock(testMid)

	require.Equal(t, 1, h.resub.count(), "only the owned supplier resends")
	require.Equal(t, 1, h.pendingCount(t, "pokt1theirs", hEnd), "non-owned supplier left untouched")
}

// Concurrent OnBlock at the same height: single-flight → exactly one resend.
func TestReconciler_ConcurrentOnBlock(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.r.OnBlock(testMid)
		}()
	}
	wg.Wait()

	require.Equal(t, 1, h.resub.count(), "single-flight + dedupe: exactly one resend at one height")
	require.Equal(t, 1, h.pendingCount(t, hSupplier, hEnd))
}

// Multiple suppliers reconciled in one pass.
func TestReconciler_MultipleGroups(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, "pokt1aaa", hEnd, "s1", testSubmit)
	h.seed(t, "pokt1bbb", hEnd, "s2", testSubmit)
	h.seed(t, "pokt1ccc", hEnd, "s3", testSubmit)

	h.r.OnBlock(testMid)
	require.Equal(t, 3, h.resub.count(), "all groups must be reconciled in one pass")
}

// newPeerReconciler builds a SECOND reconciler that shares ONLY Redis (the store)
// with the harness — no shared in-memory state. It models a different replica
// taking over a supplier after the original owner died. onChain controls what
// that replica sees on-chain; ownsAll gates the ownership filter.
func (h *reconcilerHarness) newPeerReconciler(t *testing.T, resub *mockResubmitter, onChain map[string]query.SessionProofState, owns func(string) bool) *InclusionReconciler {
	t.Helper()
	mkPhase := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return h.windowClose },
			verdict:           phaseVerdict(p),
			recordOutcome:     func(context.Context, rebroadcastEntry, string, int64, string, string, int64) error { return nil },
			recordRebroadcast: func(string, string, string) {},
		}
	}
	cfg := DefaultInclusionReconcilerConfig()
	cfg.MaxConcurrent = 4
	cfg.RebroadcastSafetyBlocks = 1
	cfg.PerGroupTimeout = 2 * time.Second
	peer := NewInclusionReconciler(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		&mockSharedQueryClient{}, h.store, resub,
		mkPhase(RebroadcastPhaseClaim), mkPhase(RebroadcastPhaseProof),
		func(context.Context, string) (map[string]query.SessionProofState, error) {
			cp := make(map[string]query.SessionProofState, len(onChain))
			for k, v := range onChain {
				cp[k] = v
			}
			return cp, nil
		}, cfg,
	)
	peer.SetOwnershipFilter(owns)
	t.Cleanup(func() { _ = peer.Close() })
	return peer
}

// HA failover: a supplier's pending rebroadcast state lives in Redis, NOT in the
// owning replica's memory. When that replica dies, a different replica that takes
// ownership resumes the resend straight from Redis — no forfeit. This is the
// invariant the live "kill the owner mid-window" test exercises, pinned here
// deterministically (no cluster, no timing).
func TestReconciler_HA_PeerRecoversNeverSentFromRedis(t *testing.T) {
	// "Replica A" persists a build-OK-but-submit-FAILED proof (OrigTxHash="") and
	// then crashes — it never runs OnBlock for it.
	h := newReconcilerHarness(t, 1)
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	// "Replica B": fresh reconciler, shares only Redis, took over the supplier,
	// still sees the proof missing on-chain.
	resubB := &mockResubmitter{}
	b := h.newPeerReconciler(t, resubB, map[string]query.SessionProofState{}, func(s string) bool { return s == hSupplier })

	// B recovers A's persisted entry from Redis and self-heals it (never-sent →
	// resend at submit+1, before the midpoint).
	b.OnBlock(testSubmit + 1)
	require.Equal(t, 1, resubB.count(), "peer must recover the persisted entry from Redis and resend")

	// The resend count persisted by B holds the MaxRebroadcasts cap across blocks
	// (and would across a further failover): no second resend.
	b.OnBlock(testSubmit + 2)
	require.Equal(t, 1, resubB.count(), "resend cap survives via the persisted count")
	require.Equal(t, 1, h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1").Rebroadcasts,
		"resend count is persisted on the Redis entry (HA-safe cap)")
}

// HA no-double-submit: while one replica owns and resends a supplier, a replica
// that does NOT own it must not also resend the same pending entry — even though
// the entry is visible to it in shared Redis. Ownership (SupplierClaimer SetNX)
// is the single-writer guarantee.
func TestReconciler_HA_NonOwnerPeerDoesNotDoubleSubmit(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	// Owner B resends.
	resubB := &mockResubmitter{}
	b := h.newPeerReconciler(t, resubB, map[string]query.SessionProofState{}, func(s string) bool { return s == hSupplier })
	// Non-owner C sees the same Redis entry but owns nothing.
	resubC := &mockResubmitter{}
	c := h.newPeerReconciler(t, resubC, map[string]query.SessionProofState{}, func(string) bool { return false })

	b.OnBlock(testSubmit + 1)
	c.OnBlock(testSubmit + 1)

	require.Equal(t, 1, resubB.count(), "the owner resends exactly once")
	require.Equal(t, 0, resubC.count(), "a non-owner must never resend another replica's supplier (no double-submit)")
}

// TestReconciler_SaturatedResendDoesNotBurnTheAttempt is the assertion that
// makes the named sentinel load-bearing rather than decorative.
//
// MaxRebroadcasts defaults to ONE. The counter is incremented regardless of
// outcome, for a good reason -- a doomed claim must not re-fire every block --
// but "the chain rejected it" and "we never reached the chain" are not the same
// thing, and only the sentinel can tell them apart. A resend that died waiting
// for a broadcast permit never signed anything and never sent anything; counting
// it burns the single resend the claim had, on nothing.
func TestReconciler_SaturatedResendDoesNotBurnTheAttempt(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.resub.failWith = fmt.Errorf("resend: %w", tx.ErrTxConcurrencySaturated)

	h.r.OnBlock(testMid)
	require.Equal(t, 1, h.resub.attemptCount(), "the reconciler should have tried")

	// The entry survives AND keeps its budget: the next block finds it exactly
	// where it was, with its one resend unspent.
	require.Equal(t, 1, h.pendingCount(t, hSupplier, hEnd), "entry retained for the next block")

	h.resub.failWith = nil
	h.r.OnBlock(testMid + 1)
	require.Equal(t, 1, h.resub.count(),
		"the saturated attempt burned the only resend: the claim can never be retried")
}

// TestReconciler_RejectedResendDoesBurnTheAttempt is the other half, and the
// pair is what proves the exemption discriminates rather than simply never
// counting. A chain rejection MUST consume the budget, or a doomed claim
// re-fires on every block until the window closes.
func TestReconciler_RejectedResendDoesBurnTheAttempt(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.resub.failWith = fmt.Errorf("chain said no")

	h.r.OnBlock(testMid)
	require.Equal(t, 1, h.resub.attemptCount())

	h.resub.failWith = nil
	h.r.OnBlock(testMid + 1)
	require.Equal(t, 0, h.resub.count(),
		"a rejected resend did not consume its attempt: a doomed claim would re-fire every block")
}

// TestReconciler_AttemptIsRecordedEvenWhenTheGroupBudgetIsSpent closes the
// resend cap from its other side.
//
// The send and the write that records it used to share the group's context. A
// resend slow enough to spend PerGroupTimeout therefore happened AND went
// unrecorded, so the next block resent again -- the cap held from neither
// direction: over-counting attempts that never reached the chain, under-counting
// the ones that did.
func TestReconciler_AttemptIsRecordedEvenWhenTheGroupBudgetIsSpent(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.r.cfg.PerGroupTimeout = 50 * time.Millisecond
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.resub.burnGroupBudget = true

	h.r.OnBlock(testMid)
	require.Equal(t, 1, h.resub.attemptCount(), "the resend should have run")

	// The attempt must be on record, so the next block does NOT resend.
	h.resub.burnGroupBudget = false
	h.r.OnBlock(testMid + 1)
	require.Equal(t, 1, h.resub.attemptCount(),
		"the attempt was not persisted: the cap does not survive a resend that spends the group budget")
}

// TestReconciler_PoolIsCappedByTxConcurrency: a worker above the transaction
// client's permit count can only ever start in order to park, and a parked
// worker spends the group's budget without reaching the chain. The pool is
// sized from the same number rather than from a constant that can drift.
func TestReconciler_PoolIsCappedByTxConcurrency(t *testing.T) {
	tests := []struct {
		name            string
		maxConcurrent   int
		txMaxConcurrent int
		want            int
	}{
		{"capped by the semaphore", 64, 32, 32},
		{"already below it", 8, 32, 8},
		{"unset tx limit leaves it alone", 64, 0, 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultInclusionReconcilerConfig()
			cfg.MaxConcurrent = tt.maxConcurrent
			cfg.TxMaxConcurrent = tt.txMaxConcurrent

			r := NewInclusionReconciler(
				logging.NewLoggerFromConfig(logging.DefaultConfig()),
				nil, nil, nil, reconcilePhase{}, reconcilePhase{}, nil, cfg,
			)
			require.Equal(t, tt.want, r.cfg.MaxConcurrent)
		})
	}
}

// A resend that meets "the chain says no proof was required" must DROP the
// entry, not count it. Mirror image of the saturation exemption above: that one
// never reached the network, this one did and can never succeed, because the
// requirement is seeded from a fixed block hash and read with params at the
// session's own heights.
//
// Counting instead would happen to look right -- MaxRebroadcasts is 1, so the
// storm is capped anyway -- while leaving the entry to be re-read on every block
// until the window closes. The assertion is therefore on the STORE, not on the
// attempt count: the attempt count cannot tell "dropped" from "capped".
func TestReconciler_NotRequiredDropsTheEntryInsteadOfCountingIt(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("%w: %w", tx.ErrTxProofNotRequired, &tx.TxRejection{
		Stage:  tx.TxStageSimulate,
		RawLog: "proof not required",
	})
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	h.r.OnBlock(testSubmit + 1) // past the one-block grace: resend fires

	require.Equal(t, 1, h.resub.attemptCount(), "the resend must have been attempted once")
	require.Zero(t, h.pendingCount(t, hSupplier, hEnd),
		"a proof the chain refused as not required has no inclusion left to verify: the entry must be gone")
}

// Driving every remaining block must not resurrect it, which is the property a
// counted-but-kept entry would fail.
func TestReconciler_NotRequiredEntryStaysGone(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = fmt.Errorf("%w: %w", tx.ErrTxProofNotRequired, &tx.TxRejection{
		Stage:  tx.TxStageSimulate,
		RawLog: "proof not required",
	})
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	for height := testSubmit; height < testWindowClose; height++ {
		h.r.OnBlock(height)
	}

	require.Equal(t, 1, h.resub.attemptCount(),
		"one doomed attempt is one too many to repeat: the entry was dropped after the first")
	require.Zero(t, h.pendingCount(t, hSupplier, hEnd))
}

// TestReconciler_AnEntryWithoutTheAttemptHeightStillSpaces covers the mixed-fleet
// entry: one written by a binary that had no LastAttemptHeight, or one an older
// binary read and rewrote, dropping the field it cannot see. Either way the
// entry arrives with a resend already made and no record of when.
//
// The fallback has to keep SPACING, from the nominal schedule, because the
// alternative is not a weaker guarantee: with a bare `base` the release
// condition is already satisfied for such an entry, so the next resend leaves on
// the very next block -- the exact back-to-back send this change exists to
// prevent, re-opened during every rolling deploy.
//
// Injection: return `base` when LastAttemptHeight is unset. Red, because the
// resend arrives at the midpoint instead of two blocks later.
func TestReconciler_AnEntryWithoutTheAttemptHeightStillSpaces(t *testing.T) {
	h := newReconcilerHarness(t, 1)

	// Written by hand, not through put(): the shape under test is one no current
	// binary produces -- a resend already counted, its height unknown.
	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:     []byte("s1"),
		SubmitHeight: testSubmit,
		TxHash:       "tx-s1",
		OrigTxHash:   "tx-s1",
		Rebroadcasts: 1,
		// LastAttemptHeight deliberately absent.
	})
	require.NoError(t, err)
	require.NoError(t, h.store.Put(context.Background(), RebroadcastPhaseProof, hSupplier, hEnd, "s1", b))

	got := resendHeights(h, testSubmit, testWindowClose)

	require.Equal(t, []int64{testMid + 2}, got,
		"an entry that has already resent once must be spaced from the nominal schedule, not released at the midpoint")
}

// TestReconciler_CapLeftUnspentAtWindowCloseIsCounted pins the signal that tells
// an operator WHY they configured a cap of 2 and saw one resend.
//
// The short window is not hypothetical: the chain's own default claim/proof
// close offset is 4 blocks (mainnet and localnet configure 10), and at 4 the
// first resend lands on the last height the guard allows, so there is no block
// left for a second. Without this counter that is indistinguishable from "a
// second resend was never needed", and the two call for opposite actions.
//
// The long-window half is the control: it proves the counter is not simply
// always firing.
//
// Injection: delete the Inc. Red on the short window.
func TestReconciler_CapLeftUnspentAtWindowCloseIsCounted(t *testing.T) {
	read := func() float64 {
		return testutil.ToFloat64(inclusionResendCapUnusedTotal.WithLabelValues(string(RebroadcastPhaseProof)))
	}

	t.Run("a window too short for the spacing reports the unspent budget", func(t *testing.T) {
		h := newReconcilerHarness(t, 1)
		h.windowClose = testSubmit + 4 // the chain's default offset
		h.seed(t, hSupplier, hEnd, "s1", testSubmit)

		before := read()
		got := resendHeights(h, testSubmit, h.windowClose+1)

		require.Len(t, got, 1, "a 4-block window has room for exactly one resend")
		require.Equal(t, float64(1), read()-before,
			"budget left unspent at window close must be reported, or it reads as a resend that was not needed")
	})

	t.Run("a window that fits both resends reports nothing", func(t *testing.T) {
		h := newReconcilerHarness(t, 1)
		h.seed(t, hSupplier, hEnd, "s2", testSubmit)

		before := read()
		got := resendHeights(h, testSubmit, testWindowClose+1)

		require.Len(t, got, 2, "the default harness window fits the whole calendar")
		require.Equal(t, float64(0), read()-before,
			"a cap that was fully spent must not be reported as unspent")
	})
}

// TestReconciler_ASpacingThatNoLongerFitsIsCompressedNotAbandoned pins the
// floor, which is the one rule in resendThreshold that nothing else asserts:
// when the spacing would push a resend past the last height the guard allows,
// the threshold is compressed to that height instead of the attempt being
// dropped.
//
// The arithmetic, written out so nobody has to re-derive it: with
// RebroadcastSafetyBlocks=1 the guard is `height < windowClose-1`, so the last
// height a resend can leave at is windowClose-2. Seeding LastAttemptHeight at
// windowClose-3 makes the spaced threshold windowClose-1 -- one past that. With
// the floor it becomes windowClose-2 and the resend goes out; without it the
// threshold stays at windowClose-1, which the guard can never satisfy, and the
// second attempt is lost in full.
//
// That is the failure mode the floor exists for, and it is not exotic: it is
// what happens whenever the first resend was itself late, which is exactly when
// the reconciler is busy and a second attempt matters most.
//
// Injection: drop the compression branch from resendThreshold. Red, with an
// empty result -- no resend at all rather than a late one.
func TestReconciler_ASpacingThatNoLongerFitsIsCompressedNotAbandoned(t *testing.T) {
	h := newReconcilerHarness(t, 1)

	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:          []byte("s1"),
		SubmitHeight:      testSubmit,
		TxHash:            "tx-s1",
		OrigTxHash:        "tx-s1",
		Rebroadcasts:      1,
		LastAttemptHeight: testWindowClose - 3,
	})
	require.NoError(t, err)
	require.NoError(t, h.store.Put(context.Background(), RebroadcastPhaseProof, hSupplier, hEnd, "s1", b))

	got := resendHeights(h, testSubmit, testWindowClose)

	require.Equal(t, []int64{testWindowClose - 2}, got,
		"a resend whose spacing no longer fits must be compressed to the last height the guard allows, not abandoned: "+
			"the guard decides whether a send can still land, the spacing only prefers when")
}

// TestReconciler_AnExpiredBudgetDoesNotBurnAnAttempt pins the money half of the
// budget fix: a resend that never happened must not spend one of the few
// attempts a claim has.
//
// PerGroupTimeout covers the listing, the inclusion query and every resend in
// the group IN SERIES. When it runs out, the entries still queued would each
// call ResubmitMessage, fail with a context error — which the counter does NOT
// exempt, the sentinel covers only a saturated permit — and have the burn
// persisted. Nothing was signed and nothing was sent.
//
// Called directly rather than through OnBlock because the property is about one
// entry meeting an expired context, and a direct call states exactly that.
//
// Injection: remove the ctx.Err() guard from rebroadcast. Red — the resubmitter
// is called and the attempt is counted.
func TestReconciler_AnExpiredBudgetDoesNotBurnAnAttempt(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)

	entry := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the budget is already gone

	h.r.rebroadcast(ctx, h.r.proofPhase, RebroadcastGroup{Supplier: hSupplier, SessionEnd: hEnd},
		"s1", entry, testMid, testWindowClose)

	require.Equal(t, 0, h.resub.attemptCount(),
		"a resend that never left the process must not spend an attempt")

	after := h.entry(t, RebroadcastPhaseProof, hSupplier, hEnd, "s1")
	require.Equal(t, 0, after.Rebroadcasts,
		"the persisted counter must be untouched, or the burn survives the block")
	require.Equal(t, 0, h.resub.count(), "ResubmitMessage must not be reached at all")
}

// TestReconciler_ABudgetSpentByTheQueryStopsTheGroupAndSaysSo covers the other
// half: the group stops on the first entry that finds the budget gone, and the
// operator can tell WHY.
//
// inclusion_resend_cap_unused_total measures the damage — budget left at window
// close — but not its cause, and the two causes call for opposite actions: a
// window too short is not something an operator can change, a group budget too
// small is. This is the same argument as S8's criterion 9, applied one level
// down.
//
// The query waits for the deadline rather than sleeping: it blocks on the
// context's own Done channel, so the expiry is a fact when the loop runs
// instead of a duration the test hopes is long enough.
//
// Injection: delete the Inc, or the break. Red on the counter.
func TestReconciler_ABudgetSpentByTheQueryStopsTheGroupAndSaysSo(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	// TWO sessions, and that is the whole reason the assertion below can mean
	// anything. The counter is per GROUP: the loop stops on the first entry that
	// finds the budget gone. With a single session, "once per group" and "once
	// per entry" both produce 1, so an exact assertion on 1 separates nothing —
	// the population is too small for the difference to exist, not the assertion
	// too loose. Measured: with one session, moving the Inc into rebroadcast
	// leaves this test green.
	h.seed(t, hSupplier, hEnd, "s1", testSubmit)
	h.seed(t, hSupplier, hEnd, "s2", testSubmit)
	h.onChainWaitForDeadline = true

	before := testutil.ToFloat64(
		inclusionGroupAbandonedTotal.WithLabelValues(string(RebroadcastPhaseProof), abandonCauseBudgetExhausted),
	)

	h.r.OnBlock(testMid)

	require.Equal(t, float64(1),
		testutil.ToFloat64(
			inclusionGroupAbandonedTotal.WithLabelValues(string(RebroadcastPhaseProof), abandonCauseBudgetExhausted),
		)-before,
		"a group whose budget the query spent must be reported ONCE, under its own cause: the event is the group "+
			"running out, not each entry meeting the consequence")

	require.Equal(t, 0, h.resub.attemptCount(), "no attempt may be spent once the budget is gone")
}

// TestReconciler_OneChainReadPerSupplierPerBlock is what makes the unified read a
// CHANGE and not a translation of types. The two phases used to query separately,
// and the unit of the query was the GROUP -- keyed by (supplier, session end) --
// so a supplier with entries at two session ends in both phases walked the same
// AllClaims index four times in one block, for identical bytes.
//
// Four groups here, one supplier: two session ends x two phases. The chain must
// be asked ONCE. Asserting the exact count and not "fewer than four" is what
// separates deduplication from luck in scheduling: the groups run concurrently on
// the pool, so a weaker assertion would pass on a build where two of them simply
// raced to the same answer.
func TestReconciler_OneChainReadPerSupplierPerBlock(t *testing.T) {
	h := newReconcilerHarness(t, 1)

	for _, end := range []int64{hEnd, hEnd + 20} {
		h.put(t, RebroadcastPhaseClaim, hSupplier, end, fmt.Sprintf("c-%d", end), testSubmit, "tx-c")
		h.put(t, RebroadcastPhaseProof, hSupplier, end, fmt.Sprintf("p-%d", end), testSubmit, "tx-p")
	}

	h.r.OnBlock(testMid)

	require.Equal(t, int64(1), h.fetchCalls.Load(),
		"four groups across both phases asked the same question at the same height; "+
			"the chain must be walked once")

	// A LATER block is a different question and must be asked again -- a cache
	// that outlived the pass would answer the next block with last block's chain.
	h.r.OnBlock(testMid + 1)
	require.Equal(t, int64(2), h.fetchCalls.Load(),
		"the answer is per height: a new block must re-read the chain")
}

// TestReconciler_ClaimPhaseIgnoresTheProofStatus pins D4, and it only became
// possible to break when the two phases started reading the SAME map. Before
// that, the claim side had its own query that returned every claim, so no edit
// could accidentally teach it to filter; now both phases receive a state and one
// of them is supposed to ignore it.
//
// A claim whose proof the chain REJECTED is still a claim that landed. The claim
// phase must call it found and stop resending it -- reading the rejection as
// "claim missing" would resend a claim that is already on chain, and pay for it.
func TestReconciler_ClaimPhaseIgnoresTheProofStatus(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.put(t, RebroadcastPhaseClaim, hSupplier, hEnd, "s1", testSubmit, "tx-s1")
	h.onChain = map[string]query.SessionProofState{"s1": query.SessionProofRejected}

	h.r.OnBlock(testMid)

	require.Equal(t, 0, h.resub.count(),
		"the claim is on chain: whatever the chain thinks of its PROOF, the claim "+
			"phase has nothing left to resend")
	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionFound, outcomes[0].outcome,
		"a claim present with a rejected proof is FOUND for the claim phase")
}
