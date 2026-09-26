//go:build test

package miner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// TestPersistedBytesDependOnHowTheSendEnded pins WHICH failed sends leave bytes
// behind for the reconciler to re-inject.
//
// A failed send hands its signed payload back whatever went wrong, so "bytes
// present" does not mean "nobody answered". Bytes the chain REFUSED are worth
// nothing to a resend: re-injecting them asks the same question and gets the
// same answer, while the entry keeps its MsgBytes and a fresh signature would
// have asked something the node has not answered yet.
//
// ONLY THE FIRST CASE IS THE DEFECT. The other three are guards in the opposite
// direction: they go red if the filter starts discarding what it must keep --
// a send that got no answer, and a refusal that says "I already hold this
// transaction", where re-signing would put a second live transaction in gossip.
func TestPersistedBytesDependOnHowTheSendEnded(t *testing.T) {
	tests := []struct {
		name string
		// arm makes every send end the way this case is about.
		arm func(*tx.TestSupplierNode)
		// wantSignedBytes is whether the stored entry must still carry the
		// transaction, so a resend re-injects instead of signing.
		wantSignedBytes bool
		// wantOrigTxHash is whether the send was accepted: the success case is
		// the only one that records a hash.
		wantOrigTxHash bool
	}{
		{
			name: "refused in CheckTx: the chain judged these bytes, they must not be kept",
			arm: func(n *tx.TestSupplierNode) {
				n.RefuseInCheckTx("sdk", 13, "insufficient fee")
			},
			wantSignedBytes: false,
		},
		{
			name: "no answer from the node: the bytes may be in flight and must be kept",
			arm: func(n *tx.TestSupplierNode) {
				n.FailBroadcasts(errors.New("connection reset by the full node"))
			},
			wantSignedBytes: true,
		},
		{
			name: "already in the mempool cache: re-signing would be a second live transaction",
			arm: func(n *tx.TestSupplierNode) {
				n.RefuseInCheckTx("sdk", 19, "tx already exists in cache")
			},
			wantSignedBytes: true,
		},
		{
			name:            "accepted: the success path keeps its transaction and its hash",
			arm:             func(*tx.TestSupplierNode) {},
			wantSignedBytes: true,
			wantOrigTxHash:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const (
				supplier  = "pokt1judgedbytes"
				sessionID = "session-judged-bytes-0000"
			)

			node := tx.NewTestSupplierNode(t, supplier)
			tt.arm(node)

			redisClient, _ := newTestRedis(t)
			store := NewRebroadcastStore(redisClient, time.Hour)

			blocks := &heightedBlocks{}
			blocks.currentHeight = 103

			lc := &LifecycleCallback{
				logger:           logging.NewLoggerFromConfig(logging.DefaultConfig()),
				sharedClient:     &defaultParamsShared{},
				blockClient:      blocks,
				smstManager:      smstStub{},
				supplierClient:   node.Client,
				serviceClient:    erroringService{},
				rebroadcastStore: store,
				config: LifecycleCallbackConfig{
					ClaimRetryAttempts: 2,
					ClaimRetryDelay:    time.Millisecond,
				},
			}

			_, _ = lc.OnSessionsNeedClaim(context.Background(), []*SessionSnapshot{{
				SessionID:               sessionID,
				SessionEndHeight:        100,
				SessionStartHeight:      81,
				SupplierOperatorAddress: supplier,
				ApplicationAddress:      "pokt1ownapp",
				ServiceID:               "svc",
				RelayCount:              10,
				TotalComputeUnits:       100,
				State:                   SessionStateClaiming,
			}})

			pending, err := store.List(context.Background(), RebroadcastPhaseClaim, supplier, 100)
			if err != nil {
				t.Fatalf("listing the rebroadcast store: %v", err)
			}
			raw, ok := pending[sessionID]
			if !ok {
				t.Fatalf("the session must reach the reconciler whatever the send did; stored = %d entries", len(pending))
			}
			entry, decErr := unmarshalRebroadcastEntry(raw)
			if decErr != nil {
				t.Fatalf("stored entry will not decode: %v", decErr)
			}

			// MsgBytes is what makes a discarded payload survivable: without it
			// the session has nothing left to re-send at all.
			if len(entry.MsgBytes) == 0 {
				t.Fatalf("the entry carries no message, so nothing can be re-sent for %q", sessionID)
			}
			if got := len(entry.SignedBytes) > 0; got != tt.wantSignedBytes {
				t.Errorf("stored transaction: got kept=%v, want kept=%v", got, tt.wantSignedBytes)
			}
			if got := entry.OrigTxHash != ""; got != tt.wantOrigTxHash {
				t.Errorf("recorded tx hash: got present=%v, want present=%v (hash %q)", got, tt.wantOrigTxHash, entry.OrigTxHash)
			}
			// A kept transaction without its deadline cannot be re-injected:
			// the reconciler refuses bytes whose expiry it cannot read.
			if tt.wantSignedBytes && entry.SignedTimeoutAt == 0 {
				t.Errorf("the transaction was kept but its deadline was not, so no resend will re-inject it")
			}
		})
	}
}
