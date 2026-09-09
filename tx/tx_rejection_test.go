//go:build test

package tx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// The literal the mock produces for a failed simulation, spelled out here on
// purpose. Sharing a constant with the mock would let both move together and
// the byte-identity assertions would stop noticing.
const simDesc = "failed to execute message; message index: 0: proof not required with gas used: '42000'"

// ---------------------------------------------------------------------------
// C1-lit -- Error() is a pure function of the fields.
//
// The rejections are built BY LITERAL, bypassing the constructors. That is the
// only thing that separates "derive on call" from "compute in the constructor
// and store": the value is immutable, so through the constructor the two are
// observationally identical. An implementation that stores the message leaves
// the hidden field empty here and comes out wrong.
// ---------------------------------------------------------------------------

func TestErrorIsAPureFunctionOfTheFields(t *testing.T) {
	cause := status.Error(codes.Unknown, simDesc)

	tests := []struct {
		name string
		rej  *TxRejection
		want string
	}{
		{
			name: "simulate",
			rej:  &TxRejection{Stage: TxStageSimulate, RawLog: simDesc, wrapped: cause},
			want: "simulation failed: rpc error: code = Unknown desc = " + simDesc,
		},
		{
			name: "checktx",
			rej:  &TxRejection{Stage: TxStageCheckTx, ABCICode: 11, RawLog: "out of gas"},
			want: "CheckTx failed (code 11): out of gas",
		},
		{
			name: "broadcast",
			rej: &TxRejection{
				Stage:   TxStageBroadcast,
				RawLog:  "node down",
				wrapped: status.Error(codes.Unavailable, "node down"),
			},
			want: "failed to broadcast transaction: rpc error: code = Unavailable desc = node down",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.rej.Error(),
				"Error() must be derivable from the fields alone: a literal never went through a constructor")
		})
	}
}

// C2 -- checktx with an EMPTY RawLog and a real code. There is a producer:
// cosmos-sdk's CheckCometError returns TxResponse{Code, Codespace, TxHash} with
// no RawLog for codes 19/20/21. The trailing space is part of today's message
// and is the first thing a "cleanup" removes.
func TestCheckTxWithEmptyRawLogKeepsTheTrailingSpace(t *testing.T) {
	rej := &TxRejection{Stage: TxStageCheckTx, ABCICode: 19, Codespace: "sdk"}
	require.Equal(t, "CheckTx failed (code 19): ", rej.Error())
}

// C4 -- Error() is total. A switch without a default returns "" for a zero
// Stage, which is an error that reads as success; and a stage whose format
// needs the wrapped error must not print a bare prefix, which is an error that
// reads as complete.
func TestErrorIsTotal(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		got := (&TxRejection{}).Error()
		require.NotEmpty(t, got, "a zero rejection must not render as an empty error")
		require.Contains(t, got, "unknown stage")
	})

	t.Run("stage that is not one of the three", func(t *testing.T) {
		got := (&TxRejection{Stage: TxStage("nope"), RawLog: "x"}).Error()
		require.NotEmpty(t, got)
		require.Contains(t, got, `"nope"`, "the invalid stage must be named, or the log cannot say what happened")
	})

	t.Run("nil receiver", func(t *testing.T) {
		// Not hypothetical for the same reason the default branch is not: the
		// fields are exported so errors.As can read them, which makes the type
		// constructible anywhere, nil pointer included. Error() is called from
		// every %v in a log line, so a panic here happens inside logging.
		var nilRej *TxRejection
		var asError error = nilRej
		require.NotPanics(t, func() { _ = asError.Error() })
		require.NotEmpty(t, asError.Error())
		require.NoError(t, errors.Unwrap(asError))
	})

	t.Run("nil cause in both stages that use one", func(t *testing.T) {
		require.Equal(t, "simulation failed: <nil cause>",
			(&TxRejection{Stage: TxStageSimulate}).Error())
		require.Equal(t, "failed to broadcast transaction: <nil cause>",
			(&TxRejection{Stage: TxStageBroadcast}).Error())
	})
}

