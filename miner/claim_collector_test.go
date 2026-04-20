//go:build test

package miner

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// TestCollectClaimBuildResults_OneFailureDoesNotAbortBatch is the
// regression guard for OnSessionsNeedClaim's batch-abort bug. Before the
// fix the collector returned at the first result.err, discarding every
// other in-flight result. Because each worker has already sealed its
// SMST (FlushTree sets claimedRoot), those discarded sessions become
// un-claimable on subsequent retries and their relays are lost on-chain.
//
// The helper now always partitions all numTasks values into
// (built, skipped, failed). This test exercises the production shape:
// four sessions, one fails buildSessionHeader (simulated via a non-nil
// err), three succeed — the batch must yield three built claims.
func TestCollectClaimBuildResults_OneFailureDoesNotAbortBatch(t *testing.T) {
	numTasks := 4
	resultsCh := make(chan claimBuildResult, numTasks)

	makeSnapshot := func(id string) *SessionSnapshot {
		return &SessionSnapshot{
			SessionID:               id,
			SupplierOperatorAddress: "pokt1supplier_test",
			ServiceID:               "svc1",
		}
	}
	makeClaim := func(id string) *prooftypes.MsgCreateClaim {
		return &prooftypes.MsgCreateClaim{
			SupplierOperatorAddress: "pokt1supplier_test",
			SessionHeader: &sessiontypes.SessionHeader{
				SessionId: id,
			},
			RootHash: []byte("root-" + id),
		}
	}

	// Three successful builds.
	for i, id := range []string{"s_ok_0", "s_ok_1", "s_ok_2"} {
		resultsCh <- claimBuildResult{
			index:    i,
			snapshot: makeSnapshot(id),
			claimMsg: makeClaim(id),
			rootHash: []byte("root-" + id),
		}
	}
	// One failed build (simulated header query failure).
	resultsCh <- claimBuildResult{
		index:    3,
		snapshot: makeSnapshot("s_fail"),
		err:      errors.New("failed to build session header: transient query error"),
	}

	got := collectClaimBuildResults(numTasks, resultsCh)

	require.Len(t, got.built, 3, "three successful sessions must survive a sibling failure")
	require.Len(t, got.skipped, 0, "no sessions were skipped in this scenario")
	require.Len(t, got.failed, 1, "the failing session must be surfaced via the failed bucket")

	// Cross-check every built claim has its identity preserved (no
	// cross-contamination from the failed result).
	builtIDs := map[string]bool{}
	for _, b := range got.built {
		require.NotNil(t, b.snapshot)
		require.NotNil(t, b.claimMsg)
		require.NoError(t, b.err)
		require.False(t, b.skipped)
		builtIDs[b.snapshot.SessionID] = true
	}
	require.True(t, builtIDs["s_ok_0"])
	require.True(t, builtIDs["s_ok_1"])
	require.True(t, builtIDs["s_ok_2"])
	require.False(t, builtIDs["s_fail"])

	// The failed entry must carry the original error for the caller's
	// warn-and-meter path.
	require.Equal(t, "s_fail", got.failed[0].snapshot.SessionID)
	require.Error(t, got.failed[0].err)
	require.Contains(t, got.failed[0].err.Error(), "session header")
}

// TestCollectClaimBuildResults_SkippedSessionsPartitionedCorrectly
// verifies the (built, skipped, failed) partition is exhaustive and
// disjoint when all three outcomes coexist in one batch.
func TestCollectClaimBuildResults_SkippedSessionsPartitionedCorrectly(t *testing.T) {
	numTasks := 5
	resultsCh := make(chan claimBuildResult, numTasks)

	mkSnap := func(id string) *SessionSnapshot {
		return &SessionSnapshot{
			SessionID:               id,
			SupplierOperatorAddress: "pokt1p",
			ServiceID:               "svc1",
		}
	}

	// 2 built
	resultsCh <- claimBuildResult{
		snapshot: mkSnap("built_a"),
		claimMsg: &prooftypes.MsgCreateClaim{},
		rootHash: []byte("root-a"),
	}
	resultsCh <- claimBuildResult{
		snapshot: mkSnap("built_b"),
		claimMsg: &prooftypes.MsgCreateClaim{},
		rootHash: []byte("root-b"),
	}
	// 2 skipped (different reasons)
	resultsCh <- claimBuildResult{
		snapshot:   mkSnap("skip_empty"),
		skipped:    true,
		skipReason: "empty_tree",
	}
	resultsCh <- claimBuildResult{
		snapshot:   mkSnap("skip_unprofitable"),
		skipped:    true,
		skipReason: "unprofitable",
	}
	// 1 failed
	resultsCh <- claimBuildResult{
		snapshot: mkSnap("fail"),
		err:      errors.New("flush failed: transient"),
	}

	got := collectClaimBuildResults(numTasks, resultsCh)

	require.Len(t, got.built, 2)
	require.Len(t, got.skipped, 2)
	require.Len(t, got.failed, 1)

	// Assert disjoint partition.
	total := len(got.built) + len(got.skipped) + len(got.failed)
	require.Equal(t, numTasks, total, "every task must land in exactly one bucket")

	// Skip reasons preserved.
	reasons := map[string]int{}
	for _, s := range got.skipped {
		reasons[s.skipReason]++
	}
	require.Equal(t, 1, reasons["empty_tree"])
	require.Equal(t, 1, reasons["unprofitable"])
}

