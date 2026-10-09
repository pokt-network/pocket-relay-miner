//go:build test

package miner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
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
// for WS-A is: (a) NotFound → skip; (b) Found → proceed; (c) other RPC error
// → fail open. We test that decision logic by calling the guard branches
// directly. What a NotFound does to the session runs through the real cycle in
// pre_proof_notfound_test.go.

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
	return err != nil && isClaimNotFoundError(err)
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

// A claim the chain does not hold at proof time ends the session: it is counted
// once in sessions_failed_total, and the money its claim put in `claimed` moves
// to `lost`, so claimed = proved + lost + unresolved still closes. A second call
// counts nothing.
func TestOnClaimMissing_CountsTheSessionOnceAndItsMoneyAsLost(t *testing.T) {
	coord, store, _ := setupTestCoordinator(t)
	coord.SetPricer(perMillionPricer{})
	ctx := context.Background()
	const service = "svc-claim-missing-lost"
	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID: "sess-missing-lost", SupplierOperatorAddress: "pokt1test", ServiceID: service,
		SessionStartHeight: 100, SessionEndHeight: 110, State: SessionStateClaimed, ClaimTxHash: "deadbeef",
		RelayCount: 3, TotalComputeUnits: 3_000_000,
	}))
	saved, err := store.Get(ctx, "sess-missing-lost")
	require.NoError(t, err)
	require.Equal(t, int64(3), saved.RelayCount, "premise: the session carries its weight")
	before := readLedger("pokt1test", service, "claim_missing", "")
	relaysBefore := testutil.ToFloat64(relaysLostTotal.WithLabelValues("pokt1test", service, "claim_missing"))

	require.NoError(t, coord.OnClaimMissing(ctx, "sess-missing-lost"))
	require.NoError(t, coord.OnClaimMissing(ctx, "sess-missing-lost"))

	after := readLedger("pokt1test", service, "claim_missing", "")
	require.Equal(t, before.sessions+1, after.sessions, "one failed session, once")
	require.InDelta(t, before.lost+3, after.lost, 1e-9, "the claimed money is lost")
	require.Equal(t, before.forgone, after.forgone, "nothing forgone: the claim was in the book")
	require.Equal(t, relaysBefore+3, testutil.ToFloat64(relaysLostTotal.WithLabelValues("pokt1test", service, "claim_missing")))
}

// A session that never held a claim never put money in `claimed`: its money
// is forgone, not lost.
func TestOnClaimMissing_ASessionThatNeverHeldAClaimIsForgone(t *testing.T) {
	coord, store, _ := setupTestCoordinator(t)
	coord.SetPricer(perMillionPricer{})
	ctx := context.Background()
	const service = "svc-claim-missing-forgone"
	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID: "sess-missing-forgone", SupplierOperatorAddress: "pokt1test", ServiceID: service,
		SessionStartHeight: 100, SessionEndHeight: 110, State: SessionStateActive,
		RelayCount: 2, TotalComputeUnits: 2_000_000,
	}))
	before := readLedger("pokt1test", service, "claim_missing", "")

	require.NoError(t, coord.OnClaimMissing(ctx, "sess-missing-forgone"))

	after := readLedger("pokt1test", service, "claim_missing", "")
	require.Equal(t, before.sessions+1, after.sessions)
	require.InDelta(t, before.forgone+2, after.forgone, 1e-9)
	require.Equal(t, before.lost, after.lost)
}

// A session another miner settled is not marked, and not counted.
func TestOnClaimMissing_ASettledSessionIsNotCounted(t *testing.T) {
	coord, store, _ := setupTestCoordinator(t)
	ctx := context.Background()
	const service = "svc-claim-missing-settled"
	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID: "sess-missing-settled", SupplierOperatorAddress: "pokt1test", ServiceID: service,
		SessionStartHeight: 100, SessionEndHeight: 110, State: SessionStateProved, RelayCount: 1, TotalComputeUnits: 1_000_000,
	}))
	before := readLedger("pokt1test", service, "claim_missing", "")
	require.NoError(t, coord.OnClaimMissing(ctx, "sess-missing-settled"))
	require.Equal(t, before, readLedger("pokt1test", service, "claim_missing", ""))
}

// reinstatedProbe reads what a reinstatement reversed.
func reinstatedProbe(supplier, service, from string) (sessions, upokt float64) {
	return testutil.ToFloat64(sessionsReinstatedTotal.WithLabelValues(supplier, service, "claim_missing")),
		testutil.ToFloat64(upoktReinstatedTotal.WithLabelValues(supplier, service, "claim_missing", from))
}

