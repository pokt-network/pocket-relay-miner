//go:build test

package miner

import (
	"context"
	"errors"
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
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// -----------------------------------------------------------------------------
// Mocks (named with the inclusionTracker prefix so they don't collide with the
// non-programmable mocks used by claim_pipeline_test.go / proof_pipeline_test.go)
// -----------------------------------------------------------------------------

type inclusionTrackerProofClient struct {
	mu    sync.Mutex
	getFn func(supplier, sessionID string, call int) (pocktclient.Claim, error)
	calls atomic.Int32
}

func (m *inclusionTrackerProofClient) GetClaim(_ context.Context, supplier, sessionID string) (pocktclient.Claim, error) {
	call := int(m.calls.Add(1))
	m.mu.Lock()
	fn := m.getFn
	m.mu.Unlock()
	if fn == nil {
		return nil, status.Error(codes.NotFound, "not set")
	}
	return fn(supplier, sessionID, call)
}

func (m *inclusionTrackerProofClient) GetParams(_ context.Context) (pocktclient.ProofParams, error) {
	return nil, errors.New("not implemented")
}

// inclusionTrackerClaim implements pocktclient.Claim (three getters); the
// tracker only checks for a non-nil return.
type inclusionTrackerClaim struct{}

func (inclusionTrackerClaim) GetSupplierOperatorAddress() string { return "pokt1test" }
func (inclusionTrackerClaim) GetSessionHeader() *sessiontypes.SessionHeader {
	return &sessiontypes.SessionHeader{}
}
func (inclusionTrackerClaim) GetRootHash() []byte { return []byte("stub-root") }

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

type trackerHarness struct {
	tracker     *InclusionTracker
	queryClient *inclusionTrackerProofClient
	blockClient *mockBlockClient
	submissions *SubmissionTracker
}

func newTrackerHarness(t *testing.T) *trackerHarness {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rc, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)

	submissions := NewSubmissionTracker(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		rc,
		1*time.Hour,
	)

	queryClient := &inclusionTrackerProofClient{}
	blockClient := &mockBlockClient{currentHeight: 110}
	sharedClient := &mockSharedQueryClient{} // default params: close_offset=4

	tracker := NewInclusionTracker(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		queryClient,
		sharedClient,
		blockClient,
		submissions,
		InclusionTrackerConfig{
			PollInterval:    5 * time.Millisecond,
			MaxConcurrent:   4,
			MaxPollDuration: 2 * time.Second,
		},
	)
	t.Cleanup(func() { _ = tracker.Close() })

	return &trackerHarness{
		tracker:     tracker,
		queryClient: queryClient,
		blockClient: blockClient,
		submissions: submissions,
	}
}

func seedSubmission(t *testing.T, tracker *SubmissionTracker, supplier, sessionID, txHash string, sessionEnd int64) {
	t.Helper()
	err := tracker.TrackClaimSubmission(
		context.Background(),
		supplier, "svc-a", "pokt1app",
		sessionID,
		100, sessionEnd,
		"0xaa", txHash,
		true, "",
		105, 110,
		50, 100,
		false, "",
	)
	require.NoError(t, err)
}

func setBlockHeight(bc *mockBlockClient, h int64) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.currentHeight = h
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

func TestInclusionTracker_OnChainFound(t *testing.T) {
	h := newTrackerHarness(t)
	seedSubmission(t, h.submissions, "pokt1test", "sess-1", "txhash-1", 110)

	h.queryClient.getFn = func(_, _ string, _ int) (pocktclient.Claim, error) {
		return inclusionTrackerClaim{}, nil
	}
	// With default params (close_offset=4, open_offset=1), claim window for
	// session ending at height 110 closes at 110+1+4 = 115. Keep height
	// inside that window.
	setBlockHeight(h.blockClient, 112)

	accepted := h.tracker.ScheduleClaimCheck(ClaimInclusionCheck{
		Supplier:         "pokt1test",
		ServiceID:        "svc-a",
		SessionID:        "sess-1",
		SessionEndHeight: 110,
		ClaimTxHash:      "txhash-1",
	})
	require.True(t, accepted)

	assertOutcomeEventually(t, h, "pokt1test", 110, "sess-1", string(ClaimInclusionFound))
}

func TestInclusionTracker_OnChainMissing_WhenWindowCloses(t *testing.T) {
	h := newTrackerHarness(t)
	seedSubmission(t, h.submissions, "pokt1test", "sess-2", "txhash-2", 110)

	// GetClaim always returns NotFound — claim never landed.
	h.queryClient.getFn = func(_, _ string, _ int) (pocktclient.Claim, error) {
		return nil, status.Error(codes.NotFound, "claim not found")
	}
	// Put the chain past claim-window-close (115 for session_end=110).
	setBlockHeight(h.blockClient, 120)

	accepted := h.tracker.ScheduleClaimCheck(ClaimInclusionCheck{
		Supplier:         "pokt1test",
		ServiceID:        "svc-a",
		SessionID:        "sess-2",
		SessionEndHeight: 110,
		ClaimTxHash:      "txhash-2",
	})
	require.True(t, accepted)

	assertOutcomeEventually(t, h, "pokt1test", 110, "sess-2", string(ClaimInclusionMissing))
}

