//go:build test

package tx

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// The two errors a real node produces, verbatim from the fork this repo builds
// against (rpc/core/tx.go): the second is the one whose text does NOT contain
// "not found", which is why the SDK does not map it to NotFound.
var (
	errTxNotFound    = status.Error(codes.NotFound, "tx not found: ABC")
	errIndexerOff    = errors.New("transaction indexing is disabled")
	errSomethingElse = errors.New("connection reset by peer")
)

func newInclusionClient(t *testing.T) (*TxClient, *testGRPCServer) {
	t.Helper()
	server := setupMockGRPCServer(t)
	t.Cleanup(server.cleanup)

	tc, err := NewTxClient(logging.NewLoggerFromConfig(logging.DefaultConfig()), nil, TxClientConfig{
		BlockTimeProvider: testBlockTime(),
		GRPCEndpoint:      server.address,
		ChainID:           "test",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })
	return tc, server
}

// TestReadTxInclusion_SeparatesTheThreeAnswers is C1 and C2 together, and it is
// the assertion the mock had to be extended to make possible: before that it
// could only ever answer "included, here is its response", so both of these
// cases were green by construction.
func TestReadTxInclusion_SeparatesTheThreeAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(s *testGRPCServer)
		want TxInclusion
	}{
		{
			name: "the index does not hold it",
			set:  func(s *testGRPCServer) { s.txServer.SetGetTxErr(errTxNotFound) },
			want: TxInclusionNotInBlock,
		},
		{
			name: "the node has no index at all",
			set:  func(s *testGRPCServer) { s.txServer.SetGetTxErr(errIndexerOff) },
			want: TxInclusionUnknown,
		},
		{
			name: "any other failure",
			set:  func(s *testGRPCServer) { s.txServer.SetGetTxErr(errSomethingElse) },
			want: TxInclusionUnknown,
		},
		{
			name: "included and its messages failed",
			set: func(s *testGRPCServer) {
				s.txServer.SetGetTxForHash("H", mockGetTxAnswer{code: 11, rawLog: "out of gas"})
			},
			want: TxInclusionIncludedFailed,
		},
		{
			name: "included and fine",
			set: func(s *testGRPCServer) {
				s.txServer.SetGetTxForHash("H", mockGetTxAnswer{code: 0})
			},
			want: TxInclusionIncludedOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newInclusionClient(t)
			tc.set(server)

			got, _ := client.ReadTxInclusion(context.Background(), "H")
			require.Equal(t, tc.want, got, "verdict was %s", got)
		})
	}
}

// TestReadTxInclusion_AnUninterpretableErrorIsNeverAbsence is C2 stated as the
// property rather than as a row, because this is the one that costs something
// when it is wrong.
//
// Absence is what would authorise a resend, so an answer nobody can read must
// never be turned into it. And the distinction it rests on today is an accident
// of wording upstream: the SDK maps its not-found by matching the substring
// "not found", and the node with no index happens to answer "transaction
// indexing is disabled". Classifying by the gRPC code instead of by that text is
// what keeps this true if the wording ever changes.
func TestReadTxInclusion_AnUninterpretableErrorIsNeverAbsence(t *testing.T) {
	for _, err := range []error{errIndexerOff, errSomethingElse, status.Error(codes.Unavailable, "node down")} {
		client, server := newInclusionClient(t)
		server.txServer.SetGetTxErr(err)

		got, _ := client.ReadTxInclusion(context.Background(), "H")
		require.NotEqual(t, TxInclusionNotInBlock, got,
			"%v was read as absence, which is the answer that authorises a resend", err)
		require.Equal(t, TxInclusionUnknown, got)
	}
}

// TestReadTxInclusion_ClassifiesByCodeAndNotByText is the property the whole
// design rests on, and it needs a case where the two ways of classifying
// DISAGREE — which the table above does not contain.
//
// Measured: replacing the code check with strings.Contains(err, "not found")
// left every other test in this file green, because the not-found error's own
// message contains that phrase and the indexer error does not. So the obvious
// cases cannot tell a code classifier from a text one.
//
// This one can. A transport failure whose message happens to contain "not
// found" is a plausible thing for a node or a proxy to say, and a text
// classifier turns it into absence — the single verdict that authorises a
// resend. Classifying by the gRPC code is what makes the answer independent of
// anyone's wording, upstream or in between.
func TestReadTxInclusion_ClassifiesByCodeAndNotByText(t *testing.T) {
	client, server := newInclusionClient(t)
	server.txServer.SetGetTxErr(status.Error(codes.Unavailable, "backend not found in pool"))

	got, _ := client.ReadTxInclusion(context.Background(), "H")
	require.Equal(t, TxInclusionUnknown, got,
		"an error carrying the words 'not found' is still not codes.NotFound, and reading it as absence would authorise a resend")
}

