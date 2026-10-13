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
	"github.com/pokt-network/pocket-relay-miner/query"
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

func notFoundClaimClose(t *testing.T) int64 {
	t.Helper()
	p := sharedtypes.DefaultParams()
	return sharedtypes.GetClaimWindowCloseHeight(&p, notFoundSessionEnd)
}

// notFoundAnsweredAt is the NotFound a node answers from height.
func notFoundAnsweredAt(height int64) error {
	return query.AnsweredAt(status.Error(codes.NotFound, "claim not found"), height)
}

// notFoundProofCycle runs OnSessionsNeedProof at height for one proving
// session whose claim query fails with claimErr, and returns the proofs
// signed, the session as stored after the cycle, and whether the cycle settled it.
func notFoundProofCycle(t *testing.T, height int64, claimErr error) (int, *SessionSnapshot, bool) {
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
			return nil, claimErr
		}},
		config: LifecycleCallbackConfig{ProofRetryAttempts: 1, ProofRetryDelay: time.Millisecond},
	}
	result, err := lc.OnSessionsNeedProof(ctx, []*SessionSnapshot{snapshot})
	require.NoError(t, err)
	got, err := store.Get(ctx, snapshot.SessionID)
	require.NoError(t, err)
	return supplier.calls, got, result.IsSettled(snapshot.SessionID)
}

func TestPreProofGuard_ANotFoundBeforeTheLastPassAsksAgain(t *testing.T) {
	notYet := func() float64 {
		return testutil.ToFloat64(proofSkippedTotal.WithLabelValues("pokt1test", t.Name(), ProofSkippedReasonClaimNotFoundYet))
	}
	before := notYet()
	signed, got, settled := notFoundProofCycle(t, notFoundProofClose(t)-2, notFoundAnsweredAt(notFoundProofClose(t)-3))
	require.False(t, settled, "a session with no claim found is not settled")
	require.Zero(t, signed, "no proof is sent for a claim the node does not show")
	require.Equal(t, SessionStateClaimed, got.State, "back to claimed, asked again next block, not claim_missing")
	require.Equal(t, before+1, notYet(), "the deferral is counted as an attempt")
}

func TestPreProofGuard_ANotFoundOnTheLastPassIsClaimMissing(t *testing.T) {
	signed, got, settled := notFoundProofCycle(t, notFoundProofClose(t)-1, notFoundAnsweredAt(notFoundClaimClose(t)))
	require.False(t, settled, "a session with no claim found is not settled")
	require.Zero(t, signed)
	require.Equal(t, SessionStateClaimMissing, got.State, "the last pass that could send a proof books it claim_missing")
}

// On the last pass, a NotFound from a node that answered from before the claim
// window closed, or from an unknown height, is not the chain's verdict: the
// claim may be on chain, and skipping its proof costs a slash. The proof is
// sent; a claim truly absent costs a rejected proof transaction.
func TestPreProofGuard_ANotFoundFromBehindTheClaimWindowOnTheLastPassSendsTheProof(t *testing.T) {
	for name, claimErr := range map[string]error{
		"answered before the claim window closed": notFoundAnsweredAt(notFoundClaimClose(t) - 1),
		"answered from an unknown height":         status.Error(codes.NotFound, "claim not found"),
	} {
		t.Run(name, func(t *testing.T) {
			signed, got, _ := notFoundProofCycle(t, notFoundProofClose(t)-1, claimErr)
			require.Equal(t, 1, signed, "the proof is sent")
			require.NotEqual(t, SessionStateClaimMissing, got.State)
		})
	}
}
