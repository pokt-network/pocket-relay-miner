//go:build test

package miner

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"

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

// failingShared fails params for the FIRST height it is asked about and answers
// normally afterwards, so the first group aborts and the second must still run.
type failingShared struct {
	pocktclient.SharedQueryClient
	mu     sync.Mutex
	failed map[int64]bool
}

func (s *failingShared) GetParamsAtHeight(_ context.Context, height int64) (*sharedtypes.Params, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = map[int64]bool{}
	}
	if height == 99 {
		s.failed[height] = true
		return nil, errors.New("params unavailable for this height")
	}
	p := sharedtypes.DefaultParams()
	return &p, nil
}

// TestOnSessionsNeedClaim_AFailingGroupDoesNotTakeTheNextOneWithIt pins the
// second half: the claim cycle used to have six function-level returns inside
// its group loop, so the first group that could not submit abandoned every group
// behind it -- those sessions reaching no verdict, with `claiming` having a
// single exit and no retry.
//
// The assertion is that the SECOND group's session is claimed, by name, while
// the first group's is not. "One session was claimed" is true of both the fixed
// and the broken code when the wrong one is claimed.
func TestOnSessionsNeedClaim_AFailingGroupDoesNotTakeTheNextOneWithIt(t *testing.T) {
	blocks := &heightedBlocks{}
	blocks.currentHeight = 103

	lc := &LifecycleCallback{
		logger:         logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:   &failingShared{},
		blockClient:    blocks,
		smstManager:    smstStub{},
		supplierClient: acceptingSupplier{},
		serviceClient:  erroringService{},
		config:         LifecycleCallbackConfig{ClaimRetryAttempts: 1},
	}

	// Two groups, both inside their claim window at this height. The one that
	// fails is end height 99, which sorts FIRST -- so a cycle that abandons the
	// rest on the first failure never reaches the other one.
	result, err := lc.OnSessionsNeedClaim(context.Background(), []*SessionSnapshot{
		{SessionID: "doomed-group-00000", SessionEndHeight: 99, SessionStartHeight: 80,
			SupplierOperatorAddress: "pokt1aborts", ServiceID: "svc", RelayCount: 10,
			TotalComputeUnits: 100, State: SessionStateClaiming},
		{SessionID: "behind-it-0000000", SessionEndHeight: 100, SessionStartHeight: 81,
			SupplierOperatorAddress: "pokt1aborts", ServiceID: "svc", RelayCount: 10,
			TotalComputeUnits: 100, State: SessionStateClaiming},
	})

	if err == nil {
		t.Fatal("the first group failed, so the cycle must report it")
	}
	if !result.IsClaimed("behind-it-0000000") {
		t.Fatalf("the group behind the failing one must still be claimed; claimed = %v", keysOf(result.Claimed))
	}
	if result.IsClaimed("doomed-group-00000") {
		t.Fatal("the group whose params failed never submitted and must not be claimed")
	}
}

// TestOnSessionsNeedClaim_GroupsAtTheCallSite is the twin of the proof path's
// call-site test, and it exists because in this file every fix has a twin: the
// two cycles are built alike, so an arrangement pinned on one side stays unpinned
// on the other until someone looks.
//
// It used to need two cases, one per branch of a flag. The flag is gone, and with
// it the branch -- so the injection that motivated this test (swapping the two
// branches, which made batched-by-default send one claim per transaction while
// the flag meant to stop that started it) is no longer WRITABLE. What is left to
// pin is that this path groups by end height at all, which is one case.
//
// The count comes from the params spy: it fails every height, so each group dies
// at the loop's first hop and the number of heights asked for IS the number of
// groups -- a cheap way to ask about the PARTITION without reaching submission.
func TestOnSessionsNeedClaim_GroupsAtTheCallSite(t *testing.T) {
	const sharedEndHeight = 909320 // one height, the way the chain's grid produces them

	spy := &heightSpyShared{failWi: errors.New("params unavailable at this height")}
	lc := &LifecycleCallback{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient: spy,
	}

	snapshots := []*SessionSnapshot{
		{SessionID: "claim-session-alpha", SessionEndHeight: sharedEndHeight},
		{SessionID: "claim-session-bravo", SessionEndHeight: sharedEndHeight},
		{SessionID: "claim-session-delta", SessionEndHeight: sharedEndHeight},
	}

	if _, err := lc.OnSessionsNeedClaim(context.Background(), snapshots); err == nil {
		t.Fatal("every group failed, so the cycle must report it")
	}

	asked := spy.askedHeights()
	if len(asked) != 1 {
		t.Fatalf("sessions sharing an end height must ride one transaction: want 1 group, got %d (heights asked: %v)",
			len(asked), asked)
	}
}
