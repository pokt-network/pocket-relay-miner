//go:build test

package miner

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// TestProofDistributionStillDisabled is a CANARY on an upstream assumption, not a
// test of our own code.
//
// poktroll seeds the probabilistic proof requirement with
// BlockHash(GetEarliestSupplierProofCommitHeight(...) - 1), and it also refuses a
// proof committed before that height. Both of those resolve to the proof window open
// height only because the per-supplier distribution spread is commented out upstream
// (x/shared/types/session.go GetEarliestSupplierProofCommitHeight). The miner's proof
// scheduling is written to survive that being re-enabled, but the surrounding timing
// assumptions — and every operator's expectation that proofs go out the moment the
// window opens — are not.
//
// If this test fails, poktroll re-enabled proof distribution. Do NOT "fix" the test:
// re-read OnSessionsNeedProof's scheduling and the proof-window timeout logic first,
// because suppliers will now be spread across the window by design.
func TestProofDistributionStillDisabled(t *testing.T) {
	suppliers := []string{
		"pokt1abcdefghijklmnopqrstuvwxyz0123456789ab",
		"pokt1zyxwvutsrqponmlkjihgfedcba9876543210zz",
		"pokt1garffmur6cyv040x52sa2k90rvzcn52huypp8s",
	}

	paramSets := []sharedtypes.Params{
		// localnet
		{
			NumBlocksPerSession: 10, GracePeriodEndOffsetBlocks: 1, ClaimWindowOpenOffsetBlocks: 1,
			ClaimWindowCloseOffsetBlocks: 8, ProofWindowOpenOffsetBlocks: 0, ProofWindowCloseOffsetBlocks: 8,
		},
		// mainnet / beta live values (verified 2026-08-14)
		{
			NumBlocksPerSession: 20, GracePeriodEndOffsetBlocks: 10, ClaimWindowOpenOffsetBlocks: 11,
			ClaimWindowCloseOffsetBlocks: 10, ProofWindowOpenOffsetBlocks: 1, ProofWindowCloseOffsetBlocks: 10,
		},
		// a non-zero proof window open offset, to catch an offset-vs-spread mix-up
		{
			NumBlocksPerSession: 4, GracePeriodEndOffsetBlocks: 1, ClaimWindowOpenOffsetBlocks: 2,
			ClaimWindowCloseOffsetBlocks: 4, ProofWindowOpenOffsetBlocks: 3, ProofWindowCloseOffsetBlocks: 6,
		},
	}

	for i, params := range paramSets {
		for _, supplier := range suppliers {
			for _, sessionEndHeight := range []int64{20, 100, 1_000, 883_667} {
				name := fmt.Sprintf("params%d/end%d/%s", i, sessionEndHeight, supplier[:10])
				t.Run(name, func(t *testing.T) {
					windowOpen := sharedtypes.GetProofWindowOpenHeight(&params, sessionEndHeight)
					earliest := sharedtypes.GetEarliestSupplierProofCommitHeight(
						&params, sessionEndHeight, nil, supplier,
					)
					require.Equal(t, windowOpen, earliest,
						"poktroll's earliest proof commit height diverged from the proof window "+
							"open height: proof distribution appears to be re-enabled upstream")
				})
			}
		}
	}
}

// TestClaimDistributionStillDisabled is the same canary for the claim side, which
// schedules claim submission at the claim window open height.
func TestClaimDistributionStillDisabled(t *testing.T) {
	params := sharedtypes.Params{
		NumBlocksPerSession: 20, GracePeriodEndOffsetBlocks: 10, ClaimWindowOpenOffsetBlocks: 11,
		ClaimWindowCloseOffsetBlocks: 10, ProofWindowOpenOffsetBlocks: 1, ProofWindowCloseOffsetBlocks: 10,
	}

	for _, supplier := range []string{
		"pokt1abcdefghijklmnopqrstuvwxyz0123456789ab",
		"pokt1garffmur6cyv040x52sa2k90rvzcn52huypp8s",
	} {
		for _, sessionEndHeight := range []int64{20, 1_000, 883_667} {
			// Subtests so a divergence names the supplier and height that produced it,
			// and so one failing combination does not hide the rest of the pattern.
			t.Run(fmt.Sprintf("end%d/%s", sessionEndHeight, supplier[:10]), func(t *testing.T) {
				windowOpen := sharedtypes.GetClaimWindowOpenHeight(&params, sessionEndHeight)
				earliest := sharedtypes.GetEarliestSupplierClaimCommitHeight(
					&params, sessionEndHeight, nil, supplier,
				)
				require.Equal(t, windowOpen, earliest,
					"poktroll's earliest claim commit height diverged from the claim window open "+
						"height: claim distribution appears to be re-enabled upstream")
			})
		}
	}
}
