//go:build test

package miner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// -----------------------------------------------------------------------------
// isClaimNotFoundError — unit tests
// -----------------------------------------------------------------------------

// isClaimNotFoundError delegates to query.IsEntityNotFound, whose own table in
// query/errors_test.go pins the full policy (explicit gRPC NotFound only, bare and
// wrapped; every transient failure fails open). Only the delegation is asserted here
// so one behaviour change does not have to be edited into three test tables in
// lockstep — the guard's decision tree is covered by runGuard below.
func TestIsClaimNotFoundError_DelegatesToTheSharedPolicy(t *testing.T) {
	require.True(t, isClaimNotFoundError(
		fmt.Errorf("failed to query claim: %w", status.Error(codes.NotFound, "claim not found"))),
		"an explicit NotFound, wrapped as query.GetClaim wraps it, means the claim is absent")

	require.False(t, isClaimNotFoundError(status.Error(codes.Unknown, "rpc error: header not found")),
		"a transient failure carrying \"not found\" must never skip a proof")

	require.False(t, isClaimNotFoundError(nil))
}

// -----------------------------------------------------------------------------
// SessionCoordinator.OnClaimMissing — integration with miniredis
// -----------------------------------------------------------------------------

func setupTestCoordinator(t *testing.T) (*SessionCoordinator, *RedisSessionStore, *redisutil.Client) {
	t.Helper()

	client, _ := newTestRedis(t)

	store := NewRedisSessionStore(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		client,
		SessionStoreConfig{
			SupplierAddress: "pokt1test",
			SessionTTL:      1 * time.Hour,
		},
	)

	coord := NewSessionCoordinator(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		store,
		SMSTRecoveryConfig{SupplierAddress: "pokt1test"},
	)

	return coord, store, client
}

func TestOnClaimMissing_MarksSessionTerminal(t *testing.T) {
	coord, store, _ := setupTestCoordinator(t)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-missing-1",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   SessionStateClaimed,
		ClaimTxHash:             "deadbeef",
	}))

	require.NoError(t, coord.OnClaimMissing(ctx, "sess-missing-1"))

	got, err := store.Get(ctx, "sess-missing-1")
	require.NoError(t, err)
	require.Equal(t, SessionStateClaimMissing, got.State)
	require.True(t, got.State.IsTerminal(), "claim_missing must be terminal")
	require.True(t, got.State.IsFailure(), "claim_missing must be a failure")
}

func TestOnClaimMissing_PreservesSuccessfulTerminalState(t *testing.T) {
	// If another miner in the HA set already settled the session successfully,
	// OnClaimMissing must NOT overwrite that. Otherwise we'd mask a real success.
	coord, store, _ := setupTestCoordinator(t)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-settled",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   SessionStateProved, // already successful
	}))

	require.NoError(t, coord.OnClaimMissing(ctx, "sess-settled"))

	got, err := store.Get(ctx, "sess-settled")
	require.NoError(t, err)
	assert.Equal(t, SessionStateProved, got.State, "must not overwrite a successful terminal state")
}

func TestOnClaimMissing_InvokesTerminalCallback(t *testing.T) {
	coord, store, _ := setupTestCoordinator(t)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-cb",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   SessionStateClaimed,
	}))

	var got SessionState
	var gotID string
	coord.SetOnSessionTerminalCallback(func(sessionID string, state SessionState) {
		gotID = sessionID
		got = state
	})

	require.NoError(t, coord.OnClaimMissing(ctx, "sess-cb"))

	assert.Equal(t, "sess-cb", gotID)
	assert.Equal(t, SessionStateClaimMissing, got)
}

// -----------------------------------------------------------------------------
// Pre-proof guard integration — covers the three outcomes that matter
// -----------------------------------------------------------------------------

type stubProofQueryClient struct {
	// getClaimFn lets each test pick the result shape.
	getClaimFn func(ctx context.Context, supplier, sessionID string) (pocktclient.Claim, error)

	calls int
}

func (s *stubProofQueryClient) GetClaim(ctx context.Context, supplier, sessionID string) (pocktclient.Claim, error) {
	s.calls++
	return s.getClaimFn(ctx, supplier, sessionID)
}

