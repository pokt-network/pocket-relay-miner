//go:build test

package query

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newCUPRTestClients spins up the mock gRPC server and a connected Clients.
func newCUPRTestClients(t *testing.T) (*Clients, *mockQueryServer) {
	t.Helper()

	_, address, cleanup, mock := setupMockQueryServer(t)
	t.Cleanup(cleanup)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	require.NotNil(t, qc)
	t.Cleanup(func() { _ = qc.Close() })

	return qc, mock
}

// serveCUPRAtHeight installs a hook returning cupr, and reports how many times
// the at-height RPC was actually reached.
func serveCUPRAtHeight(mock *mockQueryServer, cupr uint64, calls *atomic.Int64) {
	mock.getCUPRAtHeightFunc = func(_ context.Context, req *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
		calls.Add(1)
		return &servicetypes.QueryComputeUnitsPerRelayAtHeightResponse{ComputeUnitsPerRelay: cupr}, nil
	}
}

// serveLiveService installs a GetService hook returning a service with the given cupr.
func serveLiveService(mock *mockQueryServer, serviceID string, liveCUPR uint64) {
	mock.getServiceFunc = func(_ context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		return &servicetypes.QueryGetServiceResponse{
			Service: sharedtypes.Service{
				Id:                   req.Id,
				Name:                 req.Id,
				ComputeUnitsPerRelay: liveCUPR,
				OwnerAddress:         "pokt1owner",
			},
		}, nil
	}
}

// TestGetServiceComputeUnitsPerRelayAtHeight_Success verifies the request carries
// the exact serviceId and blockHeight the caller asked for, and the response value
// is returned verbatim.
func TestGetServiceComputeUnitsPerRelayAtHeight_Success(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	var gotServiceID string
	var gotHeight int64
	mock.getCUPRAtHeightFunc = func(_ context.Context, req *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
		gotServiceID = req.ServiceId
		gotHeight = req.BlockHeight
		return &servicetypes.QueryComputeUnitsPerRelayAtHeightResponse{ComputeUnitsPerRelay: 42}, nil
	}

	cupr, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(context.Background(), "develop", 100)
	require.NoError(t, err)
	require.Equal(t, uint64(42), cupr)
	require.Equal(t, "develop", gotServiceID)
	require.Equal(t, int64(100), gotHeight)

	// A successful query must leave the degrade state clear.
	require.False(t, qc.serviceClient.cuprAtHeightDegraded.Load())
	require.Zero(t, qc.serviceClient.cuprAtHeightUnsupportedUntilUnixNano.Load())
}

// TestGetServiceComputeUnitsPerRelayAtHeight_Cache verifies the same
// (service, height) is served from cache while a different height re-queries.
func TestGetServiceComputeUnitsPerRelayAtHeight_Cache(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	var calls atomic.Int64
	serveCUPRAtHeight(mock, 7, &calls)

	ctx := context.Background()

	first, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, uint64(7), first)
	require.Equal(t, int64(1), calls.Load())

	second, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, uint64(7), second)
	require.Equal(t, int64(1), calls.Load(), "same service@height must be served from cache")

	// A different height is a different immutable value — must re-query.
	third, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "develop", 101)
	require.NoError(t, err)
	require.Equal(t, uint64(7), third)
	require.Equal(t, int64(2), calls.Load())

	// A different service at the same height must also re-query.
	_, err = qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "other", 100)
	require.NoError(t, err)
	require.Equal(t, int64(3), calls.Load())

	require.Equal(t, int64(3), qc.serviceClient.cuprAtHeightCacheSize.Load())
}

// TestGetServiceComputeUnitsPerRelayAtHeight_CacheKeyIsUnambiguous guards against
// a naive concatenated key, where ("a", 11) and ("a@1", 1) would collide.
func TestGetServiceComputeUnitsPerRelayAtHeight_CacheKeyIsUnambiguous(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	mock.getCUPRAtHeightFunc = func(_ context.Context, req *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
		// Encode the request into the response so a collision is observable.
		return &servicetypes.QueryComputeUnitsPerRelayAtHeightResponse{
			ComputeUnitsPerRelay: uint64(len(req.ServiceId))*1000 + uint64(req.BlockHeight),
		}, nil
	}

	ctx := context.Background()

	a, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "a", 11)
	require.NoError(t, err)
	b, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "a@1", 1)
	require.NoError(t, err)

	require.Equal(t, uint64(1011), a)
	require.Equal(t, uint64(3001), b)
	require.NotEqual(t, a, b, "distinct (service, height) pairs must not share a cache key")
}

