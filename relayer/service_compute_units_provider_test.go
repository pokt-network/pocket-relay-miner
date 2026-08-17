//go:build test

package relayer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// fakeServiceCache is a minimal ServiceCache stand-in for testing the
// service-cache-backed compute units provider. It returns whatever service
// (or error) it is currently configured with, so tests can mutate the value
// between calls to prove the provider reads the live cache.
type fakeServiceCache struct {
	svc *sharedtypes.Service
	err error
}

func (f *fakeServiceCache) Get(_ context.Context, _ string, _ ...bool) (*sharedtypes.Service, error) {
	return f.svc, f.err
}

// fakeCUPRAtHeightClient stands in for the query layer's height-aware CUPR
// client. byHeight lets a test model an on-chain CUPR change as two different
// values at two different session start heights.
type fakeCUPRAtHeightClient struct {
	mu        sync.Mutex
	byHeight  map[int64]uint64
	err       error
	calls     atomic.Int64
	gotSvcID  string
	gotHeight int64
}

func (f *fakeCUPRAtHeightClient) GetServiceComputeUnitsPerRelayAtHeight(_ context.Context, serviceID string, blockHeight int64) (uint64, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotSvcID = serviceID
	f.gotHeight = blockHeight
	if f.err != nil {
		return 0, f.err
	}
	return f.byHeight[blockHeight], nil
}

// culog returns a logging.Logger for the compute-units provider tests
// (the package's shared testLogger returns a zerolog.Logger instead).
func culog() logging.Logger {
	return logging.NewLoggerFromConfig(logging.DefaultConfig())
}

// TestServiceCacheComputeUnitsProvider_PinsToSessionStartHeight verifies the
// provider queries at exactly the session start height it was given and returns
// that height's value.
func TestServiceCacheComputeUnitsProvider_PinsToSessionStartHeight(t *testing.T) {
	qc := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{100: 6312}}
	fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 999}}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc, qc)

	require.Equal(t, uint64(6312), p.GetServiceComputeUnits(context.Background(), "seda", 100))
	require.Equal(t, "seda", qc.gotSvcID)
	require.Equal(t, int64(100), qc.gotHeight)
	require.Equal(t, int64(1), qc.calls.Load())
}

// TestServiceCacheComputeUnitsProvider_IgnoresLiveChangeWithinSession is the core
// P0.3 regression test.
//
// A service owner changes compute_units_per_relay while a session is in flight.
// Every relay in that session must keep the SESSION-START weight, or the SMST
// becomes mixed-weight, smstSum != numRelays * cupr, and MsgCreateClaim is
// rejected with ErrProofComputeUnitsMismatch — forfeiting the whole session.
func TestServiceCacheComputeUnitsProvider_IgnoresLiveChangeWithinSession(t *testing.T) {
	qc := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{100: 6276}}
	fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6276}}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc, qc)

	const sessionStart = int64(100)
	require.Equal(t, uint64(6276), p.GetServiceComputeUnits(context.Background(), "seda", sessionStart))

	// On-chain CUPR changes mid-session; the refreshed live cache picks it up.
	fc.svc = &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}

	require.Equal(t, uint64(6276), p.GetServiceComputeUnits(context.Background(), "seda", sessionStart),
		"relays in an in-flight session must keep the session-start CUPR, not the live one")
}

// TestServiceCacheComputeUnitsProvider_TracksNewValueInNextSession keeps the
// original "must not be frozen forever" guarantee: a session starting after the
// change gets the new value.
func TestServiceCacheComputeUnitsProvider_TracksNewValueInNextSession(t *testing.T) {
	qc := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{100: 6276, 120: 6312}}
	fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc, qc)

	ctx := context.Background()
	require.Equal(t, uint64(6276), p.GetServiceComputeUnits(ctx, "seda", 100))
	require.Equal(t, uint64(6312), p.GetServiceComputeUnits(ctx, "seda", 120),
		"the next session must pick up the new on-chain CUPR")
}

// TestServiceCacheComputeUnitsProvider_NoSessionHeightUsesLiveCache covers a relay
// that carried no session header: there is no height to pin to, so the live value
// is the only option and the query client must not be called with a bogus height.
func TestServiceCacheComputeUnitsProvider_NoSessionHeightUsesLiveCache(t *testing.T) {
	testCases := []struct {
		name   string
		height int64
	}{
		{name: "zero", height: 0},
		{name: "negative", height: -1},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			qc := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{0: 111, -1: 111}}
			fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}}
			p := NewServiceCacheComputeUnitsProvider(culog(), fc, qc)

			require.Equal(t, uint64(6312), p.GetServiceComputeUnits(context.Background(), "seda", tc.height))
			require.Zero(t, qc.calls.Load(), "must not query at a meaningless height")
		})
	}
}