// C5 -- the index pair, in SIMULATE, which is the only stage that can carry one.
//
// The two halves are asserted together on purpose: false/0 is the struct's zero
// value, so an implementation with no parser at all passes the top half. And the
// index is 12, not 1: a single-digit parser passes with 1 and with 2 and
// truncates 12 in silence.
func TestMsgIndexPairDistinguishesAbsenceFromZero(t *testing.T) {
	t.Run("ante-shaped failure carries no index", func(t *testing.T) {
		rej := newSimulateRejection(status.Error(codes.Unknown,
			"insufficient fees; got: 1upokt required: 10upokt with gas used: '42000'"))
		require.False(t, rej.HasMsgIndex, "a fee failure fails before any message executes")
		require.Equal(t, 0, rej.MsgIndex)
	})

	t.Run("execution failure carries a two-digit index", func(t *testing.T) {
		rej := newSimulateRejection(status.Error(codes.Unknown,
			"failed to execute message; message index: 12: proof not required with gas used: '42000'"))
		require.True(t, rej.HasMsgIndex)
		require.Equal(t, 12, rej.MsgIndex, "a one-byte parser truncates this to 1 and stays green on 1 and 2")
	})

	t.Run("the FIRST match wins when the text carries a second one", func(t *testing.T) {
		// errorsmod.Wrapf renders as "<wrapper>: <cause>", so cosmos's index is
		// always left of anything the server's own text contains. Reading the
		// last match takes a number out of content, and an irreversible
		// per-message downgrade hangs off it. Nothing else pins this choice:
		// with strings.LastIndex every other test in this file stays green.
		idx, has := parseMsgIndex(
			"failed to execute message; message index: 3: the handler said message index: 7 is bad with gas used: '42000'")
		require.True(t, has)
		require.Equal(t, 3, idx, "the last match reads the server's content, not the index cosmos wrote")
	})

	t.Run("the events variant parses too", func(t *testing.T) {
		// Pure parser test, and it says so: the mock hardcodes only the
		// "failed to execute message" variant, so this does not go over the
		// wire. The variant is defensive rather than live.
		idx, has := parseMsgIndex("failed to create message events; message index: 2: boom")
		require.True(t, has, "anchoring on the execute-prefix drops this one silently")
		require.Equal(t, 2, idx)
	})
}

// C6 -- the impossibility is asserted, not commented. CheckTx stops before
// executing messages, so an index appearing in one of these RawLogs came from
// somewhere else. If anyone "unifies" the parser across the three stages, this
// goes red.
func TestOnlySimulateParsesAnIndex(t *testing.T) {
	const poisoned = "something with message index: 3 inside it"

	checktx := newCheckTxRejection(&cosmostypes.TxResponse{Code: 11, RawLog: poisoned})
	require.False(t, checktx.HasMsgIndex, "CheckTx does not execute messages: an index here is not ours to believe")

	broadcast := newBroadcastRejection(status.Error(codes.Unavailable, poisoned), []byte("tx"))
	require.False(t, broadcast.HasMsgIndex, "a transport failure executed nothing")
}

// C7 -- RawLog is the SERVER's text, captured at the call site. Asserted
// exactly, not with Contains: the correct value and the wrong one (the wrapped
// "rpc error: code = ..." form) are BOTH substrings of Error(), so a Contains
// assertion is green with either.
func TestRawLogIsTheServerTextNotTheWrappedForm(t *testing.T) {
	t.Run("simulate", func(t *testing.T) {
		rej := newSimulateRejection(status.Error(codes.Unknown, simDesc))
		require.Equal(t, simDesc, rej.RawLog)
	})

	t.Run("broadcast", func(t *testing.T) {
		rej := newBroadcastRejection(status.Error(codes.Unavailable, "node down"), []byte("tx"))
		require.Equal(t, "node down", rej.RawLog)
	})
}

