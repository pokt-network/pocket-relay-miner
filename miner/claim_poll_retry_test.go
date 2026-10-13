//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/query"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

func claimPending(t *testing.T, h *reconcilerHarness) int {
	t.Helper()
	m, err := h.store.List(context.Background(), RebroadcastPhaseClaim, hSupplier, hEnd)
	require.NoError(t, err)
	return len(m)
}

// A claim-phase query that fails after the claim window closed is asked again
// while a claim found could still get its proof; only past that is it
// poll_error. Clearing on the first failure left a landed claim in
// claim_tx_error with no proof: a slash for a claim the chain held.
func TestReconciler_ClaimQueryErrorAfterCloseIsAskedAgain(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	retryUntil := int64(testWindowClose + 3)
	h.r.claimPhase.pollRetryUntil = func(*sharedtypes.Params, int64) int64 { return retryUntil }
	h.put(t, RebroadcastPhaseClaim, hSupplier, hEnd, "s1", testSubmit, "tx-s1")
	h.onChainErr = fmt.Errorf("node down")

	for height := int64(testWindowClose + 1); height <= retryUntil; height++ {
		h.r.OnBlock(height)
		require.Empty(t, h.getOutcomes(), "height %d: an unanswered query is not an outcome yet", height)
		require.Equal(t, 1, claimPending(t, h), "height %d: the entry is kept", height)
	}
	require.Equal(t, 0, h.resub.count(), "nothing is resent after the window closed")

	h.r.OnBlock(retryUntil + 1)
	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionPollErr, outcomes[0].outcome, "past the last useful height it is poll_error")
	require.Equal(t, 0, claimPending(t, h))
}

// A kept entry whose next query answers is judged as usual: found.
func TestReconciler_AKeptClaimFoundLaterIsFound(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.r.claimPhase.pollRetryUntil = func(*sharedtypes.Params, int64) int64 { return testWindowClose + 3 }
	h.put(t, RebroadcastPhaseClaim, hSupplier, hEnd, "s1", testSubmit, "tx-s1")
	h.onChainErr = fmt.Errorf("node down")
	h.r.OnBlock(testWindowClose + 1)

	h.mu.Lock()
	h.onChainErr = nil
	h.onChain["s1"] = query.SessionClaim{} // present: the claim phase's found
	h.mu.Unlock()
	h.r.OnBlock(testWindowClose + 2)

	outcomes := h.getOutcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, inclusionFound, outcomes[0].outcome)
}

// The production bound: two blocks before the proof window closes, the last
// height whose found claim the lifecycle can still prove.
func TestClaimPollRetryUntil(t *testing.T) {
	p := sharedtypes.DefaultParams()
	require.Equal(t, sharedtypes.GetProofWindowCloseHeight(&p, 100)-2, claimPollRetryUntil(&p, 100))
	require.Greater(t, claimPollRetryUntil(&p, 100), sharedtypes.GetClaimWindowCloseHeight(&p, 100),
		"premise: with default params there are heights to retry")
}

// The manager wires the retry to the claim phase only: the proof phase's reads
// after its window closed see the claim settled away.
func TestEnsureSharedTrackers_OnlyTheClaimPhaseRetriesAfterClose(t *testing.T) {
	m, _ := provedReconcilerManager(t)
	p := sharedtypes.DefaultParams()
	require.NotNil(t, m.inclusionReconciler.claimPhase.pollRetryUntil)
	require.Equal(t, claimPollRetryUntil(&p, 100), m.inclusionReconciler.claimPhase.pollRetryUntil(&p, 100))
	require.Nil(t, m.inclusionReconciler.proofPhase.pollRetryUntil)
}
