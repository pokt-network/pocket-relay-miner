//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// A rebroadcast entry carries the broadcast budget its submission was born
// with, so a resend that signs a new transaction spends the window's budget and
// not the ceiling under `unknown`. The fields existed and the reconciler read
// them, but nothing in production wrote them: every resend fell to the fallback.

const entryBudgetBlockTime = 30

func expectedBudget(t *testing.T, open, close func(*sharedtypes.Params, int64) int64) (time.Duration, string) {
	t.Helper()
	p := sharedtypes.DefaultParams()
	return tx.WindowTimeout(close(&p, 100)-open(&p, 100), entryBudgetBlockTime)
}

func requireEntriesCarry(t *testing.T, store *RebroadcastStore, phase RebroadcastPhase, supplier string, want time.Duration, regime string) {
	t.Helper()
	pending, err := store.List(context.Background(), phase, supplier, 100)
	require.NoError(t, err)
	require.NotEmpty(t, pending, "premise: the cycle stored an entry to resend")
	for sessionID, raw := range pending {
		entry, decErr := unmarshalRebroadcastEntry(raw)
		require.NoError(t, decErr)
		require.Equal(t, int64(want/time.Second), entry.TimeoutSeconds, "session %s: the entry must carry its submission's budget", sessionID)
		require.Equal(t, regime, entry.TimeoutRegime, "session %s: and the rule that decided it", sessionID)
	}
}

func TestRebroadcastEntry_AnAcceptedClaimCarriesItsBudget(t *testing.T) {
	redisClient, _ := newTestRedis(t)
	store := NewRebroadcastStore(redisClient, time.Hour)
	blocks := &heightedBlocks{}
	blocks.currentHeight = 103
	lc := &LifecycleCallback{
		logger:           logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:     &defaultParamsShared{},
		blockClient:      blocks,
		smstManager:      smstStub{},
		supplierClient:   &flakySupplier{},
		serviceClient:    erroringService{},
		rebroadcastStore: store,
		config:           LifecycleCallbackConfig{ClaimRetryAttempts: 1, ClaimRetryDelay: time.Millisecond, BlockTimeSeconds: entryBudgetBlockTime},
	}
	const supplier = "pokt1entrybudget_claim"
	_, err := lc.OnSessionsNeedClaim(context.Background(), []*SessionSnapshot{{
		SessionID: "session-budget-claim", SessionEndHeight: 100, SessionStartHeight: 81,
		SupplierOperatorAddress: supplier, ServiceID: "svc", RelayCount: 10, TotalComputeUnits: 100,
		State: SessionStateClaiming,
	}})
	require.NoError(t, err)

	want, regime := expectedBudget(t, sharedtypes.GetClaimWindowOpenHeight, sharedtypes.GetClaimWindowCloseHeight)
	require.NotEqual(t, tx.TimeoutRegimeUnknown, regime, "premise: the window is measurable, so the budget is not the fallback")
	requireEntriesCarry(t, store, RebroadcastPhaseClaim, supplier, want, regime)
}

func TestRebroadcastEntry_AFailedProofCarriesItsBudget(t *testing.T) {
	redisClient, _ := newTestRedis(t)
	store := NewRebroadcastStore(redisClient, time.Hour)
	blocks := &heightedBlocks{}
	blocks.currentHeight = 108
	lc := &LifecycleCallback{
		logger:           logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:     &defaultParamsShared{},
		blockClient:      blocks,
		smstManager:      provingSMST{},
		supplierClient:   &flakySupplier{failures: 10},
		serviceClient:    erroringService{},
		rebroadcastStore: store,
		config:           LifecycleCallbackConfig{ProofRetryAttempts: 2, ProofRetryDelay: time.Millisecond, BlockTimeSeconds: entryBudgetBlockTime},
	}
	const supplier = "pokt1entrybudget_proof"
	_, _ = lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{{
		SessionID: "session-budget-proof", SessionEndHeight: 100, SessionStartHeight: 81,
		SupplierOperatorAddress: supplier, ServiceID: "svc", RelayCount: 10, TotalComputeUnits: 100,
		State: SessionStateProving, ClaimedRootHash: make([]byte, SMSTRootLen),
	}})

	want, regime := expectedBudget(t, sharedtypes.GetProofWindowOpenHeight, sharedtypes.GetProofWindowCloseHeight)
	requireEntriesCarry(t, store, RebroadcastPhaseProof, supplier, want, regime)
}
