//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// -----------------------------------------------------------------------------
// Mocks
// -----------------------------------------------------------------------------

// ptProofGetter is a programmable ProofInclusionQueryClient. getFn receives the
// 1-based call index so a test can return NotFound for the first N polls and a
// proof afterwards.
type ptProofGetter struct {
	mu    sync.Mutex
	getFn func(call int) (*prooftypes.Proof, error)
	calls atomic.Int32
}

func (m *ptProofGetter) GetProof(_ context.Context, _, _ string) (*prooftypes.Proof, error) {
	call := int(m.calls.Add(1))
	m.mu.Lock()
	fn := m.getFn
	m.mu.Unlock()
	if fn == nil {
		return nil, status.Error(codes.NotFound, "not set")
	}
	return fn(call)
}

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

type proofTrackerHarness struct {
	tracker     *ProofInclusionTracker
	proofGetter *ptProofGetter
	blockClient *mockBlockClient
	submissions *SubmissionTracker
}

func newProofTrackerHarness(t *testing.T, cfg ProofInclusionTrackerConfig) *proofTrackerHarness {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rc, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)

	submissions := NewSubmissionTracker(
		logging.NewLoggerFromConfig(logging.DefaultConfig()), rc, time.Hour)

	proofGetter := &ptProofGetter{}
	blockClient := &mockBlockClient{currentHeight: 116}
	sharedClient := &mockSharedQueryClient{} // default: proofWindowClose(sessionEnd=110)=119

	tracker := NewProofInclusionTracker(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		proofGetter, sharedClient, blockClient, submissions, cfg)
	t.Cleanup(func() { _ = tracker.Close() })

	return &proofTrackerHarness{tracker, proofGetter, blockClient, submissions}
}

// seedProofRecord creates a record with a claim + a CheckTx-accepted proof so
// UpdateProofOnChainOutcome has something to annotate.
func seedProofRecord(t *testing.T, tr *SubmissionTracker, supplier, sessionID, proofTx string, sessionEnd int64) {
	t.Helper()
	require.NoError(t, tr.TrackClaimSubmission(context.Background(),
		supplier, "svc-a", "pokt1app", sessionID, 100, sessionEnd,
		"0xclaim", "claim-tx", true, "", 105, 110, 50, 100, true, ""))
	require.NoError(t, tr.TrackProofSubmission(context.Background(),
		supplier, sessionEnd, sessionID, "0xproof", proofTx, true, "", 116, 116, true, "0xseed"))
}

func assertProofOutcomeEventually(t *testing.T, h *proofTrackerHarness, supplier string, sessionEnd int64, sessionID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := h.submissions.GetRecord(context.Background(), supplier, sessionEnd, sessionID)
		if err == nil && rec.ProofOnChainOutcome == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, err := h.submissions.GetRecord(context.Background(), supplier, sessionEnd, sessionID)
	require.NoError(t, err)
	assert.Equal(t, want, rec.ProofOnChainOutcome, "proof outcome did not converge — getProof calls=%d", h.proofGetter.calls.Load())
}

