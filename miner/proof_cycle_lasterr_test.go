//go:build test

package miner

import (
	"context"
	"errors"
	"testing"
	"time"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// SubmitProofsReturningHash mirrors flakySupplier.CreateClaimsReturningHash:
// fail the first N, then accept. It shares the call counter on purpose -- each
// test uses one phase.
func (f *flakySupplier) SubmitProofsReturningHash(_ context.Context, _ int64, _ ...pocktclient.MsgSubmitProof) (string, tx.SignedTxPayload, error) {
	f.calls++
	if f.calls <= f.failures {
		return "", tx.SignedTxPayload{}, errors.New("connection refused by the full node")
	}
	hash, signed := fakeSigned("proof")
	return hash, signed, nil
}

// provingSMST is smstStub plus the one method the proof path needs. Kept
// separate so the claim tests keep the smaller stub: a stub that answers more
// than its path asks hides which call the path actually makes.
type provingSMST struct{ smstStub }

func (provingSMST) ProveClosest(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return []byte("proof-bytes"), nil
}

// TestOnSessionsNeedProof_SuccessfulRetryIsNotCountedAsLoss is the TWIN of the
// claim-side test, and it exists because the defect was in both cycles.
//
// `lastErr` is written in exactly two places in this file (the claim loop and
// this one) and was cleared in neither, so a proof batch that failed once and
// then succeeded also entered its `lastErr != nil` block. The supervision
// reported only the claim half; the twin was found by grepping the assignment
// rather than by reading the site that was reported.
//
// It lands HARDER here than on the claim side: the success branch fills
// result.Settled BEFORE the block runs, so the same session is named settled to
// the caller AND written proof_tx_error in Redis -- two contradictory verdicts
// for one session in one cycle.
//
// THIS TEST DOES NOT OBSERVE THAT CONTRADICTION, and the gap is stated rather
// than papered over. Watching the proof_tx_error write needs a session
// coordinator this harness does not wire. Measured under injection: the
// result.Settled assertion below stays GREEN with the defect present, because
// Settled is populated either way -- it is an accidental witness, not a guard.
// The two assertions that actually bite are the returned error and the
// OrigTxHash entry.
//
// The block height is a PRECONDITION of this path, not setup: for a session
// ending at 100 under default params the proof window is 106..110, and a height
// outside it exits before the submit loop is ever reached (measured with a
// probe: 115 returned "proof window already closed").
func TestOnSessionsNeedProof_SuccessfulRetryIsNotCountedAsLoss(t *testing.T) {
	redisClient, _ := newTestRedis(t)
	store := NewRebroadcastStore(redisClient, time.Hour)

	blocks := &heightedBlocks{}
	blocks.currentHeight = 108

	lc := &LifecycleCallback{
		logger:           logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:     &defaultParamsShared{},
		blockClient:      blocks,
		smstManager:      provingSMST{},
		supplierClient:   &flakySupplier{failures: 1},
		serviceClient:    erroringService{},
		rebroadcastStore: store,
		config: LifecycleCallbackConfig{
			ProofRetryAttempts: 2,
			ProofRetryDelay:    time.Millisecond,
		},
	}

	const supplier = "pokt1prooflasterr"
	result, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{{
		SessionID:               "session-proof-0000",
		SessionEndHeight:        100,
		SessionStartHeight:      81,
		SupplierOperatorAddress: supplier,
		ServiceID:               "svc",
		RelayCount:              10,
		TotalComputeUnits:       100,
		State:                   SessionStateProving,
		ClaimedRootHash:         make([]byte, SMSTRootLen),
	}})

	// Errorf, not Fatalf: each assertion guards a different consequence, and a
	// fatal on the first would mask the two below it.
	if err != nil {
		t.Errorf("the second attempt was accepted, so the cycle must not error: %v", err)
	}
	if _, ok := result.Settled["session-proof-0000"]; !ok {
		t.Errorf("an accepted batch must settle its session, got %v", result.Settled)
	}

	pending, listErr := store.List(context.Background(), RebroadcastPhaseProof, supplier, 100)
	if listErr != nil {
		t.Fatalf("listing the rebroadcast store: %v", listErr)
	}
	for sessionID, raw := range pending {
		entry, decErr := unmarshalRebroadcastEntry(raw)
		if decErr != nil {
			t.Fatalf("stored entry for %q will not decode: %v", sessionID, decErr)
		}
		if entry.OrigTxHash == "" {
			t.Errorf(
				"session %q was SETTLED, but its entry says OrigTxHash=\"\" -- the "+
					"never-broadcast order, which burns the one resend on a duplicate",
				sessionID,
			)
		}
	}
}