// TestGetServiceComputeUnitsPerRelayAtHeight_UnimplementedReturnsSentinel is the
// F2 regression: while degraded, the method must return ErrCUPRAtHeightUnavailable
// — NOT a live value with a nil error. Returning live+nil defeats the miner claim
// guard, which then compares a mined-at-session-start tree against the live cupr
// and terminally skips a payable claim. The degrade cooldown must still arm.
func TestGetServiceComputeUnitsPerRelayAtHeight_UnimplementedReturnsSentinel(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	// Hook left nil -> mock server answers codes.Unimplemented (pre-v0.1.35 node).
	// A live service IS available — the test proves it is NOT silently returned.
	serveLiveService(mock, "develop", 99)

	cupr, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(context.Background(), "develop", 100)
	require.ErrorIs(t, err, ErrCUPRAtHeightUnavailable, "degraded at-height query must surface the sentinel error")
	require.Zero(t, cupr, "no live value may be returned while degraded")

	require.True(t, qc.serviceClient.cuprAtHeightDegraded.Load())
	require.Greater(t, qc.serviceClient.cuprAtHeightUnsupportedUntilUnixNano.Load(), time.Now().UnixNano(),
		"a cooldown deadline in the future must be armed")
}

// TestGetServiceComputeUnitsPerRelayAtHeight_DegradedIsNotCached asserts nothing is
// written into the at-height cache while degraded. A cached entry would outlive the
// cooldown and permanently defeat recovery.
func TestGetServiceComputeUnitsPerRelayAtHeight_DegradedIsNotCached(t *testing.T) {
	qc, mock := newCUPRTestClients(t)
	serveLiveService(mock, "develop", 99)

	_, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(context.Background(), "develop", 100)
	require.ErrorIs(t, err, ErrCUPRAtHeightUnavailable)

	_, cached := qc.serviceClient.cuprAtHeightCache.Load("develop@100")
	require.False(t, cached, "no value may be cached under the at-height key while degraded")
	require.Zero(t, qc.serviceClient.cuprAtHeightCacheSize.Load())
}

// TestGetServiceComputeUnitsPerRelayAtHeight_CooldownSkipsQuery asserts that while
// the cooldown is armed the at-height RPC is not retried on every relay.
func TestGetServiceComputeUnitsPerRelayAtHeight_CooldownSkipsQuery(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	var atHeightCalls atomic.Int64
	mock.getCUPRAtHeightFunc = func(_ context.Context, _ *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
		atHeightCalls.Add(1)
		return nil, status.Error(codes.Unimplemented, "unknown method")
	}
	serveLiveService(mock, "develop", 99)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		cupr, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "develop", int64(100+i))
		require.ErrorIs(t, err, ErrCUPRAtHeightUnavailable)
		require.Zero(t, cupr)
	}

	require.Equal(t, int64(1), atHeightCalls.Load(),
		"the at-height RPC must be probed once, then skipped for the cooldown window")
}

// TestGetServiceComputeUnitsPerRelayAtHeight_CooldownExpiresAndRecovers proves the
// cooldown is an EXPIRING window, not a permanent latch: once it lapses and the
// node implements the RPC, session-start pricing resumes.
//
// Expiry is simulated by rewinding the deadline atomic rather than sleeping, so
// the test is deterministic.
func TestGetServiceComputeUnitsPerRelayAtHeight_CooldownExpiresAndRecovers(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	unimplemented := atomic.Bool{}
	unimplemented.Store(true)

	var atHeightCalls atomic.Int64
	mock.getCUPRAtHeightFunc = func(_ context.Context, _ *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
		atHeightCalls.Add(1)
		if unimplemented.Load() {
			return nil, status.Error(codes.Unimplemented, "unknown method")
		}
		return &servicetypes.QueryComputeUnitsPerRelayAtHeightResponse{ComputeUnitsPerRelay: 55}, nil
	}
	serveLiveService(mock, "develop", 99)

	ctx := context.Background()

	// Degrade: the sentinel error is surfaced, not a live value.
	cupr, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "develop", 100)
	require.ErrorIs(t, err, ErrCUPRAtHeightUnavailable)
	require.Zero(t, cupr)
	require.True(t, qc.serviceClient.cuprAtHeightDegraded.Load())

	// Node is upgraded, cooldown lapses.
	unimplemented.Store(false)
	qc.serviceClient.cuprAtHeightUnsupportedUntilUnixNano.Store(time.Now().Add(-time.Second).UnixNano())

	cupr, err = qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, uint64(55), cupr, "session-start pricing must resume after recovery")
	require.False(t, qc.serviceClient.cuprAtHeightDegraded.Load(), "degrade flag must clear on recovery")
	require.Zero(t, qc.serviceClient.cuprAtHeightUnsupportedUntilUnixNano.Load(), "cooldown must be cleared on recovery")
	require.Equal(t, int64(2), atHeightCalls.Load())

	// The recovered value is now cached under the at-height key.
	entry, cached := qc.serviceClient.cuprAtHeightCache.Load("develop@100")
	require.True(t, cached)
	require.Equal(t, uint64(55), entry.computeUnitsPerRelay)
	require.Equal(t, int64(100), entry.blockHeight)
}

