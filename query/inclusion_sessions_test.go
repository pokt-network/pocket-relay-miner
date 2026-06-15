//go:build test

package query

import (
	"context"
	"testing"
	"time"

	sdkquery "github.com/cosmos/cosmos-sdk/types/query"
	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newProofClientForTest(t *testing.T, address string) *proofQueryClient {
	t.Helper()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = qc.Close() })
	pc, ok := qc.Proof().(*proofQueryClient)
	require.True(t, ok, "Proof() must be the concrete *proofQueryClient")
	return pc
}

func claimWithSession(supplier, sessionID string) prooftypes.Claim {
	return prooftypes.Claim{
		SupplierOperatorAddress: supplier,
		SessionHeader:           &sessiontypes.SessionHeader{SessionId: sessionID},
		RootHash:                []byte("root"),
	}
}

// claimWithStatus builds a claim carrying a ProofValidationStatus — the field
// GetSupplierProvenSessions filters on (only VALIDATED counts as proof-included).
func claimWithStatus(supplier, sessionID string, status prooftypes.ClaimProofStatus) prooftypes.Claim {
	c := claimWithSession(supplier, sessionID)
	c.ProofValidationStatus = status
	return c
}

// Happy path: only claims whose proof is VALIDATED count as proven. A proof is
// removed from module state in the EndBlocker of its submission block, so proof
// inclusion is read from the claim's ProofValidationStatus — PENDING_VALIDATION
// (proof not yet validated) and INVALID (proof rejected) must NOT appear.
func TestGetSupplierProvenSessions_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	const supplier = "pokt1supplier"
	mock.allClaimsFunc = func(_ context.Context, req *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		// Must filter by the supplier secondary index (not by tx hash / height).
		require.Equal(t, supplier, req.GetSupplierOperatorAddress())
		return &prooftypes.QueryAllClaimsResponse{
			Claims: []prooftypes.Claim{
				claimWithStatus(supplier, "sess-validated", prooftypes.ClaimProofStatus_VALIDATED),
				claimWithStatus(supplier, "sess-pending", prooftypes.ClaimProofStatus_PENDING_VALIDATION),
				claimWithStatus(supplier, "sess-invalid", prooftypes.ClaimProofStatus_INVALID),
			},
		}, nil
	}

	pc := newProofClientForTest(t, address)
	got, err := pc.GetSupplierProvenSessions(context.Background(), supplier)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the VALIDATED claim is proven")
	_, hasValidated := got["sess-validated"]
	require.True(t, hasValidated, "VALIDATED claim must be in the proven set")
	_, hasPending := got["sess-pending"]
	require.False(t, hasPending, "PENDING_VALIDATION claim must NOT be proven (proof still missing)")
	_, hasInvalid := got["sess-invalid"]
	require.False(t, hasInvalid, "INVALID claim must NOT be proven (proof was rejected)")
}

// Pagination is followed to completion across multiple pages.
func TestGetSupplierProvenSessions_FollowsPagination(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	const supplier = "pokt1supplier"
	var calls int
	mock.allClaimsFunc = func(_ context.Context, req *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		calls++
		// First call: no key, return page 1 + a NextKey. Second call: must carry
		// the key, return page 2 with no NextKey (terminate).
		if len(req.GetPagination().GetKey()) == 0 {
			return &prooftypes.QueryAllClaimsResponse{
				Claims:     []prooftypes.Claim{claimWithStatus(supplier, "p1", prooftypes.ClaimProofStatus_VALIDATED)},
				Pagination: &sdkquery.PageResponse{NextKey: []byte("next")},
			}, nil
		}
		require.Equal(t, []byte("next"), req.GetPagination().GetKey())
		return &prooftypes.QueryAllClaimsResponse{
			Claims: []prooftypes.Claim{claimWithStatus(supplier, "p2", prooftypes.ClaimProofStatus_VALIDATED)},
		}, nil
	}

	pc := newProofClientForTest(t, address)
	got, err := pc.GetSupplierProvenSessions(context.Background(), supplier)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "pagination must be followed across both pages")
	require.Len(t, got, 2)
	_, has1 := got["p1"]
	_, has2 := got["p2"]
	require.True(t, has1)
	require.True(t, has2)
}

// Errors from the chain are surfaced (so the reconciler can fall back to a
// poll_error outcome rather than silently treating everything as missing).
func TestGetSupplierProvenSessions_Error(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.allClaimsFunc = func(_ context.Context, _ *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		return nil, status.Error(codes.Unavailable, "node down")
	}

	pc := newProofClientForTest(t, address)
	_, err := pc.GetSupplierProvenSessions(context.Background(), "pokt1supplier")
	require.Error(t, err)
}

func TestGetSupplierClaimSessions_FollowsPagination(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	const supplier = "pokt1supplier"
	var calls int
	mock.allClaimsFunc = func(_ context.Context, req *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		calls++
		if len(req.GetPagination().GetKey()) == 0 {
			return &prooftypes.QueryAllClaimsResponse{
				Claims:     []prooftypes.Claim{claimWithSession(supplier, "c1")},
				Pagination: &sdkquery.PageResponse{NextKey: []byte("next")},
			}, nil
		}
		require.Equal(t, []byte("next"), req.GetPagination().GetKey())
		return &prooftypes.QueryAllClaimsResponse{Claims: []prooftypes.Claim{claimWithSession(supplier, "c2")}}, nil
	}

	pc := newProofClientForTest(t, address)
	got, err := pc.GetSupplierClaimSessions(context.Background(), supplier)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Len(t, got, 2)
}

func TestGetSupplierClaimSessions_Error(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.allClaimsFunc = func(_ context.Context, _ *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		return nil, status.Error(codes.Unavailable, "node down")
	}
	pc := newProofClientForTest(t, address)
	_, err := pc.GetSupplierClaimSessions(context.Background(), "pokt1supplier")
	require.Error(t, err)
}

// A VALIDATED claim with a nil SessionHeader is skipped safely (no panic, no
// phantom session id) — covers the only branch that dereferences the header.
func TestGetSupplierProvenSessions_NilHeaderSkipped(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.allClaimsFunc = func(_ context.Context, _ *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		return &prooftypes.QueryAllClaimsResponse{
			Claims: []prooftypes.Claim{
				// VALIDATED but nil SessionHeader — must be skipped, not panic.
				{SupplierOperatorAddress: "pokt1supplier", ProofValidationStatus: prooftypes.ClaimProofStatus_VALIDATED},
			},
		}, nil
	}
	pc := newProofClientForTest(t, address)
	got, err := pc.GetSupplierProvenSessions(context.Background(), "pokt1supplier")
	require.NoError(t, err)
	require.Empty(t, got, "nil-header claim contributes no session id")
}

func TestGetSupplierClaimSessions_Success(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	const supplier = "pokt1supplier"
	mock.allClaimsFunc = func(_ context.Context, req *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		require.Equal(t, supplier, req.GetSupplierOperatorAddress())
		return &prooftypes.QueryAllClaimsResponse{
			Claims: []prooftypes.Claim{
				claimWithSession(supplier, "csess-a"),
				claimWithSession(supplier, "csess-b"),
			},
		}, nil
	}

	pc := newProofClientForTest(t, address)
	got, err := pc.GetSupplierClaimSessions(context.Background(), supplier)
	require.NoError(t, err)
	require.Len(t, got, 2)
	_, hasA := got["csess-a"]
	require.True(t, hasA)
}
