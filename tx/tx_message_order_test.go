//go:build test

package tx

import (
	"context"
	"testing"

	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// The order messages are handed in is the order they must reach the wire,
// because the chain reports a failed batch by INDEX and the caller resolves that
// index against its own slice. Between the caller and the wire the batch is
// copied twice -- into the variadic interface slice, and into the []cosmostypes.Msg
// the tx builder takes -- and both preserve order by construction, with nothing
// asserting it until now.
//
// Getting this wrong does not fail: it records the WRONG session as proved.
func TestBatchReachesTheWireInTheOrderItWasGiven(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	tc := newBudgetClient(t, srv, TxClientConfig{
		BlockTimeProvider: testBlockTime()})
	client := NewHASupplierClient(tc, budgetTestSupplier, logging.NewLoggerFromConfig(logging.DefaultConfig()))

	want := []string{"session-a", "session-b", "session-c", "session-d"}
	msgs := make([]pocktclient.MsgSubmitProof, len(want))
	for i, id := range want {
		msgs[i] = generateTestProof(t, budgetTestSupplier, id)
	}

	_, _, err := client.SubmitProofsReturningHash(context.Background(), 1000, msgs...)
	require.NoError(t, err)

	require.Equal(t, want, broadcastProofSessionIDs(t, srv.getLastTxBytes()),
		"the batch was reordered somewhere between the caller and the wire")
}

// broadcastProofSessionIDs decodes the transaction the server actually received
// and reports the session of each proof, in wire order.
func broadcastProofSessionIDs(t *testing.T, txBytes []byte) []string {
	t.Helper()
	require.NotEmpty(t, txBytes, "no tx broadcast captured")

	var raw txtypes.Tx
	require.NoError(t, raw.Unmarshal(txBytes))
	require.NotNil(t, raw.Body)

	ids := make([]string, 0, len(raw.Body.Messages))
	for i, any := range raw.Body.Messages {
		var msg prooftypes.MsgSubmitProof
		require.NoError(t, msg.Unmarshal(any.Value), "message %d is not a MsgSubmitProof", i)
		require.NotNil(t, msg.SessionHeader, "message %d has no session header", i)
		ids = append(ids, msg.SessionHeader.SessionId)
	}
	return ids
}