// TestGetServiceComputeUnitsPerRelayAtHeight_NonUnimplementedErrorPropagates
// asserts only codes.Unimplemented degrades. Any other failure must surface, not
// be silently replaced with a live value the chain will not agree with.
func TestGetServiceComputeUnitsPerRelayAtHeight_NonUnimplementedErrorPropagates(t *testing.T) {
	testCases := []struct {
		name string
		code codes.Code
	}{
		{name: "internal", code: codes.Internal},
		{name: "unavailable", code: codes.Unavailable},
		{name: "not_found", code: codes.NotFound},
		{name: "deadline_exceeded", code: codes.DeadlineExceeded},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			qc, mock := newCUPRTestClients(t)

			mock.getCUPRAtHeightFunc = func(_ context.Context, _ *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
				return nil, status.Error(tc.code, "boom")
			}
			// A live service IS available — the test proves it is not silently used.
			serveLiveService(mock, "develop", 99)

			cupr, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(context.Background(), "develop", 100)
			require.Error(t, err)
			require.Zero(t, cupr)
			require.Equal(t, tc.code, status.Code(err))
			require.False(t, qc.serviceClient.cuprAtHeightDegraded.Load(),
				"a non-Unimplemented error must not arm the degrade cooldown")
			require.Zero(t, qc.serviceClient.cuprAtHeightUnsupportedUntilUnixNano.Load())
		})
	}
}

// TestGetServiceComputeUnitsPerRelayAtHeight_DegradeDoesNotConsultLive proves the
// degrade path no longer performs an internal live fallback: it surfaces the
// sentinel error WITHOUT calling GetService. The live fallback now belongs to each
// caller, not the query layer.
func TestGetServiceComputeUnitsPerRelayAtHeight_DegradeDoesNotConsultLive(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	mock.getCUPRAtHeightFunc = func(_ context.Context, _ *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
		return nil, status.Error(codes.Unimplemented, "unknown method")
	}
	var liveCalls atomic.Int64
	mock.getServiceFunc = func(_ context.Context, _ *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
		liveCalls.Add(1)
		return nil, status.Error(codes.NotFound, "service not found")
	}

	cupr, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(context.Background(), "develop", 100)
	require.ErrorIs(t, err, ErrCUPRAtHeightUnavailable)
	require.Zero(t, cupr)
	require.Zero(t, liveCalls.Load(), "the query layer must not consult the live service during degrade")
}

// TestGetServiceComputeUnitsPerRelayAtHeight_CacheEviction asserts the cache stays
// bounded and sheds the oldest heights first.
func TestGetServiceComputeUnitsPerRelayAtHeight_CacheEviction(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	var calls atomic.Int64
	serveCUPRAtHeight(mock, 3, &calls)

	ctx := context.Background()
	services := []string{"svc1", "svc2", "svc3", "svc4", "svc5", "svc6", "svc7", "svc8", "svc9", "svc10"}
	heightsPerService := int64(maxCUPRAtHeightCacheEntries/len(services)) + 10

	for h := int64(1); h <= heightsPerService; h++ {
		for _, svc := range services {
			_, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, svc, h)
			require.NoError(t, err)
		}
	}

	require.LessOrEqual(t, qc.serviceClient.cuprAtHeightCacheSize.Load(), int64(maxCUPRAtHeightCacheEntries),
		"cache size %d exceeds max %d after eviction",
		qc.serviceClient.cuprAtHeightCacheSize.Load(), maxCUPRAtHeightCacheEntries)

	for _, svc := range services {
		recent := fmt.Sprintf("%s@%d", svc, heightsPerService)
		_, ok := qc.serviceClient.cuprAtHeightCache.Load(recent)
		require.True(t, ok, "recent entry %s should still be cached", recent)

		oldest := fmt.Sprintf("%s@%d", svc, 1)
		_, ok = qc.serviceClient.cuprAtHeightCache.Load(oldest)
		require.False(t, ok, "old entry %s should have been evicted", oldest)
	}
}

// TestGetServiceComputeUnitsPerRelayAtHeight_Concurrent exercises simultaneous
// reads of a shared key and writes of fresh keys under the race detector.
func TestGetServiceComputeUnitsPerRelayAtHeight_Concurrent(t *testing.T) {
	qc, mock := newCUPRTestClients(t)

	var calls atomic.Int64
	serveCUPRAtHeight(mock, 11, &calls)

	ctx := context.Background()

	// Prime the shared key so readers hit the cache path.
	_, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "shared", 1)
	require.NoError(t, err)

	const goroutines = 16
	const iterations = 25

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*iterations)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if g%2 == 0 {
					// Reader: repeatedly hit the same cached key.
					cupr, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "shared", 1)
					if err != nil {
						errs <- err
						return
					}
					if cupr != 11 {
						errs <- fmt.Errorf("got cupr %d, want 11", cupr)
						return
					}
					continue
				}
				// Writer: force a fresh cache entry each iteration.
				if _, err := qc.Service().GetServiceComputeUnitsPerRelayAtHeight(ctx, "svc", int64(g*iterations+i)); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
