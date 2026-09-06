//go:build test

package miner

import (
	"context"
	"errors"
	"testing"
	"time"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// flakySupplier fails the first N submissions and accepts every one after that.
// It is the shape the defect needs: a batch that FAILS and then SUCCEEDS, which
// no existing test exercised -- the estate had "always accepts" and "always
// fails", and the bug lives exactly between them.
type flakySupplier struct {
	pocktclient.SupplierClient
	failures int
	calls    int
}

func (f *flakySupplier) CreateClaims(_ context.Context, _ int64, _ ...pocktclient.MsgCreateClaim) error {
	f.calls++
	if f.calls <= f.failures {
		return errors.New("connection refused by the full node")
	}
	return nil
}

// TestOnSessionsNeedClaim_SuccessfulRetryIsNotCountedAsLoss pins that a batch
// which failed once and then succeeded reaches the SUCCESS verdict only.
//
// `lastErr` is set on every failed attempt and was cleared nowhere, so the
// success branch's `break` left it non-nil and the `lastErr != nil` block after
// the loop ran anyway -- on a batch that had just been accepted.
//
// The two assertions are independent, and that is the point: they fail for
// different reasons and neither implies the other.
//
//  1. The cycle must not report an error it does not have. `groupErrs` only
//     grows inside that block.
//  2. No rebroadcast entry may carry OrigTxHash == "". That string is an ORDER
//     to the reconciler -- "never broadcast, resend at SubmitHeight+1" -- so
//     writing it for a claim already in flight burns the single MaxRebroadcasts
//     resend on a duplicate. The failure-path persist has NO `claimTxHash != ""`
//     guard, unlike the success-path one, which is why the bad entry appears
//     even though this harness produces no tx hash.
func TestOnSessionsNeedClaim_SuccessfulRetryIsNotCountedAsLoss(t *testing.T) {
	redisClient, _ := newTestRedis(t)
	store := NewRebroadcastStore(redisClient, time.Hour)

	blocks := &heightedBlocks{}
	blocks.currentHeight = 103

	lc := &LifecycleCallback{
		logger:           logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:     &defaultParamsShared{},
		blockClient:      blocks,
		smstManager:      smstStub{},
		supplierClient:   &flakySupplier{failures: 1},
		serviceClient:    erroringService{},
		rebroadcastStore: store,
		config: LifecycleCallbackConfig{
			ClaimRetryAttempts: 2,
			ClaimRetryDelay:    time.Millisecond,
		},
	}

	const supplier = "pokt1lasterr"
	snapshots := []*SessionSnapshot{{
		SessionID:               "session-retry-0000",
		SessionEndHeight:        100,
		SessionStartHeight:      81,
		SupplierOperatorAddress: supplier,
		ServiceID:               "svc",
		RelayCount:              10,
		TotalComputeUnits:       100,
		State:                   SessionStateClaiming,
	}}

	result, err := lc.OnSessionsNeedClaim(context.Background(), snapshots)
	// Errorf, not Fatalf: the store assertion below guards a DIFFERENT
	// consequence of the same defect, and a fatal here would mask it -- an
	// assertion that only runs once another has already passed proves one
	// thing, not two.
	if err != nil {
		t.Errorf("the second attempt was accepted, so the cycle must not error: %v", err)
	}
	if !result.IsClaimed("session-retry-0000") {
		t.Errorf("an accepted batch must name its session; named = %v", keysOf(result.Claimed))
	}

	pending, listErr := store.List(context.Background(), RebroadcastPhaseClaim, supplier, 100)
	if listErr != nil {
		t.Fatalf("listing the rebroadcast store: %v", listErr)
	}
	for sessionID, raw := range pending {
		entry, decErr := unmarshalRebroadcastEntry(raw)
		if decErr != nil {
			t.Fatalf("stored entry for %q will not decode: %v", sessionID, decErr)
		}
		if entry.OrigTxHash == "" {
			t.Fatalf(
				"session %q was CLAIMED, but its entry says OrigTxHash=\"\" -- that is the "+
					"never-broadcast order, and it burns the one resend on a duplicate",
				sessionID,
			)
		}
	}
}