func TestInclusionTracker_TransientErrorsKeepPollingThenResolveMissing(t *testing.T) {
	h := newTrackerHarness(t)
	seedSubmission(t, h.submissions, "pokt1test", "sess-3", "txhash-3", 110)

	h.queryClient.getFn = func(_, _ string, call int) (pocktclient.Claim, error) {
		if call <= 2 {
			return nil, status.Error(codes.Unavailable, "rpc down")
		}
		return nil, status.Error(codes.NotFound, "not found")
	}
	setBlockHeight(h.blockClient, 120) // past window-close

	accepted := h.tracker.ScheduleClaimCheck(ClaimInclusionCheck{
		Supplier:         "pokt1test",
		ServiceID:        "svc-a",
		SessionID:        "sess-3",
		SessionEndHeight: 110,
		ClaimTxHash:      "txhash-3",
	})
	require.True(t, accepted)

	assertOutcomeEventually(t, h, "pokt1test", 110, "sess-3", string(ClaimInclusionMissing))
}

func TestInclusionTracker_WaitsInsideWindow(t *testing.T) {
	h := newTrackerHarness(t)
	seedSubmission(t, h.submissions, "pokt1test", "sess-4", "txhash-4", 110)

	// NotFound for the first 3 ticks, then the claim appears. Height stays
	// inside the window — the tracker must keep polling, not give up early.
	h.queryClient.getFn = func(_, _ string, call int) (pocktclient.Claim, error) {
		if call < 3 {
			return nil, status.Error(codes.NotFound, "not yet")
		}
		return inclusionTrackerClaim{}, nil
	}
	setBlockHeight(h.blockClient, 112) // inside window throughout

	accepted := h.tracker.ScheduleClaimCheck(ClaimInclusionCheck{
		Supplier:         "pokt1test",
		ServiceID:        "svc-a",
		SessionID:        "sess-4",
		SessionEndHeight: 110,
		ClaimTxHash:      "txhash-4",
	})
	require.True(t, accepted)

	assertOutcomeEventually(t, h, "pokt1test", 110, "sess-4", string(ClaimInclusionFound))
	require.GreaterOrEqual(t, int(h.queryClient.calls.Load()), 3,
		"tracker must have waited through multiple ticks inside the window")
}

func TestInclusionTracker_ScheduleClaimCheck_Disabled(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rc, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{URL: fmt.Sprintf("redis://%s", mr.Addr())})
	require.NoError(t, err)

	submissions := NewSubmissionTracker(logging.NewLoggerFromConfig(logging.DefaultConfig()), rc, time.Hour)
	queryClient := &inclusionTrackerProofClient{}
	blockClient := &mockBlockClient{}
	sharedClient := &mockSharedQueryClient{}

	tracker := NewInclusionTracker(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		queryClient,
		sharedClient,
		blockClient,
		submissions,
		InclusionTrackerConfig{Disabled: true},
	)
	t.Cleanup(func() { _ = tracker.Close() })

	accepted := tracker.ScheduleClaimCheck(ClaimInclusionCheck{
		Supplier: "pokt1test", SessionID: "x",
	})
	require.False(t, accepted, "Disabled tracker must not accept tasks")
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, int32(0), queryClient.calls.Load())
}

func TestInclusionTracker_NilTrackerNoopSafe(t *testing.T) {
	var tracker *InclusionTracker
	require.False(t, tracker.ScheduleClaimCheck(ClaimInclusionCheck{}))
	require.NoError(t, tracker.Close())
}

func TestInclusionTracker_DoubleCloseSafe(t *testing.T) {
	h := newTrackerHarness(t)
	require.NoError(t, h.tracker.Close())
	require.NoError(t, h.tracker.Close())
}

// assertOutcomeEventually polls until the submission tracker record's
// ClaimOnChainOutcome equals the expected value, or the timeout elapses.
func assertOutcomeEventually(t *testing.T, h *trackerHarness, supplier string, sessionEnd int64, sessionID, want string) {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		rec, err := h.submissions.GetRecord(context.Background(), supplier, sessionEnd, sessionID)
		if err == nil && rec.ClaimOnChainOutcome == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, err := h.submissions.GetRecord(context.Background(), supplier, sessionEnd, sessionID)
	require.NoError(t, err)
	assert.Equal(t, want, rec.ClaimOnChainOutcome, "outcome did not converge — getClaim calls=%d", h.queryClient.calls.Load())
}
