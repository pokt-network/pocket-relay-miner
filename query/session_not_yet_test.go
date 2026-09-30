//go:build test

package query

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// A node stores a block before it commits the block's state, and poktroll
// answers a session at that height with this text (x/session/keeper
// session_hydrator.go), flattened into codes.Internal. Measured on 0.6% of CLI
// relays against one node on 2026-09-10.
const sessionHydrationNotYet = "block height 101 is ahead of the last committed block height 100: error during session hydration"

func sessionClientAnswering(t *testing.T, answer func(calls int64) (*sessiontypes.QueryGetSessionResponse, error)) (*Clients, *atomic.Int64) {
	t.Helper()
	_, address, cleanup, mock := setupMockQueryServer(t)
	t.Cleanup(cleanup)
	mock.sharedParams = generateTestSharedParams()
	var calls atomic.Int64
	mock.getSessionFunc = func(context.Context, *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		return answer(calls.Add(1))
	}
	qc, err := NewQueryClients(logging.NewLoggerFromConfig(logging.DefaultConfig()), ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = qc.Close() })
	return qc, &calls
}

// TestGetSession_RetriesANodeThatHasNotCommittedTheHeightYet: the not-yet
// answer is retried, and the session arrives.
func TestGetSession_RetriesANodeThatHasNotCommittedTheHeightYet(t *testing.T) {
	want := generateTestSession("pokt1app", "develop", 101)
	qc, calls := sessionClientAnswering(t, func(n int64) (*sessiontypes.QueryGetSessionResponse, error) {
		if n == 1 {
			return nil, status.Error(codes.Internal, sessionHydrationNotYet)
		}
		return &sessiontypes.QueryGetSessionResponse{Session: want}, nil
	})

	got, err := qc.Session().GetSession(context.Background(), "pokt1app", "develop", 101)
	require.NoError(t, err)
	require.Equal(t, want.SessionId, got.SessionId)
	require.Equal(t, int64(2), calls.Load(), "one not-yet answer, then the session")
}

// TestGetSession_GivesUpAfterTwoRetries: a node that keeps answering "not yet"
// costs the relay two retries, then the error is returned and the relay is
// refused; a far-future height answers the same text forever.
func TestGetSession_GivesUpAfterTwoRetries(t *testing.T) {
	qc, calls := sessionClientAnswering(t, func(int64) (*sessiontypes.QueryGetSessionResponse, error) {
		return nil, status.Error(codes.Internal, sessionHydrationNotYet)
	})

	_, err := qc.Session().GetSession(context.Background(), "pokt1app", "develop", 101)
	require.True(t, IsHeightNotYetAvailable(err), "the node's answer is returned: %v", err)
	require.Equal(t, int64(3), calls.Load(), "the first call and two retries")
}

// TestGetSession_DoesNotRetryAnyOtherInternalError: codes.Internal alone is not
// the signal; an internal error with other text is returned on the first call.
func TestGetSession_DoesNotRetryAnyOtherInternalError(t *testing.T) {
	qc, calls := sessionClientAnswering(t, func(int64) (*sessiontypes.QueryGetSessionResponse, error) {
		return nil, status.Error(codes.Internal, "failed to hydrate session: store closed")
	})

	_, err := qc.Session().GetSession(context.Background(), "pokt1app", "develop", 101)
	require.Error(t, err)
	require.Equal(t, int64(1), calls.Load())
}

// TestGetSession_ARetryDoesNotHoldOtherSessionsBehindIt: the not-yet retry
// sleeps outside the session cache's lock. The waiting session's node answers
// "not yet" until the OTHER session's lookup has completed. The other lookup
// has a deadline far below the query timeout: a retry held inside the lock
// keeps it waiting past that deadline, and it fails.
func TestGetSession_ARetryDoesNotHoldOtherSessionsBehindIt(t *testing.T) {
	waiting := generateTestSession("pokt1slow", "develop", 101)
	other := generateTestSession("pokt1fast", "develop", 101)
	firstSlowCall := make(chan struct{})
	otherDone := make(chan struct{})
	var slowCalls atomic.Int64
	_, address, cleanup, mock := setupMockQueryServer(t)
	t.Cleanup(cleanup)
	mock.sharedParams = generateTestSharedParams()
	mock.getSessionFunc = func(_ context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		if req.ApplicationAddress != "pokt1slow" {
			return &sessiontypes.QueryGetSessionResponse{Session: other}, nil
		}
		if slowCalls.Add(1) == 1 {
			close(firstSlowCall)
		}
		select {
		case <-otherDone:
			return &sessiontypes.QueryGetSessionResponse{Session: waiting}, nil
		default:
			return nil, status.Error(codes.Internal, sessionHydrationNotYet)
		}
	}
	clients, err := NewQueryClients(logging.NewLoggerFromConfig(logging.DefaultConfig()), ClientConfig{GRPCEndpoint: address, QueryTimeout: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clients.Close() })

	slowDone := make(chan error, 1)
	go func() {
		_, slowErr := clients.Session().GetSession(context.Background(), "pokt1slow", "develop", 101)
		slowDone <- slowErr
	}()
	<-firstSlowCall

	otherCtx, cancelOther := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelOther()
	got, err := clients.Session().GetSession(otherCtx, "pokt1fast", "develop", 101)
	require.NoError(t, err)
	require.Equal(t, other.SessionId, got.SessionId)
	close(otherDone)
	require.NoError(t, <-slowDone, "the waiting session must get its answer once the other lookup, not held behind it, is done")
}
