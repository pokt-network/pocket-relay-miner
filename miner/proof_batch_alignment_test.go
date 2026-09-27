//go:build test

package miner

import (
	"testing"

	"github.com/stretchr/testify/require"

	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// The chain names the message it refused by index, and that index is only
// meaningful if the three slices describe the same session at the same
// position. Nothing asserted that until now: the alignment held because the
// three loops happened to stay in step.
//
// The inputs are shuffled and distinguishable on purpose. Equal-looking inputs
// pass under any permutation, which is how an ordering test says nothing.
func TestAlignProofBatchKeepsTheThreeSlicesOnTheSameSession(t *testing.T) {
	built := []proofBuildResult{
		{index: 2, snapshot: &SessionSnapshot{SessionID: "s2"}, proofMsg: proofFor("s2")},
		{index: 0, snapshot: &SessionSnapshot{SessionID: "s0"}, proofMsg: proofFor("s0")},
		{index: 3, snapshot: &SessionSnapshot{SessionID: "s3"}, proofMsg: proofFor("s3")},
		{index: 1, snapshot: &SessionSnapshot{SessionID: "s1"}, proofMsg: proofFor("s1")},
	}

	proofMsgs, interfaceMsgs, snapshots := alignProofBatch(built)

	require.Len(t, proofMsgs, 4)
	require.Len(t, interfaceMsgs, 4)
	require.Len(t, snapshots, 4)

	for i, want := range []string{"s0", "s1", "s2", "s3"} {
		require.Equal(t, want, snapshots[i].SessionID,
			"the caller's ordering must survive the worker pool's completion order")
		require.Equal(t, want, proofMsgs[i].SessionHeader.SessionId,
			"proofMsgs[%d] names a different session than validProofSnapshots[%d]", i, i)

		concrete, ok := interfaceMsgs[i].(*prooftypes.MsgSubmitProof)
		require.True(t, ok)
		require.Equal(t, want, concrete.SessionHeader.SessionId,
			"the interface copy drifted from the concrete one at %d", i)
	}
}

// A batch of one still has to come out aligned, and an empty one must not panic:
// both reach this function from the same call site.
func TestAlignProofBatchEdges(t *testing.T) {
	proofMsgs, interfaceMsgs, snapshots := alignProofBatch(nil)
	require.Empty(t, proofMsgs)
	require.Empty(t, interfaceMsgs)
	require.Empty(t, snapshots)

	one := []proofBuildResult{{index: 7, snapshot: &SessionSnapshot{SessionID: "only"}, proofMsg: proofFor("only")}}
	proofMsgs, interfaceMsgs, snapshots = alignProofBatch(one)
	require.Len(t, proofMsgs, 1)
	require.Len(t, interfaceMsgs, 1)
	require.Equal(t, "only", snapshots[0].SessionID)
	require.Equal(t, "only", proofMsgs[0].SessionHeader.SessionId)
}

// alignProofBatch must not reorder its input: the caller keeps using the slice
// it passed in, and a sort in place would rearrange somebody else's data.
func TestAlignProofBatchDoesNotReorderItsInput(t *testing.T) {
	built := []proofBuildResult{
		{index: 2, snapshot: &SessionSnapshot{SessionID: "s2"}, proofMsg: proofFor("s2")},
		{index: 0, snapshot: &SessionSnapshot{SessionID: "s0"}, proofMsg: proofFor("s0")},
	}
	alignProofBatch(built)

	require.Equal(t, 2, built[0].index, "the input was sorted in place")
	require.Equal(t, 0, built[1].index)
}

func proofFor(sessionID string) *prooftypes.MsgSubmitProof {
	return &prooftypes.MsgSubmitProof{
		SessionHeader: &sessiontypes.SessionHeader{SessionId: sessionID},
	}
}