func defaultProofCfg() ProofInclusionTrackerConfig {
	c := DefaultProofInclusionTrackerConfig()
	c.PollInterval = 5 * time.Millisecond
	c.MaxConcurrent = 4
	c.MaxPollDuration = 1500 * time.Millisecond
	return c
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// Proof already on-chain: outcome found, no rebroadcast.
func TestProofInclusion_OnChainFound_NoRebroadcast(t *testing.T) {
	h := newProofTrackerHarness(t, defaultProofCfg())
	seedProofRecord(t, h.submissions, "pokt1test", "sess-1", "proof-tx-1", 110)

	h.proofGetter.getFn = func(int) (*prooftypes.Proof, error) { return &prooftypes.Proof{}, nil }
	setBlockHeight(h.blockClient, 116) // inside window (close=119)

	var resubmits atomic.Int32
	require.True(t, h.tracker.ScheduleProofCheck(ProofInclusionCheck{
		Supplier: "pokt1test", ServiceID: "svc-a", SessionID: "sess-1", SessionEndHeight: 110,
		ProofTxHash: "proof-tx-1", SubmitHeight: 115,
		Resubmit: func(context.Context) (string, error) { resubmits.Add(1); return "x", nil },
	}))

	assertProofOutcomeEventually(t, h, "pokt1test", 110, "sess-1", string(ProofInclusionFound))
	require.Equal(t, int32(0), resubmits.Load(), "proof already on-chain — must not rebroadcast")

	rec, err := h.submissions.GetRecord(context.Background(), "pokt1test", 110, "sess-1")
	require.NoError(t, err)
	require.Equal(t, int64(116), rec.ProofInclusionHeight,
		"Found outcome must record the chain height at which the proof was observed on-chain")
}

// Proof missing while window open → rebroadcast; then it lands → outcome found.
// This is the core fix: a CheckTx-accepted proof that missed its block is
// re-sent into the still-open window and recovered.
func TestProofInclusion_RebroadcastThenFound(t *testing.T) {
	cfg := defaultProofCfg()
	cfg.MaxRebroadcasts = 2
	cfg.RebroadcastSafetyBlocks = 1
	h := newProofTrackerHarness(t, cfg)
	seedProofRecord(t, h.submissions, "pokt1test", "sess-2", "proof-tx-2", 110)

	// NotFound for the first 2 polls, then the proof appears.
	h.proofGetter.getFn = func(call int) (*prooftypes.Proof, error) {
		if call <= 2 {
			return nil, status.Error(codes.NotFound, "proof not found")
		}
		return &prooftypes.Proof{}, nil
	}
	setBlockHeight(h.blockClient, 116) // inside window throughout (<= 119-1)

	var resubmits atomic.Int32
	require.True(t, h.tracker.ScheduleProofCheck(ProofInclusionCheck{
		Supplier: "pokt1test", ServiceID: "svc-a", SessionID: "sess-2", SessionEndHeight: 110,
		ProofTxHash: "proof-tx-2", SubmitHeight: 115,
		Resubmit: func(context.Context) (string, error) {
			n := resubmits.Add(1)
			return fmt.Sprintf("rebroadcast-hash-%d", n), nil
		},
	}))

	assertProofOutcomeEventually(t, h, "pokt1test", 110, "sess-2", string(ProofInclusionFound))

	require.GreaterOrEqual(t, int(resubmits.Load()), 1, "missing proof in open window must be rebroadcast")
	rec, err := h.submissions.GetRecord(context.Background(), "pokt1test", 110, "sess-2")
	require.NoError(t, err)
	require.GreaterOrEqual(t, rec.ProofRebroadcasts, 1)
	require.Contains(t, rec.ProofTxHash, "rebroadcast-hash-", "record should carry the winning rebroadcast tx hash")
}

// Proof never lands and the window closes → outcome missing (forfeit, but now
// observed instead of silent).
func TestProofInclusion_MissingWhenWindowCloses(t *testing.T) {
	h := newProofTrackerHarness(t, defaultProofCfg())
	seedProofRecord(t, h.submissions, "pokt1test", "sess-3", "proof-tx-3", 110)

	h.proofGetter.getFn = func(int) (*prooftypes.Proof, error) {
		return nil, status.Error(codes.NotFound, "proof not found")
	}
	setBlockHeight(h.blockClient, 125) // past proof window close (119)

	var resubmits atomic.Int32
	require.True(t, h.tracker.ScheduleProofCheck(ProofInclusionCheck{
		Supplier: "pokt1test", ServiceID: "svc-a", SessionID: "sess-3", SessionEndHeight: 110,
		ProofTxHash: "proof-tx-3", SubmitHeight: 115,
		Resubmit: func(context.Context) (string, error) { resubmits.Add(1); return "x", nil },
	}))

	assertProofOutcomeEventually(t, h, "pokt1test", 110, "sess-3", string(ProofInclusionMissing))
	require.Equal(t, int32(0), resubmits.Load(), "window already closed — must not rebroadcast")
}

// Rebroadcasts fire at most once per block (cadence fix) and are bounded by
// MaxRebroadcasts. The Resubmit closure advances the chain one block per call,
// so the two retries land in two distinct (later, empty) window blocks — never
// burned together in the original's burst block.
func TestProofInclusion_RebroadcastOncePerBlock_AndBounded(t *testing.T) {
	cfg := defaultProofCfg()
	cfg.MaxRebroadcasts = 2
	cfg.RebroadcastSafetyBlocks = 1
	cfg.MaxPollDuration = 400 * time.Millisecond // force a poll_error exit once stuck
	h := newProofTrackerHarness(t, cfg)
	seedProofRecord(t, h.submissions, "pokt1test", "sess-4", "proof-tx-4", 110)

	h.proofGetter.getFn = func(int) (*prooftypes.Proof, error) {
		return nil, status.Error(codes.NotFound, "proof not found")
	}
	setBlockHeight(h.blockClient, 116) // submit height 115 → 1-block grace already satisfied

	// Record the height at which each rebroadcast fires, then advance one block.
	var mu sync.Mutex
	var attemptHeights []int64
	var resubmits atomic.Int32
	require.True(t, h.tracker.ScheduleProofCheck(ProofInclusionCheck{
		Supplier: "pokt1test", ServiceID: "svc-a", SessionID: "sess-4", SessionEndHeight: 110,
		ProofTxHash: "proof-tx-4", SubmitHeight: 115,
		Resubmit: func(context.Context) (string, error) {
			mu.Lock()
			attemptHeights = append(attemptHeights, h.blockClient.LastBlock(context.Background()).Height())
			mu.Unlock()
			resubmits.Add(1)
			// Advance the chain one block so the next rebroadcast is allowed.
			setBlockHeight(h.blockClient, h.blockClient.LastBlock(context.Background()).Height()+1)
			return "x", nil
		},
	}))

	assertProofOutcomeEventually(t, h, "pokt1test", 110, "sess-4", string(ProofInclusionPollError))
	require.Equal(t, int32(2), resubmits.Load(), "rebroadcasts must be capped at MaxRebroadcasts")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int64{116, 117}, attemptHeights,
		"rebroadcasts must be one-per-block (distinct, ascending heights), not burst in one block")
}

