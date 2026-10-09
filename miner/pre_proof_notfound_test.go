//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// A NotFound for a session's claim at proof time is one node's answer, not the
// chain's verdict: a node behind the chain gives it for a claim that is there,
// and skipping that proof costs a slash. These run the REAL proof cycle.

const notFoundSessionEnd = 100

func notFoundProofClose(t *testing.T) int64 {
	t.Helper()
	p := sharedtypes.DefaultParams()
	return sharedtypes.GetProofWindowCloseHeight(&p, notFoundSessionEnd)
}

// notFoundProofCycle runs OnSessionsNeedProof at height for one proving
// session whose claim the chain answers NotFound, and returns the proofs
// signed and the session as stored after the cycle.
func notFoundProofCycle(t *testing.T, height int64) (int, *SessionSnapshot) {
	t.Helper()
	p := sharedtypes.DefaultParams()
	require.LessOrEqual(t, sharedtypes.GetEarliestSupplierProofCommitHeight(&p, notFoundSessionEnd, nil, "pokt1test"), height-1,
		"premise: the supplier's proof window is open at both heights tested")
	coord, store, _ := setupTestCoordinator(t)
	ctx := context.Background()
	snapshot := &SessionSnapshot{
		SessionID: "sess-notfound", SessionEndHeight: notFoundSessionEnd, SessionStartHeight: 91,
		SupplierOperatorAddress: "pokt1test", ServiceID: t.Name(), RelayCount: 10, TotalComputeUnits: 100,
		State: SessionStateProving, ClaimTxHash: "CLAIMTX", ClaimedRootHash: make([]byte, SMSTRootLen),
	}
	require.NoError(t, store.Save(ctx, snapshot))
	blocks := &heightedBlocks{}
	blocks.currentHeight = height
	supplier := &flakySupplier{}
	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:       &defaultParamsShared{},
		blockClient:        blocks,
		smstManager:        provingSMST{},
		supplierClient:     supplier,
		serviceClient:      erroringService{},
		sessionCoordinator: coord,
		proofQueryClient: &stubProofQueryClient{getClaimFn: func(context.Context, string, string) (pocktclient.Claim, error) {
			return nil, status.Error(codes.NotFound, "claim not found")
		}},
		config: LifecycleCallbackConfig{ProofRetryAttempts: 1, ProofRetryDelay: time.Millisecond},
	}
	result, err := lc.OnSessionsNeedProof(ctx, []*SessionSnapshot{snapshot})
	require.NoError(t, err)
	require.False(t, result.IsSettled(snapshot.SessionID), "a session with no claim found is never settled")
	got, err := store.Get(ctx, snapshot.SessionID)
	require.NoError(t, err)
	return supplier.calls, got
}

func TestPreProofGuard_ANotFoundBeforeTheLastPassAsksAgain(t *testing.T) {
	notYet := func() float64 {
		return testutil.ToFloat64(proofSkippedTotal.WithLabelValues("pokt1test", t.Name(), ProofSkippedReasonClaimNotFoundYet))
	}
	before := notYet()
	signed, got := notFoundProofCycle(t, notFoundProofClose(t)-2)
	require.Zero(t, signed, "no proof is sent for a claim the node does not show")
	require.Equal(t, SessionStateClaimed, got.State, "back to claimed, asked again next block, not claim_missing")
	require.Equal(t, before+1, notYet(), "the deferral is counted as an attempt")
}

func TestPreProofGuard_ANotFoundOnTheLastPassIsClaimMissing(t *testing.T) {
	signed, got := notFoundProofCycle(t, notFoundProofClose(t)-1)
	require.Zero(t, signed)
	require.Equal(t, SessionStateClaimMissing, got.State, "the last pass that could send a proof books it claim_missing")
}