// claimMissingStores runs a case on both stores the miner keeps sessions in.
func claimMissingStores(t *testing.T, run func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string)) {
	t.Run("redis", func(t *testing.T) {
		coord, store, _ := setupTestCoordinator(t)
		coord.SetPricer(perMillionPricer{})
		run(t, coord, store, t.Name())
	})
	t.Run("pebble", func(t *testing.T) {
		h := newPebbleCommitHarness(t, "pokt1test")
		coord := NewSessionCoordinator(logging.NewLoggerFromConfig(logging.DefaultConfig()), h.stores.sessions,
			SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
		coord.SetPricer(perMillionPricer{})
		run(t, coord, h.stores.sessions, t.Name())
	})
}

func seedForClaimMissing(t *testing.T, store SessionStore, id, service string, state SessionState) {
	t.Helper()
	created, err := store.CreateIfAbsent(context.Background(), &SessionSnapshot{
		SessionID: id, SupplierOperatorAddress: "pokt1test", ServiceID: service,
		SessionStartHeight: 100, SessionEndHeight: 110, State: state, ClaimTxHash: "deadbeef",
		RelayCount: 3, TotalComputeUnits: 3_000_000,
	})
	require.NoError(t, err)
	require.True(t, created)
}

// A claim_missing session the chain is later seen to hold the claim of comes
// back to claimed: what claim_missing counted is reversed, once, so the net
// failed sessions and the net lost money are back to where they were.
func TestClaimMissing_AReinstatedSessionIsTakenBackOnce(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedForClaimMissing(t, store, "sess-back", service, SessionStateClaimed)
		before := readLedger("pokt1test", service, "claim_missing", "")
		backBefore, backLostBefore := reinstatedProbe("pokt1test", service, ClaimMissingLost)

		require.NoError(t, coord.OnClaimMissing(ctx, "sess-back"))
		marked, err := store.Get(ctx, "sess-back")
		require.NoError(t, err)
		require.Equal(t, ClaimMissingLost, marked.ClaimMissingVerdict, "the verdict is kept with the state")

		root := make([]byte, SMSTRootLen)
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-back", root, "deadbeef"))
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-back", root, "deadbeef"))

		after := readLedger("pokt1test", service, "claim_missing", "")
		back, backLost := reinstatedProbe("pokt1test", service, ClaimMissingLost)
		require.Equal(t, before.sessions+1, after.sessions, "claim_missing counted it once")
		require.Equal(t, backBefore+1, back, "and the reinstatement took it back once")
		require.InDelta(t, after.lost-before.lost, backLost-backLostBefore, 1e-9, "net lost money is back to zero")
		require.InDelta(t, 3, backLost-backLostBefore, 1e-9)

		got, err := store.Get(ctx, "sess-back")
		require.NoError(t, err)
		require.Equal(t, SessionStateClaimed, got.State)
		require.Empty(t, got.ClaimMissingVerdict, "the flip clears the verdict: nothing is reversed twice")
	})
}

// A session that never held a claim had its money counted forgone: that is
// what its reinstatement takes back.
func TestClaimMissing_AReinstatedForgoneSessionIsTakenBackFromForgone(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedForClaimMissing(t, store, "sess-back-forgone", service, SessionStateActive)
		before := readLedger("pokt1test", service, "claim_missing", "")
		_, backForgoneBefore := reinstatedProbe("pokt1test", service, ClaimMissingForgone)

		require.NoError(t, coord.OnClaimMissing(ctx, "sess-back-forgone"))
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-back-forgone", make([]byte, SMSTRootLen), "deadbeef"))

		after := readLedger("pokt1test", service, "claim_missing", "")
		_, backForgone := reinstatedProbe("pokt1test", service, ClaimMissingForgone)
		require.InDelta(t, after.forgone-before.forgone, backForgone-backForgoneBefore, 1e-9)
		require.InDelta(t, 3, backForgone-backForgoneBefore, 1e-9)
	})
}

// A reactivation of a session that was never counted claim_missing reverses
// nothing.
func TestClaimMissing_AReactivationOfAnotherStateReversesNothing(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		// claim_tx_error counted an attempt and no money: nothing to take back.
		seedForClaimMissing(t, store, "sess-txerr", service, SessionStateClaimTxError)
		back, _ := reinstatedProbe("pokt1test", service, ClaimMissingLost)
		require.NoError(t, coord.OnClaimObservedOnChain(ctx, "sess-txerr", make([]byte, SMSTRootLen), "deadbeef"))
		again, _ := reinstatedProbe("pokt1test", service, ClaimMissingLost)
		require.Equal(t, back, again)
		require.Zero(t, testutil.ToFloat64(sessionsReinstatedTotal.WithLabelValues("pokt1test", service, string(SessionStateClaimTxError))))
	})
}
