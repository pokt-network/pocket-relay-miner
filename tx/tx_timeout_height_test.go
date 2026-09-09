//go:build test

package tx

import (
	"context"
	"errors"
	"testing"

	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// decodeBroadcastTxTimeoutHeight decodes the most recently broadcast TX bytes
// and returns the TimeoutHeight the client set on it.
//
// It reads the WIRE BYTES rather than asking the builder, and that is the point:
// the field is set after the fee and immediately before signing, so decoding
// what actually left the process is what proves it survived both. A getter on
// the builder would stay green even if the value never reached the transaction.
//
// Zero is a legitimate answer -- it is protobuf's default and cosmos-sdk reads
// it as "no height timeout" (basic.go:212 fires only above zero) -- so this
// returns it rather than failing, and each caller states which it expects.
func decodeBroadcastTxTimeoutHeight(t *testing.T, txBytes []byte) uint64 {
	t.Helper()
	require.NotEmpty(t, txBytes, "no tx broadcast captured")

	var raw txtypes.Tx
	require.NoError(t, raw.Unmarshal(txBytes))
	require.NotNil(t, raw.Body, "tx body missing")
	return raw.Body.TimeoutHeight
}

// newTimeoutHeightTestClient builds a client wired to a mock chain, with an
// explicit gas limit so the path under test does not simulate. Simulation is a
// separate concern here: the height is set AFTER the gas is decided, so a test
// that simulated would exercise a different ordering than the one being pinned.
func newTimeoutHeightTestClient(t *testing.T, supplierAddr string) (*testGRPCServer, *TxClient) {
	t.Helper()

	testServer := setupMockGRPCServer(t)
	t.Cleanup(testServer.cleanup)
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	t.Cleanup(func() { _ = km.Close() })

	tc, err := NewTxClient(logger, km, TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.000001upokt"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })

	return testServer, tc
}

// TestSignAndBroadcast_TimeoutHeightIsNotInTheSimulatedTx pins WHERE the height
// is set, which is the decision this feature turns on and which no other test
// here can see: every one of them passes an explicit gas limit, so none of them
// simulates, and moving the SetTimeoutHeight call to the other side of the
// simulation leaves them all green.
//
// The two halves are one assertion. The transaction that reaches Simulate must
// NOT carry the height, so the ante handler's height check cannot fire there --
// it runs in simulation too, and its reply is flattened to codes.Unknown with no
// ABCI code, which would displace the x/proof failure the callers already
// classify. The transaction that reaches BroadcastTx must carry it, so CheckTx
// can refuse an expired window with a code that means exactly that.
//
// gas_limit stays 0 on purpose -- the production default, per the mock's own
// note. With an explicit limit the client never simulates, nothing is captured,
// and the first half would pass by vacuity; the nil check below is what makes
// that failure loud instead of silent.
func TestSignAndBroadcast_TimeoutHeightIsNotInTheSimulatedTx(t *testing.T) {
	const supplierAddr = "pokt1supplier-simulated-tx"
	const windowClose int64 = 5150

	testServer := setupMockGRPCServer(t)
	t.Cleanup(testServer.cleanup)
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	t.Cleanup(func() { _ = km.Close() })

	tc, err := NewTxClient(logger, km, TxClientConfig{
		GRPCEndpoint:  testServer.address,
		ChainID:       "test-chain",
		GasLimit:      0, // simulate, as production does
		GasAdjustment: DefaultGasAdjustment,
		GasPrice:      parseGasPrice(t, "0.000001upokt"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })

	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-simulated-tx"),
	}
	_, err = tc.CreateClaims(context.Background(), supplierAddr, windowClose, claims)
	require.NoError(t, err)

	simulated := testServer.getLastSimulateTxBytes()
	require.NotNil(t, simulated,
		"nothing was simulated, so the first assertion below would be vacuous: "+
			"this test requires gas_limit 0")

	require.Zero(t, decodeBroadcastTxTimeoutHeight(t, simulated),
		"the SIMULATED transaction must not carry the timeout height; setting it "+
			"before the simulation moves the rejection to a layer that reports no "+
			"ABCI code and displaces the one the callers classify")
	require.Equal(t, uint64(windowClose), decodeBroadcastTxTimeoutHeight(t, testServer.getLastTxBytes()),
		"the BROADCAST transaction must carry it: that is where the chain can "+
			"refuse an expired window with a code that says so")
}

// TestSignAndBroadcast_SetsTimeoutHeightFromWindowClose pins that the window
// close every caller already computes actually reaches the transaction.
//
// Before this, the value crossed four layers and died in a blank identifier:
// signAndBroadcast took it as `_ uint64` and SetTimeoutHeight appeared nowhere
// in the tree, so every transaction we sent carried timeout_height = 0 and, as
// far as the SDK's ante handler was concerned, stayed acceptable forever.
// Nothing failed, which is why it survived: a transaction with no height
// timeout is perfectly valid, merely unprotected.
func TestSignAndBroadcast_SetsTimeoutHeightFromWindowClose(t *testing.T) {
	const supplierAddr = "pokt1supplier-timeout-height"
	const windowClose int64 = 4321

	testServer, tc := newTimeoutHeightTestClient(t, supplierAddr)

	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-timeout-height"),
	}
	_, err := tc.CreateClaims(context.Background(), supplierAddr, windowClose, claims)
	require.NoError(t, err)

	got := decodeBroadcastTxTimeoutHeight(t, testServer.getLastTxBytes())
	require.Equal(t, uint64(windowClose), got,
		"the transaction must carry the window close height the caller passed; "+
			"a zero here means the value was discarded again")
}

// TestSignAndBroadcast_SetsTimeoutHeightForProofs is the twin. The two cycles of
// this codebase are built in parallel, and a fix landing in one and not the
// other is its recurring defect, so the proof path gets its own assertion
// instead of sharing the claim one.
func TestSignAndBroadcast_SetsTimeoutHeightForProofs(t *testing.T) {
	const supplierAddr = "pokt1supplier-timeout-height-proof"
	const windowClose int64 = 8765

	testServer, tc := newTimeoutHeightTestClient(t, supplierAddr)

	proofs := []*prooftypes.MsgSubmitProof{
		generateTestProof(t, supplierAddr, "session-timeout-height-proof"),
	}
	_, err := tc.SubmitProofs(context.Background(), supplierAddr, windowClose, proofs)
	require.NoError(t, err)

	got := decodeBroadcastTxTimeoutHeight(t, testServer.getLastTxBytes())
	require.Equal(t, uint64(windowClose), got,
		"the proof transaction must carry its own window close height")
}

// TestSignAndBroadcast_NonPositiveTimeoutHeightLeavesFieldUnset pins the guard,
// and it exists because the obvious way to write that guard does not work.
//
// The conversion to uint64 used to happen at the CALL SITE, so by the time the
// value arrived a -1 had already become 18446744073709551615 -- a number that
// passes `> 0` and reads as a valid far-future height. The guard would have been
// present and would have protected nothing. Keeping the parameter signed is what
// lets it decide, which is why this drives a non-positive value through the
// public entry point rather than asserting on an internal helper.
//
// Zero is what the field must hold in that case, because zero is exactly how
// cosmos-sdk spells "no height timeout" -- so the fallback is the behaviour that
// existed before this feature, not a new one.
func TestSignAndBroadcast_NonPositiveTimeoutHeightLeavesFieldUnset(t *testing.T) {
	const supplierAddr = "pokt1supplier-timeout-height-zero"

	for _, testCase := range []struct {
		name   string
		height int64
	}{
		{name: "zero", height: 0},
		{name: "negative", height: -1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testServer, tc := newTimeoutHeightTestClient(t, supplierAddr)

			claims := []*prooftypes.MsgCreateClaim{
				generateTestClaim(t, supplierAddr, "session-timeout-height-zero"),
			}
			_, err := tc.CreateClaims(context.Background(), supplierAddr, testCase.height, claims)
			require.NoError(t, err)

			got := decodeBroadcastTxTimeoutHeight(t, testServer.getLastTxBytes())
			require.Zero(t, got,
				"a non-positive window close must leave timeout_height unset; a huge "+
					"value here means the sign was lost before the guard could see it")
		})
	}
}

