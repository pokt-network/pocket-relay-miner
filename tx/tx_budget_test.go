//go:build test

package tx

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

const budgetTestSupplier = "pokt1supplier123"

// claim submits one claim and reports the error, for tests that only care
// whether the call reached the chain.
func claim(tc *TxClient, ctx context.Context, t *testing.T) error {
	t.Helper()
	_, _, err := tc.CreateClaims(ctx, budgetTestSupplier, 1000,
		[]*prooftypes.MsgCreateClaim{generateTestClaim(t, budgetTestSupplier, "session-1")})
	return err
}

// newBudgetClient builds a client owning its connection, which is the
// production shape, with the per-attempt cap under the test's control.
func newBudgetClient(t *testing.T, srv *testGRPCServer, cfg TxClientConfig) *TxClient {
	t.Helper()

	srv.addAccount(budgetTestSupplier, 1, 0)
	km := setupTestKeyManager(t, budgetTestSupplier)
	t.Cleanup(func() { _ = km.Close() })

	cfg.GRPCEndpoint = srv.address
	cfg.ChainID = "test-chain"
	cfg.GasPrice = parseGasPrice(t, "0.001upokt")
	// GasLimit is NOT defaulted here on purpose: zero means "simulate", which
	// is what production does, and a helper that quietly set it would force
	// every caller into the mode production does not use.
	cfg.ConnProbeInterval = time.Hour

	tc, err := NewTxClient(logging.NewLoggerFromConfig(logging.DefaultConfig()), km, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })
	return tc
}

// TestDeadlineCoversTheFirstNetworkCall is written against getAccount rather
// than BroadcastTx on purpose.
//
// The deadline used to be applied 23 lines BELOW the account lookup, so a test
// that hung the broadcast would pass with the defect fully present. The account
// lookup is the first RPC of the signing path and its cache is cold exactly
// after a restart or a rebalance -- the case the deadline exists for.
func TestDeadlineCoversTheFirstNetworkCall(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	release := srv.authServer.BlockAccount()
	defer close(release)

	tc := newBudgetClient(t, srv, TxClientConfig{TxRPCTimeout: 300 * time.Millisecond})

	// Off to the side so a MISSING deadline fails by naming the defect instead
	// of hanging the package for ten minutes with nothing in the output.
	result := make(chan error, 1)
	go func() { result <- claim(tc, context.Background(), t) }()

	select {
	case err := <-result:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the account lookup outlived every deadline: the clock is applied after the first network call, not before it")
	}
}

// TestRetriesShareTheWindowBudget: the caller builds ONE context and reuses it
// across retry attempts (miner/lifecycle_callback.go). A per-attempt
// WithTimeout hands each retry a fresh full budget, so N attempts can spend N
// windows' worth of a window that lasts one.
//
// The assertion is whether the second attempt REACHES THE SERVER at all: with a
// shared absolute budget the context is already expired, so the RPC never
// leaves the process; with a per-attempt budget it starts a new one and
// arrives. That is a difference the two mechanisms cannot both produce.
func TestRetriesShareTheWindowBudget(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	release := srv.authServer.BlockAccount()
	defer close(release)
	seen := srv.authServer.NotifyAccount(8)

	tc := newBudgetClient(t, srv, TxClientConfig{
		TxRPCTimeout: time.Hour, // the window has to be the binding cap
	})

	// One context, reused -- exactly what the lifecycle does across retries.
	ctx := WithTxWindowTimeout(context.Background(), 200*time.Millisecond, TimeoutRegimeWindow)

	require.Error(t, claim(tc, ctx, t), "the first attempt should have run out of window")
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the first attempt never reached the server")
	}
	arrivals := srv.authServer.AccountCalls()

	require.Error(t, claim(tc, ctx, t), "the second attempt should have run out of window too")
	require.Equal(t, arrivals, srv.authServer.AccountCalls(),
		"the second attempt reached the server: each retry is starting a fresh budget instead of sharing the window's")
}
