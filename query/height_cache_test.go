//go:build test

package query

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// These tests pin the height-keyed caches (difficulty, compute units per relay,
// shared params at height) to how the chain writes those values: a value
// effective at height h can still change until the chain has committed h, so an
// answer is reused only once the node answered from a height at or above h.

func newHeightCacheTestClients(t *testing.T, address string) *Clients {
	t.Helper()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = qc.Close() })
	return qc
}

func cacheSizeGauge(t *testing.T, client, cacheType string) float64 {
	t.Helper()
	return testutil.ToFloat64(queryCacheSize.WithLabelValues(client, cacheType))
}

// difficultyAnswer answers the at-height difficulty RPC with targetHash, counting
// every call in rpcs.
func difficultyAnswer(rpcs *atomic.Int64, targetHash func() []byte) func(context.Context, *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
	return func(_ context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		rpcs.Add(1)
		return &servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse{
			RelayMiningDifficulty: servicetypes.RelayMiningDifficulty{
				ServiceId:    req.ServiceId,
				BlockHeight:  req.BlockHeight,
				NumRelaysEma: 1000,
				TargetHash:   targetHash(),
			},
		}, nil
	}
}

func hashOf(b byte) []byte {
	h := make([]byte, 32)
	h[0] = b
	return h
}

// flightJoinHook is implemented by a client whose concurrent misses for one key
// share one RPC. A test that makes N callers miss together releases the RPC once
// every caller has either reached the RPC or joined the shared one, so the same
// test runs, without timing, whether misses are shared or not.
type flightJoinHook interface {
	setFlightJoinHook(func())
}

// releaseWhenAllArrived returns the RPC gate for n concurrent callers: the
// handler calls arrive() and blocks until every caller has arrived at the RPC or
// joined an RPC already in flight.
func releaseWhenAllArrived(t *testing.T, client any, n int64) (arrive func(), wait func()) {
	t.Helper()
	var arrived, joined atomic.Int64
	if h, ok := client.(flightJoinHook); ok {
		h.setFlightJoinHook(func() { joined.Add(1) })
	}
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo) // a failed wait must not leave a handler blocked
	arrive = func() {
		arrived.Add(1)
		<-release
	}
	wait = func() {
		require.Eventually(t, func() bool { return arrived.Load()+joined.Load() >= n },
			5*time.Second, time.Millisecond, "every caller must reach the RPC or join it before it answers")
		letGo()
	}
	return arrive, wait
}

// TestHeightCache_Difficulty_ConcurrentMissesShareOneRPC: when a session starts,
// every relay in flight for a service asks for the same difficulty at once. They
// must send one RPC between them, and the cache must count one entry, not one per
// caller (the inflated count is what made the cache evict itself on every miss).
func TestHeightCache_Difficulty_ConcurrentMissesShareOneRPC(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)

	const callers = 16
	var rpcs atomic.Int64
	arrive, wait := releaseWhenAllArrived(t, qc.serviceClient, callers)
	answer := difficultyAnswer(&rpcs, func() []byte { return hashOf(7) })
	mock.getRelayMiningDifficultyAtHeightFunc = func(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		arrive()
		return answer(ctx, req)
	}

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(context.Background(), "svc", 100)
			errs <- err
		}()
	}
	wait()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, int64(1), rpcs.Load(), "%d concurrent misses for one key must share one RPC", callers)
	require.Equal(t, 1.0, cacheSizeGauge(t, "service", "difficulty"),
		"the size gauge must count the one entry, not the callers")
}

