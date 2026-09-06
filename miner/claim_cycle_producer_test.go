//go:build test

package miner

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// smstStub flushes a well-formed root for every session: 32 bytes of digest,
// then sum and count, which is the layout the claim path reads to decide the
// tree is not empty. Same value for every session ON PURPOSE -- if the root were
// distinctive, a producer naming the wrong session could still look right.
type smstStub struct{ SMSTManager }

func (smstStub) FlushTree(_ context.Context, _ string) ([]byte, error) {
	root := make([]byte, SMSTRootLen)
	binary.BigEndian.PutUint64(root[32:40], 100) // sum
	binary.BigEndian.PutUint64(root[40:48], 10)  // count
	return root, nil
}

// acceptingSupplier accepts every claim batch. It is deliberately NOT a
// *tx.HASupplierClient: the two type-asserts in the claim path then take their
// else branch, leaving the fee estimate at zero (which makes every claim look
// profitable, so nothing is skipped for economics) and the tx hash empty (used
// only by a nil-safe coordinator call and a log). Verified before writing this.
type acceptingSupplier struct{ pocktclient.SupplierClient }

func (acceptingSupplier) CreateClaims(_ context.Context, _ int64, _ ...pocktclient.MsgCreateClaim) error {
	return nil
}

// erroringService makes the CUPR guard fail open, which is its documented
// behaviour when the session-start service cannot be queried.
type erroringService struct{ pocktclient.ServiceQueryClient }

func (erroringService) GetServiceComputeUnitsPerRelayAtHeight(_ context.Context, _ string, _ int64) (uint64, error) {
	return 0, errServiceUnavailable
}

var errServiceUnavailable = errors.New("service query unavailable in this harness")

// TestOnSessionsNeedClaim_EachSubmittedSessionIsNamedWithItsOwnID enters the
// REAL producer. The consumer test next to this one uses a double that hands
// back a finished result, so it cannot see a producer that names the wrong id --
// measured: replacing snapshot.SessionID with validSnapshots[0].SessionID there
// leaves it green. This is the half that was missing.
//
// Three separate assertions, because the defect they guard against differs:
//   - every submitted session is named (none silently dropped);
//   - each is named with ITS OWN id (the injection above names one session N
//     times; a count of "3 named" is satisfied by three copies of the first);
//   - nothing else is named (no id invented that was never submitted).
func TestOnSessionsNeedClaim_EachSubmittedSessionIsNamedWithItsOwnID(t *testing.T) {
	blocks := &heightedBlocks{}
	// Inside the claim window for a session ending at 100 under default params:
	// past the open offset, far from the close.
	blocks.currentHeight = 103

	lc := &LifecycleCallback{
		logger:         logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:   &defaultParamsShared{},
		blockClient:    blocks,
		smstManager:    smstStub{},
		supplierClient: acceptingSupplier{},
		serviceClient:  erroringService{},
		config:         LifecycleCallbackConfig{ClaimRetryAttempts: 1},
	}

	want := []string{"session-alpha-0000", "session-bravo-0000", "session-delta-0000"}
	snapshots := make([]*SessionSnapshot, 0, len(want))
	for _, id := range want {
		snapshots = append(snapshots, &SessionSnapshot{
			SessionID:               id,
			SessionEndHeight:        100,
			SessionStartHeight:      81,
			SupplierOperatorAddress: "pokt1producer",
			ServiceID:               "svc",
			RelayCount:              10,
			TotalComputeUnits:       100,
			State:                   SessionStateClaiming,
		})
	}

	result, err := lc.OnSessionsNeedClaim(context.Background(), snapshots)
	if err != nil {
		t.Fatalf("the batch was accepted, so the cycle must not error: %v", err)
	}

	for _, id := range want {
		if !result.IsClaimed(id) {
			t.Fatalf("session %q was submitted and must be named; named = %v", id, keysOf(result.Claimed))
		}
	}
	if len(result.Claimed) != len(want) {
		t.Fatalf("only the submitted sessions may be named, got %v", keysOf(result.Claimed))
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = sync.Mutex{}
