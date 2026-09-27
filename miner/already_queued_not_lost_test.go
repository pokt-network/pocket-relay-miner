//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// TestANodeHoldingTheClaimDoesNotSettleTheSessionAsLost is the other half of
// re-injection, and without it re-injecting is worse than not doing it.
//
// A re-injection that ARRIVES is answered with ABCI code 19: the node already
// holds these bytes. That is the success case of this whole feature -- the
// transaction is in a mempool and will most likely be included -- and it comes
// back through the same return value as a failure. The retry loop used to read
// it as one: it matches no window filter and no ejection filter, so it fell to
// the generic retry, burned every attempt against a DETERMINISTIC answer, and
// then reported the session's relays and compute units as lost and wrote a
// TERMINAL claim_tx_error.
//
// So the retry that worked would have been the one that killed the session,
// while the chain went on to include the claim.
//
// It deliberately does NOT assert that the session is marked claimed. The
// sentinel's own guarantee forbids it: "already queued" means this node holds
// it now, not that the chain will include it. Unsettled plus persisted is the
// correct outcome, and the reconciler is what resolves it.
func TestANodeHoldingTheClaimDoesNotSettleTheSessionAsLost(t *testing.T) {
	const (
		supplier  = "pokt1alreadyqueued"
		sessionID = "session-already-queued-00"
	)

	store, redisClient := setupTestSessionStore(t)
	t.Cleanup(func() { _ = store.Close() })

	coord := NewSessionCoordinator(testLogger(), store, SMSTRecoveryConfig{SupplierAddress: supplier})
	t.Cleanup(func() { _ = coord.Close() })

	rebroadcast := NewRebroadcastStore(redisClient, time.Hour)

	snap := &SessionSnapshot{
		SessionID:               sessionID,
		SessionEndHeight:        100,
		SessionStartHeight:      81,
		SupplierOperatorAddress: supplier,
		ServiceID:               "svc",
		RelayCount:              10,
		TotalComputeUnits:       100,
		State:                   SessionStateClaiming,
		ClaimedRootHash:         make([]byte, SMSTRootLen),
	}
	if err := store.Save(context.Background(), snap); err != nil {
		t.Fatalf("seeding the session store: %v", err)
	}

	// The node answers every send with "I already have this", which is what a
	// node answers a re-injection it received the first time.
	node := tx.NewTestSupplierNode(t, supplier)
	node.RefuseInCheckTx("sdk", 19, "tx already exists in cache")

	blocks := &heightedBlocks{}
	blocks.currentHeight = 103 // inside the claim window (102..106) for end height 100

	lc := &LifecycleCallback{
		logger:             logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:       &defaultParamsShared{},
		blockClient:        blocks,
		smstManager:        provingSMST{},
		supplierClient:     node.Client,
		serviceClient:      erroringService{},
		rebroadcastStore:   rebroadcast,
		sessionCoordinator: coord,
		config: LifecycleCallbackConfig{
			ClaimRetryAttempts: 3,
			ClaimRetryDelay:    time.Millisecond,
		},
	}

	_, _ = lc.OnSessionsNeedClaim(context.Background(), []*SessionSnapshot{snap})

	if got := stateOf(t, store, sessionID); got == SessionStateClaimTxError {
		t.Errorf("the node reported it ALREADY HOLDS this claim, and the session was settled as %q anyway: "+
			"a terminal state with its relays and compute units counted as lost, for a transaction "+
			"that is in a mempool and will most likely be included", got)
	}

	// And the bytes still reach the reconciler, which is what actually decides
	// whether the claim landed.
	pending, listErr := rebroadcast.List(context.Background(), RebroadcastPhaseClaim, supplier, 100)
	if listErr != nil {
		t.Fatalf("listing the rebroadcast store: %v", listErr)
	}
	if _, ok := pending[sessionID]; !ok {
		t.Errorf("nothing was persisted for the reconciler, so nobody verifies whether the held "+
			"transaction was included; stored = %v", keysOfBytes(pending))
	}
}