// Grace + same-block spacing: while the chain is still in the original proof's
// submit block, NO rebroadcast fires — the original gets a fair block to land
// first. This is the core cadence fix that prevents burning the retry budget in
// the burst block (the previously-observed failure mode).
func TestProofInclusion_NoRebroadcastInSubmitBlock(t *testing.T) {
	cfg := defaultProofCfg()
	cfg.MaxRebroadcasts = 2
	cfg.RebroadcastSafetyBlocks = 1
	cfg.MaxPollDuration = 250 * time.Millisecond
	h := newProofTrackerHarness(t, cfg)
	seedProofRecord(t, h.submissions, "pokt1test", "sess-5", "proof-tx-5", 110)

	h.proofGetter.getFn = func(int) (*prooftypes.Proof, error) {
		return nil, status.Error(codes.NotFound, "proof not found")
	}
	// Chain height == submit height for the whole test: still in the burst block.
	setBlockHeight(h.blockClient, 116)

	var resubmits atomic.Int32
	require.True(t, h.tracker.ScheduleProofCheck(ProofInclusionCheck{
		Supplier: "pokt1test", ServiceID: "svc-a", SessionID: "sess-5", SessionEndHeight: 110,
		ProofTxHash: "proof-tx-5", SubmitHeight: 116, // same block as current height
		Resubmit: func(context.Context) (string, error) { resubmits.Add(1); return "x", nil },
	}))

	assertProofOutcomeEventually(t, h, "pokt1test", 110, "sess-5", string(ProofInclusionPollError))
	require.Equal(t, int32(0), resubmits.Load(),
		"must not rebroadcast while still in the original proof's submit block")
}

// Disabled tracker accepts nothing and never polls.
func TestProofInclusion_Disabled(t *testing.T) {
	cfg := defaultProofCfg()
	cfg.Disabled = true
	h := newProofTrackerHarness(t, cfg)

	accepted := h.tracker.ScheduleProofCheck(ProofInclusionCheck{
		Supplier: "pokt1test", SessionID: "x", SessionEndHeight: 110,
	})
	require.False(t, accepted)
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, int32(0), h.proofGetter.calls.Load())
}

// Nil receiver + double Close are safe.
func TestProofInclusion_NilAndDoubleCloseSafe(t *testing.T) {
	var tracker *ProofInclusionTracker
	require.False(t, tracker.ScheduleProofCheck(ProofInclusionCheck{}))
	require.NoError(t, tracker.Close())

	h := newProofTrackerHarness(t, defaultProofCfg())
	require.NoError(t, h.tracker.Close())
	require.NoError(t, h.tracker.Close())
}
