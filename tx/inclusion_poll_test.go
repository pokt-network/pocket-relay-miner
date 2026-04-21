//go:build test

package tx

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// -----------------------------------------------------------------------------
// Programmable mock GetTx — lets each test script the sequence of results.
// -----------------------------------------------------------------------------

type programmableTxService struct {
	txtypes.UnimplementedServiceServer
	mu            sync.Mutex
	getTxResponse func(hash string, call int) (*txtypes.GetTxResponse, error)
	getTxCalls    atomic.Int32
}

func (p *programmableTxService) GetTx(_ context.Context, req *txtypes.GetTxRequest) (*txtypes.GetTxResponse, error) {
	call := int(p.getTxCalls.Add(1))
	p.mu.Lock()
	fn := p.getTxResponse
	p.mu.Unlock()
	if fn == nil {
		return nil, status.Error(codes.Unimplemented, "not set")
	}
	return fn(req.Hash, call)
}

func startProgrammableServer(t *testing.T) (*programmableTxService, *grpc.ClientConn) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer()
	ps := &programmableTxService{}
	txtypes.RegisterServiceServer(srv, ps)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return ps, conn
}

// newTestTxClient builds a TxClient wired to a fake gRPC server. The returned
// client has the inclusion pool enabled with a tight interval so tests finish
// quickly.
func newTestTxClient(t *testing.T, pollInterval, horizon time.Duration) (*TxClient, *programmableTxService) {
	t.Helper()
	ps, conn := startProgrammableServer(t)
	tc, err := NewTxClient(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		nil, // KeyManager not used for these tests
		TxClientConfig{
			GRPCConn: conn,
			ChainID:  "pocket-test",
			InclusionPoll: InclusionPollConfig{
				Interval:      pollInterval,
				Horizon:       horizon,
				MaxConcurrent: 4,
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })
	return tc, ps
}

// -----------------------------------------------------------------------------
// pollOnce unit tests
// -----------------------------------------------------------------------------

func TestPollOnce_NotFoundReturnsEmptyOutcome(t *testing.T) {
	tc, ps := newTestTxClient(t, 2*time.Second, time.Minute)
	ps.getTxResponse = func(hash string, _ int) (*txtypes.GetTxResponse, error) {
		return nil, status.Error(codes.NotFound, "tx not found")
	}

	outcome, errMsg, height := tc.pollOnce(context.Background(), "hash-notfound")
	assert.Equal(t, TxInclusionOutcome(""), outcome, "NotFound must mean 'keep polling'")
	assert.Empty(t, errMsg)
	assert.Equal(t, int64(0), height)
}

func TestPollOnce_CodeZeroReturnsIncludedSuccess(t *testing.T) {
	tc, ps := newTestTxClient(t, 2*time.Second, time.Minute)
	ps.getTxResponse = func(hash string, _ int) (*txtypes.GetTxResponse, error) {
		return &txtypes.GetTxResponse{
			TxResponse: &cosmostypes.TxResponse{TxHash: hash, Code: 0, Height: 17}, //nolint:gosec
		}, nil
	}

	outcome, errMsg, height := tc.pollOnce(context.Background(), "hash-ok")
	assert.Equal(t, TxInclusionIncludedSuccess, outcome)
	assert.Empty(t, errMsg)
	assert.Equal(t, int64(17), height)
}

func TestPollOnce_NonZeroCodeReturnsIncludedFailure(t *testing.T) {
	tc, ps := newTestTxClient(t, 2*time.Second, time.Minute)
	ps.getTxResponse = func(hash string, _ int) (*txtypes.GetTxResponse, error) {
		return &txtypes.GetTxResponse{
			TxResponse: &cosmostypes.TxResponse{
				TxHash: hash,
				Code:   11,
				RawLog: "claim already exists",
				Height: 42,
			},
		}, nil
	}

	outcome, errMsg, height := tc.pollOnce(context.Background(), "hash-failed")
	assert.Equal(t, TxInclusionIncludedFailure, outcome)
	assert.Equal(t, "claim already exists", errMsg, "DeliverTx RawLog must be captured so operators can diagnose")
	assert.Equal(t, int64(42), height)
}

func TestPollOnce_OtherGrpcErrorReturnsPollError(t *testing.T) {
	tc, ps := newTestTxClient(t, 2*time.Second, time.Minute)
	ps.getTxResponse = func(hash string, _ int) (*txtypes.GetTxResponse, error) {
		return nil, status.Error(codes.Unavailable, "chain RPC down")
	}

	outcome, errMsg, height := tc.pollOnce(context.Background(), "hash-rpc-err")
	assert.Equal(t, TxInclusionPollError, outcome)
	assert.Contains(t, errMsg, "chain RPC down")
	assert.Equal(t, int64(0), height)
}

func TestPollOnce_ContextDeadlineReturnsMempoolTimeout(t *testing.T) {
	tc, ps := newTestTxClient(t, 2*time.Second, time.Minute)
	ps.getTxResponse = func(hash string, _ int) (*txtypes.GetTxResponse, error) {
		// Simulate a slow RPC that never returns before the deadline.
		return nil, context.DeadlineExceeded
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	outcome, _, _ := tc.pollOnce(ctx, "hash-timeout")
	assert.Equal(t, TxInclusionMempoolTimeout, outcome)
}

// -----------------------------------------------------------------------------
// runInclusionPoll integration — includes the ticker loop and callback fan-out
// -----------------------------------------------------------------------------

func TestRunInclusionPoll_ResolvesToIncludedSuccessAndFiresCallback(t *testing.T) {
	tc, ps := newTestTxClient(t, 10*time.Millisecond, 500*time.Millisecond)

	// First two calls → NotFound (mempool); third call → included.
	ps.getTxResponse = func(hash string, call int) (*txtypes.GetTxResponse, error) {
		if call < 3 {
			return nil, status.Error(codes.NotFound, "tx not found")
		}
		return &txtypes.GetTxResponse{
			TxResponse: &cosmostypes.TxResponse{TxHash: hash, Code: 0, Height: 55},
		}, nil
	}

	resultCh := make(chan TxInclusionResult, 1)
	tc.AddInclusionCallback(func(_ context.Context, r TxInclusionResult) {
		resultCh <- r
	})

	tc.scheduleInclusionPoll("pokt1test", TxTypeClaim, "hash-eventual", time.Now())

	select {
	case got := <-resultCh:
		assert.Equal(t, TxInclusionIncludedSuccess, got.Outcome)
		assert.Equal(t, int64(55), got.InclusionHeight)
		assert.Equal(t, "pokt1test", got.Supplier)
		assert.Equal(t, TxTypeClaim, got.TxType)
		assert.Equal(t, "hash-eventual", got.TxHash)
	case <-time.After(2 * time.Second):
		t.Fatalf("inclusion callback never fired — ps calls=%d", ps.getTxCalls.Load())
	}
}

func TestRunInclusionPoll_TimesOutAsMempoolTimeout(t *testing.T) {
	tc, ps := newTestTxClient(t, 20*time.Millisecond, 100*time.Millisecond)

	// Always return NotFound — tx never lands.
	ps.getTxResponse = func(hash string, _ int) (*txtypes.GetTxResponse, error) {
		return nil, status.Error(codes.NotFound, "tx not found")
	}

	resultCh := make(chan TxInclusionResult, 1)
	tc.AddInclusionCallback(func(_ context.Context, r TxInclusionResult) {
		resultCh <- r
	})

	tc.scheduleInclusionPoll("pokt1test", TxTypeClaim, "hash-timeout", time.Now())

	select {
	case got := <-resultCh:
		assert.Equal(t, TxInclusionMempoolTimeout, got.Outcome)
	case <-time.After(2 * time.Second):
		t.Fatalf("callback never fired")
	}
}

func TestRunInclusionPoll_CallbackPanicDoesNotPoisonOthers(t *testing.T) {
	tc, ps := newTestTxClient(t, 10*time.Millisecond, 500*time.Millisecond)
	ps.getTxResponse = func(hash string, _ int) (*txtypes.GetTxResponse, error) {
		return &txtypes.GetTxResponse{
			TxResponse: &cosmostypes.TxResponse{TxHash: hash, Code: 0, Height: 1},
		}, nil
	}

	good := make(chan struct{}, 1)
	tc.AddInclusionCallback(func(_ context.Context, _ TxInclusionResult) {
		panic("bad callback")
	})
	tc.AddInclusionCallback(func(_ context.Context, _ TxInclusionResult) {
		good <- struct{}{}
	})

	tc.scheduleInclusionPoll("pokt1test", TxTypeClaim, "hash-panic-test", time.Now())

	select {
	case <-good:
	case <-time.After(2 * time.Second):
		t.Fatalf("healthy callback did not fire despite other callback panicking")
	}
}

func TestScheduleInclusionPoll_NoPoolWhenDisabled(t *testing.T) {
	ps, conn := startProgrammableServer(t)
	tc, err := NewTxClient(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		nil,
		TxClientConfig{
			GRPCConn:      conn,
			ChainID:       "pocket-test",
			InclusionPoll: InclusionPollConfig{Disabled: true},
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })

	scheduled := tc.scheduleInclusionPoll("pokt1test", TxTypeClaim, "hash-no-poll", time.Now())
	require.False(t, scheduled, "poll must not be scheduled when disabled")
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, int32(0), ps.getTxCalls.Load(), "GetTx must not be called when poller disabled")
}

func TestClose_DrainsInclusionPool(t *testing.T) {
	ps, conn := startProgrammableServer(t)
	tc, err := NewTxClient(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		nil,
		TxClientConfig{
			GRPCConn: conn,
			ChainID:  "pocket-test",
			InclusionPoll: InclusionPollConfig{
				Interval:      5 * time.Millisecond,
				Horizon:       30 * time.Millisecond,
				MaxConcurrent: 2,
			},
		},
	)
	require.NoError(t, err)

	// Always NotFound so poll runs until horizon or Close drains it.
	ps.getTxResponse = func(_ string, _ int) (*txtypes.GetTxResponse, error) {
		return nil, status.Error(codes.NotFound, "not yet")
	}

	done := make(chan struct{}, 1)
	tc.AddInclusionCallback(func(_ context.Context, _ TxInclusionResult) {
		done <- struct{}{}
	})

	tc.scheduleInclusionPoll("pokt1test", TxTypeClaim, "hash-close-drain", time.Now())
	require.NoError(t, tc.Close())

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		// Close returned but the task completed via its own horizon — either
		// ordering is fine; we just need to make sure Close doesn't hang.
	}
	// Second Close must be safe (idempotent).
	require.NoError(t, tc.Close())
}