// TestCheckTxRejection_TimeoutHeightIsClassifiedAsWindowExpired pins that the
// chain refusing a transaction on height is recognised as terminal.
//
// The distinction is worth money. A claim whose window has closed can never be
// accepted, so retrying spends attempts and fees on nothing and settles the
// session under a cause naming the wrong problem. Every other CheckTx rejection
// stays retryable, which is why the negative controls below matter as much as
// the positive case: a classifier that said yes to everything would satisfy the
// first row on its own.
func TestCheckTxRejection_TimeoutHeightIsClassifiedAsWindowExpired(t *testing.T) {
	const supplierAddr = "pokt1supplier-window-expired"

	for _, testCase := range []struct {
		name    string
		code    uint32
		rawLog  string
		expired bool
	}{
		{
			name:    "timeout height rejection is terminal",
			code:    30,
			rawLog:  "block height: 4322, timeout height: 4321: tx timeout height",
			expired: true,
		},
		{
			// The sibling check of the SAME decorator, and the reason the
			// classifier keys on the code rather than on the word "timeout":
			// this one is our own broadcast deadline, and a resend may still
			// land, so it must NOT be terminal.
			name:    "timeout timestamp rejection is not terminal",
			code:    42,
			rawLog:  "block time: 2026-09-08T00:00:00Z, timeout timestamp: 2026-09-07T23:59:00Z: tx timeout",
			expired: false,
		},
		{
			name:    "unrelated rejection is not terminal",
			code:    18,
			rawLog:  "unordered nonce already used: invalid request",
			expired: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testServer, tc := newTimeoutHeightTestClient(t, supplierAddr)
			testServer.setBroadcastFailure(testCase.code, testCase.rawLog)

			claims := []*prooftypes.MsgCreateClaim{
				generateTestClaim(t, supplierAddr, "session-window-expired"),
			}
			_, err := tc.CreateClaims(context.Background(), supplierAddr, 4321, claims)
			require.Error(t, err, "a non-zero CheckTx code must surface as an error")

			require.Equal(t, testCase.expired, errors.Is(err, ErrTxWindowExpired),
				"classification of codespace=sdk code=%d", testCase.code)

			// The sentinel must not cost the datum. Both wrap, so a caller can
			// still ask WHAT the chain said after asking WHETHER it was this
			// condition -- the same contract ErrTxProofNotRequired carries.
			var rejection *TxRejection
			require.True(t, errors.As(err, &rejection),
				"the structured rejection must survive alongside the sentinel")
			require.Equal(t, testCase.code, rejection.ABCICode)
			require.Equal(t, "sdk", rejection.Codespace)
		})
	}
}