// TestHeightCache_CUPR_ConcurrentMissesShareOneRPC is the compute-units twin.
func TestHeightCache_CUPR_ConcurrentMissesShareOneRPC(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)

	const callers = 16
	var rpcs atomic.Int64
	arrive, wait := releaseWhenAllArrived(t, qc.serviceClient, callers)
	mock.getCUPRAtHeightFunc = func(_ context.Context, _ *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
		rpcs.Add(1)
		arrive()
		return &servicetypes.QueryComputeUnitsPerRelayAtHeightResponse{ComputeUnitsPerRelay: 9}, nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := qc.serviceClient.GetServiceComputeUnitsPerRelayAtHeight(context.Background(), "svc", 100)
			errs <- err
		}()
	}
	wait()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, int64(1), rpcs.Load(), "%d concurrent misses for one key must share one RPC", callers)
	require.Equal(t, 1.0, cacheSizeGauge(t, "service", "cupr_at_height"),
		"the size gauge must count the one entry, not the callers")
}

// TestHeightCache_ManyServicesAtOneHeightSurvive: every service's sessions start
// on the same height, so a full cache holds entries that nearly all share one
// height. Making room must never throw away the entries in use.
func TestHeightCache_ManyServicesAtOneHeightSurvive(t *testing.T) {
	ctx := context.Background()
	const height = int64(100)

	t.Run("difficulty", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()
		qc := newHeightCacheTestClients(t, address)
		var rpcs atomic.Int64
		mock.getRelayMiningDifficultyAtHeightFunc = difficultyAnswer(&rpcs, func() []byte { return hashOf(1) })

		for i := 0; i <= maxHeightDifficultyCacheEntries; i++ {
			_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, fmt.Sprintf("svc-%d", i), height)
			require.NoError(t, err)
		}
		before := rpcs.Load()
		_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, fmt.Sprintf("svc-%d", maxHeightDifficultyCacheEntries), height)
		require.NoError(t, err)
		require.Equal(t, before, rpcs.Load(), "the entry just stored must still be served from the cache")
	})

	t.Run("compute units per relay", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()
		qc := newHeightCacheTestClients(t, address)
		var rpcs atomic.Int64
		mock.getCUPRAtHeightFunc = func(_ context.Context, _ *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
			rpcs.Add(1)
			return &servicetypes.QueryComputeUnitsPerRelayAtHeightResponse{ComputeUnitsPerRelay: 3}, nil
		}

		for i := 0; i <= maxCUPRAtHeightCacheEntries; i++ {
			_, err := qc.serviceClient.GetServiceComputeUnitsPerRelayAtHeight(ctx, fmt.Sprintf("svc-%d", i), height)
			require.NoError(t, err)
		}
		before := rpcs.Load()
		_, err := qc.serviceClient.GetServiceComputeUnitsPerRelayAtHeight(ctx, fmt.Sprintf("svc-%d", maxCUPRAtHeightCacheEntries), height)
		require.NoError(t, err)
		require.Equal(t, before, rpcs.Load(), "the entry just stored must still be served from the cache")
	})
}

// TestHeightCache_Difficulty_OperatorLoop is the reported failure in miniature: a
// relayer running for many sessions, where each session start sends a burst of
// concurrent relays per service. The cache must keep counting what it holds and
// keep serving repeat lookups in every session, instead of drifting past its
// bound and emptying itself on every miss.
func TestHeightCache_Difficulty_OperatorLoop(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)

	const (
		services     = 10
		sessions     = 110 // more entries than the cache holds, so it evicts
		burst        = 32
		sessionBlock = int64(10)
	)
	var rpcs atomic.Int64
	answer := difficultyAnswer(&rpcs, func() []byte { return hashOf(2) })

	ctx := context.Background()
	for s := int64(0); s < sessions; s++ {
		start := 100 + s*sessionBlock
		mock.answeredHeight.Store(start)
		for svc := 0; svc < services; svc++ {
			serviceID := fmt.Sprintf("svc-%d", svc)
			arrive, wait := releaseWhenAllArrived(t, qc.serviceClient, burst)
			mock.getRelayMiningDifficultyAtHeightFunc = func(c context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
				arrive()
				return answer(c, req)
			}
			var wg sync.WaitGroup
			errs := make(chan error, burst)
			for i := 0; i < burst; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, serviceID, start)
					errs <- err
				}()
			}
			wait()
			wg.Wait()
			close(errs)
			for err := range errs {
				require.NoError(t, err)
			}

			// The rest of the session: these lookups must not reach the chain.
			before := rpcs.Load()
			for i := 0; i < 5; i++ {
				_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, serviceID, start)
				require.NoError(t, err)
			}
			require.Equal(t, before, rpcs.Load(),
				"session %d, %s: lookups after the session's first answer must hit the cache", s, serviceID)
		}
	}
	require.LessOrEqual(t, cacheSizeGauge(t, "service", "difficulty"), float64(maxHeightDifficultyCacheEntries),
		"the size gauge must stay within the bound")
	require.Equal(t, float64(qc.serviceClient.difficultyAtHeight.final.Size()), cacheSizeGauge(t, "service", "difficulty"),
		"the size gauge must be the map's size")
}