// TestServiceCacheComputeUnitsProvider_QueryErrorFallsBackToLive asserts a failed
// at-height query degrades to the live cache rather than dropping the relay.
func TestServiceCacheComputeUnitsProvider_QueryErrorFallsBackToLive(t *testing.T) {
	qc := &fakeCUPRAtHeightClient{err: errors.New("chain unreachable")}
	fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc, qc)

	require.Equal(t, uint64(6312), p.GetServiceComputeUnits(context.Background(), "seda", 100))
	require.Equal(t, int64(1), qc.calls.Load())
}

// TestServiceCacheComputeUnitsProvider_DefaultsToOneOnError covers both lookups
// failing: the provider floors to 1 CU rather than returning zero into claim math.
func TestServiceCacheComputeUnitsProvider_DefaultsToOneOnError(t *testing.T) {
	qc := &fakeCUPRAtHeightClient{err: errors.New("chain unreachable")}
	fc := &fakeServiceCache{err: errors.New("service not found")}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc, qc)

	require.Equal(t, uint64(1), p.GetServiceComputeUnits(context.Background(), "unknown", 100))
}

// TestServiceCacheComputeUnitsProvider_DefaultsToOneOnZero asserts a zero CUPR
// floors to 1 on BOTH the at-height and the live path.
func TestServiceCacheComputeUnitsProvider_DefaultsToOneOnZero(t *testing.T) {
	t.Run("at_height", func(t *testing.T) {
		qc := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{100: 0}}
		fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}}
		p := NewServiceCacheComputeUnitsProvider(culog(), fc, qc)

		require.Equal(t, uint64(1), p.GetServiceComputeUnits(context.Background(), "seda", 100),
			"zero CUPR would break claim math; must floor to 1")
	})

	t.Run("live", func(t *testing.T) {
		fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 0}}
		p := NewServiceCacheComputeUnitsProvider(culog(), fc, nil)

		require.Equal(t, uint64(1), p.GetServiceComputeUnits(context.Background(), "seda", 100),
			"zero CUPR would break claim math; must floor to 1")
	})
}

// TestServiceCacheComputeUnitsProvider_NilDependencies asserts the provider is
// safe when either optional dependency is absent.
func TestServiceCacheComputeUnitsProvider_NilDependencies(t *testing.T) {
	t.Run("nil_query_client_uses_cache", func(t *testing.T) {
		fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}}
		p := NewServiceCacheComputeUnitsProvider(culog(), fc, nil)

		require.Equal(t, uint64(6312), p.GetServiceComputeUnits(context.Background(), "seda", 100))
	})

	t.Run("nil_cache_still_pins_at_height", func(t *testing.T) {
		qc := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{100: 6276}}
		p := NewServiceCacheComputeUnitsProvider(culog(), nil, qc)

		require.Equal(t, uint64(6276), p.GetServiceComputeUnits(context.Background(), "seda", 100))
	})

	t.Run("both_nil_floors_to_one", func(t *testing.T) {
		p := NewServiceCacheComputeUnitsProvider(culog(), nil, nil)

		require.Equal(t, uint64(1), p.GetServiceComputeUnits(context.Background(), "seda", 100))
	})
}

// TestServiceCacheComputeUnitsProvider_Concurrent exercises the provider from
// many goroutines under the race detector — it sits on the relay hot path.
func TestServiceCacheComputeUnitsProvider_Concurrent(t *testing.T) {
	qc := &fakeCUPRAtHeightClient{byHeight: map[int64]uint64{100: 6276, 120: 6312}}
	fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc, qc)

	const goroutines = 16
	const iterations = 50

	var wg sync.WaitGroup
	bad := make(chan uint64, goroutines*iterations)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			height := int64(100)
			want := uint64(6276)
			if g%2 == 0 {
				height, want = 120, 6312
			}
			for i := 0; i < iterations; i++ {
				if got := p.GetServiceComputeUnits(context.Background(), "seda", height); got != want {
					bad <- got
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(bad)
	for got := range bad {
		require.Failf(t, "wrong CUPR under concurrency", "got %d", got)
	}
}