// GetParams is part of the pocktclient.ProofQueryClient interface. The guard
// never calls it, so an unimplemented stub suffices for these tests.
func (s *stubProofQueryClient) GetParams(_ context.Context) (pocktclient.ProofParams, error) {
	return nil, errors.New("GetParams not implemented in stub")
}

// The tests below exercise the guard decision tree directly rather than
// running the full OnSessionsNeedProof pipeline. OnSessionsNeedProof requires
// a block client, shared client, proof checker, smst manager, and a live
// transaction client — all of which are covered by other tests. What matters
// for WS-A is: (a) NotFound → mark missing + metric + skip; (b) Found →
// proceed; (c) other RPC error → fail open. We test that decision logic by
// calling the guard branches directly.

// runGuard replicates the guard logic from OnSessionsNeedProof so we can
// assert its behavior without spinning up the full pipeline. Keep this in
// sync with the production guard.
func runGuard(
	ctx context.Context,
	lc *LifecycleCallback,
	snapshot *SessionSnapshot,
) (skipped bool) {
	if lc.config.DisablePreProofClaimVerification || lc.proofQueryClient == nil {
		return false
	}
	_, err := lc.proofQueryClient.GetClaim(ctx, snapshot.SupplierOperatorAddress, snapshot.SessionID)
	if err != nil && isClaimNotFoundError(err) {
		RecordProofSkipped(snapshot.SupplierOperatorAddress, snapshot.ServiceID, ProofSkippedReasonClaimMissingOnChain)
		if lc.sessionCoordinator != nil {
			_ = lc.sessionCoordinator.OnClaimMissing(ctx, snapshot.SessionID)
		}
		return true
	}
	return false
}

func TestPreProofGuard_NotFound_SkipsAndMarks(t *testing.T) {
	coord, store, _ := setupTestCoordinator(t)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID:               "sess-notfound",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   SessionStateClaimed,
	}))

	stub := &stubProofQueryClient{
		getClaimFn: func(ctx context.Context, supplier, sessionID string) (pocktclient.Claim, error) {
			return nil, status.Error(codes.NotFound, "claim not found")
		},
	}

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:             DefaultLifecycleCallbackConfig(),
		sessionCoordinator: coord,
		proofQueryClient:   stub,
	}

	snapshot := &SessionSnapshot{
		SessionID:               "sess-notfound",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
	}

	skipped := runGuard(ctx, lc, snapshot)
	require.True(t, skipped, "NotFound must cause the guard to skip the session")
	require.Equal(t, 1, stub.calls)

	got, err := store.Get(ctx, "sess-notfound")
	require.NoError(t, err)
	assert.Equal(t, SessionStateClaimMissing, got.State)
}

func TestPreProofGuard_Found_Proceeds(t *testing.T) {
	coord, _, _ := setupTestCoordinator(t)
	stub := &stubProofQueryClient{
		getClaimFn: func(ctx context.Context, supplier, sessionID string) (pocktclient.Claim, error) {
			// Any non-nil, non-error return keeps the session in the batch.
			return nil, nil
		},
	}

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:             DefaultLifecycleCallbackConfig(),
		sessionCoordinator: coord,
		proofQueryClient:   stub,
	}

	snapshot := &SessionSnapshot{
		SessionID:               "sess-found",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
	}

	require.False(t, runGuard(context.Background(), lc, snapshot), "Found must NOT skip the session")
	require.Equal(t, 1, stub.calls)
}

func TestPreProofGuard_UnavailableFailsOpen(t *testing.T) {
	// Transient RPC failures must not drop valid proofs.
	coord, _, _ := setupTestCoordinator(t)
	stub := &stubProofQueryClient{
		getClaimFn: func(ctx context.Context, supplier, sessionID string) (pocktclient.Claim, error) {
			return nil, status.Error(codes.Unavailable, "chain RPC unavailable")
		},
	}

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:             DefaultLifecycleCallbackConfig(),
		sessionCoordinator: coord,
		proofQueryClient:   stub,
	}

	snapshot := &SessionSnapshot{
		SessionID:               "sess-flapping",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
	}

	require.False(t, runGuard(context.Background(), lc, snapshot), "Unavailable must fail open")
}