// TestHeightCache_AnswerBelowTheAskedHeightIsNotReused: a relay can carry a
// session start the node has not committed yet (the node lags, or the client is
// ahead). The chain may still write a value effective at that height, so an
// answer from below it must not be reused once the node has caught up.
func TestHeightCache_AnswerBelowTheAskedHeightIsNotReused(t *testing.T) {
	ctx := context.Background()

	t.Run("difficulty", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()
		qc := newHeightCacheTestClients(t, address)

		var rpcs atomic.Int64
		var current atomic.Value
		current.Store(hashOf(1))
		mock.getRelayMiningDifficultyAtHeightFunc = difficultyAnswer(&rpcs, func() []byte { return current.Load().([]byte) })

		mock.answeredHeight.Store(100)
		d, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 105)
		require.NoError(t, err)
		require.Equal(t, hashOf(1), d.TargetHash)

		current.Store(hashOf(2))
		mock.answeredHeight.Store(110)
		d, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 105)
		require.NoError(t, err)
		require.Equal(t, hashOf(2), d.TargetHash, "an answer from height 100 must not be reused for height 105")
	})

	// A service's first settlement writes its difficulty effective at the
	// settlement block itself. When that block is a session start S, the value at
	// S changes at S: the answer given from S-1 is not the value of S.
	t.Run("difficulty first update at a session start", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()
		qc := newHeightCacheTestClients(t, address)

		var rpcs atomic.Int64
		var current atomic.Value
		current.Store(hashOf(0xff))
		mock.getRelayMiningDifficultyAtHeightFunc = difficultyAnswer(&rpcs, func() []byte { return current.Load().([]byte) })

		const sessionStart = int64(121)
		mock.answeredHeight.Store(sessionStart - 1)
		d, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", sessionStart)
		require.NoError(t, err)
		require.Equal(t, hashOf(0xff), d.TargetHash)

		current.Store(hashOf(0x0f))
		mock.answeredHeight.Store(sessionStart)
		d, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", sessionStart)
		require.NoError(t, err)
		require.Equal(t, hashOf(0x0f), d.TargetHash, "the value written at the session start must be served")
	})

	t.Run("compute units per relay", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()
		qc := newHeightCacheTestClients(t, address)

		var current atomic.Uint64
		current.Store(5)
		mock.getCUPRAtHeightFunc = func(_ context.Context, _ *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
			return &servicetypes.QueryComputeUnitsPerRelayAtHeightResponse{ComputeUnitsPerRelay: current.Load()}, nil
		}

		mock.answeredHeight.Store(100)
		cupr, err := qc.serviceClient.GetServiceComputeUnitsPerRelayAtHeight(ctx, "svc", 105)
		require.NoError(t, err)
		require.Equal(t, uint64(5), cupr)

		current.Store(8)
		mock.answeredHeight.Store(110)
		cupr, err = qc.serviceClient.GetServiceComputeUnitsPerRelayAtHeight(ctx, "svc", 105)
		require.NoError(t, err)
		require.Equal(t, uint64(8), cupr, "an answer from height 100 must not be reused for height 105")
	})

	t.Run("shared params", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()
		qc := newHeightCacheTestClients(t, address)

		before := generateTestSharedParams()
		mock.sharedParamsAtHeight = before
		mock.answeredHeight.Store(100)
		p, err := qc.Shared().GetParamsAtHeight(ctx, 105)
		require.NoError(t, err)
		require.Equal(t, before.NumBlocksPerSession, p.NumBlocksPerSession)

		after := *before
		after.NumBlocksPerSession = before.NumBlocksPerSession + 1
		mock.sharedParamsAtHeight = &after
		mock.answeredHeight.Store(110)
		p, err = qc.Shared().GetParamsAtHeight(ctx, 105)
		require.NoError(t, err)
		require.Equal(t, after.NumBlocksPerSession, p.NumBlocksPerSession,
			"an answer from height 100 must not be reused for height 105")
	})
}

