//go:build test

package tx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// armNotRequired makes the mock answer a simulation the way a node does when the
// chain has decided no proof is needed: the registered description, wrapped by
// baseapp with a message index and flattened by the tx service.
func armNotRequired(srv *testGRPCServer) {
	srv.txServer.FailSimulation(prooftypes.ErrProofNotRequired.Error(), 0, true)
}

func submitOneProof(t *testing.T, tc *TxClient) (string, error) {
	t.Helper()
	hash, _, err := tc.SubmitProofs(context.Background(), budgetTestSupplier, 1000,
		[]*prooftypes.MsgSubmitProof{generateTestProof(t, budgetTestSupplier, "session-1")})
	return hash, err
}

// The fact and the datum are two different questions, and this is the whole
// point of the sentinel: errors.Is answers "was it this condition", errors.As
// answers "which message, and what did the server actually say". Before this
// commit SubmitProofs answered neither -- it returned nil.
func TestProofNotRequiredIsReportedAsAFactAndCarriesTheDatum(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	armNotRequired(srv)

	tc := newBudgetClient(t, srv, TxClientConfig{})
	hash, err := submitOneProof(t, tc)

	require.Empty(t, hash)
	require.Error(t, err, "returning nil here reported an unsent batch as submitted")
	require.ErrorIs(t, err, ErrTxProofNotRequired, "the fact must be askable without matching text")

	var rejection *TxRejection
	require.True(t, errors.As(err, &rejection), "the datum must survive alongside the fact")
	require.Equal(t, TxStageSimulate, rejection.Stage, "1129 arrives during simulation: CheckTx never executes messages")
	require.Contains(t, rejection.RawLog, prooftypes.ErrProofNotRequired.Error())
}

// The needle is the symbol's own registered description, not a copy of it. If
// this equality ever fails, someone typed the text by hand and the classifier
// stopped being protected by the compiler.
func TestTheNeedleIsTheRegisteredDescription(t *testing.T) {
	require.Equal(t, "proof not required", prooftypes.ErrProofNotRequired.Error(),
		"the upstream description changed: the classifier must be re-derived, not patched")

	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	// A DIFFERENT refusal with the same gRPC code must not be mistaken for it.
	// FailedPrecondition alone covers the claim window, a missing claim, a
	// malformed address and two fee-deduction failures.
	srv.txServer.FailSimulation("not enough funds to submit proof", 0, true)

	tc := newBudgetClient(t, srv, TxClientConfig{})
	_, err := submitOneProof(t, tc)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTxProofNotRequired,
		"running out of funds shares a code with proof-not-required and must not share a verdict")
}

// The counter is asserted apart from errors.Is on purpose: counting does not
// distinguish WHICH error fired, and this number has never been measured -- how
// often the local oracle says a proof is required and the chain disagrees.
func TestTheDivergenceIsCounted(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	armNotRequired(srv)

	before := testutil.ToFloat64(txProofNotRequired.WithLabelValues(budgetTestSupplier))
	tc := newBudgetClient(t, srv, TxClientConfig{})
	_, err := submitOneProof(t, tc)
	require.ErrorIs(t, err, ErrTxProofNotRequired)

	require.Equal(t, before+1, testutil.ToFloat64(txProofNotRequired.WithLabelValues(budgetTestSupplier)),
		"the divergence stayed invisible: it was swallowed and reported as a success")
}

// The empty batch is NOT the swallow. It returns ("", nil) because there was
// nothing to submit, and the change must not reach it.
func TestTheEmptyBatchIsUntouched(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	armNotRequired(srv)

	before := testutil.ToFloat64(txProofNotRequired.WithLabelValues(budgetTestSupplier))
	tc := newBudgetClient(t, srv, TxClientConfig{})

	hash, _, err := tc.SubmitProofs(context.Background(), budgetTestSupplier, 1000, nil)
	require.NoError(t, err, "an empty batch is not a refusal")
	require.Empty(t, hash)
	require.Equal(t, before, testutil.ToFloat64(txProofNotRequired.WithLabelValues(budgetTestSupplier)))
	require.Zero(t, srv.txServer.SimulateCalls(), "nothing should have reached the chain")
}

// The inverse of the assertion the previous commit made, and it is written as a
// replacement rather than a deletion: there the scaffolding mapped the sentinel
// back onto today's behaviour and this test pinned that it did; here the
// scaffolding is gone and the same two effects must NOT happen.
//
// Both were side effects of reporting a refusal as a success. The stashed hash
// overwrote whatever a concurrent submission had recorded, with an empty string.
// The fee-cache refresh took its estimate "from the most recent successful
// submission" -- one that never occurred.
func TestTheRefusalEscapesAndLeavesTheClientUntouched(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	armNotRequired(srv)

	tc := newBudgetClient(t, srv, TxClientConfig{})
	client := NewHASupplierClient(tc, budgetTestSupplier, logging.NewLoggerFromConfig(logging.DefaultConfig()))

	// State a real submission would have left behind.
	client.lastProofTxMu.Lock()
	client.lastProofTxHash = "PRIOR-HASH"
	client.lastProofTxMu.Unlock()
	client.feeCacheMu.Lock()
	client.feeCacheUpokt = 9999
	client.feeCacheTime = time.Now()
	client.feeCacheMu.Unlock()

	hash, _, err := client.SubmitProofsReturningHash(context.Background(), 1000,
		generateTestProof(t, budgetTestSupplier, "session-1"))

	require.Empty(t, hash)
	require.ErrorIs(t, err, ErrTxProofNotRequired, "the refusal must reach the caller that can decide per session")

	require.Equal(t, "PRIOR-HASH", client.GetLastProofTxHash(),
		"a refusal overwrote another submission's hash with an empty string")

	client.feeCacheMu.RLock()
	defer client.feeCacheMu.RUnlock()
	require.Equal(t, uint64(9999), client.feeCacheUpokt,
		"the fee estimate was refreshed from a submission that never happened")
	require.False(t, client.feeCacheTime.IsZero())
}

// Exactly one attempt: a refusal that can never succeed must not be retried.
func TestTheRefusalIsNotRetried(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	armNotRequired(srv)

	tc := newBudgetClient(t, srv, TxClientConfig{})
	_, err := submitOneProof(t, tc)
	require.ErrorIs(t, err, ErrTxProofNotRequired)
	require.Equal(t, 1, srv.txServer.SimulateCalls())
}
