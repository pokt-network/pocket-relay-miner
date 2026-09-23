//go:build test

package tx

import (
	"context"
	"testing"
	"time"

	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// The deadline regime is counted once per transaction SIGNED. It used to be
// counted per derivation by the miner, and per resend before the resend knew
// whether it would sign: a resend that re-injected bytes the node already held
// counted a regime with no transaction behind it, so regimes and broadcasts
// drifted apart in the live gate (1384 against 1304, 2026-09-23).

func regimeCount(phase, regime string) float64 {
	return testutil.ToFloat64(txTimeoutRegimeTotal.WithLabelValues(phase, regime))
}

func regimeTotal(t *testing.T) float64 {
	t.Helper()
	n := 0.0
	for _, phase := range []string{"claim", "proof"} {
		for _, regime := range []string{TimeoutRegimeWindow, TimeoutRegimeCeiling, TimeoutRegimeUnknown} {
			n += regimeCount(phase, regime)
		}
	}
	return n
}

func newRegimeTestClient(t *testing.T, supplierAddr string) *TxClient {
	t.Helper()
	testServer := setupMockGRPCServer(t)
	t.Cleanup(testServer.cleanup)
	testServer.addAccount(supplierAddr, 1, 0)
	km := setupTestKeyManager(t, supplierAddr)
	t.Cleanup(func() { _ = km.Close() })
	tc, err := NewTxClient(logging.NewLoggerFromConfig(logging.DefaultConfig()), km, TxClientConfig{
		BlockTimeProvider: testBlockTime(),
		GRPCEndpoint:      testServer.address,
		ChainID:           "test-chain",
		GasLimit:          100000,
		GasPrice:          parseGasPrice(t, "0.001upokt"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })
	return tc
}

func TestTimeoutRegime_CountedOncePerSignedTransaction(t *testing.T) {
	const supplier = "pokt1regime_signed"
	tc := newRegimeTestClient(t, supplier)
	ctx := WithTxWindowTimeout(context.Background(), 90*time.Second, TimeoutRegimeWindow)

	beforeClaim, beforeProof := regimeCount("claim", TimeoutRegimeWindow), regimeCount("proof", TimeoutRegimeWindow)
	beforeAll := regimeTotal(t)

	_, _, err := tc.CreateClaims(ctx, supplier, 1000, []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplier, "session-1"),
		generateTestClaim(t, supplier, "session-2"),
	})
	require.NoError(t, err)

	require.Equal(t, beforeClaim+1, regimeCount("claim", TimeoutRegimeWindow), "one transaction signed, two claims in it: one regime")
	require.Equal(t, beforeProof, regimeCount("proof", TimeoutRegimeWindow), "the phase is the transaction's type")
	require.Equal(t, beforeAll+1, regimeTotal(t), "nothing else was counted")
}

func TestTimeoutRegime_ACallerWithNoBudgetIsCountedUnknown(t *testing.T) {
	const supplier = "pokt1regime_unknown"
	tc := newRegimeTestClient(t, supplier)
	before := regimeCount("claim", TimeoutRegimeUnknown)

	_, _, err := tc.CreateClaims(context.Background(), supplier, 1000, []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplier, "session-1"),
	})
	require.NoError(t, err)

	require.Equal(t, before+1, regimeCount("claim", TimeoutRegimeUnknown),
		"a path that skips the derivation must show as unknown: that is what the live gate reads")
}

func TestTimeoutRegime_AReinjectionCountsNothing(t *testing.T) {
	const supplier = "pokt1regime_reinject"
	tc := newRegimeTestClient(t, supplier)
	ctx := WithTxWindowTimeout(context.Background(), 90*time.Second, TimeoutRegimeWindow)

	_, signed, err := tc.CreateClaims(ctx, supplier, 1000, []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplier, "session-1"),
	})
	require.NoError(t, err)
	require.NotEmpty(t, signed.Bytes, "premise: the signing path hands back its bytes")
	before := regimeTotal(t)

	_, err = tc.BroadcastRawReturningHash(ctx, supplier, "claim", signed)
	require.NoError(t, err)

	require.Equal(t, before, regimeTotal(t), "the same bytes again: no transaction was signed, no regime counted")
}
