//go:build test

package miner

import (
	"math/big"
	"testing"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/stretchr/testify/require"
)

// TestComputeClaimRewardRat exercises the reward formula used by the economic
// viability check. The formula is:
//
//	reward = (totalCU × difficultyMultiplier × CUTTM) / granularity
//
// matching x/proof/types/claim.go:GetClaimeduPOKT. These tests lock the
// formula against the chain's reference and, importantly, verify the big.Rat
// pipeline does not truncate sub-uPOKT rewards (which was the earlier bug
// that caused every session to appear unprofitable at cost=0).
func TestComputeClaimRewardRat(t *testing.T) {
	// Base difficulty → multiplier == 1. Use this for deterministic tests.
	baseHash := protocol.BaseRelayDifficultyHashBz

	tests := []struct {
		name        string
		totalCU     uint64
		cuttm       uint64
		granularity uint64
		// want is the expected reward expressed as a rational "num/denom"
		// string for exact equality regardless of rat normalization.
		wantRatStr string
	}{
		// tilt defaults: cuttm=1, granularity=1_000_000
		// 1 relay of 1000 CU → 1000 × 1 / 1_000_000 = 0.001 uPOKT
		{"tilt sub-uPOKT (1 relay)", 1000, 1, 1_000_000, "1/1000"},
		// 1000 relays at 1000 CU each = 1_000_000 CU → exactly 1 uPOKT
		{"tilt one uPOKT (1000 relays)", 1_000_000, 1, 1_000_000, "1/1"},
		// 2001 relays = 2_001_000 CU → 2.001 uPOKT (crosses the 2-uPOKT floor)
		{"tilt just over threshold", 2_001_000, 1, 1_000_000, "2001/1000"},
		// Realistic mainnet-ish values: cuttm=77033, granularity=1_000_000
		// 10 CU → 10 × 77033 / 1_000_000 = 0.77033 uPOKT
		{"mainnet sub-uPOKT", 10, 77033, 1_000_000, "77033/100000"},
		// 100_000 CU → 100_000 × 77033 / 1_000_000 = 7703.3 uPOKT
		{"mainnet multi uPOKT", 100_000, 77033, 1_000_000, "77033/10"},
		// Zero CU → zero reward
		{"zero CU", 0, 77033, 1_000_000, "0/1"},
		// Granularity of 0 is treated as 1 (defensive)
		{"zero granularity coerced", 1000, 1, 0, "1000/1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeClaimRewardRat(tt.totalCU, baseHash, tt.cuttm, tt.granularity)
			want, ok := new(big.Rat).SetString(tt.wantRatStr)
			require.True(t, ok, "parse want")
			require.Zerof(t, got.Cmp(want), "got=%s want=%s", got.String(), want.String())
		})
	}
}

// TestComputeClaimRewardRat_NoTruncation regression-tests the specific bug
// that caused sub-uPOKT rewards to compare against a cost in big.Int and be
// silently truncated to zero. We verify that the returned Rat's numerator
// is the pre-division product — no premature Div that would lose precision.
func TestComputeClaimRewardRat_NoTruncation(t *testing.T) {
	reward := computeClaimRewardRat(1, protocol.BaseRelayDifficultyHashBz, 1, 1_000_000)

	// 1 × 1 × 1 / 1_000_000 = 1/1_000_000 — a rat with denom 1_000_000.
	require.Equalf(t, "1/1000000", reward.String(), "reward should be 1/1000000, got %s", reward.String())

	// And it must compare correctly to a 1-uPOKT cost.
	cost := new(big.Rat).SetInt64(1)
	require.Negativef(t, reward.Cmp(cost), "reward=%s must be < cost=%s", reward.String(), cost.String())
}
