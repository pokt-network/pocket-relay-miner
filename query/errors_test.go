//go:build test

package query

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestIsEntityNotFound_DefinitiveAbsence pins the ONLY signal that may be read as
// "the chain says this entity does not exist". poktroll answers a missing entity
// with an explicit codes.NotFound, and the query layer wraps errors with
// fmt.Errorf("...: %w", err), so both the bare and the wrapped forms must resolve.
func TestIsEntityNotFound_DefinitiveAbsence(t *testing.T) {
	notFound := status.Error(codes.NotFound, "claim not found")

	tests := []struct {
		name string
		err  error
	}{
		{"bare gRPC NotFound", notFound},
		{"wrapped once, as query.GetClaim wraps it", fmt.Errorf("failed to query claim: %w", notFound)},
		{"wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("failed to query claim: %w", notFound))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.True(t, IsEntityNotFound(tt.err),
				"an explicit NotFound must be recognised through error wrapping")
		})
	}
}

// TestIsEntityNotFound_TransientFailuresFailOpen is the regression test for the
// defect this helper exists to remove.
//
// The previous implementation fell back to strings.Contains(err.Error(), "not found"),
// so every error below — all of them transport or node failures that merely mention
// "not found" — was classified as "the claim is not on-chain". On the pre-proof path
// that skips the proof and marks the session claim_missing, which is TERMINAL: the
// claim then expires and the supplier is slashed. Each of these MUST fail open.
func TestIsEntityNotFound_TransientFailuresFailOpen(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		// "header not found" is a real CometBFT error for a height the node does
		// not have yet or has already pruned.
		{"Unknown carrying CometBFT header not found", status.Error(codes.Unknown, "rpc error: header not found")},
		{"Internal carrying header not found", status.Error(codes.Internal, "header not found")},
		{"Unavailable carrying peer not found", status.Error(codes.Unavailable, "no connection: peer not found")},
		{"wrapped transient carrying not found", fmt.Errorf("failed to query claim: %w", status.Error(codes.Unknown, "header not found"))},
		{"plain transport error carrying not found", errors.New("dial tcp: route not found")},
		{"plain error whose text merely claims absence", errors.New("something: claim not found for session X")},
		{"node unavailable", status.Error(codes.Unavailable, "chain RPC down")},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "context deadline exceeded")},
		{"connection refused", errors.New("connection refused")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.False(t, IsEntityNotFound(tt.err),
				"a failure to get an answer must never be read as a definitive absence")
		})
	}
}

func TestIsEntityNotFound_NilIsNotAbsence(t *testing.T) {
	require.False(t, IsEntityNotFound(nil))
}