// C8c -- the broadcast hash is recovered, and it is the one the node would have
// reported: uppercase hex of the FULL sha256 of the same bytes. The expected
// value is computed here with the standard library so it shares no function
// with production, where tmhash.SumTruncated is the 20-byte neighbour.
func TestBroadcastHashIsTheFullUppercaseSha256(t *testing.T) {
	txBytes := []byte("the exact bytes we signed")
	sum := sha256.Sum256(txBytes)
	want := strings.ToUpper(hex.EncodeToString(sum[:]))

	rej := newBroadcastRejection(status.Error(codes.Unavailable, "node down"), txBytes)
	require.Equal(t, want, rej.TxHash)
	require.Len(t, rej.TxHash, 64, "a truncated hash is 40 characters and looks plausible")
}

// C9 -- the two numbering systems stay in two differently typed fields, and
// each stage populates the one it actually has.
//
// The checktx half does NOT discriminate and that is written down: codes.OK is
// zero, so "we never touched it" and "we set it to OK deliberately" read the
// same. It is benign because zero is the true statement there. The broadcast
// half is the one that discriminates.
func TestTheTwoNumberingsStayApart(t *testing.T) {
	sim := newSimulateRejection(status.Error(codes.Unknown, simDesc))
	require.Equal(t, codes.Unknown, sim.GRPCCode)
	require.Zero(t, sim.ABCICode, "simulate has no ABCI code: the tx service flattened it away")
	require.Empty(t, sim.Codespace)

	check := newCheckTxRejection(&cosmostypes.TxResponse{Code: 11, Codespace: "sdk", RawLog: "out of gas"})
	require.EqualValues(t, 11, check.ABCICode)
	require.Equal(t, "sdk", check.Codespace, "the codespace used to be thrown away here")
	require.Equal(t, codes.OK, check.GRPCCode)

	bcast := newBroadcastRejection(status.Error(codes.DeadlineExceeded, "too slow"), []byte("tx"))
	require.Equal(t, codes.DeadlineExceeded, bcast.GRPCCode,
		"this is the stage where the transport code is the only thing that says nobody rejected anything")
	require.Zero(t, bcast.ABCICode)
}

// ---------------------------------------------------------------------------
// C1-path and C10 -- through the REAL path, from the external caller.
//
// C1-lit pins the format GIVEN the fields; these pin that the construction
// sites POPULATE those fields so the message a caller sees is still today's.
// The two injections are different: storing the string kills C1-lit, populating
// a field wrongly kills these.
//
// Both the outer string and the recovered rejection are asserted, so a red is
// attributable: moving a wrapper's text reddens only the outer assertion, while
// moving the rejection's format reddens both. Without that, someone who changes
// a wrapper legitimately edits the expected string and swallows any rejection
// change inside it.
// ---------------------------------------------------------------------------

func TestRealPathKeepsTodaysMessageAndCarriesTheRejection(t *testing.T) {
	tests := []struct {
		name      string
		arm       func(*testGRPCServer)
		wantStage TxStage
		wantInner string
		wantOuter string
	}{
		{
			name:      "simulate",
			arm:       func(s *testGRPCServer) { s.txServer.FailSimulation("proof not required", 0, true) },
			wantStage: TxStageSimulate,
			wantInner: "simulation failed: rpc error: code = Unknown desc = " + simDesc,
			wantOuter: "failed to broadcast claims: gas simulation failed (gas_limit=0 requires successful simulation): " +
				"simulation failed: rpc error: code = Unknown desc = " + simDesc,
		},
		{
			name:      "checktx",
			arm:       func(s *testGRPCServer) { s.setBroadcastFailure(11, "out of gas") },
			wantStage: TxStageCheckTx,
			wantInner: "CheckTx failed (code 11): out of gas",
			wantOuter: "failed to broadcast claims: CheckTx failed (code 11): out of gas",
		},
		{
			name:      "broadcast",
			arm:       func(s *testGRPCServer) { s.setBroadcastError(status.Error(codes.Unavailable, "node down")) },
			wantStage: TxStageBroadcast,
			wantInner: "failed to broadcast transaction: rpc error: code = Unavailable desc = node down",
			wantOuter: "failed to broadcast claims: failed to broadcast transaction: " +
				"rpc error: code = Unavailable desc = node down",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := setupMockGRPCServer(t)
			t.Cleanup(srv.cleanup)
			tt.arm(srv)

			tc := newBudgetClient(t, srv, TxClientConfig{})
			err := claim(tc, context.Background(), t)
			require.Error(t, err)

			require.Equal(t, tt.wantOuter, err.Error(),
				"the message a caller sees must be byte-identical to the one before TxRejection existed")

			var rej *TxRejection
			require.True(t, errors.As(err, &rej),
				"the rejection must survive every wrapper between the construction site and the caller")
			require.Equal(t, tt.wantStage, rej.Stage)
			require.Equal(t, tt.wantInner, rej.Error(),
				"asserted separately so a wrapper change and a format change do not produce the same red")
		})
	}
}

