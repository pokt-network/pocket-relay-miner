//go:build test

package tx

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// Which failures leave a signed transaction still worth re-injecting.
//
// The two mistakes are not equally priced, and the table encodes that. Keeping
// bytes the chain will refuse again re-sends them on EVERY block until the
// window closes, and the resend never signs the valid replacement that would
// have landed -- an unbounded repeat. Discarding usable bytes costs one
// signature and one extra live transaction. So the default is discard, and only
// answers meaning "the transaction was not judged" preserve.
func TestRejectionPreservesBytes(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "no error at all",
			err:  nil, want: true,
		},
		{
			// Nothing judged the transaction: a cancelled context, a dial
			// failure. Discarding here would make a local hiccup cost a
			// signature for no reason.
			name: "not a chain rejection",
			err:  fmt.Errorf("dial tcp: connection refused"), want: true,
		},
		{
			// THE case re-injection exists for: we never learned whether it
			// arrived, so signing a replacement is how one claim ends up with
			// two live transactions.
			name: "broadcast stage: the send got no answer",
			err:  &TxRejection{Stage: TxStageBroadcast, GRPCCode: 14},
			want: true,
		},
		{
			name: "sdk/19: the node already holds these exact bytes",
			err:  &TxRejection{Stage: TxStageCheckTx, Codespace: "sdk", ABCICode: 19},
			want: true,
		},
		{
			// The opposite of 19 and easy to collapse with it: 20 says the node
			// could not take it at all, so it was never read, let alone refused.
			name: "sdk/20: the mempool was full",
			err:  &TxRejection{Stage: TxStageCheckTx, Codespace: "sdk", ABCICode: 20},
			want: true,
		},
		{
			// Generic ErrInvalidRequest. It CAN mean our own earlier
			// transaction took this unordered nonce, but nothing here can tell
			// that from any other invalid request, so it falls to the safe
			// default. Named in the table so the gap is visible.
			name: "sdk/18 is discarded: generic, and we cannot tell why",
			err:  &TxRejection{Stage: TxStageCheckTx, Codespace: "sdk", ABCICode: 18},
			want: false,
		},
		{
			name: "sdk/30: the height had passed",
			err:  &TxRejection{Stage: TxStageCheckTx, Codespace: "sdk", ABCICode: 30},
			want: false,
		},
		{
			// The codespace is half the identity: codes are registered per
			// codespace, so a 19 from x/proof is an unrelated error and must
			// not be read as "already queued".
			name: "19 from another module is not the sdk's 19",
			err:  &TxRejection{Stage: TxStageCheckTx, Codespace: "proof", ABCICode: 19},
			want: false,
		},
		{
			// Simulation runs the messages: a failure there is a judgement.
			name: "simulate stage is a judgement",
			err:  &TxRejection{Stage: TxStageSimulate, Codespace: "sdk", ABCICode: 20},
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, RejectionPreservesBytes(tt.err))
			// Wrapped the way callers actually see it.
			require.Equal(t, tt.want, RejectionPreservesBytes(fmt.Errorf("resend: %w", tt.err)))
		})
	}
}

// The predicate is exercised through a REAL broadcast, not only against
// rejections built here.
//
// A table that constructs its own inputs proves what the code does WITH them and
// nothing about whether the client produces them: this repository has already
// paid for that once, when a classifier could be made to return false
// unconditionally with every test still green.
func TestRejectionPreservesBytes_ThroughARealBroadcast(t *testing.T) {
	const supplierAddr = "pokt1supplier-preserves"
	testServer, tc := newTimeoutHeightTestClient(t, supplierAddr)
	testServer.setBroadcastFailureFrom("sdk", 20, "mempool is full")

	_, _, err := tc.CreateClaims(context.Background(), supplierAddr, 4321,
		[]*prooftypes.MsgCreateClaim{generateTestClaim(t, supplierAddr, "session-preserves")})

	require.Error(t, err)
	require.True(t, RejectionPreservesBytes(err),
		"a full mempool must reach the caller as a rejection that spares the bytes, "+
			"or the resend signs a new transaction for a node that simply had no room")
}