// TestCollectClaimBuildResults_ZeroTasksReturnsEmpty guards the
// len(candidateSnapshots)==0 edge case at the call site.
func TestCollectClaimBuildResults_ZeroTasksReturnsEmpty(t *testing.T) {
	ch := make(chan claimBuildResult) // unbuffered, never written — must not block
	got := collectClaimBuildResults(0, ch)
	require.Len(t, got.built, 0)
	require.Len(t, got.skipped, 0)
	require.Len(t, got.failed, 0)
}

// TestCollectClaimBuildResults_AllFailuresReportedNoPartialLoss confirms
// that even in the degenerate all-fail case, every failing result is
// reported (not lost to the first-error short-circuit). The caller uses
// this to decide whether to retry the entire group.
func TestCollectClaimBuildResults_AllFailuresReportedNoPartialLoss(t *testing.T) {
	numTasks := 3
	resultsCh := make(chan claimBuildResult, numTasks)
	for i := 0; i < numTasks; i++ {
		resultsCh <- claimBuildResult{
			snapshot: &SessionSnapshot{SessionID: "s" + string(rune('0'+i))},
			err:      errors.New("transient failure"),
		}
	}

	got := collectClaimBuildResults(numTasks, resultsCh)
	require.Len(t, got.built, 0)
	require.Len(t, got.skipped, 0)
	require.Len(t, got.failed, numTasks)
}

// TestCollectClaimBuildResults_BaselineAgainstOldFirstErrorBehavior
// documents (and asserts against) the pre-fix behavior. If we were to
// resurrect the "return on first err" shape the collector had before
// this commit, the same input shape that yields 3 built claims now
// would have yielded ZERO built claims — and the first error would
// have propagated up the stack, discarding the other in-flight
// results. That is precisely how claims were being silently dropped
// in production even though the underlying SMSTs had already been
// sealed. This test encodes that contrast so any future refactor that
// re-introduces the early-return semantics trips this baseline.
func TestCollectClaimBuildResults_BaselineAgainstOldFirstErrorBehavior(t *testing.T) {
	numTasks := 4
	resultsCh := make(chan claimBuildResult, numTasks)
	resultsCh <- claimBuildResult{
		snapshot: &SessionSnapshot{SessionID: "fail_first"},
		err:      errors.New("header build failure"),
	}
	for i, id := range []string{"ok_1", "ok_2", "ok_3"} {
		resultsCh <- claimBuildResult{
			index:    i + 1,
			snapshot: &SessionSnapshot{SessionID: id},
			claimMsg: &prooftypes.MsgCreateClaim{},
			rootHash: []byte("root-" + id),
		}
	}

	// Emulate the OLD collector semantics inline. Drain all so we don't
	// leak a reader; but check how many "built" we'd have captured.
	oldStyleBuilt := 0
	oldStyleReturnedErr := error(nil)
	for i := 0; i < numTasks; i++ {
		r := <-resultsCh
		if oldStyleReturnedErr != nil {
			// Old code would have returned; we keep draining for test
			// hygiene, but count no more builds.
			continue
		}
		if r.err != nil {
			oldStyleReturnedErr = r.err
			continue
		}
		if r.skipped {
			continue
		}
		oldStyleBuilt++
	}
	require.Equal(t, 0, oldStyleBuilt,
		"baseline: the old first-error-returns semantics discards all in-flight builds when the first result is an error")
	require.Error(t, oldStyleReturnedErr)

	// Now show the new helper handles the SAME shape correctly. We need
	// a fresh channel — the old emulation drained the first.
	freshCh := make(chan claimBuildResult, numTasks)
	freshCh <- claimBuildResult{
		snapshot: &SessionSnapshot{SessionID: "fail_first"},
		err:      errors.New("header build failure"),
	}
	for i, id := range []string{"ok_1", "ok_2", "ok_3"} {
		freshCh <- claimBuildResult{
			index:    i + 1,
			snapshot: &SessionSnapshot{SessionID: id},
			claimMsg: &prooftypes.MsgCreateClaim{},
			rootHash: []byte("root-" + id),
		}
	}
	newStyle := collectClaimBuildResults(numTasks, freshCh)
	require.Len(t, newStyle.built, 3, "new helper must preserve the three successful sibling builds")
	require.Len(t, newStyle.failed, 1)
}