// TestHeightCache_HitAllocatesNothing: the difficulty and compute-units lookups
// run on every relay. A hit must not allocate.
func TestHeightCache_HitAllocatesNothing(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)

	var rpcs atomic.Int64
	mock.getRelayMiningDifficultyAtHeightFunc = difficultyAnswer(&rpcs, func() []byte { return hashOf(3) })
	mock.getCUPRAtHeightFunc = func(_ context.Context, _ *servicetypes.QueryComputeUnitsPerRelayAtHeightRequest) (*servicetypes.QueryComputeUnitsPerRelayAtHeightResponse, error) {
		return &servicetypes.QueryComputeUnitsPerRelayAtHeightResponse{ComputeUnitsPerRelay: 4}, nil
	}

	ctx := context.Background()
	_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
	require.NoError(t, err)
	_, err = qc.serviceClient.GetServiceComputeUnitsPerRelayAtHeight(ctx, "svc", 100)
	require.NoError(t, err)

	difficultyAllocs := testing.AllocsPerRun(100, func() {
		_, _ = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
	})
	cuprAllocs := testing.AllocsPerRun(100, func() {
		_, _ = qc.serviceClient.GetServiceComputeUnitsPerRelayAtHeight(ctx, "svc", 100)
	})
	require.Zero(t, difficultyAllocs, "a difficulty cache hit must not allocate")
	require.Zero(t, cuprAllocs, "a compute-units cache hit must not allocate")
}

// TestHeightCache_NotFinalAnswerIsAskedAgainOncePerBlock: an answer from below
// the asked height is served to every lookup until this process sees a new
// block, then asked once more: one RPC per block per key, never one per relay.
func TestHeightCache_NotFinalAnswerIsAskedAgainOncePerBlock(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)
	var block atomic.Int64
	block.Store(100)
	qc.SetBlockHeightSource(block.Load)

	var rpcs atomic.Int64
	mock.getRelayMiningDifficultyAtHeightFunc = difficultyAnswer(&rpcs, func() []byte { return hashOf(1) })
	mock.answeredHeight.Store(99)

	ctx := context.Background()
	for i := 0; i < 100; i++ {
		_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), rpcs.Load(), "lookups within one block must share the one answer")

	block.Store(101)
	for i := 0; i < 100; i++ {
		_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
		require.NoError(t, err)
	}
	require.Equal(t, int64(2), rpcs.Load(), "a new block asks the not-final answer once more")
	require.Zero(t, qc.serviceClient.difficultyAtHeight.final.Size(), "an answer from below the height is not final")
}

// TestHeightCache_LaggingNodeAnswerAtOrAboveHeightIsFinal: this process can be
// ahead of the node it queries. What makes an answer final is the node having
// committed the asked height, not the node being as far as this process.
func TestHeightCache_LaggingNodeAnswerAtOrAboveHeightIsFinal(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)
	var block atomic.Int64
	block.Store(110)
	qc.SetBlockHeightSource(block.Load)

	var rpcs atomic.Int64
	mock.getRelayMiningDifficultyAtHeightFunc = difficultyAnswer(&rpcs, func() []byte { return hashOf(1) })
	mock.answeredHeight.Store(105)

	ctx := context.Background()
	_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
	require.NoError(t, err)
	block.Store(120)
	_, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), rpcs.Load(), "an answer from height 105 is final for height 100")
}

