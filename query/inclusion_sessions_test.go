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

// The root is derived from the session id so an assertion can tell WHICH claim's
// root travelled. A shared literal would pass even if every entry carried the
// same root, which is the mistake the reconciler would then make silently: it
// compares that root against the one the miner stored for that session.
func claimWithSession(supplier, sessionID string) prooftypes.Claim {
	return prooftypes.Claim{
		SupplierOperatorAddress: supplier,
		SessionHeader:           &sessiontypes.SessionHeader{SessionId: sessionID},
		RootHash:                []byte("root-" + sessionID),
	}
}

// claimWithStatus builds a claim carrying a ProofValidationStatus — the field the
// proof phase discriminates on (only VALIDATED confirms proof inclusion).
func claimWithStatus(supplier, sessionID string, status prooftypes.ClaimProofStatus) prooftypes.Claim {
	c := claimWithSession(supplier, sessionID)
	c.ProofValidationStatus = status
	return c
}

// The whole point of the unified read: one walk carries THREE distinct answers,
// where the pair of queries it replaced could only carry a yes/no each. A proof
// is removed from module state in the EndBlocker of its own block, so proof
// inclusion is read from the claim's ProofValidationStatus.
//
// Asserting the VALUES and not membership is deliberate. The old proven-set test
// could only say "sess-invalid is absent", which is true both when the status is
// read correctly and when it is dropped on the floor; here a rejected claim has
// to be PRESENT and carry SessionProofRejected, which only one of those does.
func TestGetSupplierSessionStates_EachStatusKeepsItsOwnMeaning(t *testing.T) {
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
	got, err := pc.GetSupplierSessionStates(context.Background(), supplier)
	require.NoError(t, err)

	require.Equal(t, map[string]SessionClaim{
		"sess-validated": {ProofState: SessionProofValidated, RootHash: []byte("root-sess-validated")},
		"sess-pending":   {ProofState: SessionProofPending, RootHash: []byte("root-sess-pending")},
		"sess-invalid":   {ProofState: SessionProofRejected, RootHash: []byte("root-sess-invalid")},
	}, got, "every claim is carried, and each keeps the status AND the root the chain gave it")
}

// An unrecognised status must become Unknown, NEVER Rejected. This is the whole
// reason the enum is translated at this boundary instead of being handed through:
// callers treat Unknown as "not proven, keep trying", and a fourth value added
// upstream that arrived as a rejection would silently stop resends that were
// still worth making. Driven through the mapper rather than the wire because a
// value poktroll has not defined yet cannot be built from its own enum.
func TestStateFromClaimStatus_AnUnrecognisedStatusIsUnknownNotRejected(t *testing.T) {
	require.Equal(t, SessionProofValidated, stateFromClaimStatus(prooftypes.ClaimProofStatus_VALIDATED))
	require.Equal(t, SessionProofRejected, stateFromClaimStatus(prooftypes.ClaimProofStatus_INVALID))
	require.Equal(t, SessionProofPending, stateFromClaimStatus(prooftypes.ClaimProofStatus_PENDING_VALIDATION))

	for _, raw := range []int32{3, 4, 99, -1} {
		got := stateFromClaimStatus(prooftypes.ClaimProofStatus(raw))
		require.Equal(t, SessionProofUnknown, got,
			"status %d must map to Unknown so callers keep resending; mapping it to "+
				"Rejected by elimination would abandon a session the chain never condemned", raw)
	}
}

// Pagination is followed to completion, and the state survives the page boundary
// — a second page whose claims lost their status would be invisible to a test
// that only counted sessions.
func TestGetSupplierSessionStates_FollowsPagination(t *testing.T) {
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
			Claims: []prooftypes.Claim{claimWithStatus(supplier, "p2", prooftypes.ClaimProofStatus_INVALID)},
		}, nil
	}

	pc := newProofClientForTest(t, address)
	got, err := pc.GetSupplierSessionStates(context.Background(), supplier)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "pagination must be followed across both pages")
	require.Equal(t, map[string]SessionClaim{
		"p1": {ProofState: SessionProofValidated, RootHash: []byte("root-p1")},
		"p2": {ProofState: SessionProofRejected, RootHash: []byte("root-p2")},
	}, got, "state and root both survive the page boundary")
}

// Errors from the chain are surfaced, so the reconciler takes its degraded path
// rather than silently treating every session as missing.
//
// There used to be a second, identical error test for the claim-side query. Both
// methods were the same pagination body under different predicates, so the pair
// exercised one code path twice; with one method there is one test.
func TestGetSupplierSessionStates_Error(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.allClaimsFunc = func(_ context.Context, _ *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		return nil, status.Error(codes.Unavailable, "node down")
	}

	pc := newProofClientForTest(t, address)
	_, err := pc.GetSupplierSessionStates(context.Background(), "pokt1supplier")
	require.Error(t, err)
}

// A claim with a nil SessionHeader is skipped safely (no panic, no phantom
// session id) — it covers the only branch that dereferences the header.
func TestGetSupplierSessionStates_NilHeaderSkipped(t *testing.T) {
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
	got, err := pc.GetSupplierSessionStates(context.Background(), "pokt1supplier")
	require.NoError(t, err)
	require.Empty(t, got, "nil-header claim contributes no session id")
}

// TestPaginateSupplierClaims_AppliesNoStatusFilter is the guard on the shared
// pagination body, and its job INVERTED with the unified read. It used to prove
// that the VALIDATED-only filter lived in the caller's predicate and not in the
// loop; there is no predicate any more, so what has to be proved is that the loop
// filters NOTHING — every claim with a session header comes back, carrying its
// own state. A status filter reintroduced here would silently re-break the claim
// phase, which needs the rejected and pending ones present.
func TestPaginateSupplierClaims_AppliesNoStatusFilter(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	const supplier = "pokt1supplier"
	mock.allClaimsFunc = func(_ context.Context, req *prooftypes.QueryAllClaimsRequest) (*prooftypes.QueryAllClaimsResponse, error) {
		require.Equal(t, supplier, req.GetSupplierOperatorAddress())
		return &prooftypes.QueryAllClaimsResponse{
			Claims: []prooftypes.Claim{
				claimWithStatus(supplier, "sess-validated", prooftypes.ClaimProofStatus_VALIDATED),
				claimWithStatus(supplier, "sess-pending", prooftypes.ClaimProofStatus_PENDING_VALIDATION),
				claimWithStatus(supplier, "sess-invalid", prooftypes.ClaimProofStatus_INVALID),
				// A claim built without touching the field at all: the chain's
				// own zero value, which is PENDING_VALIDATION and not "unset".
				claimWithSession(supplier, "sess-default"),
			},
		}, nil
	}

	pc := newProofClientForTest(t, address)
	all, err := pc.paginateSupplierClaims(context.Background(), supplier, "all claims")
	require.NoError(t, err)
	require.Equal(t, map[string]SessionClaim{
		"sess-validated": {ProofState: SessionProofValidated, RootHash: []byte("root-sess-validated")},
		"sess-pending":   {ProofState: SessionProofPending, RootHash: []byte("root-sess-pending")},
		"sess-invalid":   {ProofState: SessionProofRejected, RootHash: []byte("root-sess-invalid")},
		"sess-default":   {ProofState: SessionProofPending, RootHash: []byte("root-sess-default")},
	}, all, "the loop keeps every claim with a header, applies no status filter of its own, "+
		"and carries each claim's OWN root -- the rejection diagnosis compares that root "+
		"against what the miner stored, so a shared or dropped one would misdiagnose")
}
