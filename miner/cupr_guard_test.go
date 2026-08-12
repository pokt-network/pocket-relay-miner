//go:build test

package miner

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/stretchr/testify/require"
)

// fakeCUPRAtHeightClient models the chain's CUPR history: a value per height,
// plus an optional error. It records the height it was asked for so tests can
// prove the guard queries at session START, not at the latest height.
type fakeCUPRAtHeightClient struct {
	mu        sync.Mutex
	byHeight  map[int64]uint64
	err       error
	gotHeight int64
	calls     int
}

func (f *fakeCUPRAtHeightClient) GetServiceComputeUnitsPerRelayAtHeight(_ context.Context, _ string, blockHeight int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotHeight = blockHeight
	if f.err != nil {
		return 0, f.err
	}
	return f.byHeight[blockHeight], nil
}

// TestEvaluateClaimCUPRGuard_PostSessionCUPRChangeStillAllowsClaim is the P0.4
// regression test.
//
// Before v0.1.35 the chain priced a claim with the LATEST CUPR, so this guard
// read live. v0.1.35 moved both x/proof claim validation and x/tokenomics
// settlement to the session-START CUPR. A guard still reading live would
// terminally skip — result.skipped has no retry — a claim the chain would have
// accepted, forfeiting the whole session's revenue with no on-chain error.
//
// Sequence: session mined entirely at CUPR 100, owner raises it to 200 AFTER the
// session ends, claim is built during the claim window.
func TestEvaluateClaimCUPRGuard_PostSessionCUPRChangeStillAllowsClaim(t *testing.T) {
	const (
		sessionStartHeight = int64(100)
		latestHeight       = int64(180)
		relays             = uint64(1783)
		cuprAtSessionStart = uint64(100)
		cuprNow            = uint64(200)
	)

	client := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{
		sessionStartHeight: cuprAtSessionStart,
		latestHeight:       cuprNow,
	}}

	smstSum := relays * cuprAtSessionStart

	allowed, cupr, err := evaluateClaimCUPRGuard(
		context.Background(), client, "seda", sessionStartHeight, smstSum, relays,
	)

	require.NoError(t, err)
	require.True(t, allowed, "a post-session CUPR change must not skip a claim the chain would accept")
	require.Equal(t, cuprAtSessionStart, cupr)
	require.Equal(t, sessionStartHeight, client.gotHeight, "guard must query at session START height")
	require.Equal(t, 1, client.calls, "guard must not cache-bust; one query per claim")
}

// TestEvaluateClaimCUPRGuard_DegradedQueryFailsOpen is the F2 regression at the
// guard boundary. While the query layer's codes.Unimplemented cooldown is armed
// (pre-v0.1.35 node, or an ingress/LB blip), GetServiceComputeUnitsPerRelayAtHeight
// returns ErrCUPRAtHeightUnavailable instead of a live value. The guard MUST fail
// open on it: it cannot resolve the session-start CUPR, so it cannot prove a
// mismatch, and terminally skipping (result.skipped has no retry) would forfeit a
// payable session. A prior bug returned the LIVE cupr with a nil error here, which
// the guard could not distinguish from a real at-height answer — so a session mined
// at the old CUPR, compared against the changed live CUPR, was wrongly skipped.
func TestEvaluateClaimCUPRGuard_DegradedQueryFailsOpen(t *testing.T) {
	const (
		sessionStartHeight = int64(100)
		relays             = uint64(1783)
		cuprAtSessionStart = uint64(100)
	)

	// Session mined entirely at the old CUPR; the at-height query is degraded.
	client := &fakeCUPRAtHeightClient{err: query.ErrCUPRAtHeightUnavailable}
	smstSum := relays * cuprAtSessionStart

	allowed, cupr, err := evaluateClaimCUPRGuard(
		context.Background(), client, "seda", sessionStartHeight, smstSum, relays,
	)

	require.ErrorIs(t, err, query.ErrCUPRAtHeightUnavailable)
	require.True(t, allowed, "a degraded at-height query must fail OPEN, never skip a payable claim")
	require.Zero(t, cupr)
}

