//go:build test

package query

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

func paramsAtHeightMisses() float64 {
	return testutil.ToFloat64(queryCacheMisses.WithLabelValues("shared", "params_at_height"))
}

// TestGetParamsAtHeight_ConcurrentMissesShareOneRPC: when a session ends, every
// supplier asks for the params at that same height at once. Callers that miss
// the cache together must send one RPC between them, not one each.
func TestGetParamsAtHeight_ConcurrentMissesShareOneRPC(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	mock.sharedParamsAtHeight = generateTestSharedParams()

	release := make(chan struct{})
	var rpcs atomic.Int64
	mock.onParamsAtHeight = func(int64) {
		rpcs.Add(1)
		<-release
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = qc.Close() }()

	const callers = 16
	const height = int64(777)
	missesBefore := paramsAtHeightMisses()

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := qc.Shared().GetParamsAtHeight(context.Background(), height)
			if err == nil {
				require.Equal(t, mock.sharedParamsAtHeight.NumBlocksPerSession, p.NumBlocksPerSession)
			}
			errs <- err
		}()
	}

	// Every caller has missed the cache before the RPC is let go, so without
	// coalescing each one is past the cache and on its way to its own RPC.
	require.Eventually(t, func() bool { return paramsAtHeightMisses()-missesBefore == callers },
		5*time.Second, time.Millisecond, "all callers must miss the cache before the RPC answers")
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, int64(1), rpcs.Load(),
		"%d concurrent misses for one height must share one ParamsAtHeight RPC", callers)
}

// TestGetParamsAtHeight_ACancelledCallerDoesNotFailTheOthers: the shared RPC is
// not tied to the context of the caller that started it. That caller stops
// waiting when its context ends; the RPC keeps going and answers the rest.
func TestGetParamsAtHeight_ACancelledCallerDoesNotFailTheOthers(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	mock.sharedParamsAtHeight = generateTestSharedParams()

	release := make(chan struct{})
	var rpcs atomic.Int64
	mock.onParamsAtHeight = func(int64) {
		rpcs.Add(1)
		<-release
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = qc.Close() }()

	const height = int64(888)
	missesBefore := paramsAtHeightMisses()

	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstErr := make(chan error, 1)
	go func() {
		_, err := qc.Shared().GetParamsAtHeight(first, height)
		firstErr <- err
	}()
	require.Eventually(t, func() bool { return rpcs.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the first caller's RPC must be in flight")

	secondErr := make(chan error, 1)
	go func() {
		_, err := qc.Shared().GetParamsAtHeight(context.Background(), height)
		secondErr <- err
	}()
	require.Eventually(t, func() bool { return paramsAtHeightMisses()-missesBefore == 2 }, 5*time.Second, time.Millisecond,
		"the second caller must have missed the cache while the first RPC is in flight")

	cancelFirst()
	require.ErrorIs(t, <-firstErr, context.Canceled, "the cancelled caller stops waiting")

	close(release)
	require.NoError(t, <-secondErr, "the other caller must get the params the shared RPC returns")
	require.Equal(t, int64(1), rpcs.Load(), "the cancelled caller's RPC is the one the other caller used")
}
