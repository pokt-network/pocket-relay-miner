//go:build test

package relayer

import (
	"testing"

	"github.com/stretchr/testify/require"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// onchainHeader is the authoritative session header a test compares against.
func onchainHeader() *sessiontypes.SessionHeader {
	return &sessiontypes.SessionHeader{
		ApplicationAddress:      "pokt1app",
		ServiceId:               "seda",
		SessionId:               "session-abc",
		SessionStartBlockHeight: 91,
		SessionEndBlockHeight:   100,
	}
}

// TestCompareSessionHeaders_Matching accepts an honest header.
func TestCompareSessionHeaders_Matching(t *testing.T) {
	require.NoError(t, compareSessionHeaders(onchainHeader(), onchainHeader()))
}

// TestCompareSessionHeaders_RejectsForgedFields is the P2.2 regression test.
//
// session_start_block_height and session_end_block_height are client-supplied and
// were previously unverified — only the session ID was compared. Because
// getTargetSessionBlockHeight returns the CLAIMED start height for an active
// session, a client could supply any height inside the real session's range, pass
// the ID check, and have the forged height reach the difficulty target, the mined
// message, and (from v0.1.35) the CUPR the chain prices the claim with.
//
// At proof time the chain re-compares the sampled relay's header against the
// claim's, so a forged relay sampled from the tree yields ErrProofInvalidRelay ->
// invalid proof -> the supplier is SLASHED.
func TestCompareSessionHeaders_RejectsForgedFields(t *testing.T) {
	testCases := []struct {
		name    string
		mutate  func(*sessiontypes.SessionHeader)
		wantMsg string
	}{
		{
			name:    "forged session start height",
			mutate:  func(h *sessiontypes.SessionHeader) { h.SessionStartBlockHeight = 95 },
			wantMsg: "session start height mismatch",
		},
		{
			name:    "forged session end height",
			mutate:  func(h *sessiontypes.SessionHeader) { h.SessionEndBlockHeight = 120 },
			wantMsg: "session end height mismatch",
		},
		{
			name:    "forged application address",
			mutate:  func(h *sessiontypes.SessionHeader) { h.ApplicationAddress = "pokt1other" },
			wantMsg: "application address mismatch",
		},
		{
			name:    "forged service id",
			mutate:  func(h *sessiontypes.SessionHeader) { h.ServiceId = "other" },
			wantMsg: "service ID mismatch",
		},
		{
			name:    "forged session id",
			mutate:  func(h *sessiontypes.SessionHeader) { h.SessionId = "session-xyz" },
			wantMsg: "session ID mismatch",
		},
		{
			name:    "zero start height",
			mutate:  func(h *sessiontypes.SessionHeader) { h.SessionStartBlockHeight = 0 },
			wantMsg: "session start height mismatch",
		},
		{
			name:    "negative end height",
			mutate:  func(h *sessiontypes.SessionHeader) { h.SessionEndBlockHeight = -1 },
			wantMsg: "session end height mismatch",
		},
		{
			name:    "empty application address",
			mutate:  func(h *sessiontypes.SessionHeader) { h.ApplicationAddress = "" },
			wantMsg: "application address mismatch",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			forged := onchainHeader()
			tc.mutate(forged)

			err := compareSessionHeaders(onchainHeader(), forged)
			require.Error(t, err, "a forged header field must be rejected at ingest")
			require.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// TestCompareSessionHeaders_NilHeaders covers the degenerate inputs: a nil
// on-chain header must error rather than panic, and a nil request header must not
// be treated as matching.
func TestCompareSessionHeaders_NilHeaders(t *testing.T) {
	t.Run("nil onchain header errors", func(t *testing.T) {
		err := compareSessionHeaders(nil, onchainHeader())
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil")
	})

	t.Run("nil request header does not match a real session", func(t *testing.T) {
		err := compareSessionHeaders(onchainHeader(), nil)
		require.Error(t, err, "a nil request header must never pass validation")
	})

	t.Run("both nil errors", func(t *testing.T) {
		require.Error(t, compareSessionHeaders(nil, nil))
	})
}
