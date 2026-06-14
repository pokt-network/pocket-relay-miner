//go:build test

package tx

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// TestSubmitProofs_SyncAcceptIsSuccess_NoInclusionCheck proves the foundational
// half of the PROOF_MISSING silent-forfeit theory at the tx-client layer:
//
//   - Proofs are broadcast in BROADCAST_MODE_SYNC (tx_client.go:609), so a
//     code-0 response means CheckTx accepted the tx into the MEMPOOL only.
//   - SubmitProofs reports that code-0 response as success.
//   - The client makes ZERO post-broadcast inclusion queries (no GetTx), so it
//     has no way to learn whether the proof was ever included in a block before
//     the proof window closed.
//
// Net: "success" from SubmitProofs == mempool acceptance, NOT on-chain
// inclusion. A proof accepted here and later evicted/never-included is
// forfeited at settlement with the miner none the wiser.
func TestSubmitProofs_SyncAcceptIsSuccess_NoInclusionCheck(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.001upokt"),
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	proofs := []*prooftypes.MsgSubmitProof{
		generateTestProof(t, supplierAddr, "session-1"),
	}

	// CheckTx accepts (broadcastCode defaults to 0). SubmitProofs reports success.
	txHash, err := tc.SubmitProofs(ctx, supplierAddr, 1000, proofs)
	require.NoError(t, err, "SYNC code-0 (mempool acceptance) is reported as success")
	require.NotEmpty(t, txHash)

	require.Equal(t, 1, testServer.getBroadcastCount(), "exactly one BroadcastTx")

	// THE PROOF: the client performed NO post-broadcast inclusion verification.
	// Success was derived solely from the CheckTx code — nothing confirms the
	// tx ever made it into a block.
	require.Equal(t, 0, testServer.getGetTxCount(),
		"BUG SURFACE: SubmitProofs verifies no block inclusion; mempool acceptance is treated as final success")
}

// TestSubmitProofs_OnlyCheckTxErrorsAreSurfaced confirms the corollary: the only
// failure signal the submit path can react to is a CheckTx-level rejection. A
// mempool eviction / non-inclusion after a code-0 CheckTx produces no error
// here, so the lifecycle retry loop (which retries only on error) never fires.
func TestSubmitProofs_OnlyCheckTxErrorsAreSurfaced(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	// A CheckTx-level rejection (non-zero code) — the ONLY thing the submit path
	// can detect. Anything that happens after mempool acceptance is invisible.
	testServer.setBroadcastFailure(11, "out of gas: gas required exceeds allowance")

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.001upokt"),
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	proofs := []*prooftypes.MsgSubmitProof{
		generateTestProof(t, supplierAddr, "session-1"),
	}

	_, err = tc.SubmitProofs(ctx, supplierAddr, 1000, proofs)
	require.Error(t, err, "CheckTx-level rejection is surfaced as an error (retryable)")
	require.Contains(t, err.Error(), "out of gas")

	// Still no inclusion query — failure, like success, is decided at CheckTx.
	require.Equal(t, 0, testServer.getGetTxCount())
}
