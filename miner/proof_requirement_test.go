//go:build test

package miner

import (
	"context"
	"errors"
	"testing"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRootProvider is a minimal ClaimedRootProvider for testing the
// rehydration path in ProofRequirementChecker.resolveClaimedRoot.
type stubRootProvider struct {
	root []byte
	err  error
}

func (s *stubRootProvider) GetTreeRoot(_ context.Context, _ string) ([]byte, error) {
	return s.root, s.err
}

// TestResolveClaimedRoot_UsesSnapshotFastPath asserts the normal case where
// the snapshot already carries the claimed root — no rehydration happens.
func TestResolveClaimedRoot_UsesSnapshotFastPath(t *testing.T) {
	checker := &ProofRequirementChecker{
		logger: logging.NewLoggerFromConfig(logging.DefaultConfig()),
	}
	snap := &SessionSnapshot{
		SessionID:       "sess-fast",
		ClaimedRootHash: []byte("pre-loaded-root"),
	}

	got, err := checker.resolveClaimedRoot(context.Background(), snap)
	require.NoError(t, err)
	assert.Equal(t, []byte("pre-loaded-root"), got,
		"snapshot's existing ClaimedRootHash must be used as-is")
}

// TestResolveClaimedRoot_RehydratesFromSMSTWhenSnapshotNil reproduces the
// HA failover read-side defense for BUG 2. The snapshot loaded after
// failover has ClaimedRootHash == nil (because OnSessionClaimed's Redis
// write failed and was only logged as Warn). Before the fix,
// claim.GetClaimeduPOKT would panic / return garbage on a nil root and
// IsProofRequired would error → the caller fail-opens to submit a
// proof from a fabricated root. After the fix, resolveClaimedRoot
// rehydrates from the SMST via the provider and backfills the snapshot.
func TestResolveClaimedRoot_RehydratesFromSMSTWhenSnapshotNil(t *testing.T) {
	provider := &stubRootProvider{root: []byte("rehydrated-root")}
	checker := &ProofRequirementChecker{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		rootProvider: provider,
	}
	snap := &SessionSnapshot{
		SessionID:       "sess-ha-failover",
		ClaimedRootHash: nil, // simulates Warn-logged OnSessionClaimed write failure
	}

	got, err := checker.resolveClaimedRoot(context.Background(), snap)
	require.NoError(t, err)
	assert.Equal(t, []byte("rehydrated-root"), got,
		"root must be rehydrated from the SMST provider when snapshot field is nil")
	assert.Equal(t, []byte("rehydrated-root"), snap.ClaimedRootHash,
		"snapshot must be backfilled so downstream consumers see the rehydrated root")
}

// TestResolveClaimedRoot_ReturnsSentinelWhenProviderAbsent asserts that
// without a provider wired, a nil snapshot root is a FATAL condition —
// not a silent fall-open to proof submission with a fabricated root.
func TestResolveClaimedRoot_ReturnsSentinelWhenProviderAbsent(t *testing.T) {
	checker := &ProofRequirementChecker{
		logger: logging.NewLoggerFromConfig(logging.DefaultConfig()),
	}
	snap := &SessionSnapshot{SessionID: "sess-no-provider"}

	got, err := checker.resolveClaimedRoot(context.Background(), snap)
	assert.Nil(t, got, "no root must be returned when we cannot resolve one")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClaimedRootUnavailable),
		"error must unwrap to ErrClaimedRootUnavailable so callers can distinguish it from transient failures")
}

// TestResolveClaimedRoot_ReturnsSentinelWhenSMSTEmpty covers the case
// where the SMST provider responds but has no root for the session
// (e.g., tree was already cleaned up). The caller MUST receive
// ErrClaimedRootUnavailable, not an empty []byte that would be passed
// into claim.GetClaimeduPOKT.
func TestResolveClaimedRoot_ReturnsSentinelWhenSMSTEmpty(t *testing.T) {
	provider := &stubRootProvider{root: nil}
	checker := &ProofRequirementChecker{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		rootProvider: provider,
	}
	snap := &SessionSnapshot{SessionID: "sess-empty-smst"}

	got, err := checker.resolveClaimedRoot(context.Background(), snap)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClaimedRootUnavailable))
}

// TestResolveClaimedRoot_OpaqueProviderErrorStaysTerminal asserts that a
// provider error the classifier cannot recognise stays TERMINAL. A bare
// errors.New carries nothing IsRetryableError or IsShutdownCancelError can
// read — no net.Error, no redis sentinel, no context error — so it is
// treated as an answer ("the root is not available") rather than as silence.
//
// The wording matters: this test used to call that string "transient" and
// assert the same outcome, which read as proof that transient failures were
// covered. They were not; nothing here reaches the deferral path. What
// exercises it is a REAL Redis that stopped answering — see
// proof_deferral_test.go.
func TestResolveClaimedRoot_OpaqueProviderErrorStaysTerminal(t *testing.T) {
	providerErr := errors.New("smst: root payload malformed")
	provider := &stubRootProvider{err: providerErr}
	checker := &ProofRequirementChecker{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		rootProvider: provider,
	}
	snap := &SessionSnapshot{SessionID: "sess-provider-err"}

	got, err := checker.resolveClaimedRoot(context.Background(), snap)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClaimedRootUnavailable),
		"an unclassifiable provider error must stay terminal")
	assert.False(t, errors.Is(err, ErrClaimedRootUnreadable),
		"it must NOT be deferred: nothing says the root would be readable next block")
	assert.ErrorIs(t, err, providerErr,
		"the provider error must stay in the chain so operators see the cause")
}

// TestClaimFromSnapshot_UsesExplicitRoot asserts the new two-argument
// claimFromSnapshot always anchors the claim on the caller-supplied root,
// not on snapshot.ClaimedRootHash. This is the shape change that
// enforces "caller must think about nil roots" at the type level.
func TestClaimFromSnapshot_UsesExplicitRoot(t *testing.T) {
	snap := &SessionSnapshot{
		SessionID:               "sess-explicit",
		SupplierOperatorAddress: "pokt1test",
		ApplicationAddress:      "pokt1app",
		ServiceID:               "svc",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		// ClaimedRootHash deliberately different from the explicit root
		// passed below so the test can prove which one the claim uses.
		ClaimedRootHash: []byte("stale-root"),
	}

	claim := claimFromSnapshot(snap, []byte("explicit-root"))
	require.NotNil(t, claim)
	assert.Equal(t, []byte("explicit-root"), claim.RootHash,
		"claim must anchor on the caller-supplied root, not snapshot.ClaimedRootHash")
	assert.Equal(t, snap.SessionID, claim.SessionHeader.SessionId)
	assert.Equal(t, snap.SupplierOperatorAddress, claim.SupplierOperatorAddress)
}
