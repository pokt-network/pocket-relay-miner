//go:build test

package tx

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSimulationPathIsExercisedWhenGasIsUnset asserts the mock RECORDED the
// call. That distinction is the test: passing GasLimit 0 is the precondition
// that makes simulation possible, not proof that it happened -- and if anyone
// breaks the wiring so Simulate stops being invoked, only an assertion on the
// server side goes red.
//
// It matters because gas_limit is commented out in config.miner.example.yaml,
// so production runs at zero and simulates, while every existing test sets
// GasLimit: 100000 and exercises the other mode. This path had zero coverage.
func TestSimulationPathIsExercisedWhenGasIsUnset(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)

	tc := newBudgetClient(t, srv, TxClientConfig{
		BlockTimeProvider: testBlockTime()}) // GasLimit unset => 0 => simulate

	before := srv.txServer.SimulateCalls()
	require.NoError(t, claim(tc, context.Background(), t))
	require.Greater(t, srv.txServer.SimulateCalls(), before,
		"the claim never simulated: with gas_limit unset the client must estimate, and this is the production default")
}

// TestSimulationFailureCarriesTheWholeWrappingChain pins what a simulation
// error LOOKS like by the time it reaches us, because that shape is what every
// classifier in the next commits will have to match.
//
// A mock returning the keeper's bare text would let a classifier look correct
// and match nothing in production: the real message is wrapped by baseapp,
// flattened by the tx service with a per-call gas number, and carried over
// gRPC. Exact equality against the registered description can therefore never
// match -- which is the defect this test exists to make visible.
func TestSimulationFailureCarriesTheWholeWrappingChain(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	srv.txServer.FailSimulation("proof not required", 0, true)

	tc := newBudgetClient(t, srv, TxClientConfig{
		BlockTimeProvider: testBlockTime()})

	err := claim(tc, context.Background(), t)
	require.Error(t, err)
	msg := err.Error()

	require.Contains(t, msg, "proof not required", "the server's own text must survive to us")
	require.Contains(t, msg, "message index: 0", "baseapp's wrap must survive: it is the only structured datum simulation leaves")
	require.Contains(t, msg, "with gas used:", "the tx service's flattening must be reproduced, gas suffix included")
	require.NotEqual(t, "proof not required", msg,
		"the mock returned the bare description: a classifier tested against this would match nothing in production")
}

// TestAnteHandlerFailureCarriesNoMessageIndex is the shape that makes a plain
// MsgIndex int dangerous.
//
// The ante decorators run in simulate too and fail BEFORE runMsgs, and
// "message index" is written only inside runMsgs. So a fee, nonce or TTL
// failure arrives with no index at all -- and a zero-valued MsgIndex would be
// indistinguishable from "message 0 failed", which is the datum that decides an
// irreversible batch degradation later.
func TestAnteHandlerFailureCarriesNoMessageIndex(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	srv.txServer.FailSimulation("insufficient fees", 0, false)

	tc := newBudgetClient(t, srv, TxClientConfig{
		BlockTimeProvider: testBlockTime()})

	err := claim(tc, context.Background(), t)
	require.Error(t, err)
	require.Contains(t, err.Error(), "insufficient fees")
	require.False(t, strings.Contains(err.Error(), "message index"),
		"an ante-handler failure carried a message index: the mock is not reproducing the pre-runMsgs shape")
}