// TestEvaluateClaimCUPRGuard_MidSessionChangeStillSkips proves the guard has not
// been defanged: a tree genuinely built at two weights is still caught, because
// its sum matches neither the session-start CUPR nor any single value.
func TestEvaluateClaimCUPRGuard_MidSessionChangeStillSkips(t *testing.T) {
	const sessionStartHeight = int64(100)

	client := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{sessionStartHeight: 6312}}

	// The observed incident value: a mixed-weight tree with a non-integer average.
	allowed, cupr, err := evaluateClaimCUPRGuard(
		context.Background(), client, "seda", sessionStartHeight, 11190188, 1783,
	)

	require.NoError(t, err)
	require.False(t, allowed, "a mixed-weight tree must still be skipped")
	require.Equal(t, uint64(6312), cupr)
}

// TestEvaluateClaimCUPRGuard_FailsOpen covers every path where the guard cannot
// prove the claim is doomed. It must allow the claim: a wrongly-skipped claim is
// certain revenue loss, a wrongly-submitted one is only a rejected tx.
func TestEvaluateClaimCUPRGuard_FailsOpen(t *testing.T) {
	testCases := []struct {
		name       string
		client     ClaimCUPRQueryClient
		wantCUPR   uint64
		wantErr    bool
		wantCalled bool
	}{
		{
			name:     "nil client skips the guard",
			client:   nil,
			wantCUPR: 0,
		},
		{
			name:       "query error allows the claim",
			client:     &fakeCUPRAtHeightClient{err: errors.New("chain unreachable")},
			wantCUPR:   0,
			wantErr:    true,
			wantCalled: true,
		},
		{
			name:       "unknown CUPR (zero) allows the claim",
			client:     &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{100: 0}},
			wantCUPR:   0,
			wantCalled: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, cupr, err := evaluateClaimCUPRGuard(
				context.Background(), tc.client, "seda", 100, 1783*6276, 1783,
			)

			require.True(t, allowed, "guard must fail OPEN when it cannot prove a mismatch")
			require.Equal(t, tc.wantCUPR, cupr)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if fake, ok := tc.client.(*fakeCUPRAtHeightClient); ok {
				require.Equal(t, tc.wantCalled, fake.calls > 0)
			}
		})
	}
}

// TestEvaluateClaimCUPRGuard_ConsistentTreeAllowed covers the ordinary case: a
// uniform tree priced at the session-start CUPR passes untouched.
func TestEvaluateClaimCUPRGuard_ConsistentTreeAllowed(t *testing.T) {
	client := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{100: 6312}}

	allowed, cupr, err := evaluateClaimCUPRGuard(
		context.Background(), client, "seda", 100, 1783*6312, 1783,
	)

	require.NoError(t, err)
	require.True(t, allowed)
	require.Equal(t, uint64(6312), cupr)
}

func TestIsClaimCUPRConsistent(t *testing.T) {
	tests := []struct {
		name     string
		smstSum  uint64
		smstCnt  uint64
		cupr     uint64
		expected bool
	}{
		{
			name:     "uniform CUPR matches",
			smstSum:  1783 * 6312,
			smstCnt:  1783,
			cupr:     6312,
			expected: true,
		},
		{
			name:     "mixed weights (non-integer average) is inconsistent",
			smstSum:  11190188, // the observed incident value
			smstCnt:  1783,
			cupr:     6312,
			expected: false,
		},
		{
			name:     "uniform-old sum against changed (new) CUPR is inconsistent",
			smstSum:  1783 * 6276, // mined entirely at old CUPR
			smstCnt:  1783,
			cupr:     6312, // chain now uses new CUPR
			expected: false,
		},
		{
			name:     "unknown CUPR (zero) fails open (consistent)",
			smstSum:  1783 * 6276,
			smstCnt:  1783,
			cupr:     0,
			expected: true,
		},
		{
			name:     "single relay uniform",
			smstSum:  6312,
			smstCnt:  1,
			cupr:     6312,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isClaimCUPRConsistent(tt.smstSum, tt.smstCnt, tt.cupr)
			if got != tt.expected {
				t.Fatalf("isClaimCUPRConsistent(%d, %d, %d) = %v, want %v",
					tt.smstSum, tt.smstCnt, tt.cupr, got, tt.expected)
			}
		})
	}
}