func TestPreProofGuard_FlagDisabled_NoCall(t *testing.T) {
	coord, _, _ := setupTestCoordinator(t)
	stub := &stubProofQueryClient{
		getClaimFn: func(ctx context.Context, supplier, sessionID string) (pocktclient.Claim, error) {
			t.Fatalf("GetClaim must NOT be called when guard is disabled")
			return nil, nil
		},
	}

	cfg := DefaultLifecycleCallbackConfig()
	cfg.DisablePreProofClaimVerification = true

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:             cfg,
		sessionCoordinator: coord,
		proofQueryClient:   stub,
	}

	snapshot := &SessionSnapshot{
		SessionID:               "sess-flagged-off",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
	}

	require.False(t, runGuard(context.Background(), lc, snapshot))
	assert.Equal(t, 0, stub.calls)
}

func TestPreProofGuard_NilClient_NoCall(t *testing.T) {
	coord, _, _ := setupTestCoordinator(t)

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:             DefaultLifecycleCallbackConfig(),
		sessionCoordinator: coord,
		// proofQueryClient intentionally nil
	}

	snapshot := &SessionSnapshot{
		SessionID:               "sess-nil-client",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-a",
	}

	require.False(t, runGuard(context.Background(), lc, snapshot))
}

// judgedProofCycle runs the REAL OnSessionsNeedProof for one session whose
// claim the chain reports with the given proof status, and returns how many
// proofs were signed and the cycle's result.
func judgedProofCycle(t *testing.T, st prooftypes.ClaimProofStatus) (int, ProofCycleResult) {
	t.Helper()
	blocks := &heightedBlocks{}
	blocks.currentHeight = 108
	supplier := &flakySupplier{}
	lc := &LifecycleCallback{
		logger:         logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:   &defaultParamsShared{},
		blockClient:    blocks,
		smstManager:    provingSMST{},
		supplierClient: supplier,
		serviceClient:  erroringService{},
		proofQueryClient: &stubProofQueryClient{getClaimFn: func(context.Context, string, string) (pocktclient.Claim, error) {
			return &prooftypes.Claim{ProofValidationStatus: st}, nil
		}},
		config: LifecycleCallbackConfig{ProofRetryAttempts: 1, ProofRetryDelay: time.Millisecond},
	}
	result, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{{
		SessionID: "session-judged", SessionEndHeight: 100, SessionStartHeight: 81,
		SupplierOperatorAddress: "pokt1judgedcycle", ServiceID: "svc", RelayCount: 10, TotalComputeUnits: 100,
		State: SessionStateProving, ClaimedRootHash: make([]byte, SMSTRootLen),
	}})
	require.NoError(t, err)
	return supplier.calls, result
}

// A session back in claimed after a kill between its proof's broadcast and the
// write of its hash must not send the proof again once the chain validated it:
// poktroll deletes the judged proof and would accept and charge a second one.
func TestPreProofGuard_AValidatedProofIsNotSentAgain(t *testing.T) {
	signed, result := judgedProofCycle(t, prooftypes.ClaimProofStatus_VALIDATED)
	require.Zero(t, signed, "the chain already validated this proof: no second one is charged")
	require.True(t, result.IsSettled("session-judged"), "and the session is settled, since its proof is on chain")
}

func TestPreProofGuard_AnInvalidProofIsNotSentAgain(t *testing.T) {
	signed, result := judgedProofCycle(t, prooftypes.ClaimProofStatus_INVALID)
	require.Zero(t, signed, "the same proof would be judged the same way")
	require.False(t, result.IsSettled("session-judged"), "and a rejected proof is not a settlement")
}

func TestPreProofGuard_APendingClaimGetsItsProof(t *testing.T) {
	signed, result := judgedProofCycle(t, prooftypes.ClaimProofStatus_PENDING_VALIDATION)
	require.Equal(t, 1, signed, "control: a claim waiting for its proof gets it")
	require.True(t, result.IsSettled("session-judged"))
}