// TestHeightCache_AnswerWithoutHeightIsNeverFinal: a proxy can strip the header
// that says which height the node answered from. Then no answer can be proven
// final: each is served within its block and asked again on the next, and the
// missing header is counted.
func TestHeightCache_AnswerWithoutHeightIsNeverFinal(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)
	var block atomic.Int64
	block.Store(200)
	qc.SetBlockHeightSource(block.Load)
	mock.omitHeightHeader.Store(true)

	var rpcs atomic.Int64
	mock.getRelayMiningDifficultyAtHeightFunc = difficultyAnswer(&rpcs, func() []byte { return hashOf(1) })
	missingBefore := testutil.ToFloat64(queryCacheAnswerHeightMissing.WithLabelValues("service", "difficulty"))

	ctx := context.Background()
	_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
	require.NoError(t, err)
	_, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), rpcs.Load(), "within one block the answer is served")

	block.Store(201)
	_, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 100)
	require.NoError(t, err)
	require.Equal(t, int64(2), rpcs.Load(), "without the header the answer is asked again on the next block")
	require.Zero(t, qc.serviceClient.difficultyAtHeight.final.Size())
	require.Equal(t, missingBefore+2,
		testutil.ToFloat64(queryCacheAnswerHeightMissing.WithLabelValues("service", "difficulty")))
}

// TestHeightCache_ErrorsAreNeverStored: a failed answer leaves nothing behind;
// the next lookup asks the chain again.
func TestHeightCache_ErrorsAreNeverStored(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)
	var block atomic.Int64
	block.Store(100)
	qc.SetBlockHeightSource(block.Load)

	var rpcs atomic.Int64
	mock.getRelayMiningDifficultyAtHeightFunc = func(context.Context, *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		rpcs.Add(1)
		return nil, status.Error(codes.NotFound, "service not found")
	}

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "missing", 100)
		require.Equal(t, codes.NotFound, status.Code(errors.Unwrap(err)), "the chain's error must surface")
	}
	require.Equal(t, int64(2), rpcs.Load(), "a failed answer must not be served again")
	cache := qc.serviceClient.difficultyAtHeight
	require.Zero(t, cache.final.Size())
	require.Zero(t, cache.pending.Size())
}

// TestHeightCache_Difficulty_ACancelledCallerDoesNotFailTheOthers mirrors the
// params test: the shared RPC is not tied to the caller that started it.
func TestHeightCache_Difficulty_ACancelledCallerDoesNotFailTheOthers(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)

	var joins atomic.Int64
	qc.serviceClient.setFlightJoinHook(func() { joins.Add(1) })
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	defer letGo()
	var rpcs atomic.Int64
	answer := difficultyAnswer(&rpcs, func() []byte { return hashOf(5) })
	mock.getRelayMiningDifficultyAtHeightFunc = func(c context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		<-release
		return answer(c, req)
	}

	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstErr := make(chan error, 1)
	go func() {
		_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(first, "svc", 100)
		firstErr <- err
	}()
	require.Eventually(t, func() bool { return joins.Load() == 1 }, 5*time.Second, time.Millisecond)

	secondErr := make(chan error, 1)
	go func() {
		_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(context.Background(), "svc", 100)
		secondErr <- err
	}()
	require.Eventually(t, func() bool { return joins.Load() == 2 }, 5*time.Second, time.Millisecond)

	cancelFirst()
	require.ErrorIs(t, <-firstErr, context.Canceled, "the cancelled caller stops waiting")
	letGo()
	require.NoError(t, <-secondErr, "the other caller gets the shared answer")
	require.Equal(t, int64(1), rpcs.Load())
}

