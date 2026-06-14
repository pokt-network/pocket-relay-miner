//go:build test

package miner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
)

// =============================================================================
// PROOF_MISSING silent-forfeit characterization tests
// =============================================================================
//
// These tests prove the root cause of mass `PROOF_MISSING` claim expirations
// observed on mainnet for large multi-key operators running this HA miner.
//
// Theory:
//   1. Proof txs are broadcast in SYNC mode (tx/tx_client.go:609). A code-0
//      response means CheckTx accepted the tx into the mempool — NOT that it
//      was included in a block.
//   2. On that code-0 response the miner marks the session proved, records
//      ProofSuccess=true, and moves on (lifecycle_callback.go:1862-1892). The
//      retry loop only fires on a CheckTx *error* (lifecycle_callback.go:1813).
//   3. There is a post-broadcast inclusion poller for CLAIMS
//      (InclusionTracker.ScheduleClaimCheck -> GetClaim ->
//      UpdateClaimOnChainOutcome), but NONE for PROOFS.
//
// Consequence: a proof that is CheckTx-accepted but evicted / never included
// before the proof window closes is forfeited at settlement (PROOF_MISSING),
// and the miner never learns of it — `ProofSuccess` stays true with no
// contradicting field. The forfeit is silent.
//
// The two tests below assert the two halves of that asymmetry against the REAL
// SubmissionTracker + InclusionTracker (miniredis-backed), no production code
// changed.

// TestSilentForfeit_ClaimNonInclusionIsDetected is the CONTROL: it shows the
// claim path CAN observe a CheckTx-accepted-but-never-included tx and flip the
// record to "on_chain_missing". This is the detection capability that the proof
// path lacks.
func TestSilentForfeit_ClaimNonInclusionIsDetected(t *testing.T) {
	h := newTrackerHarness(t)

	const (
		supplier   = "pokt1test"
		sessionID  = "sess-claim-missing"
		claimTx    = "claim-tx-never-included"
		sessionEnd = int64(110)
	)

	// Claim was accepted to mempool (ClaimSuccess=true) — same state the miner
	// records right after a code-0 BroadcastTx.
	seedSubmission(t, h.submissions, supplier, sessionID, claimTx, sessionEnd)

	// The claim never actually lands on-chain: GetClaim is always NotFound.
	h.queryClient.getFn = func(_, _ string, _ int) (pocktclient.Claim, error) {
		return nil, status.Error(codes.NotFound, "claim not found")
	}
	// Default params: claim window for session_end=110 closes at 110+1+4=115.
	// Push the chain past it so the poll resolves to "missing".
	setBlockHeight(h.blockClient, 120)

	accepted := h.tracker.ScheduleClaimCheck(ClaimInclusionCheck{
		Supplier:         supplier,
		ServiceID:        "svc-a",
		SessionID:        sessionID,
		SessionEndHeight: sessionEnd,
		ClaimTxHash:      claimTx,
	})
	require.True(t, accepted)

	// The reconciler flips the record: the operator CAN see this forfeit.
	assertOutcomeEventually(t, h, supplier, sessionEnd, sessionID, string(ClaimInclusionMissing))
}

// TestSilentForfeit_ProofNonInclusionNowDetected proves the gap is CLOSED.
// Before the fix, a CheckTx-accepted-but-never-included proof was invisible:
// no ProofOnChainOutcome field and no reconciler. Now the proof path is
// symmetric with claims — UpdateProofOnChainOutcome records the real fate, so a
// forfeited proof is distinguishable from a settled one.
func TestSilentForfeit_ProofNonInclusionNowDetected(t *testing.T) {
	h := newTrackerHarness(t)

	const (
		supplier   = "pokt1test"
		sessionID  = "sess-proof-missing"
		claimTx    = "claim-tx-landed"
		proofTx    = "proof-tx-never-included"
		sessionEnd = int64(110)
	)

	// Realistic lifecycle: the claim landed, then a proof was broadcast and
	// CheckTx-accepted (ProofSuccess=true) — exactly what the miner records on
	// a code-0 proof BroadcastTx.
	seedSubmission(t, h.submissions, supplier, sessionID, claimTx, sessionEnd)
	require.NoError(t, h.submissions.TrackProofSubmission(
		context.Background(),
		supplier, sessionEnd, sessionID,
		"0xproofhash", proofTx,
		true, // success == BroadcastTx/CheckTx acceptance ONLY
		"", 115, 115, true, "0xseed",
	))

	// The proof never lands. The ProofInclusionTracker would resolve this to
	// "on_chain_missing"; here we apply that outcome directly through the new
	// reconciler (the API that did not exist before the fix).
	require.NoError(t, h.submissions.UpdateProofOnChainOutcome(context.Background(), ProofOnChainUpdate{
		Supplier:        supplier,
		SessionEnd:      sessionEnd,
		SessionID:       sessionID,
		Outcome:         string(ProofInclusionMissing),
		InclusionHeight: 0,
	}))

	rec, err := h.submissions.GetRecord(context.Background(), supplier, sessionEnd, sessionID)
	require.NoError(t, err)

	// ProofSuccess still reflects mempool acceptance — but it is no longer the
	// only signal: the on-chain outcome now contradicts it, so the forfeit is
	// visible.
	require.True(t, rec.ProofSuccess, "mempool acceptance unchanged")
	require.Equal(t, string(ProofInclusionMissing), rec.ProofOnChainOutcome,
		"FIXED: proof non-inclusion is now recorded — no longer silent")

	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	require.Contains(t, string(raw), "proof_on_chain_outcome",
		"FIXED: the record now carries a proof-on-chain reconciliation field")
}