// TestReadInclusionForEntry_PrecedenceIsEvaluatedInOrder is C5.
//
// A precedence rule reads correctly and gets implemented backwards without
// anything complaining, so each row makes the two hashes DISAGREE and names
// which one must win.
func TestReadInclusionForEntry_PrecedenceIsEvaluatedInOrder(t *testing.T) {
	for _, tc := range []struct {
		name         string
		orig, resent mockGetTxAnswer
		want         TxInclusion
	}{
		{
			name:   "success on the resent hash beats failure on the original",
			orig:   mockGetTxAnswer{code: 11},
			resent: mockGetTxAnswer{code: 0},
			want:   TxInclusionIncludedOK,
		},
		{
			name:   "success on the original beats failure on the resent",
			orig:   mockGetTxAnswer{code: 0},
			resent: mockGetTxAnswer{code: 11},
			want:   TxInclusionIncludedOK,
		},
		{
			name:   "failure beats absence",
			orig:   mockGetTxAnswer{err: errTxNotFound},
			resent: mockGetTxAnswer{code: 11},
			want:   TxInclusionIncludedFailed,
		},
		{
			name:   "absence needs BOTH to say so",
			orig:   mockGetTxAnswer{err: errTxNotFound},
			resent: mockGetTxAnswer{err: errTxNotFound},
			want:   TxInclusionNotInBlock,
		},
		{
			name:   "one unreadable answer sinks an otherwise absent pair",
			orig:   mockGetTxAnswer{err: errTxNotFound},
			resent: mockGetTxAnswer{err: errIndexerOff},
			want:   TxInclusionUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newInclusionClient(t)
			server.txServer.SetGetTxForHash("orig", tc.orig)
			server.txServer.SetGetTxForHash("resent", tc.resent)

			got, _ := client.ReadInclusionForEntry(context.Background(), "orig", "resent")
			require.Equal(t, tc.want, got, "verdict was %s", got)
		})
	}
}

// TestProbeInclusionRead_ResolvesTheStateAtStartup is C3's producing half: the
// state must be knowable before any claim has been reconciled, so that an
// operator meets it when the process comes up rather than during the incident
// it explains.
func TestProbeInclusionRead_ResolvesTheStateAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want InclusionReadState
	}{
		{"a node with an index says not found", errTxNotFound, InclusionReadAvailable},
		// The name of this case used to say "not evidence either way" and the
		// assertion demanded unavailable -- the comment knew the answer and the
		// assertion asked for the other one. A transport failure says nothing
		// about how the node is configured.
		{"a transport failure is not evidence either way", status.Error(codes.Unavailable, "node down"), InclusionReadUnknown},
		{"a timeout is not evidence either way", status.Error(codes.DeadlineExceeded, "too slow"), InclusionReadUnknown},
		{"the node refusing for its own reason IS evidence", errIndexerOff, InclusionReadUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newInclusionClient(t)
			server.txServer.SetGetTxErr(tc.err)

			require.Equal(t, tc.want, client.ProbeInclusionRead(context.Background()))
		})
	}
}

// TestProbeInclusionRead_AZeroHashResolvingIsNotEvidence: a hash of zeroes is
// not something a real node holds, so an answer claiming it does says the node
// is not behaving as assumed. Reading that as "the read works" would be reading
// a broken assumption as a passing check.
func TestProbeInclusionRead_AZeroHashResolvingIsNotEvidence(t *testing.T) {
	client, server := newInclusionClient(t)
	server.txServer.SetGetTxForHash(zeroTxHash, mockGetTxAnswer{code: 0})

	require.Equal(t, InclusionReadUnknown, client.ProbeInclusionRead(context.Background()))
}