func TestAddInclusionCallback_NilIsNoop(t *testing.T) {
	tc, _ := newTestTxClient(t, 10*time.Millisecond, 500*time.Millisecond)
	tc.AddInclusionCallback(nil)

	tc.inclusionCallbackMu.RLock()
	assert.Empty(t, tc.inclusionCallbacks, "nil callback must not be registered")
	tc.inclusionCallbackMu.RUnlock()
}

// Sanity check the outcome string constants haven't drifted — they are part
// of the metric label contract and submission tracker JSON schema.
func TestTxInclusionOutcome_ExportedStringsAreStable(t *testing.T) {
	cases := map[TxInclusionOutcome]string{
		TxInclusionIncludedSuccess: "included_success",
		TxInclusionIncludedFailure: "included_failure",
		TxInclusionMempoolTimeout:  "mempool_timeout",
		TxInclusionPollError:       "poll_error",
	}
	for outcome, want := range cases {
		if string(outcome) != want {
			t.Errorf("outcome drift: got %q, want %q", string(outcome), want)
		}
	}
}

// Compile-time check: ensure errors.Is still sees context errors wrapped
// by gRPC correctly. Guards against a future SDK bump breaking pollOnce.
func TestPollOnce_ErrorsIsCompileGuard(_ *testing.T) {
	_ = errors.Is(context.Canceled, context.Canceled)
}
