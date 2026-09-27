//go:build test

package redis

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/miner"
)

// A record whose claim nobody reported on must read as unknown, in the table
// and in the filters. It used to read as a failure: the proof path creates a
// record with no claim fields at all, and a zero bool renders "✗ FAILED", so
// 250 claims that had been broadcast and paid were listed as failures by the
// command an operator runs during an incident -- including under --failed-only.
func TestSubmissions_AClaimNobodyReportedOnReadsAsUnknown(t *testing.T) {
	unknown := submissionRecord{SessionEnd: 40, Service: "svc-a", ProofTxHash: "0xp", ProofSuccess: true}
	accepted := submissionRecord{
		SessionEnd: 60, Service: "svc-a", ClaimTxHash: "0xc",
		ClaimSuccess: true, ClaimBroadcastOutcome: miner.ClaimBroadcastAccepted, ProofTxHash: "0xp", ProofSuccess: true,
	}
	rejected := submissionRecord{
		SessionEnd: 80, Service: "svc-a", ClaimErrorReason: "mempool full",
		ClaimBroadcastOutcome: miner.ClaimBroadcastRejected,
	}
	// Written by a binary from before claim_broadcast_outcome existed.
	legacyAccepted := submissionRecord{SessionEnd: 20, Service: "svc-a", ClaimTxHash: "0xc", ClaimSuccess: true}

	require.Equal(t, "", claimOutcome(unknown), "LINK claim-unknown-cli: no claim answer is no answer")
	require.Equal(t, miner.ClaimBroadcastAccepted, claimOutcome(accepted))
	require.Equal(t, miner.ClaimBroadcastRejected, claimOutcome(rejected))
	require.Equal(t, miner.ClaimBroadcastAccepted, claimOutcome(legacyAccepted),
		"a record from an older binary still reads through the bool")

	out := capture(t, func() { printSubmissionsTable([]submissionRecord{unknown, accepted, rejected}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 5, "header, rule, and one line per record:\n%s", out)
	require.Contains(t, lines[2], "-", "LINK claim-unknown-cli: an unknown claim renders as -")
	require.NotContains(t, lines[2], "FAILED", "and never as failed:\n%s", out)
	require.Contains(t, lines[3], "✓ SUCCESS")
	require.Contains(t, lines[4], "✗ FAILED")

	require.False(t, matchesOutcomeFilters(unknown, true, false),
		"LINK claim-unknown-cli: --failed-only must not list a claim nobody reported on")
	require.True(t, matchesOutcomeFilters(rejected, true, false), "it lists a claim that was rejected")
	require.False(t, matchesOutcomeFilters(unknown, false, true),
		"and --success-only must not list it either")
	require.True(t, matchesOutcomeFilters(accepted, false, true))
}
