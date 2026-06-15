//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// --- mocks -------------------------------------------------------------------

type mockResubmitter struct {
	mu       sync.Mutex
	calls    []string
	failNext bool
}

func (m *mockResubmitter) ResubmitMessage(_ context.Context, phase RebroadcastPhase, supplier string, msgBytes []byte, _ int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	onChain     map[string]struct{}
	onChainErr  error
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

func newReconcilerHarness(t *testing.T, safetyBlocks int64) *reconcilerHarness {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rc, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{URL: fmt.Sprintf("redis://%s", mr.Addr())})
	require.NoError(t, err)

	h := &reconcilerHarness{
		store:       NewRebroadcastStore(rc, time.Hour),
		resub:       &mockResubmitter{},
		onChain:     map[string]struct{}{},
		windowClose: testWindowClose,
	}

	mkPhase := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return h.windowClose },
			onChainSessions: func(_ context.Context, _ string) (map[string]struct{}, error) {
				h.mu.Lock()
				defer h.mu.Unlock()
				if h.onChainErr != nil {
					return nil, h.onChainErr
				}
				cp := make(map[string]struct{}, len(h.onChain))
				for k := range h.onChain {
					cp[k] = struct{}{}
				}
				return cp, nil
			},
			recordOutcome: func(_ context.Context, _ rebroadcastEntry, supplier string, _ int64, sessionID, outcome string, height int64) {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.outcomes = append(h.outcomes, capturedOutcome{supplier, sessionID, outcome, height})
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
	h.onChain = map[string]struct{}{"s1": {}}

	h.r.OnBlock(testMid)

	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionFound, outcomes[0].outcome)
	require.Equal(t, testMid, outcomes[0].height)
	require.Equal(t, 0, h.resub.count(), "found proof must not rebroadcast")
	require.Equal(t, 0, h.pendingCount(t, hSupplier, hEnd), "found entry must be cleared")
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

	// Bounded: a later block does not resend again (MaxRebroadcasts=1).
	h.r.OnBlock(testMid + 1)
	require.Equal(t, 1, h.resub.count(), "resend is capped at MaxRebroadcasts (1)")
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
	h.onChain = map[string]struct{}{"s1": {}}

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
	h.onChain = map[string]struct{}{"s1": {}}

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
	h.r.Close() // rebuild with MaxRebroadcasts=0
	cfg := DefaultInclusionReconcilerConfig()
	cfg.MaxConcurrent = 4
	cfg.MaxRebroadcasts = 0
	cfg.RebroadcastSafetyBlocks = 1
	cfg.PerGroupTimeout = 2 * time.Second
	mkPhase := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return h.windowClose },
			onChainSessions: func(_ context.Context, _ string) (map[string]struct{}, error) {
				return map[string]struct{}{}, nil
			},
			recordOutcome: func(_ context.Context, _ rebroadcastEntry, supplier string, _ int64, sessionID, outcome string, height int64) {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.outcomes = append(h.outcomes, capturedOutcome{supplier, sessionID, outcome, height})
			},
			recordRebroadcast: func(string, string, string) {},
		}
	}
	h.r = NewInclusionReconciler(logging.NewLoggerFromConfig(logging.DefaultConfig()), &mockSharedQueryClient{}, h.store, h.resub,
		mkPhase(RebroadcastPhaseClaim), mkPhase(RebroadcastPhaseProof), cfg)
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
func (h *reconcilerHarness) newPeerReconciler(t *testing.T, resub *mockResubmitter, onChain map[string]struct{}, owns func(string) bool) *InclusionReconciler {
	t.Helper()
	mkPhase := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return h.windowClose },
			onChainSessions: func(_ context.Context, _ string) (map[string]struct{}, error) {
				cp := make(map[string]struct{}, len(onChain))
				for k := range onChain {
					cp[k] = struct{}{}
				}
				return cp, nil
			},
			recordOutcome:     func(context.Context, rebroadcastEntry, string, int64, string, string, int64) {},
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
		mkPhase(RebroadcastPhaseClaim), mkPhase(RebroadcastPhaseProof), cfg,
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
	b := h.newPeerReconciler(t, resubB, map[string]struct{}{}, func(s string) bool { return s == hSupplier })

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
	b := h.newPeerReconciler(t, resubB, map[string]struct{}{}, func(s string) bool { return s == hSupplier })
	// Non-owner C sees the same Redis entry but owns nothing.
	resubC := &mockResubmitter{}
	c := h.newPeerReconciler(t, resubC, map[string]struct{}{}, func(string) bool { return false })

	b.OnBlock(testSubmit + 1)
	c.OnBlock(testSubmit + 1)

	require.Equal(t, 1, resubB.count(), "the owner resends exactly once")
	require.Equal(t, 0, resubC.count(), "a non-owner must never resend another replica's supplier (no double-submit)")
}