// TestHeightCache_NotFinalAnswerIsNotReusedPastItsBound: when no block arrives (a
// stalled block feed), an answer that is not final must not become permanent.
func TestHeightCache_NotFinalAnswerIsNotReusedPastItsBound(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)
	var block atomic.Int64
	block.Store(100)
	qc.SetBlockHeightSource(block.Load)

	var rpcs atomic.Int64
	mock.getRelayMiningDifficultyAtHeightFunc = difficultyAnswer(&rpcs, func() []byte { return hashOf(1) })
	mock.answeredHeight.Store(99)

	ctx := context.Background()
	key := heightKey{id: "svc", height: 100}
	_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, key.id, key.height)
	require.NoError(t, err)

	cache := qc.serviceClient.difficultyAtHeight
	e, ok := cache.pending.Load(key)
	require.True(t, ok)
	e.fetchedAt = time.Now().Add(-maxPendingReuse)
	cache.pending.Store(key, e)

	_, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, key.id, key.height)
	require.NoError(t, err)
	require.Equal(t, int64(2), rpcs.Load(), "a not-final answer past its bound is asked again with no new block")
}

// TestHeightCache_BlockDuringTheRPCIsNotCoveredByItsAnswer: an answer the node
// gave while this process moved to a new block was computed before that block,
// so it must not be reused for it.
func TestHeightCache_BlockDuringTheRPCIsNotCoveredByItsAnswer(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	qc := newHeightCacheTestClients(t, address)
	var block atomic.Int64
	block.Store(100)
	qc.SetBlockHeightSource(block.Load)

	var rpcs atomic.Int64
	answer := difficultyAnswer(&rpcs, func() []byte { return hashOf(1) })
	mock.answeredHeight.Store(99)
	mock.getRelayMiningDifficultyAtHeightFunc = func(c context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		if rpcs.Load() == 0 {
			block.Store(101) // a block arrives while the first RPC is in flight
		}
		return answer(c, req)
	}

	ctx := context.Background()
	_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 102)
	require.NoError(t, err)
	_, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "svc", 102)
	require.NoError(t, err)
	require.Equal(t, int64(2), rpcs.Load(), "the block that arrived during the RPC asks the answer again")
}

// Standalone mode builds two caches of each type, one per side, on the same
// gauge series: the size and pending gauges are the sum of both, not the count
// of whichever stored last.
func TestHeightCache_SizeAndPendingGaugesSumTheCachesOfOneType(t *testing.T) {
	const client, cacheType = "gauge-sum-test", "difficulty"
	fetch := func(context.Context, heightKey) (int, int64, error) { return 0, 0, nil }
	equal := func(a, b int) bool { return a == b }
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	a := newHeightCache(logger, client, cacheType, 100, time.Second, &blockHeightSource{}, fetch, equal)
	b := newHeightCache(logger, client, cacheType, 100, time.Second, &blockHeightSource{}, fetch, equal)
	size := testutil.ToFloat64(queryCacheSize.WithLabelValues(client, cacheType))
	pending := testutil.ToFloat64(queryCachePending.WithLabelValues(client, cacheType))

	for i := 0; i < 2; i++ {
		require.True(t, a.store(heightKey{id: "svc", height: int64(10 + i)}, i, 100, 0))
	}
	for i := 0; i < 3; i++ {
		require.True(t, b.store(heightKey{id: "svc", height: int64(20 + i)}, i, 100, 0))
	}
	require.False(t, a.store(heightKey{id: "svc", height: 500}, 0, 100, 0), "premise: answered from below 500")
	require.False(t, b.store(heightKey{id: "svc", height: 600}, 0, 100, 0))
	require.False(t, b.store(heightKey{id: "svc", height: 601}, 0, 100, 0))

	require.Equal(t, size+5, testutil.ToFloat64(queryCacheSize.WithLabelValues(client, cacheType)), "2 final in one cache, 3 in the other")
	require.Equal(t, pending+3, testutil.ToFloat64(queryCachePending.WithLabelValues(client, cacheType)), "1 not final in one cache, 2 in the other")

	a.unpublish()
	require.Equal(t, size+3, testutil.ToFloat64(queryCacheSize.WithLabelValues(client, cacheType)), "a closed cache takes its share out")
	require.Equal(t, pending+2, testutil.ToFloat64(queryCachePending.WithLabelValues(client, cacheType)))
}
