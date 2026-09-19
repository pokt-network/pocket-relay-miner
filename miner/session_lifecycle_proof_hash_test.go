//go:build test

package miner

import (
	"testing"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// proofHashParams gives a proof window that closes at a height these tests can
// name exactly.
func proofHashParams() *sharedtypes.Params {
	return &sharedtypes.Params{
		NumBlocksPerSession:          4,
		GracePeriodEndOffsetBlocks:   1,
		ClaimWindowOpenOffsetBlocks:  1,
		ClaimWindowCloseOffsetBlocks: 4,
		ProofWindowOpenOffsetBlocks:  0,
		ProofWindowCloseOffsetBlocks: 4,
	}
}

// TestDetermineTransition_AProofAlreadySubmittedIsNotATimeout is the defect.
//
// A session still in `proving` when the window closes has had its proof
// transaction accepted by the mempool if it carries a hash -- that is what the
// hash means everywhere else in this package. Calling it a timeout books a loss
// against a session whose proof is on chain, and the operator reads the book.
//
// Measured 2026-09-18: five sessions ended this way after a kill -9, all five
// on chain, all five paid.
//
// LINK: proof-hash-is-not-a-timeout
func TestDetermineTransition_AProofAlreadySubmittedIsNotATimeout(t *testing.T) {
	m := newTransitionTestManager()
	params := proofHashParams()
	const sessionEnd = int64(100)
	closeHeight := sharedtypes.GetProofWindowCloseHeight(params, sessionEnd)

	submitted := &SessionSnapshot{
		State:            SessionStateProving,
		SessionEndHeight: sessionEnd,
		ProofTxHash:      "8E4B2A1C",
	}

	newState, action := m.determineTransition(submitted, closeHeight, params)

	require.Equal(t, SessionStateProved, newState,
		"LINK proof-hash-is-not-a-timeout: a submitted proof ends in proved, the state its callback would have written")
	require.NotEqual(t, "proof_timeout", action,
		"LINK proof-hash-is-not-a-timeout: calling it a timeout is what books the loss")
}

// TestDetermineTransition_NoProofSubmittedIsStillATimeout is the other half,
// and it must NOT move: with no hash no proof transaction ever went out, the
// claim is on chain and will never be paid, and that is a real loss.
//
// LINK: no-hash-is-still-a-loss
func TestDetermineTransition_NoProofSubmittedIsStillATimeout(t *testing.T) {
	m := newTransitionTestManager()
	params := proofHashParams()
	const sessionEnd = int64(100)
	closeHeight := sharedtypes.GetProofWindowCloseHeight(params, sessionEnd)

	neverSent := &SessionSnapshot{
		State:            SessionStateProving,
		SessionEndHeight: sessionEnd,
	}

	newState, action := m.determineTransition(neverSent, closeHeight, params)

	require.Equal(t, SessionStateProofWindowClosed, newState,
		"LINK no-hash-is-still-a-loss: without a hash nothing was ever sent")
	require.Equal(t, "proof_timeout", action,
		"LINK no-hash-is-still-a-loss: that one really is a timeout")
}

// TestDetermineTransition_AProofSubmittedInsideTheWindowStillWaits pins that
// the hash only changes the decision AT the close. Inside the window the
// session keeps waiting for its own callback, which is the path that writes
// `proved` with its metrics and its cleanup.
//
// LINK: hash-only-decides-at-the-close
func TestDetermineTransition_AProofSubmittedInsideTheWindowStillWaits(t *testing.T) {
	m := newTransitionTestManager()
	params := proofHashParams()
	const sessionEnd = int64(100)
	closeHeight := sharedtypes.GetProofWindowCloseHeight(params, sessionEnd)

	submitted := &SessionSnapshot{
		State:            SessionStateProving,
		SessionEndHeight: sessionEnd,
		ProofTxHash:      "8E4B2A1C",
	}

	newState, action := m.determineTransition(submitted, closeHeight-1, params)

	require.Equal(t, SessionState(""), newState,
		"LINK hash-only-decides-at-the-close: inside the window the callback owns the transition")
	require.Equal(t, "", action)
}
