//go:build test

package miner

import (
	"testing"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// newTransitionTestManager builds a minimal manager sufficient for exercising the
// pure transition-decision helpers (they only use the logger plus their params arg).
func newTransitionTestManager() *SessionLifecycleManager {
	return &SessionLifecycleManager{
		logger: logging.NewLoggerFromConfig(logging.DefaultConfig()),
	}
}

// TestDetermineTransition_NilParamsFailsSafe verifies that a session whose epoch params
// could not be resolved (GetParamsAtHeight error -> nil) is left untouched, never fired
// into a window-timeout/transition. A transient param-query failure must not lose rewards.
func TestDetermineTransition_NilParamsFailsSafe(t *testing.T) {
	m := newTransitionTestManager()
	for _, state := range []SessionState{
		SessionStateActive, SessionStateClaiming, SessionStateClaimed, SessionStateProving,
	} {
		s := &SessionSnapshot{State: state, SessionEndHeight: 100, SessionStartHeight: 97}
		newState, action := m.determineTransition(s, 1_000_000, nil)
		require.Equal(t, SessionState(""), newState, "state=%s must not transition on nil params", state)
		require.Equal(t, "", action, "state=%s must not transition on nil params", state)
	}
}

// TestSessionMightNeedTransition_NilParamsFailsOpen verifies the pre-filter fails OPEN:
// when params are unavailable the session is still selected for the full Redis-backed
// check rather than silently skipped.
func TestSessionMightNeedTransition_NilParamsFailsOpen(t *testing.T) {
	m := newTransitionTestManager()
	s := &SessionSnapshot{State: SessionStateActive, SessionEndHeight: 100, SessionStartHeight: 97}
	require.True(t, m.sessionMightNeedTransition(s, 1, nil),
		"pre-filter must fail open (return true) when params cannot be resolved")
}

// TestDetermineTransition_DecisionDependsOnEpochParams proves the transition decision is
// driven by the params object passed in. The same session at the same currentHeight yields
// a DIFFERENT decision under old-epoch vs new-epoch (different ClaimWindowOpenOffsetBlocks)
// params — which is exactly why checkSessionTransitions now resolves params per session at
// its own SessionEndHeight instead of using a single live snapshot.
func TestDetermineTransition_DecisionDependsOnEpochParams(t *testing.T) {
	m := newTransitionTestManager()

	oldParams := &sharedtypes.Params{
		NumBlocksPerSession:          4,
		GracePeriodEndOffsetBlocks:   1,
		ClaimWindowOpenOffsetBlocks:  1,
		ClaimWindowCloseOffsetBlocks: 4,
		ProofWindowOpenOffsetBlocks:  0,
		ProofWindowCloseOffsetBlocks: 4,
	}
	// New epoch pushes the claim window open far later for the same sessionEndHeight.
	newParams := &sharedtypes.Params{
		NumBlocksPerSession:          4,
		GracePeriodEndOffsetBlocks:   1,
		ClaimWindowOpenOffsetBlocks:  50,
		ClaimWindowCloseOffsetBlocks: 4,
		ProofWindowOpenOffsetBlocks:  0,
		ProofWindowCloseOffsetBlocks: 4,
	}

	s := &SessionSnapshot{State: SessionStateActive, SessionEndHeight: 104, SessionStartHeight: 101}

	// Pick a height inside the OLD claim window.
	oldOpen := sharedtypes.GetClaimWindowOpenHeight(oldParams, s.SessionEndHeight)
	oldClose := sharedtypes.GetClaimWindowCloseHeight(oldParams, s.SessionEndHeight)
	currentHeight := oldOpen
	require.Less(t, currentHeight, oldClose, "test setup: height must be within the old claim window")

	// Sanity: the new epoch's window opens strictly later, so at currentHeight it is not open yet.
	newOpen := sharedtypes.GetClaimWindowOpenHeight(newParams, s.SessionEndHeight)
	require.Greater(t, newOpen, currentHeight, "test setup: new-epoch window must open later")

	// Under OLD-epoch params the session must move active -> claiming.
	gotOld, _ := m.determineTransition(s, currentHeight, oldParams)
	require.Equal(t, SessionStateClaiming, gotOld, "old-epoch params: should be in claim window")

	// Under NEW-epoch params, at the same height, the window is not open yet -> no transition.
	gotNew, _ := m.determineTransition(s, currentHeight, newParams)
	require.Equal(t, SessionState(""), gotNew, "new-epoch params: window not open -> no transition")
}