// C10b through the OTHER external wrapper. Claims are wrapped at one site and
// proofs at another; a criterion standing on only one of them leaves the other
// path without the guarantee.
func TestTheRejectionSurvivesTheProofWrapperToo(t *testing.T) {
	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	srv.setBroadcastFailure(11, "out of gas")

	tc := newBudgetClient(t, srv, TxClientConfig{})
	_, _, err := tc.SubmitProofs(context.Background(), budgetTestSupplier, 1000,
		[]*prooftypes.MsgSubmitProof{generateTestProof(t, budgetTestSupplier, "session-1")})
	require.Error(t, err)

	require.Equal(t, "failed to broadcast proofs: CheckTx failed (code 11): out of gas", err.Error())

	var rej *TxRejection
	require.True(t, errors.As(err, &rej))
	require.Equal(t, TxStageCheckTx, rej.Stage)
}

// C8a/C8b -- the hash by stage, over the wire. Simulate has none because the
// simulated bytes are re-signed before broadcast, so a hash of them would name
// a transaction that never existed.
func TestHashByStageOverTheWire(t *testing.T) {
	t.Run("simulate has no hash", func(t *testing.T) {
		srv := setupMockGRPCServer(t)
		t.Cleanup(srv.cleanup)
		srv.txServer.FailSimulation("proof not required", 0, true)

		tc := newBudgetClient(t, srv, TxClientConfig{})
		var rej *TxRejection
		require.True(t, errors.As(claim(tc, context.Background(), t), &rej))
		require.Empty(t, rej.TxHash, "the simulated bytes are not the transaction: the tx is re-signed after simulating")
	})

	t.Run("checktx reports the server's hash", func(t *testing.T) {
		srv := setupMockGRPCServer(t)
		t.Cleanup(srv.cleanup)
		srv.setBroadcastFailure(11, "out of gas")

		tc := newBudgetClient(t, srv, TxClientConfig{})
		var rej *TxRejection
		require.True(t, errors.As(claim(tc, context.Background(), t), &rej))
		require.Equal(t, "test-hash-1", rej.TxHash)
	})

	t.Run("broadcast recovers the hash of the bytes the server received", func(t *testing.T) {
		srv := setupMockGRPCServer(t)
		t.Cleanup(srv.cleanup)
		srv.setBroadcastError(status.Error(codes.Unavailable, "node down"))

		tc := newBudgetClient(t, srv, TxClientConfig{})
		var rej *TxRejection
		require.True(t, errors.As(claim(tc, context.Background(), t), &rej))

		sum := sha256.Sum256(srv.getLastTxBytes())
		require.Equal(t, strings.ToUpper(hex.EncodeToString(sum[:])), rej.TxHash,
			fmt.Sprintf("the recovered hash must be the one the node would report for these %d bytes",
				len(srv.getLastTxBytes())))
	})
}
