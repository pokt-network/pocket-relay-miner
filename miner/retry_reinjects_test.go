//go:build test

package miner

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// claimCycleAgainst runs one claim cycle through the REAL supplier client on a
// mock node, which is what makes these tests about transactions rather than
// about a double's bookkeeping.
func claimCycleAgainst(t *testing.T, node *tx.TestSupplierNode, supplier, sessionID string) {
	t.Helper()

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
			ClaimRetryAttempts: 3,
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
}

// TestRetryReinjectsTheSameTransaction pins that a retry of a send that got NO
// ANSWER puts the SAME transaction on the wire again, instead of signing a
// second one.
//
// Signing again makes a transaction with a new unordered nonce, so the node
// cannot tell it is the one it may already hold: the chain's own duplicate
// answer ("I already have this") is designed out between siblings of one retry
// loop. What that costs is one more live transaction, and one more fee, per
// attempt.
//
// THE ASSERTION IS THE BYTES, NOT THE COUNT. "There were two broadcasts" is
// what the previous code did too; the question is whether the second one was
// the same transaction.
func TestRetryReinjectsTheSameTransaction(t *testing.T) {
	const supplier = "pokt1reinjectclaim"

	node := tx.NewTestSupplierNode(t, supplier)
	// No answer: signed and sent, and nobody said whether it arrived. Those are
	// exactly the bytes a retry must re-inject.
	node.FailBroadcasts(errors.New("connection reset by the full node"))

	claimCycleAgainst(t, node, supplier, "session-reinject-0000")

	if node.Broadcasts() < 2 {
		t.Fatalf("a send that got no answer must be retried; broadcasts = %d", node.Broadcasts())
	}
	first, last := node.FirstTxBytes(), node.LastTxBytes()
	if len(first) == 0 {
		t.Fatal("no transaction reached the node, so nothing about retries was exercised")
	}
	if !bytes.Equal(first, last) {
		t.Fatalf("the retry signed a SECOND transaction instead of re-injecting the first "+
			"(%d bytes then %d, and they differ): two live transactions for one claim, "+
			"which the chain cannot recognise as duplicates of each other",
			len(first), len(last))
	}
}

// TestRetryOfAJudgedRefusalSignsAgain is the guard in the opposite direction.
//
// Bytes the chain REFUSED are worth nothing to a resend: re-injecting them asks
// the same question and gets the same answer, burning an attempt of a window
// that is about ten blocks long. Every attempt must carry its own signature
// here, so no two sends are identical.
func TestRetryOfAJudgedRefusalSignsAgain(t *testing.T) {
	const supplier = "pokt1reinjectjudged"

	node := tx.NewTestSupplierNode(t, supplier)
	node.RefuseInCheckTx("sdk", 13, "insufficient fee")

	claimCycleAgainst(t, node, supplier, "session-judged-retry-0000")

	if node.Broadcasts() < 2 {
		t.Fatalf("a judged refusal is still retried; broadcasts = %d", node.Broadcasts())
	}
	if bytes.Equal(node.FirstTxBytes(), node.LastTxBytes()) {
		t.Fatal("the retry RE-INJECTED bytes the chain had already refused: same question, same answer, " +
			"and one attempt of the window spent on it")
	}
}
